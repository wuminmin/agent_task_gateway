package dataconnector

import (
	"errors"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

func TestViewRegistryRevisionIsOrderInvariantAndDefinitionSensitive(t *testing.T) {
	base := viewcompiler.RelationName{Schema: "reporting", Name: "orders_snapshot"}
	child := viewcompiler.RelationName{Schema: "reporting", Name: "monthly_orders"}
	definition := "SELECT id, amount FROM reporting.orders_snapshot WHERE label = 'a  b'::text"
	relations := map[viewcompiler.RelationName]viewcompiler.Relation{
		child: {
			Name: child, Kind: viewcompiler.RelationView, DefinitionSQL: definition,
			DefinitionDigest: viewcompiler.ExactDefinitionDigest(definition),
			Columns:          []viewcompiler.Column{{Name: "id", SQLType: "bigint"}, {Name: "amount", SQLType: "numeric"}},
			Dependencies:     []viewcompiler.RelationName{base},
		},
		base: {
			Name: base, Kind: viewcompiler.RelationBase, ProductName: "orders",
			DefinitionSQL:    "SELECT id, amount, label FROM legacy.orders",
			DefinitionDigest: viewcompiler.ExactDefinitionDigest("SELECT id, amount, label FROM legacy.orders"),
			Columns:          []viewcompiler.Column{{Name: "id", SQLType: "bigint"}, {Name: "amount", SQLType: "numeric"}, {Name: "label", SQLType: "text", Collation: "C", CollationVersion: "1"}},
		},
	}
	left, err := viewRegistryRevision(16, relations)
	if err != nil {
		t.Fatal(err)
	}
	reordered := map[viewcompiler.RelationName]viewcompiler.Relation{base: relations[base], child: relations[child]}
	right, err := viewRegistryRevision(16, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("registry revision depends on map order: %s != %s", left, right)
	}
	changed := cloneRegistryRelations(relations)
	changedChild := changed[child]
	changedChild.DefinitionSQL = strings.ReplaceAll(definition, "a  b", "a b")
	changedChild.DefinitionDigest = viewcompiler.ExactDefinitionDigest(changedChild.DefinitionSQL)
	changed[child] = changedChild
	changedDigest, err := viewRegistryRevision(16, changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == left {
		t.Fatal("registry revision ignored significant definition-literal whitespace")
	}
}

func TestViewRegistryExpectationRequiresExactPairBinding(t *testing.T) {
	digest := strings.Repeat("a", 64)
	root := viewcompiler.RelationName{Schema: "reporting", Name: "customer_value"}
	left := &ViewRegistryExpectation{
		Roots: []viewcompiler.RelationName{root},
		BaseProducts: map[string]string{
			"reporting.orders_snapshot": "orders",
		},
		ExpectedRevisionDigest: digest,
	}
	right := &ViewRegistryExpectation{
		Roots: []viewcompiler.RelationName{root},
		BaseProducts: map[string]string{
			"reporting.orders_snapshot": "orders",
		},
		ExpectedRevisionDigest: digest,
	}
	if _, err := matchingViewRegistryExpectations(left, right); err != nil {
		t.Fatalf("equivalent pair expectation rejected: %v", err)
	}
	right.ExpectedRevisionDigest = strings.Repeat("b", 64)
	if _, err := matchingViewRegistryExpectations(left, right); !IsCode(err, CodeViewSemanticChanged) {
		t.Fatalf("mismatched pair expectation error = %v", err)
	}
	if _, err := matchingViewRegistryExpectations(left, nil); !IsCode(err, CodeViewSemanticChanged) {
		t.Fatalf("one-sided pair expectation error = %v", err)
	}
}

func TestViewSemanticErrorRedactsPhysicalDetails(t *testing.T) {
	err := viewSemanticError(errors.New("reporting.secret_payroll definition changed"))
	if !IsCode(err, CodeViewSemanticChanged) {
		t.Fatalf("view semantic error code = %v", err)
	}
	if strings.Contains(err.Error(), "secret_payroll") || strings.Contains(err.Error(), "definition") {
		t.Fatalf("public view semantic error leaked its cause: %q", err.Error())
	}
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "secret_payroll") {
		t.Fatalf("internal diagnostic cause was not retained: %v", cause)
	}
}

func TestNormalizeViewRegistryExpectationRejectsUnsafeNamesAndDigests(t *testing.T) {
	valid := ViewRegistryExpectation{
		Roots: []viewcompiler.RelationName{{Schema: "reporting", Name: "customer_value"}},
		BaseProducts: map[string]string{
			"reporting.orders_snapshot": "orders",
		},
		ExpectedRevisionDigest: strings.Repeat("a", 64),
	}
	if _, err := normalizeViewRegistryExpectation(valid, true); err != nil {
		t.Fatalf("valid expectation rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ViewRegistryExpectation){
		"system root": func(value *ViewRegistryExpectation) {
			value.Roots[0].Schema = "pg_catalog"
		},
		"qualified injection": func(value *ViewRegistryExpectation) {
			value.Roots[0].Name = `value";DROP TABLE x;--`
		},
		"raw base key": func(value *ViewRegistryExpectation) {
			value.BaseProducts = map[string]string{"reporting.orders.extra": "orders"}
		},
		"invalid digest": func(value *ViewRegistryExpectation) {
			value.ExpectedRevisionDigest = "not-a-digest"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := ViewRegistryExpectation{
				Roots:        append([]viewcompiler.RelationName(nil), valid.Roots...),
				BaseProducts: cloneStringMap(valid.BaseProducts), ExpectedRevisionDigest: valid.ExpectedRevisionDigest,
			}
			mutate(&candidate)
			if _, err := normalizeViewRegistryExpectation(candidate, true); err == nil {
				t.Fatal("unsafe expectation was accepted")
			}
		})
	}
}

func TestInspectPostgreSQLViewDefinitionSupplementsCatalogDependencies(t *testing.T) {
	owner := viewcompiler.RelationName{Schema: "reporting", Name: "combined_orders"}
	inspection, err := viewcompiler.InspectDefinition(owner, `
SELECT a.id, b.amount
FROM reporting.order_keys AS a
INNER JOIN reporting.order_amounts AS b ON a.id = b.id`)
	if err != nil {
		t.Fatalf("inspect join definition: %v", err)
	}
	want := []viewcompiler.RelationName{
		{Schema: "reporting", Name: "order_amounts"},
		{Schema: "reporting", Name: "order_keys"},
	}
	if inspection.RecursiveSelf || !sameRelationNames(inspection.References, want) {
		t.Fatalf("join inspection = %+v, recursive=%t; want %+v", inspection.References, inspection.RecursiveSelf, want)
	}

	if _, err := viewcompiler.InspectDefinition(owner, `
SELECT id, clock_timestamp() AS observed_at
FROM reporting.order_keys`); err == nil {
		t.Fatal("pinned volatile built-in escaped AST validation")
	}

	inspection, err = viewcompiler.InspectDefinition(owner, `
WITH RECURSIVE combined_orders(id) AS (
  VALUES (1)
  UNION ALL SELECT id + 1 FROM combined_orders WHERE id < 2
)
SELECT id FROM combined_orders`)
	if err != nil {
		t.Fatalf("inspect recursive definition: %v", err)
	}
	if !inspection.RecursiveSelf || len(inspection.References) != 1 || inspection.References[0] != owner {
		t.Fatalf("recursive inspection = %+v, recursive=%t", inspection.References, inspection.RecursiveSelf)
	}
}

func cloneRegistryRelations(source map[viewcompiler.RelationName]viewcompiler.Relation) map[viewcompiler.RelationName]viewcompiler.Relation {
	result := make(map[viewcompiler.RelationName]viewcompiler.Relation, len(source))
	for name, relation := range source {
		relation.Columns = append([]viewcompiler.Column(nil), relation.Columns...)
		relation.Dependencies = append([]viewcompiler.RelationName(nil), relation.Dependencies...)
		result[name] = relation
	}
	return result
}
