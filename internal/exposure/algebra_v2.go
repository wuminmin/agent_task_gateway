package exposure

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// SQLTruth models PostgreSQL three-valued predicate evaluation. Only TRUE
// contributes to the positive-output dependency footprint.
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

// WitnessMultiset retains join fanout and aggregate multiplicity. The
// positive-output dependency footprint derives a set from it; derived identity
// commits to the full multiset.
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

// MergeMax is the idempotent alternative merge used by UNION DISTINCT. It
// preserves multiplicity already present inside either proof (for example from
// join fanout), while repeating the same UNION branch cannot double it.
func (w WitnessMultiset) MergeMax(other WitnessMultiset) error {
	for hash, item := range other {
		if existing, present := w[hash]; present {
			set, _ := NewFactSet(existing.Fact)
			if err := set.Add(item.Fact); err != nil {
				return err
			}
			if item.Multiplicity > existing.Multiplicity {
				w[hash] = item
			}
			continue
		}
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
	ID                     string `json:"id"`
	SQLType                string `json:"sql_type"`
	Expression             string `json:"expression,omitempty"`
	Collation              string `json:"collation,omitempty"`
	CollationVersion       string `json:"collation_version,omitempty"`
	CollationDeterministic bool   `json:"collation_deterministic,omitempty"`
}

type BaseRowV2 struct {
	EntityKey string         `json:"entity_key"`
	Values    map[string]any `json:"values"`
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

// JoinPredicateV2 is one typed SQL equality in a conjunctive equijoin. Field
// identifiers are stable, role-qualified Catalog IDs rather than SQL aliases.
type JoinPredicateV2 struct {
	LeftField  string
	RightField string
}

func ScanV2(spec BaseRelationSpecV2) (RelationV2, error) {
	if invalidToken(spec.SourceNamespace) || invalidToken(spec.Snapshot) || invalidToken(spec.StableRole) || len(spec.Fields) == 0 {
		return RelationV2{}, fmt.Errorf("%w: V2 scan metadata is incomplete", ErrInvalid)
	}
	seenFields := make(map[string]struct{}, len(spec.Fields))
	canonicalFields := make([]FieldV2, 0, len(spec.Fields))
	for _, field := range spec.Fields {
		typeName, err := CanonicalSQLTypeV2(field.SQLType)
		if invalidToken(field.ID) || err != nil || validateFieldCollationV2(typeName, field) != nil {
			return RelationV2{}, fmt.Errorf("%w: V2 scan field is incomplete", ErrInvalid)
		}
		if _, duplicate := seenFields[field.ID]; duplicate {
			return RelationV2{}, fmt.Errorf("%w: duplicate field %q", ErrInvalid, field.ID)
		}
		seenFields[field.ID] = struct{}{}
		field.SQLType = typeName
		field.Expression = spec.SourceNamespace + "." + field.ID
		canonicalFields = append(canonicalFields, field)
	}
	result := RelationV2{Fields: canonicalFields, SnapshotBundle: []SnapshotBinding{{SourceNamespace: spec.SourceNamespace, Snapshot: spec.Snapshot}}}
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
		for _, field := range canonicalFields {
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
			row.Cells[field.ID] = CellV2{Value: value, SQLType: field.SQLType, Support: support,
				Witness: witness, Expression: field.Expression, ReleaseFact: &factCopy}
		}
		result.Rows = append(result.Rows, row)
	}
	if err := ValidateRelationV2(result); err != nil {
		return RelationV2{}, err
	}
	return result, nil
}

func SelectV2(input RelationV2, predicateFields []string, predicate func(AnnotatedRowV2) SQLTruth) (RelationV2, error) {
	if err := ValidateRelationV2(input); err != nil {
		return RelationV2{}, err
	}
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
	return result, ValidateRelationV2(result)
}

func ProjectV2(input RelationV2, fields ...string) (RelationV2, error) {
	if err := ValidateRelationV2(input); err != nil {
		return RelationV2{}, err
	}
	if len(fields) == 0 {
		return RelationV2{}, fmt.Errorf("%w: V2 projection requires at least one field", ErrInvalid)
	}
	if err := requireFieldsV2(input, fields); err != nil {
		return RelationV2{}, err
	}
	result := RelationV2{SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...), CanonicalOrder: input.CanonicalOrder}
	for _, field := range fields {
		result.Fields = append(result.Fields, fieldDefinitionV2(input, field))
	}
	for _, source := range input.Rows {
		row := AnnotatedRowV2{Key: source.Key, Cells: make(map[string]CellV2, len(fields)), RowSupport: source.RowSupport.Clone(),
			RowWitness: source.RowWitness.Clone(), Origins: append([]RowOriginV2(nil), source.Origins...)}
		for _, field := range fields {
			row.Cells[field] = cloneCellV2(source.Cells[field])
		}
		result.Rows = append(result.Rows, row)
	}
	return result, ValidateRelationV2(result)
}

// JoinV2 uses Catalog stable role IDs, never SQL aliases. Swapping operands
// leaves both the schema IDs and JoinRowKey unchanged.
func JoinV2(left, right RelationV2, leftKey, rightKey string) (RelationV2, error) {
	return JoinOnV2(left, right, []JoinPredicateV2{{LeftField: leftKey, RightField: rightKey}})
}

// JoinOnV2 implements a conjunctive PostgreSQL equijoin. Every comparison
// must be TRUE; FALSE and UNKNOWN both exclude the pair. The result row key is
// built from the two immediate input-row identities, making Join closed over
// Scan, Select, Project, Union, Group, Page, and nested Join results.
func JoinOnV2(left, right RelationV2, predicates []JoinPredicateV2) (RelationV2, error) {
	if err := ValidateRelationV2(left); err != nil {
		return RelationV2{}, err
	}
	if err := ValidateRelationV2(right); err != nil {
		return RelationV2{}, err
	}
	if len(predicates) == 0 {
		return RelationV2{}, fmt.Errorf("%w: V2 equijoin requires at least one equality", ErrInvalid)
	}
	leftKeys := make([]string, 0, len(predicates))
	rightKeys := make([]string, 0, len(predicates))
	types := make([]string, 0, len(predicates))
	seenPredicates := make(map[string]struct{}, len(predicates))
	leftTypes, rightTypes := fieldTypeMapV2(left), fieldTypeMapV2(right)
	for _, predicate := range predicates {
		if err := requireFieldsV2(left, []string{predicate.LeftField}); err != nil {
			return RelationV2{}, err
		}
		if err := requireFieldsV2(right, []string{predicate.RightField}); err != nil {
			return RelationV2{}, err
		}
		key := predicate.LeftField + "\x00" + predicate.RightField
		if _, duplicate := seenPredicates[key]; duplicate {
			return RelationV2{}, fmt.Errorf("%w: duplicate V2 join predicate", ErrInvalid)
		}
		seenPredicates[key] = struct{}{}
		leftType, err := CanonicalSQLTypeV2(leftTypes[predicate.LeftField])
		if err != nil {
			return RelationV2{}, err
		}
		rightType, err := CanonicalSQLTypeV2(rightTypes[predicate.RightField])
		if err != nil {
			return RelationV2{}, err
		}
		if leftType != rightType {
			return RelationV2{}, fmt.Errorf("%w: V2 join keys require the same canonical SQL type", ErrInvalid)
		}
		leftField := fieldDefinitionV2(left, predicate.LeftField)
		rightField := fieldDefinitionV2(right, predicate.RightField)
		if isCollatableTypeV2(leftType) && (leftField.Collation != rightField.Collation || leftField.CollationVersion != rightField.CollationVersion ||
			!leftField.CollationDeterministic || !rightField.CollationDeterministic) {
			return RelationV2{}, fmt.Errorf("%w: V2 join keys require the same deterministic collation", ErrInvalid)
		}
		leftKeys = append(leftKeys, predicate.LeftField)
		rightKeys = append(rightKeys, predicate.RightField)
		types = append(types, leftType)
	}
	if err := requireFieldsV2(left, leftKeys); err != nil {
		return RelationV2{}, err
	}
	if err := requireFieldsV2(right, rightKeys); err != nil {
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
	rightIndex := make(map[string][]AnnotatedRowV2, len(right.Rows))
	for _, rightRow := range right.Rows {
		key, comparable, err := joinEqualityKeyV2(rightRow, rightKeys, types)
		if err != nil {
			return RelationV2{}, err
		}
		if comparable {
			rightIndex[key] = append(rightIndex[key], rightRow)
		}
	}
	for _, leftRow := range left.Rows {
		key, comparable, err := joinEqualityKeyV2(leftRow, leftKeys, types)
		if err != nil {
			return RelationV2{}, err
		}
		if !comparable {
			continue
		}
		for _, rightRow := range rightIndex[key] {
			origins := append(append([]RowOriginV2(nil), leftRow.Origins...), rightRow.Origins...)
			leftIdentity, err := relationRowIdentityV2(left, leftRow)
			if err != nil {
				return RelationV2{}, err
			}
			rightIdentity, err := relationRowIdentityV2(right, rightRow)
			if err != nil {
				return RelationV2{}, err
			}
			key, err := joinRowKeyV2(leftIdentity, rightIdentity)
			if err != nil {
				return RelationV2{}, err
			}
			row := AnnotatedRowV2{Key: key, Cells: make(map[string]CellV2, len(fields)), RowSupport: leftRow.RowSupport.Clone(),
				RowWitness: leftRow.RowWitness.Clone(), Origins: origins}
			if err := row.RowSupport.MergeChecked(rightRow.RowSupport); err != nil {
				return RelationV2{}, err
			}
			for index := range predicates {
				for _, cell := range []CellV2{leftRow.Cells[leftKeys[index]], rightRow.Cells[rightKeys[index]]} {
					if err := row.RowSupport.MergeChecked(cell.Support); err != nil {
						return RelationV2{}, err
					}
					if err := row.RowWitness.Merge(cell.Witness); err != nil {
						return RelationV2{}, err
					}
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
	return result, ValidateRelationV2(result)
}

// joinEqualityKeyV2 is an injective, in-memory encoding of a conjunctive
// equality tuple. NULL has no key because SQL equality with NULL is UNKNOWN.
func joinEqualityKeyV2(row AnnotatedRowV2, fields, types []string) (string, bool, error) {
	var encoded bytes.Buffer
	writeCanonicalUint64(&encoded, uint64(len(fields)))
	for index, field := range fields {
		canonical, err := CanonicalSQLValue(types[index], row.Cells[field].Value)
		if err != nil {
			return "", false, err
		}
		if canonical == "null" {
			return "", false, nil
		}
		writeCanonicalString(&encoded, types[index])
		writeCanonicalString(&encoded, canonical)
	}
	return encoded.String(), true, nil
}

func UnionDistinctV2(left, right RelationV2) (RelationV2, error) {
	if err := ValidateRelationV2(left); err != nil {
		return RelationV2{}, err
	}
	if err := ValidateRelationV2(right); err != nil {
		return RelationV2{}, err
	}
	if !sameSchemaV2(left.Fields, right.Fields) {
		return RelationV2{}, fmt.Errorf("%w: union schemas differ", ErrInvalid)
	}
	bundle, err := mergeSnapshotBundles(left.SnapshotBundle, right.SnapshotBundle)
	if err != nil {
		return RelationV2{}, err
	}
	// An identical branch is foldable only when its full typed tuples are already
	// distinct. Otherwise SQL bag semantics still require duplicate elimination.
	// This mirrors the normal form's set-valued idempotence without suppressing
	// the equivalence-field dependencies of a real DISTINCT operation.
	if reflect.DeepEqual(left, right) {
		distinct, err := relationTuplesDistinctV2(left)
		if err != nil {
			return RelationV2{}, err
		}
		if distinct {
			return cloneRelationV2(left), nil
		}
	}
	resultFields := append([]FieldV2(nil), left.Fields...)
	rightFieldIndex := make(map[string]FieldV2, len(right.Fields))
	for _, field := range right.Fields {
		rightFieldIndex[field.ID] = field
	}
	for index := range resultFields {
		leftExpression := resultFields[index].Expression
		rightExpression := rightFieldIndex[resultFields[index].ID].Expression
		if leftExpression != rightExpression {
			expressions := []string{leftExpression, rightExpression}
			sort.Strings(expressions)
			resultFields[index].Expression = "union(" + strings.Join(expressions, ",") + ")"
		}
	}
	sort.Slice(resultFields, func(i, j int) bool { return resultFields[i].ID < resultFields[j].ID })
	result := RelationV2{Fields: resultFields, SnapshotBundle: bundle}
	index := make(map[string]int)
	type unionInput struct {
		relation RelationV2
		row      AnnotatedRowV2
	}
	inputs := make([]unionInput, 0, len(left.Rows)+len(right.Rows))
	for _, row := range left.Rows {
		inputs = append(inputs, unionInput{relation: left, row: row})
	}
	for _, row := range right.Rows {
		inputs = append(inputs, unionInput{relation: right, row: row})
	}
	for _, input := range inputs {
		source := input.row
		rowSupport, rowWitness, err := unionDistinctRowDependencyV2(source, result.Fields)
		if err != nil {
			return RelationV2{}, err
		}
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
			if err := row.RowSupport.MergeChecked(rowSupport); err != nil {
				return RelationV2{}, err
			}
			if err := row.RowWitness.MergeMax(rowWitness); err != nil {
				return RelationV2{}, err
			}
			for _, field := range result.Fields {
				cell := row.Cells[field.ID]
				sourceCell := source.Cells[field.ID]
				if err := cell.Support.MergeChecked(source.Cells[field.ID].Support); err != nil {
					return RelationV2{}, err
				}
				if err := cell.Witness.MergeMax(sourceCell.Witness); err != nil {
					return RelationV2{}, err
				}
				sourceFact, err := materializeCellReleaseV2(input.relation, source, sourceCell)
				if err != nil {
					return RelationV2{}, err
				}
				if cell.ReleaseFact == nil || !sameFactV2(*cell.ReleaseFact, sourceFact) {
					cell.ReleaseFact = nil
					cell.Expression = field.Expression
				}
				row.Cells[field.ID] = cell
			}
			continue
		}
		row := cloneRowV2(source)
		row.Key = key
		row.RowSupport = rowSupport
		row.RowWitness = rowWitness
		for _, field := range result.Fields {
			cell := row.Cells[field.ID]
			fact, err := materializeCellReleaseV2(input.relation, source, cell)
			if err != nil {
				return RelationV2{}, err
			}
			cell.ReleaseFact = &fact
			row.Cells[field.ID] = cell
		}
		index[key] = len(result.Rows)
		result.Rows = append(result.Rows, row)
	}
	return result, ValidateRelationV2(result)
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
	if err := ValidateRelationV2(input); err != nil {
		return RelationV2{}, err
	}
	if err := requireFieldsV2(input, groupFields); err != nil {
		return RelationV2{}, err
	}
	if len(groupFields)+len(specs) == 0 {
		return RelationV2{}, fmt.Errorf("%w: V2 group requires a key or aggregate", ErrInvalid)
	}
	if len(groupFields) == 0 && len(outputRows) != 1 {
		return RelationV2{}, fmt.Errorf("%w: a global PostgreSQL aggregate has exactly one output row", ErrInvalid)
	}
	outputIDs := make(map[string]struct{}, len(groupFields)+len(specs))
	for _, field := range groupFields {
		outputIDs[field] = struct{}{}
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
		if invalidToken(spec.OutputID) {
			return RelationV2{}, fmt.Errorf("%w: aggregate output metadata is incomplete", ErrInvalid)
		}
		if _, duplicate := outputIDs[spec.OutputID]; duplicate {
			return RelationV2{}, fmt.Errorf("%w: duplicate V2 group output %q", ErrInvalid, spec.OutputID)
		}
		outputIDs[spec.OutputID] = struct{}{}
		outputType, err := CanonicalSQLTypeV2(spec.OutputType)
		if err != nil {
			return RelationV2{}, err
		}
		inputType := ""
		if spec.Field != "*" {
			inputType = fieldTypeMapV2(input)[spec.Field]
		}
		if expectedAggregateOutputTypeV2(function, inputType) != outputType {
			return RelationV2{}, fmt.Errorf("%w: V2 aggregate %s(%s) has the wrong output type", ErrInvalid, function, spec.Field)
		}
	}
	types := fieldTypeMapV2(input)
	result := RelationV2{SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...)}
	for _, field := range groupFields {
		definition := fieldDefinitionV2(input, field)
		definition.Expression = "group(" + inputExpressionV2(input, field) + ")"
		result.Fields = append(result.Fields, definition)
	}
	for _, spec := range specs {
		typeName, _ := CanonicalSQLTypeV2(spec.OutputType)
		outputField := FieldV2{ID: spec.OutputID, SQLType: typeName, Expression: aggregateExpressionV2(input, spec)}
		if isCollatableTypeV2(typeName) && spec.Field != "*" {
			inputField := fieldDefinitionV2(input, spec.Field)
			outputField.Collation = inputField.Collation
			outputField.CollationVersion = inputField.CollationVersion
			outputField.CollationDeterministic = inputField.CollationDeterministic
		}
		result.Fields = append(result.Fields, outputField)
	}
	for _, output := range outputRows {
		groupComponents := make([]string, 0, len(groupFields))
		for _, field := range groupFields {
			if _, present := output[field]; !present {
				return RelationV2{}, fmt.Errorf("%w: aggregate output misses group field %q", ErrInvalid, field)
			}
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
		members, err := matchingGroupRowsV2(input, groupFields, output)
		if err != nil {
			return RelationV2{}, err
		}
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
			// Group membership is established by every key field, even when the
			// caller does not include that key in the final visible field set.
			// Key cells are same-proof inputs for this member, then members compose
			// with ordinary multiplicity-preserving addition.
			for _, field := range groupFields {
				if err := row.RowSupport.MergeChecked(member.Cells[field].Support); err != nil {
					return RelationV2{}, err
				}
				if err := row.RowWitness.Merge(member.Cells[field].Witness); err != nil {
					return RelationV2{}, err
				}
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
				Expression: fieldDefinitionV2(result, field).Expression}
		}
		for _, spec := range specs {
			if _, present := output[spec.OutputID]; !present {
				return RelationV2{}, fmt.Errorf("%w: aggregate output misses value %q", ErrInvalid, spec.OutputID)
			}
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
			typeName, _ := CanonicalSQLTypeV2(spec.OutputType)
			row.Cells[spec.OutputID] = CellV2{Value: output[spec.OutputID], SQLType: typeName,
				Support: support, Witness: witness, Expression: aggregateExpressionV2(input, spec)}
		}
		result.Rows = append(result.Rows, row)
	}
	if len(groupFields) > 0 {
		expectedGroups := make(map[string]struct{})
		for _, source := range input.Rows {
			components := make([]string, 0, len(groupFields))
			for _, field := range groupFields {
				canonical, err := CanonicalSQLValue(types[field], source.Cells[field].Value)
				if err != nil {
					return RelationV2{}, err
				}
				components = append(components, inputExpressionV2(input, field)+"\x00"+types[field]+"\x00"+canonical)
			}
			sort.Strings(components)
			key, _ := ComposeCanonicalKeyV2("group-row", components...)
			expectedGroups[key] = struct{}{}
		}
		if len(result.Rows) != len(expectedGroups) {
			return RelationV2{}, fmt.Errorf("%w: PostgreSQL group oracle omitted or duplicated a positive group", ErrInvalid)
		}
	}
	return result, ValidateRelationV2(result)
}

func expectedAggregateOutputTypeV2(function, input string) string {
	if function == "count" {
		return "bigint"
	}
	if function == "sum" {
		// SUM is admitted only over the exact/integer fragment. IEEE-754 addition
		// is non-associative, so PostgreSQL's float SUM depends on the physical
		// aggregation order, which the relational bag and the typed normal form do
		// not fix. Admitting SUM(real)/SUM(double precision) would break Effect
		// determinism (Theorem~Effect determinism); they fall through to "" and
		// are rejected by AggregateFromResultsV2.
		switch input {
		case "smallint", "integer":
			return "bigint"
		case "bigint":
			return "numeric"
		case "numeric":
			return "numeric"
		}
	}
	if function == "min" || function == "max" {
		switch input {
		case "smallint", "integer", "bigint", "numeric", "real", "double precision",
			"date", "time without time zone", "timestamp with time zone", "timestamp without time zone",
			"text", "character", "character varying":
			return input
		}
	}
	return ""
}

func PageV2(input RelationV2, offset, limit int) (RelationV2, error) {
	if err := ValidateRelationV2(input); err != nil {
		return RelationV2{}, err
	}
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
	return result, ValidateRelationV2(result)
}

func ObserveV2(input RelationV2, visibleFields ...string) (Observation, error) {
	if err := ValidateRelationV2(input); err != nil {
		return Observation{}, err
	}
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
			fact, err := materializeCellReleaseV2(input, row, cell)
			if err != nil {
				return Observation{}, err
			}
			if err := release.Add(fact); err != nil {
				return Observation{}, err
			}
		}
	}
	return (Observation{ProfileVersion: ProfileV2, Release: release.Values(), Influence: influence.Values()}).Normalize()
}

func joinRowKeyV2(leftIdentity, rightIdentity string) (string, error) {
	identities := []string{leftIdentity, rightIdentity}
	sort.Strings(identities)
	return ComposeCanonicalKeyV2("join-row", identities...)
}

func relationRowIdentityV2(relation RelationV2, row AnnotatedRowV2) (string, error) {
	components := []string{normalizedSchemaV2(relation.Fields), row.Key}
	for _, binding := range relation.SnapshotBundle {
		components = append(components, binding.SourceNamespace, binding.Snapshot)
	}
	return ComposeCanonicalKeyV2("relation-row", components...)
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

func matchingGroupRowsV2(input RelationV2, fields []string, output map[string]any) ([]AnnotatedRowV2, error) {
	result := make([]AnnotatedRowV2, 0)
	types := fieldTypeMapV2(input)
	for _, row := range input.Rows {
		matches := true
		for _, field := range fields {
			equal, err := SQLValueNotDistinctV2(types[field], row.Cells[field].Value, output[field])
			if err != nil {
				return nil, err
			}
			if !equal {
				matches = false
				break
			}
		}
		if matches {
			result = append(result, row)
		}
	}
	return result, nil
}

// SQLValueEqualV2 is PostgreSQL "=" over the deterministic V2 scalar domain.
// NULL yields UNKNOWN; jsonb is canonicalized structurally.
func SQLValueEqualV2(sqlType string, left, right any) (SQLTruth, error) {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return SQLUnknown, err
	}
	leftCanonical, err := CanonicalSQLValue(typeName, left)
	if err != nil {
		return SQLUnknown, err
	}
	rightCanonical, err := CanonicalSQLValue(typeName, right)
	if err != nil {
		return SQLUnknown, err
	}
	if leftCanonical == "null" || rightCanonical == "null" {
		return SQLUnknown, nil
	}
	if leftCanonical == rightCanonical {
		return SQLTrue, nil
	}
	return SQLFalse, nil
}

// SQLValueNotDistinctV2 is the equality relation used by GROUP BY, DISTINCT,
// and UNION DISTINCT: two NULLs are equal and one NULL differs from non-NULL.
func SQLValueNotDistinctV2(sqlType string, left, right any) (bool, error) {
	typeName, err := CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return false, err
	}
	leftCanonical, err := CanonicalSQLValue(typeName, left)
	if err != nil {
		return false, err
	}
	rightCanonical, err := CanonicalSQLValue(typeName, right)
	if err != nil {
		return false, err
	}
	return leftCanonical == rightCanonical, nil
}

func materializeCellReleaseV2(relation RelationV2, row AnnotatedRowV2, cell CellV2) (FactID, error) {
	if cell.ReleaseFact != nil {
		return *cell.ReleaseFact, nil
	}
	commitment, err := cell.Witness.Commitment()
	if err != nil {
		return FactID{}, err
	}
	return NewDerivedFactV2(relation.SnapshotBundle, row.Key, cell.Expression, cell.SQLType, cell.Value, commitment)
}

func sameFactV2(left, right FactID) bool {
	leftPayload, leftErr := left.CanonicalPayload()
	rightPayload, rightErr := right.CanonicalPayload()
	return leftErr == nil && rightErr == nil && string(leftPayload) == string(rightPayload)
}

// ValidateRelationV2 checks the representation invariant assumed by every
// inference rule. In particular, dependency annotations contain only V2 base
// facts, witness support equals set support, row keys are unique, and every
// typed cell value belongs to the admitted PostgreSQL scalar domain.
func ValidateRelationV2(relation RelationV2) error {
	if len(relation.Fields) == 0 || len(relation.SnapshotBundle) == 0 {
		return fmt.Errorf("%w: V2 relation requires a non-empty schema and snapshot bundle", ErrInvalid)
	}
	canonicalBundle, err := mergeSnapshotBundles(relation.SnapshotBundle, nil)
	if err != nil {
		return err
	}
	if len(canonicalBundle) != len(relation.SnapshotBundle) {
		return fmt.Errorf("%w: V2 snapshot bundle contains duplicate namespaces", ErrInvalid)
	}
	for index := range canonicalBundle {
		if canonicalBundle[index] != relation.SnapshotBundle[index] || invalidToken(canonicalBundle[index].SourceNamespace) || invalidToken(canonicalBundle[index].Snapshot) {
			return fmt.Errorf("%w: V2 snapshot bundle is not canonical", ErrInvalid)
		}
	}
	types := make(map[string]string, len(relation.Fields))
	for _, field := range relation.Fields {
		canonicalType, typeErr := CanonicalSQLTypeV2(field.SQLType)
		if invalidToken(field.ID) || invalidToken(field.Expression) || typeErr != nil || canonicalType != field.SQLType || validateFieldCollationV2(canonicalType, field) != nil {
			return fmt.Errorf("%w: V2 relation field is not canonical", ErrInvalid)
		}
		if _, duplicate := types[field.ID]; duplicate {
			return fmt.Errorf("%w: duplicate V2 relation field %q", ErrInvalid, field.ID)
		}
		types[field.ID] = canonicalType
	}
	rows := make(map[string]struct{}, len(relation.Rows))
	for _, row := range relation.Rows {
		if invalidToken(row.Key) || len(row.Cells) != len(relation.Fields) {
			return fmt.Errorf("%w: V2 row key or cell shape is invalid", ErrInvalid)
		}
		if _, duplicate := rows[row.Key]; duplicate {
			return fmt.Errorf("%w: duplicate V2 row key", ErrInvalid)
		}
		rows[row.Key] = struct{}{}
		if err := validateSupportWitnessV2(row.RowSupport, row.RowWitness, relation.SnapshotBundle); err != nil {
			return fmt.Errorf("V2 row %q: %w", row.Key, err)
		}
		for _, origin := range row.Origins {
			if invalidToken(origin.StableRole) || invalidToken(origin.SourceNamespace) || invalidToken(origin.EntityKey) {
				return fmt.Errorf("%w: V2 row origin is incomplete", ErrInvalid)
			}
		}
		for _, field := range relation.Fields {
			cell, present := row.Cells[field.ID]
			if !present || cell.SQLType != types[field.ID] || invalidToken(cell.Expression) {
				return fmt.Errorf("%w: V2 cell %q is incomplete or mistyped", ErrInvalid, field.ID)
			}
			canonicalValue, valueErr := CanonicalSQLValue(cell.SQLType, cell.Value)
			if valueErr != nil {
				return valueErr
			}
			if err := validateSupportWitnessV2(cell.Support, cell.Witness, relation.SnapshotBundle); err != nil {
				return fmt.Errorf("V2 cell %q: %w", field.ID, err)
			}
			if cell.ReleaseFact != nil {
				if err := cell.ReleaseFact.Validate(); err != nil || !cell.ReleaseFact.IsV2() {
					return fmt.Errorf("%w: V2 cell has an invalid release fact", ErrInvalid)
				}
				if cell.ReleaseFact.SQLType != cell.SQLType || cell.ReleaseFact.CanonicalValue != canonicalValue {
					return fmt.Errorf("%w: V2 release fact does not identify its cell value", ErrInvalid)
				}
				if !factCoveredByBundleV2(*cell.ReleaseFact, relation.SnapshotBundle) {
					return fmt.Errorf("%w: V2 release fact is outside the relation snapshot bundle", ErrInvalid)
				}
				if err := validateReleaseProvenanceV2(cell); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateReleaseProvenanceV2(cell CellV2) error {
	release := *cell.ReleaseFact
	switch release.Kind {
	case FactBaseCell:
		hash, err := release.HashBytes()
		if err != nil {
			return err
		}
		supported, present := cell.Support[hash]
		if !present || !sameFactV2(supported, release) {
			return fmt.Errorf("%w: V2 base release fact is absent from cell support", ErrInvalid)
		}
	case FactDerived:
		commitment, err := cell.Witness.Commitment()
		if err != nil {
			return err
		}
		if release.NormalizedExpression != cell.Expression || release.WitnessCommitment != commitment {
			return fmt.Errorf("%w: V2 derived release fact disagrees with cell provenance", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: V2 cell release must be a base-cell or derived fact", ErrInvalid)
	}
	return nil
}

func validateSupportWitnessV2(support FactSet, witness WitnessMultiset, bundle []SnapshotBinding) error {
	if support == nil || witness == nil {
		return fmt.Errorf("%w: nil V2 support or witness", ErrInvalid)
	}
	for hash, fact := range support {
		actual, err := fact.HashBytes()
		if err != nil || actual != hash || !isBaseFactV2(fact) || !factCoveredByBundleV2(fact, bundle) {
			return fmt.Errorf("%w: invalid V2 support fact", ErrInvalid)
		}
	}
	if len(support) != len(witness) {
		return fmt.Errorf("%w: witness support differs from set support", ErrInvalid)
	}
	for hash, item := range witness {
		actual, err := item.Fact.Hash()
		if err != nil || actual != hash || item.Multiplicity == 0 || !isBaseFactV2(item.Fact) || !factCoveredByBundleV2(item.Fact, bundle) {
			return fmt.Errorf("%w: invalid V2 witness fact", ErrInvalid)
		}
		supportHash, err := item.Fact.HashBytes()
		if err != nil {
			return err
		}
		if fact, present := support[supportHash]; !present || !sameFactV2(fact, item.Fact) {
			return fmt.Errorf("%w: witness support differs from set support", ErrInvalid)
		}
	}
	return nil
}

func isBaseFactV2(fact FactID) bool {
	return fact.IsV2() && (fact.Kind == FactBaseRow || fact.Kind == FactBaseCell)
}

func factCoveredByBundleV2(fact FactID, bundle []SnapshotBinding) bool {
	bindings := make(map[string]string, len(bundle))
	for _, binding := range bundle {
		bindings[binding.SourceNamespace] = binding.Snapshot
	}
	if fact.Kind == FactBaseRow || fact.Kind == FactBaseCell {
		return bindings[fact.SourceNamespace] == fact.Snapshot
	}
	if fact.Kind == FactDerived {
		for _, binding := range fact.SnapshotBundle {
			if bindings[binding.SourceNamespace] != binding.Snapshot {
				return false
			}
		}
		return true
	}
	return false
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

func fieldDefinitionV2(relation RelationV2, fieldID string) FieldV2 {
	for _, field := range relation.Fields {
		if field.ID == fieldID {
			return field
		}
	}
	return FieldV2{}
}

func isCollatableTypeV2(sqlType string) bool {
	return sqlType == "text" || sqlType == "character" || sqlType == "character varying"
}

func validateFieldCollationV2(sqlType string, field FieldV2) error {
	if isCollatableTypeV2(sqlType) {
		if invalidToken(field.Collation) || invalidToken(field.CollationVersion) || !field.CollationDeterministic {
			return fmt.Errorf("%w: collatable V2 field requires exact deterministic collation metadata", ErrInvalid)
		}
		return nil
	}
	if field.Collation != "" || field.CollationVersion != "" || field.CollationDeterministic {
		return fmt.Errorf("%w: non-collatable V2 field carries collation metadata", ErrInvalid)
	}
	return nil
}

func inputExpressionV2(relation RelationV2, field string) string {
	return fieldDefinitionV2(relation, field).Expression
}

func aggregateExpressionV2(relation RelationV2, spec AggregateSpecV2) string {
	expression := strings.ToLower(strings.TrimSpace(spec.Function)) + "("
	if spec.Field == "*" {
		expression += "*"
	} else {
		expression += inputExpressionV2(relation, spec.Field)
	}
	return expression + ")"
}

func cloneRelationShapeV2(input RelationV2) RelationV2 {
	return RelationV2{Fields: append([]FieldV2(nil), input.Fields...), SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...), CanonicalOrder: input.CanonicalOrder}
}

func cloneRelationV2(input RelationV2) RelationV2 {
	result := cloneRelationShapeV2(input)
	for _, row := range input.Rows {
		result.Rows = append(result.Rows, cloneRowV2(row))
	}
	return result
}

func relationTuplesDistinctV2(input RelationV2) (bool, error) {
	fields := append([]FieldV2(nil), input.Fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].ID < fields[j].ID })
	seen := make(map[string]struct{}, len(input.Rows))
	for _, row := range input.Rows {
		components := []string{normalizedSchemaV2(fields)}
		for _, field := range fields {
			canonical, err := CanonicalSQLValue(field.SQLType, row.Cells[field.ID].Value)
			if err != nil {
				return false, err
			}
			components = append(components, field.SQLType, canonical)
		}
		key, err := ComposeCanonicalKeyV2("union-distinct-row", components...)
		if err != nil {
			return false, err
		}
		if _, duplicate := seen[key]; duplicate {
			return false, nil
		}
		seen[key] = struct{}{}
	}
	return true, nil
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

// unionDistinctRowDependencyV2 constructs one candidate member's same-proof
// dependency annotation. Duplicate elimination compares every schema field,
// so all of those cell annotations belong to the row-level dependency even if
// ObserveV2 later exposes only a projection. Alternative class members are
// combined by the caller with MergeMax to preserve UNION DISTINCT idempotence.
func unionDistinctRowDependencyV2(source AnnotatedRowV2, fields []FieldV2) (FactSet, WitnessMultiset, error) {
	support := source.RowSupport.Clone()
	witness := source.RowWitness.Clone()
	for _, field := range fields {
		cell := source.Cells[field.ID]
		if err := support.MergeChecked(cell.Support); err != nil {
			return nil, nil, err
		}
		if err := witness.Merge(cell.Witness); err != nil {
			return nil, nil, err
		}
	}
	return support, witness, nil
}

func sameSchemaV2(left, right []FieldV2) bool {
	return normalizedSchemaV2(left) == normalizedSchemaV2(right)
}

func normalizedSchemaV2(fields []FieldV2) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		typeName, _ := CanonicalSQLTypeV2(field.SQLType)
		parts = append(parts, field.ID+":"+typeName+":"+field.Collation+":"+field.CollationVersion+":"+fmt.Sprintf("%t", field.CollationDeterministic))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}
