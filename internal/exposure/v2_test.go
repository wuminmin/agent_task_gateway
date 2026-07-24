package exposure

import (
	"math/rand"
	"testing"
)

func TestV2FactIdentityIsTypedCanonicalAndProfileBound(t *testing.T) {
	first, err := NewBaseCellFactV2("travel.expense", "snapshot-1", "row-1", "amount", "numeric", "1.00")
	if err != nil {
		t.Fatal(err)
	}
	equivalent, _ := NewBaseCellFactV2("travel.expense", "snapshot-1", "row-1", "amount", "numeric", "1")
	integer, _ := NewBaseCellFactV2("travel.expense", "snapshot-1", "row-1", "amount", "bigint", int64(1))
	changedSnapshot, _ := NewBaseCellFactV2("travel.expense", "snapshot-2", "row-1", "amount", "numeric", "1")
	firstHash, _ := first.Hash()
	equivalentHash, _ := equivalent.Hash()
	integerHash, _ := integer.Hash()
	changedHash, _ := changedSnapshot.Hash()
	if firstHash != equivalentHash {
		t.Fatal("equivalent PostgreSQL numeric values produced different V2 facts")
	}
	if firstHash == integerHash || firstHash == changedHash {
		t.Fatal("SQL type or snapshot change reused a V2 fact")
	}
	payload, err := first.CanonicalPayload()
	if err != nil || len(payload) == 0 {
		t.Fatalf("canonical payload = %x, %v", payload, err)
	}
}

func TestV2ProjectionReusesBaseCellFactsAndAggregateOverlapsInfluence(t *testing.T) {
	base := v2Expenses(t)
	projected, err := ProjectV2(base, "department", "amount")
	if err != nil {
		t.Fatal(err)
	}
	baseObservation, _ := ObserveV2(base, "department", "amount")
	projectedObservation, _ := ObserveV2(projected)
	assertSameObservation(t, baseObservation, projectedObservation)

	aggregated, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "renamed", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "renamed": "30"}, {"department": "rnd", "renamed": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	aggregateObservation, err := ObserveV2(aggregated)
	if err != nil {
		t.Fatal(err)
	}
	rawInfluence, _ := NewFactSet(baseObservation.Influence...)
	aggregateInfluence, _ := NewFactSet(aggregateObservation.Influence...)
	overlap := 0
	for hash := range rawInfluence {
		if _, present := aggregateInfluence[hash]; present {
			overlap++
		}
	}
	if overlap == 0 {
		t.Fatal("aggregate and raw effects have no influence overlap")
	}
}

func TestV2AggregateAliasRewriteProducesIdenticalFactPayloadAndHash(t *testing.T) {
	base := v2Expenses(t)
	left, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "total": "30"}, {"department": "rnd", "total": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "renamed", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "renamed": "30"}, {"department": "rnd", "renamed": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	leftEffect, _ := ObserveV2(left, "total")
	rightEffect, _ := ObserveV2(right, "renamed")
	assertSameObservation(t, leftEffect, rightEffect)
	for index := range leftEffect.Release {
		leftPayload, _ := leftEffect.Release[index].CanonicalPayload()
		rightPayload, _ := rightEffect.Release[index].CanonicalPayload()
		if string(leftPayload) != string(rightPayload) {
			t.Fatalf("alias changed canonical payload: %x / %x", leftPayload, rightPayload)
		}
	}
}

func TestV2CountStarAndCountColumnUseDifferentWitnessKinds(t *testing.T) {
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "snapshot-1", StableRole: "expense",
		Fields: []FieldV2{{ID: "amount", SQLType: "numeric"}}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"amount": "10"}},
			{EntityKey: "r2", Values: map[string]any{"amount": nil}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	aggregated, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "row_count", OutputType: "bigint"},
		{Function: "count", Field: "amount", OutputID: "value_count", OutputType: "bigint"},
	}, []map[string]any{{"row_count": int64(2), "value_count": int64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	effect, err := ObserveV2(aggregated)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Release) != 2 {
		t.Fatalf("release facts = %d, want 2", len(effect.Release))
	}
	left, _ := effect.Release[0].Hash()
	right, _ := effect.Release[1].Hash()
	if left == right {
		t.Fatal("COUNT(*) and COUNT(column) collapsed to one derived fact")
	}
}

func TestV2WitnessPreservesMultiplicityAndJoinOrder(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.employee", Snapshot: "s1", StableRole: "employee",
		Fields: []FieldV2{{ID: "employee.department", SQLType: "text"}},
		Rows:   []BaseRowV2{{EntityKey: "e1", Values: map[string]any{"employee.department": "sales"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "s1", StableRole: "expense",
		Fields: []FieldV2{{ID: "expense.department", SQLType: "text"}, {ID: "expense.amount", SQLType: "numeric"}},
		Rows: []BaseRowV2{
			{EntityKey: "x1", Values: map[string]any{"expense.department": "sales", "expense.amount": "5"}},
			{EntityKey: "x2", Values: map[string]any{"expense.department": "sales", "expense.amount": "5"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := JoinV2(left, right, "employee.department", "expense.department")
	if err != nil {
		t.Fatal(err)
	}
	swapped, err := JoinV2(right, left, "expense.department", "employee.department")
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.Rows) != 2 || len(swapped.Rows) != 2 || joined.Rows[0].Key != swapped.Rows[0].Key || joined.Rows[1].Key != swapped.Rows[1].Key {
		t.Fatalf("join keys depend on input order: %+v / %+v", joined.Rows, swapped.Rows)
	}
	witness := make(WitnessMultiset)
	for _, row := range joined.Rows {
		if err := witness.Merge(row.Cells["employee.department"].Witness); err != nil {
			t.Fatal(err)
		}
	}
	if len(witness) != 1 {
		t.Fatalf("employee witness facts = %d, want 1", len(witness))
	}
	for _, item := range witness {
		if item.Multiplicity != 2 {
			t.Fatalf("join fanout multiplicity = %d, want 2", item.Multiplicity)
		}
	}
}

func TestOptimizeEffectsCountsSharedFactOnce(t *testing.T) {
	fact := v2Fact(t, "shared")
	effect := Observation{ProfileVersion: ProfileV2, Release: []FactID{fact}, Influence: []FactID{fact}}
	plan, err := OptimizeEffects([]EffectCandidate{
		{ID: "a", Requirement: "r1", AnswerCompleteness: 1, Effect: effect},
		{ID: "b", Requirement: "r2", AnswerCompleteness: 1, Effect: effect},
	}, Observation{ProfileVersion: ProfileV2}, 1, 1, UtilityWeights{AnswerCompleteness: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Selected) != 2 || plan.ReleaseCost != 1 || plan.InfluenceCost != 1 || plan.Utility != 2 {
		t.Fatalf("overlap plan = %+v", plan)
	}
}

func TestOptimizeEffectsMatchesBruteForceOracle(t *testing.T) {
	random := rand.New(rand.NewSource(20260724))
	pool := make([]FactID, 8)
	for index := range pool {
		pool[index] = v2Fact(t, string(rune('a'+index)))
	}
	for trial := 0; trial < 500; trial++ {
		var candidates []EffectCandidate
		for requirement := 0; requirement < 3; requirement++ {
			for option := 0; option < 2; option++ {
				release, influence := randomFactSlice(random, pool), randomFactSlice(random, pool)
				candidates = append(candidates, EffectCandidate{ID: string(rune('a' + requirement*2 + option)),
					Requirement: string(rune('x' + requirement)), AnswerCompleteness: float64(1+random.Intn(5)) / 5,
					Effect: Observation{ProfileVersion: ProfileV2, Release: release, Influence: influence}})
			}
		}
		history := Observation{ProfileVersion: ProfileV2, Release: randomFactSlice(random, pool), Influence: randomFactSlice(random, pool)}
		budgetRelease, budgetInfluence := int64(random.Intn(7)), int64(random.Intn(7))
		actual, err := OptimizeEffects(candidates, history, budgetRelease, budgetInfluence, UtilityWeights{AnswerCompleteness: 1})
		if err != nil {
			t.Fatal(err)
		}
		want := bruteForceUtility(t, candidates, history, budgetRelease, budgetInfluence)
		if actual.Utility != want {
			t.Fatalf("trial %d utility=%v want=%v plan=%+v", trial, actual.Utility, want, actual)
		}
	}
}

func v2Expenses(t *testing.T) RelationV2 {
	t.Helper()
	relation, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "snapshot-1", StableRole: "expense",
		Fields: []FieldV2{{ID: "department", SQLType: "text"}, {ID: "amount", SQLType: "numeric"}},
		Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"department": "sales", "amount": "10"}},
			{EntityKey: "r2", Values: map[string]any{"department": "sales", "amount": "20"}},
			{EntityKey: "r3", Values: map[string]any{"department": "rnd", "amount": "30"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	return relation
}

func v2Fact(t *testing.T, entity string) FactID {
	t.Helper()
	fact, err := NewBaseCellFactV2("oracle.source", "snapshot-1", entity, "value", "text", entity)
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

func randomFactSlice(random *rand.Rand, pool []FactID) []FactID {
	var result []FactID
	for _, fact := range pool {
		if random.Intn(3) == 0 {
			result = append(result, fact)
		}
	}
	return result
}

func bruteForceUtility(t *testing.T, candidates []EffectCandidate, history Observation, releaseBudget, influenceBudget int64) float64 {
	t.Helper()
	historyRelease, _ := NewFactSet(history.Release...)
	historyInfluence, _ := NewFactSet(history.Influence...)
	best := float64(0)
	for mask := 0; mask < 1<<len(candidates); mask++ {
		requirements := make(map[string]struct{})
		release, influence := make(FactSet), make(FactSet)
		utility, valid := float64(0), true
		for index, candidate := range candidates {
			if mask&(1<<index) == 0 {
				continue
			}
			if _, duplicate := requirements[candidate.Requirement]; duplicate {
				valid = false
				break
			}
			requirements[candidate.Requirement] = struct{}{}
			candidateRelease, _ := NewFactSet(candidate.Effect.Release...)
			candidateInfluence, _ := NewFactSet(candidate.Effect.Influence...)
			release.Merge(candidateRelease)
			influence.Merge(candidateInfluence)
			utility += candidate.AnswerCompleteness
		}
		if !valid {
			continue
		}
		for hash := range historyRelease {
			delete(release, hash)
		}
		for hash := range historyInfluence {
			delete(influence, hash)
		}
		if int64(len(release)) <= releaseBudget && int64(len(influence)) <= influenceBudget && utility > best {
			best = utility
		}
	}
	return best
}
