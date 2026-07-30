package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestMCPRowDecodePreservesExactPostgreSQLNumberLexemes(t *testing.T) {
	raw := json.RawMessage(`{"rows":[[0,36761375.00,225000,75000],[1,36758875.00,225000,75000],[2,36760625.00,225000,75000]],"row_count":3}`)
	var response executeResponse
	if err := decodeStructuredContent(raw, &response); err != nil {
		t.Fatal(err)
	}
	for rowIndex, row := range response.Rows {
		for columnIndex, value := range row {
			if _, ok := value.(json.Number); !ok {
				t.Fatalf("Gateway row %d value %d decoded as %T, want json.Number", rowIndex, columnIndex, value)
			}
		}
	}
	direct := [][]any{
		{int16(0), pgtype.Numeric{Int: big.NewInt(3_676_137_500), Exp: -2, Valid: true}, int64(225_000), int64(75_000)},
		{int16(1), pgtype.Numeric{Int: big.NewInt(3_675_887_500), Exp: -2, Valid: true}, int64(225_000), int64(75_000)},
		{int16(2), pgtype.Numeric{Int: big.NewInt(3_676_062_500), Exp: -2, Valid: true}, int64(225_000), int64(75_000)},
	}
	directDigest, err := resultDigest(direct)
	if err != nil {
		t.Fatal(err)
	}
	gatewayDigest, err := resultDigest(response.Rows)
	if err != nil {
		t.Fatal(err)
	}
	if directDigest != gatewayDigest {
		t.Fatalf("equivalent typed rows differ: direct=%s gateway=%s", directDigest, gatewayDigest)
	}
	if directDigest != "8caf12ba5fcc4035a484f705946e1e3317db0a47092b524d0304b0ca9c3f0ea5" {
		t.Fatalf("diagnosed first-run direct digest changed: %s", directDigest)
	}
	var lossy executeResponse
	if err := json.Unmarshal(raw, &lossy); err != nil {
		t.Fatal(err)
	}
	lossyDigest, err := resultDigest(lossy.Rows)
	if err != nil {
		t.Fatal(err)
	}
	if lossyDigest != "3d4a2935d4610351e83705f46d60179abff25867501f41ceb3398c5f88655846" {
		t.Fatalf("diagnosed first-run float64 digest changed: %s", lossyDigest)
	}
}

func TestPrepareNarrowConfigBindsTwentyOrderedIndependentRoots(t *testing.T) {
	cfg := validConfig()
	rowCount, release, influence, outcome := int64(3), int64(12), int64(1_035_000), int64(1)
	cfg.Cases[0] = workloadCase{
		ID: "join-group-max-point-overlap-0", Shape: "join_group", TargetOverlapPercent: 0,
		OverlapDimension: "influence", Plan: json.RawMessage(narrowPlanContract), DirectSQL: narrowDirectSQL,
		Expected: expectedResult{RowCount: &rowCount, ReleaseFacts: &release, InfluenceFacts: &influence, OutcomeFacts: &outcome},
	}
	pool := narrowTaskPool{SchemaVersion: narrowTaskPoolSchema, Dataset: narrowTaskPoolDataset}
	for trial := narrowTaskCount; trial >= 1; trial-- {
		pool.Tasks = append(pool.Tasks, narrowTask{TaskID: "task-" + fmt.Sprint(trial), Trial: trial, Orders: narrowOrderCount})
	}
	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareNarrowConfig(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Cases[0].TaskIDs) != narrowTaskCount || prepared.Cases[0].TaskIDs[0] != "task-1" ||
		prepared.Cases[0].TaskIDs[narrowTaskCount-1] != "task-20" {
		t.Fatalf("prepared task IDs = %#v", prepared.Cases[0].TaskIDs)
	}
	if err := validateConfig(prepared); err != nil {
		t.Fatalf("prepared config is invalid: %v", err)
	}
}

func TestPrepareNarrowConfigRejectsWrongCardinality(t *testing.T) {
	pool := narrowTaskPool{SchemaVersion: narrowTaskPoolSchema, Dataset: narrowTaskPoolDataset,
		Tasks: []narrowTask{{TaskID: "only-one", Trial: 1, Orders: narrowOrderCount}}}
	raw, _ := json.Marshal(pool)
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readNarrowTaskPool(path); err == nil {
		t.Fatal("one-root pool was accepted")
	}
}

func TestCheckedInNarrowTemplateMatchesPinnedPlanAndSQL(t *testing.T) {
	path := filepath.Join(findRepositoryRoot(), "evaluation", "v4-acceptance", "narrow-max-point.template.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateNarrowTemplate(cfg); err != nil {
		t.Fatal(err)
	}
}

func validConfig() config {
	return config{
		SchemaVersion:  configSchema,
		Gateway:        gatewayConfig{URL: "http://gateway:8082", TokenEnv: "TOKEN"},
		BusinessDSNEnv: "BUSINESS_DSN", ControlDSNEnv: "CONTROL_DSN",
		RequestTimeoutMS: 5000, StatementTimeoutMS: 4000, OverlapTolerancePoint: 0.01,
		Cases: []workloadCase{{ID: "scan-0", Shape: "scan", TargetOverlapPercent: 0,
			TaskIDs: []string{"task-1"}, Plan: []byte(`{"product":"expense_detail","columns":["receipt_no"]}`),
			DirectSQL: "SELECT receipt_no FROM reporting.expense_detail"}},
	}
}

func TestValidateConfigRejectsTaskReuseAcrossFreshTrials(t *testing.T) {
	cfg := validConfig()
	second := cfg.Cases[0]
	second.ID = "page-50"
	second.Shape = "page"
	cfg.Cases = append(cfg.Cases, second)
	if err := validateConfig(cfg); err == nil {
		t.Fatal("duplicate task id was accepted")
	}
}

func TestValidateConfigRejectsNullPlan(t *testing.T) {
	cfg := validConfig()
	cfg.Cases[0].Plan = []byte(`null`)
	if err := validateConfig(cfg); err == nil {
		t.Fatal("null plan was accepted")
	}
}

func TestValidateWarmActivationRequiresStrictReceiptBinding(t *testing.T) {
	cfg := validConfig()
	cfg.Activation = &commandMetric{Argv: []string{"activate"}, WarmVerified: true}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "bound receipt") {
		t.Fatalf("unbound warm activation err = %v", err)
	}
	cfg.ActivationVerification = &commandMetric{Argv: []string{"verify", "{{verification_receipt}}"}}
	cfg.Activation.Argv = []string{"activate", "{{verification_receipt}}", "{{verification_receipt_sha256}}"}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("strict receipt-bound activation was rejected: %v", err)
	}
}

func TestReplaceCommandMetricTokenDoesNotMutateConfig(t *testing.T) {
	original := &commandMetric{Argv: []string{"verify", "{{verification_receipt}}"}, Runs: 1}
	replaced := replaceCommandMetricToken(original, "{{verification_receipt}}", "/evidence/receipt.json")
	if replaced == original || replaced.Argv[1] != "/evidence/receipt.json" ||
		original.Argv[1] != "{{verification_receipt}}" {
		t.Fatalf("replacement original=%#v replaced=%#v", original, replaced)
	}
}

func TestOverlapPercentUsesExactChargedVsActualDimension(t *testing.T) {
	value := exposureResult{ActualInfluenceFacts: 100, ChargedInfluenceFacts: 10,
		ActualReleaseFacts: 20, ChargedReleaseFacts: 20, ActualOutcomeFacts: 1, ChargedOutcomeFacts: 1}
	got := overlapPercent(value, "influence")
	if got == nil || math.Abs(*got-90) > 1e-12 {
		t.Fatalf("overlap = %v, want 90", got)
	}
	if got := overlapPercent(value, "all"); got == nil || math.Abs(*got-(90.0/121.0*100)) > 1e-12 {
		t.Fatalf("all-dimensional overlap = %v", got)
	}
}

func TestCoverageNeverTreatsMissingOverlapAsPassing(t *testing.T) {
	cfg := validConfig()
	value := 0.0
	got := buildCoverage(cfg, []sample{{CaseID: "scan-0", Shape: "scan", Phase: "novel", Status: "measured",
		TargetOverlapPercent: 0, ObservedOverlapPercent: &value},
		{CaseID: "scan-0", Shape: "scan", Phase: "semantic_replay", Status: "measured", SemanticReplay: true}})
	if got.Overlaps["0"].Status != "measured" || got.Overlaps["50"].Status != "unmeasured" {
		t.Fatalf("unexpected overlap coverage: %#v", got.Overlaps)
	}
	if got.Shapes["scan"].Status != "measured" || got.Shapes["union"].Status != "unmeasured" {
		t.Fatalf("unexpected shape coverage: %#v", got.Shapes)
	}
}

func TestPercentileUsesHyndmanFanTypeSeven(t *testing.T) {
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.95); math.Abs(got-4.8) > 1e-12 {
		t.Fatalf("p95 = %v, want 4.8", got)
	}
}

func TestResultDigestIsOrderIndependentButMultiplicitySensitive(t *testing.T) {
	left, err := resultDigest([][]any{{"b", 2}, {"a", 1}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := resultDigest([][]any{{"a", 1}, {"b", 2}})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := resultDigest([][]any{{"a", 1}, {"b", 2}, {"b", 2}})
	if err != nil {
		t.Fatal(err)
	}
	if left != right || left == duplicate {
		t.Fatalf("multiset digests left=%s right=%s duplicate=%s", left, right, duplicate)
	}
}

func TestByteSizeRejectsSymlinkAndDoesNotDoubleCount(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(file, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := byteSize([]string{dir, file})
	if err != nil || value != 4 {
		t.Fatalf("size = %d, err=%v", value, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := byteSize([]string{dir}); err == nil {
		t.Fatal("artifact symlink was accepted")
	}
}

func TestBitmapAndStreamGatesRemainUnmeasuredWithoutHonestTimers(t *testing.T) {
	cfg := validConfig()
	result := report{Coverage: buildCoverage(cfg, nil)}
	gates := evaluateGates(cfg, result)
	statuses := make(map[string]string)
	for _, one := range gates {
		statuses[one.ID] = one.Status
	}
	if statuses["bitmap_derivation_end_to_end"] != "unmeasured" || statuses["ordinal_stream_end_to_end"] != "unmeasured" {
		t.Fatalf("unsupported timers were promoted: %#v", statuses)
	}
}

func TestBitmapAndStreamGatesAcceptIndependentProductionTimers(t *testing.T) {
	cfg := validConfig()
	result := report{Coverage: buildCoverage(cfg, nil), Summaries: []summary{{
		Phase: "novel", Samples: 1, ComponentMS: map[string]distribution{
			"bitmap_derivation":     {Count: 1, P50: 12},
			"ordinal_stream":        {Count: 1, P50: 20},
			"provenance_postgresql": {Count: 1, P50: 8},
		},
	}}}
	statuses := make(map[string]string)
	for _, one := range evaluateGates(cfg, result) {
		statuses[one.ID] = one.Status
	}
	if statuses["bitmap_derivation_end_to_end"] != "pass" || statuses["ordinal_stream_end_to_end"] != "pass" {
		t.Fatalf("independent timers were not accepted: %#v", statuses)
	}
}

func TestOrdinalStreamGateRequiresIndependentPostgreSQLTimer(t *testing.T) {
	cfg := validConfig()
	result := report{Coverage: buildCoverage(cfg, nil), Summaries: []summary{{
		Phase: "novel", Samples: 1, ComponentMS: map[string]distribution{
			"bitmap_derivation": {Count: 1, P50: 12},
			"ordinal_stream":    {Count: 1, P50: 20},
		},
	}}}
	for _, one := range evaluateGates(cfg, result) {
		if one.ID == "ordinal_stream_end_to_end" && one.Status != "unmeasured" {
			t.Fatalf("stream gate without PostgreSQL leaf = %s, want unmeasured", one.Status)
		}
	}
}

func TestOrdinalTimingValidationRejectsPlaceholderAndIncoherentTimers(t *testing.T) {
	valid := map[string]float64{
		"provenance_postgresql":       12,
		"ordinal_stream":              17,
		"ordinal_stream_consumer":     5,
		"ordinal_visible_preparation": 3,
		"ordinal_finish":              7,
		"bitmap_derivation":           15,
	}
	if err := validateOrdinalTimingComponents(valid); err != nil {
		t.Fatalf("valid timers: %v", err)
	}
	placeholder := make(map[string]float64, len(valid))
	for name := range valid {
		placeholder[name] = 0
	}
	if err := validateOrdinalTimingComponents(placeholder); err == nil {
		t.Fatal("zero placeholder timers were accepted")
	}
	incoherent := make(map[string]float64, len(valid))
	for name, value := range valid {
		incoherent[name] = value
	}
	incoherent["bitmap_derivation"]++
	if err := validateOrdinalTimingComponents(incoherent); err == nil {
		t.Fatal("incoherent aggregate timer was accepted")
	}
}

func TestLatencyGatesUseOnlyExactMaximumFactPoint(t *testing.T) {
	cfg := validConfig()
	digest := strings.Repeat("a", 64)
	exposureAt := func(release, influence int64) *exposureResult {
		return &exposureResult{ProfileVersion: "taskgate-exposure-v4", ActualReleaseFacts: release,
			ActualInfluenceFacts: influence, ActualOutcomeFacts: 1, ObservationSHA256: digest,
			DictionarySetDigest: digest, ReleaseSetSHA256: digest, InfluenceSetSHA256: digest, OutcomeSetSHA256: digest}
	}
	result := report{Coverage: buildCoverage(cfg, nil), Samples: []sample{
		{Phase: "novel", Status: "measured", ClientLatencyMS: 1, Exposure: exposureAt(1, 1)},
		{Phase: "semantic_replay", Status: "measured", ClientLatencyMS: 1, Exposure: exposureAt(1, 1)},
		{Phase: "novel", Status: "measured", ClientLatencyMS: 5000, Exposure: exposureAt(maxPointReleaseFacts, maxPointInfluenceFacts)},
		{Phase: "semantic_replay", Status: "measured", ClientLatencyMS: 200, Exposure: exposureAt(maxPointReleaseFacts, maxPointInfluenceFacts)},
	}}
	statuses := make(map[string]string)
	for _, one := range evaluateGates(cfg, result) {
		statuses[one.ID] = one.Status
	}
	if statuses["novel_latency"] != "fail" || statuses["semantic_replay_latency"] != "fail" {
		t.Fatalf("small queries diluted maximum-point SLO: %#v", statuses)
	}

	result.Samples = result.Samples[:2]
	statuses = make(map[string]string)
	for _, one := range evaluateGates(cfg, result) {
		statuses[one.ID] = one.Status
	}
	if statuses["novel_latency"] != "unmeasured" || statuses["semantic_replay_latency"] != "unmeasured" {
		t.Fatalf("missing maximum point was accepted: %#v", statuses)
	}
}

func TestSourceDigestFindsRepositoryFromPackageDirectory(t *testing.T) {
	if got := sourceDigest(); len(got) != 64 {
		t.Fatalf("source digest = %q", got)
	}
}

func TestSourceDigestUsesEmbeddedValueWithoutRuntimeSourceTree(t *testing.T) {
	originalDigest := embeddedSourceDigest
	embeddedSourceDigest = strings.Repeat("ab", 32)
	t.Cleanup(func() { embeddedSourceDigest = originalDigest })

	originalWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWorkingDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if got := sourceDigest(); got != embeddedSourceDigest {
		t.Fatalf("source digest = %q, want embedded %q", got, embeddedSourceDigest)
	}
}

func TestSourceDigestFromRootFailsClosedOnIncompleteTree(t *testing.T) {
	repositoryRoot := t.TempDir()
	for _, root := range sourceDigestRoots {
		directory := filepath.Join(repositoryRoot, root)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "placeholder.go"), []byte("package placeholder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if digest, err := sourceDigestFromRoot(repositoryRoot); err == nil {
		t.Fatalf("incomplete source tree produced digest %q", digest)
	}
}

func TestEnvironmentManifestRequiresDigestAndAllEvidenceSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "environment.json")
	raw := []byte(`{"host":{"cpu":"x"},"software":{"go":"1"},"database":{"postgres":"16"},"datasets":{"snapshot":"digest"}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	measured := measureEnvironment(&artifactReference{Path: path, SHA256: sha256Hex(raw)})
	if measured.Status != "measured" {
		t.Fatalf("valid manifest = %#v", measured)
	}
	failed := measureEnvironment(&artifactReference{Path: path, SHA256: sha256Hex([]byte("different"))})
	if failed.Status != "failed" {
		t.Fatalf("digest mismatch = %#v", failed)
	}
}
