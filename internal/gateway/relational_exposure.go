package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

type relationalExposureContext struct {
	compilation queryplan.RelationalCompilation
	products    map[string]catalog.Product
}

func relationalQueryProduct(product catalog.Product, approved map[string]struct{}) queryplan.Product {
	types := make(map[string]string, len(product.Fields))
	collations := make(map[string]string, len(product.Fields))
	versions := make(map[string]string, len(product.Fields))
	for _, field := range product.Fields {
		types[field.Name] = field.Type
		collations[field.Name] = field.Collation
		versions[field.Name] = field.CollationVersion
	}
	aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
	for _, aggregate := range product.AllowedAggregates {
		aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
	}
	return queryplan.Product{
		Name: product.Name, Columns: approved, AllowedAggregates: aggregates,
		ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
		SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot,
		StableRole: product.StableRelationRole, StableEntityKey: append([]string(nil), product.EntityKey...),
		LineageDigest: product.LineageManifestDigest, RequiredEvidence: append([]string(nil), product.Scopes...),
	}
}

func buildRelationalExposureContext(plan queryplan.QueryPlan, compilation queryplan.RelationalCompilation, products map[string]catalog.Product, approved map[string][]string) (*planExposureContext, error) {
	if compilation.VisibleSQL == "" || compilation.ProvenanceSQL == "" || len(compilation.Sources) == 0 {
		return nil, errors.New("relational compilation is incomplete")
	}
	productList := make([]catalog.Product, 0, len(compilation.Products))
	metering := make(map[string][]string, len(compilation.Products))
	for _, name := range compilation.Products {
		product, present := products[name]
		if !present || product.FactNamespace == "" || product.Snapshot == "" || product.StableRelationRole == "" || len(product.EntityKey) == 0 {
			return nil, fmt.Errorf("V2 product %q lacks stable semantic metadata", name)
		}
		set := stringSetFromSlice(approved[name])
		for _, column := range append(append([]string(nil), product.EntityKey...), product.Scopes...) {
			if _, present := catalogFieldByName(product.Fields, column); !present {
				return nil, fmt.Errorf("metering field %q is absent from product %q", column, name)
			}
			set[column] = struct{}{}
		}
		for _, source := range compilation.Sources {
			if source.Product != name {
				continue
			}
			for _, field := range source.EvidenceFields {
				set[field] = struct{}{}
			}
		}
		metering[name] = sortedStringSet(set)
		productList = append(productList, product)
	}
	semanticProducts := make(map[string]queryplan.Product, len(products))
	for name, product := range products {
		semanticProducts[name] = relationalQueryProduct(product, stringSetFromSlice(product.FieldNames()))
	}
	normal, err := queryplan.SemanticNormalForm(plan, compilation, semanticProducts)
	if err != nil {
		return nil, err
	}
	context := &planExposureContext{
		products: productList, plan: cloneQueryPlan(plan), mainSQL: compilation.VisibleSQL,
		provenanceSQL: compilation.ProvenanceSQL, visibleFields: append([]string(nil), compilation.VisibleFields...),
		factFields: append([]string(nil), compilation.VisibleFields...), provenanceFields: append([]string(nil), compilation.ProvenanceFields...),
		grouped: len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0, expandedEvidence: compilation.ExpandedEvidence,
		meteringByProduct: metering, relational: &relationalExposureContext{compilation: compilation, products: products},
		algebraNormalForm: &normal, planDigest: normal.SHA256,
	}
	return context, nil
}

func relationalAlgebraPlan(plan queryplan.QueryPlan, compilation queryplan.RelationalCompilation, products map[string]catalog.Product) (queryplan.AlgebraPlanV2, error) {
	makeScan := func(source queryplan.RelationalSource, semanticRole string) (queryplan.AlgebraPlanV2, error) {
		product := products[source.Product]
		schema := make([]queryplan.AlgebraFieldV2, 0, len(source.EvidenceFields))
		for _, field := range source.EvidenceFields {
			definition, present := catalogFieldByName(product.Fields, field)
			if !present {
				return queryplan.AlgebraPlanV2{}, fmt.Errorf("evidence field %q is absent", field)
			}
			id := semanticRole + "." + field
			schema = append(schema, queryplan.AlgebraFieldV2{ID: id, SQLType: definition.Type, Collation: definition.Collation,
				CollationVersion: definition.CollationVersion, CollationDeterministic: definition.Collation != ""})
		}
		node := queryplan.AlgebraPlanV2{Op: "scan", SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot,
			StableRole: product.StableRelationRole, Schema: schema}
		if len(source.Filters) > 0 {
			predicates, err := normalizedAlgebraFilters(source.Filters, semanticRole, product)
			if err != nil {
				return queryplan.AlgebraPlanV2{}, err
			}
			input := node
			node = queryplan.AlgebraPlanV2{Op: "select", Input: &input, Predicates: predicates}
		}
		return node, nil
	}
	var node queryplan.AlgebraPlanV2
	switch compilation.Kind {
	case "scan":
		source := compilation.Sources[0]
		var err error
		node, err = makeScan(source, source.Role)
		if err != nil {
			return node, err
		}
	case "join":
		if len(compilation.Sources) < 2 {
			return node, errors.New("join compilation requires at least two sources")
		}
		left, err := makeScan(compilation.Sources[0], compilation.Sources[0].Role)
		if err != nil {
			return node, err
		}
		node = left
		joinedRoles := map[string]struct{}{compilation.Sources[0].Role: {}}
		usedPredicates := 0
		for _, source := range compilation.Sources[1:] {
			right, scanErr := makeScan(source, source.Role)
			if scanErr != nil {
				return node, scanErr
			}
			predicates := make([]queryplan.AlgebraJoinPredicateV2, 0)
			for _, predicate := range compilation.JoinPredicates {
				leftRole, _, leftOK := splitRelationalField(predicate.Left)
				rightRole, _, rightOK := splitRelationalField(predicate.Right)
				if !leftOK || !rightOK {
					return node, errors.New("join predicate contains an invalid field")
				}
				switch {
				case rightRole == source.Role:
					if _, present := joinedRoles[leftRole]; present {
						predicates = append(predicates, queryplan.AlgebraJoinPredicateV2{LeftField: predicate.Left, RightField: predicate.Right})
						usedPredicates++
					}
				case leftRole == source.Role:
					if _, present := joinedRoles[rightRole]; present {
						predicates = append(predicates, queryplan.AlgebraJoinPredicateV2{LeftField: predicate.Right, RightField: predicate.Left})
						usedPredicates++
					}
				}
			}
			if len(predicates) == 0 {
				return node, errors.New("join source is disconnected from the compiled component")
			}
			leftInput := node
			node = queryplan.AlgebraPlanV2{Op: "join", Left: &leftInput, Right: &right, JoinPredicates: predicates}
			joinedRoles[source.Role] = struct{}{}
		}
		if usedPredicates != len(compilation.JoinPredicates) {
			return node, errors.New("join graph contains an unbound equality")
		}
	case "union_distinct":
		role := plan.From.UnionDistinct.Role
		left, err := makeScan(compilation.Sources[0], role)
		if err != nil {
			return node, err
		}
		right, err := makeScan(compilation.Sources[1], role)
		if err != nil {
			return node, err
		}
		fields := make([]string, 0, len(compilation.UnionColumns))
		for _, field := range compilation.UnionColumns {
			fields = append(fields, role+"."+field)
		}
		leftInput, rightInput := left, right
		left = queryplan.AlgebraPlanV2{Op: "project", Input: &leftInput, Fields: fields}
		right = queryplan.AlgebraPlanV2{Op: "project", Input: &rightInput, Fields: fields}
		node = queryplan.AlgebraPlanV2{Op: "union", Left: &left, Right: &right}
	default:
		return node, errors.New("unknown relational operator")
	}
	if len(plan.Filters) > 0 {
		predicates, err := normalizedOuterFilters(plan.Filters, products, compilation)
		if err != nil {
			return node, err
		}
		input := node
		node = queryplan.AlgebraPlanV2{Op: "select", Input: &input, Predicates: predicates}
	}
	if len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0 {
		aggregates := make([]queryplan.AlgebraAggregateV2, 0, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			outputType, err := relationalAggregateType(aggregate, products, compilation)
			if err != nil {
				return node, err
			}
			aggregates = append(aggregates, queryplan.AlgebraAggregateV2{Function: strings.ToLower(aggregate.Function), Field: aggregate.Column, OutputType: outputType})
		}
		input := node
		node = queryplan.AlgebraPlanV2{Op: "group", Input: &input, GroupBy: append([]string(nil), plan.GroupBy...), Aggregates: aggregates}
	}
	fields := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		fields = append(fields, strings.ToLower(strings.TrimSpace(aggregate.Function))+"("+aggregate.Column+")")
	}
	if len(fields) > 0 {
		input := node
		node = queryplan.AlgebraPlanV2{Op: "project", Input: &input, Fields: fields}
	}
	return node, nil
}

func normalizedAlgebraFilters(filters []queryplan.Filter, role string, product catalog.Product) ([]queryplan.NormalizedFilter, error) {
	result := make([]queryplan.NormalizedFilter, 0, len(filters))
	for _, filter := range filters {
		definition, present := catalogFieldByName(product.Fields, filter.Column)
		if !present {
			return nil, fmt.Errorf("filter field %q is absent", filter.Column)
		}
		value, err := json.Marshal(filter.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, queryplan.NormalizedFilter{Column: role + "." + filter.Column, SQLType: definition.Type, Op: filter.Op, Value: value})
	}
	return result, nil
}

func normalizedOuterFilters(filters []queryplan.Filter, products map[string]catalog.Product, compilation queryplan.RelationalCompilation) ([]queryplan.NormalizedFilter, error) {
	result := make([]queryplan.NormalizedFilter, 0, len(filters))
	for _, filter := range filters {
		role, column, ok := splitRelationalField(filter.Column)
		if !ok {
			return nil, fmt.Errorf("invalid field %q", filter.Column)
		}
		var definition catalog.Field
		found := false
		for _, source := range compilation.Sources {
			semantic := source.Role
			if compilation.Kind == "union_distinct" {
				semantic = strings.Split(filter.Column, ".")[0]
			}
			if semantic != role {
				continue
			}
			definition, found = catalogFieldByName(products[source.Product].Fields, column)
			if found {
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("filter field %q is absent", filter.Column)
		}
		value, err := json.Marshal(filter.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, queryplan.NormalizedFilter{Column: filter.Column, SQLType: definition.Type, Op: filter.Op, Value: value})
	}
	return result, nil
}

func relationalAggregateType(aggregate queryplan.Aggregate, products map[string]catalog.Product, compilation queryplan.RelationalCompilation) (string, error) {
	fn := strings.ToLower(strings.TrimSpace(aggregate.Function))
	if aggregate.Column == "*" {
		if fn == "count" {
			return "bigint", nil
		}
		return "", errors.New("only count accepts star")
	}
	role, column, ok := splitRelationalField(aggregate.Column)
	if !ok {
		return "", errors.New("aggregate field is invalid")
	}
	for _, source := range compilation.Sources {
		semantic := source.Role
		if compilation.Kind == "union_distinct" {
			semantic = role
		}
		if semantic != role {
			continue
		}
		field, present := catalogFieldByName(products[source.Product].Fields, column)
		if present {
			return aggregateSQLType(fn, field.Type), nil
		}
	}
	return "", errors.New("aggregate field is absent")
}

func (context *planExposureContext) deriveRelationalObservationV2(visible, provenance dataconnector.Result) (exposure.Observation, error) {
	if context.algebraNormalForm == nil || context.planDigest == "" {
		return exposure.Observation{}, errors.New("relational V2 query has no algebra normal form")
	}
	if provenance.Truncated {
		return exposure.Observation{}, errProvenanceTruncated
	}
	visible, err := context.relational.canonicalVisibleResult(visible)
	if err != nil {
		return exposure.Observation{}, err
	}
	positions, err := columnPositions(provenance.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	compilation := context.relational.compilation
	relations := make([]exposure.RelationV2, 0, len(compilation.Sources))
	for index, source := range compilation.Sources {
		semanticRole := source.Role
		branch := -1
		if compilation.Kind == "union_distinct" {
			semanticRole = context.plan.From.UnionDistinct.Role
			branch = index
		}
		relation, scanErr := context.scanRelationV2(provenance, positions, source, semanticRole, branch)
		if scanErr != nil {
			return exposure.Observation{}, scanErr
		}
		relations = append(relations, relation)
	}
	var relation exposure.RelationV2
	switch compilation.Kind {
	case "scan":
		relation = relations[0]
	case "join":
		if len(relations) < 2 {
			return exposure.Observation{}, errors.New("join observation requires at least two source relations")
		}
		relation = relations[0]
		joinedRoles := map[string]struct{}{compilation.Sources[0].Role: {}}
		usedPredicates := 0
		for sourceIndex := 1; sourceIndex < len(compilation.Sources); sourceIndex++ {
			source := compilation.Sources[sourceIndex]
			predicates := make([]exposure.JoinPredicateV2, 0)
			for _, predicate := range compilation.JoinPredicates {
				leftRole, _, leftOK := splitRelationalField(predicate.Left)
				rightRole, _, rightOK := splitRelationalField(predicate.Right)
				if !leftOK || !rightOK {
					return exposure.Observation{}, errors.New("join predicate contains an invalid field")
				}
				switch {
				case rightRole == source.Role:
					if _, present := joinedRoles[leftRole]; present {
						predicates = append(predicates, exposure.JoinPredicateV2{LeftField: predicate.Left, RightField: predicate.Right})
						usedPredicates++
					}
				case leftRole == source.Role:
					if _, present := joinedRoles[rightRole]; present {
						predicates = append(predicates, exposure.JoinPredicateV2{LeftField: predicate.Right, RightField: predicate.Left})
						usedPredicates++
					}
				}
			}
			if len(predicates) == 0 {
				return exposure.Observation{}, errors.New("join source is disconnected from the observed component")
			}
			relation, err = exposure.JoinOnV2(relation, relations[sourceIndex], predicates)
			if err != nil {
				return exposure.Observation{}, err
			}
			joinedRoles[source.Role] = struct{}{}
		}
		if usedPredicates != len(compilation.JoinPredicates) {
			return exposure.Observation{}, errors.New("join graph contains an unbound equality")
		}
		allowedPairs, allowedErr := context.provenanceJoinPairs(provenance, positions)
		if allowedErr != nil {
			return exposure.Observation{}, allowedErr
		}
		relation, err = exposure.SelectV2(relation, nil, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if _, ok := allowedPairs[originPairKey(row.Origins)]; ok {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if err != nil {
			return exposure.Observation{}, err
		}
	case "union_distinct":
		relation, err = exposure.UnionDistinctV2(relations[0], relations[1])
		if err != nil {
			return exposure.Observation{}, err
		}
	default:
		return exposure.Observation{}, errors.New("unknown relational operator")
	}
	if len(context.plan.Filters) > 0 {
		fields := make([]string, 0, len(context.plan.Filters))
		for _, filter := range context.plan.Filters {
			fields = append(fields, filter.Column)
		}
		relation, err = exposure.SelectV2(relation, uniqueStrings(fields), func(exposure.AnnotatedRowV2) exposure.SQLTruth { return exposure.SQLTrue })
		if err != nil {
			return exposure.Observation{}, err
		}
	}
	visiblePositions, err := columnPositions(visible.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	if !context.grouped {
		if compilation.Kind == "union_distinct" {
			relation, err = restrictRelationToVisibleTuples(relation, visible, visiblePositions, compilation.InternalFields)
			if err != nil {
				return exposure.Observation{}, err
			}
		}
		if err := visibleMatchesRelationV2(visible, visiblePositions, relation, compilation.InternalFields); err != nil {
			return exposure.Observation{}, err
		}
		return exposure.ObserveV2(relation, context.visibleFields...)
	}
	if len(context.plan.GroupBy) > 0 {
		relation, err = restrictRelationToVisibleGroups(relation, visible, visiblePositions, context.plan.GroupBy)
		if err != nil {
			return exposure.Observation{}, err
		}
	}
	outputs := make([]map[string]any, 0, len(visible.Rows))
	for _, row := range visible.Rows {
		output := make(map[string]any, len(context.visibleFields)+len(context.plan.GroupBy))
		for _, field := range append(append([]string(nil), context.visibleFields...), context.plan.GroupBy...) {
			position, present := visiblePositions[field]
			if !present || position >= len(row) {
				return exposure.Observation{}, fmt.Errorf("visible result misses %q", field)
			}
			output[field] = row[position]
		}
		outputs = append(outputs, output)
	}
	types := relationFieldTypes(relation)
	specs := make([]exposure.AggregateSpecV2, 0, len(context.plan.Aggregates))
	for _, aggregate := range context.plan.Aggregates {
		specs = append(specs, exposure.AggregateSpecV2{Function: strings.ToLower(aggregate.Function), Field: aggregate.Column, OutputID: aggregate.Alias, OutputType: aggregateSQLType(strings.ToLower(aggregate.Function), types[aggregate.Column])})
	}
	aggregated, err := exposure.AggregateFromResultsV2(relation, context.plan.GroupBy, specs, outputs)
	if err != nil {
		return exposure.Observation{}, err
	}
	return exposure.ObserveV2(aggregated, context.visibleFields...)
}

func (context *relationalExposureContext) canonicalVisibleResult(result dataconnector.Result) (dataconnector.Result, error) {
	reverse := make(map[string]string, len(context.compilation.OutputAliases))
	for semantic, alias := range context.compilation.OutputAliases {
		if previous, duplicate := reverse[alias]; duplicate && previous != semantic {
			return dataconnector.Result{}, errors.New("relational output alias collision")
		}
		reverse[alias] = semantic
	}
	canonical := result
	canonical.Columns = append([]dataconnector.Column(nil), result.Columns...)
	semanticSet := stringSetFromSlice(context.compilation.InternalFields)
	for index := range canonical.Columns {
		if _, alreadyCanonical := semanticSet[canonical.Columns[index].Name]; alreadyCanonical {
			continue
		}
		semantic, present := reverse[canonical.Columns[index].Name]
		if !present {
			return dataconnector.Result{}, fmt.Errorf("unexpected relational output column %q", canonical.Columns[index].Name)
		}
		canonical.Columns[index].Name = semantic
	}
	return canonical, nil
}

func (context *planExposureContext) scanRelationV2(provenance dataconnector.Result, positions map[string]int, source queryplan.RelationalSource, semanticRole string, branch int) (exposure.RelationV2, error) {
	product := context.relational.products[source.Product]
	fields := make([]exposure.FieldV2, 0, len(source.EvidenceFields))
	types := make(map[string]string, len(source.EvidenceFields))
	for _, name := range source.EvidenceFields {
		definition, present := catalogFieldByName(product.Fields, name)
		if !present {
			return exposure.RelationV2{}, fmt.Errorf("evidence field %q is absent", name)
		}
		typeName, err := exposure.CanonicalSQLTypeV2(definition.Type)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		id := semanticRole + "." + name
		types[name] = typeName
		fields = append(fields, exposure.FieldV2{ID: id, SQLType: typeName, Collation: definition.Collation, CollationVersion: definition.CollationVersion, CollationDeterministic: definition.Collation != ""})
	}
	rowsByKey := make(map[string]exposure.BaseRowV2)
	signatures := make(map[string]string)
	for _, row := range provenance.Rows {
		if branch >= 0 {
			value, err := branchNumber(row, positions["tg_branch"])
			if err != nil {
				return exposure.RelationV2{}, err
			}
			if value != branch {
				continue
			}
		}
		components := []string{product.FactNamespace}
		for _, keyField := range product.EntityKey {
			position, present := positions[source.EvidenceAlias[keyField]]
			if !present || position >= len(row) {
				return exposure.RelationV2{}, fmt.Errorf("provenance misses entity key %q", keyField)
			}
			canonical, err := exposure.CanonicalSQLValue(types[keyField], row[position])
			if err != nil {
				return exposure.RelationV2{}, err
			}
			components = append(components, keyField, types[keyField], canonical)
		}
		key, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		values := make(map[string]any, len(source.EvidenceFields))
		signatureParts := make([]string, 0, len(source.EvidenceFields)*2)
		for _, name := range source.EvidenceFields {
			position, present := positions[source.EvidenceAlias[name]]
			if !present || position >= len(row) {
				return exposure.RelationV2{}, fmt.Errorf("provenance misses field %q", name)
			}
			id := semanticRole + "." + name
			values[id] = row[position]
			canonical, err := exposure.CanonicalSQLValue(types[name], row[position])
			if err != nil {
				return exposure.RelationV2{}, err
			}
			signatureParts = append(signatureParts, id+"\x00"+canonical)
		}
		sort.Strings(signatureParts)
		signature := strings.Join(signatureParts, "\x00")
		if previous, present := signatures[key]; present && previous != signature {
			return exposure.RelationV2{}, errors.New("one entity key has inconsistent provenance values")
		}
		signatures[key] = signature
		rowsByKey[key] = exposure.BaseRowV2{EntityKey: key, Values: values}
	}
	keys := make([]string, 0, len(rowsByKey))
	for key := range rowsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]exposure.BaseRowV2, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, rowsByKey[key])
	}
	relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot, StableRole: product.StableRelationRole, Fields: fields, Rows: rows})
	if err != nil {
		return exposure.RelationV2{}, err
	}
	if len(source.Filters) > 0 {
		predicateFields := make([]string, 0, len(source.Filters))
		for _, filter := range source.Filters {
			predicateFields = append(predicateFields, semanticRole+"."+filter.Column)
		}
		relation, err = exposure.SelectV2(relation, uniqueStrings(predicateFields), func(exposure.AnnotatedRowV2) exposure.SQLTruth { return exposure.SQLTrue })
		if err != nil {
			return exposure.RelationV2{}, err
		}
	}
	if branch >= 0 {
		project := make([]string, 0, len(context.relational.compilation.UnionColumns))
		for _, field := range context.relational.compilation.UnionColumns {
			project = append(project, semanticRole+"."+field)
		}
		relation, err = exposure.ProjectV2(relation, project...)
	}
	return relation, err
}

func (context *planExposureContext) provenanceJoinPairs(provenance dataconnector.Result, positions map[string]int) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(provenance.Rows))
	for _, row := range provenance.Rows {
		origins := make([]exposure.RowOriginV2, 0, len(context.relational.compilation.Sources))
		for _, source := range context.relational.compilation.Sources {
			product := context.relational.products[source.Product]
			types := catalogTypeMap(product)
			components := []string{product.FactNamespace}
			for _, field := range product.EntityKey {
				position, present := positions[source.EvidenceAlias[field]]
				if !present || position >= len(row) {
					return nil, fmt.Errorf("join provenance misses key %q", field)
				}
				canonical, err := exposure.CanonicalSQLValue(types[field], row[position])
				if err != nil {
					return nil, err
				}
				components = append(components, field, types[field], canonical)
			}
			key, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
			if err != nil {
				return nil, err
			}
			origins = append(origins, exposure.RowOriginV2{StableRole: product.StableRelationRole, SourceNamespace: product.FactNamespace, EntityKey: key})
		}
		result[originPairKey(origins)] = struct{}{}
	}
	return result, nil
}

func originPairKey(origins []exposure.RowOriginV2) string {
	parts := make([]string, 0, len(origins))
	for _, origin := range origins {
		parts = append(parts, origin.StableRole+"\x00"+origin.SourceNamespace+"\x00"+origin.EntityKey)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}
func branchNumber(row []any, position int) (int, error) {
	if position < 0 || position >= len(row) {
		return 0, errors.New("union provenance misses branch marker")
	}
	switch value := row[position].(type) {
	case int:
		return value, nil
	case int16:
		return int(value), nil
	case int32:
		return int(value), nil
	case int64:
		return int(value), nil
	case json.Number:
		number, err := strconv.Atoi(string(value))
		return number, err
	case string:
		number, err := strconv.Atoi(value)
		return number, err
	default:
		return 0, fmt.Errorf("invalid union branch marker %T", row[position])
	}
}

func restrictRelationToVisibleTuples(relation exposure.RelationV2, visible dataconnector.Result, positions map[string]int, fields []string) (exposure.RelationV2, error) {
	types := relationFieldTypes(relation)
	allowed := make(map[string]struct{}, len(visible.Rows))
	for _, row := range visible.Rows {
		key, err := typedTupleKey(fields, row, positions, types)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		allowed[key] = struct{}{}
	}
	return exposure.SelectV2(relation, fields, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
		key, err := annotatedTupleKey(fields, row, types)
		if err == nil {
			if _, ok := allowed[key]; ok {
				return exposure.SQLTrue
			}
		}
		return exposure.SQLFalse
	})
}
func restrictRelationToVisibleGroups(relation exposure.RelationV2, visible dataconnector.Result, positions map[string]int, fields []string) (exposure.RelationV2, error) {
	types := relationFieldTypes(relation)
	allowed := make(map[string]struct{}, len(visible.Rows))
	for _, row := range visible.Rows {
		key, err := typedTupleKey(fields, row, positions, types)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		allowed[key] = struct{}{}
	}
	return exposure.SelectV2(relation, fields, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
		key, err := annotatedTupleKey(fields, row, types)
		if err == nil {
			if _, ok := allowed[key]; ok {
				return exposure.SQLTrue
			}
		}
		return exposure.SQLFalse
	})
}

func visibleMatchesRelationV2(visible dataconnector.Result, positions map[string]int, relation exposure.RelationV2, fields []string) error {
	if len(visible.Rows) != len(relation.Rows) {
		return fmt.Errorf("visible rows %d differ from annotated rows %d", len(visible.Rows), len(relation.Rows))
	}
	types := relationFieldTypes(relation)
	actual := make(map[string]int)
	for _, row := range visible.Rows {
		key, err := typedTupleKey(fields, row, positions, types)
		if err != nil {
			return err
		}
		actual[key]++
	}
	for _, row := range relation.Rows {
		key, err := annotatedTupleKey(fields, row, types)
		if err != nil {
			return err
		}
		actual[key]--
		if actual[key] < 0 {
			return errors.New("annotated tuple is absent from visible result")
		}
	}
	for _, count := range actual {
		if count != 0 {
			return errors.New("visible tuple is absent from annotated relation")
		}
	}
	return nil
}
func typedTupleKey(fields []string, row []any, positions map[string]int, types map[string]string) (string, error) {
	components := make([]string, 0, len(fields)*2)
	for _, field := range fields {
		position, present := positions[field]
		if !present || position >= len(row) {
			return "", fmt.Errorf("visible result misses %q", field)
		}
		canonical, err := exposure.CanonicalSQLValue(types[field], row[position])
		if err != nil {
			return "", err
		}
		components = append(components, field, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("online-tuple", components...)
}
func annotatedTupleKey(fields []string, row exposure.AnnotatedRowV2, types map[string]string) (string, error) {
	components := make([]string, 0, len(fields)*2)
	for _, field := range fields {
		cell, present := row.Cells[field]
		if !present {
			return "", fmt.Errorf("annotated row misses %q", field)
		}
		canonical, err := exposure.CanonicalSQLValue(types[field], cell.Value)
		if err != nil {
			return "", err
		}
		components = append(components, field, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("online-tuple", components...)
}
func relationFieldTypes(relation exposure.RelationV2) map[string]string {
	result := make(map[string]string, len(relation.Fields))
	for _, field := range relation.Fields {
		result[field.ID] = field.SQLType
	}
	return result
}
func catalogTypeMap(product catalog.Product) map[string]string {
	result := make(map[string]string, len(product.Fields))
	for _, field := range product.Fields {
		typeName, _ := exposure.CanonicalSQLTypeV2(field.Type)
		result[field.Name] = typeName
	}
	return result
}
func splitRelationalField(value string) (string, string, bool) {
	parts := strings.Split(value, ".")
	return func() (string, string, bool) {
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", false
		}
		return parts[0], parts[1], true
	}()
}
