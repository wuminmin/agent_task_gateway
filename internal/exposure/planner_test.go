package exposure

import "testing"

func TestOptimizeChoosesHighestMeasuredUtilityWithinBothBudgets(t *testing.T) {
	candidates := []Candidate{
		{ID: "detail", Requirement: "expenses", Product: "expense_detail", Representation: RepresentationRaw, ReleaseCost: 8, InfluenceCost: 10, AnswerCompleteness: 1, QueryCoverage: 1},
		{ID: "summary", Requirement: "expenses", Product: "expense_summary", Representation: RepresentationAggregate, ReleaseCost: 2, InfluenceCost: 6, AnswerCompleteness: .8, QueryCoverage: 1},
		{ID: "exact-employees", Requirement: "employees", Product: "employees", Representation: RepresentationProjection, ReleaseCost: 4, InfluenceCost: 4, AnswerCompleteness: 1, QueryCoverage: 1},
		{ID: "general-employees", Requirement: "employees", Product: "employees_generalized", Representation: RepresentationGeneralized, ReleaseCost: 1, InfluenceCost: 2, AnswerCompleteness: .6, QueryCoverage: .8},
	}
	plan, err := Optimize(candidates, 6, 10, UtilityWeights{AnswerCompleteness: .7, QueryCoverage: .3})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 || plan.Candidates[0].ID != "exact-employees" || plan.Candidates[1].ID != "summary" {
		t.Fatalf("selected candidates = %+v", plan.Candidates)
	}
	if plan.ReleaseCost != 6 || plan.InfluenceCost != 10 {
		t.Fatalf("cost = (%d,%d), want (6,10)", plan.ReleaseCost, plan.InfluenceCost)
	}
}

func TestOptimizeTieBreakIsDeterministic(t *testing.T) {
	candidates := []Candidate{
		{ID: "b", Requirement: "r", Product: "p", Representation: RepresentationProjection, ReleaseCost: 1, InfluenceCost: 1, AnswerCompleteness: 1, QueryCoverage: 1},
		{ID: "a", Requirement: "r", Product: "p", Representation: RepresentationProjection, ReleaseCost: 1, InfluenceCost: 1, AnswerCompleteness: 1, QueryCoverage: 1},
	}
	plan, err := Optimize(candidates, 1, 1, UtilityWeights{AnswerCompleteness: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].ID != "a" {
		t.Fatalf("selected = %+v, want a", plan.Candidates)
	}
}
