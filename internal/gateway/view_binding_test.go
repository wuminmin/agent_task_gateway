package gateway

import (
	"context"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

type registryFakeConnector struct {
	*fakeConnector
	snapshot viewcompiler.RegistrySnapshot
	err      error
}

func (connector *registryFakeConnector) DiscoverViewRegistry(_ context.Context, _ []viewcompiler.RelationName,
	_ map[string]string) (viewcompiler.RegistrySnapshot, error) {
	if connector.err != nil {
		return viewcompiler.RegistrySnapshot{}, connector.err
	}
	return connector.snapshot, nil
}

func TestResolveViewBindingCompilesNestedContractAndIncludesOpaqueTaskProducts(t *testing.T) {
	logical, err := catalog.Load("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	base, ok := logical.LookupProduct("expense_detail")
	if !ok {
		t.Fatal("missing base product")
	}
	root := viewcompiler.RelationName{Schema: "reporting", Name: "semantic_expense"}
	leaf := viewcompiler.RelationName{Schema: "reporting", Name: "expense_detail"}
	department, ok := catalogFieldByName(base.Fields, "department")
	if !ok {
		t.Fatal("missing base department field")
	}
	definition := `SELECT d.receipt_no, d.department, d.amount FROM reporting.expense_detail AS d`
	snapshot := viewcompiler.RegistrySnapshot{PostgreSQLMajorVersion: 16, RevisionDigest: strings.Repeat("9", 64),
		Relations: map[viewcompiler.RelationName]viewcompiler.Relation{
			leaf: {Name: leaf, Kind: viewcompiler.RelationBase, ProductName: base.Name, Columns: viewColumns(base.Fields)},
			root: {Name: root, Kind: viewcompiler.RelationView, DefinitionSQL: definition,
				DefinitionDigest: viewcompiler.ExactDefinitionDigest(definition), Dependencies: []viewcompiler.RelationName{leaf},
				Columns: []viewcompiler.Column{{Name: "receipt_no", SQLType: "text", Collation: base.Fields[0].Collation,
					CollationVersion: base.Fields[0].CollationVersion}, {Name: "department", SQLType: "text",
					Collation: department.Collation, CollationVersion: department.CollationVersion},
					{Name: "amount", SQLType: "numeric"}}},
		}}
	products := map[string]queryplan.Product{base.Name: relationalQueryProduct(base, stringSetFromSlice(base.FieldNames()))}
	compiler, err := viewcompiler.New(snapshot, products)
	if err != nil {
		t.Fatalf("new compiler: %v", err)
	}
	artifact, err := compiler.Compile(root)
	if err != nil {
		t.Fatalf("compile contract: %v", err)
	}
	semantic := catalog.Product{
		Name: "semantic_expense", Source: base.Source, ReportingView: root.String(),
		Description: "nested semantic product", Sensitivity: base.Sensitivity,
		Fields: []catalog.Field{{Name: "receipt_no", Type: "text", Collation: base.Fields[0].Collation,
			CollationVersion: base.Fields[0].CollationVersion, Description: "receipt"},
			{Name: "department", Type: "text", Collation: department.Collation,
				CollationVersion: department.CollationVersion, Description: "department"},
			{Name: "amount", Type: "numeric", Description: "amount"}},
		Scopes: []string{"department"}, AllowedAggregates: []string{"count", "sum", "min", "max"},
		Snapshot: base.Snapshot, EntityKey: []string{"receipt_no"},
		FactNamespace: "travel.semantic_expense", StableRelationRole: "semantic_expense",
		ViewContract: &catalog.ViewContract{ProfileVersion: catalog.ViewContractV1,
			DefinitionDigest: artifact.DefinitionDigest, DependencyDigest: artifact.DependencyDigest,
			CanonicalPlanDigest: artifact.CanonicalPlanDigest, InterfaceDigest: artifact.InterfaceDigest},
	}
	logical.Products = append(logical.Products, semantic)
	connector := &registryFakeConnector{fakeConnector: &fakeConnector{}, snapshot: snapshot}
	service := &Service{catalog: logical, connector: connector}

	resolved, err := service.resolveViewBinding(context.Background(), []string{semantic.Name, "expense_summary"})
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	set, err := resolved.pendingViewBinding.validate()
	if err != nil {
		t.Fatalf("validate pending binding: %v", err)
	}
	if len(set.Products) != 2 || set.Products[0].Product != "expense_summary" || set.Products[1].Product != semantic.Name {
		t.Fatalf("binding products = %+v", set.Products)
	}
	if resolved.Expectation.ExpectedRevisionDigest != snapshot.RevisionDigest || len(resolved.Dependencies) != 3 {
		t.Fatalf("unexpected execution/dependency evidence: %+v / %+v", resolved.Expectation, resolved.Dependencies)
	}
	if len(resolved.Expectation.BaseProducts) != 1 || resolved.Expectation.BaseProducts[leaf.String()] != base.Name {
		t.Fatalf("execution expectation retained unreachable base candidates: %+v", resolved.Expectation.BaseProducts)
	}
	if executable, present := resolved.Artifacts[semantic.Name]; !present ||
		executable.CanonicalPlanDigest != artifact.CanonicalPlanDigest {
		t.Fatalf("resolved binding omitted its private executable artifact: %+v", resolved.Artifacts)
	}

	drifted := snapshot
	drifted.Relations = make(map[viewcompiler.RelationName]viewcompiler.Relation, len(snapshot.Relations))
	for name, relation := range snapshot.Relations {
		drifted.Relations[name] = relation
	}
	changed := drifted.Relations[root]
	changed.DefinitionSQL += ` WHERE d.amount > 0`
	changed.DefinitionDigest = viewcompiler.ExactDefinitionDigest(changed.DefinitionSQL)
	drifted.Relations[root] = changed
	connector.snapshot = drifted
	if _, err := service.resolveViewBinding(context.Background(), []string{semantic.Name, "expense_summary"}); err == nil {
		t.Fatal("semantic drift was accepted")
	}
}

func viewColumns(fields []catalog.Field) []viewcompiler.Column {
	result := make([]viewcompiler.Column, 0, len(fields))
	for _, field := range fields {
		result = append(result, viewcompiler.Column{Name: field.Name, SQLType: field.Type,
			Collation: field.Collation, CollationVersion: field.CollationVersion})
	}
	return result
}
