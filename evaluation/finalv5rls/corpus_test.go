package finalv5rls

import (
	"encoding/json"
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestFrozenCorpusExpandsExactlyOneHundredConcreteQueries(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	steps, err := manifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 100 || steps[0].Index != 1 || steps[99].Index != 100 {
		t.Fatalf("trace length/bounds = %d/%d/%d", len(steps), steps[0].Index, steps[99].Index)
	}
	families := map[string]int{}
	for index, step := range steps {
		families[step.Family]++
		if step.ExpectedSHA256 != ResultSHA256(step.ExpectedRows) || step.DirectSQL == "" || step.LogicalSQL(UnlimitedProduct) == step.DirectSQL || step.Index != index+1 {
			t.Fatalf("invalid frozen step %d: %#v", index+1, step)
		}
	}
	want := map[string]int{"equality": 6, "pagination": 30, "equivalent_predicate": 30, "repeated_aggregation": 30, "adaptive_choice": 4}
	if !reflect.DeepEqual(families, want) {
		t.Fatalf("families = %#v, want %#v", families, want)
	}
	for index, wantScalar := range []int64{4, 5, 4, 5} {
		step := steps[96+index]
		if step.Scalar == nil || *step.Scalar != wantScalar || step.Decision.PreviousStep != step.Index-1 || step.Decision.Rule != "odd->880;even->553" {
			t.Fatalf("adaptive step %d = %#v", step.Index, step)
		}
	}
}

func TestIndependentOracleHasFrozenFullUnionAndSeventyPercentBudget(t *testing.T) {
	manifest, _ := Load()
	steps, _ := manifest.Trace()
	result, err := finalv5oracle.Evaluate(OracleTrace(steps))
	if err != nil {
		t.Fatal(err)
	}
	if result.Release.Cardinality != 10 || result.Release.Budget != BoundedMaxReleaseFacts ||
		result.Dependency.Cardinality != 18 || result.Dependency.Budget != BoundedMaxDependencyFacts ||
		result.Outcome.Cardinality != 27 || result.Outcome.Budget != BoundedMaxOutcomeFacts {
		t.Fatalf("oracle union = %+v", result)
	}
	stop, err := ComputeBoundedStop(steps)
	if err != nil {
		t.Fatal(err)
	}
	if stop.Index != 37 || stop.SuccessfulQueries != 36 || stop.Dimension != "dependency" ||
		stop.ErrorReason != "ROOT_DEPENDENCY_CEILING_EXCEEDED" || stop.Full != result ||
		stop.Before.Release.Cardinality != 6 || stop.Before.Dependency.Cardinality != 12 || stop.Before.Outcome.Cardinality != 15 ||
		stop.Candidate.Release.Cardinality != 6 || stop.Candidate.Dependency.Cardinality != 18 || stop.Candidate.Outcome.Cardinality != 17 {
		t.Fatalf("bounded stop = %+v", stop)
	}
	if stop.Before.Release.Cardinality > result.Release.Budget || stop.Before.Dependency.Cardinality > result.Dependency.Budget ||
		stop.Before.Outcome.Cardinality > result.Outcome.Budget || stop.Candidate.Dependency.Cardinality <= result.Dependency.Budget {
		t.Fatalf("bounded stop does not prove the exact first strict crossing: %+v", stop)
	}
}

func TestPolicyAndFixtureCanonicalEvidenceIsFrozen(t *testing.T) {
	for name, value := range map[string]json.RawMessage{"policies": ExpectedPoliciesJSON(), "memberships": ExpectedMembershipJSON(), "grants": ExpectedGrantsJSON()} {
		if !json.Valid(value) {
			t.Fatalf("%s JSON is invalid", name)
		}
	}
	manifest, _ := Load()
	if DatasetSHA256(manifest) == "" {
		t.Fatal("dataset digest is empty")
	}
}

func TestPolicyInvisibleControlBindsARealOutOfScopeFixtureRow(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	step, err := manifest.PolicyInvisibleStep()
	if err != nil {
		t.Fatal(err)
	}
	target := manifest.row(PolicyInvisibleReceipt)
	if target.ReceiptNo != PolicyInvisibleReceipt || target.Department == manifest.PolicyDepartment {
		t.Fatalf("policy-control target = %+v under policy department %q", target, manifest.PolicyDepartment)
	}
	if step.Index != 1 || step.ID != "policy-invisible-receipt" || step.Variant != "force-rls-zero-row" ||
		len(step.ExpectedRows) != 0 || step.ExpectedSHA256 != ResultSHA256(nil) ||
		step.LogicalSQL(UnlimitedProduct) == step.DirectSQL {
		t.Fatalf("policy-control step = %+v", step)
	}
	union, err := finalv5oracle.Evaluate([]finalv5oracle.Observation{step.Oracle})
	if err != nil {
		t.Fatal(err)
	}
	if union.Release.Cardinality != 0 || union.Dependency.Cardinality != 0 || union.Outcome.Cardinality != 2 {
		t.Fatalf("policy-control union = %+v, want release=0 dependency=0 outcome=2", union)
	}
}

func TestCorpusBytesAreDefensiveCopy(t *testing.T) {
	first := Bytes()
	first[0] = 'x'
	if Bytes()[0] != '{' {
		t.Fatal("caller mutated embedded RLS corpus")
	}
}
