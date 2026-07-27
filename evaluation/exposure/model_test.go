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
		report.RQ5.Passed != report.RQ5.Scenarios || report.RQ5.Scenarios != 501 {
		t.Fatalf("incomplete exposure report: %+v", report)
	}
	if report.SchemaVersion != 3 {
		t.Fatalf("exposure report schema = %d, want 3", report.SchemaVersion)
	}
	if len(report.RQ5.Results) == 0 || report.RQ5.Results[0].ID != "shared-fact-budget-one" ||
		report.RQ5.Results[0].ExactUtility != 2 || report.RQ5.Results[0].AdditiveProxyUtility != 1 {
		t.Fatalf("shared-fact planner regression is missing: %+v", report.RQ5)
	}
	if report.RQ4Status != "measured_controlled_local_postgresql_campaign" {
		t.Fatalf("RQ4 status is stale: %q", report.RQ4Status)
	}
	if report.RQ5Agent.Status != "complete" || report.RQ5Agent.Tasks != 24 ||
		len(report.RQ5Agent.Policies) != 6 || report.RQ5Agent.Kinds != 5 {
		t.Fatalf("RQ5 agent-task report is incomplete: %+v", report.RQ5Agent)
	}
	if len(report.CorpusSHA256) != 64 {
		t.Fatalf("report provenance = sha %q", report.CorpusSHA256)
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
