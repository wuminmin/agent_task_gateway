package gateway

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestJoinManyAlgebraIdentityIsStableAcrossInputOrder(t *testing.T) {
	products := joinManyCatalogProducts()
	approved := map[string][]string{
		"detail":  {"department", "receipt_no"},
		"region":  {"department", "region_code"},
		"summary": {"department", "month"},
	}
	queryProducts := make(map[string]queryplan.Product, len(products))
	for name, product := range products {
		queryProducts[name] = relationalQueryProduct(product, stringSetFromSlice(approved[name]))
	}
	first := queryplan.QueryPlan{From: &queryplan.From{JoinMany: &queryplan.JoinMany{
		Sources: []queryplan.Scan{{Product: "region", Role: "region"}, {Product: "detail", Role: "detail"}, {Product: "summary", Role: "summary"}},
		On: []queryplan.JoinPredicate{
			{Left: "region.department", Right: "summary.department"},
			{Left: "summary.department", Right: "detail.department"},
		},
	}}, Columns: []string{"detail.receipt_no", "region.region_code"}}
	second := queryplan.QueryPlan{From: &queryplan.From{JoinMany: &queryplan.JoinMany{
		Sources: []queryplan.Scan{{Product: "summary", Role: "summary"}, {Product: "detail", Role: "detail"}, {Product: "region", Role: "region"}},
		On: []queryplan.JoinPredicate{
			{Left: "detail.department", Right: "summary.department"},
			{Left: "summary.department", Right: "region.department"},
		},
	}}, Columns: append([]string(nil), first.Columns...)}

	left := joinManyNormalForm(t, first, queryProducts, products)
	right := joinManyNormalForm(t, second, queryProducts, products)
	if left.SHA256 != right.SHA256 || string(left.Canonical) != string(right.Canonical) {
		t.Fatalf("equivalent join_many algebra differs:\nleft=%s %s\nright=%s %s",
			left.SHA256, left.Canonical, right.SHA256, right.Canonical)
	}

	leftObservation, leftDigest := joinManyObservation(t, first, queryProducts, products, approved)
	rightObservation, rightDigest := joinManyObservation(t, second, queryProducts, products, approved)
	if leftDigest != rightDigest || !sameJoinManyFacts(t, leftObservation.Release, rightObservation.Release) ||
		!sameJoinManyFacts(t, leftObservation.Influence, rightObservation.Influence) {
		t.Fatalf("equivalent join_many observations differ: digest %s/%s release %d/%d influence %d/%d",
			leftDigest, rightDigest, len(leftObservation.Release), len(rightObservation.Release),
			len(leftObservation.Influence), len(rightObservation.Influence))
	}
}

func joinManyNormalForm(t *testing.T, plan queryplan.QueryPlan, queryProducts map[string]queryplan.Product, products map[string]catalog.Product) queryplan.AlgebraNormalFormV2 {
	t.Helper()
	compilation, err := queryplan.CompileRelational(plan, queryProducts)
	if err != nil {
		t.Fatal(err)
	}
	algebra, err := relationalAlgebraPlan(plan, compilation, products)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := queryplan.NormalizeAlgebraV2(algebra)
	if err != nil {
		t.Fatal(err)
	}
	return normal
}

func joinManyObservation(t *testing.T, plan queryplan.QueryPlan, queryProducts map[string]queryplan.Product, products map[string]catalog.Product, approved map[string][]string) (exposure.Observation, string) {
	t.Helper()
	compilation, err := queryplan.CompileRelational(plan, queryProducts)
	if err != nil {
		t.Fatal(err)
	}
	context, err := buildRelationalExposureContext(plan, compilation, products, approved)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"detail.department": "sales", "detail.receipt_no": "R-1",
		"region.department": "sales", "region.region_code": "PH",
		"summary.department": "sales", "summary.month": "2026-07",
	}
	visible := dataconnector.Result{RowCount: 1}
	visibleRow := make([]any, 0, len(compilation.InternalFields))
	for _, field := range compilation.InternalFields {
		visible.Columns = append(visible.Columns, dataconnector.Column{Name: compilation.OutputAliases[field]})
		visibleRow = append(visibleRow, values[field])
	}
	visible.Rows = [][]any{visibleRow}
	provenance := dataconnector.Result{RowCount: 1}
	provenanceRow := make([]any, 0, len(compilation.ProvenanceFields))
	byAlias := make(map[string]any)
	for _, source := range compilation.Sources {
		for _, field := range source.EvidenceFields {
			byAlias[source.EvidenceAlias[field]] = values[source.Role+"."+field]
		}
	}
	for _, field := range compilation.ProvenanceFields {
		provenance.Columns = append(provenance.Columns, dataconnector.Column{Name: field})
		provenanceRow = append(provenanceRow, byAlias[field])
	}
	provenance.Rows = [][]any{provenanceRow}
	observation, err := context.deriveRelationalObservationV2(visible, provenance)
	if err != nil {
		t.Fatal(err)
	}
	return observation, context.planDigest
}

func sameJoinManyFacts(t *testing.T, left, right []exposure.FactID) bool {
	t.Helper()
	leftSet, err := exposure.NewFactSet(left...)
	if err != nil {
		t.Fatal(err)
	}
	rightSet, err := exposure.NewFactSet(right...)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for hash := range leftSet {
		if _, present := rightSet[hash]; !present {
			return false
		}
	}
	return true
}

func joinManyCatalogProducts() map[string]catalog.Product {
	field := func(name string) catalog.Field {
		return catalog.Field{Name: name, Type: "text", Collation: "C", CollationVersion: "builtin"}
	}
	return map[string]catalog.Product{
		"detail": {
			Name: "detail", Sensitivity: domain.SensitivityLow, FactNamespace: "travel.detail", StableRelationRole: "detail",
			Snapshot: "s1", EntityKey: []string{"receipt_no"}, Fields: []catalog.Field{field("department"), field("receipt_no")},
		},
		"region": {
			Name: "region", Sensitivity: domain.SensitivityLow, FactNamespace: "travel.region", StableRelationRole: "region",
			Snapshot: "s1", EntityKey: []string{"region_code"}, Fields: []catalog.Field{field("department"), field("region_code")},
		},
		"summary": {
			Name: "summary", Sensitivity: domain.SensitivityLow, FactNamespace: "travel.summary", StableRelationRole: "summary",
			Snapshot: "s1", EntityKey: []string{"month"}, Fields: []catalog.Field{field("department"), field("month")},
		},
	}
}
