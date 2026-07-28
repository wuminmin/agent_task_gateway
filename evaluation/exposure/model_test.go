package exposureeval

import (
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

func TestExposureEvaluationCorpus(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.RQ1.Passed != report.RQ1.Cases || report.RQ1.Cases != 21 || report.RQ1.DatasetRows != 16 ||
		report.RQ1.ReleaseFacts == 0 || report.RQ1.InfluenceFacts == 0 || len(report.RQ1.OracleSourceSHA256) != 64 ||
		report.RQ2.GeneratedAttempts != 16 || report.RQ2.UniqueNormalizedPairs != 16 ||
		report.RQ2.ExecutedUniquePairs != 16 || report.RQ2.DuplicateAttempts != 0 ||
		report.RQ2.RewriteTemplates != 2 || report.RQ2.Mismatches != 0 ||
		report.RQ3.DeterministicPassed != report.RQ3.DeterministicCases ||
		report.RQ3.DeterministicCases+len(report.RQ3.IntegrationManifest) != report.RQ3.Cases ||
		!report.RQ3.OutcomeProbing.Passed || report.RQ3.OutcomeProbing.DistinctOutcomeFacts != 3 ||
		report.RQ3.OutcomeProbing.NovelOutcomeCharges != 3 || report.RQ3.OutcomeProbing.ReplayOutcomeCharge != 0 ||
		report.RQ3.OutcomeProbing.EquivalentRewriteCharge != 0 {
		t.Fatalf("incomplete exposure report: %+v", report)
	}
	if report.SchemaVersion != 6 || report.ProfileVersion != exposure.ProfileV3 {
		t.Fatalf("exposure report schema/profile = %d/%s, want 6/%s", report.SchemaVersion, report.ProfileVersion, exposure.ProfileV3)
	}
	if report.RQ4Status != "measured_controlled_local_postgresql_campaign" {
		t.Fatalf("RQ4 status is stale: %q", report.RQ4Status)
	}
	if len(report.CorpusSHA256) != 64 {
		t.Fatalf("report provenance = sha %q", report.CorpusSHA256)
	}
}

func TestPolicyCalibrationUsesExactFactSetUnions(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.RQ5Policy.Status != "complete_deterministic_policy_calibration" ||
		report.RQ5Policy.FixtureRows != 16 || len(report.RQ5Policy.Scenarios) != 3 {
		t.Fatalf("incomplete policy calibration: %+v", report.RQ5Policy)
	}
	for _, scenario := range report.RQ5Policy.Scenarios {
		if scenario.Goals != 3 || scenario.FullReleaseFacts == 0 || scenario.FullDependencyFacts == 0 ||
			len(scenario.Curve) != 4 || scenario.Curve[len(scenario.Curve)-1].UtilityPercent != 100 {
			t.Fatalf("incomplete policy scenario %s: %+v", scenario.ID, scenario)
		}
		if got := scenario.DependencyBreakdown.Rows + scenario.DependencyBreakdown.OrdinaryFields +
			scenario.DependencyBreakdown.SensitiveFields + scenario.DependencyBreakdown.Derived; got != scenario.FullDependencyFacts {
			t.Fatalf("dependency classification for %s sums to %d, want %d", scenario.ID, got, scenario.FullDependencyFacts)
		}
		if got := scenario.ReleaseBreakdown.Rows + scenario.ReleaseBreakdown.OrdinaryFields +
			scenario.ReleaseBreakdown.SensitiveFields + scenario.ReleaseBreakdown.Derived; got != scenario.FullReleaseFacts {
			t.Fatalf("release classification for %s sums to %d, want %d", scenario.ID, got, scenario.FullReleaseFacts)
		}
	}
}

func TestRQ1OracleHasNoProductionExposureDependency(t *testing.T) {
	source, err := os.ReadFile("../exposureoracle/oracle.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"internal/exposure", "internal/queryplan", "internal/gateway", "internal/sqlpolicy",
		"evaluation/exposure\"", "evaluation/postgresoracle",
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("independent RQ1 oracle imports forbidden package %q", forbidden)
		}
	}
}

func TestHistoryAwareBaselineChargesReplayZero(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.Baselines.QueryCount != 1 || report.Baselines.ProvenanceNoHistory == 0 ||
		report.Baselines.FullFirst.Release == 0 || report.Baselines.FullFirst.Influence == 0 ||
		report.Baselines.FullReplay != (ChargeVector{}) {
		t.Fatalf("unexpected baseline contrast: %+v", report.Baselines)
	}
}

func BenchmarkExposureDerivation(b *testing.B) {
	fixtures, err := loadCorpus()
	if err != nil {
		b.Fatal(err)
	}
	relations, err := buildRelations(fixtures.Relations)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, _, err := evaluateOperation(fixtures.ProfileVersion, relations, "group_sum_count"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWeightedCellsBaseline(b *testing.B) {
	fixtures, err := loadCorpus()
	if err != nil {
		b.Fatal(err)
	}
	relations, err := buildRelations(fixtures.Relations)
	if err != nil {
		b.Fatal(err)
	}
	relation := relations["expenses"]
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		cells := 0
		for range relation.Rows {
			cells += len(relation.Fields)
		}
		if cells == 0 {
			b.Fatal(exposure.ErrInvalid)
		}
	}
}
