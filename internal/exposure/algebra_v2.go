package exposure

import (
	"fmt"
	"sort"
	"strings"
)

// SQLTruth models PostgreSQL three-valued predicate evaluation. Only TRUE
// contributes to positive-support influence.
type SQLTruth uint8

const (
	SQLUnknown SQLTruth = iota
	SQLFalse
	SQLTrue
)

type WitnessItem struct {
	Fact         FactID
	Multiplicity uint64
}

// WitnessMultiset retains join fanout and aggregate multiplicity. Influence
// derives a set from it; derived identity commits to the full multiset.
type WitnessMultiset map[string]WitnessItem

func NewWitness(facts ...FactID) (WitnessMultiset, error) {
	result := make(WitnessMultiset)
	for _, fact := range facts {
		if err := result.Add(fact, 1); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (w WitnessMultiset) Add(fact FactID, multiplicity uint64) error {
	if w == nil || multiplicity == 0 || !fact.IsV2() || (fact.Kind != FactBaseRow && fact.Kind != FactBaseCell) {
		return fmt.Errorf("%w: witness requires a positive V2 base fact", ErrInvalid)
	}
	hash, err := fact.Hash()
	if err != nil {
		return err
	}
	if existing, present := w[hash]; present {
		set, _ := NewFactSet(existing.Fact)
		if err := set.Add(fact); err != nil {
			return err
		}
		if ^uint64(0)-existing.Multiplicity < multiplicity {
			return fmt.Errorf("%w: witness multiplicity overflow", ErrInvalid)
		}
		existing.Multiplicity += multiplicity
		w[hash] = existing
		return nil
	}
	w[hash] = WitnessItem{Fact: fact, Multiplicity: multiplicity}
	return nil
}

func (w WitnessMultiset) Merge(other WitnessMultiset) error {
	for _, item := range other {
		if err := w.Add(item.Fact, item.Multiplicity); err != nil {
			return err
		}
	}
	return nil
}

func (w WitnessMultiset) Clone() WitnessMultiset {
	result := make(WitnessMultiset, len(w))
	for hash, item := range w {
		result[hash] = item
	}
	return result
}

func (w WitnessMultiset) Support() (FactSet, error) {
	result := make(FactSet, len(w))
	for _, item := range w {
		if err := result.Add(item.Fact); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (w WitnessMultiset) Commitment() (string, error) {
	if w == nil {
		return "", fmt.Errorf("%w: nil witness", ErrInvalid)
	}
	hashes := make([]string, 0, len(w))
	for hash := range w {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	values := make([]string, 0, len(hashes)*2)
	for _, hash := range hashes {
		values = append(values, hash, fmt.Sprintf("%020d", w[hash].Multiplicity))
	}
	if len(values) == 0 {
		values = append(values, "empty")
	}
	return ComposeCanonicalKeyV2("witness-multiset", values...)
}

type FieldV2 struct {
	ID      string
	SQLType string
}

type BaseRowV2 struct {
	EntityKey string
	Values    map[string]any
}

type RowOriginV2 struct {
	StableRole      string
	SourceNamespace string
	EntityKey       string
}

type CellV2 struct {
	Value       any
	SQLType     string
	Support     FactSet
	Witness     WitnessMultiset
	Expression  string
	ReleaseFact *FactID
}

type AnnotatedRowV2 struct {
	Key        string
	Cells      map[string]CellV2
	RowSupport FactSet
	RowWitness WitnessMultiset
	Origins    []RowOriginV2
}

type RelationV2 struct {
	Fields         []FieldV2
	Rows           []AnnotatedRowV2
	SnapshotBundle []SnapshotBinding
	CanonicalOrder bool
}

type BaseRelationSpecV2 struct {
	SourceNamespace string
	Snapshot        string
	StableRole      string
	Fields          []FieldV2
	Rows            []BaseRowV2
}

func ScanV2(spec BaseRelationSpecV2) (RelationV2, error) {
	if invalidToken(spec.SourceNamespace) || invalidToken(spec.Snapshot) || invalidToken(spec.StableRole) || len(spec.Fields) == 0 {
		return RelationV2{}, fmt.Errorf("%w: V2 scan metadata is incomplete", ErrInvalid)
	}
	seenFields := make(map[string]struct{}, len(spec.Fields))
	for _, field := range spec.Fields {
		if invalidToken(field.ID) || normalizeSQLType(field.SQLType) == "" {
			return RelationV2{}, fmt.Errorf("%w: V2 scan field is incomplete", ErrInvalid)
		}
		if _, duplicate := seenFields[field.ID]; duplicate {
			return RelationV2{}, fmt.Errorf("%w: duplicate field %q", ErrInvalid, field.ID)
		}
		seenFields[field.ID] = struct{}{}
	}
	result := RelationV2{Fields: append([]FieldV2(nil), spec.Fields...), SnapshotBundle: []SnapshotBinding{{SourceNamespace: spec.SourceNamespace, Snapshot: spec.Snapshot}}}
	seenRows := make(map[string]struct{}, len(spec.Rows))
	for _, input := range spec.Rows {
		if invalidToken(input.EntityKey) {
			return RelationV2{}, fmt.Errorf("%w: base entity key is required", ErrInvalid)
		}
		if _, duplicate := seenRows[input.EntityKey]; duplicate {
			return RelationV2{}, fmt.Errorf("%w: duplicate base entity key", ErrInvalid)
		}
		seenRows[input.EntityKey] = struct{}{}
		rowFact, err := NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, input.EntityKey)
		if err != nil {
			return RelationV2{}, err
		}
		rowSupport, _ := NewFactSet(rowFact)
		rowWitness, _ := NewWitness(rowFact)
		row := AnnotatedRowV2{Key: input.EntityKey, Cells: make(map[string]CellV2, len(spec.Fields)),
			RowSupport: rowSupport, RowWitness: rowWitness,
			Origins: []RowOriginV2{{StableRole: spec.StableRole, SourceNamespace: spec.SourceNamespace, EntityKey: input.EntityKey}}}
		for _, field := range spec.Fields {
			value, present := input.Values[field.ID]
			if !present {
				return RelationV2{}, fmt.Errorf("%w: row %q misses %q", ErrInvalid, input.EntityKey, field.ID)
			}
			fact, err := NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, input.EntityKey, field.ID, field.SQLType, value)
			if err != nil {
				return RelationV2{}, err
			}
			support, _ := NewFactSet(fact)
			witness, _ := NewWitness(fact)
			factCopy := fact
			row.Cells[field.ID] = CellV2{Value: value, SQLType: normalizeSQLType(field.SQLType), Support: support,
				Witness: witness, Expression: spec.SourceNamespace + "." + field.ID, ReleaseFact: &factCopy}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func SelectV2(input RelationV2, predicateFields []string, predicate func(AnnotatedRowV2) SQLTruth) (RelationV2, error) {
	if predicate == nil {
		return RelationV2{}, fmt.Errorf("%w: V2 predicate is required", ErrInvalid)
	}
	if err := requireFieldsV2(input, predicateFields); err != nil {
		return RelationV2{}, err
	}
	result := cloneRelationShapeV2(input)
	for _, source := range input.Rows {
		if predicate(source) != SQLTrue {
			continue
		}
		row := cloneRowV2(source)
		for _, field := range predicateFields {
			if err := row.RowSupport.MergeChecked(source.Cells[field].Support); err != nil {
				return RelationV2{}, err
			}
			if err := row.RowWitness.Merge(source.Cells[field].Witness); err != nil {
				return RelationV2{}, err
			}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func ProjectV2(input RelationV2, fields ...string) (RelationV2, error) {
	if err := requireFieldsV2(input, fields); err != nil {
		return RelationV2{}, err
	}
	types := fieldTypeMapV2(input)
	result := RelationV2{SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...), CanonicalOrder: input.CanonicalOrder}
	for _, field := range fields {
		result.Fields = append(result.Fields, FieldV2{ID: field, SQLType: types[field]})
	}
	for _, source := range input.Rows {
		row := AnnotatedRowV2{Key: source.Key, Cells: make(map[string]CellV2, len(fields)), RowSupport: source.RowSupport.Clone(),
			RowWitness: source.RowWitness.Clone(), Origins: append([]RowOriginV2(nil), source.Origins...)}
		for _, field := range fields {
			row.Cells[field] = cloneCellV2(source.Cells[field])
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

// JoinV2 uses Catalog stable role IDs, never SQL aliases. Swapping operands
// leaves both the schema IDs and JoinRowKey unchanged.
func JoinV2(left, right RelationV2, leftKey, rightKey string) (RelationV2, error) {
	if err := requireFieldsV2(left, []string{leftKey}); err != nil {
		return RelationV2{}, err
	}
	if err := requireFieldsV2(right, []string{rightKey}); err != nil {
		return RelationV2{}, err
	}
	bundle, err := mergeSnapshotBundles(left.SnapshotBundle, right.SnapshotBundle)
	if err != nil {
		return RelationV2{}, err
	}
	fieldMap := make(map[string]FieldV2)
	for _, relation := range []RelationV2{left, right} {
		for _, field := range relation.Fields {
			id := field.ID
			if _, duplicate := fieldMap[id]; duplicate {
				return RelationV2{}, fmt.Errorf("%w: join requires stable role-qualified field IDs", ErrInvalid)
			}
			fieldMap[id] = field
		}
	}
	fields := make([]FieldV2, 0, len(fieldMap))
	for _, field := range fieldMap {
		fields = append(fields, field)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].ID < fields[j].ID })
	result := RelationV2{Fields: fields, SnapshotBundle: bundle}
	for _, leftRow := range left.Rows {
		for _, rightRow := range right.Rows {
			if !equalValues(leftRow.Cells[leftKey].Value, rightRow.Cells[rightKey].Value) {
				continue
			}
			origins := append(append([]RowOriginV2(nil), leftRow.Origins...), rightRow.Origins...)
			key, err := joinRowKeyV2(origins)
			if err != nil {
				return RelationV2{}, err
			}
			row := AnnotatedRowV2{Key: key, Cells: make(map[string]CellV2, len(fields)), RowSupport: leftRow.RowSupport.Clone(),
				RowWitness: leftRow.RowWitness.Clone(), Origins: origins}
			if err := row.RowSupport.MergeChecked(rightRow.RowSupport); err != nil {
				return RelationV2{}, err
			}
			for _, cell := range []CellV2{leftRow.Cells[leftKey], rightRow.Cells[rightKey]} {
				if err := row.RowSupport.MergeChecked(cell.Support); err != nil {
					return RelationV2{}, err
				}
				if err := row.RowWitness.Merge(cell.Witness); err != nil {
					return RelationV2{}, err
				}
			}
			if err := row.RowWitness.Merge(rightRow.RowWitness); err != nil {
				return RelationV2{}, err
			}
			for id, cell := range leftRow.Cells {
				row.Cells[id] = cloneCellV2(cell)
			}
			for id, cell := range rightRow.Cells {
				row.Cells[id] = cloneCellV2(cell)
			}
			result.Rows = append(result.Rows, row)
		}
	}
	return result, nil
}

func UnionDistinctV2(left, right RelationV2) (RelationV2, error) {
	if !sameSchemaV2(left.Fields, right.Fields) {
		return RelationV2{}, fmt.Errorf("%w: union schemas differ", ErrInvalid)
	}
	bundle, err := mergeSnapshotBundles(left.SnapshotBundle, right.SnapshotBundle)
	if err != nil {
		return RelationV2{}, err
	}
	result := RelationV2{Fields: append([]FieldV2(nil), left.Fields...), SnapshotBundle: bundle}
	index := make(map[string]int)
	for _, source := range append(append([]AnnotatedRowV2(nil), left.Rows...), right.Rows...) {
		components := make([]string, 0, len(result.Fields)*2+1)
		components = append(components, normalizedSchemaV2(result.Fields))
		for _, field := range result.Fields {
			canonical, err := CanonicalSQLValue(field.SQLType, source.Cells[field.ID].Value)
			if err != nil {
				return RelationV2{}, err
			}
			components = append(components, field.SQLType, canonical)
		}
		key, _ := ComposeCanonicalKeyV2("union-distinct-row", components...)
		if existing, present := index[key]; present {
			row := &result.Rows[existing]
			if err := row.RowSupport.MergeChecked(source.RowSupport); err != nil {
				return RelationV2{}, err
			}
			if err := row.RowWitness.Merge(source.RowWitness); err != nil {
				return RelationV2{}, err
			}
			for _, field := range result.Fields {
				cell := row.Cells[field.ID]
				if err := cell.Support.MergeChecked(source.Cells[field.ID].Support); err != nil {
					return RelationV2{}, err
				}
				if err := cell.Witness.Merge(source.Cells[field.ID].Witness); err != nil {
					return RelationV2{}, err
				}
				cell.ReleaseFact = nil
				cell.Expression = "union(" + field.ID + ")"
				row.Cells[field.ID] = cell
			}
			continue
		}
		row := cloneRowV2(source)
		row.Key = key
		for _, field := range result.Fields {
			cell := row.Cells[field.ID]
			cell.ReleaseFact = nil
			cell.Expression = "union(" + field.ID + ")"
			row.Cells[field.ID] = cell
		}
		index[key] = len(result.Rows)
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

type AggregateSpecV2 struct {
	Function   string
	Field      string
	OutputID   string
	OutputType string
}

// AggregateFromResultsV2 annotates PostgreSQL-computed aggregate rows. This
// avoids reimplementing PostgreSQL numeric/collation semantics in Go while
// still applying the formal group and witness rules to trusted query results.
func AggregateFromResultsV2(input RelationV2, groupFields []string, specs []AggregateSpecV2, outputRows []map[string]any) (RelationV2, error) {
	if err := requireFieldsV2(input, groupFields); err != nil {
		return RelationV2{}, err
	}
	for _, spec := range specs {
		function := strings.ToLower(strings.TrimSpace(spec.Function))
		if function != "count" && function != "sum" && function != "min" && function != "max" {
			return RelationV2{}, fmt.Errorf("%w: unsupported V2 aggregate %q", ErrInvalid, spec.Function)
		}
		if spec.Field != "*" {
			if err := requireFieldsV2(input, []string{spec.Field}); err != nil {
				return RelationV2{}, err
			}
		}
		if invalidToken(spec.OutputID) || normalizeSQLType(spec.OutputType) == "" {
			return RelationV2{}, fmt.Errorf("%w: aggregate output metadata is incomplete", ErrInvalid)
		}
	}
	types := fieldTypeMapV2(input)
	result := RelationV2{SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...)}
	for _, field := range groupFields {
		result.Fields = append(result.Fields, FieldV2{ID: field, SQLType: types[field]})
	}
	for _, spec := range specs {
		result.Fields = append(result.Fields, FieldV2{ID: spec.OutputID, SQLType: normalizeSQLType(spec.OutputType)})
	}
	for _, output := range outputRows {
		groupComponents := make([]string, 0, len(groupFields))
		for _, field := range groupFields {
			canonical, err := CanonicalSQLValue(types[field], output[field])
			if err != nil {
				return RelationV2{}, err
			}
			groupComponents = append(groupComponents, inputExpressionV2(input, field)+"\x00"+types[field]+"\x00"+canonical)
		}
		if len(groupComponents) == 0 {
			groupComponents = append(groupComponents, "global")
		}
		sort.Strings(groupComponents)
		key, _ := ComposeCanonicalKeyV2("group-row", groupComponents...)
		members := matchingGroupRowsV2(input, groupFields, output)
		if len(groupFields) > 0 && len(members) == 0 {
			return RelationV2{}, fmt.Errorf("%w: aggregate output group has no positive source row", ErrInvalid)
		}
		row := AnnotatedRowV2{Key: key, Cells: make(map[string]CellV2, len(result.Fields)), RowSupport: make(FactSet), RowWitness: make(WitnessMultiset)}
		for _, member := range members {
			if err := row.RowSupport.MergeChecked(member.RowSupport); err != nil {
				return RelationV2{}, err
			}
			if err := row.RowWitness.Merge(member.RowWitness); err != nil {
				return RelationV2{}, err
			}
		}
		for _, field := range groupFields {
			support := make(FactSet)
			witness := make(WitnessMultiset)
			for _, member := range members {
				if err := support.MergeChecked(member.Cells[field].Support); err != nil {
					return RelationV2{}, err
				}
				if err := witness.Merge(member.Cells[field].Witness); err != nil {
					return RelationV2{}, err
				}
			}
			row.Cells[field] = CellV2{Value: output[field], SQLType: types[field], Support: support, Witness: witness,
				Expression: "group(" + inputExpressionV2(input, field) + ")"}
		}
		for _, spec := range specs {
			support := make(FactSet)
			witness := make(WitnessMultiset)
			for _, member := range members {
				if spec.Field == "*" {
					if err := support.MergeChecked(member.RowSupport); err != nil {
						return RelationV2{}, err
					}
					if err := witness.Merge(member.RowWitness); err != nil {
						return RelationV2{}, err
					}
					continue
				}
				if err := support.MergeChecked(member.Cells[spec.Field].Support); err != nil {
					return RelationV2{}, err
				}
				if err := witness.Merge(member.Cells[spec.Field].Witness); err != nil {
					return RelationV2{}, err
				}
			}
			expression := strings.ToLower(spec.Function) + "("
			if spec.Field == "*" {
				expression += "*"
			} else {
				expression += inputExpressionV2(input, spec.Field)
			}
			expression += ")"
			row.Cells[spec.OutputID] = CellV2{Value: output[spec.OutputID], SQLType: normalizeSQLType(spec.OutputType),
				Support: support, Witness: witness, Expression: expression}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, nil
}

func PageV2(input RelationV2, offset, limit int) (RelationV2, error) {
	if offset < 0 || limit < 0 || !input.CanonicalOrder {
		return RelationV2{}, fmt.Errorf("%w: V2 page requires non-negative bounds and a canonical total order", ErrInvalid)
	}
	start := minInt(offset, len(input.Rows))
	end := len(input.Rows)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	result := cloneRelationShapeV2(input)
	for _, row := range input.Rows[start:end] {
		result.Rows = append(result.Rows, cloneRowV2(row))
	}
	return result, nil
}

func ObserveV2(input RelationV2, visibleFields ...string) (Observation, error) {
	if len(visibleFields) == 0 {
		for _, field := range input.Fields {
			visibleFields = append(visibleFields, field.ID)
		}
	}
	if err := requireFieldsV2(input, visibleFields); err != nil {
		return Observation{}, err
	}
	release := make(FactSet)
	influence := make(FactSet)
	for _, row := range input.Rows {
		if err := influence.MergeChecked(row.RowSupport); err != nil {
			return Observation{}, err
		}
		for _, field := range visibleFields {
			cell := row.Cells[field]
			if err := influence.MergeChecked(cell.Support); err != nil {
				return Observation{}, err
			}
			var fact FactID
			if cell.ReleaseFact != nil {
				fact = *cell.ReleaseFact
			} else {
				commitment, err := cell.Witness.Commitment()
				if err != nil {
					return Observation{}, err
				}
				fact, err = NewDerivedFactV2(input.SnapshotBundle, row.Key, cell.Expression, cell.SQLType, cell.Value, commitment)
				if err != nil {
					return Observation{}, err
				}
			}
			if err := release.Add(fact); err != nil {
				return Observation{}, err
			}
		}
	}
	return (Observation{ProfileVersion: ProfileV2, Release: release.Values(), Influence: influence.Values()}).Normalize()
}

func joinRowKeyV2(origins []RowOriginV2) (string, error) {
	values := make([]string, 0, len(origins))
	for _, origin := range origins {
		if invalidToken(origin.StableRole) || invalidToken(origin.SourceNamespace) || invalidToken(origin.EntityKey) {
			return "", fmt.Errorf("%w: join origin metadata is incomplete", ErrInvalid)
		}
		values = append(values, origin.StableRole+"\x00"+origin.SourceNamespace+"\x00"+origin.EntityKey)
	}
	sort.Strings(values)
	return ComposeCanonicalKeyV2("join-row", values...)
}

func mergeSnapshotBundles(left, right []SnapshotBinding) ([]SnapshotBinding, error) {
	values := make(map[string]string, len(left)+len(right))
	for _, binding := range append(append([]SnapshotBinding(nil), left...), right...) {
		if snapshot, present := values[binding.SourceNamespace]; present && snapshot != binding.Snapshot {
			return nil, fmt.Errorf("%w: one source namespace is bound to different snapshots", ErrInvalid)
		}
		values[binding.SourceNamespace] = binding.Snapshot
	}
	result := make([]SnapshotBinding, 0, len(values))
	for namespace, snapshot := range values {
		result = append(result, SnapshotBinding{SourceNamespace: namespace, Snapshot: snapshot})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SourceNamespace < result[j].SourceNamespace })
	return result, nil
}

func matchingGroupRowsV2(input RelationV2, fields []string, output map[string]any) []AnnotatedRowV2 {
	result := make([]AnnotatedRowV2, 0)
	for _, row := range input.Rows {
		matches := true
		for _, field := range fields {
			if !equalValues(row.Cells[field].Value, output[field]) {
				matches = false
				break
			}
		}
		if matches {
			result = append(result, row)
		}
	}
	return result
}

func requireFieldsV2(relation RelationV2, fields []string) error {
	available := fieldTypeMapV2(relation)
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("%w: duplicate field %q", ErrInvalid, field)
		}
		seen[field] = struct{}{}
		if _, present := available[field]; !present {
			return fmt.Errorf("%w: unknown field %q", ErrInvalid, field)
		}
	}
	return nil
}

func fieldTypeMapV2(relation RelationV2) map[string]string {
	result := make(map[string]string, len(relation.Fields))
	for _, field := range relation.Fields {
		result[field.ID] = normalizeSQLType(field.SQLType)
	}
	return result
}

func inputExpressionV2(relation RelationV2, field string) string {
	if len(relation.Rows) > 0 {
		return relation.Rows[0].Cells[field].Expression
	}
	return field
}

func cloneRelationShapeV2(input RelationV2) RelationV2 {
	return RelationV2{Fields: append([]FieldV2(nil), input.Fields...), SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...), CanonicalOrder: input.CanonicalOrder}
}

func cloneCellV2(source CellV2) CellV2 {
	result := CellV2{Value: source.Value, SQLType: source.SQLType, Support: source.Support.Clone(), Witness: source.Witness.Clone(), Expression: source.Expression}
	if source.ReleaseFact != nil {
		fact := *source.ReleaseFact
		result.ReleaseFact = &fact
	}
	return result
}

func cloneRowV2(source AnnotatedRowV2) AnnotatedRowV2 {
	result := AnnotatedRowV2{Key: source.Key, Cells: make(map[string]CellV2, len(source.Cells)), RowSupport: source.RowSupport.Clone(),
		RowWitness: source.RowWitness.Clone(), Origins: append([]RowOriginV2(nil), source.Origins...)}
	for field, cell := range source.Cells {
		result.Cells[field] = cloneCellV2(cell)
	}
	return result
}

func sameSchemaV2(left, right []FieldV2) bool {
	return normalizedSchemaV2(left) == normalizedSchemaV2(right)
}

func normalizedSchemaV2(fields []FieldV2) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field.ID+":"+normalizeSQLType(field.SQLType))
	}
	return strings.Join(parts, "\x00")
}
