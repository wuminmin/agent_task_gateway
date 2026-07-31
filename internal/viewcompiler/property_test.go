package viewcompiler

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const phaseBPropertyCases = 64

// phaseBJoinCase is a compact, reproducible description of a restricted SQL
// fragment. Parent describes a connected tree over 2..5 governed relations;
// Multi marks edges that carry both an id equality and a tenant equality.
// Generate always emits at least one multi-predicate edge.
type phaseBJoinCase struct {
	Count        int
	Parent       [5]int
	Multi        [5]bool
	Start        int
	Priority     [5]uint32
	AliasSalt    uint32
	Project      int
	FilterValue  int
	WrapperDepth int
}

func (phaseBJoinCase) Generate(random *rand.Rand, _ int) reflect.Value {
	generated := phaseBJoinCase{
		Count:        2 + random.Intn(4),
		FilterValue:  1 + random.Intn(1000),
		WrapperDepth: 1 + random.Intn(3),
		AliasSalt:    random.Uint32(),
	}
	generated.Parent[0] = -1
	for vertex := 1; vertex < generated.Count; vertex++ {
		generated.Parent[vertex] = random.Intn(vertex)
		generated.Multi[vertex] = random.Intn(2) == 0
	}
	// A nonzero start guarantees that the connected alternative traversal is
	// observably different from the natural parent-before-child traversal.
	generated.Start = 1 + random.Intn(generated.Count-1)
	generated.Project = random.Intn(generated.Count)
	for vertex := 0; vertex < generated.Count; vertex++ {
		generated.Priority[vertex] = random.Uint32()
	}
	generated.Multi[1+random.Intn(generated.Count-1)] = true
	return reflect.ValueOf(generated)
}

func TestPhaseBPropertyCompileDeterminism(t *testing.T) {
	phaseBQuickCheck(t, 2026080101, func(generated phaseBJoinCase) error {
		registry, products, dependencies := phaseBFixture(generated, false)
		root := RelationName{Schema: "semantic", Name: "phaseb_deterministic"}
		sql, err := phaseBJoinSQL(generated, phaseBNaturalOrder(generated), phaseBAliases(generated, false), "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		registry.Relations[root] = phaseBView(root, sql, "metric", dependencies...)

		compiler, err := New(registry, products)
		if err != nil {
			return fmt.Errorf("construct compiler: %w", err)
		}
		first, err := compiler.Compile(root)
		if err != nil {
			return fmt.Errorf("first compile: %w", err)
		}
		second, err := compiler.Compile(root)
		if err != nil {
			return fmt.Errorf("second compile: %w", err)
		}
		if !reflect.DeepEqual(first, second) {
			return fmt.Errorf("repeated Compile calls returned different artifacts")
		}

		reordered, reorderedProducts, reorderedDependencies := phaseBFixture(generated, true)
		reordered.Relations[root] = phaseBView(root, sql, "metric", reorderedDependencies...)
		rebuilt, err := New(reordered, reorderedProducts)
		if err != nil {
			return fmt.Errorf("construct reordered compiler: %w", err)
		}
		third, err := rebuilt.Compile(root)
		if err != nil {
			return fmt.Errorf("compile reordered registry: %w", err)
		}
		if !reflect.DeepEqual(first, third) {
			return fmt.Errorf("registry/product insertion order changed the artifact")
		}
		return nil
	})
}

func TestPhaseBPropertyAliasInvariance(t *testing.T) {
	phaseBQuickCheck(t, 2026080102, func(generated phaseBJoinCase) error {
		order := phaseBNaturalOrder(generated)
		baselineSQL, err := phaseBJoinSQL(generated, order, phaseBAliases(generated, false), "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		renamedSQL, err := phaseBJoinSQL(generated, order, phaseBAliases(generated, true), "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		baseline, err := phaseBCompileDirect(generated, "phaseb_alias_baseline", baselineSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile baseline aliases: %w", err)
		}
		renamed, err := phaseBCompileDirect(generated, "phaseb_alias_renamed", renamedSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile renamed aliases: %w", err)
		}
		if baseline.DefinitionDigest == renamed.DefinitionDigest {
			return fmt.Errorf("generated alias rewrite did not change exact definition bytes")
		}
		return phaseBRequireSemanticEquality(baseline, renamed)
	})
}

func TestPhaseBPropertyJoinOrderInvariance(t *testing.T) {
	phaseBQuickCheck(t, 2026080103, func(generated phaseBJoinCase) error {
		aliases := phaseBAliases(generated, false)
		baselineSQL, err := phaseBJoinSQL(generated, phaseBNaturalOrder(generated), aliases, "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		alternativeOrder, err := phaseBConnectedOrder(generated)
		if err != nil {
			return err
		}
		alternativeSQL, err := phaseBJoinSQL(generated, alternativeOrder, aliases, "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		baseline, err := phaseBCompileDirect(generated, "phaseb_order_baseline", baselineSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile natural join order: %w", err)
		}
		alternative, err := phaseBCompileDirect(generated, "phaseb_order_alternative", alternativeSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile connected join order: %w", err)
		}
		if baseline.DefinitionDigest == alternative.DefinitionDigest {
			return fmt.Errorf("generated join-order rewrite did not change exact definition bytes")
		}
		return phaseBRequireSemanticEquality(baseline, alternative)
	})
}

func TestPhaseBPropertyDirectAndNestedViewEquivalence(t *testing.T) {
	phaseBQuickCheck(t, 2026080104, func(generated phaseBJoinCase) error {
		direct, nested, sourceCount, err := phaseBCompileSplitJoinChain(generated)
		if err != nil {
			return err
		}
		if got, want := len(direct.DependencyClosure), sourceCount+1; got != want {
			return fmt.Errorf("direct closure has %d nodes, want %d", got, want)
		}
		// There is one view for every added edge: View1 is A JOIN B,
		// View2 is View1 JOIN C, and later views extend the same frontier.
		if got, want := len(nested.DependencyClosure), sourceCount+(sourceCount-1); got != want {
			return fmt.Errorf("split closure has %d nodes, want %d", got, want)
		}
		return phaseBRequireSemanticEquality(direct, nested)
	})
}

func TestPhaseBPropertySemanticDriftChangesCorrespondingDigests(t *testing.T) {
	phaseBQuickCheck(t, 2026080105, func(generated phaseBJoinCase) error {
		order, aliases := phaseBNaturalOrder(generated), phaseBAliases(generated, false)
		baselineSQL, err := phaseBJoinSQL(generated, order, aliases, "metric", generated.FilterValue)
		if err != nil {
			return err
		}
		filterDriftSQL, err := phaseBJoinSQL(generated, order, aliases, "metric", generated.FilterValue+1)
		if err != nil {
			return err
		}
		baseline, err := phaseBCompileDirect(generated, "phaseb_drift", baselineSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile drift baseline: %w", err)
		}
		filterDrift, err := phaseBCompileDirect(generated, "phaseb_drift", filterDriftSQL, "metric")
		if err != nil {
			return fmt.Errorf("compile filter drift: %w", err)
		}
		if baseline.DefinitionDigest == filterDrift.DefinitionDigest ||
			baseline.DependencyDigest == filterDrift.DependencyDigest ||
			baseline.CanonicalPlanDigest == filterDrift.CanonicalPlanDigest ||
			baseline.BindingDigest == filterDrift.BindingDigest {
			return fmt.Errorf("filter semantic drift did not change definition, dependency, plan, and binding digests")
		}
		if baseline.InterfaceDigest != filterDrift.InterfaceDigest {
			return fmt.Errorf("filter semantic drift unexpectedly changed the public interface digest")
		}

		interfaceSQL, err := phaseBJoinSQL(generated, order, aliases, "metric_changed", generated.FilterValue)
		if err != nil {
			return err
		}
		interfaceDrift, err := phaseBCompileDirect(generated, "phaseb_drift", interfaceSQL, "metric_changed")
		if err != nil {
			return fmt.Errorf("compile interface drift: %w", err)
		}
		if baseline.CanonicalPlanDigest != interfaceDrift.CanonicalPlanDigest {
			return fmt.Errorf("public rename changed relational semantics")
		}
		if baseline.DefinitionDigest == interfaceDrift.DefinitionDigest ||
			baseline.DependencyDigest == interfaceDrift.DependencyDigest ||
			baseline.InterfaceDigest == interfaceDrift.InterfaceDigest ||
			baseline.BindingDigest == interfaceDrift.BindingDigest {
			return fmt.Errorf("public rename did not change definition, dependency, interface, and binding digests")
		}

		publicBaseline, err := phaseBCompilePublicColumnClosure(generated, false)
		if err != nil {
			return fmt.Errorf("compile public-column baseline closure: %w", err)
		}
		publicAdded, err := phaseBCompilePublicColumnClosure(generated, true)
		if err != nil {
			return fmt.Errorf("compile added public column closure: %w", err)
		}
		if publicBaseline.DefinitionDigest == publicAdded.DefinitionDigest ||
			publicBaseline.DependencyDigest == publicAdded.DependencyDigest ||
			publicBaseline.InterfaceDigest == publicAdded.InterfaceDigest ||
			publicBaseline.CanonicalPlanDigest == publicAdded.CanonicalPlanDigest ||
			publicBaseline.BindingDigest == publicAdded.BindingDigest {
			return fmt.Errorf("added public column did not change definition, dependency, interface, plan, and binding digests")
		}
		if !reflect.DeepEqual(publicBaseline.BaseProducts, publicAdded.BaseProducts) {
			return fmt.Errorf("added public column unexpectedly changed the governed base-product closure")
		}

		predicateDriftSQL, err := phaseBJoinPredicateDriftSQL(generated, order, aliases, generated.FilterValue)
		if err != nil {
			return err
		}
		dependencyBaseline, err := phaseBCompileNestedDrift(generated, baselineSQL)
		if err != nil {
			return fmt.Errorf("compile nested dependency baseline: %w", err)
		}
		dependencyDrift, err := phaseBCompileNestedDrift(generated, predicateDriftSQL)
		if err != nil {
			return fmt.Errorf("compile nested join-predicate drift: %w", err)
		}
		if dependencyBaseline.DefinitionDigest != dependencyDrift.DefinitionDigest {
			return fmt.Errorf("child join-predicate drift changed the unchanged root definition digest")
		}
		if dependencyBaseline.DependencyDigest == dependencyDrift.DependencyDigest ||
			dependencyBaseline.CanonicalPlanDigest == dependencyDrift.CanonicalPlanDigest ||
			dependencyBaseline.BindingDigest == dependencyDrift.BindingDigest {
			return fmt.Errorf("child join-predicate drift did not change dependency, plan, and binding digests")
		}
		if dependencyBaseline.InterfaceDigest != dependencyDrift.InterfaceDigest {
			return fmt.Errorf("child join-predicate drift unexpectedly changed the root interface digest")
		}
		if !reflect.DeepEqual(dependencyBaseline.BaseProducts, dependencyDrift.BaseProducts) {
			return fmt.Errorf("child join-predicate drift unexpectedly changed the governed base-product closure")
		}
		return nil
	})
}

func phaseBQuickCheck(t *testing.T, seed int64, property func(phaseBJoinCase) error) {
	t.Helper()
	var detail error
	check := func(generated phaseBJoinCase) bool {
		detail = property(generated)
		return detail == nil
	}
	config := &quick.Config{
		MaxCount: phaseBPropertyCases,
		Rand:     rand.New(rand.NewSource(seed)),
	}
	if err := quick.Check(check, config); err != nil {
		t.Fatalf("QuickCheck failed (seed=%d, replay the reported phaseBJoinCase): %v; detail: %v", seed, err, detail)
	}
}

func phaseBFixture(generated phaseBJoinCase, reverse bool) (RegistrySnapshot, map[string]queryplan.Product, []RelationName) {
	registry := RegistrySnapshot{
		PostgreSQLMajorVersion: 16,
		Relations:              make(map[RelationName]Relation, generated.Count),
	}
	products := make(map[string]queryplan.Product, generated.Count)
	dependencies := make([]RelationName, generated.Count)
	for vertex := 0; vertex < generated.Count; vertex++ {
		productName := fmt.Sprintf("pbp%d", vertex)
		dependencies[vertex] = RelationName{Schema: "raw", Name: productName}
	}
	for step := 0; step < generated.Count; step++ {
		vertex := step
		if reverse {
			vertex = generated.Count - 1 - step
		}
		productName := fmt.Sprintf("pbp%d", vertex)
		product := queryplan.Product{
			Name:             productName,
			SourceNamespace:  "property." + productName,
			Snapshot:         "property-snapshot-1",
			StableRole:       productName,
			StableEntityKey:  []string{"id"},
			RequiredEvidence: []string{"tenant_id"},
			Columns: map[string]struct{}{
				"id": {}, "parent_id": {}, "tenant_id": {}, "value": {},
			},
			ColumnTypes: map[string]string{
				"id": "integer", "parent_id": "integer", "tenant_id": "integer", "value": "integer",
			},
			ColumnCollations:  map[string]string{},
			CollationVersions: map[string]string{},
			AllowedAggregates: map[string]struct{}{
				"count": {}, "sum": {}, "min": {}, "max": {},
			},
		}
		products[productName] = product
		relationName := dependencies[vertex]
		registry.Relations[relationName] = Relation{
			Name: relationName, Kind: RelationBase, ProductName: productName,
			Columns: []Column{
				{Name: "id", SQLType: "integer"},
				{Name: "parent_id", SQLType: "integer"},
				{Name: "tenant_id", SQLType: "integer"},
				{Name: "value", SQLType: "integer"},
			},
		}
	}
	return registry, products, dependencies
}

func phaseBView(name RelationName, sql, outputName string, dependencies ...RelationName) Relation {
	return phaseBViewColumns(name, sql, []Column{{Name: outputName, SQLType: "integer"}}, dependencies...)
}

func phaseBViewColumns(name RelationName, sql string, columns []Column, dependencies ...RelationName) Relation {
	return Relation{
		Name:             name,
		Kind:             RelationView,
		DefinitionSQL:    sql,
		DefinitionDigest: ExactDefinitionDigest(sql),
		Columns:          append([]Column(nil), columns...),
		Dependencies:     append([]RelationName(nil), dependencies...),
	}
}

func phaseBCompileDirect(generated phaseBJoinCase, rootName, sql, outputName string) (Artifact, error) {
	registry, products, dependencies := phaseBFixture(generated, false)
	root := RelationName{Schema: "semantic", Name: rootName}
	registry.Relations[root] = phaseBView(root, sql, outputName, dependencies...)
	compiler, err := New(registry, products)
	if err != nil {
		return Artifact{}, err
	}
	return compiler.Compile(root)
}

func phaseBCompileNestedDrift(generated phaseBJoinCase, childSQL string) (Artifact, error) {
	registry, products, dependencies := phaseBFixture(generated, false)
	child := RelationName{Schema: "semantic", Name: "phaseb_drift_child"}
	root := RelationName{Schema: "semantic", Name: "phaseb_drift_root"}
	registry.Relations[child] = phaseBView(child, childSQL, "metric", dependencies...)
	rootSQL := "SELECT child.metric FROM semantic.phaseb_drift_child AS child"
	registry.Relations[root] = phaseBView(root, rootSQL, "metric", child)
	compiler, err := New(registry, products)
	if err != nil {
		return Artifact{}, err
	}
	return compiler.Compile(root)
}

// phaseBCompileSplitJoinChain compares a direct 3..5-source JoinGraph with a
// genuinely split view closure. The split is semantic rather than a stack of
// transparent wrappers: View1 joins A-B, View2 joins View1-C, and each later
// view adds one more governed source through the previous public frontier.
func phaseBCompileSplitJoinChain(generated phaseBJoinCase) (Artifact, Artifact, int, error) {
	chain := generated
	chain.Count = 3 + int((generated.AliasSalt+uint32(generated.Count))%3)
	chain.Parent[0] = -1
	for vertex := 1; vertex < chain.Count; vertex++ {
		chain.Parent[vertex] = vertex - 1
	}
	chain.Project = chain.Count - 1
	chain.Multi[1] = true

	aliases := phaseBAliases(chain, true)
	directSQL, err := phaseBJoinSQL(chain, phaseBNaturalOrder(chain), aliases, "metric", chain.FilterValue)
	if err != nil {
		return Artifact{}, Artifact{}, 0, err
	}
	registry, products, dependencies := phaseBFixture(chain, false)
	directRoot := RelationName{Schema: "semantic", Name: "phaseb_split_direct"}
	registry.Relations[directRoot] = phaseBView(directRoot, directSQL, "metric", dependencies...)

	frontierColumns := []Column{
		{Name: "frontier_id", SQLType: "integer"},
		{Name: "frontier_tenant", SQLType: "integer"},
	}
	first := RelationName{Schema: "semantic", Name: "phaseb_split_view1"}
	firstLeftAlias := fmt.Sprintf("split_%08x_a", chain.AliasSalt)
	firstRightAlias := fmt.Sprintf("split_%08x_b", chain.AliasSalt)
	firstSQL := fmt.Sprintf(
		"SELECT %[2]s.id AS frontier_id, %[2]s.tenant_id AS frontier_tenant FROM raw.pbp0 AS %[1]s INNER JOIN raw.pbp1 AS %[2]s ON %[1]s.id = %[2]s.parent_id AND %[1]s.tenant_id = %[2]s.tenant_id",
		firstLeftAlias,
		firstRightAlias,
	)
	registry.Relations[first] = phaseBViewColumns(first, firstSQL, frontierColumns, dependencies[0], dependencies[1])
	previous := first
	for vertex := 2; vertex < chain.Count; vertex++ {
		name := RelationName{Schema: "semantic", Name: fmt.Sprintf("phaseb_split_view%d", vertex)}
		frontierAlias := fmt.Sprintf("frontier_%08x_%d", chain.AliasSalt, vertex)
		sourceAlias := fmt.Sprintf("split_%08x_source_%d", chain.AliasSalt, vertex)
		predicate := fmt.Sprintf("%s.frontier_id = %s.parent_id", frontierAlias, sourceAlias)
		if chain.Multi[vertex] {
			predicate += fmt.Sprintf(" AND %s.frontier_tenant = %s.tenant_id", frontierAlias, sourceAlias)
		}
		var sql string
		var columns []Column
		if vertex == chain.Count-1 {
			sql = fmt.Sprintf(
				"SELECT %[2]s.value AS metric FROM %[1]s AS %[3]s INNER JOIN raw.pbp%[4]d AS %[2]s ON %[5]s WHERE %[2]s.value = %[6]d::integer",
				previous,
				sourceAlias,
				frontierAlias,
				vertex,
				predicate,
				chain.FilterValue,
			)
			columns = []Column{{Name: "metric", SQLType: "integer"}}
		} else {
			sql = fmt.Sprintf(
				"SELECT %[2]s.id AS frontier_id, %[2]s.tenant_id AS frontier_tenant FROM %[1]s AS %[3]s INNER JOIN raw.pbp%[4]d AS %[2]s ON %[5]s",
				previous,
				sourceAlias,
				frontierAlias,
				vertex,
				predicate,
			)
			columns = frontierColumns
		}
		registry.Relations[name] = phaseBViewColumns(name, sql, columns, previous, dependencies[vertex])
		previous = name
	}

	compiler, err := New(registry, products)
	if err != nil {
		return Artifact{}, Artifact{}, 0, fmt.Errorf("construct split-chain compiler: %w", err)
	}
	direct, err := compiler.Compile(directRoot)
	if err != nil {
		return Artifact{}, Artifact{}, 0, fmt.Errorf("compile direct %d-source JoinGraph: %w", chain.Count, err)
	}
	nested, err := compiler.Compile(previous)
	if err != nil {
		return Artifact{}, Artifact{}, 0, fmt.Errorf("compile split %d-source JoinGraph: %w", chain.Count, err)
	}
	return direct, nested, chain.Count, nil
}

func phaseBCompilePublicColumnClosure(generated phaseBJoinCase, includePublicID bool) (Artifact, error) {
	order, aliases := phaseBNaturalOrder(generated), phaseBAliases(generated, false)
	childSQL, err := phaseBJoinSQLWithPublicID(generated, order, aliases, generated.FilterValue)
	if err != nil {
		return Artifact{}, err
	}
	registry, products, dependencies := phaseBFixture(generated, false)
	child := RelationName{Schema: "semantic", Name: "phaseb_public_child"}
	root := RelationName{Schema: "semantic", Name: "phaseb_public_root"}
	childColumns := []Column{
		{Name: "metric", SQLType: "integer"},
		{Name: "public_id", SQLType: "integer"},
	}
	registry.Relations[child] = phaseBViewColumns(child, childSQL, childColumns, dependencies...)
	rootSQL := "SELECT child.metric FROM semantic.phaseb_public_child AS child"
	rootColumns := childColumns[:1]
	if includePublicID {
		rootSQL = "SELECT child.metric, child.public_id FROM semantic.phaseb_public_child AS child"
		rootColumns = childColumns
	}
	registry.Relations[root] = phaseBViewColumns(root, rootSQL, rootColumns, child)
	compiler, err := New(registry, products)
	if err != nil {
		return Artifact{}, err
	}
	return compiler.Compile(root)
}

func phaseBJoinSQLWithPublicID(generated phaseBJoinCase, order []int, aliases []string, filterValue int) (string, error) {
	sql, err := phaseBJoinSQL(generated, order, aliases, "metric", filterValue)
	if err != nil {
		return "", err
	}
	marker := fmt.Sprintf("SELECT %s.value AS metric FROM", aliases[generated.Project])
	replacement := fmt.Sprintf("SELECT %s.value AS metric, %s.id AS public_id FROM", aliases[generated.Project], aliases[0])
	if !strings.Contains(sql, marker) {
		return "", fmt.Errorf("generated SQL is missing projection marker %q", marker)
	}
	return strings.Replace(sql, marker, replacement, 1), nil
}

func phaseBJoinPredicateDriftSQL(generated phaseBJoinCase, order []int, aliases []string, filterValue int) (string, error) {
	sql, err := phaseBJoinSQL(generated, order, aliases, "metric", filterValue)
	if err != nil {
		return "", err
	}
	// Vertex 1 always has vertex 0 as its generated parent. Both columns have
	// the same attested SQL type, so this remains a valid equi-join while
	// changing the canonical edge predicate from id to value.
	baseline := fmt.Sprintf("%s.id = %s.parent_id", aliases[0], aliases[1])
	drift := fmt.Sprintf("%s.value = %s.parent_id", aliases[0], aliases[1])
	if !strings.Contains(sql, baseline) {
		return "", fmt.Errorf("generated SQL is missing join predicate %q", baseline)
	}
	return strings.Replace(sql, baseline, drift, 1), nil
}

func phaseBNaturalOrder(generated phaseBJoinCase) []int {
	order := make([]int, generated.Count)
	for vertex := range order {
		order[vertex] = vertex
	}
	return order
}

func phaseBConnectedOrder(generated phaseBJoinCase) ([]int, error) {
	visited := make([]bool, generated.Count)
	visited[generated.Start] = true
	order := []int{generated.Start}
	for len(order) < generated.Count {
		selected := -1
		for vertex := 0; vertex < generated.Count; vertex++ {
			if visited[vertex] || !phaseBHasVisitedNeighbor(generated, vertex, visited) {
				continue
			}
			if selected == -1 || generated.Priority[vertex] < generated.Priority[selected] ||
				(generated.Priority[vertex] == generated.Priority[selected] && vertex < selected) {
				selected = vertex
			}
		}
		if selected == -1 {
			return nil, fmt.Errorf("generated tree has no connected traversal frontier")
		}
		visited[selected] = true
		order = append(order, selected)
	}
	return order, nil
}

func phaseBHasVisitedNeighbor(generated phaseBJoinCase, vertex int, visited []bool) bool {
	if vertex > 0 && visited[generated.Parent[vertex]] {
		return true
	}
	for child := 1; child < generated.Count; child++ {
		if generated.Parent[child] == vertex && visited[child] {
			return true
		}
	}
	return false
}

func phaseBAliases(generated phaseBJoinCase, renamed bool) []string {
	aliases := make([]string, generated.Count)
	for vertex := range aliases {
		if renamed {
			aliases[vertex] = fmt.Sprintf("renamed_%08x_%d", generated.AliasSalt, vertex)
		} else {
			aliases[vertex] = fmt.Sprintf("source_%d", vertex)
		}
	}
	return aliases
}

func phaseBJoinSQL(generated phaseBJoinCase, order []int, aliases []string, outputName string, filterValue int) (string, error) {
	if len(order) != generated.Count || len(aliases) != generated.Count {
		return "", fmt.Errorf("invalid generated order or alias count")
	}
	if generated.Project < 0 || generated.Project >= generated.Count {
		return "", fmt.Errorf("invalid generated projection vertex %d", generated.Project)
	}
	seen := make([]bool, generated.Count)
	first := order[0]
	if first < 0 || first >= generated.Count {
		return "", fmt.Errorf("invalid first vertex %d", first)
	}
	seen[first] = true
	var sql strings.Builder
	fmt.Fprintf(&sql, "SELECT %s.value AS %s FROM raw.pbp%d AS %s", aliases[generated.Project], outputName, first, aliases[first])
	for _, vertex := range order[1:] {
		if vertex < 0 || vertex >= generated.Count || seen[vertex] {
			return "", fmt.Errorf("invalid or duplicate traversal vertex %d", vertex)
		}
		parent, child, present := phaseBConnectingEdge(generated, vertex, seen)
		if !present {
			return "", fmt.Errorf("vertex %d is disconnected from the current JOIN prefix", vertex)
		}
		fmt.Fprintf(&sql, " INNER JOIN raw.pbp%d AS %s ON %s.id = %s.parent_id", vertex, aliases[vertex], aliases[parent], aliases[child])
		if generated.Multi[child] {
			fmt.Fprintf(&sql, " AND %s.tenant_id = %s.tenant_id", aliases[parent], aliases[child])
		}
		seen[vertex] = true
	}
	fmt.Fprintf(&sql, " WHERE %s.value = %d::integer", aliases[generated.Project], filterValue)
	return sql.String(), nil
}

func phaseBConnectingEdge(generated phaseBJoinCase, vertex int, seen []bool) (int, int, bool) {
	if vertex > 0 && seen[generated.Parent[vertex]] {
		return generated.Parent[vertex], vertex, true
	}
	for child := 1; child < generated.Count; child++ {
		if generated.Parent[child] == vertex && seen[child] {
			return vertex, child, true
		}
	}
	return 0, 0, false
}

func phaseBRequireSemanticEquality(left, right Artifact) error {
	if left.CanonicalPlanDigest != right.CanonicalPlanDigest {
		return fmt.Errorf("canonical plan digests differ: %s != %s", left.CanonicalPlanDigest, right.CanonicalPlanDigest)
	}
	if left.InterfaceDigest != right.InterfaceDigest {
		return fmt.Errorf("interface digests differ: %s != %s", left.InterfaceDigest, right.InterfaceDigest)
	}
	if !reflect.DeepEqual(left.Plan, right.Plan) {
		return fmt.Errorf("canonical QueryPlans differ: %#v != %#v", left.Plan, right.Plan)
	}
	if !reflect.DeepEqual(left.Outputs, right.Outputs) {
		return fmt.Errorf("expanded outputs differ: %#v != %#v", left.Outputs, right.Outputs)
	}
	if !reflect.DeepEqual(left.BaseProducts, right.BaseProducts) {
		return fmt.Errorf("base products differ: %v != %v", left.BaseProducts, right.BaseProducts)
	}
	return nil
}
