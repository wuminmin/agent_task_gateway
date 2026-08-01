package gateway

import (
	"crypto/sha256"
	"encoding/hex"
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
	product           catalog.Product
	products          []catalog.Product
	plan              queryplan.QueryPlan
	mainSQL           string
	provenanceSQL     string
	visibleFields     []string
	factFields        []string
	provenanceFields  []string
	meteringColumns   []string
	grouped           bool
	expandedEvidence  bool
	meteringByProduct map[string][]string
	// internalPolicyProducts is the least-privilege terminal-product closure
	// for one gateway-compiled semantic View. It is derived from the signed
	// View artifact and task scope, and is never copied into the public Grant.
	internalPolicyProducts map[string]sqlpolicy.ProductGrant
	viewBindingDigest      string
	viewRegistryRevision   string
	relational             *relationalExposureContext
	normalForm             *queryplan.NormalForm
	algebraNormalForm      *queryplan.AlgebraNormalFormV2
	planDigest             string
	ordinal                *boundOrdinalExecution
	predicateFootprint     *queryplan.PredicateFootprint
}

func buildPlanExposureContext(plan queryplan.QueryPlan, product catalog.Product, approvedColumns map[string]struct{}, allowedAggregates map[string]struct{}) (*planExposureContext, error) {
	if strings.TrimSpace(product.Snapshot) == "" || len(product.EntityKey) == 0 {
		return nil, fmt.Errorf("product does not define a stable snapshot and entity key")
	}
	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
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
		if aggregate.Column != "*" {
			provenanceSet[aggregate.Column] = struct{}{}
		}
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
	} else {
		// Group keys participate in positive-output dependency even when they
		// are not delivered. Select them internally so the paired result can
		// identify groups and build their annotations, then visibleResult removes
		// them from the external response.
		selected := stringSetFromSlice(mainPlan.Columns)
		for _, group := range plan.GroupBy {
			if _, present := selected[group]; !present {
				mainPlan.Columns = append(mainPlan.Columns, group)
				selected[group] = struct{}{}
			}
		}
	}
	if grouped && (plan.Limit > 0 || plan.Offset > 0) {
		ordered := make(map[string]struct{}, len(mainPlan.OrderBy)+len(plan.GroupBy))
		for _, order := range mainPlan.OrderBy {
			ordered[order.Column] = struct{}{}
		}
		for _, key := range sortedStringSet(stringSetFromSlice(plan.GroupBy)) {
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

func (context *planExposureContext) configureV2(approvedColumns map[string]struct{}, allowedAggregates map[string]struct{}) error {
	if strings.TrimSpace(context.product.FactNamespace) == "" || strings.TrimSpace(context.product.StableRelationRole) == "" {
		return fmt.Errorf("V2 product lacks canonical fact namespace or stable relation role")
	}
	columnTypes := make(map[string]string, len(context.product.Fields))
	columnCollations := make(map[string]string, len(context.product.Fields))
	collationVersions := make(map[string]string, len(context.product.Fields))
	for _, field := range context.product.Fields {
		columnTypes[field.Name] = field.Type
		columnCollations[field.Name] = field.Collation
		collationVersions[field.Name] = field.CollationVersion
	}
	normal, err := queryplan.NormalizeV2(context.plan, queryplan.Product{
		Name: context.product.Name, Columns: approvedColumns, AllowedAggregates: allowedAggregates,
		ColumnTypes: columnTypes, ColumnCollations: columnCollations, CollationVersions: collationVersions,
		SourceNamespace: context.product.FactNamespace, Snapshot: context.product.Snapshot,
		StableEntityKey: append([]string(nil), context.product.EntityKey...), LineageDigest: context.product.LineageManifestDigest,
	})
	if err != nil {
		return err
	}
	digest, err := normal.Digest()
	if err != nil {
		return err
	}
	context.normalForm = &normal
	context.planDigest = digest
	return nil
}

func (context *planExposureContext) configurePredicateFootprintV5(catalogDigest string, mandatoryScope []byte,
	products map[string]queryplan.Product, callerFilters []queryplan.PredicateFilterBinding,
	fields map[string]queryplan.PredicateFieldBinding, semanticProductID string, limits queryplan.PredicateLimits) error {
	if context == nil || len(products) == 0 {
		return errors.New("V5 predicate footprint lacks product bindings")
	}
	scopeHash := sha256.Sum256(append([]byte("TASKGATE-EFFECTIVE-MANDATORY-SCOPE-V1\x00"), mandatoryScope...))
	bindings := queryplan.PredicateBindings{CatalogSHA256: catalogDigest, Products: products,
		ViewBindingSHA256: context.viewBindingDigest, CallerFilters: callerFilters, Fields: fields,
		SemanticProductID: semanticProductID}
	footprint, err := queryplan.BuildPredicateFootprint(context.plan, bindings, hex.EncodeToString(scopeHash[:]), limits)
	if err != nil {
		return err
	}
	if context.relational == nil {
		product := products[context.plan.Product]
		normal, normalizeErr := queryplan.NormalizeV4(context.plan, product)
		if normalizeErr != nil {
			return normalizeErr
		}
		digest, digestErr := normal.Digest()
		if digestErr != nil {
			return digestErr
		}
		context.normalForm = &normal
		context.planDigest = digest
	} else {
		normal, normalizeErr := queryplan.SemanticNormalFormV4(context.plan, context.relational.compilation, products)
		if normalizeErr != nil {
			return normalizeErr
		}
		context.algebraNormalForm = &normal
		context.planDigest = normal.SHA256
	}
	context.predicateFootprint = &footprint
	return nil
}

func (context *planExposureContext) extendGrant(grant sqlpolicy.Grant) (sqlpolicy.Grant, error) {
	result := clonePolicyGrant(grant)
	if len(context.internalPolicyProducts) != 0 {
		names := make([]string, 0, len(context.internalPolicyProducts))
		for name := range context.internalPolicyProducts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			internal := clonePolicyProductGrant(context.internalPolicyProducts[name])
			found := false
			for index := range result.Products {
				if result.Products[index].LogicalName != name {
					continue
				}
				found = true
				if result.Products[index].PhysicalSchema != internal.PhysicalSchema ||
					result.Products[index].PhysicalView != internal.PhysicalView {
					return sqlpolicy.Grant{}, fmt.Errorf("semantic View terminal product %q conflicts with the task grant", name)
				}
				columns := stringSetFromSlice(result.Products[index].ApprovedColumns)
				for _, column := range internal.ApprovedColumns {
					columns[column] = struct{}{}
				}
				result.Products[index].ApprovedColumns = sortedStringSet(columns)
				for _, predicate := range internal.MandatoryScope {
					if !containsScopePredicate(result.Products[index].MandatoryScope, predicate) {
						cloned := predicate
						cloned.Values = append([]string(nil), predicate.Values...)
						result.Products[index].MandatoryScope = append(result.Products[index].MandatoryScope, cloned)
					}
				}
				break
			}
			if !found {
				result.Products = append(result.Products, internal)
			}
		}
	}
	if context.relational != nil {
		found := make(map[string]bool, len(context.meteringByProduct))
		for index := range result.Products {
			columns, required := context.meteringByProduct[result.Products[index].LogicalName]
			if !required {
				continue
			}
			found[result.Products[index].LogicalName] = true
			set := stringSetFromSlice(result.Products[index].ApprovedColumns)
			for _, column := range columns {
				set[column] = struct{}{}
			}
			result.Products[index].ApprovedColumns = sortedStringSet(set)
		}
		for product := range context.meteringByProduct {
			if !found[product] {
				return sqlpolicy.Grant{}, fmt.Errorf("exposure product %q is absent from the task grant", product)
			}
		}
		return result, nil
	}
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

func containsScopePredicate(predicates []sqlpolicy.ScopePredicate, target sqlpolicy.ScopePredicate) bool {
	for _, predicate := range predicates {
		if predicate.Column != target.Column || predicate.Operator != target.Operator || len(predicate.Values) != len(target.Values) {
			continue
		}
		equal := true
		for index := range predicate.Values {
			if predicate.Values[index] != target.Values[index] {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
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

func (context *planExposureContext) deriveObservationV2(visible, provenance dataconnector.Result) (exposure.Observation, error) {
	if context.normalForm == nil || context.planDigest == "" {
		return exposure.Observation{}, fmt.Errorf("V2 query context has no normal form")
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
			OutputID: aggregate.Alias, OutputType: outputType})
	}
	aggregated, err := exposure.AggregateFromResultsV2(relation, context.plan.GroupBy, specs, outputs)
	if err != nil {
		return exposure.Observation{}, err
	}
	return exposure.ObserveV2(aggregated, context.visibleFields...)
}

func (context *planExposureContext) usesExpandedEvidence() bool {
	return context.grouped || context.expandedEvidence
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
