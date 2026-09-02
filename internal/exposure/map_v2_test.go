package exposure

import "testing"

func mapTestRelation(t *testing.T) RelationV2 {
	t.Helper()
	base, err := ScanV2(BaseRelationSpecV2{SourceNamespace: "sales.orders", Snapshot: "snapshot-1", StableRole: "orders",
		Fields: []FieldV2{{ID: "price", SQLType: "numeric"}, {ID: "qty", SQLType: "bigint"}},
		Rows: []BaseRowV2{
			{EntityKey: "r1", Values: map[string]any{"price": "10.50", "qty": int64(3)}},
			{EntityKey: "r2", Values: map[string]any{"price": "2", "qty": int64(7)}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func TestMapV2DerivedValueAndDependencyUnion(t *testing.T) {
	base := mapTestRelation(t)
	mapped, err := MapV2(base, []DerivedFieldSpecV2{{
		OutputID: "revenue", OutputType: "numeric",
		Expression: "mul(sales.orders.price,sales.orders.qty)",
		Tree:       &DerivedNodeV2{Op: "mul", Left: &DerivedNodeV2{Field: "price"}, Right: &DerivedNodeV2{Field: "qty"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, row := range mapped.Rows {
		cell := row.Cells["revenue"]
		values[row.Key] = cell.Value.(string)
		if cell.ReleaseFact != nil {
			t.Fatal("derived cell must mint its Release identity at Observe, not carry a base fact")
		}
		priceSupport := row.Cells["price"].Support
		if cell.Support.Len() < priceSupport.Len() {
			t.Fatal("derived dependency must contain the argument cells' support")
		}
	}
	if len(values) != 2 {
		t.Fatalf("expected two rows, got %v", values)
	}
	found := map[string]bool{}
	for _, value := range values {
		found[value] = true
	}
	if !found["31.5"] || !found["14"] {
		t.Fatalf("derived values wrong: %v", values)
	}
	effect, err := ObserveV2(mapped)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Release) == 0 || effect.Dependency.Len() == 0 {
		t.Fatal("observed derived relation must carry Release and Dependency facts")
	}
}

func TestMapV2FailsClosed(t *testing.T) {
	base := mapTestRelation(t)
	if _, err := MapV2(base, []DerivedFieldSpecV2{{
		OutputID: "x", OutputType: "integer", Expression: "mul(a,b)",
		Tree: &DerivedNodeV2{Op: "mul",
			Left:  &DerivedNodeV2{Field: "qty"},
			Right: &DerivedNodeV2{Literal: "4000000000"}},
	}}); err == nil {
		t.Fatal("integer overflow must fail closed")
	}
	if _, err := MapV2(base, []DerivedFieldSpecV2{{
		OutputID: "y", OutputType: "numeric", Expression: "div(a,b)",
		Tree: &DerivedNodeV2{Op: "div",
			Left:  &DerivedNodeV2{Field: "price"},
			Right: &DerivedNodeV2{Field: "qty"}},
	}}); err == nil {
		t.Fatal("division must stay outside the exact fold profile")
	}
	if _, err := MapV2(base, []DerivedFieldSpecV2{{
		OutputID: "z", OutputType: "numeric", Expression: "mul(a,b)",
		Tree: &DerivedNodeV2{Op: "mul",
			Left:  &DerivedNodeV2{Field: "absent"},
			Right: &DerivedNodeV2{Field: "qty"}},
	}}); err == nil {
		t.Fatal("an absent argument field must fail closed")
	}
}
