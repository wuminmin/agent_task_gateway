package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func TestClearedProfilesUsesOnlyTheThreePreActivationStates(t *testing.T) {
	digest := strings.Repeat("a", 64)
	registry := finalv5profile.Registry{SchemaVersion: 1, RegistryVersion: "registry-v1",
		ContractRelease: "contract-v1", Profiles: []finalv5profile.Profile{
			{ID: "profile-b", Alias: "included", CatalogSHA256: digest,
				Closure: finalv5profile.Closure{Products: []string{"product-b"}, SHA256: digest},
				Status: finalv5profile.ProfileStatus{ClosureComplete: true, CatalogMaterializable: true,
					LiveRouteAvailable: true, ActivationSupported: false, TargetedValidationPassed: false},
				Routable: false, TargetedRunEligible: false},
			{ID: "profile-a", Alias: "no-route", CatalogSHA256: digest,
				Closure: finalv5profile.Closure{Products: []string{"product-a"}, SHA256: digest},
				Status:  finalv5profile.ProfileStatus{ClosureComplete: true, CatalogMaterializable: true}},
		}}
	profiles, err := clearedProfiles(registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Alias != "included" {
		t.Fatalf("cleared profiles = %+v", profiles)
	}
	_, err = deriveProbePlan(registry, digest, digest)
	if err == nil || !strings.Contains(err.Error(), "no outside Product") {
		t.Fatalf("single-profile universe should have no outside probe, got %v", err)
	}
}

func TestValidateIntersectionRejectsAnyDrift(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	if inputs.registrySHA256 != "155a116c9aa37850de11cc7846aeb5d4e9cb202c8255b1e73a4c7b2fe393a9ed" {
		t.Fatalf("registry digest = %s", inputs.registrySHA256)
	}
	if inputs.intersectionSHA256 != "ea8f083fdb986684b8136b55f09b5bb8bb15ea4a04d2e4e8b6faccb8521ad71a" {
		t.Fatalf("intersection digest = %s", inputs.intersectionSHA256)
	}
	for name, mutate := range map[string]func(*intersectionReport){
		"profile count": func(report *intersectionReport) { report.ProfileCount++ },
		"pair order": func(report *intersectionReport) {
			report.Pairs[0], report.Pairs[1] = report.Pairs[1], report.Pairs[0]
		},
		"alias":         func(report *intersectionReport) { report.Pairs[0].LeftAlias = "invented" },
		"products":      func(report *intersectionReport) { report.Pairs[0].LeftProducts = []string{"invented"} },
		"applicability": func(report *intersectionReport) { report.Pairs[0].SameQueryLiveTestApplicable = true },
	} {
		t.Run(name, func(t *testing.T) {
			report := inputs.intersection
			report.Pairs = append([]intersectionPair(nil), inputs.intersection.Pairs...)
			mutate(&report)
			if err := validateIntersection(inputs.registry, report); err == nil {
				t.Fatal("drifted product-intersection matrix was accepted")
			}
		})
	}
}

func TestV13GoldenReproducesTheRouteMatrix(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProfileCount != 7 || plan.UniqueProductCount != 9 || plan.ExpectedProbeCount != 54 {
		t.Fatalf("golden plan = profiles:%d products:%d probes:%d", plan.ProfileCount,
			plan.UniqueProductCount, plan.ExpectedProbeCount)
	}
	evidenceDirectory := filepath.Join(root, "evidence")
	writeGoldenEvidence(t, evidenceDirectory)
	probes, err := aggregateEvidence(evidenceDirectory, plan, inputs.registry)
	if err != nil {
		t.Fatal(err)
	}
	matrix, err := buildRouteMatrix(inputs.registry.ContractRelease, inputs.registrySHA256,
		inputs.intersectionSHA256, plan, probes)
	if err != nil {
		t.Fatal(err)
	}
	if matrix.MatrixSHA256 != "f3163dc62d99eaaf55e400d646f299ab079a91981fb9d4ef84c09863bb8cfcae" {
		t.Fatalf("matrix_sha256 = %s", matrix.MatrixSHA256)
	}
	if matrix.ExecutedProbeCount != 54 || matrix.PassedProbeCount != 54 ||
		matrix.FailedProbeCount != 0 || matrix.Status != "pass" {
		t.Fatalf("golden counts/status = %+v", matrix)
	}
	expectedPerProfile := map[string]int{
		"attack-expense-detail":      8,
		"concurrency-expense-detail": 8,
		"expense-detail":             8,
		"provsql-nonce-join":         6,
		"result-heavy":               8,
		"rls-bounded":                8,
		"rls-unlimited":              8,
	}
	actualPerProfile := map[string]int{}
	trueNegativeAssertions := 0
	for _, probe := range matrix.Probes {
		actualPerProfile[probe.TargetProfileAlias]++
		if probe.StableRefusalClassification != "tool_error" {
			t.Fatalf("golden classification for %s/%s = %q", probe.TargetProfileAlias,
				probe.RequestedProduct, probe.StableRefusalClassification)
		}
		for _, assertion := range []bool{probe.CatalogListAbsent, probe.LiveRequestRefused,
			probe.NoActiveTask, probe.NoArtifact, probe.NoAvailable, probe.NoBusinessSQL,
			probe.NoObservation, probe.NoReceipt, probe.NoRootLedgerChange, probe.NoSemanticCacheHit} {
			if !assertion {
				t.Fatalf("golden negative assertion is false for %s/%s", probe.TargetProfileAlias,
					probe.RequestedProduct)
			}
			trueNegativeAssertions++
		}
	}
	if !reflect.DeepEqual(actualPerProfile, expectedPerProfile) {
		t.Fatalf("golden per-profile probe counts = %v, want %v", actualPerProfile, expectedPerProfile)
	}
	if trueNegativeAssertions != 540 {
		t.Fatalf("golden true negative assertions = %d", trueNegativeAssertions)
	}
	encoded, err := encodeRouteMatrix(matrix)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if got := hex.EncodeToString(digest[:]); got != "342d257718a58bf3e084c7d776f5dc169b6a9d67f5063f72caff5cc1d1694bad" {
		t.Fatalf("route matrix file SHA-256 = %s", got)
	}
	if len(encoded) != 45794 {
		t.Fatalf("route matrix byte length = %d", len(encoded))
	}
}

func TestParseFlagsHelpAndDeriveOnly(t *testing.T) {
	var help bytes.Buffer
	if _, err := parseFlags([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h error = %v", err)
	}
	for _, flagName := range []string{"-derive-only", "-mode", "-activation-evidence-dir",
		"-activator-binary", "-probe-token-env", "-profile-alias"} {
		if !strings.Contains(help.String(), flagName) {
			t.Fatalf("help omitted %s:\n%s", flagName, help.String())
		}
	}
	opts, err := parseFlags([]string{"-mode", "live", "-derive-only"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.mode != "plan" || !opts.deriveOnly {
		t.Fatalf("-derive-only options = %+v", opts)
	}
}

func TestRouteMatrixSchemaSerializationAndDigest(t *testing.T) {
	digest := strings.Repeat("a", 64)
	product := "outside"
	probe := passingRouteProbe(product, digest, "profile", "profile-a", digest)
	plan := probePlan{ProfileCount: 1, UniqueProductCount: 2, ExpectedProbeCount: 1}
	matrix, err := buildRouteMatrix("contract", digest, digest, plan, []routeProbe{probe})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeRouteMatrix(matrix)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 14 {
		t.Fatalf("top-level field count = %d", len(document))
	}
	projected := document["probes"].([]any)[0].(map[string]any)
	if len(projected) != 17 {
		t.Fatalf("probe field count = %d", len(projected))
	}
	withoutDigest := matrix
	withoutDigest.MatrixSHA256 = ""
	canonical, err := json.MarshalIndent(withoutDigest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(canonical)
	if matrix.MatrixSHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatal("matrix_sha256 did not hash MarshalIndent with matrix_sha256 omitted")
	}
	text := string(encoded)
	ordered := []string{`"contract_release"`, `"executed_probe_count"`, `"expected_probe_count"`,
		`"failed_probe_count"`, `"matrix_sha256"`, `"passed_probe_count"`, `"probes"`,
		`"product_intersection_matrix_sha256"`, `"profile_count"`, `"profile_registry_sha256"`,
		`"record"`, `"schema_version"`, `"status"`, `"unique_product_count"`}
	last := -1
	for _, field := range ordered {
		position := strings.Index(text, field)
		if position <= last {
			t.Fatalf("field %s is out of serialization order", field)
		}
		last = position
	}
}

func TestFailedProbeCannotProducePass(t *testing.T) {
	digest := strings.Repeat("b", 64)
	probe := passingRouteProbe("outside", digest, "profile", "profile-a", digest)
	probe.NoSemanticCacheHit = false
	matrix, err := buildRouteMatrix("contract", digest, digest,
		probePlan{ProfileCount: 1, UniqueProductCount: 2, ExpectedProbeCount: 1}, []routeProbe{probe})
	if err != nil {
		t.Fatal(err)
	}
	if matrix.Status != "fail" || matrix.PassedProbeCount != 0 || matrix.FailedProbeCount != 1 {
		t.Fatalf("failed probe summary = status:%s passed:%d failed:%d", matrix.Status,
			matrix.PassedProbeCount, matrix.FailedProbeCount)
	}
}

func TestStableRefusalClassificationRequiresThePredeclaredToolError(t *testing.T) {
	for value, want := range map[string]bool{
		"tool_error":           true,
		"http_403":             false,
		"jsonrpc_error_-32000": false,
		"http_204":             false,
		"request_accepted":     false,
		" tool_error ":         false,
	} {
		if got := stableRefusalClassification(value); got != want {
			t.Errorf("stableRefusalClassification(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestLiveSubprocessFailureDoesNotWritePass(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "matrix.json")
	const sentinel = "existing output must survive\n"
	if err := os.WriteFile(output, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := options{mode: "live", root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath, evidenceDir: "live-evidence", outputPath: output,
		composeProject: "project", composeFiles: "compose.yaml", deploymentID: "deployment",
		gatewayURL: "http://gateway", adminTokenEnv: "ADMIN", profileArtifactRoot: "artifacts/profiles",
		profileArtifactManifest: "artifacts/manifest.json", businessDSNEnv: "DSN",
		schemaAttestations: "config/profiles/schema-attestations-v1.json", probeTokenEnv: "TOKEN",
		readyTimeout: timeMinute, activationSequenceStart: 10}
	prepareLiveEnvironment(t, opts, plan)
	calls := 0
	runner := func(_ context.Context, directory, executable string, arguments []string,
		_, _ io.Writer) error {
		calls++
		if directory != root || executable != "go" {
			t.Fatalf("subprocess directory/executable = %s / %s", directory, executable)
		}
		joined := strings.Join(arguments, " ")
		for _, required := range []string{"final-v5-profile-activate", "-outside-products", "-profile-artifact-dir",
			"-profile-artifact-manifest", "-business-dsn-env", "-schema-attestations", "-probe-token-env"} {
			if !strings.Contains(joined, required) {
				t.Fatalf("subprocess omitted %s: %s", required, joined)
			}
		}
		return errors.New("live deployment unavailable")
	}
	if err := executeLive(context.Background(), opts, plan, io.Discard, runner); err == nil {
		t.Fatal("live subprocess failure was ignored")
	}
	if calls != 1 {
		t.Fatalf("subprocess calls = %d", calls)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != sentinel {
		t.Fatal("subprocess failure rewrote the output artifact")
	}
}

func TestAggregateRequiresFullEvidenceAndOneContiguousDeploymentChain(t *testing.T) {
	for name, mutate := range map[string]func(*finalv5profile.ActivationEvidence){
		"shared validator": func(evidence *finalv5profile.ActivationEvidence) {
			evidence.SchemaAttestationStatus = "unverified"
		},
		"deployment identity": func(evidence *finalv5profile.ActivationEvidence) {
			evidence.DeploymentID = "another-deployment"
		},
		"sequence continuity": func(evidence *finalv5profile.ActivationEvidence) {
			evidence.ActivationSequence += 20
		},
		"sequence uniqueness": func(evidence *finalv5profile.ActivationEvidence) {
			evidence.ActivationSequence--
		},
		"previous profile chain": func(evidence *finalv5profile.ActivationEvidence) {
			evidence.PreviousProfileID = "profile-not-the-previous-activation"
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, inputs, plan, evidenceDirectory := goldenAggregateFixture(t)
			mutateGoldenEvidence(t, evidenceDirectory, "expense-detail", mutate)
			if _, err := aggregateEvidence(evidenceDirectory, plan, inputs.registry); err == nil {
				t.Fatal("mutated activation evidence was accepted")
			}
		})
	}
}

func TestGenericTransportRefusalWritesFailMatrixAndReturnsFailure(t *testing.T) {
	root, inputs, _, evidenceDirectory := goldenAggregateFixture(t)
	mutateGoldenEvidence(t, evidenceDirectory, "expense-detail", func(evidence *finalv5profile.ActivationEvidence) {
		evidence.OutsideProduct[0].Classification = "http_403"
	})
	output := filepath.Join(root, "failed-matrix.json")
	opts := options{mode: "aggregate", root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath, evidenceDir: evidenceDirectory, outputPath: output}
	err := execute(context.Background(), opts, io.Discard, io.Discard, nil)
	if !errors.Is(err, errRouteMatrixFailed) {
		t.Fatalf("aggregate error = %v, want route-matrix failure sentinel", err)
	}
	payload, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var matrix routeMatrix
	if err := decodeStrict(payload, &matrix); err != nil {
		t.Fatal(err)
	}
	if matrix.Status != "fail" || matrix.FailedProbeCount != 1 || matrix.PassedProbeCount != 53 ||
		matrix.ProfileRegistrySHA256 != inputs.registrySHA256 {
		t.Fatalf("failed aggregate matrix = %+v", matrix)
	}
}

func TestUnsafeAliasCannotBecomeAnEvidencePath(t *testing.T) {
	digest := strings.Repeat("a", 64)
	registry := finalv5profile.Registry{Profiles: []finalv5profile.Profile{{ID: "profile-a",
		Alias: "../escape", CatalogSHA256: digest, Closure: finalv5profile.Closure{
			SHA256: digest, Products: []string{"product"}}, Status: finalv5profile.ProfileStatus{
			ClosureComplete: true, CatalogMaterializable: true, LiveRouteAvailable: true}}}}
	if _, err := clearedProfiles(registry); err == nil {
		t.Fatal("path-traversing profile alias was accepted")
	}
}

func TestLivePreflightSkipsMissingEnvironmentWithoutWritingOrRunning(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	opts := liveTestOptions(root)
	prepareLiveEnvironment(t, opts, plan)
	t.Setenv(opts.adminTokenEnv, "")
	calls := 0
	err = executeLive(context.Background(), opts, plan, io.Discard,
		func(context.Context, string, string, []string, io.Writer, io.Writer) error {
			calls++
			return nil
		})
	if !errors.Is(err, errLiveEnvironmentSkipped) {
		t.Fatalf("missing environment error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("live runner called %d times after failed preflight", calls)
	}
	if _, err := os.Stat(filepath.Join(root, "matrix.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-environment preflight wrote matrix: %v", err)
	}
}

func TestLivePreflightSkipsMissingArtifactInputs(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	opts := liveTestOptions(root)
	t.Setenv(opts.adminTokenEnv, "admin-token")
	t.Setenv(opts.probeTokenEnv, "probe-token")
	t.Setenv(opts.businessDSNEnv, "postgres://test")
	calls := 0
	err = executeLive(context.Background(), opts, plan, io.Discard,
		func(context.Context, string, string, []string, io.Writer, io.Writer) error {
			calls++
			return nil
		})
	if !errors.Is(err, errLiveEnvironmentSkipped) || !strings.Contains(err.Error(), "profile artifact root") {
		t.Fatalf("missing artifact error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("live runner called %d times without artifact inputs", calls)
	}
}

func TestLivePreflightChecksEveryEvidenceTargetBeforeFirstActivation(t *testing.T) {
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	opts := liveTestOptions(root)
	prepareLiveEnvironment(t, opts, plan)
	evidenceDirectory := filepath.Join(root, opts.evidenceDir)
	if err := os.MkdirAll(evidenceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	last := plan.Profiles[len(plan.Profiles)-1]
	if err := os.WriteFile(filepath.Join(evidenceDirectory, last.ProfileAlias+".json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = executeLive(context.Background(), opts, plan, io.Discard,
		func(context.Context, string, string, []string, io.Writer, io.Writer) error {
			calls++
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("existing later evidence target error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("live runner called %d times before discovering an existing target", calls)
	}
}

const timeMinute = 60 * 1e9

func passingRouteProbe(product, responseDigest, alias, profileID, catalogDigest string) routeProbe {
	return routeProbe{CatalogListAbsent: true, LiveRequestRefused: true, NoActiveTask: true,
		NoArtifact: true, NoAvailable: true, NoBusinessSQL: true, NoObservation: true,
		NoReceipt: true, NoRootLedgerChange: true, NoSemanticCacheHit: true,
		RequestedProduct: product, RequestedProductSHA256: requestedProductSHA256(product),
		ResponseSHA256: responseDigest, StableRefusalClassification: "tool_error",
		TargetCatalogSHA256: catalogDigest, TargetProfileAlias: alias, TargetProfileID: profileID}
}

func writeV13Inputs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeCompressedFixture(t, filepath.Join(root, defaultRegistryPath), v13RegistryGZIPBase64)
	writeCompressedFixture(t, filepath.Join(root, defaultIntersectionPath), v13IntersectionGZIPBase64)
	return root
}

func writeCompressedFixture(t *testing.T, path, encoded string) {
	t.Helper()
	encoded = strings.Map(func(value rune) rune {
		if value == '\n' || value == '\r' || value == '\t' || value == ' ' {
			return -1
		}
		return value
	}, encoded)
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGoldenEvidence(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "v1.3", "routes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".base64" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join("testdata", "v1.3", "routes", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(entry.Name(), ".json.gz.base64") + ".json"
		writeCompressedFixture(t, filepath.Join(directory, name), string(encoded))
	}
}

func TestGoldenRawActivationFixturesAreComplete(t *testing.T) {
	directory := t.TempDir()
	writeGoldenEvidence(t, directory)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	wantDigests := map[string]string{
		"attack-expense-detail.json":      "c0b1dfc1b3045e34f25c12065a8d78a5a06f21070f0ac18e92725ef12710e730",
		"concurrency-expense-detail.json": "71ddd98a43f179a973c020a20d59a2f14b9df7371cbf4df78928f1d0fb1ab39a",
		"expense-detail.json":             "f66a98b73165a064aa8807490f5de1d2e1c7e6c86b177031fd3ec82f2cc04c15",
		"provsql-nonce-join.json":         "f7f9e2d00c7eb6aab11c8a195098a7f2cb83e0dad555208ab403d412c2bce4ef",
		"result-heavy.json":               "470f2ae0f5a4d47dc5ff74fbd169073ec73951f18f3a9e1941c4e2da36574f66",
		"rls-bounded.json":                "7456a12114730016451ebf8a2899f6b8d31cee656001a2dfca8d2ad98e5dce1b",
		"rls-unlimited.json":              "f631e1cfd858042ed270464df7af21d2fbda4a0e1ee52c749a3254462893eb71",
	}
	probes := 0
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var evidence finalv5profile.ActivationEvidence
		if err := decodeStrict(payload, &evidence); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		if got := hex.EncodeToString(digest[:]); got != wantDigests[entry.Name()] {
			t.Fatalf("raw activation fixture %s digest = %s", entry.Name(), got)
		}
		for _, probe := range evidence.OutsideProduct {
			if probe.Classification != "tool_error" || !validSHA256(probe.ResponseSHA256) {
				t.Fatalf("non-canonical raw probe in %s: %+v", entry.Name(), probe)
			}
			probes++
		}
	}
	if len(entries) != 7 || probes != 54 {
		t.Fatalf("raw activation fixtures = %d records / %d probes", len(entries), probes)
	}
}

func goldenAggregateFixture(t *testing.T) (string, inputSet, probePlan, string) {
	t.Helper()
	root := writeV13Inputs(t)
	inputs, err := loadInputs(options{root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "evidence")
	writeGoldenEvidence(t, directory)
	return root, inputs, plan, directory
}

func mutateGoldenEvidence(t *testing.T, directory, alias string,
	mutate func(*finalv5profile.ActivationEvidence)) {
	t.Helper()
	path := filepath.Join(directory, alias+".json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evidence finalv5profile.ActivationEvidence
	if err := decodeStrict(payload, &evidence); err != nil {
		t.Fatal(err)
	}
	mutate(&evidence)
	payload, err = json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func liveTestOptions(root string) options {
	return options{mode: "live", root: root, registryPath: defaultRegistryPath,
		intersectionPath: defaultIntersectionPath, evidenceDir: "live-evidence",
		outputPath: "matrix.json", composeProject: "project", composeFiles: "compose.yaml",
		deploymentID: "deployment", gatewayURL: "http://gateway", adminTokenEnv: "ADMIN",
		profileArtifactRoot: "artifacts/profiles", profileArtifactManifest: "artifacts/manifest.json",
		businessDSNEnv: "DSN", schemaAttestations: "config/profiles/schema-attestations-v1.json",
		probeTokenEnv: "TOKEN", readyTimeout: timeMinute, activationSequenceStart: 10}
}

func prepareLiveEnvironment(t *testing.T, opts options, plan probePlan) {
	t.Helper()
	t.Setenv(opts.adminTokenEnv, "admin-token")
	t.Setenv(opts.probeTokenEnv, "probe-token")
	t.Setenv(opts.businessDSNEnv, "postgres://test")
	artifactRoot := resolvePath(opts.root, opts.profileArtifactRoot)
	for _, profile := range plan.Profiles {
		if err := os.MkdirAll(filepath.Join(artifactRoot, profile.ProfileID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := resolvePath(opts.root, opts.profileArtifactManifest)
	if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const v13RegistryGZIPBase64 = `H4sIAAAAAAACA+1dSW/jSJa+568gfJlLshT7Unnq6e6ZvnUD1bfBQIjVZlsmlSTlTHeh/vs8UqJWSiLl3DxJFFBpU2+PeO99EcGQf3+XJHeVewhPZv4cyior8rtfE/y+eVyG+6yqy5e9D+5qUz3emzqkMcvNIn3m6aeifFwUxqduUVSrMqTLsojZIqTP5K4V44q8Lo2r52VYBFOFRsyWu/uwSp/xL3TN8GQ+zx+Ker7InrJ6bl/qUDU2CSklwQL10FSuWLZyl6FM/mxqsyjuU1uscp/8Nxj7ybwkWV7VJnch+dvf/5m0XGtlG2sbDf8DvyfJ7+3/d5/MM99K3niFkHUqSs8FpZFr0Upp6c0iM42Yu3JRpau81RH87vNNfICiU7FW4lfg/lb95nkboPkzn4Ow+VbYPHxehrwKcx9qky3uthz/+35P5MouMmdqGLETsRv+dM0PMe8XURWr0oUTbhip57AA5U/FGb5mHE7YfFiasn4Ked3PtQnMvHowhIsmgsdBZpzx4LnDJkRPMUGBCo6JQZp5j7SQwiGhpcM8Yu+Q6fT8sQ0+DH69qg5j3+l1xdNyEepmZOpyFfYtW0+l+RNMojKDAf63sYseukX2HOZlsarD3DxDYPupYJpnz+24zKvVclmUzfS4SPVUPIb50lRVH2FtyvvQTIpnMMyvOba00SyqfeJVXoaqWDwDeQlJeDo1ft/7eROvsE74s1reH7K4wh9ylKt8nkOOhs/BrepThs0kBpa8SDquJC/ydG8KJyAleTBV0klJ6oesSjbp+B9V4sJiUd3tSf5jN8lOpkEzRpvBOYjQodFhkd1np0N4Z1ceiOYnJWOXsU1JO0j/vRTbTvm7rmTO17YfiAHumfFmCZMgpBghEDBr/vm4ghkYqhl8vhfHq+SndWjDtCwgwi9QCPIMrGzLcLGYVVl+vwh9Si7S77Sc+NpUnDJrkv/U0VPqLuF2pYAFgzHUAeNwwIxS+MV6IgNhVHNvrCSBOCsE5dYYqBsEWROwcEx57q3Vdyeyl6Z+aCSDDzG7n3WjOTsYt1821L+8mKfFTkbTcqCWZdEc1+z9/NkvwY2m06q7nwmt0Cz34fPcZ/ehqhsWE4i20WvEjVPKSisc9YxK+M9p76KBaETFJHaUauVoUEQJGayIzCMh+aGGXQ8VVL07TpRd+OsCnG776gHH9vMBnXk9tuHjKishm/aK2XoCtf0RfIQgJuZMq4ak38/xjupDAjgki9DK6yr57W9/SmGGfEg2GgJwLV7WfJu6/iHJoOI1H9UPAYSF56xY7VeOBgk4A9jnfVIB/snrzHW//2OvhyYGbPsn4J7EwijBhK8+JGVwRemTnXsJSIfccOHD/sOGc+ttGZI8gAtJ0w2TIoL/T1CKwVKfVKZpQbt59ikDT/JdrDeV6N1eNbuGU7xSjgXtYfJwgW0PTjmcljcClZ8FkRyF0zGJuHNQgwwRykZIE2adwNoEqrGnXDJHo6bG+0ijxRMimRDJt0Ykm5SCyrZqkck4IGLq2rjH2V/T+gEG6qFYeGiVYb0mhH8aZDPzUORdvQ8VhnDlBeTsAb74yGdQvrM8pJDQeZU1YW4oN/DCrrKFH8NQNp7no+DI2vAjJUMAiibMU8WDgLIQg7fMKsElIhEbxHSwBLpjpIgQxpRm0ljMGRNKYiy5ktoPByiHpXJCKBNCedMIBXPlRIzOuRAJVbQHoayTMv0iQGW7o7IW+rNspRxH2WguA4Ni5JTVPASjqFQWIU0cJjRKR5AUBsi9gZWVjRNwmYDLd9tK2eT/U/DZ6mn0VsoGi/wpXZp7kNiEYNZN0LQumsfQVc9imCt8Jyimj60lbXg6/oHqTvnOqfvPtOlkMKOgJqTVx8Xs2UB65e2G+nllF7nOqfpz2nRNaBZp5mfQD0IK3SP1WYyA7PJ6Bu3laVnU8CMkxnJhXm6Qcavurj2e1/yXtFousjpd5cfj2T4/H6trjOdMPuRb/zxoJlxjXCscDW0HYFmHCEPSYcE09yQKAqhOOKdx5DHQGKxBRDL4CUmmgkUqAqyFh95w6Uyww7Fsb2efIO0Ead82pFUAorx2zDvEouNnDgfbyH+Zo8GNqJ8GzR4FWGvDI8OUY0hQBRWKMapdSySwiNrHCMnJmfWSO6z8hGYnNPtdDwY3+ZpK9MVPBo+rypCDu47nix/b6aiwNb5pnYKi6IL1kI7c2xB1AyZiJDFoJyEtpeAqYgStljgWNSNa2zDu2G7jxoQfJvzwpvED1UxGjlSIPGgYoB78ANPfrUpY80BOf9l9sT3JPwucOI639NFRLYQXhjgULBMWGyp4xAojq42SWiMKNcsSG8P0ntEEJ74jnNivBGOxxB7vrGqn3BYZ4M2DfRxxQP5gmnO8sihqgB+zWEAG+zmAkBVklomhfhnImUOmlFB6GsWQs03bHcT4Cp23KuU36+S3q3yFzl6lwzHenuAhWI96HgUVjJPopCYCU+WthCUYrNCckcZ7BYBPGucoVFsAQFwQ6+BjZTwRCA3Heueb3wT9Juj3pqEfQA1CqJAeskdw0Qf9AN/UDyztfE+fs/Dp1Yeh8O8LCKvma+mH8xDYn6uPi3nzpkVWh6f+TyGyoazGg8QNf9pJP0m0jmCt4BuhyM6aeTPs7Ysl88fwcmjY2qBLFIcfDYSjx3NAoeCJhBVxhMIhNWLGAChV0QQdNBc2Sh0Ah2LnrTDYkK8OR4/R3hk8ekx2BpBeIjtEpMeU3waS9pp9Boy2kThx4BIqrVb2X835z5AacAJk+0do3rr+IbluzIbmaOg6/ryYm2WTfFAj1iSARIC3ucSzqSjzbdHuHhhbNSd/sSye5q1w17WBtYhWPUhctZNkH0e/HzAWZ6bkmdG4ZtOFsRhWGQ8Go2lXjfSu8SW+CBUEsU7a6lc9rNvcP9ZGjXa9N8nOON4X6ot2b4YRWm9r8GF4oc8W8HjtXCsa+m2e2JB0GTXamZ98mXdIdbrOy1eLxaAFnDVVaDrV7Dc2a+dnynoOlPuoTo6r+4iOj9OHrx06aUMWDueB/3kEfxChU/yLJuz7A2DfdpqPAL/ctmtBg4hrXo71fa8CduW4Q4MnmHQcAv4Zoe1NoPR4bJB0njhFJSPKBhmajdFmOzTCKtZzHzVTQkVY3iKuAlXoB9kjnTDpbZj0bGqedN2vgzq3BXIEKP2qEOuimZcDelBw3h/VmOHwcjN1GsSWdKZs4BmYsu4rbVw6yNl1mV+TLG9n5xHbr42knQDAzM2Niuesfpnw3XfHd2T223/hi9huTXHx7ctj4rwon5oS2tafTyXMx0FMZ5HjmuDCS5hHpOi6Q2iUR+gml9BVn9A3gsLCB28V9spbjiSOVGropRhFeMS1lJg4J6DbKqcCksjZ4IMVgTPBJIp8xC2is0Dqi26hXwVAfZvognsXsUPBRxJEdJFFxQliViEUJRXKE4wFU85F4qilkRgXVAzGMUKJx2c20RFGiEHQ3vVUsiE+7DDaVQ8Usj4EAeowNRAxbghjWgYplREUTMZEkEikYj5KFa3B2GKjFZiItXb47DEAogSJcScBWAokiRTTiujtnQZIzUJ0BHMdozE89t/eXn/fTuXMvvLbzgE6afO1tNELnu2p9KFZP/LC5jjGSjIkoLzSQIjAyFMsjGSQx0gQ+MkpTJGhlERLBNJITLvtb3tlczbH3vZu+16W/r/bkz+qUt9rR36k4wMG5Jrz/ZX1Bv99cIuml63938M706nEtGr9gqtWOsPoMeXo8eJCb0c1cK23Yxi53NtnPLvi2xENWskCOVBf9bClGe5fSz7euw3bJd9akoGeMf6YEnLVuS3ZYP+2HKNd3OO84OWW6oKjbQFtzrpC7tfvkpHQvB34mILkcmGWac/GwBCmV+lsX08cr7Vhe5VefpNa/kqt+iat+hatlCOEbhjaY77Xah4zwKecr9XOb1XOX69b36r7pvG+JY1fl8W3JfFrc/imFH5lBt+UwBfz94b91GON07sG07sGg7fWjEBGKukEc5ozjnu21rqNX0DpLqT/KrL8q75k0Kr5Tu8frF18u28nHI+mlNEIy2SgktBgEZaEW4k141YZrIXkNGoUSSQBcSf1dINrusH13W5wbbO0+DT6BteGd7arUek92Lxs2m3Pou0i+ebDwfTdHwEYxjDOnJHWjDSG8VHWNORjzGnot/YMBzadigEwxniuqFJKBi6xMRhKnhESGecdY0pEwYQVNDCDkBdSMExs4I5ipiQnFMvh58SnTXA6ID7jQ38T7XMAk8CYE9Ia5qQlKiobVdRKW6QY11JHjrChyiPhmhMoo2PzFX3ORqYDp6jfASIIjO9Pd7ythJaCTiD87R1vGyWchznCnfEikN4/ohKq1aJOH4J5fnn1FyW1suZrWbcfbe+b9I0wMZgU7otyOBw+CqziHmuKnAnOeRsofM4BDEcXIqURnsIDKx1xXBPr7fT9SBMc/n5w2EKJengy5eNNgLiDILP9NF3vHn/GoudbGi/QsxHk44SPET3S8H67d2cjYheMS4c64kLMTqnYIFnX7Rpk1RCbhlh03Z5hYRoUpUFB2ovRiO/13EyEPrFD1hKcUkUlAE+AmhEzbBBIE0EyqRTgU2J1sIoJjB2OWnhArAIpJCTjlkWJxvxpnb2Z+kVXEWd784AvbGDBtEAVAC3XHDz2VmpYNSkRoGkahajjgUocMSy4UPtHPyRV0mJEkFdnoCy0XEypwOOwbMc1Ydm3h2WjQMFxWF03Szdv0IC7a6/cTX7tpvCPv6t7HFPMZNQe1pWaUGaiYD4wr1WUnBEnqGWSOGUAzlpYf2Lmpztnb/rNzLMpNd05u+3O2XTFbHpZ78LLevjqFTM85ooZvuWKGb52xQwPv2KGr18xw6OumOGbrpjhq1fMcP8Vs15afnWQ+JhB4tfizYfHm1+PNx8Vb341cvxbXs4z2OkohQ0iYOhIwpFAsWWCKawwC7BgCBFpHWiUxlNLKbK2uc4nokIe1gu3X877Kkcub3TDf8M0rZF+qDXSu6ap/fHu/wAV5qweSH4AAA==`

const v13IntersectionGZIPBase64 = `H4sIAAAAAAACA+2Y227jIBCG7/sUka/LKgefsq+yqhCBccqW2Clg71ZV332J47rpwRuMidLUvYlkwwyDv8zMr3m8mkwCRW9hQ3AFUvEiD35OZte71xJoIZl5DDRRd2uiAWU8JwJVEdrKgpVUI55rYwVUG0NUzYLakBa5loRqdeAyaE3bVbP/xyJojlpzpeXDocH7Q/8U8k4UhCEqClVK2EWRcQGomu/dNM+YFmWujY9k/5Zw2b6a7+9WmIME2W55vsav1qetiTJPv8zDZPJY/5rXAjKNnw/h9ad5DmE6XdE0S1gULxZZtIzrgGojyde3nVYsTWkIS5aukiierV6s6qOI4GQXRiCFQmUu+IZrYG9dt7vg7xZyBYiBJly88dUQe7lVvVR/W1xF2JyA2xNw4wk3npr9N+/v9IHLY7aHf5md5ccrr4HUy4psAN+XYP4mgleANShz+e1WcEpWAszejAgF9fan6xOTm0UpjbOMUgrZfJEuhpEjWhN6hz4HwNblPio8EqApnc3ZkoaMTsOMRsOA7natzIXZ4Z4zYdy5bGIZCcvFMkyyaJpCFsEyTpJhLE3DoqWUkNOHz5ahB6GNBC2JpyRJExqHdBmF0WwYWuO1UvcC5eY7Avpd8PxcSJtIzBfOwTjbtHEcrNVRfrRgZJoB+GWRpzFlIcsiSlgM83hgZQZVCo1ugVQP5y/NdTB4H8znx9epVn0opa7S6k0qjVoTuaGz1ESW6PqKom/140TNVv1YUhsof751jhNEW51jCdFR6HwrmtPAtVQ0tnW1r6QZoXbpViE+GuARUeJrOGApPr58X3SDadsX+8H0NR3wyfaS26UbW9t22Y/t0PGAB6aj6KKOzC27aM/i7Dwf8FqdL6u5drZJX1Pad+3yhDNa26749UuxE9Y+E9pjWH3MZ33gHEcVdsPdYzp7DPew2azXvL2sAtxdSn1kqk1x9Zy4fcrm6BPXkb5l4jrQd89jX9wvNI+7M9IHyf8l5VCCJ0m0iyFsfm+unq7+AS4+ZjFOJwAA`
