package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestArtifactManifestIsDeterministicAndSpecBound(t *testing.T) {
	args := artifactManifestArgs(testSHA('a'), testSHA('b'), testSHA('c'), testSHA('d'))
	codeOne, outputOne, errorsOne := invokeCLI(args, "")
	codeTwo, outputTwo, errorsTwo := invokeCLI(args, "")
	if codeOne != 0 || codeTwo != 0 || errorsOne != "" || errorsTwo != "" {
		t.Fatalf("artifact-manifest codes=%d/%d stderr=%q/%q", codeOne, codeTwo, errorsOne, errorsTwo)
	}
	if outputOne != outputTwo {
		t.Fatal("identical artifact-manifest invocations changed canonical bytes")
	}
	manifest, err := finalv5oracle.DecodeManifest([]byte(outputOne))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExperimentID != "artifact" || manifest.WorkloadID != "result-heavy" || manifest.Scale != "100x4" ||
		manifest.Mode != "novel" || manifest.Generation.Seed != finalv5oracle.ArtifactGeneratorSeed ||
		manifest.Generation.GeneratorVersion != finalv5oracle.ArtifactGeneratorVersion ||
		manifest.Expected.RowCount == nil || *manifest.Expected.RowCount != 100 ||
		manifest.Expected.ColumnCount == nil || *manifest.Expected.ColumnCount != 4 {
		t.Fatalf("artifact manifest = %+v", manifest)
	}

	oneSHA, err := finalv5oracle.ManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutatedArgs := range map[string][]string{
		"dataset/Product":     artifactManifestArgs(testSHA('e'), testSHA('b'), testSHA('c'), testSHA('d')),
		"Catalog/Publication": artifactManifestArgs(testSHA('a'), testSHA('e'), testSHA('c'), testSHA('d')),
		"query":               artifactManifestArgs(testSHA('a'), testSHA('b'), testSHA('e'), testSHA('d')),
		"normalization":       artifactManifestArgs(testSHA('a'), testSHA('b'), testSHA('c'), testSHA('e')),
	} {
		t.Run(name, func(t *testing.T) {
			mutatedCode, mutatedOutput, mutatedErrors := invokeCLI(mutatedArgs, "")
			if mutatedCode != 0 || mutatedErrors != "" {
				t.Fatalf("mutated Spec invocation code=%d stderr=%q", mutatedCode, mutatedErrors)
			}
			if mutatedOutput == outputOne {
				t.Fatal("Spec mutation retained the manifest bytes")
			}
			mutatedManifest, err := finalv5oracle.DecodeManifest([]byte(mutatedOutput))
			if err != nil {
				t.Fatal(err)
			}
			mutatedSHA, err := finalv5oracle.ManifestSHA256(mutatedManifest)
			if err != nil {
				t.Fatal(err)
			}
			if oneSHA == mutatedSHA {
				t.Fatal("Spec mutation retained the manifest SHA-256")
			}
		})
	}
}

func TestVerifyManifestRejectsCanonicalMemberMutations(t *testing.T) {
	code, canonical, stderr := invokeCLI(
		artifactManifestArgs(testSHA('a'), testSHA('b'), testSHA('c'), testSHA('d')), "")
	if code != 0 || stderr != "" {
		t.Fatalf("artifact-manifest code=%d stderr=%q", code, stderr)
	}
	manifest, err := finalv5oracle.DecodeManifest([]byte(canonical))
	if err != nil {
		t.Fatal(err)
	}
	wantSHA, err := finalv5oracle.ManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	verifyCode, verifyOutput, verifyErrors := invokeCLI([]string{"verify-manifest", "--input", "-"}, canonical)
	if verifyCode != 0 || verifyOutput != wantSHA+"\n" || verifyErrors != "" {
		t.Fatalf("verify code=%d stdout=%q stderr=%q", verifyCode, verifyOutput, verifyErrors)
	}

	mutations := []struct {
		name   string
		mutate func(*finalv5oracle.OracleManifest)
	}{
		{name: "seed", mutate: func(value *finalv5oracle.OracleManifest) { value.Generation.Seed++ }},
		{name: "spec hash", mutate: func(value *finalv5oracle.OracleManifest) { value.DatasetSpecSHA256 = testSHA('e') }},
		{name: "logical member", mutate: func(value *finalv5oracle.OracleManifest) {
			value.Expected.CanonicalResultSHA256 = testSHA('f')
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := manifest
			test.mutate(&changed)
			changedBytes, err := finalv5oracle.CanonicalManifest(changed)
			if err != nil {
				t.Fatalf("mutation did not remain structurally canonical: %v", err)
			}
			code, output, errorsOutput := invokeCLI([]string{"verify-manifest", "--input", "-"}, string(changedBytes))
			if code == 0 || output != "" || errorsOutput == "" {
				t.Fatalf("mutated verify code=%d stdout=%q stderr=%q", code, output, errorsOutput)
			}
		})
	}

	code, output, errorsOutput := invokeCLI([]string{"verify-manifest", "--input", "-"}, " "+canonical)
	if code == 0 || output != "" || errorsOutput == "" {
		t.Fatalf("non-canonical verify code=%d stdout=%q stderr=%q", code, output, errorsOutput)
	}
}

func TestVerifyManifestDispatchesScaleAndFailsClosedForUnsupportedWorkloads(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "final-v5-wsl2", "oracle-manifests", "scale",
		"dependency-e2e", "10k-overlap-50", "novel.json")
	canonical, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := finalv5oracle.DecodeManifest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	wantSHA, err := finalv5oracle.ManifestSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	code, output, errorsOutput := invokeCLI([]string{"verify-manifest", "--input", "-"}, string(canonical))
	if code != 0 || output != wantSHA+"\n" || errorsOutput != "" {
		t.Fatalf("Scale verify code=%d stdout=%q stderr=%q", code, output, errorsOutput)
	}

	mutated := manifest
	mutated.Expected.ExistingSetSHA256 = testSHA('a')
	changed, err := finalv5oracle.CanonicalManifest(mutated)
	if err != nil {
		t.Fatal(err)
	}
	code, output, errorsOutput = invokeCLI([]string{"verify-manifest", "--input", "-"}, string(changed))
	if code == 0 || output != "" || errorsOutput == "" {
		t.Fatalf("mutated Scale verify code=%d stdout=%q stderr=%q", code, output, errorsOutput)
	}

	unsupported := manifest
	unsupported.ExperimentID, unsupported.WorkloadID, unsupported.Scale = "baseline", "S1", "SF1"
	changed, err = finalv5oracle.CanonicalManifest(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	code, output, errorsOutput = invokeCLI([]string{"verify-manifest", "--input", "-"}, string(changed))
	if code == 0 || output != "" || errorsOutput == "" {
		t.Fatalf("unsupported verifier code=%d stdout=%q stderr=%q", code, output, errorsOutput)
	}
}

func TestDependencyReportAndOutcomeScheduleAreDeterministicJSON(t *testing.T) {
	dependencyArgs := []string{
		"dependency-report", "--candidate-facts", "20", "--existing-facts", "20", "--overlap-facts", "10",
	}
	codeOne, outputOne, errorsOne := invokeCLI(dependencyArgs, "")
	codeTwo, outputTwo, errorsTwo := invokeCLI(dependencyArgs, "")
	if codeOne != 0 || codeTwo != 0 || errorsOne != "" || errorsTwo != "" || outputOne != outputTwo {
		t.Fatalf("dependency codes=%d/%d equal=%v stderr=%q/%q", codeOne, codeTwo, outputOne == outputTwo, errorsOne, errorsTwo)
	}
	var report finalv5oracle.DependencyOracleReport
	if err := json.Unmarshal([]byte(outputOne), &report); err != nil {
		t.Fatal(err)
	}
	if report.Candidate.Cardinality != 20 || report.Existing.Cardinality != 20 || report.Overlap.Cardinality != 10 ||
		report.Novel.Cardinality != 10 || report.Union.Cardinality != 30 ||
		report.Candidate.Stats.PeakBufferedMembers > dependencyMemoryMembers || len(report.Candidate.SampleMembers) > dependencyCapturedMembers {
		t.Fatalf("dependency report = %+v", report)
	}

	scheduleArgs := []string{
		"outcome-schedule", "--seed", "7", "--candidate-cardinality", "1", "--target-percent", "50",
	}
	scheduleCode, scheduleOutput, scheduleErrors := invokeCLI(scheduleArgs, "")
	if scheduleCode != 0 || scheduleErrors != "" {
		t.Fatalf("outcome-schedule code=%d stderr=%q", scheduleCode, scheduleErrors)
	}
	var schedule finalv5oracle.OutcomeOverlapSchedule
	if err := json.Unmarshal([]byte(scheduleOutput), &schedule); err != nil {
		t.Fatal(err)
	}
	if err := finalv5oracle.ValidateOutcomeOverlapSchedule(schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.TotalCandidateMemberships != 30 || schedule.TotalOverlapMemberships != 15 {
		t.Fatalf("outcome schedule = %+v", schedule)
	}
	mutatedArgs := append([]string(nil), scheduleArgs...)
	mutatedArgs[2] = "8"
	mutatedCode, mutatedOutput, mutatedErrors := invokeCLI(mutatedArgs, "")
	if mutatedCode != 0 || mutatedErrors != "" || mutatedOutput == scheduleOutput {
		t.Fatalf("seed mutation code=%d changed=%v stderr=%q", mutatedCode, mutatedOutput != scheduleOutput, mutatedErrors)
	}
}

func TestDatasetFingerprintIsDeterministicStreamingJSON(t *testing.T) {
	codeOne, outputOne, errorsOne := invokeCLI([]string{"dataset-fingerprint"}, "")
	codeTwo, outputTwo, errorsTwo := invokeCLI([]string{"dataset-fingerprint"}, "")
	if codeOne != 0 || codeTwo != 0 || errorsOne != "" || errorsTwo != "" || outputOne != outputTwo {
		t.Fatalf("dataset-fingerprint codes=%d/%d equal=%v stderr=%q/%q", codeOne, codeTwo, outputOne == outputTwo, errorsOne, errorsTwo)
	}
	var summary finalv5oracle.DatasetFingerprintSummary
	if err := json.Unmarshal([]byte(outputOne), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.ProductCount != 5 || summary.RowCount != 815_000 || summary.PeakBufferedRows != 1 || len(summary.Products) != 5 {
		t.Fatalf("dataset fingerprint = %+v", summary)
	}
	code, output, errorsOutput := invokeCLI([]string{"dataset-fingerprint", "unexpected"}, "")
	if code == 0 || output != "" || errorsOutput == "" {
		t.Fatalf("unexpected argument code=%d stdout=%q stderr=%q", code, output, errorsOutput)
	}
}

func TestCLIRejectsIncompleteAmbiguousOrSecretBearingArguments(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		input string
	}{
		{name: "no subcommand"},
		{name: "unknown subcommand", args: []string{"generate-campaign"}},
		{name: "missing explicit Spec", args: []string{"artifact-manifest", "--row-count", "100", "--column-count", "4"}},
		{name: "uppercase Spec", args: artifactManifestArgs(strings.ToUpper(testSHA('a')), testSHA('b'), testSHA('c'), testSHA('d'))},
		{name: "duplicate flag", args: []string{"outcome-schedule", "--seed", "7", "--seed", "8", "--candidate-cardinality", "1", "--target-percent", "50"}},
		{name: "non-canonical integer", args: []string{"dependency-report", "--candidate-facts", "020", "--existing-facts", "20", "--overlap-facts", "10"}},
		{name: "unknown DSN flag", args: []string{"outcome-schedule", "--dsn", "redacted-value", "--seed", "7", "--candidate-cardinality", "1", "--target-percent", "50"}},
		{name: "missing manifest", args: []string{"verify-manifest", "--input", "-"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, output, errorsOutput := invokeCLI(test.args, test.input)
			if code == 0 || output != "" || errorsOutput == "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, output, errorsOutput)
			}
		})
	}
}

func artifactManifestArgs(dataset, catalog, query, normalization string) []string {
	return []string{
		"artifact-manifest", "--row-count", "100", "--column-count", "4",
		"--dataset-spec-sha256", dataset, "--catalog-spec-sha256", catalog,
		"--query-spec-sha256", query, "--normalization-spec-sha256", normalization,
	}
}

func testSHA(character byte) string {
	return strings.Repeat(string(character), 64)
}

func invokeCLI(args []string, input string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
