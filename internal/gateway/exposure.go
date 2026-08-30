package gateway

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

var errProvenanceTruncated = errors.New("provenance result exceeded the connector row ceiling")

// planExposureContext is the Gateway's half of one prepared operation.
//
// Every member below is read off a sealed physicalquery.PreparedOperation
// rather than derived here -- see planExposureContextFrom. What remains in this
// package is the half that cannot be a function of the preparation inputs:
// decoding Connector results, building an exposure Observation, holding the live
// SnapshotIndex handles, and shaping the response. Preparation owns what is
// compiled and authorized; this owns what is observed.
type planExposureContext struct {
	product          catalog.Product
	plan             queryplan.QueryPlan
	mainSQL          string
	provenanceSQL    string
	visibleFields    []string
	factFields       []string
	provenanceFields []string
	grouped          bool
	expandedEvidence bool

	viewBindingDigest    string
	viewRegistryRevision string
	relational           *relationalExposureContext
	planDigest           string
	ordinal              *boundOrdinalExecution
	predicateFootprint   *queryplan.PredicateFootprint

	// prepared is the sealed preparation this context was built from, and
	// policyGrant the authorization it produced.
	//
	// The grant is carried rather than recomputed by widening the task's own: the
	// metering closure and the ordinal sidecars that widen it are preparation's
	// output, and a second widening here would be the duplicate derivation the
	// extraction removes. The preparation itself travels because the Query
	// Execution Binding V2 signs its binding whole.
	prepared    physicalquery.PreparedOperation
	policyGrant sqlpolicy.Grant
}

func clonePolicyGrant(input sqlpolicy.Grant) sqlpolicy.Grant {
	result := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, len(input.Products))}
	for index, product := range input.Products {
		result.Products[index] = clonePolicyProductGrant(product)
	}
	return result
}

func clonePolicyProductGrant(input sqlpolicy.ProductGrant) sqlpolicy.ProductGrant {
	result := input
	result.ApprovedColumns = append([]string(nil), input.ApprovedColumns...)
	result.AllowedFunctions = append([]string(nil), input.AllowedFunctions...)
	result.AllowedAggregates = append([]string(nil), input.AllowedAggregates...)
	result.AllowedOperators = append([]string(nil), input.AllowedOperators...)
	result.MandatoryScope = make([]sqlpolicy.ScopePredicate, len(input.MandatoryScope))
	for index, predicate := range input.MandatoryScope {
		result.MandatoryScope[index] = predicate
		result.MandatoryScope[index].Values = append([]string(nil), predicate.Values...)
	}
	return result
}

func (context *planExposureContext) deriveObservation(visible, provenance dataconnector.Result, profile string) (exposure.Observation, error) {
	if context.relational != nil {
		if profile != exposure.ProfileV2 && profile != exposure.ProfileV3 && profile != exposure.ProfileV4 {
			return exposure.Observation{}, fmt.Errorf("online Join/Union requires %s, %s, or %s", exposure.ProfileV2, exposure.ProfileV3, exposure.ProfileV4)
		}
		observation, err := context.deriveRelationalObservationV2(visible, provenance)
		if err != nil || profile == exposure.ProfileV2 {
			return observation, err
		}
		if profile == exposure.ProfileV4 {
			return exposure.AttachOutcomeV4(observation, queryplan.NormalFormVersion, context.planDigest, int64(len(visible.Rows)))
		}
		return exposure.AttachOutcomeV3(observation, queryplan.NormalFormVersion, context.planDigest, int64(len(visible.Rows)))
	}
	if profile == exposure.ProfileV2 || profile == exposure.ProfileV3 || profile == exposure.ProfileV4 {
		observation, err := context.deriveObservationV2(visible, provenance)
		if err != nil || profile == exposure.ProfileV2 {
			return observation, err
		}
		if profile == exposure.ProfileV4 {
			return exposure.AttachOutcomeV4(observation, queryplan.NormalFormVersion, context.planDigest, int64(len(visible.Rows)))
		}
		return exposure.AttachOutcomeV3(observation, queryplan.NormalFormVersion, context.planDigest, int64(len(visible.Rows)))
	}
	if provenance.Truncated {
		return exposure.Observation{}, errProvenanceTruncated
	}
	positions, err := columnPositions(provenance.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	baseRows := make([]exposure.BaseRow, 0, len(provenance.Rows))
	provenanceKeys := make(map[string]struct{}, len(provenance.Rows))
	for _, values := range provenance.Rows {
		keyValues, err := valuesForFields(values, positions, context.product.EntityKey)
		if err != nil {
			return exposure.Observation{}, err
		}
		key, err := exposure.ComposeKey(keyValues...)
		if err != nil {
			return exposure.Observation{}, err
		}
		rowValues := make(map[string]any, len(context.provenanceFields))
		for _, field := range context.provenanceFields {
			if positions[field] >= len(values) {
				return exposure.Observation{}, fmt.Errorf("provenance row is shorter than its metadata")
			}
			rowValues[field] = values[positions[field]]
		}
		provenanceKeys[key] = struct{}{}
		baseRows = append(baseRows, exposure.BaseRow{EntityKey: key, Values: rowValues})
	}
	relation, err := exposure.NewBaseRelation(context.product.Name, context.product.Snapshot, context.provenanceFields, baseRows)
	if err != nil {
		return exposure.Observation{}, err
	}
	visiblePositions, err := columnPositions(visible.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	groupKeys := make(map[string]struct{})
	if context.grouped && len(context.plan.GroupBy) > 0 {
		for _, row := range visible.Rows {
			values, err := valuesForFields(row, visiblePositions, context.plan.GroupBy)
			if err != nil {
				return exposure.Observation{}, err
			}
			key, err := exposure.ComposeKey(values...)
			if err != nil {
				return exposure.Observation{}, err
			}
			groupKeys[key] = struct{}{}
		}
		relation, err = exposure.Select(relation, context.plan.GroupBy, func(row exposure.Row) bool {
			values := make([]any, 0, len(context.plan.GroupBy))
			for _, field := range context.plan.GroupBy {
				values = append(values, row.Cells[field].Value)
			}
			key, keyErr := exposure.ComposeKey(values...)
			_, keep := groupKeys[key]
			return keyErr == nil && keep
		})
		if err != nil {
			return exposure.Observation{}, err
		}
	}
	sourceObservation, err := exposure.Observe(profile, relation, context.provenanceFields...)
	if err != nil {
		return exposure.Observation{}, err
	}

	release, err := exposure.NewFactSet()
	if err != nil {
		return exposure.Observation{}, err
	}
	groupSources, err := sourceHashesByGroupAndField(relation, context.plan.GroupBy)
	if err != nil {
		return exposure.Observation{}, err
	}
	if !context.grouped && len(visible.Rows) != len(provenance.Rows) {
		return exposure.Observation{}, fmt.Errorf("visible and provenance row sets differ")
	}
	for _, row := range visible.Rows {
		var entityKey string
		groupKey := ""
		if context.grouped {
			groupValues, err := valuesForFields(row, visiblePositions, context.plan.GroupBy)
			if err != nil {
				return exposure.Observation{}, err
			}
			groupKey = composeExposureGroupKey(groupValues)
			if len(context.plan.GroupBy) > 0 {
				if _, present := groupSources[groupKey]; !present {
					return exposure.Observation{}, fmt.Errorf("visible group is absent from provenance result")
				}
			}
			entityKey = groupKey
		} else {
			keyValues, err := valuesForFields(row, visiblePositions, context.product.EntityKey)
			if err != nil {
				return exposure.Observation{}, err
			}
			entityKey, err = exposure.ComposeKey(keyValues...)
			if err != nil {
				return exposure.Observation{}, err
			}
			if _, present := provenanceKeys[entityKey]; !present {
				return exposure.Observation{}, fmt.Errorf("visible row is absent from provenance result")
			}
		}
		for index, field := range context.visibleFields {
			position, ok := visiblePositions[field]
			if !ok || position >= len(row) {
				return exposure.Observation{}, fmt.Errorf("visible result is missing field %q", field)
			}
			var fact exposure.FactID
			if context.grouped && index >= len(context.plan.Columns) {
				aggregate := context.plan.Aggregates[index-len(context.plan.Columns)]
				sourceHashes := groupSources[groupKey][aggregate.Column]
				version, err := exposure.ValueVersion(map[string]any{"value": row[position], "sources": sourceHashes})
				if err != nil {
					return exposure.Observation{}, err
				}
				fact = exposure.FactID{Product: context.product.Name, Snapshot: context.product.Snapshot,
					EntityKey: entityKey, Field: context.factFields[index], ValueVersion: version}
				if err := fact.Validate(); err != nil {
					return exposure.Observation{}, err
				}
			} else {
				var err error
				fact, err = exposure.NewFact(context.product.Name, context.product.Snapshot, entityKey, context.factFields[index], row[position])
				if err != nil {
					return exposure.Observation{}, err
				}
			}
			if err := release.Add(fact); err != nil {
				return exposure.Observation{}, err
			}
		}
	}
	releaseValues, err := release.Values()
	if err != nil {
		return exposure.Observation{}, err
	}
	return (exposure.Observation{ProfileVersion: profile, Release: releaseValues, Influence: sourceObservation.Influence}).Normalize()
}

func (context *planExposureContext) deriveObservationV2(visible, provenance dataconnector.Result) (exposure.Observation, error) {
	if context.planDigest == "" {
		return exposure.Observation{}, fmt.Errorf("V2 query context has no canonical plan identity")
	}
	if provenance.Truncated {
		return exposure.Observation{}, errProvenanceTruncated
	}
	positions, err := columnPositions(provenance.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	types := make(map[string]string, len(context.product.Fields))
	for _, field := range context.product.Fields {
		types[field.Name] = field.Type
	}
	fields := make([]exposure.FieldV2, 0, len(context.provenanceFields))
	for _, field := range context.provenanceFields {
		if types[field] == "" {
			return exposure.Observation{}, fmt.Errorf("V2 provenance field %q has no catalog type", field)
		}
		catalogField, present := catalogFieldByName(context.product.Fields, field)
		if !present {
			return exposure.Observation{}, fmt.Errorf("V2 provenance field %q is absent from the Catalog", field)
		}
		fields = append(fields, exposure.FieldV2{ID: field, SQLType: types[field], Collation: catalogField.Collation,
			CollationVersion: catalogField.CollationVersion, CollationDeterministic: catalogField.Collation != ""})
	}
	baseRows := make([]exposure.BaseRowV2, 0, len(provenance.Rows))
	provenanceKeys := make(map[string]struct{}, len(provenance.Rows))
	for _, values := range provenance.Rows {
		key, err := context.baseEntityKeyV2(values, positions, types)
		if err != nil {
			return exposure.Observation{}, err
		}
		rowValues := make(map[string]any, len(context.provenanceFields))
		for _, field := range context.provenanceFields {
			if positions[field] >= len(values) {
				return exposure.Observation{}, fmt.Errorf("provenance row is shorter than its metadata")
			}
			rowValues[field] = values[positions[field]]
		}
		if _, duplicate := provenanceKeys[key]; duplicate {
			return exposure.Observation{}, fmt.Errorf("V2 provenance contains a duplicate stable entity key")
		}
		provenanceKeys[key] = struct{}{}
		baseRows = append(baseRows, exposure.BaseRowV2{EntityKey: key, Values: rowValues})
	}
	relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: context.product.FactNamespace,
		Snapshot: context.product.Snapshot, StableRole: context.product.StableRelationRole, Fields: fields, Rows: baseRows})
	if err != nil {
		return exposure.Observation{}, err
	}
	predicateFields := make([]string, 0, len(context.plan.Filters))
	for _, filter := range context.plan.Filters {
		predicateFields = append(predicateFields, filter.Column)
	}
	predicateFields = uniqueStrings(predicateFields)
	if len(predicateFields) > 0 {
		relation, err = exposure.SelectV2(relation, predicateFields, func(exposure.AnnotatedRowV2) exposure.SQLTruth { return exposure.SQLTrue })
		if err != nil {
			return exposure.Observation{}, err
		}
	}
	visiblePositions, err := columnPositions(visible.Columns)
	if err != nil {
		return exposure.Observation{}, err
	}
	if !context.grouped {
		if len(visible.Rows) != len(provenance.Rows) {
			return exposure.Observation{}, fmt.Errorf("visible and provenance row sets differ")
		}
		visibleKeys := make(map[string]struct{}, len(visible.Rows))
		for _, values := range visible.Rows {
			key, err := context.baseEntityKeyV2(values, visiblePositions, types)
			if err != nil {
				return exposure.Observation{}, err
			}
			visibleKeys[key] = struct{}{}
		}
		for key := range provenanceKeys {
			if _, present := visibleKeys[key]; !present {
				return exposure.Observation{}, fmt.Errorf("visible and provenance entity sets differ")
			}
		}
		return exposure.ObserveV2(relation, context.visibleFields...)
	}

	// The companion query returns every positive source row. Restrict it to
	// groups actually returned by a paged aggregate before constructing effects.
	groupKeys := make(map[string]struct{}, len(visible.Rows))
	for _, row := range visible.Rows {
		key, err := typedGroupKeyV2(context.plan.GroupBy, row, visiblePositions, types, context.product.FactNamespace)
		if err != nil {
			return exposure.Observation{}, err
		}
		groupKeys[key] = struct{}{}
	}
	if len(context.plan.GroupBy) > 0 {
		relation, err = exposure.SelectV2(relation, context.plan.GroupBy, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			key, keyErr := annotatedGroupKeyV2(context.plan.GroupBy, row, types, context.product.FactNamespace)
			if keyErr == nil {
				if _, present := groupKeys[key]; present {
					return exposure.SQLTrue
				}
			}
			return exposure.SQLFalse
		})
		if err != nil {
			return exposure.Observation{}, err
		}
	}
	outputs := make([]map[string]any, 0, len(visible.Rows))
	for _, row := range visible.Rows {
		output := make(map[string]any, len(context.visibleFields)+len(context.plan.GroupBy))
		for _, field := range context.visibleFields {
			position, present := visiblePositions[field]
			if !present || position >= len(row) {
				return exposure.Observation{}, fmt.Errorf("visible result is missing field %q", field)
			}
			output[field] = row[position]
		}
		for _, field := range context.plan.GroupBy {
			position, present := visiblePositions[field]
			if !present || position >= len(row) {
				return exposure.Observation{}, fmt.Errorf("visible result is missing hidden group field %q", field)
			}
			output[field] = row[position]
		}
		outputs = append(outputs, output)
	}
	specs := make([]exposure.AggregateSpecV2, 0, len(context.plan.Aggregates))
	for _, aggregate := range context.plan.Aggregates {
		function := strings.ToLower(aggregate.Function)
		outputType := aggregateSQLType(function, types[aggregate.Column])
		specs = append(specs, exposure.AggregateSpecV2{Function: function, Field: aggregate.Column,
			OutputID: aggregate.Alias, OutputType: outputType, Distinct: aggregate.Distinct})
	}
	aggregated, err := exposure.AggregateFromResultsV2(relation, context.plan.GroupBy, specs, outputs)
	if err != nil {
		return exposure.Observation{}, err
	}
	aggregated, err = applyHavingV2(aggregated, context.plan)
	if err != nil {
		return exposure.Observation{}, err
	}
	return exposure.ObserveV2(aggregated, context.visibleFields...)
}

// usesExpandedEvidence is asked of the sealed preparation, not recomputed from
// the members copied off it.
//
// The disjunction used to be written here as well as inside the receipt's
// binding, and the two spellings disagreed: this one combined grouped with
// expanded evidence, the binding read expanded evidence alone. Production
// derived its limits by this rule and then signed a binding whose own
// arithmetic rejected them -- which only showed up once every profile began
// signing one.
func (context *planExposureContext) usesExpandedEvidence() bool {
	return context.prepared.Binding().UsesExpandedEvidence()
}

func catalogFieldByName(fields []catalog.Field, name string) (catalog.Field, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return catalog.Field{}, false
}

func (context *planExposureContext) baseEntityKeyV2(row []any, positions map[string]int, types map[string]string) (string, error) {
	components := []string{context.product.FactNamespace}
	for _, field := range context.product.EntityKey {
		position, present := positions[field]
		if !present || position >= len(row) {
			return "", fmt.Errorf("result is missing stable entity key %q", field)
		}
		canonical, err := exposure.CanonicalSQLValue(types[field], row[position])
		if err != nil {
			return "", err
		}
		components = append(components, field, types[field], canonical)
	}
	return exposure.ComposeCanonicalKeyV2("base-entity", components...)
}

func typedGroupKeyV2(fields []string, row []any, positions map[string]int, types map[string]string, namespace string) (string, error) {
	components := make([]string, 0, len(fields)*2)
	for _, field := range fields {
		position, present := positions[field]
		if !present || position >= len(row) {
			return "", fmt.Errorf("result is missing group field %q", field)
		}
		canonical, err := exposure.CanonicalSQLValue(types[field], row[position])
		if err != nil {
			return "", err
		}
		components = append(components, namespace+"."+field+"\x00"+types[field]+"\x00"+canonical)
	}
	if len(components) == 0 {
		components = append(components, "global")
	}
	sort.Strings(components)
	return exposure.ComposeCanonicalKeyV2("group-row", components...)
}

func annotatedGroupKeyV2(fields []string, row exposure.AnnotatedRowV2, types map[string]string, namespace string) (string, error) {
	components := make([]string, 0, len(fields)*2)
	for _, field := range fields {
		canonical, err := exposure.CanonicalSQLValue(types[field], row.Cells[field].Value)
		if err != nil {
			return "", err
		}
		components = append(components, namespace+"."+field+"\x00"+types[field]+"\x00"+canonical)
	}
	if len(components) == 0 {
		components = append(components, "global")
	}
	sort.Strings(components)
	return exposure.ComposeCanonicalKeyV2("group-row", components...)
}

func aggregateSQLType(function, input string) string {
	switch function {
	case "count":
		return "bigint"
	case "avg":
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "smallint", "integer", "bigint", "numeric":
			return "numeric"
		}
	case "sum":
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "smallint", "integer":
			return "bigint"
		case "bigint":
			return "numeric"
		}
	}
	return input
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (context *planExposureContext) visibleResult(result dataconnector.Result) (dataconnector.Result, error) {
	if context.relational != nil {
		var err error
		result, err = context.relational.canonicalVisibleResult(result)
		if err != nil {
			return dataconnector.Result{}, err
		}
	}
	if len(result.Columns) == len(context.visibleFields) {
		ordered := true
		for index, field := range context.visibleFields {
			if result.Columns[index].Name != field {
				ordered = false
				break
			}
		}
		if ordered {
			return result, nil
		}
	}
	positions, err := columnPositions(result.Columns)
	if err != nil {
		return dataconnector.Result{}, err
	}
	trimmed := result
	trimmed.Columns = make([]dataconnector.Column, 0, len(context.visibleFields))
	for _, field := range context.visibleFields {
		position, present := positions[field]
		if !present || position >= len(result.Columns) {
			return dataconnector.Result{}, fmt.Errorf("visible result is missing requested field %q", field)
		}
		trimmed.Columns = append(trimmed.Columns, result.Columns[position])
	}
	trimmed.Rows = make([][]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		visible := make([]any, 0, len(context.visibleFields))
		for _, field := range context.visibleFields {
			position := positions[field]
			if position >= len(row) {
				return dataconnector.Result{}, fmt.Errorf("visible result row is shorter than its metadata")
			}
			visible = append(visible, row[position])
		}
		trimmed.Rows = append(trimmed.Rows, visible)
	}
	return trimmed, nil
}

func sourceHashesByGroupAndField(relation exposure.Relation, groupFields []string) (map[string]map[string][]string, error) {
	sets := make(map[string]map[string]map[string]struct{})
	for _, row := range relation.Rows {
		values := make([]any, 0, len(groupFields))
		for _, field := range groupFields {
			values = append(values, row.Cells[field].Value)
		}
		key := composeExposureGroupKey(values)
		if sets[key] == nil {
			sets[key] = make(map[string]map[string]struct{})
		}
		for field, cell := range row.Cells {
			if sets[key][field] == nil {
				sets[key][field] = make(map[string]struct{})
			}
			if err := cell.Sources.Range(func(hash [32]byte, _ exposure.FactID) error {
				sets[key][field][hex.EncodeToString(hash[:])] = struct{}{}
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	result := make(map[string]map[string][]string, len(sets))
	for key, fields := range sets {
		result[key] = make(map[string][]string, len(fields))
		for field, set := range fields {
			result[key][field] = sortedStringSet(set)
		}
	}
	return result, nil
}

func composeExposureGroupKey(values []any) string {
	if len(values) == 0 {
		key, _ := exposure.ComposeKey("global")
		return key
	}
	key, _ := exposure.ComposeKey(values...)
	return key
}

func columnPositions(columns []dataconnector.Column) (map[string]int, error) {
	result := make(map[string]int, len(columns))
	for index, column := range columns {
		if _, duplicate := result[column.Name]; duplicate {
			return nil, fmt.Errorf("result contains duplicate column %q", column.Name)
		}
		result[column.Name] = index
	}
	return result, nil
}

func valuesForFields(row []any, positions map[string]int, fields []string) ([]any, error) {
	values := make([]any, 0, len(fields))
	for _, field := range fields {
		position, ok := positions[field]
		if !ok || position >= len(row) {
			return nil, fmt.Errorf("result is missing field %q", field)
		}
		values = append(values, row[position])
	}
	return values, nil
}

func cloneQueryPlan(plan queryplan.QueryPlan) queryplan.QueryPlan {
	plan.Columns = append([]string(nil), plan.Columns...)
	plan.Aggregates = append([]queryplan.Aggregate(nil), plan.Aggregates...)
	plan.Filters = append([]queryplan.Filter(nil), plan.Filters...)
	plan.GroupBy = append([]string(nil), plan.GroupBy...)
	plan.OrderBy = append([]queryplan.Order(nil), plan.OrderBy...)
	if plan.From != nil {
		from := *plan.From
		if from.Scan != nil {
			scan := *from.Scan
			scan.Filters = append([]queryplan.Filter(nil), scan.Filters...)
			from.Scan = &scan
		}
		if from.Join != nil {
			join := *from.Join
			join.Left.Filters = append([]queryplan.Filter(nil), join.Left.Filters...)
			join.Right.Filters = append([]queryplan.Filter(nil), join.Right.Filters...)
			join.On = append([]queryplan.JoinPredicate(nil), join.On...)
			from.Join = &join
		}
		if from.JoinMany != nil {
			join := *from.JoinMany
			join.Sources = append([]queryplan.Scan(nil), join.Sources...)
			for index := range join.Sources {
				join.Sources[index].Filters = append([]queryplan.Filter(nil), join.Sources[index].Filters...)
			}
			join.On = append([]queryplan.JoinPredicate(nil), join.On...)
			from.JoinMany = &join
		}
		if from.UnionDistinct != nil {
			union := *from.UnionDistinct
			union.Columns = append([]string(nil), union.Columns...)
			union.Left.Filters = append([]queryplan.Filter(nil), union.Left.Filters...)
			union.Right.Filters = append([]queryplan.Filter(nil), union.Right.Filters...)
			from.UnionDistinct = &union
		}
		plan.From = &from
	}
	return plan
}

func stringSetFromSlice(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// applyHavingV2 models HAVING as a Select over the group's aggregate outputs.
// PostgreSQL has already filtered the groups (the visible result restricts the
// relation to returned groups), so the predicate is TRUE here; the Select adds
// the read aggregate cells to the surviving rows' support, which is the
// Dependency rule HAVING shares with Select over base columns.
func applyHavingV2(aggregated exposure.RelationV2, plan queryplan.QueryPlan) (exposure.RelationV2, error) {
	if len(plan.Having) == 0 {
		return aggregated, nil
	}
	fields := make([]string, 0, len(plan.Having))
	for _, filter := range plan.Having {
		fields = append(fields, filter.Column)
	}
	return exposure.SelectV2(aggregated, uniqueStrings(fields), func(exposure.AnnotatedRowV2) exposure.SQLTruth { return exposure.SQLTrue })
}
