package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

var errProvenanceTruncated = errors.New("provenance result exceeded the connector row ceiling")

type planExposureContext struct {
	product          catalog.Product
	plan             queryplan.QueryPlan
	mainSQL          string
	provenanceSQL    string
	visibleFields    []string
	factFields       []string
	provenanceFields []string
	meteringColumns  []string
	grouped          bool
}

func buildPlanExposureContext(plan queryplan.QueryPlan, product catalog.Product, approvedColumns map[string]struct{}, allowedAggregates map[string]struct{}) (*planExposureContext, error) {
	if strings.TrimSpace(product.Snapshot) == "" || len(product.EntityKey) == 0 {
		return nil, fmt.Errorf("product does not define a stable snapshot and entity key")
	}
	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
	if grouped {
		selected := stringSetFromSlice(plan.Columns)
		for _, group := range plan.GroupBy {
			if _, ok := selected[group]; !ok {
				return nil, fmt.Errorf("exposure-accounted group_by fields must be selected")
			}
		}
	}
	visibleFields := append([]string(nil), plan.Columns...)
	factFields := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		function := strings.ToLower(aggregate.Function)
		switch function {
		case "count", "sum", "min", "max":
		default:
			return nil, fmt.Errorf("aggregate %q is outside the exposure-accounted fragment", aggregate.Function)
		}
		visibleFields = append(visibleFields, aggregate.Alias)
		factFields = append(factFields, function+"("+aggregate.Column+")")
	}

	catalogColumns := make(map[string]struct{}, len(product.Fields))
	for _, field := range product.Fields {
		catalogColumns[field.Name] = struct{}{}
	}
	meteringSet := make(map[string]struct{}, len(approvedColumns)+len(product.EntityKey)+len(product.Scopes))
	for column := range approvedColumns {
		meteringSet[column] = struct{}{}
	}
	for _, column := range append(append([]string(nil), product.EntityKey...), product.Scopes...) {
		if _, ok := catalogColumns[column]; !ok {
			return nil, fmt.Errorf("metering column %q is not published", column)
		}
		meteringSet[column] = struct{}{}
	}
	meteringColumns := sortedStringSet(meteringSet)
	internalProduct := queryplan.Product{Name: product.Name, Columns: meteringSet, AllowedAggregates: allowedAggregates}

	provenanceSet := make(map[string]struct{})
	for _, column := range product.EntityKey {
		provenanceSet[column] = struct{}{}
	}
	for _, column := range product.Scopes {
		provenanceSet[column] = struct{}{}
	}
	for _, column := range plan.Columns {
		provenanceSet[column] = struct{}{}
	}
	for _, aggregate := range plan.Aggregates {
		provenanceSet[aggregate.Column] = struct{}{}
	}
	for _, filter := range plan.Filters {
		provenanceSet[filter.Column] = struct{}{}
	}
	for _, group := range plan.GroupBy {
		provenanceSet[group] = struct{}{}
	}
	provenanceFields := sortedStringSet(provenanceSet)

	mainPlan := cloneQueryPlan(plan)
	if !grouped {
		// The visible and provenance statements use the same hidden projection
		// and deterministic key tie-breaker, so their capped row sets are equal.
		selected := stringSetFromSlice(mainPlan.Columns)
		for _, field := range provenanceFields {
			if _, present := selected[field]; !present {
				mainPlan.Columns = append(mainPlan.Columns, field)
				selected[field] = struct{}{}
			}
		}
		ordered := make(map[string]struct{}, len(mainPlan.OrderBy)+len(product.EntityKey))
		for _, order := range mainPlan.OrderBy {
			ordered[order.Column] = struct{}{}
		}
		for _, key := range product.EntityKey {
			if _, present := ordered[key]; !present {
				mainPlan.OrderBy = append(mainPlan.OrderBy, queryplan.Order{Column: key, Direction: "asc"})
				ordered[key] = struct{}{}
			}
		}
	}
	mainSQL, err := queryplan.Compile(mainPlan, internalProduct)
	if err != nil {
		return nil, err
	}
	provenanceSQL := mainSQL
	if grouped {
		provenancePlan := queryplan.QueryPlan{Product: plan.Product, Columns: provenanceFields, Filters: append([]queryplan.Filter(nil), plan.Filters...)}
		provenanceSQL, err = queryplan.Compile(provenancePlan, internalProduct)
		if err != nil {
			return nil, err
		}
	}
	return &planExposureContext{product: product, plan: cloneQueryPlan(plan), mainSQL: mainSQL,
		provenanceSQL: provenanceSQL, visibleFields: visibleFields, factFields: factFields,
		provenanceFields: provenanceFields, meteringColumns: meteringColumns, grouped: grouped}, nil
}

func (context *planExposureContext) extendGrant(grant sqlpolicy.Grant) (sqlpolicy.Grant, error) {
	result := sqlpolicy.Grant{Products: append([]sqlpolicy.ProductGrant(nil), grant.Products...)}
	found := false
	for index := range result.Products {
		if result.Products[index].LogicalName != context.product.Name {
			continue
		}
		found = true
		columns := stringSetFromSlice(result.Products[index].ApprovedColumns)
		for _, column := range context.meteringColumns {
			columns[column] = struct{}{}
		}
		result.Products[index].ApprovedColumns = sortedStringSet(columns)
	}
	if !found {
		return sqlpolicy.Grant{}, fmt.Errorf("exposure product is absent from the task grant")
	}
	return result, nil
}

func (context *planExposureContext) deriveObservation(visible, provenance dataconnector.Result, profile string) (exposure.Observation, error) {
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

	release := make(exposure.FactSet)
	groupSources := sourceHashesByGroupAndField(relation, context.plan.GroupBy)
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
	return (exposure.Observation{ProfileVersion: profile, Release: release.Values(), Influence: sourceObservation.Influence}).Normalize()
}

func (context *planExposureContext) visibleResult(result dataconnector.Result) (dataconnector.Result, error) {
	if context.grouped || len(result.Columns) == len(context.visibleFields) {
		return result, nil
	}
	if len(result.Columns) < len(context.visibleFields) {
		return dataconnector.Result{}, fmt.Errorf("visible result is missing requested columns")
	}
	trimmed := result
	trimmed.Columns = append([]dataconnector.Column(nil), result.Columns[:len(context.visibleFields)]...)
	trimmed.Rows = make([][]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < len(context.visibleFields) {
			return dataconnector.Result{}, fmt.Errorf("visible result row is shorter than its metadata")
		}
		trimmed.Rows = append(trimmed.Rows, append([]any(nil), row[:len(context.visibleFields)]...))
	}
	return trimmed, nil
}

func sourceHashesByGroupAndField(relation exposure.Relation, groupFields []string) map[string]map[string][]string {
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
			for hash := range cell.Sources {
				sets[key][field][hash] = struct{}{}
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
	return result
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
