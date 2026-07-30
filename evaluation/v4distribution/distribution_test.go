package v4distribution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testConfig() Config {
	return Config{Cardinality: 1_000, Runs: 2, ClusterCount: 10, RandomSeed: 17,
		ReplayLookupsPerRun: 128, MaxPeakHeapBytes: 512 << 20}
}

func TestRunBuildsExactDeterministicDistributionOverlapMatrix(t *testing.T) {
	report, err := Run(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Cells) != 12 {
		t.Fatalf("cells=%d, want 12", len(report.Cells))
	}
	if report.AcceptanceEligible || report.EligibilityReason == "" {
		t.Fatal("small diagnostic matrix was mislabeled as acceptance evidence")
	}
	physical := map[string]bool{}
	for index, cell := range report.Cells {
		distribution := distributionNames[index/4]
		target := overlapTargets[index%4]
		if cell.Distribution != distribution || cell.TargetOverlapPercent != target ||
			cell.ObservedOverlapPercent != float64(target) {
			t.Fatalf("cell %d identity/overlap = %#v", index, cell)
		}
		if cell.Effect.Cardinality != testConfig().Cardinality ||
			cell.LedgerBefore.Cardinality != testConfig().Cardinality*uint64(target)/100 ||
			cell.NovelDelta.Cardinality != testConfig().Cardinality*(100-uint64(target))/100 ||
			cell.LedgerAfter.Cardinality != testConfig().Cardinality || cell.ReplayDelta.Cardinality != 0 {
			t.Fatalf("cell %d cardinalities are not exact: %#v", index, cell)
		}
		if cell.ObservationSHA256 != cell.ReplayObservationSHA256 || !cell.ReplayMatched {
			t.Fatalf("cell %d replay did not reuse the committed observation digest", index)
		}
		for label, metric := range map[string]BitmapMetrics{"effect": cell.Effect, "prior": cell.LedgerBefore,
			"novel": cell.NovelDelta, "after": cell.LedgerAfter, "replay": cell.ReplayDelta} {
			if !metric.PortableRoundTripVerified || metric.RoundTripDigest != metric.Digest {
				t.Fatalf("cell %d %s omitted exact portable round trip: %#v", index, label, metric)
			}
		}
		physical[physicalSignature(cell.Effect)] = true
	}
	if len(physical) != 3 {
		t.Fatalf("physical distribution signatures=%d, want 3", len(physical))
	}
	if err := ValidateReport(report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
}

func TestAcceptanceEligibilityRequiresPinnedMillionFactContract(t *testing.T) {
	if DefaultConfig().Runs != 50 || minimumEvidenceRuns != 50 {
		t.Fatalf("default/minimum evidence runs = %d/%d, want 50/50", DefaultConfig().Runs, minimumEvidenceRuns)
	}
	eligible, reason := evidenceEligibility(DefaultConfig())
	if !eligible || reason != "" {
		t.Fatalf("default evidence contract eligible=%t reason=%q", eligible, reason)
	}
	config := DefaultConfig()
	config.Runs = minimumEvidenceRuns - 1
	if eligible, reason := evidenceEligibility(config); eligible || reason == "" {
		t.Fatal("under-sampled matrix was acceptance eligible")
	}
}

func TestMatrixDigestIgnoresNondeterministicMeasurements(t *testing.T) {
	first, err := Run(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if first.MatrixSHA256 != second.MatrixSHA256 {
		t.Fatalf("matrix digest changed: %s != %s", first.MatrixSHA256, second.MatrixSHA256)
	}
	for index := range first.Cells {
		if first.Cells[index].DeterministicCellSHA256 != second.Cells[index].DeterministicCellSHA256 ||
			!reflect.DeepEqual(first.Cells[index].Effect, second.Cells[index].Effect) ||
			!reflect.DeepEqual(first.Cells[index].LedgerBefore, second.Cells[index].LedgerBefore) ||
			!reflect.DeepEqual(first.Cells[index].NovelDelta, second.Cells[index].NovelDelta) {
			t.Fatalf("deterministic cell %d changed", index)
		}
	}
}

func TestValidateReportRejectsStructuralAndRawSampleTampering(t *testing.T) {
	report, err := Run(testConfig())
	if err != nil {
		t.Fatal(err)
	}

	tampered := report
	tampered.Cells = append([]Cell(nil), report.Cells...)
	tampered.Cells[0].Effect.PortableBytes++
	if err := ValidateReport(tampered); err == nil {
		t.Fatal("tampered portable byte count was accepted")
	}

	tampered = report
	tampered.Cells = append([]Cell(nil), report.Cells...)
	tampered.Cells[0].Effect.PortableRoundTripVerified = false
	if err := ValidateReport(tampered); err == nil {
		t.Fatal("missing full-scale portable round-trip verification was accepted")
	}

	tampered = report
	tampered.Cells = append([]Cell(nil), report.Cells...)
	tampered.Cells[0].NovelBitmapLatency.SamplesMS = append([]float64(nil), report.Cells[0].NovelBitmapLatency.SamplesMS...)
	tampered.Cells[0].NovelBitmapLatency.SamplesMS[0] *= 2
	if err := ValidateReport(tampered); err == nil {
		t.Fatal("tampered raw latency without a recomputed summary was accepted")
	}

	tampered = report
	tampered.Cells = append([]Cell(nil), report.Cells...)
	tampered.Cells[5], tampered.Cells[6] = tampered.Cells[6], tampered.Cells[5]
	if err := ValidateReport(tampered); err == nil {
		t.Fatal("noncanonical matrix order was accepted")
	}
}

func TestReadAndValidateRoundTripAndRejectsUnknownFields(t *testing.T) {
	report, err := Run(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidate(path); err != nil {
		t.Fatal(err)
	}

	unknown := append([]byte(nil), raw[:len(raw)-1]...)
	unknown = append(unknown, []byte(`,"not_in_schema":true}`)...)
	unknownPath := filepath.Join(t.TempDir(), "unknown.json")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidate(unknownPath); err == nil {
		t.Fatal("unknown report field was accepted")
	}

	duplicate := append([]byte(`{"schema_version":1,`), raw[1:]...)
	duplicatePath := filepath.Join(t.TempDir(), "duplicate.json")
	if err := os.WriteFile(duplicatePath, duplicate, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAndValidate(duplicatePath); err == nil {
		t.Fatal("duplicate report field was accepted")
	}
}

func TestConfigValidationRejectsInexactOrUnsafeMatrix(t *testing.T) {
	tests := []Config{
		{},
		{Cardinality: 999, Runs: 1, ClusterCount: 10, ReplayLookupsPerRun: 1, MaxPeakHeapBytes: 1},
		{Cardinality: 1_000, Runs: 0, ClusterCount: 10, ReplayLookupsPerRun: 1, MaxPeakHeapBytes: 1},
		{Cardinality: 1_000, Runs: 1, ClusterCount: 1, ReplayLookupsPerRun: 1, MaxPeakHeapBytes: 1},
		{Cardinality: 1_000, Runs: 1, ClusterCount: 10, ReplayLookupsPerRun: 0, MaxPeakHeapBytes: 1},
	}
	for index, config := range tests {
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid config %d was accepted", index)
		}
	}
}

func physicalSignature(metric BitmapMetrics) string {
	return fmt.Sprintf("%d/%d/%d/%d", metric.ContainerCount, metric.PortableBytes,
		metric.MinimumOrdinal, metric.MaximumOrdinal)
}
