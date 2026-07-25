package exposure

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
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
		Fields: []FieldV2{{ID: "employee.department", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows:   []BaseRowV2{{EntityKey: "e1", Values: map[string]any{"employee.department": "sales"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "s1", StableRole: "expense",
		Fields: []FieldV2{{ID: "expense.department", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}, {ID: "expense.amount", SQLType: "numeric"}},
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

func TestV2JoinUsesPostgreSQLNullAndTypedNumericEquality(t *testing.T) {
	nullLeft, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.key", SQLType: "numeric"}},
		Rows:   []BaseRowV2{{EntityKey: "l-null", Values: map[string]any{"left.key": nil}}}})
	if err != nil {
		t.Fatal(err)
	}
	nullRight, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.key", SQLType: "decimal"}},
		Rows:   []BaseRowV2{{EntityKey: "r-null", Values: map[string]any{"right.key": nil}}}})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := JoinV2(nullLeft, nullRight, "left.key", "right.key")
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.Rows) != 0 {
		t.Fatalf("NULL = NULL produced %d V2 join rows", len(joined.Rows))
	}

	left, _ := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.key", SQLType: "numeric"}},
		Rows:   []BaseRowV2{{EntityKey: "l-one", Values: map[string]any{"left.key": "1.00"}}}})
	right, _ := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.key", SQLType: "numeric"}},
		Rows:   []BaseRowV2{{EntityKey: "r-one", Values: map[string]any{"right.key": "1"}}}})
	joined, err = JoinV2(left, right, "left.key", "right.key")
	if err != nil || len(joined.Rows) != 1 {
		t.Fatalf("typed numeric equality join rows=%d err=%v", len(joined.Rows), err)
	}
}

func TestV2JoinRejectsCollationDrift(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows:   []BaseRowV2{{EntityKey: "l1", Values: map[string]any{"left.key": "x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.key", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}},
		Rows:   []BaseRowV2{{EntityKey: "r1", Values: map[string]any{"right.key": "x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JoinV2(left, right, "left.key", "right.key"); err == nil {
		t.Fatal("join accepted keys with different exact collation profiles")
	}
}

func TestV2EmptyRelationsStillRejectJSONEqualityOperators(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "json.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.value", SQLType: "json"}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "json.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.value", SQLType: "json"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JoinV2(left, right, "left.value", "right.value"); err == nil {
		t.Fatal("empty json inputs bypassed the V2 join type rule")
	}
	if _, err := AggregateFromResultsV2(left, []string{"left.value"}, nil, nil); err == nil {
		t.Fatal("empty json input bypassed the V2 group type rule")
	}
}

func TestV2RelationRejectsForgedReleaseProvenance(t *testing.T) {
	base := v2Expenses(t)
	forged := cloneRelationShapeV2(base)
	forged.Rows = append(forged.Rows, cloneRowV2(base.Rows[0]))
	otherFact := *base.Rows[1].Cells["amount"].ReleaseFact
	cell := forged.Rows[0].Cells["amount"]
	cell.Value = base.Rows[1].Cells["amount"].Value
	cell.ReleaseFact = &otherFact
	forged.Rows[0].Cells["amount"] = cell
	if err := ValidateRelationV2(forged); err == nil {
		t.Fatal("relation accepted a same-typed release fact absent from cell support")
	}

	grouped, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "total": "30"}, {"department": "rnd", "total": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	derivedFact, err := materializeCellReleaseV2(grouped, grouped.Rows[0], grouped.Rows[0].Cells["total"])
	if err != nil {
		t.Fatal(err)
	}
	derivedFact.WitnessCommitment = strings.Repeat("0", 64)
	derived := cloneRelationShapeV2(grouped)
	derived.Rows = append(derived.Rows, cloneRowV2(grouped.Rows[0]))
	derivedCell := derived.Rows[0].Cells["total"]
	derivedCell.ReleaseFact = &derivedFact
	derived.Rows[0].Cells["total"] = derivedCell
	if err := ValidateRelationV2(derived); err == nil {
		t.Fatal("relation accepted a derived release fact with a forged witness commitment")
	}
}

func TestV2ConjunctiveJoinAndNestedDerivedJoinAreClosed(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.k1", SQLType: "integer"}, {ID: "left.k2", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows: []BaseRowV2{
			{EntityKey: "l1", Values: map[string]any{"left.k1": int64(1), "left.k2": "x"}},
			{EntityKey: "l2", Values: map[string]any{"left.k1": int64(1), "left.k2": nil}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "join.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.k1", SQLType: "integer"}, {ID: "right.k2", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"right.k1": int64(1), "right.k2": "x"}},
			{EntityKey: "r2", Values: map[string]any{"right.k1": int64(1), "right.k2": nil}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	predicates := []JoinPredicateV2{{LeftField: "left.k1", RightField: "right.k1"}, {LeftField: "left.k2", RightField: "right.k2"}}
	joined, err := JoinOnV2(left, right, predicates)
	if err != nil || len(joined.Rows) != 1 {
		t.Fatalf("conjunctive join rows=%d err=%v", len(joined.Rows), err)
	}
	swapped, err := JoinOnV2(right, left, []JoinPredicateV2{
		{LeftField: "right.k2", RightField: "left.k2"}, {LeftField: "right.k1", RightField: "left.k1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joinedEffect, _ := ObserveV2(joined)
	swappedEffect, _ := ObserveV2(swapped)
	assertSameObservation(t, joinedEffect, swappedEffect)
	if joined.Rows[0].Key != swapped.Rows[0].Key {
		t.Fatal("conjunctive join row identity depends on operand or predicate order")
	}

	grouped, err := AggregateFromResultsV2(left, []string{"left.k2"}, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "left.count", OutputType: "bigint"},
	}, []map[string]any{{"left.k2": "x", "left.count": int64(1)}, {"left.k2": nil, "left.count": int64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := JoinV2(grouped, right, "left.k2", "right.k2")
	if err != nil || len(nested.Rows) != 1 {
		t.Fatalf("join after group is not closed: rows=%d err=%v", len(nested.Rows), err)
	}
	if err := ValidateRelationV2(nested); err != nil {
		t.Fatalf("nested relation invariant: %v", err)
	}
}

func TestV2UnionDistinctIsIdempotentForBaseAndDerivedEffects(t *testing.T) {
	base := v2Expenses(t)
	baseEffect, _ := ObserveV2(base)
	baseUnion, err := UnionDistinctV2(base, base)
	if err != nil {
		t.Fatal(err)
	}
	baseUnionEffect, err := ObserveV2(baseUnion)
	if err != nil {
		t.Fatal(err)
	}
	assertSameObservation(t, baseEffect, baseUnionEffect)

	aggregated, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "total": "30"}, {"department": "rnd", "total": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	aggregateEffect, _ := ObserveV2(aggregated)
	aggregateUnion, err := UnionDistinctV2(aggregated, aggregated)
	if err != nil {
		t.Fatal(err)
	}
	aggregateUnionEffect, err := ObserveV2(aggregateUnion)
	if err != nil {
		t.Fatal(err)
	}
	assertSameObservation(t, aggregateEffect, aggregateUnionEffect)
}

func TestV2UnionDistinctCommutativitySurvivesDownstreamJoin(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "u.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}}, Rows: []BaseRowV2{{EntityKey: "l1", Values: map[string]any{"u.key": "same"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "u.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}}, Rows: []BaseRowV2{{EntityKey: "r1", Values: map[string]any{"u.key": "same"}}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := UnionDistinctV2(left, right)
	if err != nil {
		t.Fatal(err)
	}
	second, err := UnionDistinctV2(right, left)
	if err != nil {
		t.Fatal(err)
	}
	firstEffect, _ := ObserveV2(first)
	secondEffect, _ := ObserveV2(second)
	assertSameObservation(t, firstEffect, secondEffect)

	lookup, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.lookup", Snapshot: "s1", StableRole: "lookup",
		Fields: []FieldV2{{ID: "lookup.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}}, Rows: []BaseRowV2{{EntityKey: "x1", Values: map[string]any{"lookup.key": "same"}}}})
	if err != nil {
		t.Fatal(err)
	}
	joinedFirst, err := JoinV2(first, lookup, "u.key", "lookup.key")
	if err != nil {
		t.Fatal(err)
	}
	joinedSecond, err := JoinV2(second, lookup, "u.key", "lookup.key")
	if err != nil {
		t.Fatal(err)
	}
	if joinedFirst.Rows[0].Key != joinedSecond.Rows[0].Key {
		t.Fatal("UNION operand order leaked into a downstream join row key")
	}
	firstEffect, _ = ObserveV2(joinedFirst)
	secondEffect, _ = ObserveV2(joinedSecond)
	assertSameObservation(t, firstEffect, secondEffect)
}

func TestV2UnionDistinctCommutativitySurvivesDownstreamGroup(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "u.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows:   []BaseRowV2{{EntityKey: "l1", Values: map[string]any{"u.key": "a"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "u.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}},
		Rows:   []BaseRowV2{{EntityKey: "r1", Values: map[string]any{"u.key": "b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := UnionDistinctV2(left, right)
	if err != nil {
		t.Fatal(err)
	}
	second, err := UnionDistinctV2(right, left)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []map[string]any{{"u.key": "a", "n": int64(1)}, {"u.key": "b", "n": int64(1)}}
	firstGroup, err := AggregateFromResultsV2(first, []string{"u.key"}, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
	}, outputs)
	if err != nil {
		t.Fatal(err)
	}
	secondGroup, err := AggregateFromResultsV2(second, []string{"u.key"}, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
	}, outputs)
	if err != nil {
		t.Fatal(err)
	}
	firstEffect, _ := ObserveV2(firstGroup)
	secondEffect, _ := ObserveV2(secondGroup)
	assertSameObservation(t, firstEffect, secondEffect)
}

func TestV2GroupUsesNotDistinctNullSemanticsAndCompleteOracle(t *testing.T) {
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "group.input", Snapshot: "s1", StableRole: "input",
		Fields: []FieldV2{{ID: "group.key", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"group.key": nil}},
			{EntityKey: "r2", Values: map[string]any{"group.key": nil}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := AggregateFromResultsV2(base, []string{"group.key"}, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
	}, []map[string]any{{"group.key": nil, "n": int64(2)}})
	if err != nil || len(grouped.Rows) != 1 {
		t.Fatalf("NULL group rows=%d err=%v", len(grouped.Rows), err)
	}
	if _, err := AggregateFromResultsV2(base, []string{"group.key"}, nil, nil); err == nil {
		t.Fatal("incomplete PostgreSQL group oracle was accepted")
	}
}

func TestV2CanonicalSQLDomainIsTypedAndTotalOnAdmissibleValues(t *testing.T) {
	for _, test := range []struct {
		sqlType string
		left    any
		right   any
	}{
		{sqlType: "numeric", left: "1.00", right: "1"},
		{sqlType: "character", left: "x   ", right: "x"},
		{sqlType: "jsonb", left: []byte(`{"b":1.0,"a":[true,null]}`), right: []byte(`{"a":[true,null],"b":1}`)},
		{sqlType: "uuid", left: "550E8400-E29B-41D4-A716-446655440000", right: "550e8400e29b41d4a716446655440000"},
	} {
		left, err := CanonicalSQLValue(test.sqlType, test.left)
		if err != nil {
			t.Fatalf("CanonicalSQLValue(%s,left): %v", test.sqlType, err)
		}
		right, err := CanonicalSQLValue(test.sqlType, test.right)
		if err != nil {
			t.Fatalf("CanonicalSQLValue(%s,right): %v", test.sqlType, err)
		}
		if left != right {
			t.Fatalf("%s semantic equivalents differ: %q / %q", test.sqlType, left, right)
		}
	}
	date, err := CanonicalSQLValue("date", time.Date(2026, 7, 25, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)))
	if err != nil || date != "d:2026-07-25" {
		t.Fatalf("date canonicalization=%q err=%v", date, err)
	}
	if _, err := CanonicalSQLValue("numeric", float64(0.1)); err == nil {
		t.Fatal("binary float entered the exact numeric Fact domain")
	}
	if _, err := CanonicalSQLValue("uuid", "not-a-uuid"); err == nil {
		t.Fatal("invalid UUID entered the V2 Fact domain")
	}
	if _, err := CanonicalSQLValue("time with time zone", "12:00:00+08"); err == nil {
		t.Fatal("unsupported timetz entered the V2 Fact domain")
	}
	if value, err := CanonicalSQLValue("double precision", math.NaN()); err != nil || value != "f:nan" {
		t.Fatalf("PostgreSQL NaN canonicalization=%q err=%v", value, err)
	}
	if value, err := CanonicalSQLValue("numeric", "NaN"); err != nil || value != "n:nan" {
		t.Fatalf("PostgreSQL numeric NaN canonicalization=%q err=%v", value, err)
	}
	if _, err := CanonicalSQLValue("smallint", int64(32768)); err == nil {
		t.Fatal("out-of-range smallint entered the V2 value domain")
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
		Fields: []FieldV2{{ID: "department", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}, {ID: "amount", SQLType: "numeric"}},
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
