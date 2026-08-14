package exposure

import "testing"

const testProfile = "exposure-v1"

func baseExpenses(t *testing.T) Relation {
	t.Helper()
	relation, err := NewBaseRelation("expense_detail", "snapshot-1", []string{"department", "amount"}, []BaseRow{
		{EntityKey: "r1", Values: map[string]any{"department": "sales", "amount": int64(10)}},
		{EntityKey: "r2", Values: map[string]any{"department": "sales", "amount": int64(20)}},
		{EntityKey: "r3", Values: map[string]any{"department": "rnd", "amount": int64(30)}},
	})
	if err != nil {
		t.Fatalf("NewBaseRelation: %v", err)
	}
	return relation
}

func TestProjectionSelectionRewriteInvariant(t *testing.T) {
	base := baseExpenses(t)
	selected, err := Select(base, []string{"department"}, func(row Row) bool { return row.Cells["department"].Value == "sales" })
	if err != nil {
		t.Fatal(err)
	}
	left, err := Project(selected, "department", "amount")
	if err != nil {
		t.Fatal(err)
	}
	projected, err := Project(base, "department", "amount")
	if err != nil {
		t.Fatal(err)
	}
	right, err := Select(projected, []string{"department"}, func(row Row) bool { return row.Cells["department"].Value == "sales" })
	if err != nil {
		t.Fatal(err)
	}
	leftObservation, _ := Observe(testProfile, left)
	rightObservation, _ := Observe(testProfile, right)
	assertSameObservation(t, leftObservation, rightObservation)
}

func TestJoinMultiplicityDoesNotMultiplyUniqueInfluence(t *testing.T) {
	left, _ := NewBaseRelation("employees", "snapshot-1", []string{"department"}, []BaseRow{
		{EntityKey: "e1", Values: map[string]any{"department": "sales"}},
	})
	right, _ := NewBaseRelation("expenses", "snapshot-1", []string{"department", "amount"}, []BaseRow{
		{EntityKey: "x1", Values: map[string]any{"department": "sales", "amount": 10}},
		{EntityKey: "x2", Values: map[string]any{"department": "sales", "amount": 20}},
	})
	joined, err := Join(left, right, "department", "department")
	if err != nil {
		t.Fatal(err)
	}
	observation, err := Observe(testProfile, joined, "left.department", "right.amount")
	if err != nil {
		t.Fatal(err)
	}
	set, _ := NewFactSet(observation.Influence...)
	leftDepartment, _ := NewFact("employees", "snapshot-1", "e1", "department", "sales")
	hash, _ := leftDepartment.HashBytes()
	if _, ok := set[hash]; !ok {
		t.Fatal("left join key is absent from source influence")
	}
	count := 0
	for candidateHash := range set {
		if candidateHash == hash {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("left join key charged %d times, want once", count)
	}
}

func TestSplitMergeAndPaginationConserveExposure(t *testing.T) {
	base := baseExpenses(t)
	full, _ := Observe(testProfile, base)
	first, _ := Page(base, 0, 2)
	second, _ := Page(base, 2, 2)
	firstObservation, _ := Observe(testProfile, first)
	secondObservation, _ := Observe(testProfile, second)
	merged, err := MergeObservations(testProfile, firstObservation, secondObservation, firstObservation)
	if err != nil {
		t.Fatal(err)
	}
	assertSameObservation(t, full, merged)

	departments, _ := Project(base, "department")
	amounts, _ := Project(base, "amount")
	departmentObservation, _ := Observe(testProfile, departments)
	amountObservation, _ := Observe(testProfile, amounts)
	merged, _ = MergeObservations(testProfile, departmentObservation, amountObservation)
	assertSameObservation(t, full, merged)
}

func TestAggregateSeparatesReleaseFromSourceInfluence(t *testing.T) {
	base := baseExpenses(t)
	aggregated, err := Aggregate(base, []string{"department"}, []AggregateSpec{{Function: "sum", Field: "amount", Alias: "total"}, {Function: "count", Field: "*", Alias: "items"}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := Observe(testProfile, aggregated)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Release) != 6 {
		t.Fatalf("release facts = %d, want 6", len(observation.Release))
	}
	if len(observation.Influence) <= len(observation.Release) {
		t.Fatalf("source influence = %d, release = %d; aggregate influence was collapsed to output rows", len(observation.Influence), len(observation.Release))
	}
}

func TestGlobalAggregateUsesStableDerivedEntity(t *testing.T) {
	base := baseExpenses(t)
	aggregated, err := Aggregate(base, nil, []AggregateSpec{{Function: "count", Field: "*", Alias: "items"}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := Observe(testProfile, aggregated)
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregated.Rows) != 1 || len(observation.Release) != 1 || len(observation.Influence) != len(base.Rows) {
		t.Fatalf("global aggregate rows=%d release=%d influence=%d", len(aggregated.Rows), len(observation.Release), len(observation.Influence))
	}
}

func TestAggregateReleaseBindsCanonicalExpressionAndSources(t *testing.T) {
	left, err := NewBaseRelation("expenses", "snapshot-1", []string{"department", "amount"}, []BaseRow{
		{EntityKey: "left", Values: map[string]any{"department": "sales", "amount": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewBaseRelation("expenses", "snapshot-1", []string{"department", "amount"}, []BaseRow{
		{EntityKey: "right-1", Values: map[string]any{"department": "sales", "amount": 2}},
		{EntityKey: "right-2", Values: map[string]any{"department": "sales", "amount": 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	leftAggregate, _ := Aggregate(left, []string{"department"}, []AggregateSpec{{Function: "sum", Field: "amount", Alias: "left_alias"}})
	leftRenamed, _ := Aggregate(left, []string{"department"}, []AggregateSpec{{Function: "sum", Field: "amount", Alias: "renamed"}})
	rightAggregate, _ := Aggregate(right, []string{"department"}, []AggregateSpec{{Function: "sum", Field: "amount", Alias: "right_alias"}})
	leftObservation, _ := Observe(testProfile, leftAggregate, "left_alias")
	renamedObservation, _ := Observe(testProfile, leftRenamed, "renamed")
	rightObservation, _ := Observe(testProfile, rightAggregate, "right_alias")
	if len(leftObservation.Release) != 1 || leftObservation.Release[0].Field != "sum(amount)" {
		t.Fatalf("left aggregate release = %+v", leftObservation.Release)
	}
	assertSameObservation(t, leftObservation, renamedObservation)
	if sameHashes(mustFactSet(t, leftObservation.Release), mustFactSet(t, rightObservation.Release)) {
		t.Fatal("equal aggregate values from different sources reused one release identity")
	}
}

func TestAggregatesMatchSQLNullSemantics(t *testing.T) {
	base, err := NewBaseRelation("expenses", "snapshot-1", []string{"amount"}, []BaseRow{
		{EntityKey: "null", Values: map[string]any{"amount": nil}},
		{EntityKey: "ten", Values: map[string]any{"amount": int64(10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregated, err := Aggregate(base, nil, []AggregateSpec{
		{Function: "count", Field: "amount", Alias: "non_null"},
		{Function: "count", Field: "*", Alias: "rows"},
		{Function: "sum", Field: "amount", Alias: "total"},
		{Function: "min", Field: "amount", Alias: "minimum"},
		{Function: "max", Field: "amount", Alias: "maximum"},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := aggregated.Rows[0]
	if row.Cells["non_null"].Value != int64(1) || row.Cells["rows"].Value != int64(2) ||
		row.Cells["total"].Value != float64(10) || row.Cells["minimum"].Value != int64(10) ||
		row.Cells["maximum"].Value != int64(10) {
		t.Fatalf("NULL aggregate values = %+v", row.Cells)
	}

	allNull, _ := NewBaseRelation("expenses", "snapshot-1", []string{"amount"}, []BaseRow{
		{EntityKey: "null", Values: map[string]any{"amount": nil}},
	})
	aggregated, err = Aggregate(allNull, nil, []AggregateSpec{
		{Function: "count", Field: "amount", Alias: "items"},
		{Function: "sum", Field: "amount", Alias: "total"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aggregated.Rows[0].Cells["items"].Value != int64(0) || aggregated.Rows[0].Cells["total"].Value != nil {
		t.Fatalf("all-NULL aggregate values = %+v", aggregated.Rows[0].Cells)
	}
}

func TestSnapshotAndValueVersionCreateNewFacts(t *testing.T) {
	first, _ := NewFact("expense_detail", "snapshot-1", "r1", "amount", 10)
	updated, _ := NewFact("expense_detail", "snapshot-2", "r1", "amount", 20)
	firstHash, _ := first.Hash()
	updatedHash, _ := updated.Hash()
	if firstHash == updatedHash {
		t.Fatal("updated snapshot reused the old fact identity")
	}
}

func assertSameObservation(t *testing.T, left, right Observation) {
	t.Helper()
	leftRelease, _ := NewFactSet(left.Release...)
	rightRelease, _ := NewFactSet(right.Release...)
	leftInfluence, _ := NewFactSet(left.Influence...)
	rightInfluence, _ := NewFactSet(right.Influence...)
	if !sameHashes(leftRelease, rightRelease) || !sameHashes(leftInfluence, rightInfluence) {
		t.Fatalf("observations differ:\nleft=%+v\nright=%+v", left, right)
	}
}

func sameHashes(left, right FactSet) bool {
	if len(left) != len(right) {
		return false
	}
	for hash := range left {
		if _, ok := right[hash]; !ok {
			return false
		}
	}
	return true
}

func mustFactSet(t *testing.T, facts []FactID) FactSet {
	t.Helper()
	set, err := NewFactSet(facts...)
	if err != nil {
		t.Fatal(err)
	}
	return set
}
