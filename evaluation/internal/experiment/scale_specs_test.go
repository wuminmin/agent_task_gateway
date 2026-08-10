package experiment

import "testing"

func TestDependencyScaleDecision18Algebra(t *testing.T) {
	tests := []struct {
		scale            string
		candidateFacts   int64
		overlapFacts     int64
		chargedFacts     int64
		unionFacts       int64
		candidateRows    int64
		historyLower     int64
		historyUpper     int64
		intersectionRows int64
	}{
		{scale: "10k-overlap-0", candidateFacts: 10_000, overlapFacts: 0, chargedFacts: 10_000, unionFacts: 20_000, candidateRows: 2_000, historyLower: 2_000, historyUpper: 4_000, intersectionRows: 0},
		{scale: "10k-overlap-50", candidateFacts: 10_000, overlapFacts: 5_000, chargedFacts: 5_000, unionFacts: 15_000, candidateRows: 2_000, historyLower: 1_000, historyUpper: 3_000, intersectionRows: 1_000},
		{scale: "10k-overlap-90", candidateFacts: 10_000, overlapFacts: 9_000, chargedFacts: 1_000, unionFacts: 11_000, candidateRows: 2_000, historyLower: 200, historyUpper: 2_200, intersectionRows: 1_800},
		{scale: "10k-overlap-100", candidateFacts: 10_000, overlapFacts: 10_000, chargedFacts: 0, unionFacts: 10_000, candidateRows: 2_000, historyLower: 0, historyUpper: 2_000, intersectionRows: 2_000},
		{scale: "100k-overlap-0", candidateFacts: 100_000, overlapFacts: 0, chargedFacts: 100_000, unionFacts: 200_000, candidateRows: 20_000, historyLower: 20_000, historyUpper: 40_000, intersectionRows: 0},
		{scale: "100k-overlap-50", candidateFacts: 100_000, overlapFacts: 50_000, chargedFacts: 50_000, unionFacts: 150_000, candidateRows: 20_000, historyLower: 10_000, historyUpper: 30_000, intersectionRows: 10_000},
		{scale: "100k-overlap-90", candidateFacts: 100_000, overlapFacts: 90_000, chargedFacts: 10_000, unionFacts: 110_000, candidateRows: 20_000, historyLower: 2_000, historyUpper: 22_000, intersectionRows: 18_000},
		{scale: "100k-overlap-100", candidateFacts: 100_000, overlapFacts: 100_000, chargedFacts: 0, unionFacts: 100_000, candidateRows: 20_000, historyLower: 0, historyUpper: 20_000, intersectionRows: 20_000},
		{scale: "1035000-overlap-0", candidateFacts: 1_035_000, overlapFacts: 0, chargedFacts: 1_035_000, unionFacts: 2_070_000, candidateRows: 207_000, historyLower: 207_000, historyUpper: 414_000, intersectionRows: 0},
		{scale: "1035000-overlap-50", candidateFacts: 1_035_000, overlapFacts: 517_500, chargedFacts: 517_500, unionFacts: 1_552_500, candidateRows: 207_000, historyLower: 103_500, historyUpper: 310_500, intersectionRows: 103_500},
		{scale: "1035000-overlap-90", candidateFacts: 1_035_000, overlapFacts: 931_500, chargedFacts: 103_500, unionFacts: 1_138_500, candidateRows: 207_000, historyLower: 20_700, historyUpper: 227_700, intersectionRows: 186_300},
		{scale: "1035000-overlap-100", candidateFacts: 1_035_000, overlapFacts: 1_035_000, chargedFacts: 0, unionFacts: 1_035_000, candidateRows: 207_000, historyLower: 0, historyUpper: 207_000, intersectionRows: 207_000},
	}
	if len(tests) != 12 {
		t.Fatalf("dependency algebra covers %d cells, want 12", len(tests))
	}

	for _, test := range tests {
		t.Run(test.scale, func(t *testing.T) {
			spec, err := ParseDependencyScale(test.scale)
			if err != nil {
				t.Fatal(err)
			}
			state := spec.NovelState()
			if spec.CandidateFacts != test.candidateFacts || spec.ExistingFacts != test.candidateFacts ||
				spec.OverlapFacts != test.overlapFacts || spec.UnionFacts != test.unionFacts {
				t.Fatalf("spec = %+v", spec)
			}
			if state.ActualDependencyFacts != test.candidateFacts || state.ChargedDependencyFacts != test.chargedFacts {
				t.Fatalf("actual/charged = %d/%d, want %d/%d", state.ActualDependencyFacts,
					state.ChargedDependencyFacts, test.candidateFacts, test.chargedFacts)
			}
			if state.ActualDependencyFacts-state.ChargedDependencyFacts != test.overlapFacts {
				t.Fatalf("actual-charged = %d, want overlap %d",
					state.ActualDependencyFacts-state.ChargedDependencyFacts, test.overlapFacts)
			}

			candidateInterval := DependencyScaleMemberRankInterval{LowerExclusive: 0, UpperInclusive: test.candidateRows}
			historyInterval := DependencyScaleMemberRankInterval{LowerExclusive: test.historyLower, UpperInclusive: test.historyUpper}
			unionInterval := DependencyScaleMemberRankInterval{LowerExclusive: 0, UpperInclusive: test.historyUpper}
			if state.Candidate.MemberRanks != candidateInterval || state.Candidate.QueryIdentity != "count(*)" {
				t.Fatalf("candidate state = %+v", state.Candidate)
			}
			if state.History.MemberRanks != historyInterval || state.History.QueryIdentity != "sum(metric)" ||
				state.History.QueryIdentity == state.Candidate.QueryIdentity {
				t.Fatalf("history state = %+v", state.History)
			}
			if state.History.DependencyCardinality != spec.ExistingFacts || state.History.MemberRanks.Rows() != test.candidateRows {
				t.Fatalf("history cardinality/rows = %d/%d, want %d/%d", state.History.DependencyCardinality,
					state.History.MemberRanks.Rows(), spec.ExistingFacts, test.candidateRows)
			}
			if got := state.Candidate.MemberRanks.IntersectionRows(state.History.MemberRanks); got != test.intersectionRows ||
				got*dependencyScaleFactsPerRetainedRow != test.overlapFacts {
				t.Fatalf("candidate/history intersection rows = %d, want %d (%d facts)",
					got, test.intersectionRows, test.overlapFacts)
			}
			if test.overlapFacts == 0 && (state.History.DependencyCardinality == 0 ||
				state.Candidate.MemberRanks.IntersectionRows(state.History.MemberRanks) != 0) {
				t.Fatal("zero-overlap cell lacks a full disjoint history")
			}

			if state.Candidate.SummaryRole != "candidate" || state.History.SummaryRole != "existing" ||
				state.Union.SummaryRole != "union" {
				t.Fatalf("candidate/history/union summary roles = %q/%q/%q", state.Candidate.SummaryRole,
					state.History.SummaryRole, state.Union.SummaryRole)
			}
			if state.Union.MemberRanks != unionInterval || state.Union.DependencyCardinality != test.unionFacts {
				t.Fatalf("union state = %+v", state.Union)
			}
			if state.RootBefore != state.History.DependencyScaleSetState ||
				state.RootBefore.DependencyCardinality != spec.ExistingFacts || state.RootBefore.SummaryRole != "existing" {
				t.Fatalf("RootBefore = %+v, history = %+v", state.RootBefore, state.History.DependencyScaleSetState)
			}
			if state.RootAfter != state.Union || state.RootAfter.DependencyCardinality != spec.UnionFacts {
				t.Fatalf("RootAfter = %+v, union = %+v", state.RootAfter, state.Union)
			}
			if state.RootAfter.SummaryRole != "union" ||
				state.RootAfter.SummaryRole == state.Candidate.SummaryRole {
				t.Fatalf("RootAfter summary role %q is not independent from candidate role %q",
					state.RootAfter.SummaryRole, state.Candidate.SummaryRole)
			}
		})
	}
}
