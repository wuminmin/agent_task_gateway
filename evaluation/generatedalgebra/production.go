package generatedalgebra

import (
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/evaluation/exposureoracle"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

// scan builds the production relation for a reference Relation.
func scan(source Relation) (exposure.RelationV2, error) {
	fields := make([]exposure.FieldV2, 0, len(source.Fields))
	for _, field := range source.Fields {
		item := exposure.FieldV2{ID: field.ID, SQLType: field.SQLType}
		if field.SQLType == "text" {
			item.Collation, item.CollationVersion, item.CollationDeterministic = "C", "builtin", true
		}
		fields = append(fields, item)
	}
	rows := make([]exposure.BaseRowV2, 0, len(source.Rows))
	for _, row := range source.Rows {
		rows = append(rows, exposure.BaseRowV2{EntityKey: row.EntityKey, Values: row.Values})
	}
	relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot,
		StableRole: source.StableRole, Fields: fields, Rows: rows})
	if err != nil {
		return exposure.RelationV2{}, err
	}
	relation.CanonicalOrder = true
	return relation, nil
}

// EvaluateProduction runs the plan through the production V2 algebra and
// returns its observation converted to semantic facts. Every FactID hash is
// checked against the reference hash of the converted fact.
func EvaluateProduction(expenses, departments Relation, plan Plan) (Observation, error) {
	input, err := scan(expenses)
	if err != nil {
		return Observation{}, err
	}
	fieldTypes := map[string]string{}
	for _, field := range expenses.Fields {
		fieldTypes[field.ID] = field.SQLType
	}
	if plan.Join {
		right, joinErr := scan(departments)
		if joinErr != nil {
			return Observation{}, joinErr
		}
		for _, field := range departments.Fields {
			fieldTypes[field.ID] = field.SQLType
		}
		input, err = exposure.JoinV2(right, input, "department.department", "expense.department")
		if err != nil {
			return Observation{}, err
		}
	}
	if len(plan.Predicates) > 0 {
		predicateFields := make([]string, 0, len(plan.Predicates))
		for _, predicate := range plan.Predicates {
			predicateFields = append(predicateFields, predicate.Field)
		}
		predicates := plan.Predicates
		input, err = exposure.SelectV2(input, predicateFields, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			for _, predicate := range predicates {
				truth := compare(fieldTypes[predicate.Field], row.Cells[predicate.Field].Value, predicate.Literal, predicate.Op)
				switch truth {
				case Unknown:
					return exposure.SQLUnknown
				case False:
					return exposure.SQLFalse
				}
			}
			return exposure.SQLTrue
		})
		if err != nil {
			return Observation{}, err
		}
	}
	var relation exposure.RelationV2
	var visible []string
	switch plan.Kind {
	case "project":
		relation = input
		if plan.Page != nil {
			relation.Rows = append([]exposure.AnnotatedRowV2(nil), input.Rows...)
			sort.SliceStable(relation.Rows, func(i, j int) bool { return relation.Rows[i].Key < relation.Rows[j].Key })
			relation.CanonicalOrder = true
			relation, err = exposure.PageV2(relation, plan.Page[0], plan.Page[1])
			if err != nil {
				return Observation{}, err
			}
		}
		relation, err = exposure.ProjectV2(relation, plan.Project...)
		if err != nil {
			return Observation{}, err
		}
		visible = plan.Project
	case "group", "global":
		var groupFields []string
		if plan.Kind == "group" {
			groupFields = []string{plan.GroupField}
		}
		specs := make([]exposure.AggregateSpecV2, 0, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			specs = append(specs, exposure.AggregateSpecV2{Function: aggregate.Function, Field: aggregate.Field, OutputID: aggregate.OutputID, OutputType: aggregate.OutputType})
		}
		outputRows, rowsErr := aggregateOutputRows(expenses, departments, input, plan)
		if rowsErr != nil {
			return Observation{}, rowsErr
		}
		relation, err = exposure.AggregateFromResultsV2(input, groupFields, specs, outputRows)
		if err != nil {
			return Observation{}, err
		}
		if plan.Kind == "group" && plan.GroupKeyVisible {
			visible = append(visible, plan.GroupField)
		}
		for _, aggregate := range plan.Aggregates {
			visible = append(visible, aggregate.OutputID)
		}
	default:
		return Observation{}, fmt.Errorf("unknown plan kind %q", plan.Kind)
	}
	observation, err := exposure.ObserveV2(relation, visible...)
	if err != nil {
		return Observation{}, err
	}
	release, err := convert(observation.Release)
	if err != nil {
		return Observation{}, err
	}
	influence, err := convert(observation.Influence)
	if err != nil {
		return Observation{}, err
	}
	return Observation{Release: release, Influence: influence}, nil
}

// aggregateOutputRows computes the aggregate results the way PostgreSQL would
// return them, which the production algebra consumes as its output rows.
func aggregateOutputRows(expenses, departments Relation, input exposure.RelationV2, plan Plan) ([]map[string]any, error) {
	groups := map[string][]joinedRow{}
	order := []string{}
	keyValues := map[string]any{}
	for _, row := range input.Rows {
		values := map[string]any{}
		for id, cell := range row.Cells {
			values[id] = cell.Value
		}
		member := joinedRow{values: values}
		key := "global"
		if plan.Kind == "group" {
			canonical, err := CanonicalValue(groupType(expenses, departments, plan.GroupField), values[plan.GroupField])
			if err != nil {
				return nil, err
			}
			key = canonical
			keyValues[key] = values[plan.GroupField]
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], member)
	}
	if plan.Kind == "global" && len(order) == 0 {
		order = append(order, "global")
		groups["global"] = nil
	}
	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		output := map[string]any{}
		if plan.Kind == "group" {
			output[plan.GroupField] = keyValues[key]
		}
		for _, aggregate := range plan.Aggregates {
			if aggregate.Field == "*" {
				output[aggregate.OutputID] = int64(len(groups[key]))
				continue
			}
			output[aggregate.OutputID] = AggregateValue(aggregate.Function, aggregate.Field, groups[key])
		}
		rows = append(rows, output)
	}
	return rows, nil
}

func convert(facts []exposure.FactID) (map[string]exposureoracle.Fact, error) {
	result := make(map[string]exposureoracle.Fact, len(facts))
	for _, fact := range facts {
		bindings := make([]exposureoracle.SnapshotBinding, 0, len(fact.SnapshotBundle))
		for _, binding := range fact.SnapshotBundle {
			bindings = append(bindings, exposureoracle.SnapshotBinding{SourceNamespace: binding.SourceNamespace, Snapshot: binding.Snapshot})
		}
		converted := exposureoracle.Fact{Profile: fact.Profile, Kind: string(fact.Kind), Snapshot: fact.Snapshot, EntityKey: fact.EntityKey,
			Field: fact.Field, SourceNamespace: fact.SourceNamespace, SQLType: fact.SQLType, CanonicalValue: fact.CanonicalValue,
			SnapshotBundle: bindings, OutputRowKey: fact.OutputRowKey, NormalizedExpression: fact.NormalizedExpression,
			WitnessCommitment: fact.WitnessCommitment}
		productionHash, err := fact.Hash()
		if err != nil {
			return nil, err
		}
		if referenceHash := exposureoracle.Hash(converted); referenceHash != productionHash {
			return nil, fmt.Errorf("production hash %s differs from the reference hash %s for %s", productionHash[:12], referenceHash[:12], exposureoracle.Key(converted))
		}
		result[exposureoracle.Key(converted)] = converted
	}
	return result, nil
}
