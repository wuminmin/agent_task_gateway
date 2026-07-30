package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
