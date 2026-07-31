package viewcompiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

var (
	catalogIdentifier  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	relationIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// Compiler owns cloned registry and product snapshots and is safe for
// concurrent Compile calls.
type Compiler struct {
	snapshot RegistrySnapshot
	products map[string]queryplan.Product
}

func New(snapshot RegistrySnapshot, products map[string]queryplan.Product) (*Compiler, error) {
	if len(snapshot.Relations) == 0 {
		return nil, reject(CodeInvalidRegistry, RelationName{}, "registry has no relations")
	}
	if len(products) == 0 {
		return nil, reject(CodeInvalidRegistry, RelationName{}, "compiler has no governed products")
	}
	cloned := RegistrySnapshot{
		PostgreSQLMajorVersion: snapshot.PostgreSQLMajorVersion,
		RevisionDigest:         snapshot.RevisionDigest,
		Relations:              make(map[RelationName]Relation, len(snapshot.Relations)),
	}
	for key, relation := range snapshot.Relations {
		relation.Columns = append([]Column(nil), relation.Columns...)
		relation.Dependencies = append([]RelationName(nil), relation.Dependencies...)
		cloned.Relations[key] = relation
	}
	clonedProducts := make(map[string]queryplan.Product, len(products))
	for name, product := range products {
		clonedProducts[name] = cloneProduct(product)
	}
	return &Compiler{snapshot: cloned, products: clonedProducts}, nil
}

type aggregateBinding struct {
	Function string
	Argument string
}

type outputBinding struct {
	Output
	aggregate *aggregateBinding
}

type fragment struct {
	sources    []queryplan.Scan
	predicates []queryplan.JoinPredicate
	filters    []queryplan.Filter
	groupBy    []string
	outputs    []outputBinding
	grouped    bool
}

type compileState struct {
	compiler       *Compiler
	root           RelationName
	parsed         map[RelationName]*pg_query.SelectStmt
	memo           map[RelationName]fragment
	reachable      map[RelationName]Relation
	visiting       map[RelationName]bool
	visited        map[RelationName]bool
	totalBytes     int
	dependencyEdge int
}

func (compiler *Compiler) Compile(root RelationName) (Artifact, error) {
	if compiler == nil {
		return Artifact{}, reject(CodeInvalidRegistry, root, "compiler is nil")
	}
	state := &compileState{
		compiler: compiler, root: root, parsed: make(map[RelationName]*pg_query.SelectStmt),
		memo: make(map[RelationName]fragment), reachable: make(map[RelationName]Relation),
		visiting: make(map[RelationName]bool), visited: make(map[RelationName]bool),
	}
	if err := state.preflight(root, 1); err != nil {
		return Artifact{}, err
	}
	expanded, err := state.compileRelation(root)
	if err != nil {
		return Artifact{}, err
	}
	plan, err := state.materializePlan(root, expanded)
	if err != nil {
		return Artifact{}, err
	}
	_, normal, semanticErr := queryplan.CompileSemantic(plan, compiler.products)
	if semanticErr != nil {
		return Artifact{}, reject(CodePlanInvalid, root, "expanded semantic plan: %v", semanticErr)
	}

	outputs := make([]Output, len(expanded.outputs))
	for index, binding := range expanded.outputs {
		outputs[index] = binding.Output
	}
	interfaceDigest, err := digestInterface(outputs)
	if err != nil {
		return Artifact{}, reject(CodePlanInvalid, root, "interface digest: %v", err)
	}
	closure := state.dependencyClosure()
	baseProducts := baseProductNames(expanded.sources)
	dependencyDigest, err := state.digestDependencies(closure)
	if err != nil {
		return Artifact{}, reject(CodePlanInvalid, root, "dependency digest: %v", err)
	}
	bindingDigest, err := state.digestBinding(normal.SHA256, interfaceDigest, dependencyDigest, closure, baseProducts)
	if err != nil {
		return Artifact{}, reject(CodePlanInvalid, root, "binding digest: %v", err)
	}
	return Artifact{
		Root: root, Plan: plan, Outputs: outputs, BaseProducts: baseProducts,
		DependencyClosure: closure, DefinitionDigest: state.reachable[root].DefinitionDigest,
		DependencyDigest: dependencyDigest, InterfaceDigest: interfaceDigest,
		CanonicalPlanDigest: normal.SHA256, BindingDigest: bindingDigest,
	}, nil
}

func (state *compileState) preflight(name RelationName, depth int) error {
	if state.visiting[name] {
		return reject(CodeCycle, name, "dependency graph contains a cycle")
	}
	if state.visited[name] {
		return nil
	}
	relation, present := state.compiler.snapshot.Relations[name]
	if !present {
		return reject(CodeRelationNotFound, name, "relation is absent from the registry snapshot")
	}
	if relation.Name != name {
		return reject(CodeInvalidRegistry, name, "relation record name does not match its registry key")
	}
	if err := validateRelationName(name); err != nil {
		return reject(CodeInvalidRegistry, name, "%v", err)
	}
	if relation.Kind == RelationView && depth > MaxViewDepth {
		return reject(CodeDepthLimit, name, "view dependency depth exceeds %d", MaxViewDepth)
	}
	if len(state.reachable) >= MaxViewNodes {
		return reject(CodeNodeLimit, name, "reachable relation count exceeds %d", MaxViewNodes)
	}
	state.reachable[name] = relation
	state.visiting[name] = true
	defer delete(state.visiting, name)

	if len(relation.Columns) == 0 {
		return reject(CodeSchemaMismatch, name, "relation has no attested output columns")
	}
	if err := validateDeclaredColumns(relation.Columns); err != nil {
		return reject(CodeSchemaMismatch, name, "%v", err)
	}
	if duplicateRelationNames(relation.Dependencies) {
		return reject(CodeInvalidRegistry, name, "declared dependencies are not unique")
	}
	state.totalBytes += len(relation.DefinitionSQL)
	if state.totalBytes > MaxDefinitionBytes {
		return reject(CodeDefinitionBytesLimit, name, "reachable definition bytes exceed %d", MaxDefinitionBytes)
	}

	switch relation.Kind {
	case RelationBase:
		if relation.ProductName == "" {
			return reject(CodeInvalidRegistry, name, "base relation has no governed product")
		}
		if len(relation.Dependencies) != 0 {
			return reject(CodeInvalidRegistry, name, "base relation must be an opaque dependency leaf")
		}
		if relation.DefinitionSQL != "" || relation.DefinitionDigest != "" {
			if relation.DefinitionSQL == "" || relation.DefinitionDigest != ExactDefinitionDigest(relation.DefinitionSQL) {
				return reject(CodeDefinitionDigestMismatch, name, "base definition digest does not bind its exact bytes")
			}
		}
		if err := state.validateBaseSchema(relation); err != nil {
			return err
		}
	case RelationView:
		if relation.ProductName != "" {
			return reject(CodeInvalidRegistry, name, "expandable view cannot also be an opaque product")
		}
		if strings.TrimSpace(relation.DefinitionSQL) == "" {
			return reject(CodeDefinitionUnsupported, name, "view definition is empty")
		}
		if relation.DefinitionDigest != ExactDefinitionDigest(relation.DefinitionSQL) {
			return reject(CodeDefinitionDigestMismatch, name, "definition digest does not bind exact pg_get_viewdef bytes")
		}
		statement, actual, parseErr := state.parseDefinition(name, relation.DefinitionSQL)
		if parseErr != nil {
			return parseErr
		}
		state.parsed[name] = statement
		if !sameRelationSet(actual, relation.Dependencies) {
			return reject(CodeDependencyMismatch, name, "parsed relations %v do not match declared dependencies %v", relationNamesText(actual), relationNamesText(relation.Dependencies))
		}
		state.dependencyEdge += len(relation.Dependencies)
		if state.dependencyEdge > MaxDependencyEdges {
			return reject(CodeEdgeLimit, name, "reachable dependency edges exceed %d", MaxDependencyEdges)
		}
		for _, dependency := range sortedRelationNames(relation.Dependencies) {
			if err := state.preflight(dependency, depth+1); err != nil {
				return err
			}
		}
	default:
		return reject(CodeInvalidRegistry, name, "unknown relation kind %q", relation.Kind)
	}
	state.visited[name] = true
	return nil
}

func (state *compileState) validateBaseSchema(relation Relation) error {
	product, present := state.compiler.products[relation.ProductName]
	if !present || product.Name != relation.ProductName {
		return reject(CodeInvalidRegistry, relation.Name, "governed product %q is absent", relation.ProductName)
	}
	if product.StableRole == "" || product.SourceNamespace == "" || product.Snapshot == "" {
		return reject(CodeInvalidRegistry, relation.Name, "governed product lacks stable semantic metadata")
	}
	if len(relation.Columns) != len(product.Columns) {
		return reject(CodeSchemaMismatch, relation.Name, "base output count %d does not match product field count %d", len(relation.Columns), len(product.Columns))
	}
	seen := make(map[string]struct{}, len(relation.Columns))
	for _, column := range relation.Columns {
		if _, approved := product.Columns[column.Name]; !approved {
			return reject(CodeSchemaMismatch, relation.Name, "base column %q is not present in governed product", column.Name)
		}
		if err := sameColumnType(column, product.ColumnTypes[column.Name], product.ColumnCollations[column.Name], product.CollationVersions[column.Name]); err != nil {
			return reject(CodeSchemaMismatch, relation.Name, "base column %q: %v", column.Name, err)
		}
		seen[column.Name] = struct{}{}
	}
	if len(seen) != len(product.Columns) {
		return reject(CodeSchemaMismatch, relation.Name, "base relation omits governed product fields")
	}
	return nil
}

func (state *compileState) compileRelation(name RelationName) (fragment, error) {
	if cached, present := state.memo[name]; present {
		return cloneFragment(cached), nil
	}
	relation := state.reachable[name]
	var result fragment
	var err error
	if relation.Kind == RelationBase {
		result, err = state.baseFragment(relation)
	} else {
		result, err = state.compileSelect(name, state.parsed[name], relation)
	}
	if err != nil {
		return fragment{}, err
	}
	state.memo[name] = cloneFragment(result)
	return cloneFragment(result), nil
}

func (state *compileState) baseFragment(relation Relation) (fragment, error) {
	product := state.compiler.products[relation.ProductName]
	outputs := make([]outputBinding, 0, len(relation.Columns))
	for _, column := range relation.Columns {
		typeName, err := exposure.CanonicalSQLTypeV2(column.SQLType)
		if err != nil {
			return fragment{}, reject(CodeSchemaMismatch, relation.Name, "column %q type: %v", column.Name, err)
		}
		outputs = append(outputs, outputBinding{Output: Output{
			Name: column.Name, SQLType: typeName, Collation: column.Collation,
			CollationVersion: column.CollationVersion, Kind: OutputField,
			FieldID: product.StableRole + "." + column.Name,
		}})
	}
	return fragment{sources: []queryplan.Scan{{Product: product.Name, Role: product.StableRole}}, outputs: outputs}, nil
}

func (state *compileState) materializePlan(root RelationName, expanded fragment) (queryplan.QueryPlan, error) {
	if len(expanded.sources) == 0 || len(expanded.sources) > queryplan.MaxJoinSources {
		return queryplan.QueryPlan{}, reject(CodeSourceLimit, root, "expanded source count %d is outside 1..%d", len(expanded.sources), queryplan.MaxJoinSources)
	}
	seenProducts := make(map[string]struct{}, len(expanded.sources))
	seenRoles := make(map[string]struct{}, len(expanded.sources))
	for _, scan := range expanded.sources {
		if _, duplicate := seenProducts[scan.Product]; duplicate {
			return queryplan.QueryPlan{}, reject(CodeStableRoleCollision, root, "expanded plan repeats governed product %q", scan.Product)
		}
		if _, duplicate := seenRoles[scan.Role]; duplicate {
			return queryplan.QueryPlan{}, reject(CodeStableRoleCollision, root, "expanded plan repeats stable role %q", scan.Role)
		}
		seenProducts[scan.Product] = struct{}{}
		seenRoles[scan.Role] = struct{}{}
	}

	plan := queryplan.QueryPlan{Filters: append([]queryplan.Filter(nil), expanded.filters...), GroupBy: append([]string(nil), expanded.groupBy...)}
	for _, output := range expanded.outputs {
		switch output.Kind {
		case OutputField:
			plan.Columns = append(plan.Columns, output.FieldID)
		case OutputAggregate:
			if output.aggregate == nil {
				return queryplan.QueryPlan{}, reject(CodePlanInvalid, root, "aggregate output %q has no expression", output.Name)
			}
			plan.Aggregates = append(plan.Aggregates, queryplan.Aggregate{Function: output.aggregate.Function, Column: output.aggregate.Argument, Alias: output.Name})
		default:
			return queryplan.QueryPlan{}, reject(CodePlanInvalid, root, "output %q has unknown binding kind", output.Name)
		}
	}
	if len(expanded.sources) == 1 {
		plan.From = &queryplan.From{Scan: &expanded.sources[0]}
	} else {
		graph, graphErr := state.fragmentJoinGraph(expanded)
		if graphErr != nil {
			return queryplan.QueryPlan{}, graphErr
		}
		join, joinErr := graph.JoinMany(state.compiler.products)
		if joinErr != nil {
			return queryplan.QueryPlan{}, reject(CodePlanInvalid, root, "canonical join graph: %v", joinErr)
		}
		plan.From = &queryplan.From{JoinMany: &join}
	}
	canonical, err := queryplan.CanonicalizeRelational(plan, state.compiler.products)
	if err != nil {
		return queryplan.QueryPlan{}, reject(CodePlanInvalid, root, "canonical QueryPlan: %v", err)
	}
	return canonical, nil
}

func (state *compileState) fragmentJoinGraph(expanded fragment) (sqllowering.JoinGraph, error) {
	nodes := make([]sqllowering.RelationNode, 0, len(expanded.sources))
	for _, scan := range expanded.sources {
		nodes = append(nodes, sqllowering.RelationNode{Relation: scan.Role, Product: scan.Product})
	}
	edges := make(map[string]*sqllowering.JoinEdge)
	for _, predicate := range expanded.predicates {
		leftRole, leftColumn, leftOK := splitFieldID(predicate.Left)
		rightRole, rightColumn, rightOK := splitFieldID(predicate.Right)
		if !leftOK || !rightOK || leftRole == rightRole {
			return sqllowering.JoinGraph{}, reject(CodePlanInvalid, state.root, "invalid expanded join predicate %q = %q", predicate.Left, predicate.Right)
		}
		if rightRole < leftRole {
			leftRole, rightRole = rightRole, leftRole
			leftColumn, rightColumn = rightColumn, leftColumn
		}
		key := leftRole + "\x00" + rightRole
		edge := edges[key]
		if edge == nil {
			edge = &sqllowering.JoinEdge{LeftRelation: leftRole, RightRelation: rightRole}
			edges[key] = edge
		}
		edge.Predicates = append(edge.Predicates, sqllowering.EqualityPredicate{LeftColumn: leftColumn, RightColumn: rightColumn})
	}
	graph := sqllowering.JoinGraph{Nodes: nodes, Edges: make([]sqllowering.JoinEdge, 0, len(edges))}
	for _, edge := range edges {
		graph.Edges = append(graph.Edges, *edge)
	}
	return graph, nil
}

func (state *compileState) dependencyClosure() []DependencyRef {
	names := make([]RelationName, 0, len(state.reachable))
	for name := range state.reachable {
		names = append(names, name)
	}
	sortRelationNames(names)
	result := make([]DependencyRef, 0, len(names))
	for _, name := range names {
		relation := state.reachable[name]
		dependencies := sortedRelationNames(relation.Dependencies)
		result = append(result, DependencyRef{
			Name: name, Kind: relation.Kind, ProductName: relation.ProductName,
			DefinitionDigest: relation.DefinitionDigest, Dependencies: dependencies,
			Root: name == state.root,
		})
	}
	return result
}

func digestInterface(outputs []Output) (string, error) {
	type publicOutput struct {
		Name             string `json:"name"`
		SQLType          string `json:"sql_type"`
		Collation        string `json:"collation,omitempty"`
		CollationVersion string `json:"collation_version,omitempty"`
	}
	payload := make([]publicOutput, 0, len(outputs))
	for _, output := range outputs {
		payload = append(payload, publicOutput{Name: output.Name, SQLType: output.SQLType,
			Collation: output.Collation, CollationVersion: output.CollationVersion})
	}
	return digestJSON("TASKGATE-VIEW-INTERFACE-V1\x00", payload)
}

func (state *compileState) digestDependencies(closure []DependencyRef) (string, error) {
	type closureNode struct {
		DependencyRef
		InterfaceDigest string `json:"interface_digest"`
	}
	nodes := make([]closureNode, 0, len(closure))
	for _, dependency := range closure {
		compiled, present := state.memo[dependency.Name]
		if !present {
			return "", fmt.Errorf("compiled dependency %q is absent", dependency.Name)
		}
		outputs := make([]Output, len(compiled.outputs))
		for index, output := range compiled.outputs {
			outputs[index] = output.Output
		}
		interfaceDigest, err := digestInterface(outputs)
		if err != nil {
			return "", err
		}
		nodes = append(nodes, closureNode{DependencyRef: dependency, InterfaceDigest: interfaceDigest})
	}
	payload := struct {
		PostgreSQLMajorVersion int           `json:"postgresql_major_version"`
		Nodes                  []closureNode `json:"nodes"`
	}{state.compiler.snapshot.PostgreSQLMajorVersion, nodes}
	return digestJSON("TASKGATE-VIEW-DEPENDENCY-V1\x00", payload)
}

func (state *compileState) digestBinding(planDigest, interfaceDigest, dependencyDigest string, closure []DependencyRef, baseProducts []string) (string, error) {
	type productBinding struct {
		Name             string   `json:"name"`
		SourceNamespace  string   `json:"source_namespace"`
		Snapshot         string   `json:"snapshot"`
		StableRole       string   `json:"stable_role"`
		LineageDigest    string   `json:"lineage_digest,omitempty"`
		StableEntityKey  []string `json:"stable_entity_key"`
		RequiredEvidence []string `json:"required_evidence,omitempty"`
	}
	bindings := make([]productBinding, 0, len(baseProducts))
	for _, name := range baseProducts {
		product := state.compiler.products[name]
		bindings = append(bindings, productBinding{
			Name: product.Name, SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot,
			StableRole: product.StableRole, LineageDigest: product.LineageDigest,
			StableEntityKey:  append([]string(nil), product.StableEntityKey...),
			RequiredEvidence: append([]string(nil), product.RequiredEvidence...),
		})
	}
	payload := struct {
		PlanDigest       string           `json:"plan_digest"`
		InterfaceDigest  string           `json:"interface_digest"`
		DependencyDigest string           `json:"dependency_digest"`
		Closure          []DependencyRef  `json:"dependency_closure"`
		Products         []productBinding `json:"products"`
	}{planDigest, interfaceDigest, dependencyDigest, closure, bindings}
	return digestJSON("TASKGATE-VIEW-BINDING-V1\x00", payload)
}

func digestJSON(domain string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(domain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func validateRelationName(name RelationName) error {
	if name.Name == "" || !relationIdentifier.MatchString(name.Name) {
		return errors.New("relation name must be a lowercase unquoted identifier")
	}
	if name.Schema != "" && !relationIdentifier.MatchString(name.Schema) {
		return errors.New("relation schema must be a lowercase unquoted identifier")
	}
	return nil
}

func validateDeclaredColumns(columns []Column) error {
	seen := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		if !catalogIdentifier.MatchString(column.Name) {
			return fmt.Errorf("column %q is not a lowercase QueryPlan identifier", column.Name)
		}
		if _, duplicate := seen[column.Name]; duplicate {
			return fmt.Errorf("column %q is duplicated", column.Name)
		}
		seen[column.Name] = struct{}{}
		typeName, err := exposure.CanonicalSQLTypeV2(column.SQLType)
		if err != nil {
			return fmt.Errorf("column %q has unsupported SQL type", column.Name)
		}
		collatable := typeName == "text" || typeName == "character" || typeName == "character varying"
		if (collatable && (column.Collation == "" || column.CollationVersion == "")) ||
			(!collatable && (column.Collation != "" || column.CollationVersion != "")) {
			return fmt.Errorf("column %q has an incomplete or inapplicable collation profile", column.Name)
		}
		if strings.TrimSpace(column.Collation) != column.Collation || strings.TrimSpace(column.CollationVersion) != column.CollationVersion ||
			strings.ContainsAny(column.Collation+column.CollationVersion, "\x00\r\n\t") {
			return fmt.Errorf("column %q has non-canonical collation metadata", column.Name)
		}
	}
	return nil
}

func sameColumnType(column Column, sqlType, collation, version string) error {
	left, leftErr := exposure.CanonicalSQLTypeV2(column.SQLType)
	right, rightErr := exposure.CanonicalSQLTypeV2(sqlType)
	if leftErr != nil || rightErr != nil || left != right {
		return fmt.Errorf("attested type %q does not equal product type %q", column.SQLType, sqlType)
	}
	if column.Collation != collation || column.CollationVersion != version {
		return errors.New("attested and product collation profiles differ")
	}
	return nil
}

func splitFieldID(value string) (string, string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || !catalogIdentifier.MatchString(parts[0]) || !catalogIdentifier.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func cloneProduct(product queryplan.Product) queryplan.Product {
	product.Columns = cloneSet(product.Columns)
	product.AllowedAggregates = cloneSet(product.AllowedAggregates)
	product.ColumnTypes = cloneMap(product.ColumnTypes)
	product.ColumnCollations = cloneMap(product.ColumnCollations)
	product.CollationVersions = cloneMap(product.CollationVersions)
	product.StableEntityKey = append([]string(nil), product.StableEntityKey...)
	product.RequiredEvidence = append([]string(nil), product.RequiredEvidence...)
	return product
}

func cloneSet(input map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(input))
	for key := range input {
		result[key] = struct{}{}
	}
	return result
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneFragment(input fragment) fragment {
	result := input
	result.sources = append([]queryplan.Scan(nil), input.sources...)
	result.predicates = append([]queryplan.JoinPredicate(nil), input.predicates...)
	result.filters = append([]queryplan.Filter(nil), input.filters...)
	result.groupBy = append([]string(nil), input.groupBy...)
	result.outputs = make([]outputBinding, len(input.outputs))
	for index, output := range input.outputs {
		result.outputs[index] = output
		if output.aggregate != nil {
			aggregate := *output.aggregate
			result.outputs[index].aggregate = &aggregate
		}
	}
	return result
}

func duplicateRelationNames(names []RelationName) bool {
	seen := make(map[RelationName]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return true
		}
		seen[name] = struct{}{}
	}
	return false
}

func sameRelationSet(left, right []RelationName) bool {
	if len(left) != len(right) || duplicateRelationNames(left) || duplicateRelationNames(right) {
		return false
	}
	set := make(map[RelationName]struct{}, len(left))
	for _, name := range left {
		set[name] = struct{}{}
	}
	for _, name := range right {
		if _, present := set[name]; !present {
			return false
		}
	}
	return true
}

func sortedRelationNames(input []RelationName) []RelationName {
	result := append([]RelationName(nil), input...)
	sortRelationNames(result)
	return result
}

func sortRelationNames(input []RelationName) {
	sort.Slice(input, func(i, j int) bool { return input[i].String() < input[j].String() })
}

func relationNamesText(input []RelationName) []string {
	names := sortedRelationNames(input)
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = name.String()
	}
	return result
}

func baseProductNames(sources []queryplan.Scan) []string {
	set := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		set[source.Product] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
