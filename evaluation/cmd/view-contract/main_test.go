package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

type fakeViewDiscoverer struct {
	snapshot viewcompiler.RegistrySnapshot
	err      error
	calls    int
	roots    [][]viewcompiler.RelationName
	baseMaps []map[string]string
}

func (fake *fakeViewDiscoverer) DiscoverViewRegistry(_ context.Context, roots []viewcompiler.RelationName,
	baseMap map[string]string) (viewcompiler.RegistrySnapshot, error) {
	fake.calls++
	fake.roots = append(fake.roots, append([]viewcompiler.RelationName(nil), roots...))
	clonedBaseMap := make(map[string]string, len(baseMap))
	for relation, product := range baseMap {
		clonedBaseMap[relation] = product
	}
	fake.baseMaps = append(fake.baseMaps, clonedBaseMap)
	if fake.err != nil {
		return viewcompiler.RegistrySnapshot{}, fake.err
	}
	return fake.snapshot, nil
}

func TestSelectSemanticProductsExplicitSelectionIsSortedAndSingleSource(t *testing.T) {
	logical := &catalog.Catalog{
		Sources: []catalog.Source{{Name: "primary"}, {Name: "secondary"}},
		Products: []catalog.Product{
			semanticProduct("z_report", "primary", "semantic.z_report", nil),
			semanticProduct("a_report", "primary", "semantic.a_report", nil),
			semanticProduct("foreign_report", "secondary", "semantic.foreign_report", nil),
		},
	}

	selected, source, err := selectSemanticProducts(logical, " z_report, a_report ")
	if err != nil {
		t.Fatal(err)
	}
	if got := productNames(selected); !reflect.DeepEqual(got, []string{"a_report", "z_report"}) {
		t.Fatalf("selected products = %v", got)
	}
	if source.Name != "primary" {
		t.Fatalf("source = %q, want primary", source.Name)
	}

	if _, _, err := selectSemanticProducts(logical, "a_report,foreign_report"); err == nil ||
		!strings.Contains(err.Error(), "multiple sources") {
		t.Fatalf("cross-source selection error = %v", err)
	}
}

func TestSelectSemanticProductsDefaultsToExistingContracts(t *testing.T) {
	logical := &catalog.Catalog{
		Sources: []catalog.Source{{Name: "primary"}},
		Products: []catalog.Product{
			semanticProduct("new_candidate", "primary", "semantic.new_candidate", nil),
			semanticProduct("z_existing", "primary", "semantic.z_existing", oldContract()),
			semanticProduct("a_existing", "primary", "semantic.a_existing", oldContract()),
		},
	}

	selected, source, err := selectSemanticProducts(logical, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := productNames(selected); !reflect.DeepEqual(got, []string{"a_existing", "z_existing"}) {
		t.Fatalf("default selection = %v", got)
	}
	if source.Name != "primary" {
		t.Fatalf("source = %q, want primary", source.Name)
	}
}

func TestSelectSemanticProductsRejectsInvalidSelections(t *testing.T) {
	base := &catalog.Catalog{
		Sources: []catalog.Source{{Name: "primary"}},
		Products: []catalog.Product{
			semanticProduct("report", "primary", "semantic.report", nil),
		},
	}
	tests := []struct {
		name     string
		logical  *catalog.Catalog
		supplied string
		contains string
	}{
		{name: "nil catalog", logical: nil, supplied: "report", contains: "catalog is nil"},
		{name: "empty component", logical: base, supplied: "report,", contains: "empty name"},
		{name: "duplicate", logical: base, supplied: "report, report", contains: "repeated"},
		{name: "absent", logical: base, supplied: "missing", contains: "absent"},
		{name: "no default contracts", logical: base, contains: "no semantic products"},
		{name: "unknown source", logical: &catalog.Catalog{Products: []catalog.Product{
			semanticProduct("report", "missing", "semantic.report", nil),
		}}, supplied: "report", contains: "has no source"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := selectSemanticProducts(test.logical, test.supplied)
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want text %q", err, test.contains)
			}
		})
	}
}

func TestGenerateContractsSupportsFirstTimeExplicitGeneration(t *testing.T) {
	logical, selected, snapshot := oneViewFixture(nil)
	fake := &fakeViewDiscoverer{snapshot: snapshot}

	output, err := generateContracts(context.Background(), logical, fake, []catalog.Product{selected})
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", fake.calls)
	}
	if want := []viewcompiler.RelationName{{Schema: "semantic", Name: "report"}}; !reflect.DeepEqual(fake.roots[0], want) {
		t.Fatalf("discovery roots = %#v, want %#v", fake.roots[0], want)
	}
	if want := map[string]string{"raw.a": "base_a"}; !reflect.DeepEqual(fake.baseMaps[0], want) {
		t.Fatalf("base map = %#v, want %#v", fake.baseMaps[0], want)
	}
	if output.Version != catalog.ViewContractV1 || output.RegistryRevisionDigest != snapshot.RevisionDigest {
		t.Fatalf("output identity = %#v", output)
	}
	contract, present := output.Contracts[selected.Name]
	if !present {
		t.Fatalf("contract missing from %#v", output.Contracts)
	}
	if contract.ProfileVersion != catalog.ViewContractV1 ||
		contract.DefinitionDigest != snapshot.Relations[viewName("semantic", "report")].DefinitionDigest {
		t.Fatalf("generated contract = %#v", contract)
	}
	for name, digest := range map[string]string{
		"definition": contract.DefinitionDigest, "dependency": contract.DependencyDigest,
		"plan": contract.CanonicalPlanDigest, "interface": contract.InterfaceDigest,
	} {
		if len(digest) != 64 {
			t.Errorf("%s digest length = %d, want 64", name, len(digest))
		}
	}
	if want := []string{"raw.a", "semantic.child", "semantic.report"}; !reflect.DeepEqual(output.Dependencies[selected.Name], want) {
		t.Fatalf("dependencies = %v, want %v", output.Dependencies[selected.Name], want)
	}
}

func TestGenerateContractsDefaultSelectionRegeneratesExistingContract(t *testing.T) {
	logical, _, snapshot := oneViewFixture(oldContract())
	selected, _, err := selectSemanticProducts(logical, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := productNames(selected); !reflect.DeepEqual(got, []string{"semantic_report"}) {
		t.Fatalf("default selected products = %v", got)
	}

	output, err := generateContracts(context.Background(), logical, &fakeViewDiscoverer{snapshot: snapshot}, selected)
	if err != nil {
		t.Fatal(err)
	}
	generated := output.Contracts["semantic_report"]
	if generated.DefinitionDigest == strings.Repeat("a", 64) ||
		generated.DefinitionDigest != snapshot.Relations[viewName("semantic", "report")].DefinitionDigest {
		t.Fatalf("existing contract was not regenerated from the snapshot: %#v", generated)
	}
}

func TestGenerateContractsRejectsCatalogViewInterfaceMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*catalog.Product)
	}{
		{name: "field count", mutate: func(product *catalog.Product) {
			product.Fields = product.Fields[:1]
		}},
		{name: "field name", mutate: func(product *catalog.Product) {
			product.Fields[1].Name = "amount"
		}},
		{name: "field type", mutate: func(product *catalog.Product) {
			product.Fields[1].Type = "bigint"
		}},
		{name: "collation", mutate: func(product *catalog.Product) {
			product.Fields[1].Collation = "C"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logical, selected, snapshot := oneViewFixture(nil)
			test.mutate(&selected)
			for index := range logical.Products {
				if logical.Products[index].Name == selected.Name {
					logical.Products[index] = selected
				}
			}
			_, err := generateContracts(context.Background(), logical,
				&fakeViewDiscoverer{snapshot: snapshot}, []catalog.Product{selected})
			if err == nil || !strings.Contains(err.Error(), "disagrees with its View") {
				t.Fatalf("interface mismatch error = %v", err)
			}
		})
	}
}

func TestGenerateContractsIsDeterministicAcrossSelectedRootOrder(t *testing.T) {
	logical, snapshot, alpha, zeta := twoViewFixture()
	first, err := generateContracts(context.Background(), logical,
		&fakeViewDiscoverer{snapshot: snapshot}, []catalog.Product{zeta, alpha})
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateContracts(context.Background(), logical,
		&fakeViewDiscoverer{snapshot: snapshot}, []catalog.Product{alpha, zeta})
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("root order changed output:\nfirst  %s\nsecond %s", firstJSON, secondJSON)
	}
	if want := []string{"raw.a", "semantic.alpha"}; !reflect.DeepEqual(first.Dependencies[alpha.Name], want) {
		t.Fatalf("alpha dependencies = %v, want %v", first.Dependencies[alpha.Name], want)
	}
	if want := []string{"raw.b", "semantic.zeta"}; !reflect.DeepEqual(first.Dependencies[zeta.Name], want) {
		t.Fatalf("zeta dependencies = %v, want %v", first.Dependencies[zeta.Name], want)
	}
}

func TestGenerateContractsValidatesInputsAndPropagatesDiscoveryFailure(t *testing.T) {
	logical, selected, _ := oneViewFixture(nil)
	if _, err := generateContracts(context.Background(), nil, &fakeViewDiscoverer{}, []catalog.Product{selected}); err == nil {
		t.Fatal("nil catalog was accepted")
	}
	if _, err := generateContracts(context.Background(), logical, nil, []catalog.Product{selected}); err == nil {
		t.Fatal("nil discoverer was accepted")
	}
	if _, err := generateContracts(context.Background(), logical, &fakeViewDiscoverer{}, nil); err == nil {
		t.Fatal("empty selection was accepted")
	}
	want := errors.New("discovery failed")
	if _, err := generateContracts(context.Background(), logical, &fakeViewDiscoverer{err: want},
		[]catalog.Product{selected}); !errors.Is(err, want) {
		t.Fatalf("discovery error = %v, want %v", err, want)
	}
}

func oneViewFixture(contract *catalog.ViewContract) (*catalog.Catalog, catalog.Product, viewcompiler.RegistrySnapshot) {
	base := terminalProduct("base_a", "primary", "raw.a", "base_a")
	selected := semanticProduct("semantic_report", "primary", "semantic.report", contract)
	childSQL := `SELECT a.id, a.value FROM raw.a AS a`
	rootSQL := `SELECT child.id, child.value FROM semantic.child AS child`
	baseName := viewName("raw", "a")
	childName := viewName("semantic", "child")
	rootName := viewName("semantic", "report")
	snapshot := viewcompiler.RegistrySnapshot{
		PostgreSQLMajorVersion: 16,
		RevisionDigest:         strings.Repeat("f", 64),
		Relations: map[viewcompiler.RelationName]viewcompiler.Relation{
			baseName: {
				Name: baseName, Kind: viewcompiler.RelationBase, ProductName: base.Name,
				Columns: integerColumns("id", "value"),
			},
			childName: testRegistryView(childName, childSQL, integerColumns("id", "value"), baseName),
			rootName:  testRegistryView(rootName, rootSQL, integerColumns("id", "value"), childName),
		},
	}
	return &catalog.Catalog{
		Sources:  []catalog.Source{{Name: "primary"}},
		Products: []catalog.Product{selected, base},
	}, selected, snapshot
}

func twoViewFixture() (*catalog.Catalog, viewcompiler.RegistrySnapshot, catalog.Product, catalog.Product) {
	baseA := terminalProduct("base_a", "primary", "raw.a", "base_a")
	baseB := terminalProduct("base_b", "primary", "raw.b", "base_b")
	alpha := semanticProduct("alpha_report", "primary", "semantic.alpha", nil)
	zeta := semanticProduct("zeta_report", "primary", "semantic.zeta", nil)
	alphaSQL := `SELECT a.id, a.value FROM raw.a AS a`
	zetaSQL := `SELECT b.id, b.value FROM raw.b AS b`
	baseAName, baseBName := viewName("raw", "a"), viewName("raw", "b")
	alphaName, zetaName := viewName("semantic", "alpha"), viewName("semantic", "zeta")
	snapshot := viewcompiler.RegistrySnapshot{
		PostgreSQLMajorVersion: 16,
		RevisionDigest:         strings.Repeat("e", 64),
		Relations: map[viewcompiler.RelationName]viewcompiler.Relation{
			baseAName: {
				Name: baseAName, Kind: viewcompiler.RelationBase, ProductName: baseA.Name,
				Columns: integerColumns("id", "value"),
			},
			baseBName: {
				Name: baseBName, Kind: viewcompiler.RelationBase, ProductName: baseB.Name,
				Columns: integerColumns("id", "value"),
			},
			alphaName: testRegistryView(alphaName, alphaSQL, integerColumns("id", "value"), baseAName),
			zetaName:  testRegistryView(zetaName, zetaSQL, integerColumns("id", "value"), baseBName),
		},
	}
	return &catalog.Catalog{
		Sources:  []catalog.Source{{Name: "primary"}},
		Products: []catalog.Product{zeta, baseB, alpha, baseA},
	}, snapshot, alpha, zeta
}

func terminalProduct(name, source, reportingView, role string) catalog.Product {
	return catalog.Product{
		Name: name, Source: source, ReportingView: reportingView,
		Fields:   []catalog.Field{{Name: "id", Type: "integer"}, {Name: "value", Type: "integer"}},
		Snapshot: "snapshot-1", EntityKey: []string{"id"}, FactNamespace: "test." + name,
		StableRelationRole: role,
	}
}

func semanticProduct(name, source, reportingView string, contract *catalog.ViewContract) catalog.Product {
	product := terminalProduct(name, source, reportingView, name)
	product.ViewContract = contract
	return product
}

func oldContract() *catalog.ViewContract {
	digest := strings.Repeat("a", 64)
	return &catalog.ViewContract{
		ProfileVersion: catalog.ViewContractV1, DefinitionDigest: digest, DependencyDigest: digest,
		CanonicalPlanDigest: digest, InterfaceDigest: digest,
	}
}

func testRegistryView(name viewcompiler.RelationName, sql string, columns []viewcompiler.Column,
	dependencies ...viewcompiler.RelationName) viewcompiler.Relation {
	return viewcompiler.Relation{
		Name: name, Kind: viewcompiler.RelationView, DefinitionSQL: sql,
		DefinitionDigest: viewcompiler.ExactDefinitionDigest(sql), Columns: columns,
		Dependencies: append([]viewcompiler.RelationName(nil), dependencies...),
	}
}

func integerColumns(names ...string) []viewcompiler.Column {
	columns := make([]viewcompiler.Column, 0, len(names))
	for _, name := range names {
		columns = append(columns, viewcompiler.Column{Name: name, SQLType: "integer"})
	}
	return columns
}

func viewName(schema, name string) viewcompiler.RelationName {
	return viewcompiler.RelationName{Schema: schema, Name: name}
}

func productNames(products []catalog.Product) []string {
	names := make([]string, 0, len(products))
	for _, product := range products {
		names = append(names, product.Name)
	}
	return names
}
