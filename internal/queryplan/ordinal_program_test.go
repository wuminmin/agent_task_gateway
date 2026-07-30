package queryplan

import (
	"slices"
	"strings"
	"testing"
)

func TestCompileOrdinalBuildsGroupedStreamingProgram(t *testing.T) {
	product := ordinalTestProduct()
	plan := QueryPlan{
		Product: "expenses", Columns: []string{"department"},
		Aggregates: []Aggregate{{Function: "sum", Column: "amount", Alias: "total"}},
		Filters:    []Filter{{Column: "scope", Op: "=", Value: "Sales"}},
		GroupBy:    []string{"department"},
	}
	compiled, err := CompileOrdinal(plan, product)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compiled.VisibleSQL, "ORDER BY") {
		t.Fatalf("grouped provenance ordering changed visible SQL: %s", compiled.VisibleSQL)
	}
	if !strings.HasSuffix(compiled.ProvenanceSQL, ` ORDER BY "department" ASC, "id" ASC`) {
		t.Fatalf("grouped companion is not group/entity ordered: %s", compiled.ProvenanceSQL)
	}
	if !slices.Equal(compiled.ProvenanceFields, []string{"amount", "department", "id", "scope"}) {
		t.Fatalf("provenance fields = %#v", compiled.ProvenanceFields)
	}
	program := compiled.OrdinalProgram
	if len(program.Sources) != 1 || program.Sources[0].HandleAlias == "" || !program.Sources[0].HandleRequired {
		t.Fatalf("ordinal source handle contract = %#v", program.Sources)
	}
	if err := program.ValidateBoundSidecars(); err != nil {
		t.Fatalf("bound test sidecar rejected: %v", err)
	}
	if err := program.ValidateProvenanceFields(compiled.ProvenanceFields); err == nil {
		t.Fatal("entity-key evidence incorrectly satisfied the required row_handle contract")
	}
	boundFields := append([]string{program.Sources[0].HandleAlias}, compiled.ProvenanceFields...)
	if err := program.ValidateProvenanceFields(boundFields); err != nil {
		t.Fatalf("complete provenance mapping rejected: %v", err)
	}
	if len(program.Groups) != 1 || program.Groups[0].CanonicalExpression != "group(travel.expense.department)" {
		t.Fatalf("group program = %#v", program.Groups)
	}
	if len(program.Aggregates) != 1 || program.Aggregates[0].CanonicalExpression != "sum(travel.expense.amount)" || program.Aggregates[0].SQLType != "numeric" {
		t.Fatalf("aggregate program = %#v", program.Aggregates)
	}
	if !hasWitnessRule(program.WitnessRules, "outer_filter", "$row", "travel.expense.scope", 1, "add") ||
		!hasWitnessRule(program.WitnessRules, "group_cell", "department", "travel.expense.department", 1, "add") ||
		!hasWitnessRule(program.WitnessRules, "aggregate", "total", "travel.expense.amount", 1, "add") {
		t.Fatalf("witness program misses filter/group/aggregate rules: %#v", program.WitnessRules)
	}
}

func TestCompileRelationalGroupedJoinOrdersCompanionAndRecordsMultiplicity(t *testing.T) {
	products := relationalTestProducts()
	for name, product := range products {
		product.SnapshotPublication = "publication-" + name
		product.SidecarManifestDigest = strings.Repeat(name[:1], 64)
		products[name] = product
	}
	plan := QueryPlan{
		From: &From{Join: &Join{
			Left:  Scan{Product: "detail", Role: "detail", Filters: []Filter{{Column: "amount", Op: ">", Value: float64(0)}}},
			Right: Scan{Product: "summary", Role: "summary"},
			On: []JoinPredicate{
				{Left: "detail.department", Right: "summary.department"},
				{Left: "detail.department", Right: "summary.month"},
			},
		}},
		Columns: []string{"summary.month"},
		Aggregates: []Aggregate{
			{Function: "sum", Column: "detail.amount", Alias: "total"},
			{Function: "count", Column: "*", Alias: "rows"},
		},
		Filters: []Filter{{Column: "summary.total", Op: ">", Value: float64(0)}},
		GroupBy: []string{"summary.month"},
	}
	compiled, err := CompileRelational(plan, products)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compiled.VisibleSQL, "ORDER BY") {
		t.Fatalf("companion ordering changed grouped visible SQL: %s", compiled.VisibleSQL)
	}
	wantOrder := ` ORDER BY "summary"."month" ASC, "detail"."receipt_no" ASC, "summary"."department" ASC`
	if !strings.HasSuffix(compiled.ProvenanceSQL, wantOrder) {
		t.Fatalf("grouped join companion order = %s", compiled.ProvenanceSQL)
	}
	program := compiled.OrdinalProgram
	if len(program.SnapshotBundle) != 2 || len(program.Joins) != 2 || len(program.Aggregates) != 2 {
		t.Fatalf("relational ordinal program is incomplete: %#v", program)
	}
	var detail OrdinalSource
	for _, source := range program.Sources {
		if source.SourceAlias == "detail" {
			detail = source
		}
	}
	if len(detail.JoinKeyFields) != 1 || detail.JoinKeyFields[0].FieldID != "detail.department" || detail.JoinKeyFields[0].Multiplicity != 2 {
		t.Fatalf("join-key witness multiplicity = %#v", detail.JoinKeyFields)
	}
	joinRules := 0
	for _, rule := range program.WitnessRules {
		if rule.Stage == "join" && rule.InputExpression == "travel.detail.detail.department" {
			joinRules += int(rule.Multiplicity)
		}
	}
	if joinRules != 2 {
		t.Fatalf("join rules contribute %d copies, want 2: %#v", joinRules, program.WitnessRules)
	}
	if !hasWitnessRule(program.WitnessRules, "leaf_filter", "$row", "travel.detail.detail.amount", 1, "add") ||
		!hasWitnessRule(program.WitnessRules, "outer_filter", "$row", "travel.summary.summary.total", 1, "add") ||
		!hasWitnessRule(program.WitnessRules, "aggregate", "rows", "$row", 1, "add") {
		t.Fatalf("relational witness stages are incomplete: %#v", program.WitnessRules)
	}
}

func TestCompileRelationalGroupedUnionOrdersByAliases(t *testing.T) {
	products := relationalTestProducts()
	plan := QueryPlan{
		From: &From{UnionDistinct: &UnionDistinct{
			Role: "summary", Columns: []string{"department", "month"},
			Left:  Scan{Product: "summary", Role: "left_branch", Filters: []Filter{{Column: "month", Op: "=", Value: "2026-01"}}},
			Right: Scan{Product: "summary", Role: "right_branch", Filters: []Filter{{Column: "month", Op: "=", Value: "2026-02"}}},
		}},
		Columns:    []string{"summary.department"},
		Aggregates: []Aggregate{{Function: "count", Column: "*", Alias: "rows"}},
		GroupBy:    []string{"summary.department"},
	}
	compiled, err := CompileRelational(plan, products)
	if err != nil {
		t.Fatal(err)
	}
	want := ` ORDER BY "tg_summary_department" ASC, "tg_summary_month" ASC, "tg_branch" ASC`
	if !strings.HasSuffix(compiled.ProvenanceSQL, want) {
		t.Fatalf("grouped union companion order = %s", compiled.ProvenanceSQL)
	}
	if len(compiled.OrdinalProgram.Sources) != 2 || compiled.OrdinalProgram.Sources[0].Branch == compiled.OrdinalProgram.Sources[1].Branch {
		t.Fatalf("union branch mappings = %#v", compiled.OrdinalProgram.Sources)
	}
	maxRules := 0
	for _, rule := range compiled.OrdinalProgram.WitnessRules {
		if rule.Merge == "max" {
			maxRules++
		}
	}
	if maxRules == 0 {
		t.Fatal("UNION DISTINCT program omitted alternative-proof max rules")
	}
}

func TestOrdinalProgramDigestNormalizesSetLikeMetadata(t *testing.T) {
	compiled, err := CompileOrdinal(QueryPlan{Product: "expenses", Columns: []string{"department"}}, ordinalTestProduct())
	if err != nil {
		t.Fatal(err)
	}
	left := compiled.OrdinalProgram
	right := compiled.OrdinalProgram
	right.Sources = append([]OrdinalSource(nil), right.Sources...)
	right.Sources[0].EvidenceFields = append([]OrdinalFieldBinding(nil), right.Sources[0].EvidenceFields...)
	right.CanonicalExpressions = append([]string(nil), right.CanonicalExpressions...)
	slices.Reverse(right.Sources[0].EvidenceFields)
	slices.Reverse(right.CanonicalExpressions)
	leftDigest, err := left.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := right.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("canonical digest depends on set-like slice order: %s != %s", leftDigest, rightDigest)
	}
}

func TestOrdinalProgramRejectsBranchAndSourceLocalBindingMutations(t *testing.T) {
	scan, err := CompileOrdinal(QueryPlan{Product: "expenses", Columns: []string{"department"}}, ordinalTestProduct())
	if err != nil {
		t.Fatal(err)
	}
	badScan := scan.OrdinalProgram
	badScan.Sources = append([]OrdinalSource(nil), badScan.Sources...)
	badScan.Sources[0].Branch = 0
	if err := badScan.ValidateBoundSidecars(); err == nil {
		t.Fatal("scan program accepted a UNION branch identity")
	}

	products := relationalTestProducts()
	for name, product := range products {
		product.SnapshotPublication = "publication-" + name
		product.SidecarManifestDigest = strings.Repeat(name[:1], 64)
		products[name] = product
	}
	join, err := CompileRelational(QueryPlan{From: &From{Join: &Join{
		Left: Scan{Product: "detail", Role: "detail"}, Right: Scan{Product: "summary", Role: "summary"},
		On: []JoinPredicate{{Left: "detail.department", Right: "summary.department"}},
	}}, Columns: []string{"detail.amount"}}, products)
	if err != nil {
		t.Fatal(err)
	}
	badJoin := join.OrdinalProgram
	badJoin.Sources = append([]OrdinalSource(nil), badJoin.Sources...)
	if len(badJoin.Sources) != 2 || len(badJoin.Sources[1].EvidenceFields) == 0 {
		t.Fatalf("unexpected join sources: %#v", badJoin.Sources)
	}
	badJoin.Sources[0].JoinKeyFields = []OrdinalFieldUse{fieldUse(badJoin.Sources[1].EvidenceFields[0], 1)}
	if err := badJoin.ValidateBoundSidecars(); err == nil {
		t.Fatal("join source accepted another source's evidence binding")
	}

	duplicateSourceAlias := join.OrdinalProgram
	duplicateSourceAlias.Sources = append([]OrdinalSource(nil), duplicateSourceAlias.Sources...)
	duplicateSourceAlias.Sources[1].SourceAlias = duplicateSourceAlias.Sources[0].SourceAlias
	if err := duplicateSourceAlias.ValidateBoundSidecars(); err == nil {
		t.Fatal("join program accepted duplicate global source aliases")
	}

	duplicateHandleAlias := join.OrdinalProgram
	duplicateHandleAlias.Sources = append([]OrdinalSource(nil), duplicateHandleAlias.Sources...)
	duplicateHandleAlias.Sources[1].HandleAlias = duplicateHandleAlias.Sources[0].HandleAlias
	if err := duplicateHandleAlias.ValidateBoundSidecars(); err == nil {
		t.Fatal("join program accepted duplicate global provenance aliases")
	}

	duplicateEvidenceAlias := join.OrdinalProgram
	duplicateEvidenceAlias.Sources = append([]OrdinalSource(nil), duplicateEvidenceAlias.Sources...)
	duplicateEvidenceAlias.Sources[1].EvidenceFields = append([]OrdinalFieldBinding(nil), duplicateEvidenceAlias.Sources[1].EvidenceFields...)
	duplicateEvidenceAlias.Sources[1].EvidenceFields[0].ProvenanceAlias = duplicateEvidenceAlias.Sources[0].EvidenceFields[0].ProvenanceAlias
	if err := duplicateEvidenceAlias.ValidateBoundSidecars(); err == nil {
		t.Fatal("join program accepted duplicate global evidence aliases")
	}

	unionPlan := QueryPlan{From: &From{UnionDistinct: &UnionDistinct{
		Role: "summary", Columns: []string{"department", "month"},
		Left: Scan{Product: "summary", Role: "left_branch"}, Right: Scan{Product: "summary", Role: "right_branch"},
	}}, Columns: []string{"summary.department"}}
	union, err := CompileRelational(unionPlan, products)
	if err != nil {
		t.Fatal(err)
	}
	badUnion := union.OrdinalProgram
	badUnion.Sources = append([]OrdinalSource(nil), badUnion.Sources...)
	badUnion.Sources[1].Branch = badUnion.Sources[0].Branch
	if err := badUnion.ValidateBoundSidecars(); err == nil {
		t.Fatal("UNION DISTINCT accepted duplicate branch identities")
	}

	badSharedUnionAlias := union.OrdinalProgram
	badSharedUnionAlias.Sources = append([]OrdinalSource(nil), badSharedUnionAlias.Sources...)
	badSharedUnionAlias.Sources[1].EvidenceFields = append([]OrdinalFieldBinding(nil), badSharedUnionAlias.Sources[1].EvidenceFields...)
	badSharedUnionAlias.Sources[1].EvidenceFields[0].CanonicalExpression += ".mutated"
	if err := badSharedUnionAlias.ValidateBoundSidecars(); err == nil {
		t.Fatal("UNION DISTINCT accepted a shared alias with conflicting semantic bindings")
	}
}

func ordinalTestProduct() Product {
	return Product{
		Name: "expenses", Columns: map[string]struct{}{"department": {}, "amount": {}, "scope": {}},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
		ColumnTypes:       map[string]string{"id": "bigint", "department": "text", "amount": "numeric", "scope": "text"},
		ColumnCollations:  map[string]string{"department": "C", "scope": "C"},
		CollationVersions: map[string]string{"department": "builtin", "scope": "builtin"},
		SourceNamespace:   "travel.expense", Snapshot: "s1", StableRole: "expense", StableEntityKey: []string{"id"},
		RequiredEvidence: []string{"scope"}, LineageDigest: strings.Repeat("a", 64),
		SnapshotPublication: "expense-publication-v1", SidecarManifestDigest: strings.Repeat("b", 64),
	}
}

func hasWitnessRule(rules []OrdinalWitnessRule, stage, target, input string, multiplicity uint64, merge string) bool {
	for _, rule := range rules {
		if rule.Stage == stage && rule.TargetID == target && rule.InputExpression == input && rule.Multiplicity == multiplicity && rule.Merge == merge {
			return true
		}
	}
	return false
}
