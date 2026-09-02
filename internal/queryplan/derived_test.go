package queryplan

import (
	"strings"
	"testing"
)

func derivedTestProduct() Product {
	return Product{
		Name: "sales_product", SourceNamespace: "sales", Snapshot: "snap-1",
		Columns: map[string]struct{}{"price": {}, "qty": {}, "region": {}},
		ColumnTypes: map[string]string{"price": "numeric", "qty": "numeric", "region": "text"},
		ColumnCollations: map[string]string{"region": "en_US.utf8"},
		CollationVersions: map[string]string{"region": "2.36"},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
	}
}

func revenueColumn() DerivedColumn {
	return DerivedColumn{Alias: "revenue", SQLType: "numeric", Expr: &DerivedExpr{
		Op: ArithMul, SQLType: "numeric", Operands: []ArithOperand{
			{Column: "price", SQLType: "numeric"}, {Column: "qty", SQLType: "numeric"},
		}}}
}

func TestCompileEmitsDerivedProjection(t *testing.T) {
	product := derivedTestProduct()
	sql, err := Compile(QueryPlan{Product: product.Name, Columns: []string{"region"},
		Derived: []DerivedColumn{revenueColumn()}}, product)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `("price" * "qty") AS "revenue"`) {
		t.Fatalf("derived projection missing from SQL: %s", sql)
	}
}

func TestNormalFormV5OnlyWithArithmetic(t *testing.T) {
	product := derivedTestProduct()
	plain, err := NormalizeV2(QueryPlan{Product: product.Name, Columns: []string{"region"}}, product)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Version != NormalFormVersion || len(plain.Derived) != 0 {
		t.Fatalf("plan without arithmetic must keep the frozen V3 identity, got %s", plain.Version)
	}
	derived, err := NormalizeV2(QueryPlan{Product: product.Name, Columns: []string{"region"},
		Derived: []DerivedColumn{revenueColumn()}}, product)
	if err != nil {
		t.Fatal(err)
	}
	if derived.Version != NormalFormVersionV5 {
		t.Fatalf("arithmetic plan must settle under V5, got %s", derived.Version)
	}
	if len(derived.Derived) != 1 || derived.Derived[0].Expression != "mul(sales.price,sales.qty)" {
		t.Fatalf("canonical derived spelling wrong: %+v", derived.Derived)
	}
}

func TestDerivedTypeDomainFailsClosed(t *testing.T) {
	product := derivedTestProduct()
	mixed := revenueColumn()
	mixed.Expr.Operands[1] = ArithOperand{Column: "region", SQLType: "numeric"}
	if _, err := NormalizeV2(QueryPlan{Product: product.Name, Columns: []string{"region"},
		Derived: []DerivedColumn{mixed}}, product); err == nil {
		t.Fatal("text operand must fail closed")
	}
	unapproved := revenueColumn()
	unapproved.Expr.Operands[1] = ArithOperand{Column: "secret", SQLType: "numeric"}
	if _, err := Compile(QueryPlan{Product: product.Name, Columns: []string{"region"},
		Derived: []DerivedColumn{unapproved}}, product); err == nil {
		t.Fatal("unapproved column must fail closed at Compile")
	}
}
