package finalv5oracle

import (
	"fmt"
	"testing"
)

func TestEvaluateExactUnionAndPreregisteredBudget(t *testing.T) {
	fact := func(number int) string { return fmt.Sprintf("%064x", number) }
	trace := []Observation{
		{Release: []string{fact(1), fact(2)}, Dependency: []string{fact(10)}, Outcome: []string{fact(20)}},
		{Release: []string{fact(2), fact(3)}, Dependency: []string{fact(10), fact(11)}, Outcome: []string{fact(21)}},
	}
	result, err := Evaluate(trace)
	if err != nil {
		t.Fatal(err)
	}
	if result.Release.Cardinality != 3 || result.Release.Budget != 2 || result.Dependency.Cardinality != 2 || result.Dependency.Budget != 1 || result.Outcome.Cardinality != 2 || result.Outcome.Budget != 1 {
		t.Fatalf("trace union = %+v", result)
	}
	permuted, err := Evaluate([]Observation{trace[1], trace[0]})
	if err != nil || permuted.Release.SetSHA256 != result.Release.SetSHA256 || permuted.Dependency.SetSHA256 != result.Dependency.SetSHA256 || permuted.Outcome.SetSHA256 != result.Outcome.SetSHA256 {
		t.Fatalf("union digest depends on trace order: %+v err=%v", permuted, err)
	}
}

func TestEvaluateRejectsMalformedFactID(t *testing.T) {
	if _, err := Evaluate([]Observation{{Release: []string{"not-a-digest"}}}); err == nil {
		t.Fatal("malformed FactID accepted")
	}
}

func TestEvaluatePrefixesRetainsEveryExactUnion(t *testing.T) {
	fact := func(number int) string { return fmt.Sprintf("%064x", number) }
	trace := []Observation{
		{Release: []string{fact(1)}, Dependency: []string{fact(10)}, Outcome: []string{fact(20)}},
		{Release: []string{fact(1), fact(2)}, Dependency: []string{fact(11)}, Outcome: []string{fact(20), fact(21)}},
	}
	prefixes, err := EvaluatePrefixes(trace)
	if err != nil || len(prefixes) != 2 || prefixes[0].Queries != 1 || prefixes[0].Release.Cardinality != 1 ||
		prefixes[1].Queries != 2 || prefixes[1].Release.Cardinality != 2 || prefixes[1].Dependency.Cardinality != 2 || prefixes[1].Outcome.Cardinality != 2 {
		t.Fatalf("prefixes = %+v, err=%v", prefixes, err)
	}
}
