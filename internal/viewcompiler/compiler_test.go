package viewcompiler

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestCompileNestedTransparentViewsEqualsDirectPlan(t *testing.T) {
	products, bases := testProductsAndBases("a", "b")
	directName := rel("semantic", "direct")
	nestedName := rel("semantic", "nested")
	childName := rel("semantic", "ab")
	directSQL := `
		SELECT a.id, b.value AS b_value
		FROM raw.a AS a INNER JOIN raw.b AS b ON a.id = b.parent_id
		WHERE a.value >= 1::integer`
	childSQL := `
		SELECT left_alias.id, right_alias.value AS b_value
		FROM raw.b AS right_alias
		INNER JOIN raw.a AS left_alias ON right_alias.parent_id = left_alias.id
		WHERE left_alias.value >= 1::pg_catalog.int4`
	registry := RegistrySnapshot{PostgreSQLMajorVersion: 16, Relations: bases}
	registry.Relations[directName] = testView(directName, directSQL,
		[]Column{intColumn("id"), intColumn("b_value")}, rel("raw", "a"), rel("raw", "b"))
	registry.Relations[childName] = testView(childName, childSQL,
		[]Column{intColumn("id"), intColumn("b_value")}, rel("raw", "b"), rel("raw", "a"))
	registry.Relations[nestedName] = testView(nestedName,
		`SELECT layer.id, layer.b_value FROM semantic.ab AS layer`,
		[]Column{intColumn("id"), intColumn("b_value")}, childName)

	compiler := mustCompiler(t, registry, products)
	direct := mustCompile(t, compiler, directName)
	nested := mustCompile(t, compiler, nestedName)
	if !reflect.DeepEqual(direct.Plan, nested.Plan) {
		t.Fatalf("transparent expansion changed plan:\ndirect=%#v\nnested=%#v", direct.Plan, nested.Plan)
	}
	if direct.CanonicalPlanDigest != nested.CanonicalPlanDigest {
		t.Fatalf("canonical digests differ: %s != %s", direct.CanonicalPlanDigest, nested.CanonicalPlanDigest)
	}
	if direct.InterfaceDigest != nested.InterfaceDigest {
		t.Fatalf("interfaces differ: %s != %s", direct.InterfaceDigest, nested.InterfaceDigest)
	}
	if !reflect.DeepEqual(nested.BaseProducts, []string{"a", "b"}) {
		t.Fatalf("base products = %v", nested.BaseProducts)
	}
	if nested.DefinitionDigest != ExactDefinitionDigest(registry.Relations[nestedName].DefinitionSQL) {
		t.Fatalf("root definition digest = %q", nested.DefinitionDigest)
	}
	if nested.DependencyDigest == "" || nested.BindingDigest == "" {
		t.Fatal("dependency or binding digest is empty")
	}
}

func TestCompileMeasuredMatchesCompileArtifact(t *testing.T) {
	products, bases := testProductsAndBases("a", "b")
	name := rel("semantic", "measured")
	registry := RegistrySnapshot{PostgreSQLMajorVersion: 16, Relations: bases}
	registry.Relations[name] = testView(name, `SELECT a.id, b.value AS b_value FROM raw.a a JOIN raw.b b ON a.id=b.parent_id`, []Column{intColumn("id"), intColumn("b_value")}, rel("raw", "a"), rel("raw", "b"))
	compiler := mustCompiler(t, registry, products)
	measured, metrics, err := compiler.CompileMeasured(name)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := compiler.Compile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(measured, plain) {
		t.Fatal("measured compile changed artifact")
	}
	sum := metrics.ParseValidation + metrics.RecursiveExpansion + metrics.JoinGraphCanonicalization + metrics.PlanMaterialization + metrics.DigestGeneration
	if metrics.Total < sum {
		t.Fatalf("overlapping compile stages: %+v", metrics)
	}
}

func TestCompileArbitraryDepthAndOneAggregateBarrier(t *testing.T) {
	products, bases := testProductsAndBases("orders", "lines")
	joinName := rel("semantic", "order_lines")
	aggregateName := rel("semantic", "customer_totals")
	rootName := rel("semantic", "report")
	registry := RegistrySnapshot{Relations: bases}
	registry.Relations[joinName] = testView(joinName, `
		SELECT o.tenant_id AS customer_id, l.value AS amount
		FROM raw.orders o JOIN raw.lines l ON o.id = l.parent_id`,
		[]Column{intColumn("customer_id"), intColumn("amount")}, rel("raw", "orders"), rel("raw", "lines"))
	registry.Relations[aggregateName] = testView(aggregateName, `
		SELECT x.customer_id, SUM(x.amount) AS revenue
		FROM semantic.order_lines x
		GROUP BY x.customer_id`,
		[]Column{intColumn("customer_id"), bigintColumn("revenue")}, joinName)
	registry.Relations[rootName] = testView(rootName,
		`SELECT totals.customer_id, totals.revenue FROM semantic.customer_totals totals`,
		[]Column{intColumn("customer_id"), bigintColumn("revenue")}, aggregateName)

	artifact := mustCompile(t, mustCompiler(t, registry, products), rootName)
	if len(artifact.Plan.Aggregates) != 1 || artifact.Plan.Aggregates[0].Function != "sum" ||
		artifact.Plan.Aggregates[0].Column != "lines.value" || artifact.Plan.Aggregates[0].Alias != "revenue" {
		t.Fatalf("aggregate plan = %#v", artifact.Plan.Aggregates)
	}
	if !reflect.DeepEqual(artifact.Plan.GroupBy, []string{"orders.tenant_id"}) {
		t.Fatalf("group by = %v", artifact.Plan.GroupBy)
	}

	previous := rootName
	for index := 0; index < 10; index++ {
		name := rel("semantic", fmt.Sprintf("wrapper_%02d", index))
		sql := fmt.Sprintf("SELECT w.customer_id, w.revenue FROM %s w", previous)
		registry.Relations[name] = testView(name, sql,
			[]Column{intColumn("customer_id"), bigintColumn("revenue")}, previous)
		previous = name
	}
	deep := mustCompile(t, mustCompiler(t, registry, products), previous)
	if deep.CanonicalPlanDigest != artifact.CanonicalPlanDigest {
		t.Fatalf("transparent depth changed aggregate semantics: %s != %s", deep.CanonicalPlanDigest, artifact.CanonicalPlanDigest)
	}
}

func TestCompileRejectsOperationsAcrossAggregateBarrier(t *testing.T) {
	products, bases := testProductsAndBases("orders", "lines", "customers")
	joinName := rel("semantic", "order_lines")
	aggregateName := rel("semantic", "customer_totals")
	registry := RegistrySnapshot{Relations: bases}
	registry.Relations[joinName] = testView(joinName, `
		SELECT o.tenant_id AS customer_id, l.value AS amount
		FROM raw.orders o JOIN raw.lines l ON o.id = l.parent_id`,
		[]Column{intColumn("customer_id"), intColumn("amount")}, rel("raw", "orders"), rel("raw", "lines"))
	registry.Relations[aggregateName] = testView(aggregateName, `
		SELECT x.customer_id, SUM(x.amount) AS revenue
		FROM semantic.order_lines x GROUP BY x.customer_id`,
		[]Column{intColumn("customer_id"), bigintColumn("revenue")}, joinName)

	tests := []struct {
		name    string
		sql     string
		columns []Column
		deps    []RelationName
	}{
		{name: "join", sql: `SELECT t.customer_id, t.revenue FROM semantic.customer_totals t JOIN raw.customers c ON t.customer_id = c.tenant_id`,
			columns: []Column{intColumn("customer_id"), bigintColumn("revenue")}, deps: []RelationName{aggregateName, rel("raw", "customers")}},
		{name: "where", sql: `SELECT t.customer_id, t.revenue FROM semantic.customer_totals t WHERE t.revenue > 0::bigint`,
			columns: []Column{intColumn("customer_id"), bigintColumn("revenue")}, deps: []RelationName{aggregateName}},
		{name: "reaggregate", sql: `SELECT SUM(t.revenue) AS total FROM semantic.customer_totals t`,
			columns: []Column{numericColumn("total")}, deps: []RelationName{aggregateName}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := rel("semantic", "bad_"+test.name)
			local := cloneRegistry(registry)
			local.Relations[name] = testView(name, test.sql, test.columns, test.deps...)
			_, err := mustCompiler(t, local, products).Compile(name)
			assertCode(t, err, CodeAggregationBarrier)
		})
	}
}

func TestSharedDAGDependencyExpandsPerOccurrence(t *testing.T) {
	products, bases := testProductsAndBases("a")
	leftName, rightName, rootName := rel("semantic", "left_a"), rel("semantic", "right_a"), rel("semantic", "shared_root")
	registry := RegistrySnapshot{Relations: bases}
	registry.Relations[leftName] = testView(leftName, `SELECT a.id FROM raw.a a`, []Column{intColumn("id")}, rel("raw", "a"))
	registry.Relations[rightName] = testView(rightName, `SELECT other.id FROM raw.a other`, []Column{intColumn("id")}, rel("raw", "a"))
	registry.Relations[rootName] = testView(rootName, `
		SELECT l.id
		FROM semantic.left_a l JOIN semantic.right_a r ON l.id = r.id`,
		[]Column{intColumn("id")}, leftName, rightName)

	_, err := mustCompiler(t, registry, products).Compile(rootName)
	assertCode(t, err, CodeStableRoleCollision)
}

func TestExpandedSourceGuardAccepts16Rejects17(t *testing.T) {
	build := func(count int) (RegistrySnapshot, map[string]queryplan.Product, RelationName) {
		products := make(map[string]queryplan.Product, count)
		relations := make(map[RelationName]Relation, count+1)
		dependencies := make([]RelationName, 0, count)
		var sql strings.Builder
		sql.WriteString("SELECT a00.value FROM raw.p00 a00")
		for index := 0; index < count; index++ {
			name := fmt.Sprintf("p%02d", index)
			product := testProduct(name)
			products[name] = product
			relationName := rel("raw", name)
			relations[relationName] = testBase(relationName, product)
			dependencies = append(dependencies, relationName)
			if index > 0 {
				fmt.Fprintf(&sql, " JOIN raw.%s a%02d ON a00.id = a%02d.parent_id", name, index, index)
			}
		}
		root := rel("semantic", fmt.Sprintf("join_%02d", count))
		relations[root] = testView(root, sql.String(), []Column{intColumn("value")}, dependencies...)
		return RegistrySnapshot{Relations: relations}, products, root
	}
	registry, products, root := build(queryplan.MaxJoinSources)
	artifact := mustCompile(t, mustCompiler(t, registry, products), root)
	if artifact.Plan.From == nil || artifact.Plan.From.JoinMany == nil || len(artifact.Plan.From.JoinMany.Sources) != queryplan.MaxJoinSources {
		t.Fatalf("16-source plan = %#v", artifact.Plan.From)
	}
	registry, products, root = build(queryplan.MaxJoinSources + 1)
	_, err := mustCompiler(t, registry, products).Compile(root)
	assertCode(t, err, CodeSourceLimit)
}

func TestViewDepthGuardAccepts16Rejects17(t *testing.T) {
	products, bases := testProductsAndBases("a")
	registry := RegistrySnapshot{Relations: bases}
	dependency := rel("raw", "a")
	var accepted RelationName
	for index := 0; index < MaxViewDepth+1; index++ {
		name := rel("semantic", fmt.Sprintf("depth_%02d", index))
		sql := fmt.Sprintf("SELECT x.id FROM %s x", dependency)
		registry.Relations[name] = testView(name, sql, []Column{intColumn("id")}, dependency)
		dependency = name
		if index == MaxViewDepth-1 {
			accepted = name
		}
	}
	mustCompile(t, mustCompiler(t, registry, products), accepted)
	_, err := mustCompiler(t, registry, products).Compile(dependency)
	assertCode(t, err, CodeDepthLimit)
}

func TestCycleDependencyAndSchemaMismatch(t *testing.T) {
	dummyProducts, dummyBases := testProductsAndBases("a")
	v1, v2 := rel("semantic", "v1"), rel("semantic", "v2")
	cycle := RegistrySnapshot{Relations: dummyBases}
	cycle.Relations[v1] = testView(v1, `SELECT x.id FROM semantic.v2 x`, []Column{intColumn("id")}, v2)
	cycle.Relations[v2] = testView(v2, `SELECT x.id FROM semantic.v1 x`, []Column{intColumn("id")}, v1)
	_, err := mustCompiler(t, cycle, dummyProducts).Compile(v1)
	assertCode(t, err, CodeCycle)

	mismatchName := rel("semantic", "dependency_mismatch")
	dependencyMismatch := RegistrySnapshot{Relations: cloneRelations(dummyBases)}
	dependencyMismatch.Relations[mismatchName] = testView(mismatchName, `SELECT a.id FROM raw.a a`, []Column{intColumn("id")})
	_, err = mustCompiler(t, dependencyMismatch, dummyProducts).Compile(mismatchName)
	assertCode(t, err, CodeDependencyMismatch)

	schemaName := rel("semantic", "schema_mismatch")
	schemaMismatch := RegistrySnapshot{Relations: cloneRelations(dummyBases)}
	schemaMismatch.Relations[schemaName] = testView(schemaName, `SELECT a.value FROM raw.a a`, []Column{bigintColumn("value")}, rel("raw", "a"))
	_, err = mustCompiler(t, schemaMismatch, dummyProducts).Compile(schemaName)
	assertCode(t, err, CodeSchemaMismatch)

	digestName := rel("semantic", "bad_digest")
	digestMismatch := RegistrySnapshot{Relations: cloneRelations(dummyBases)}
	digestMismatch.Relations[digestName] = Relation{Name: digestName, Kind: RelationView,
		DefinitionSQL: `SELECT a.id FROM raw.a a`, DefinitionDigest: strings.Repeat("0", 64),
		Columns: []Column{intColumn("id")}, Dependencies: []RelationName{rel("raw", "a")}}
	_, err = mustCompiler(t, digestMismatch, dummyProducts).Compile(digestName)
	assertCode(t, err, CodeDefinitionDigestMismatch)
}

func TestExactDefinitionsAndInterfaceDriftAreSeparated(t *testing.T) {
	products, bases := testProductsAndBases("a")
	root := rel("semantic", "filtered")
	compileSQL := func(sql string, columns []Column) Artifact {
		registry := RegistrySnapshot{Relations: cloneRelations(bases)}
		registry.Relations[root] = testView(root, sql, columns, rel("raw", "a"))
		return mustCompile(t, mustCompiler(t, registry, products), root)
	}
	first := compileSQL(`SELECT a.value AS amount FROM raw.a a WHERE a.value = 1::integer`, []Column{intColumn("amount")})
	semanticMutation := compileSQL(`SELECT a.value AS amount FROM raw.a a WHERE a.value = 2::integer`, []Column{intColumn("amount")})
	spacingOnly := compileSQL("SELECT  a.value AS amount\nFROM raw.a a WHERE a.value = 1::integer", []Column{intColumn("amount")})
	renameOnly := compileSQL(`SELECT a.value AS total FROM raw.a a WHERE a.value = 1::integer`, []Column{intColumn("total")})

	if first.CanonicalPlanDigest == semanticMutation.CanonicalPlanDigest {
		t.Fatal("filter mutation did not change canonical plan digest")
	}
	if first.CanonicalPlanDigest != spacingOnly.CanonicalPlanDigest {
		t.Fatal("formatting changed canonical plan digest")
	}
	if first.DefinitionDigest == spacingOnly.DefinitionDigest || first.DependencyDigest == spacingOnly.DependencyDigest || first.BindingDigest == spacingOnly.BindingDigest {
		t.Fatal("exact definition drift was not retained in dependency binding")
	}
	if first.CanonicalPlanDigest != renameOnly.CanonicalPlanDigest {
		t.Fatal("public alias changed canonical relational semantics")
	}
	if first.InterfaceDigest == renameOnly.InterfaceDigest {
		t.Fatal("public alias rename did not change interface digest")
	}
}

func TestDeparserLiteralCastsAreExactAndProjectionCastsRemainRejected(t *testing.T) {
	products, bases := testProductsAndBases("a")
	root := rel("semantic", "cast_literals")
	registry := RegistrySnapshot{Relations: bases}
	registry.Relations[root] = testView(root,
		`SELECT a.label FROM raw.a a WHERE a.label = 'x'::pg_catalog.text AND a.value >= 1::pg_catalog.int4`,
		[]Column{textColumn("label")}, rel("raw", "a"))
	mustCompile(t, mustCompiler(t, registry, products), root)

	badType := cloneRegistry(registry)
	badTypeSQL := `SELECT a.label FROM raw.a a WHERE a.value = 1::numeric`
	badType.Relations[root] = testView(root, badTypeSQL, []Column{textColumn("label")}, rel("raw", "a"))
	_, err := mustCompiler(t, badType, products).Compile(root)
	assertCode(t, err, CodeDefinitionUnsupported)

	projection := cloneRegistry(registry)
	projectionSQL := `SELECT a.value::bigint AS value FROM raw.a a`
	projection.Relations[root] = testView(root, projectionSQL, []Column{bigintColumn("value")}, rel("raw", "a"))
	_, err = mustCompiler(t, projection, products).Compile(root)
	assertCode(t, err, CodeDefinitionUnsupported)
}

func TestCompileDeterministicAcrossCallsAndRegistryMapOrder(t *testing.T) {
	products, bases := testProductsAndBases("a", "b", "c")
	root := rel("semantic", "deterministic")
	rootRelation := testView(root, `
		SELECT a.value
		FROM raw.a a JOIN raw.b b ON a.id = b.parent_id
		JOIN raw.c c ON b.id = c.parent_id`, []Column{intColumn("value")}, rel("raw", "a"), rel("raw", "b"), rel("raw", "c"))
	registry := RegistrySnapshot{Relations: bases}
	registry.Relations[root] = rootRelation
	compiler := mustCompiler(t, registry, products)
	first := mustCompile(t, compiler, root)
	second := mustCompile(t, compiler, root)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same compiler returned non-deterministic artifacts:\nfirst=%#v\nsecond=%#v", first, second)
	}

	names := make([]RelationName, 0, len(registry.Relations))
	for name := range registry.Relations {
		names = append(names, name)
	}
	random := rand.New(rand.NewSource(77))
	random.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	reordered := RegistrySnapshot{Relations: make(map[RelationName]Relation, len(names))}
	for _, name := range names {
		reordered.Relations[name] = registry.Relations[name]
	}
	third := mustCompile(t, mustCompiler(t, reordered, products), root)
	if !reflect.DeepEqual(first, third) {
		t.Fatal("registry insertion order changed artifact")
	}
}

func TestPropertyAliasAndJoinOrderDoNotChangeCanonicalPlan(t *testing.T) {
	products, bases := testProductsAndBases("a", "b", "c")
	root := rel("semantic", "property_root")
	baselineRegistry := RegistrySnapshot{Relations: cloneRelations(bases)}
	baselineRegistry.Relations[root] = testView(root, `
		SELECT a.value
		FROM raw.a a JOIN raw.b b ON a.id = b.parent_id
		JOIN raw.c c ON b.id = c.parent_id`, []Column{intColumn("value")}, rel("raw", "a"), rel("raw", "b"), rel("raw", "c"))
	want := mustCompile(t, mustCompiler(t, baselineRegistry, products), root).CanonicalPlanDigest
	aliases := []string{"left_node", "middle_node", "right_node", "alpha", "beta", "gamma"}
	property := func(seed uint8) bool {
		a := aliases[int(seed)%len(aliases)]
		b := aliases[(int(seed)+1)%len(aliases)]
		c := aliases[(int(seed)+2)%len(aliases)]
		if a == b || b == c || a == c {
			return true
		}
		var sql string
		if seed%2 == 0 {
			sql = fmt.Sprintf(`SELECT %s.value FROM raw.a %s JOIN raw.b %s ON %s.id = %s.parent_id JOIN raw.c %s ON %s.id = %s.parent_id`, a, a, b, a, b, c, b, c)
		} else {
			sql = fmt.Sprintf(`SELECT %s.value FROM raw.c %s JOIN raw.b %s ON %s.parent_id = %s.id JOIN raw.a %s ON %s.parent_id = %s.id`, a, c, b, c, b, a, b, a)
		}
		registry := RegistrySnapshot{Relations: cloneRelations(bases)}
		registry.Relations[root] = testView(root, sql, []Column{intColumn("value")}, rel("raw", "c"), rel("raw", "a"), rel("raw", "b"))
		compiler, err := New(registry, products)
		if err != nil {
			return false
		}
		artifact, err := compiler.Compile(root)
		return err == nil && artifact.CanonicalPlanDigest == want
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 100, Rand: rand.New(rand.NewSource(20260731))}); err != nil {
		t.Fatal(err)
	}
}

func TestInspectDefinitionFindsPinnedAndRecursiveReferences(t *testing.T) {
	owner := rel("reporting", "recursive_report")
	inspection, err := InspectDefinition(owner, `
		WITH RECURSIVE recursive_report(id) AS (
			SELECT 1
			UNION ALL
			SELECT id + 1 FROM recursive_report WHERE id < 3
		)
		SELECT id FROM recursive_report`)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.RecursiveSelf || !reflect.DeepEqual(inspection.References, []RelationName{owner}) {
		t.Fatalf("recursive inspection = %#v", inspection)
	}

	ordinary, err := InspectDefinition(rel("reporting", "joined"), `
		SELECT a.id
		FROM raw.a a JOIN semantic.child c ON a.id = c.id
		WHERE a.value = 1::pg_catalog.int4`)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.RecursiveSelf || !reflect.DeepEqual(ordinary.References, []RelationName{rel("raw", "a"), rel("semantic", "child")}) {
		t.Fatalf("ordinary inspection = %#v", ordinary)
	}
	if _, err := InspectDefinition(rel("reporting", "volatile"), `SELECT clock_timestamp() FROM raw.a`); err == nil {
		t.Fatal("volatile scalar function was accepted")
	}
}

func testProductsAndBases(names ...string) (map[string]queryplan.Product, map[RelationName]Relation) {
	products := make(map[string]queryplan.Product, len(names))
	relations := make(map[RelationName]Relation, len(names))
	for _, name := range names {
		product := testProduct(name)
		products[name] = product
		relationName := rel("raw", name)
		relations[relationName] = testBase(relationName, product)
	}
	return products, relations
}

func testProduct(name string) queryplan.Product {
	return queryplan.Product{
		Name: name, SourceNamespace: "test." + name, Snapshot: "snapshot-1", StableRole: name,
		StableEntityKey: []string{"id"}, RequiredEvidence: []string{"tenant_id"},
		Columns:          map[string]struct{}{"id": {}, "parent_id": {}, "value": {}, "tenant_id": {}, "label": {}},
		ColumnTypes:      map[string]string{"id": "integer", "parent_id": "integer", "value": "integer", "tenant_id": "integer", "label": "text"},
		ColumnCollations: map[string]string{"label": "C"}, CollationVersions: map[string]string{"label": "builtin"},
		AllowedAggregates: map[string]struct{}{"count": {}, "sum": {}, "min": {}, "max": {}},
	}
}

func testBase(name RelationName, product queryplan.Product) Relation {
	return Relation{Name: name, Kind: RelationBase, ProductName: product.Name,
		Columns: []Column{intColumn("id"), intColumn("parent_id"), intColumn("value"), intColumn("tenant_id"), textColumn("label")}}
}

func testView(name RelationName, sql string, columns []Column, dependencies ...RelationName) Relation {
	return Relation{Name: name, Kind: RelationView, DefinitionSQL: sql, DefinitionDigest: ExactDefinitionDigest(sql),
		Columns: append([]Column(nil), columns...), Dependencies: append([]RelationName(nil), dependencies...)}
}

func intColumn(name string) Column     { return Column{Name: name, SQLType: "integer"} }
func bigintColumn(name string) Column  { return Column{Name: name, SQLType: "bigint"} }
func numericColumn(name string) Column { return Column{Name: name, SQLType: "numeric"} }
func textColumn(name string) Column {
	return Column{Name: name, SQLType: "text", Collation: "C", CollationVersion: "builtin"}
}

func rel(schema, name string) RelationName { return RelationName{Schema: schema, Name: name} }

func mustCompiler(t *testing.T, registry RegistrySnapshot, products map[string]queryplan.Product) *Compiler {
	t.Helper()
	compiler, err := New(registry, products)
	if err != nil {
		t.Fatal(err)
	}
	return compiler
}

func mustCompile(t *testing.T, compiler *Compiler, root RelationName) Artifact {
	t.Helper()
	artifact, err := compiler.Compile(root)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func assertCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", want)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != want {
		t.Fatalf("error = %#v, want code %s", err, want)
	}
}

func cloneRelations(input map[RelationName]Relation) map[RelationName]Relation {
	result := make(map[RelationName]Relation, len(input))
	for name, relation := range input {
		relation.Columns = append([]Column(nil), relation.Columns...)
		relation.Dependencies = append([]RelationName(nil), relation.Dependencies...)
		result[name] = relation
	}
	return result
}

func cloneRegistry(input RegistrySnapshot) RegistrySnapshot {
	input.Relations = cloneRelations(input.Relations)
	return input
}
