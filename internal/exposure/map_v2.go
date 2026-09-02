package exposure

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// P9.D derived-cell accounting (docs/p9d_fragment_extension_design.md).
// MapV2 appends one arithmetic projection field per row: the value is
// deterministically re-computed from the argument cells in the exact domain
// (int64 with overflow fail-closed; numeric via big.Rat, closed under
// +,-,*), the dependency Support/Witness is the union of the argument
// cells', and ReleaseFact stays nil so ObserveV2 mints the derived Release
// identity from Expression + type + value + witness, exactly like aggregate
// output cells.

// DerivedNodeV2 is the typed arithmetic tree in canonical evaluation form.
// Exactly one of Field, Literal, or the operator triple is set per node.
type DerivedNodeV2 struct {
	Op          string // "add", "sub", "mul" for interior nodes
	Left, Right *DerivedNodeV2
	Field       string // argument field ID in the input relation
	Literal     string // canonical integer literal
}

// DerivedFieldSpecV2 is one arithmetic projection to append.
type DerivedFieldSpecV2 struct {
	OutputID   string
	OutputType string // exact: smallint|integer|bigint|numeric
	Expression string // canonical N_arith spelling for identity
	Tree       *DerivedNodeV2
}

// MapV2 evaluates the derived fields over every row of the input relation.
func MapV2(input RelationV2, specs []DerivedFieldSpecV2) (RelationV2, error) {
	if err := ValidateRelationV2(input); err != nil {
		return RelationV2{}, err
	}
	if len(specs) == 0 {
		return input, nil
	}
	result := RelationV2{SnapshotBundle: append([]SnapshotBinding(nil), input.SnapshotBundle...),
		CanonicalOrder: input.CanonicalOrder, Fields: append([]FieldDefinitionV2(nil), input.Fields...)}
	for _, spec := range specs {
		if spec.OutputID == "" || spec.Expression == "" || spec.Tree == nil || !derivedExactTypeV2(spec.OutputType) {
			return RelationV2{}, fmt.Errorf("%w: derived field spec is incomplete", ErrInvalid)
		}
		result.Fields = append(result.Fields, FieldDefinitionV2{ID: spec.OutputID, SQLType: spec.OutputType,
			Expression: spec.Expression})
	}
	for _, source := range input.Rows {
		row := AnnotatedRowV2{Key: source.Key, Cells: make(map[string]CellV2, len(source.Cells)+len(specs)),
			RowSupport: source.RowSupport.Clone(), RowWitness: source.RowWitness.Clone(),
			Origins: append([]RowOriginV2(nil), source.Origins...)}
		for id, cell := range source.Cells {
			row.Cells[id] = cloneCellV2(cell)
		}
		for _, spec := range specs {
			support := newEmptyFactSet()
			witness := make(WitnessMultiset)
			exact, err := evalDerivedNode(spec.Tree, spec.OutputType, source, support, witness)
			if err != nil {
				return RelationV2{}, fmt.Errorf("derived field %q: %w", spec.OutputID, err)
			}
			rendered, err := DerivedRatCanonical(exact, spec.OutputType)
			if err != nil {
				return RelationV2{}, fmt.Errorf("derived field %q: %w", spec.OutputID, err)
			}
			row.Cells[spec.OutputID] = CellV2{Value: rendered, SQLType: spec.OutputType,
				Support: support, Witness: witness, Expression: spec.Expression}
		}
		result.Rows = append(result.Rows, row)
	}
	return result, ValidateRelationV2(result)
}

func derivedExactTypeV2(typeName string) bool {
	switch strings.TrimSpace(typeName) {
	case "smallint", "integer", "bigint", "numeric":
		return true
	}
	return false
}

// evalDerivedNode re-computes the node's exact value while unioning the
// argument cells' dependency annotations into support/witness.
func evalDerivedNode(node *DerivedNodeV2, outputType string, row AnnotatedRowV2,
	support FactSet, witness WitnessMultiset) (*big.Rat, error) {
	if node == nil {
		return nil, fmt.Errorf("%w: empty derived node", ErrInvalid)
	}
	if node.Field != "" {
		cell, present := row.Cells[node.Field]
		if !present {
			return nil, fmt.Errorf("%w: derived argument %q is absent", ErrInvalid, node.Field)
		}
		if err := support.MergeChecked(cell.Support); err != nil {
			return nil, err
		}
		if err := witness.Merge(cell.Witness); err != nil {
			return nil, err
		}
		canonical, err := canonicalSQLValueOfType(cell.SQLType, cell.Value)
		if err != nil {
			return nil, err
		}
		return parseExactRat(canonical)
	}
	if node.Literal != "" {
		return parseExactRat(node.Literal)
	}
	left, err := evalDerivedNode(node.Left, outputType, row, support, witness)
	if err != nil {
		return nil, err
	}
	right, err := evalDerivedNode(node.Right, outputType, row, support, witness)
	if err != nil {
		return nil, err
	}
	out := new(big.Rat)
	switch node.Op {
	case "add":
		out.Add(left, right)
	case "sub":
		out.Sub(left, right)
	case "mul":
		out.Mul(left, right)
	default:
		return nil, fmt.Errorf("%w: derived operator %q is outside the exact fold profile", ErrInvalid, node.Op)
	}
	return out, nil
}

func parseExactRat(canonical string) (*big.Rat, error) {
	if canonical == "null" {
		return nil, fmt.Errorf("%w: derived arithmetic over NULL is undefined and settles nothing", ErrInvalid)
	}
	trimmed := canonical
	switch {
	case strings.HasPrefix(canonical, "i:"), strings.HasPrefix(canonical, "n:"):
		trimmed = canonical[2:]
	}
	value, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return nil, fmt.Errorf("%w: derived operand %q is not an exact number", ErrInvalid, canonical)
	}
	return value, nil
}

// DerivedRatCanonical renders the exact result in the output type's
// canonical spelling: integers must fit their domain (overflow fails
// closed); numeric renders the finite decimal expansion big.Rat guarantees
// under +,-,*.
func DerivedRatCanonical(value *big.Rat, outputType string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("%w: derived value is empty", ErrInvalid)
	}
	switch outputType {
	case "smallint", "integer", "bigint":
		if !value.IsInt() {
			return "", fmt.Errorf("%w: integer derived value is fractional", ErrInvalid)
		}
		integer := value.Num()
		if !integer.IsInt64() {
			return "", fmt.Errorf("%w: derived value overflows the exact integer domain", ErrInvalid)
		}
		concrete := integer.Int64()
		switch outputType {
		case "smallint":
			if concrete < math.MinInt16 || concrete > math.MaxInt16 {
				return "", fmt.Errorf("%w: derived value overflows smallint", ErrInvalid)
			}
		case "integer":
			if concrete < math.MinInt32 || concrete > math.MaxInt32 {
				return "", fmt.Errorf("%w: derived value overflows integer", ErrInvalid)
			}
		}
		return integer.String(), nil
	case "numeric":
		return ratFiniteDecimal(value)
	}
	return "", fmt.Errorf("%w: derived output type %q is outside the exact domain", ErrInvalid, outputType)
}

// ratFiniteDecimal renders a rational whose reduced denominator is of the
// form 2^a * 5^b as its exact decimal string; anything else fails closed
// (unreachable under +,-,* over decimal inputs, kept as a guard).
func ratFiniteDecimal(value *big.Rat) (string, error) {
	denominator := new(big.Int).Set(value.Denom())
	scale := 0
	for _, prime := range []int64{2, 5} {
		p := big.NewInt(prime)
		remainder := new(big.Int)
		for {
			quotient, r := new(big.Int).QuoRem(denominator, p, remainder)
			if r.Sign() != 0 {
				break
			}
			denominator.Set(quotient)
			scale++
		}
	}
	if denominator.CmpAbs(big.NewInt(1)) != 0 {
		return "", fmt.Errorf("%w: derived numeric value has no finite decimal expansion", ErrInvalid)
	}
	text := value.FloatString(scale)
	if scale > 0 {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
		if text == "" || text == "-" {
			text = "0"
		}
	}
	return text, nil
}
