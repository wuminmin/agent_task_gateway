package exposure

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

type Cell struct {
	Value       any
	Sources     FactSet
	factField   string
	sourceBound bool
}

type Row struct {
	Key     string
	Cells   map[string]Cell
	Lineage FactSet
}

type Relation struct {
	Product  string
	Snapshot string
	Fields   []string
	Rows     []Row
}

type BaseRow struct {
	EntityKey string         `json:"entity_key"`
	Values    map[string]any `json:"values"`
}

func NewBaseRelation(product, snapshot string, fields []string, rows []BaseRow) (Relation, error) {
	if strings.TrimSpace(product) == "" || strings.TrimSpace(snapshot) == "" || len(fields) == 0 {
		return Relation{}, fmt.Errorf("%w: product, snapshot, and fields are required", ErrInvalid)
	}
	if err := validateFields(fields); err != nil {
		return Relation{}, err
	}
	result := Relation{Product: product, Snapshot: snapshot, Fields: append([]string(nil), fields...), Rows: make([]Row, 0, len(rows))}
	seenKeys := make(map[string]struct{}, len(rows))
	for _, base := range rows {
		if strings.TrimSpace(base.EntityKey) == "" {
			return Relation{}, fmt.Errorf("%w: base row entity key is required", ErrInvalid)
		}
		if _, duplicate := seenKeys[base.EntityKey]; duplicate {
			return Relation{}, fmt.Errorf("%w: duplicate entity key %q", ErrInvalid, base.EntityKey)
		}
		seenKeys[base.EntityKey] = struct{}{}
		rowFact, err := NewFact(product, snapshot, base.EntityKey, RowExistenceField, base.EntityKey)
		if err != nil {
			return Relation{}, err
		}
		lineage, _ := NewFactSet(rowFact)
		row := Row{Key: base.EntityKey, Cells: make(map[string]Cell, len(fields)), Lineage: lineage}
		for _, field := range fields {
			value, ok := base.Values[field]
			if !ok {
				return Relation{}, fmt.Errorf("%w: row %q is missing field %q", ErrInvalid, base.EntityKey, field)
			}
			fact, err := NewFact(product, snapshot, base.EntityKey, field, value)
			if err != nil {
				return Relation{}, err
			}
			sources, _ := NewFactSet(fact)
			row.Cells[field] = Cell{Value: value, Sources: sources}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func Project(input Relation, fields ...string) (Relation, error) {
	if err := requireFields(input, fields); err != nil {
		return Relation{}, err
	}
	result := Relation{Product: input.Product, Snapshot: input.Snapshot, Fields: append([]string(nil), fields...), Rows: make([]Row, 0, len(input.Rows))}
	for _, source := range input.Rows {
		row := Row{Key: source.Key, Cells: make(map[string]Cell, len(fields)), Lineage: source.Lineage.Clone()}
		for _, field := range fields {
			row.Cells[field] = cloneCell(source.Cells[field])
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

// Select keeps rows accepted by predicate and records predicateFields as
// source influence for each retained result row.
func Select(input Relation, predicateFields []string, predicate func(Row) bool) (Relation, error) {
	if predicate == nil {
		return Relation{}, fmt.Errorf("%w: selection predicate is required", ErrInvalid)
	}
	if err := requireFields(input, predicateFields); err != nil {
		return Relation{}, err
	}
	result := Relation{Product: input.Product, Snapshot: input.Snapshot, Fields: append([]string(nil), input.Fields...)}
	for _, source := range input.Rows {
		if !predicate(source) {
			continue
		}
		row := cloneRow(source)
		for _, field := range predicateFields {
			row.Lineage.Merge(source.Cells[field].Sources)
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func Page(input Relation, offset, limit int) (Relation, error) {
	if offset < 0 || limit < 0 {
		return Relation{}, fmt.Errorf("%w: page bounds cannot be negative", ErrInvalid)
	}
	start := minInt(offset, len(input.Rows))
	end := len(input.Rows)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	result := Relation{Product: input.Product, Snapshot: input.Snapshot, Fields: append([]string(nil), input.Fields...), Rows: make([]Row, 0, end-start)}
	for _, row := range input.Rows[start:end] {
		result.Rows = append(result.Rows, cloneRow(row))
	}
	return result, nil
}

func Join(left, right Relation, leftKey, rightKey string) (Relation, error) {
	if err := requireFields(left, []string{leftKey}); err != nil {
		return Relation{}, err
	}
	if err := requireFields(right, []string{rightKey}); err != nil {
		return Relation{}, err
	}
	product := "join(" + left.Product + "," + right.Product + ")"
	snapshot, err := ComposeKey(left.Snapshot, right.Snapshot)
	if err != nil {
		return Relation{}, err
	}
	fields := make([]string, 0, len(left.Fields)+len(right.Fields))
	for _, field := range left.Fields {
		fields = append(fields, "left."+field)
	}
	for _, field := range right.Fields {
		fields = append(fields, "right."+field)
	}
	result := Relation{Product: product, Snapshot: snapshot, Fields: fields}
	for _, lrow := range left.Rows {
		for _, rrow := range right.Rows {
			if !equalValues(lrow.Cells[leftKey].Value, rrow.Cells[rightKey].Value) {
				continue
			}
			key, err := ComposeKey(lrow.Key, rrow.Key)
			if err != nil {
				return Relation{}, err
			}
			lineage := lrow.Lineage.Clone()
			lineage.Merge(rrow.Lineage)
			lineage.Merge(lrow.Cells[leftKey].Sources)
			lineage.Merge(rrow.Cells[rightKey].Sources)
			row := Row{Key: key, Cells: make(map[string]Cell, len(fields)), Lineage: lineage}
			for _, field := range left.Fields {
				row.Cells["left."+field] = cloneCell(lrow.Cells[field])
			}
			for _, field := range right.Fields {
				row.Cells["right."+field] = cloneCell(rrow.Cells[field])
			}
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}

// Union implements SQL UNION set semantics and merges provenance when the same
// output tuple is produced by more than one input row.
func Union(left, right Relation) (Relation, error) {
	if strings.Join(left.Fields, "\x00") != strings.Join(right.Fields, "\x00") {
		return Relation{}, fmt.Errorf("%w: union schemas differ", ErrInvalid)
	}
	if left.Product != right.Product || left.Snapshot != right.Snapshot {
		return Relation{}, fmt.Errorf("%w: union inputs must share product and snapshot", ErrInvalid)
	}
	result := Relation{Product: left.Product, Snapshot: left.Snapshot, Fields: append([]string(nil), left.Fields...)}
	index := make(map[string]int)
	for _, source := range append(append([]Row(nil), left.Rows...), right.Rows...) {
		values := make([]any, 0, len(result.Fields))
		for _, field := range result.Fields {
			values = append(values, source.Cells[field].Value)
		}
		key, err := ComposeKey(values...)
		if err != nil {
			return Relation{}, err
		}
		if existing, ok := index[key]; ok {
			result.Rows[existing].Lineage.Merge(source.Lineage)
			for _, field := range result.Fields {
				cell := result.Rows[existing].Cells[field]
				cell.Sources.Merge(source.Cells[field].Sources)
				result.Rows[existing].Cells[field] = cell
			}
			continue
		}
		row := cloneRow(source)
		row.Key = key
		index[key] = len(result.Rows)
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

type AggregateSpec struct {
	Function string
	Field    string
	Alias    string
}

func Aggregate(input Relation, groupFields []string, specs []AggregateSpec) (Relation, error) {
	if len(specs) == 0 {
		return Relation{}, fmt.Errorf("%w: at least one aggregate is required", ErrInvalid)
	}
	if err := requireFields(input, groupFields); err != nil {
		return Relation{}, err
	}
	seen := make(map[string]struct{}, len(groupFields)+len(specs))
	for _, field := range groupFields {
		seen[field] = struct{}{}
	}
	for index := range specs {
		specs[index].Function = strings.ToLower(strings.TrimSpace(specs[index].Function))
		if specs[index].Function != "count" && specs[index].Function != "sum" && specs[index].Function != "min" && specs[index].Function != "max" {
			return Relation{}, fmt.Errorf("%w: aggregate %q is outside the supported fragment", ErrInvalid, specs[index].Function)
		}
		if specs[index].Field != "*" {
			if err := requireFields(input, []string{specs[index].Field}); err != nil {
				return Relation{}, err
			}
		} else if specs[index].Function != "count" {
			return Relation{}, fmt.Errorf("%w: only count supports *", ErrInvalid)
		}
		if strings.TrimSpace(specs[index].Alias) == "" {
			return Relation{}, fmt.Errorf("%w: aggregate alias is required", ErrInvalid)
		}
		if _, duplicate := seen[specs[index].Alias]; duplicate {
			return Relation{}, fmt.Errorf("%w: duplicate output field %q", ErrInvalid, specs[index].Alias)
		}
		seen[specs[index].Alias] = struct{}{}
	}

	type group struct {
		key    string
		values []any
		rows   []Row
	}
	groups := make(map[string]*group)
	order := make([]string, 0)
	for _, row := range input.Rows {
		values := make([]any, 0, len(groupFields))
		for _, field := range groupFields {
			values = append(values, row.Cells[field].Value)
		}
		var key string
		var err error
		if len(values) == 0 {
			key, err = ComposeKey("global")
		} else {
			key, err = ComposeKey(values...)
		}
		if err != nil {
			return Relation{}, err
		}
		if groups[key] == nil {
			groups[key] = &group{key: key, values: values}
			order = append(order, key)
		}
		groups[key].rows = append(groups[key].rows, row)
	}
	if len(input.Rows) == 0 && len(groupFields) == 0 {
		key, _ := ComposeKey("global")
		groups[key] = &group{key: key}
		order = append(order, key)
	}
	sort.Strings(order)
	fields := append([]string(nil), groupFields...)
	for _, spec := range specs {
		fields = append(fields, spec.Alias)
	}
	result := Relation{Product: input.Product, Snapshot: input.Snapshot, Fields: fields, Rows: make([]Row, 0, len(order))}
	for _, key := range order {
		current := groups[key]
		row := Row{Key: key, Cells: make(map[string]Cell, len(fields)), Lineage: newEmptyFactSet()}
		for _, source := range current.rows {
			row.Lineage.Merge(source.Lineage)
		}
		for index, field := range groupFields {
			sources := newEmptyFactSet()
			for _, source := range current.rows {
				sources.Merge(source.Cells[field].Sources)
			}
			row.Cells[field] = Cell{Value: current.values[index], Sources: sources}
		}
		for _, spec := range specs {
			value, sources, err := evaluateAggregate(current.rows, spec)
			if err != nil {
				return Relation{}, err
			}
			row.Cells[spec.Alias] = Cell{Value: value, Sources: sources,
				factField: spec.Function + "(" + spec.Field + ")", sourceBound: true}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func Observe(profile string, input Relation, visibleFields ...string) (Observation, error) {
	if strings.TrimSpace(profile) == "" {
		return Observation{}, fmt.Errorf("%w: profile is required", ErrInvalid)
	}
	if len(visibleFields) == 0 {
		visibleFields = append([]string(nil), input.Fields...)
	}
	if err := requireFields(input, visibleFields); err != nil {
		return Observation{}, err
	}
	release := newEmptyFactSet()
	influence := newEmptyFactSet()
	for _, row := range input.Rows {
		influence.Merge(row.Lineage)
		for _, field := range visibleFields {
			cell := row.Cells[field]
			factField := field
			var fact FactID
			var err error
			if cell.sourceBound {
				factField = cell.factField
				sourceHashes := make([]string, 0, cell.Sources.Len())
				if err := cell.Sources.Range(func(hash [32]byte, _ FactID) error {
					sourceHashes = append(sourceHashes, hex.EncodeToString(hash[:]))
					return nil
				}); err != nil {
					return Observation{}, err
				}
				sort.Strings(sourceHashes)
				version, versionErr := ValueVersion(map[string]any{"value": cell.Value, "sources": sourceHashes})
				if versionErr != nil {
					return Observation{}, versionErr
				}
				fact = FactID{Product: input.Product, Snapshot: input.Snapshot, EntityKey: row.Key,
					Field: factField, ValueVersion: version}
				err = fact.Validate()
			} else {
				fact, err = NewFact(input.Product, input.Snapshot, row.Key, factField, cell.Value)
			}
			if err != nil {
				return Observation{}, err
			}
			if err := release.Add(fact); err != nil {
				return Observation{}, err
			}
			influence.Merge(cell.Sources)
		}
	}
	releaseValues, err := release.Values()
	if err != nil {
		return Observation{}, err
	}
	influenceValues, err := influence.Values()
	if err != nil {
		return Observation{}, err
	}
	return Observation{ProfileVersion: profile, Release: releaseValues, Influence: influenceValues}, nil
}

func evaluateAggregate(rows []Row, spec AggregateSpec) (any, FactSet, error) {
	sources := newEmptyFactSet()
	values := make([]any, 0, len(rows))
	for _, row := range rows {
		if spec.Field == "*" {
			sources.Merge(row.Lineage)
			values = append(values, true)
			continue
		}
		cell := row.Cells[spec.Field]
		sources.Merge(cell.Sources)
		// SQL aggregates other than COUNT(*) ignore NULL inputs. The NULL fact
		// remains influential because its nullness determines non-contribution.
		if cell.Value != nil {
			values = append(values, cell.Value)
		}
	}
	switch spec.Function {
	case "count":
		return int64(len(values)), sources, nil
	case "sum":
		if len(values) == 0 {
			return nil, sources, nil
		}
		total := float64(0)
		for _, value := range values {
			number, err := numeric(value)
			if err != nil {
				return nil, nil, err
			}
			total += number
		}
		if math.IsInf(total, 0) || math.IsNaN(total) {
			return nil, nil, fmt.Errorf("%w: non-finite aggregate", ErrInvalid)
		}
		return total, sources, nil
	case "min", "max":
		if len(values) == 0 {
			return nil, sources, nil
		}
		best := values[0]
		for _, value := range values[1:] {
			comparison, err := compareValues(value, best)
			if err != nil {
				return nil, nil, err
			}
			if (spec.Function == "min" && comparison < 0) || (spec.Function == "max" && comparison > 0) {
				best = value
			}
		}
		return best, sources, nil
	default:
		return nil, nil, fmt.Errorf("%w: unsupported aggregate", ErrInvalid)
	}
}

func numeric(value any) (float64, error) {
	switch typed := value.(type) {
	case int:
		return float64(typed), nil
	case int32:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case float32:
		return float64(typed), nil
	case float64:
		return typed, nil
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("%w: aggregate value is not numeric", ErrInvalid)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%w: aggregate value %T is not numeric", ErrInvalid, value)
	}
}

func compareValues(left, right any) (int, error) {
	leftNumber, leftErr := numeric(left)
	rightNumber, rightErr := numeric(right)
	if leftErr == nil && rightErr == nil {
		switch {
		case leftNumber < rightNumber:
			return -1, nil
		case leftNumber > rightNumber:
			return 1, nil
		default:
			return 0, nil
		}
	}
	leftString, leftOK := left.(string)
	rightString, rightOK := right.(string)
	if leftOK && rightOK {
		return strings.Compare(leftString, rightString), nil
	}
	return 0, fmt.Errorf("%w: min/max values are not comparable", ErrInvalid)
}

func validateFields(fields []string) error {
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if strings.TrimSpace(field) == "" {
			return fmt.Errorf("%w: field names cannot be empty", ErrInvalid)
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("%w: duplicate field %q", ErrInvalid, field)
		}
		seen[field] = struct{}{}
	}
	return nil
}

func requireFields(relation Relation, fields []string) error {
	available := make(map[string]struct{}, len(relation.Fields))
	for _, field := range relation.Fields {
		available[field] = struct{}{}
	}
	if err := validateFields(fields); err != nil && len(fields) != 0 {
		return err
	}
	for _, field := range fields {
		if _, ok := available[field]; !ok {
			return fmt.Errorf("%w: unknown field %q", ErrInvalid, field)
		}
	}
	return nil
}

func cloneCell(cell Cell) Cell {
	return Cell{Value: cell.Value, Sources: cell.Sources.Clone(), factField: cell.factField, sourceBound: cell.sourceBound}
}

func cloneRow(source Row) Row {
	row := Row{Key: source.Key, Cells: make(map[string]Cell, len(source.Cells)), Lineage: source.Lineage.Clone()}
	for field, cell := range source.Cells {
		row.Cells[field] = cloneCell(cell)
	}
	return row
}

func equalValues(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
