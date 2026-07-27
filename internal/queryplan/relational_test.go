package queryplan

import (
	"strings"
	"testing"
)

func TestCompileRelationalJoinProducesPairedRoleQualifiedSQL(t *testing.T) {
	products := relationalTestProducts()
	plan := QueryPlan{
		From: &From{Join: &Join{
			Left: Scan{Product: "detail", Role: "detail"}, Right: Scan{Product: "summary", Role: "summary"},
			On: []JoinPredicate{{Left: "detail.department", Right: "summary.department"}},
		}},
		Columns: []string{"detail.receipt_no", "summary.total"},
	}
	compiled, err := CompileRelational(plan, products)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`FROM "detail" AS "detail" INNER JOIN "summary" AS "summary"`,
		`"detail"."department" = "summary"."department"`,
		`AS "` + compiled.OutputAliases["detail.receipt_no"] + `"`, `ORDER BY "detail"."receipt_no" ASC`,
	} {
		if !strings.Contains(compiled.VisibleSQL, fragment) {
			t.Fatalf("visible SQL misses %q: %s", fragment, compiled.VisibleSQL)
		}
	}
	if strings.Contains(compiled.ProvenanceSQL, "GROUP BY") || !strings.Contains(compiled.ProvenanceSQL, `AS "tg_detail_receipt_no"`) {
		t.Fatalf("join provenance SQL is not a positive-row companion: %s", compiled.ProvenanceSQL)
	}
	if strings.Contains(compiled.VisibleSQL, `"detail"."amount"`) || strings.Contains(compiled.ProvenanceSQL, `"detail"."amount"`) {
		t.Fatalf("join companion fetched an unrelated approved field: visible=%s provenance=%s", compiled.VisibleSQL, compiled.ProvenanceSQL)
	}
}

func TestCompileRelationalUnionPreservesHiddenDistinctMembers(t *testing.T) {
	products := relationalTestProducts()
	plan := QueryPlan{
		From: &From{UnionDistinct: &UnionDistinct{
			Role: "summary", Columns: []string{"department", "month"},
			Left:  Scan{Product: "summary", Role: "left_branch", Filters: []Filter{{Column: "month", Op: "=", Value: "2026-01"}}},
			Right: Scan{Product: "summary", Role: "right_branch", Filters: []Filter{{Column: "month", Op: "=", Value: "2026-02"}}},
		}},
		Columns: []string{"summary.department"},
	}
	compiled, err := CompileRelational(plan, products)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compiled.VisibleSQL, " UNION ") || strings.Contains(compiled.VisibleSQL, "UNION ALL") {
		t.Fatalf("visible SQL is not UNION DISTINCT: %s", compiled.VisibleSQL)
	}
	if !strings.Contains(compiled.VisibleSQL, `AS "`+compiled.OutputAliases["summary.month"]+`"`) {
		t.Fatalf("hidden dedup field is absent from internal visible tuple: %s", compiled.VisibleSQL)
	}
	if !strings.Contains(compiled.ProvenanceSQL, " UNION ALL ") || !strings.Contains(compiled.ProvenanceSQL, `AS "tg_branch"`) {
		t.Fatalf("provenance did not retain every branch member: %s", compiled.ProvenanceSQL)
	}
	if !compiled.ExpandedEvidence {
		t.Fatal("union provenance was not marked as membership-expanded")
	}
}

func TestCompileRelationalRejectsAliasDefinedSemanticRoleAndUnionAllShape(t *testing.T) {
	products := relationalTestProducts()
	_, err := CompileRelational(QueryPlan{From: &From{Scan: &Scan{Product: "detail", Role: "arbitrary"}}, Columns: []string{"arbitrary.receipt_no"}}, products)
	if err == nil {
		t.Fatal("scan accepted a caller-defined semantic role")
	}
	_, err = CompileRelational(QueryPlan{From: &From{UnionDistinct: &UnionDistinct{Role: "summary", Columns: []string{"department"}, Left: Scan{Product: "summary", Role: "same"}, Right: Scan{Product: "summary", Role: "same"}}}, Columns: []string{"summary.department"}}, products)
	if err == nil {
		t.Fatal("union accepted indistinguishable branch aliases")
	}
}

func relationalTestProducts() map[string]Product {
	textCollation := map[string]string{"department": "C", "month": "C", "receipt_no": "C"}
	versions := map[string]string{"department": "builtin", "month": "builtin", "receipt_no": "builtin"}
	return map[string]Product{
		"detail":  {Name: "detail", StableRole: "detail", SourceNamespace: "travel.detail", Snapshot: "s1", StableEntityKey: []string{"receipt_no"}, Columns: map[string]struct{}{"receipt_no": {}, "department": {}, "amount": {}}, ColumnTypes: map[string]string{"receipt_no": "text", "department": "text", "amount": "numeric"}, ColumnCollations: textCollation, CollationVersions: versions, AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}}},
		"summary": {Name: "summary", StableRole: "summary", SourceNamespace: "travel.summary", Snapshot: "s1", StableEntityKey: []string{"month", "department"}, Columns: map[string]struct{}{"month": {}, "department": {}, "total": {}}, ColumnTypes: map[string]string{"month": "text", "department": "text", "total": "numeric"}, ColumnCollations: textCollation, CollationVersions: versions, AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}}},
	}
}
