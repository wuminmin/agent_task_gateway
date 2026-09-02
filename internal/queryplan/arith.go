package queryplan

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// P9.D derived-cell arithmetic (docs/p9d_fragment_extension_design.md).
// This file defines only the canonical arithmetic normal form N_arith and its
// admission rules; nothing is wired into lowering, normal forms, or effect
// extraction yet, so every existing plan hash and replay identity is
// untouched. The producer is deliberately a string grammar, matching the
// existing flat expression normal forms (sum(ns.field), group(ns.field)).

// Arithmetic operators admitted by the derived-cell rule. Everything else
// fails closed.
const (
	ArithAdd = "add"
	ArithSub = "sub"
	ArithMul = "mul"
	ArithDiv = "div"
)

// arithCommutative reports whether operand order is erased from identity.
func arithCommutative(op string) bool { return op == ArithAdd || op == ArithMul }

// ArithOperand is one leaf of a derived expression: exactly one of Column
// (a catalog-resolved namespace.field) or Literal (a typed canonical value)
// or Nested (a sub-expression).
type ArithOperand struct {
	Column  string
	SQLType string
	Literal string
	Nested  *DerivedExpr
}

// DerivedExpr is a typed arithmetic expression over admitted operands. The
// type domain is exact only: bigint and numeric. Floating domains are
// rejected because the closed algebra fixes no fold order for them.
type DerivedExpr struct {
	Op       string
	SQLType  string
	Operands []ArithOperand
}

const maxArithDepth = 4

// NormalFormVersionV5 is the arithmetic-extended normal-form identity. It is
// used only when a plan carries a derived expression; plans without
// arithmetic keep their V3/V4 bytes and hashes untouched.
const NormalFormVersionV5 = "taskgate-query-normal-form-v5"

var arithExactTypes = map[string]bool{"bigint": true, "numeric": true, "integer": true, "smallint": true}

// NormalizeArithmetic renders the canonical N_arith spelling: operator-named
// prefix form with commutative operands sorted bytewise, non-commutative
// operands kept in order, columns as namespace.field, and literals as
// lit(type):value. It is injective over the admitted domain given canonical
// operand spellings, and deterministic by construction.
func NormalizeArithmetic(expr *DerivedExpr) (string, error) {
	return normalizeArithmetic(expr, 0)
}

func normalizeArithmetic(expr *DerivedExpr, depth int) (string, error) {
	if expr == nil {
		return "", errors.New("derived expression is empty")
	}
	if depth >= maxArithDepth {
		return "", fmt.Errorf("derived expression exceeds depth %d", maxArithDepth)
	}
	switch expr.Op {
	case ArithAdd, ArithSub, ArithMul, ArithDiv:
	default:
		return "", fmt.Errorf("arithmetic operator %q is outside the derived-cell rule", expr.Op)
	}
	if !arithExactTypes[expr.SQLType] {
		return "", fmt.Errorf("derived expression type %q is outside the exact domain", expr.SQLType)
	}
	if len(expr.Operands) != 2 {
		return "", errors.New("derived expressions are binary")
	}
	rendered := make([]string, 0, len(expr.Operands))
	for _, operand := range expr.Operands {
		value, err := normalizeOperand(operand, depth+1)
		if err != nil {
			return "", err
		}
		rendered = append(rendered, value)
	}
	if arithCommutative(expr.Op) {
		sort.Strings(rendered)
	}
	return expr.Op + "(" + strings.Join(rendered, ",") + ")", nil
}

func normalizeOperand(operand ArithOperand, depth int) (string, error) {
	set := 0
	if operand.Column != "" {
		set++
	}
	if operand.Literal != "" {
		set++
	}
	if operand.Nested != nil {
		set++
	}
	if set != 1 {
		return "", errors.New("derived operand must be exactly one of column, literal, or nested")
	}
	switch {
	case operand.Nested != nil:
		return normalizeArithmetic(operand.Nested, depth)
	case operand.Column != "":
		if !strings.Contains(operand.Column, ".") || strings.ContainsAny(operand.Column, " \t\n(),") {
			return "", fmt.Errorf("derived operand column %q is not a canonical namespace.field", operand.Column)
		}
		return operand.Column, nil
	default:
		if !arithExactTypes[operand.SQLType] {
			return "", fmt.Errorf("derived literal type %q is outside the exact domain", operand.SQLType)
		}
		if strings.ContainsAny(operand.Literal, " \t\n(),") {
			return "", fmt.Errorf("derived literal %q is not canonical", operand.Literal)
		}
		return "lit(" + operand.SQLType + "):" + operand.Literal, nil
	}
}

// NormalizeArithmeticInNamespace renders N_arith with every column operand
// rewritten to its canonical namespace.field spelling.
func NormalizeArithmeticInNamespace(expr *DerivedExpr, namespace string) (string, error) {
	rewritten, err := rewriteArithColumns(expr, namespace, 0)
	if err != nil {
		return "", err
	}
	return NormalizeArithmetic(rewritten)
}

func rewriteArithColumns(expr *DerivedExpr, namespace string, depth int) (*DerivedExpr, error) {
	if expr == nil || depth >= maxArithDepth {
		return nil, errors.New("derived expression is empty or too deep")
	}
	out := &DerivedExpr{Op: expr.Op, SQLType: expr.SQLType}
	for _, operand := range expr.Operands {
		clone := operand
		if operand.Column != "" && !strings.Contains(operand.Column, ".") {
			clone.Column = canonicalField(namespace, operand.Column)
		}
		if operand.Nested != nil {
			nested, err := rewriteArithColumns(operand.Nested, namespace, depth+1)
			if err != nil {
				return nil, err
			}
			clone.Nested = nested
		}
		out.Operands = append(out.Operands, clone)
	}
	return out, nil
}

var arithSQLOperator = map[string]string{ArithAdd: "+", ArithSub: "-", ArithMul: "*", ArithDiv: "/"}

// ValidateArithShape checks operator, exact-type domain, arity, depth, and
// operand exclusivity without requiring canonical namespace spellings, so it
// applies to plan-level expressions whose columns are bare product columns.
func ValidateArithShape(expr *DerivedExpr) error { return validateArithShape(expr, 0) }

func validateArithShape(expr *DerivedExpr, depth int) error {
	if expr == nil {
		return errors.New("derived expression is empty")
	}
	if depth >= maxArithDepth {
		return fmt.Errorf("derived expression exceeds depth %d", maxArithDepth)
	}
	switch expr.Op {
	case ArithAdd, ArithSub, ArithMul, ArithDiv:
	default:
		return fmt.Errorf("arithmetic operator %q is outside the derived-cell rule", expr.Op)
	}
	if !arithExactTypes[expr.SQLType] {
		return fmt.Errorf("derived expression type %q is outside the exact domain", expr.SQLType)
	}
	if len(expr.Operands) != 2 {
		return errors.New("derived expressions are binary")
	}
	for _, operand := range expr.Operands {
		set := 0
		if operand.Column != "" {
			set++
		}
		if operand.Literal != "" {
			set++
		}
		if operand.Nested != nil {
			set++
		}
		if set != 1 {
			return errors.New("derived operand must be exactly one of column, literal, or nested")
		}
		if operand.Nested != nil {
			if err := validateArithShape(operand.Nested, depth+1); err != nil {
				return err
			}
		}
		if operand.Literal != "" {
			if !arithExactTypes[operand.SQLType] {
				return fmt.Errorf("derived literal type %q is outside the exact domain", operand.SQLType)
			}
			if strings.ContainsAny(operand.Literal, " \t\n(),;'\"") {
				return fmt.Errorf("derived literal %q is not canonical", operand.Literal)
			}
		}
	}
	return nil
}

// DerivedSQL renders the PostgreSQL expression for a derived column. quote
// maps a plan column name to its quoted SQL identifier and validates it
// against the approved product surface.
func DerivedSQL(expr *DerivedExpr, quote func(string) (string, error)) (string, error) {
	if err := ValidateArithShape(expr); err != nil {
		return "", err
	}
	return derivedSQL(expr, quote, 0)
}

func derivedSQL(expr *DerivedExpr, quote func(string) (string, error), depth int) (string, error) {
	if expr == nil || depth >= maxArithDepth {
		return "", errors.New("derived expression is empty or too deep")
	}
	rendered := make([]string, 0, len(expr.Operands))
	for _, operand := range expr.Operands {
		switch {
		case operand.Nested != nil:
			value, err := derivedSQL(operand.Nested, quote, depth+1)
			if err != nil {
				return "", err
			}
			rendered = append(rendered, value)
		case operand.Column != "":
			value, err := quote(operand.Column)
			if err != nil {
				return "", err
			}
			rendered = append(rendered, value)
		default:
			rendered = append(rendered, operand.Literal)
		}
	}
	return "(" + rendered[0] + " " + arithSQLOperator[expr.Op] + " " + rendered[1] + ")", nil
}
