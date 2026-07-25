package exposureeval

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

func TestExposureEvaluationCorpus(t *testing.T) {
	report, err := Run()
	if err != nil {
		t.Fatal(err)
	}
	if report.RQ1.Passed != report.RQ1.Cases || report.RQ2.GeneratedPairs != 1024 ||
		report.RQ2.UniqueRewrites != 8 || report.RQ2.RewriteTemplates != 2 || report.RQ2.Mismatches != 0 ||
		report.RQ3.DeterministicPassed != report.RQ3.DeterministicCases ||
		report.RQ3.DeterministicCases+len(report.RQ3.PostgresIntegrationIDs) != report.RQ3.Cases ||
		report.RQ5.Passed != report.RQ5.Scenarios {
		t.Fatalf("incomplete exposure report: %+v", report)
	}
	if report.RQ4Status != "not_measured_requires_external_postgresql_campaign" {
		t.Fatalf("RQ4 status overclaims measurement: %q", report.RQ4Status)
	}
	if len(report.CorpusSHA256) != 64 || report.RewriteSeed != 20260723 {
		t.Fatalf("report provenance = sha %q seed %d", report.CorpusSHA256, report.RewriteSeed)
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
