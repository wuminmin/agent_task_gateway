package main

import (
	"math"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

func TestPercentileUsesHyndmanFanTypeSeven(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentile(values, 0.95); math.Abs(got-4.8) > 1e-12 {
		t.Fatalf("p95 = %v, want 4.8", got)
	}
}

func TestPairedAblationDerivesV2Facts(t *testing.T) {
	observation, err := deriveV2(dataconnector.Result{
		Columns:  []dataconnector.Column{{Name: "receipt_no"}, {Name: "department"}, {Name: "amount"}},
		Rows:     [][]any{{"TR-2026-0001", "销售部", "1680.00"}},
		RowCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Release) != 3 || len(observation.Influence) != 4 {
		t.Fatalf("release/influence = %d/%d, want 3/4", len(observation.Release), len(observation.Influence))
	}
}

func TestHistoryRatesOnlyApplyToFullPath(t *testing.T) {
	samples := []sample{{ActualReleaseFacts: 3, ActualInfluenceFacts: 4, SemanticReplay: true}}
	ablation := summarizeCell("paired_plus_algebra", 1, 1, 1, nil, nil, nil, samples)
	if ablation.FactHistoryHitRate != nil || ablation.QueryHistoryHitRate != nil || ablation.SemanticReplayRate != nil {
		t.Fatalf("non-ledger ablation was labeled as a history hit: %+v", ablation)
	}
	full := summarizeCell("full_history_hit", 1, 1, 1, nil, nil, nil, samples)
	if full.FactHistoryHitRate == nil || *full.FactHistoryHitRate != 1 ||
		full.QueryHistoryHitRate == nil || *full.QueryHistoryHitRate != 1 ||
		full.SemanticReplayRate == nil || *full.SemanticReplayRate != 1 {
		t.Fatalf("full zero-charge observation did not register as a hit: %+v", full)
	}
}
