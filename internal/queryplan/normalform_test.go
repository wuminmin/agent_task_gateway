package queryplan

import (
	"encoding/json"
	"testing"
)

func TestNormalFormV2ErasesAliasesAndSortsRestrictedRewrites(t *testing.T) {
	product := Product{Name: "expenses", Columns: map[string]struct{}{"department": {}, "amount": {}, "id": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}}, ColumnTypes: map[string]string{"department": "text", "amount": "numeric", "id": "bigint"},
		ColumnCollations: map[string]string{"department": "en_US.utf8"}, CollationVersions: map[string]string{"department": "2.36"},
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
	left := AlgebraPlanV2{Op: "scan", SourceNamespace: "travel.employee", Snapshot: "s1", StableRole: "employee",
		Schema: []AlgebraFieldV2{{ID: "employee.department", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}}}
	right := AlgebraPlanV2{Op: "scan", SourceNamespace: "travel.expense", Snapshot: "s1", StableRole: "expense",
		Schema: []AlgebraFieldV2{{ID: "expense.department", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}}}
	predicates := []AlgebraJoinPredicateV2{{LeftField: "employee.department", RightField: "expense.department"}}
	first, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &left, Right: &right, JoinPredicates: predicates})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &right, Right: &left,
		JoinPredicates: []AlgebraJoinPredicateV2{{LeftField: "expense.department", RightField: "employee.department"}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("join operand order changed the V2 normal form")
	}
	unionPlan := AlgebraPlanV2{Op: "union", Left: &left, Right: &left}
	union, err := NormalizeAlgebraV2(unionPlan)
	if err != nil {
		t.Fatal(err)
	}
	scan, _ := NormalizeAlgebraV2(left)
	if union.SHA256 == scan.SHA256 {
		t.Fatal("bag-valued scan was incorrectly assumed to be tuple-distinct")
	}
	setUnion, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "union", Left: &unionPlan, Right: &unionPlan})
	if err != nil {
		t.Fatal(err)
	}
	if setUnion.SHA256 != union.SHA256 {
		t.Fatal("duplicate set-valued UNION branch was not idempotent")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "union", Left: &left, Right: &right, UnionAll: true}); err == nil {
		t.Fatal("UNION ALL unexpectedly entered the V2 normal form")
	}
}

func TestAlgebraNormalFormV2IsTypedAndFailClosed(t *testing.T) {
	numeric := AlgebraPlanV2{Op: "scan", SourceNamespace: "n", Snapshot: "s1", StableRole: "n",
		Schema: []AlgebraFieldV2{{ID: "n.value", SQLType: "decimal"}}}
	text := AlgebraPlanV2{Op: "scan", SourceNamespace: "t", Snapshot: "s1", StableRole: "t",
		Schema: []AlgebraFieldV2{{ID: "t.value", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true}}}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &numeric, Right: &text,
		JoinPredicates: []AlgebraJoinPredicateV2{{LeftField: "n.value", RightField: "t.value"}}}); err == nil {
		t.Fatal("ill-typed join entered the algebra normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &numeric, Right: &numeric}); err == nil {
		t.Fatal("predicate-free equijoin entered the algebra normal form")
	}
	badCollation := AlgebraPlanV2{Op: "scan", SourceNamespace: "t", Snapshot: "s1", StableRole: "t",
		Schema: []AlgebraFieldV2{{ID: "t.value", SQLType: "text"}}}
	if _, err := NormalizeAlgebraV2(badCollation); err == nil {
		t.Fatal("unattested text collation entered the algebra normal form")
	}
	group, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "group", Input: &numeric,
		Aggregates: []AlgebraAggregateV2{{Function: "sum", Field: "n.value", OutputType: "numeric"}}})
	if err != nil || len(group.Canonical) == 0 {
		t.Fatalf("typed numeric group normal form: %v", err)
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "group", Input: &numeric,
		Aggregates: []AlgebraAggregateV2{{Function: "sum", Field: "n.value", OutputType: "bigint"}}}); err == nil {
		t.Fatal("aggregate with incorrect PostgreSQL output type entered the normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "select", Input: &numeric,
		Predicates: []NormalizedFilter{{Column: "n.value", Op: "IN", Value: json.RawMessage(`1`)}}}); err == nil {
		t.Fatal("scalar IN literal entered the algebra normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "select", Input: &numeric,
		Predicates: []NormalizedFilter{{Column: "n.value", Op: "LIKE", Value: json.RawMessage(`"1%"`)}}}); err == nil {
		t.Fatal("numeric LIKE entered the algebra normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "select", Input: &numeric,
		Predicates: []NormalizedFilter{{Column: "n.value", Op: "=", Value: json.RawMessage(`"not-a-number"`)}}}); err == nil {
		t.Fatal("mistyped numeric literal entered the algebra normal form")
	}
	jsonLeft := AlgebraPlanV2{Op: "scan", SourceNamespace: "jl", Snapshot: "s1", StableRole: "jl",
		Schema: []AlgebraFieldV2{{ID: "jl.value", SQLType: "json"}}}
	jsonRight := AlgebraPlanV2{Op: "scan", SourceNamespace: "jr", Snapshot: "s1", StableRole: "jr",
		Schema: []AlgebraFieldV2{{ID: "jr.value", SQLType: "json"}}}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "join", Left: &jsonLeft, Right: &jsonRight,
		JoinPredicates: []AlgebraJoinPredicateV2{{LeftField: "jl.value", RightField: "jr.value"}}}); err == nil {
		t.Fatal("PostgreSQL json equality entered the V2 join normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "group", Input: &jsonLeft, GroupBy: []string{"jl.value"}}); err == nil {
		t.Fatal("PostgreSQL json equality entered the V2 group normal form")
	}
	if _, err := NormalizeAlgebraV2(AlgebraPlanV2{Op: "union", Left: &jsonLeft, Right: &jsonLeft}); err == nil {
		t.Fatal("PostgreSQL json equality entered the V2 union normal form")
	}
}
