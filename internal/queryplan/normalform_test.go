package queryplan

import "testing"

func TestNormalFormV2ErasesAliasesAndSortsRestrictedRewrites(t *testing.T) {
	product := Product{Name: "expenses", Columns: map[string]struct{}{"department": {}, "amount": {}, "id": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}}, ColumnTypes: map[string]string{"department": "text", "amount": "numeric", "id": "bigint"},
		SourceNamespace: "travel.expense", Snapshot: "s1", StableEntityKey: []string{"id"}}
	left := QueryPlan{Product: "expenses", Columns: []string{"department"},
		Aggregates: []Aggregate{{Function: "SUM", Column: "amount", Alias: "total"}},
		Filters:    []Filter{{Column: "amount", Op: ">", Value: float64(0)}, {Column: "department", Op: "=", Value: "sales"}},
		GroupBy:    []string{"department"}, OrderBy: []Order{{Column: "total", Direction: "desc"}}, Limit: 10}
	right := left
	right.Aggregates = []Aggregate{{Function: "sum", Column: "amount", Alias: "renamed"}}
	right.Filters = []Filter{left.Filters[1], left.Filters[0]}
	right.OrderBy = []Order{{Column: "renamed", Direction: "DESC"}}
	leftNF, err := NormalizeV2(left, product)
	if err != nil {
		t.Fatal(err)
	}
	rightNF, err := NormalizeV2(right, product)
	if err != nil {
		t.Fatal(err)
	}
	leftDigest, _ := leftNF.Digest()
	rightDigest, _ := rightNF.Digest()
	if leftDigest != rightDigest {
		t.Fatalf("restricted rewrites differ:\n%+v\n%+v", leftNF, rightNF)
	}
	if len(leftNF.OrderBy) != 2 || leftNF.OrderBy[1].Expression != "travel.expense.department" {
		t.Fatalf("paged group query lacks stable group-key tie-breaker: %+v", leftNF.OrderBy)
	}
	if len(leftNF.Columns) != 1 || leftNF.Columns[0] != "travel.expense.department" {
		t.Fatalf("projection field is not namespace-qualified: %+v", leftNF.Columns)
	}
}

func TestNormalFormV2DistinguishesCountStarAndCountExpression(t *testing.T) {
	product := Product{Name: "expenses", Columns: map[string]struct{}{"amount": {}}, AllowedAggregates: map[string]struct{}{"count": {}},
		ColumnTypes: map[string]string{"amount": "numeric"}, SourceNamespace: "travel.expense", Snapshot: "s1"}
	star, err := NormalizeV2(QueryPlan{Product: product.Name, Aggregates: []Aggregate{{Function: "count", Column: "*", Alias: "n"}}}, product)
	if err != nil {
		t.Fatal(err)
	}
	column, err := NormalizeV2(QueryPlan{Product: product.Name, Aggregates: []Aggregate{{Function: "count", Column: "amount", Alias: "n"}}}, product)
	if err != nil {
		t.Fatal(err)
	}
	starDigest, _ := star.Digest()
	columnDigest, _ := column.Digest()
	if starDigest == columnDigest || star.Aggregates[0].Expression != "count(*)" || column.Aggregates[0].Expression != "count(travel.expense.amount)" {
		t.Fatalf("COUNT forms collapsed: %+v / %+v", star.Aggregates, column.Aggregates)
	}
}

func TestCompileSupportsOffset(t *testing.T) {
	product := Product{Name: "expenses", Columns: map[string]struct{}{"id": {}}, AllowedAggregates: map[string]struct{}{}}
	sql, err := Compile(QueryPlan{Product: "expenses", Columns: []string{"id"}, OrderBy: []Order{{Column: "id"}}, Limit: 5, Offset: 10}, product)
	if err != nil {
		t.Fatal(err)
	}
	if sql != `SELECT "id" FROM "expenses" ORDER BY "id" ASC LIMIT 5 OFFSET 10` {
		t.Fatalf("SQL = %s", sql)
	}
}

func TestAlgebraNormalFormV2CanonicalizesJoinAndUnionOperands(t *testing.T) {
	left := AlgebraPlanV2{Op: "scan", SourceNamespace: "travel.employee", Snapshot: "s1", StableRole: "employee"}
	right := AlgebraPlanV2{Op: "scan", SourceNamespace: "travel.expense", Snapshot: "s1", StableRole: "expense"}
	first, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &left, Right: &right, JoinPredicates: []string{"employee.department=expense.department"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &right, Right: &left, JoinPredicates: []string{"employee.department=expense.department"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("join operand order changed the V2 normal form")
	}
	union, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "union", Left: &left, Right: &left})
	if err != nil {
		t.Fatal(err)
	}
	scan, _ := NormalizeAlgebraV2(left)
	if union.SHA256 != scan.SHA256 {
		t.Fatal("duplicate UNION branch was not idempotent")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "union", Left: &left, Right: &right, UnionAll: true}); err == nil {
		t.Fatal("UNION ALL unexpectedly entered the V2 normal form")
	}
}
