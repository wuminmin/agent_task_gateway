// Package compilerfixture defines the frozen, deterministic inputs for the
// final V5 View compiler experiment. The source-controlled registries are
// deterministic by protocol design and materialized in memory so compiler
// latency does not include registry discovery. They are not a substitute for
// the measured adapter: correctness is checked separately against the live
// PostgreSQL fixture installed by db/init/08-final-v5-compiler-fixture.sql.
package compilerfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const (
	Version              = "taskgate-final-v5-compiler-fixture-v1"
	SchemaName           = "final_v5_compiler"
	PostgreSQLMajor      = 16
	maxFixtureProducts   = 17
	FixtureSQLSHA256     = "2caacddd5d7e79535d5b84ceb6eb58dfe48983c7db76c956fc96a686e82214bc"
	registryRevisionSeed = Version + "\x00registry"
)

// Cell is one exact workload/scale/mode tuple in the frozen protocol.
type Cell struct {
	WorkloadID string
	Scale      string
	Mode       string
}

// FrozenCells is deliberately explicit so capability tests can prove that the
// adapter exhausts the preregistered matrix rather than accepting arbitrary
// inputs that happen to compile.
var FrozenCells = []Cell{
	{WorkloadID: "view-depth", Scale: "1", Mode: "compile"},
	{WorkloadID: "view-depth", Scale: "2", Mode: "compile"},
	{WorkloadID: "view-depth", Scale: "4", Mode: "compile"},
	{WorkloadID: "view-depth", Scale: "8", Mode: "compile"},
	{WorkloadID: "view-depth", Scale: "16", Mode: "compile"},
	{WorkloadID: "join-sources", Scale: "2", Mode: "compile"},
	{WorkloadID: "join-sources", Scale: "4", Mode: "compile"},
	{WorkloadID: "join-sources", Scale: "8", Mode: "compile"},
	{WorkloadID: "join-sources", Scale: "16", Mode: "compile"},
	{WorkloadID: "limit-controls", Scale: "depth-17", Mode: "structured_rejection"},
	{WorkloadID: "limit-controls", Scale: "sources-17", Mode: "structured_rejection"},
}

// Case contains immutable compiler inputs plus the independently written SQL
// oracle for one frozen cell. Maps must be treated as read-only by callers.
type Case struct {
	Cell
	Registry        viewcompiler.RegistrySnapshot
	Products        map[string]queryplan.Product
	MeasuredRoot    viewcompiler.RelationName
	SemanticRoots   map[string]viewcompiler.RelationName
	ExpectedDepth   int
	ExpectedSources int
	DirectSQL       string
	ExpectedRows    [][]any
}

// ArtifactDescriptor is the independently recomputable, non-sensitive shape
// and digest summary retained in raw evidence for each compiler artifact.
type ArtifactDescriptor struct {
	ArtifactSHA256      string
	DefinitionSHA256    string
	DependencySHA256    string
	InterfaceSHA256     string
	CanonicalPlanSHA256 string
	BindingSHA256       string
	OutputsSHA256       string
	BaseProductsSHA256  string
	ReachableRelations  int
	DependencyEdges     int
	ExpandedSources     int
	DefinitionBytes     int
	CanonicalPlanBytes  int
}

func IsFrozenCell(workloadID, scale, mode string) bool {
	for _, cell := range FrozenCells {
		if cell.WorkloadID == workloadID && cell.Scale == scale && cell.Mode == mode {
			return true
		}
	}
	return false
}

func Build(workloadID, scale, mode string) (Case, error) {
	if !IsFrozenCell(workloadID, scale, mode) {
		return Case{}, fmt.Errorf("unsupported frozen compiler cell %s/%s/%s", workloadID, scale, mode)
	}
	switch workloadID {
	case "view-depth":
		depth, _ := strconv.Atoi(scale)
		return buildDepthCase(depth, false)
	case "join-sources":
		sources, _ := strconv.Atoi(scale)
		return buildJoinCase(sources, false)
	case "limit-controls":
		if scale == "depth-17" {
			return buildDepthCase(17, true)
		}
		return buildJoinCase(17, true)
	default:
		return Case{}, errors.New("unreachable compiler workload")
	}
}

func (one Case) NewCompiler() (*viewcompiler.Compiler, error) {
	return viewcompiler.New(one.Registry, one.Products)
}

func (one Case) ExpectedVariantNames() []string {
	if one.Mode != "compile" {
		return nil
	}
	result := []string{"measured", "direct", "nested", "alias", "parenthesized", "repeat", "allocation"}
	if one.WorkloadID == "join-sources" {
		result = append(result, "join_order")
	}
	sort.Strings(result)
	return result
}

func buildDepthCase(depth int, rejection bool) (Case, error) {
	if depth < 1 || depth > 17 {
		return Case{}, fmt.Errorf("invalid fixture depth %d", depth)
	}
	products, relations, bases := baseFixture(1)
	direct := relationName("depth_direct_" + twoDigits(depth))
	directSQL := "SELECT source.id, source.value FROM raw.compiler_p00 AS source"
	relations[direct] = view(direct, directSQL, []viewcompiler.Column{integerColumn("id"), integerColumn("value")}, bases[0])

	dependency := bases[0]
	var nested viewcompiler.RelationName
	for index := 1; index <= depth; index++ {
		nested = relationName("depth_nested_" + twoDigits(depth) + "_" + twoDigits(index))
		sql := fmt.Sprintf("SELECT layer.id, layer.value FROM %s AS layer", dependency)
		relations[nested] = view(nested, sql, []viewcompiler.Column{integerColumn("id"), integerColumn("value")}, dependency)
		dependency = nested
	}

	alias := relationName("depth_alias_" + twoDigits(depth))
	aliasSQL := "SELECT renamed.id, renamed.value FROM raw.compiler_p00 AS renamed"
	relations[alias] = view(alias, aliasSQL, []viewcompiler.Column{integerColumn("id"), integerColumn("value")}, bases[0])
	parenthesized := relationName("depth_parenthesized_" + twoDigits(depth))
	parenthesizedSQL := "SELECT (wrapped.id) AS id, (wrapped.value) AS value FROM raw.compiler_p00 AS wrapped"
	relations[parenthesized] = view(parenthesized, parenthesizedSQL, []viewcompiler.Column{integerColumn("id"), integerColumn("value")}, bases[0])

	cell := Cell{WorkloadID: "view-depth", Scale: strconv.Itoa(depth), Mode: "compile"}
	if rejection {
		cell = Cell{WorkloadID: "limit-controls", Scale: "depth-17", Mode: "structured_rejection"}
	}
	return Case{
		Cell: cell,
		Registry: viewcompiler.RegistrySnapshot{
			PostgreSQLMajorVersion: PostgreSQLMajor,
			RevisionDigest:         SHA256String(registryRevisionSeed),
			Relations:              relations,
		},
		Products: products, MeasuredRoot: nested,
		SemanticRoots: map[string]viewcompiler.RelationName{
			"measured": nested, "direct": direct, "nested": nested,
			"alias": alias, "parenthesized": parenthesized,
		},
		ExpectedDepth: depth, ExpectedSources: 1,
		DirectSQL:    fmt.Sprintf("SELECT id, value FROM %s.compiler_p00", SchemaName),
		ExpectedRows: [][]any{{int32(1), int32(100)}, {int32(101), int32(200)}},
	}, nil
}

func buildJoinCase(sources int, rejection bool) (Case, error) {
	if sources < 2 || sources > maxFixtureProducts {
		return Case{}, fmt.Errorf("invalid fixture source count %d", sources)
	}
	products, relations, bases := baseFixture(sources)
	direct := relationName("join_direct_" + twoDigits(sources))
	naturalAliases := aliases(sources, "source")
	directSQL := joinSQL(sources, naturalOrder(sources), naturalAliases, false)
	relations[direct] = view(direct, directSQL, []viewcompiler.Column{integerColumn("metric")}, bases...)

	nested, nestedRelations := nestedJoinRelations(sources, bases)
	for name, relation := range nestedRelations {
		relations[name] = relation
	}
	alias := relationName("join_alias_" + twoDigits(sources))
	aliasSQL := joinSQL(sources, naturalOrder(sources), aliases(sources, "renamed"), false)
	relations[alias] = view(alias, aliasSQL, []viewcompiler.Column{integerColumn("metric")}, bases...)
	joinOrder := relationName("join_order_" + twoDigits(sources))
	orderSQL := joinSQL(sources, reverseOrder(sources), naturalAliases, false)
	relations[joinOrder] = view(joinOrder, orderSQL, []viewcompiler.Column{integerColumn("metric")}, bases...)
	parenthesized := relationName("join_parenthesized_" + twoDigits(sources))
	parenthesizedSQL := joinSQL(sources, naturalOrder(sources), aliases(sources, "wrapped"), true)
	relations[parenthesized] = view(parenthesized, parenthesizedSQL, []viewcompiler.Column{integerColumn("metric")}, bases...)

	cell := Cell{WorkloadID: "join-sources", Scale: strconv.Itoa(sources), Mode: "compile"}
	if rejection {
		cell = Cell{WorkloadID: "limit-controls", Scale: "sources-17", Mode: "structured_rejection"}
	}
	last := sources - 1
	return Case{
		Cell: cell,
		Registry: viewcompiler.RegistrySnapshot{
			PostgreSQLMajorVersion: PostgreSQLMajor,
			RevisionDigest:         SHA256String(registryRevisionSeed),
			Relations:              relations,
		},
		Products: products, MeasuredRoot: direct,
		SemanticRoots: map[string]viewcompiler.RelationName{
			"measured": direct, "direct": direct, "nested": nested, "alias": alias,
			"join_order": joinOrder, "parenthesized": parenthesized,
		},
		ExpectedDepth: 1, ExpectedSources: sources,
		DirectSQL:    directPostgreSQLJoin(sources),
		ExpectedRows: [][]any{{int32(100 + last)}, {int32(200 + last)}},
	}, nil
}

func baseFixture(count int) (map[string]queryplan.Product, map[viewcompiler.RelationName]viewcompiler.Relation, []viewcompiler.RelationName) {
	products := make(map[string]queryplan.Product, count)
	relations := make(map[viewcompiler.RelationName]viewcompiler.Relation, count+count)
	bases := make([]viewcompiler.RelationName, count)
	for index := 0; index < count; index++ {
		name := productName(index)
		product := queryplan.Product{
			Name: name, SourceNamespace: "taskgate.compiler." + name,
			Snapshot: Version, StableRole: name, StableEntityKey: []string{"id"},
			RequiredEvidence: []string{"tenant_id"},
			Columns:          map[string]struct{}{"id": {}, "parent_id": {}, "tenant_id": {}, "value": {}},
			ColumnTypes:      map[string]string{"id": "integer", "parent_id": "integer", "tenant_id": "integer", "value": "integer"},
			ColumnCollations: map[string]string{}, CollationVersions: map[string]string{},
			AllowedAggregates: map[string]struct{}{"count": {}, "sum": {}, "min": {}, "max": {}},
		}
		products[name] = product
		base := viewcompiler.RelationName{Schema: "raw", Name: name}
		bases[index] = base
		relations[base] = viewcompiler.Relation{
			Name: base, Kind: viewcompiler.RelationBase, ProductName: name,
			Columns: []viewcompiler.Column{integerColumn("id"), integerColumn("parent_id"), integerColumn("tenant_id"), integerColumn("value")},
		}
	}
	return products, relations, bases
}

func nestedJoinRelations(count int, bases []viewcompiler.RelationName) (viewcompiler.RelationName, map[viewcompiler.RelationName]viewcompiler.Relation) {
	result := make(map[viewcompiler.RelationName]viewcompiler.Relation, count-1)
	var previous viewcompiler.RelationName
	for index := 1; index < count; index++ {
		name := relationName("join_nested_" + twoDigits(count) + "_" + twoDigits(index))
		if index == 1 {
			if index == count-1 {
				sql := "SELECT right_source.value AS metric FROM raw.compiler_p00 AS left_source INNER JOIN raw.compiler_p01 AS right_source ON left_source.id = right_source.parent_id"
				result[name] = view(name, sql, []viewcompiler.Column{integerColumn("metric")}, bases[0], bases[1])
			} else {
				sql := "SELECT right_source.id AS frontier_id FROM raw.compiler_p00 AS left_source INNER JOIN raw.compiler_p01 AS right_source ON left_source.id = right_source.parent_id"
				result[name] = view(name, sql, []viewcompiler.Column{integerColumn("frontier_id")}, bases[0], bases[1])
			}
			previous = name
			continue
		}
		if index == count-1 {
			sql := fmt.Sprintf("SELECT source.value AS metric FROM %s AS frontier INNER JOIN raw.%s AS source ON frontier.frontier_id = source.parent_id", previous, productName(index))
			result[name] = view(name, sql, []viewcompiler.Column{integerColumn("metric")}, previous, bases[index])
		} else {
			sql := fmt.Sprintf("SELECT source.id AS frontier_id FROM %s AS frontier INNER JOIN raw.%s AS source ON frontier.frontier_id = source.parent_id", previous, productName(index))
			result[name] = view(name, sql, []viewcompiler.Column{integerColumn("frontier_id")}, previous, bases[index])
		}
		previous = name
	}
	return previous, result
}

func joinSQL(count int, order []int, names []string, parenthesized bool) string {
	last := count - 1
	first := order[0]
	from := fmt.Sprintf("raw.%s AS %s", productName(first), names[first])
	seen := map[int]bool{first: true}
	for _, vertex := range order[1:] {
		left, right := -1, -1
		if vertex > 0 && seen[vertex-1] {
			left, right = vertex-1, vertex
		} else if vertex+1 < count && seen[vertex+1] {
			left, right = vertex, vertex+1
		}
		join := fmt.Sprintf("%s INNER JOIN raw.%s AS %s ON %s.id = %s.parent_id", from, productName(vertex), names[vertex], names[left], names[right])
		if parenthesized {
			join = "(" + join + ")"
		}
		from = join
		seen[vertex] = true
	}
	return fmt.Sprintf("SELECT %s.value AS metric FROM %s", names[last], from)
}

func directPostgreSQLJoin(count int) string {
	var sql strings.Builder
	fmt.Fprintf(&sql, "SELECT p%02d.value AS metric FROM %s.%s AS p00", count-1, SchemaName, productName(0))
	for index := 1; index < count; index++ {
		fmt.Fprintf(&sql, " INNER JOIN %s.%s AS p%02d ON p%02d.id = p%02d.parent_id", SchemaName, productName(index), index, index-1, index)
	}
	return sql.String()
}

func naturalOrder(count int) []int {
	result := make([]int, count)
	for index := range result {
		result[index] = index
	}
	return result
}

func reverseOrder(count int) []int {
	result := make([]int, count)
	for index := range result {
		result[index] = count - index - 1
	}
	return result
}

func aliases(count int, prefix string) []string {
	result := make([]string, count)
	for index := range result {
		result[index] = prefix + twoDigits(index)
	}
	return result
}

func relationName(name string) viewcompiler.RelationName {
	return viewcompiler.RelationName{Schema: "semantic", Name: name}
}

func view(name viewcompiler.RelationName, sql string, columns []viewcompiler.Column, dependencies ...viewcompiler.RelationName) viewcompiler.Relation {
	return viewcompiler.Relation{
		Name: name, Kind: viewcompiler.RelationView, DefinitionSQL: sql,
		DefinitionDigest: viewcompiler.ExactDefinitionDigest(sql),
		Columns:          append([]viewcompiler.Column(nil), columns...), Dependencies: append([]viewcompiler.RelationName(nil), dependencies...),
	}
}

func integerColumn(name string) viewcompiler.Column {
	return viewcompiler.Column{Name: name, SQLType: "integer"}
}

func productName(index int) string { return "compiler_p" + twoDigits(index) }
func twoDigits(value int) string   { return fmt.Sprintf("%02d", value) }

func RegistrySHA256(registry viewcompiler.RegistrySnapshot) string {
	relations := make([]viewcompiler.Relation, 0, len(registry.Relations))
	for _, relation := range registry.Relations {
		relations = append(relations, relation)
	}
	sort.Slice(relations, func(i, j int) bool { return relations[i].Name.String() < relations[j].Name.String() })
	return JSONSHA256(struct {
		PostgreSQLMajorVersion int                     `json:"postgresql_major_version"`
		RevisionDigest         string                  `json:"revision_digest"`
		Relations              []viewcompiler.Relation `json:"relations"`
	}{registry.PostgreSQLMajorVersion, registry.RevisionDigest, relations})
}

func ProductsSHA256(products map[string]queryplan.Product) string {
	type productDigest struct {
		Name              string      `json:"name"`
		Columns           []string    `json:"columns"`
		AllowedAggregates []string    `json:"allowed_aggregates"`
		ColumnTypes       [][2]string `json:"column_types"`
		ColumnCollations  [][2]string `json:"column_collations"`
		CollationVersions [][2]string `json:"collation_versions"`
		SourceNamespace   string      `json:"source_namespace"`
		Snapshot          string      `json:"snapshot"`
		StableRole        string      `json:"stable_role"`
		StableEntityKey   []string    `json:"stable_entity_key"`
		LineageDigest     string      `json:"lineage_digest"`
		RequiredEvidence  []string    `json:"required_evidence"`
	}
	names := make([]string, 0, len(products))
	for name := range products {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]productDigest, 0, len(names))
	for _, name := range names {
		product := products[name]
		entry := productDigest{
			Name: product.Name, SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot,
			StableRole: product.StableRole, StableEntityKey: append([]string(nil), product.StableEntityKey...),
			LineageDigest: product.LineageDigest, RequiredEvidence: append([]string(nil), product.RequiredEvidence...),
		}
		for column := range product.Columns {
			entry.Columns = append(entry.Columns, column)
		}
		for aggregate := range product.AllowedAggregates {
			entry.AllowedAggregates = append(entry.AllowedAggregates, aggregate)
		}
		entry.ColumnTypes = sortedPairs(product.ColumnTypes)
		entry.ColumnCollations = sortedPairs(product.ColumnCollations)
		entry.CollationVersions = sortedPairs(product.CollationVersions)
		sort.Strings(entry.Columns)
		sort.Strings(entry.AllowedAggregates)
		result = append(result, entry)
	}
	return JSONSHA256(result)
}

func sortedPairs(values map[string]string) [][2]string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([][2]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, [2]string{key, values[key]})
	}
	return result
}

func DescribeArtifact(artifact viewcompiler.Artifact, registry viewcompiler.RegistrySnapshot) ArtifactDescriptor {
	planBytes, _ := json.Marshal(artifact.Plan)
	definitionBytes, dependencyEdges := 0, 0
	for _, dependency := range artifact.DependencyClosure {
		relation := registry.Relations[dependency.Name]
		definitionBytes += len(relation.DefinitionSQL)
		dependencyEdges += len(relation.Dependencies)
	}
	return ArtifactDescriptor{
		ArtifactSHA256: JSONSHA256(artifact), DefinitionSHA256: artifact.DefinitionDigest,
		DependencySHA256: artifact.DependencyDigest, InterfaceSHA256: artifact.InterfaceDigest,
		CanonicalPlanSHA256: artifact.CanonicalPlanDigest, BindingSHA256: artifact.BindingDigest,
		OutputsSHA256: JSONSHA256(artifact.Outputs), BaseProductsSHA256: JSONSHA256(artifact.BaseProducts),
		ReachableRelations: len(artifact.DependencyClosure), DependencyEdges: dependencyEdges,
		ExpandedSources: len(artifact.BaseProducts), DefinitionBytes: definitionBytes, CanonicalPlanBytes: len(planBytes),
	}
}

func JSONSHA256(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return SHA256Bytes(encoded)
}

func SHA256String(value string) string { return SHA256Bytes([]byte(value)) }
func SHA256Bytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// DatasetRows is the exact independently expected content of all seventeen
// PostgreSQL fixture tables. Numeric values use int32 to match pgx's decoding of
// PostgreSQL integer columns, although JSON hashing is type-width agnostic.
func DatasetRows() [][]any {
	rows := make([][]any, 0, maxFixtureProducts*2)
	for index := 0; index < maxFixtureProducts; index++ {
		firstParent, secondParent := int32(0), int32(0)
		if index > 0 {
			firstParent, secondParent = int32(index), int32(100+index)
		}
		rows = append(rows,
			[]any{productName(index), int32(index + 1), firstParent, int32(7), int32(100 + index)},
			[]any{productName(index), int32(101 + index), secondParent, int32(8), int32(200 + index)},
		)
	}
	return rows
}

func DatasetQuery() string {
	parts := make([]string, 0, maxFixtureProducts)
	for index := 0; index < maxFixtureProducts; index++ {
		name := productName(index)
		parts = append(parts, fmt.Sprintf("SELECT '%s'::text AS table_name,id,parent_id,tenant_id,value FROM %s.%s", name, SchemaName, name))
	}
	return strings.Join(parts, " UNION ALL ") + " ORDER BY table_name,id"
}
