package exposure

import (
	"math"
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
	if len(leftEffect.Release) != 2 {
		t.Fatalf("equal-valued aggregate groups collapsed to %d derived facts", len(leftEffect.Release))
	}
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

func TestV2RejectsFloatingPointSumForDeterminism(t *testing.T) {
	// IEEE-754 addition is non-associative, so PostgreSQL's SUM(real)/SUM(double
	// precision) depends on physical aggregation order the typed normal form does
	// not fix. The closed language admits SUM only over the exact/integer
	// fragment; floating-point SUM must fail closed to preserve Effect determinism.
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "snapshot-1", StableRole: "expense",
		Fields: []FieldV2{
			{ID: "amount", SQLType: "numeric"},
			{ID: "measure", SQLType: "double precision"},
			{ID: "reading", SQLType: "real"},
		}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"amount": "10", "measure": 1.5, "reading": 2.5}},
			{EntityKey: "r2", Values: map[string]any{"amount": "20", "measure": 0.5, "reading": 0.5}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		typed string
	}{
		{"measure", "double precision"},
		{"reading", "real"},
	} {
		_, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
			{Function: "sum", Field: tc.field, OutputID: "total", OutputType: tc.typed},
		}, []map[string]any{{"total": 2.0}})
		if err == nil {
			t.Fatalf("SUM(%s) was admitted; expected rejection for determinism", tc.typed)
		}
	}
	// Sanity: SUM over the exact/integer fragment remains admissible.
	if _, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
	}, []map[string]any{{"total": "30"}}); err != nil {
		t.Fatalf("SUM(numeric) should still be admitted: %v", err)
	}
}

func TestV2EmptyGlobalAggregateFactBindsProductAndSnapshotBundle(t *testing.T) {
	observe := func(namespace, snapshot string) (RelationV2, map[string]string) {
		t.Helper()
		base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: namespace, Snapshot: snapshot, StableRole: "input",
			Fields: []FieldV2{{ID: "amount", SQLType: "numeric"}}})
		if err != nil {
			t.Fatal(err)
		}
		aggregated, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
			{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
			{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
		}, []map[string]any{{"n": int64(0), "total": nil}})
		if err != nil {
			t.Fatal(err)
		}
		if len(aggregated.Rows) != 1 || aggregated.Rows[0].Cells["n"].Value != int64(0) || aggregated.Rows[0].Cells["total"].Value != nil {
			t.Fatalf("empty global aggregate = %+v", aggregated.Rows)
		}
		effect, err := ObserveV2(aggregated)
		if err != nil {
			t.Fatal(err)
		}
		if len(effect.Release) != 2 || len(effect.Influence) != 0 {
			t.Fatalf("empty global effect = %+v", effect)
		}
		hashes := make(map[string]string, len(effect.Release))
		for _, fact := range effect.Release {
			hash, hashErr := fact.Hash()
			if hashErr != nil {
				t.Fatal(hashErr)
			}
			hashes[fact.NormalizedExpression] = hash
		}
		return aggregated, hashes
	}

	first, firstFacts := observe("global.product", "snapshot-1")
	changedSnapshot, snapshotFacts := observe("global.product", "snapshot-2")
	changedProduct, productFacts := observe("other.product", "snapshot-1")
	if first.Rows[0].Key != changedSnapshot.Rows[0].Key || first.Rows[0].Key != changedProduct.Rows[0].Key {
		t.Fatal("global aggregate did not use its canonical constant output-row key")
	}
	if firstFacts["count(*)"] == snapshotFacts["count(*)"] || firstFacts["count(*)"] == productFacts["count(*)"] {
		t.Fatal("global aggregate FactID omitted its product/snapshot bundle")
	}
}

func TestV2OutputRowKeysDistinguishJoinPairsAndUnionTuples(t *testing.T) {
	left, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "keys.left", Snapshot: "s1", StableRole: "left",
		Fields: []FieldV2{{ID: "left.k", SQLType: "integer"}},
		Rows:   []BaseRowV2{{EntityKey: "l1", Values: map[string]any{"left.k": int64(1)}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "keys.right", Snapshot: "s1", StableRole: "right",
		Fields: []FieldV2{{ID: "right.k", SQLType: "integer"}}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"right.k": int64(1)}},
			{EntityKey: "r2", Values: map[string]any{"right.k": int64(1)}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := JoinV2(left, right, "left.k", "right.k")
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.Rows) != 2 || joined.Rows[0].Key == joined.Rows[1].Key {
		t.Fatalf("join pair keys = %+v", joined.Rows)
	}

	base := v2Expenses(t)
	projected, err := ProjectV2(base, "department")
	if err != nil {
		t.Fatal(err)
	}
	union, err := UnionDistinctV2(projected, projected)
	if err != nil {
		t.Fatal(err)
	}
	if len(union.Rows) != 2 || union.Rows[0].Key == union.Rows[1].Key {
		t.Fatalf("union typed-tuple keys = %+v", union.Rows)
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

func TestV2RejectsJSONAtTheTypeBoundary(t *testing.T) {
	if _, err := CanonicalSQLTypeV2("json"); err == nil {
		t.Fatal("PostgreSQL json entered the V2 type domain")
	}
	if _, err := CanonicalSQLValue("json", []byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("PostgreSQL json entered the V2 value domain")
	}
	if _, err := CanonicalSQLValue("json", nil); err == nil {
		t.Fatal("NULL with PostgreSQL json type entered the V2 value domain")
	}
	if _, err := NewBaseCellFactV2("json.source", "s1", "row-1", "value", "json", []byte(`{"a":1}`)); err == nil {
		t.Fatal("PostgreSQL json produced a V2 FactID")
	}
	if _, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "json.source", Snapshot: "s1", StableRole: "source",
		Fields: []FieldV2{{ID: "source.value", SQLType: "json"}}}); err == nil {
		t.Fatal("PostgreSQL json entered a V2 relation")
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

	baseCount, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
	}, []map[string]any{{"n": int64(len(base.Rows))}})
	if err != nil {
		t.Fatal(err)
	}
	unionCount, err := AggregateFromResultsV2(baseUnion, nil, []AggregateSpecV2{
		{Function: "count", Field: "*", OutputID: "n", OutputType: "bigint"},
	}, []map[string]any{{"n": int64(len(base.Rows))}})
	if err != nil {
		t.Fatal(err)
	}
	baseCountEffect, _ := ObserveV2(baseCount)
	unionCountEffect, _ := ObserveV2(unionCount)
	assertSameObservation(t, baseCountEffect, unionCountEffect)
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

func TestV2HiddenGroupKeyIsPositiveOutputDependency(t *testing.T) {
	base := v2Expenses(t)
	grouped, err := AggregateFromResultsV2(base, []string{"department"}, []AggregateSpecV2{
		{Function: "sum", Field: "amount", OutputID: "total", OutputType: "numeric"},
	}, []map[string]any{{"department": "sales", "total": "30"}, {"department": "rnd", "total": "30"}})
	if err != nil {
		t.Fatal(err)
	}
	effect, err := ObserveV2(grouped, "total")
	if err != nil {
		t.Fatal(err)
	}
	dependency := mustFactSet(t, effect.Influence)
	if len(effect.Release) != 2 || len(dependency) != 9 {
		t.Fatalf("hidden-key group release=%d dependency=%d, want 2/9", len(effect.Release), len(dependency))
	}
	for _, row := range base.Rows {
		assertFactSetContainsV2(t, dependency, row.RowSupport)
		assertFactSetContainsV2(t, dependency, row.Cells["department"].Support)
		assertFactSetContainsV2(t, dependency, row.Cells["amount"].Support)
	}
}

func TestV2UnionDistinctIncludesHiddenEquivalenceFields(t *testing.T) {
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "union.hidden", Snapshot: "s1", StableRole: "input",
		Fields: []FieldV2{
			{ID: "visible", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
			{ID: "dedup", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
		}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"visible": "same", "dedup": "left"}},
			{EntityKey: "r2", Values: map[string]any{"visible": "same", "dedup": "right"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	left, err := SelectV2(base, nil, func(row AnnotatedRowV2) SQLTruth {
		if row.Key == "r1" {
			return SQLTrue
		}
		return SQLFalse
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := SelectV2(base, nil, func(row AnnotatedRowV2) SQLTruth {
		if row.Key == "r2" {
			return SQLTrue
		}
		return SQLFalse
	})
	if err != nil {
		t.Fatal(err)
	}
	union, err := UnionDistinctV2(left, right)
	if err != nil {
		t.Fatal(err)
	}
	effect, err := ObserveV2(union, "visible")
	if err != nil {
		t.Fatal(err)
	}
	dependency := mustFactSet(t, effect.Influence)
	if len(effect.Release) != 2 || len(dependency) != 6 {
		t.Fatalf("hidden-dedup union release=%d dependency=%d, want 2/6", len(effect.Release), len(dependency))
	}
	for _, row := range base.Rows {
		assertFactSetContainsV2(t, dependency, row.RowSupport)
		assertFactSetContainsV2(t, dependency, row.Cells["visible"].Support)
		assertFactSetContainsV2(t, dependency, row.Cells["dedup"].Support)
	}
}

func TestV2AggregateArgumentsConservativelyIncludeNullAndNonExtrema(t *testing.T) {
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "aggregate.inputs", Snapshot: "s1", StableRole: "input",
		Fields: []FieldV2{{ID: "amount", SQLType: "numeric"}}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"amount": "2"}},
			{EntityKey: "r2", Values: map[string]any{"amount": "5"}},
			{EntityKey: "r3", Values: map[string]any{"amount": "7"}},
			{EntityKey: "r4", Values: map[string]any{"amount": nil}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	grouped, err := AggregateFromResultsV2(base, nil, []AggregateSpecV2{
		{Function: "count", Field: "amount", OutputID: "n", OutputType: "bigint"},
		{Function: "min", Field: "amount", OutputID: "lo", OutputType: "numeric"},
		{Function: "max", Field: "amount", OutputID: "hi", OutputType: "numeric"},
	}, []map[string]any{{"n": int64(3), "lo": "2", "hi": "7"}})
	if err != nil {
		t.Fatal(err)
	}
	effect, err := ObserveV2(grouped)
	if err != nil {
		t.Fatal(err)
	}
	dependency := mustFactSet(t, effect.Influence)
	if len(effect.Release) != 3 || len(dependency) != 8 {
		t.Fatalf("aggregate release=%d dependency=%d, want 3/8", len(effect.Release), len(dependency))
	}
	for _, row := range base.Rows {
		assertFactSetContainsV2(t, dependency, row.RowSupport)
		assertFactSetContainsV2(t, dependency, row.Cells["amount"].Support)
	}
}

func TestV2SelectionAndPageExcludeNegativeAndOrderInformation(t *testing.T) {
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "boundary.input", Snapshot: "s1", StableRole: "input",
		Fields: []FieldV2{
			{ID: "predicate", SQLType: "boolean"},
			{ID: "order", SQLType: "integer"},
			{ID: "payload", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
		}, Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"predicate": true, "order": int64(1), "payload": "kept"}},
			{EntityKey: "r2", Values: map[string]any{"predicate": false, "order": int64(2), "payload": "false"}},
			{EntityKey: "r3", Values: map[string]any{"predicate": nil, "order": int64(3), "payload": "unknown"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectV2(base, []string{"predicate"}, func(row AnnotatedRowV2) SQLTruth {
		switch row.Cells["predicate"].Value {
		case true:
			return SQLTrue
		case false:
			return SQLFalse
		default:
			return SQLUnknown
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedEffect, err := ObserveV2(selected, "payload")
	if err != nil {
		t.Fatal(err)
	}
	selectedDependency := mustFactSet(t, selectedEffect.Influence)
	if len(selectedDependency) != 3 {
		t.Fatalf("selection dependency=%d, want retained row/predicate/payload only", len(selectedDependency))
	}
	assertFactSetContainsV2(t, selectedDependency, base.Rows[0].RowSupport)
	assertFactSetContainsV2(t, selectedDependency, base.Rows[0].Cells["predicate"].Support)
	assertFactSetContainsV2(t, selectedDependency, base.Rows[0].Cells["payload"].Support)

	base.CanonicalOrder = true // fixture order is the proven order-field order
	page, err := PageV2(base, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	pageEffect, err := ObserveV2(page, "payload")
	if err != nil {
		t.Fatal(err)
	}
	pageDependency := mustFactSet(t, pageEffect.Influence)
	if len(pageDependency) != 2 {
		t.Fatalf("page dependency=%d, want delivered row/payload only", len(pageDependency))
	}
	assertFactSetContainsV2(t, pageDependency, base.Rows[1].RowSupport)
	assertFactSetContainsV2(t, pageDependency, base.Rows[1].Cells["payload"].Support)
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

func assertFactSetContainsV2(t *testing.T, actual FactSet, expected FactSet) {
	t.Helper()
	for hash := range expected {
		if _, present := actual[hash]; !present {
			t.Fatalf("dependency FactSet is missing %s", hash)
		}
	}
}
