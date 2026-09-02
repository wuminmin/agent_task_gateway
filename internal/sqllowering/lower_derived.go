package sqllowering

import (
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// P9.D derived-cell arithmetic lowering (docs/p9d_fragment_extension_design.md).
// Admits binary {+,-,*,/} over approved columns and integer literals in the
// exact type domain, single-product plans only; everything else keeps the
// original fail-closed rejections.

// Division is deliberately absent: PostgreSQL numeric division picks result
// scales the offline exact re-computation cannot reproduce, and every
// evaluation target uses only +, -, *. It keeps the original rejection.
var derivedOperators = map[string]string{
	"+": queryplan.ArithAdd, "-": queryplan.ArithSub,
	"*": queryplan.ArithMul,
}

func (l *lowerer) lowerDerivedProjection(expr *pg_query.A_Expr, target *pg_query.ResTarget, resultCast string) (queryplan.DerivedColumn, *Error) {

	if resultCast != "" {
		return queryplan.DerivedColumn{}, reject(CodeNotLowerable, "PROJECTION_CAST_UNSUPPORTED",
			"A derived projection may not carry a cast.", "SELECT", target.GetLocation(), "",
			"Remove the cast; the expression keeps its exact Catalog type.")
	}
	alias := target.GetName()
	if alias == "" {
		return queryplan.DerivedColumn{}, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"A derived projection requires an explicit alias.", "SELECT", target.GetLocation(), "",
			"Name the expression with AS, for example (price * qty) AS revenue.")
	}
	derived, err := l.lowerDerivedExpr(expr, target.GetLocation())
	if err != nil {
		return queryplan.DerivedColumn{}, err
	}
	return queryplan.DerivedColumn{Expr: derived, Alias: alias, SQLType: derived.SQLType}, nil
}

// lowerDerivedExpr builds the typed arithmetic tree and unifies its exact
// type: every column operand must carry one common exact Catalog type, and
// integer literals adopt it.
func (l *lowerer) lowerDerivedExpr(expr *pg_query.A_Expr, location int32) (*queryplan.DerivedExpr, *Error) {
	if l.multi {
		return nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"Derived arithmetic is admitted over one approved product only.", "SELECT", location, "",
			"Query one approved product, or project columns directly across the join.")
	}
	tree, columnTypes, err := l.buildDerivedNode(&pg_query.Node{Node: &pg_query.Node_AExpr{AExpr: expr}}, location, 0)
	if err != nil {
		return nil, err
	}
	// Exact-domain type promotion: any numeric operand promotes the whole
	// expression to numeric; an all-integer expression computes in bigint
	// (the offline re-computation still fails closed on int64 overflow).
	unified := ""
	for _, sqlType := range columnTypes {
		switch sqlType {
		case "numeric":
			unified = "numeric"
		case "smallint", "integer", "bigint":
			if unified == "" {
				unified = "bigint"
			}
		}
	}
	if unified == "" {
		return nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"Derived arithmetic requires at least one approved column operand.", "SELECT", location, "",
			"Reference at least one approved column in the expression.")
	}
	applyDerivedType(tree, unified)
	if shapeErr := queryplan.ValidateArithShape(tree); shapeErr != nil {
		return nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"The arithmetic expression is outside the derived-cell rule.", "SELECT", location, "",
			"Use binary +, -, * over approved exact-typed columns and integer literals.")
	}
	return tree, nil
}

func (l *lowerer) buildDerivedNode(node *pg_query.Node, location int32, depth int) (*queryplan.DerivedExpr, []string, *Error) {
	if depth >= 4 {
		return nil, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"The arithmetic expression nests too deeply.", "SELECT", location, "",
			"Flatten the expression to at most four levels.")
	}
	aExpr := node.GetAExpr()
	if aExpr == nil || aExpr.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP {
		return nil, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"Only binary arithmetic operators are admitted in a derived projection.", "SELECT", location, "",
			"Use binary +, -, * over approved columns and integer literals.")
	}
	operator, known := derivedOperators[operatorName(aExpr.GetName())]
	if !known || aExpr.GetLexpr() == nil || aExpr.GetRexpr() == nil {
		return nil, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"Only binary +, -, and * are admitted in a derived projection.", "SELECT", location, "",
			"Use binary +, -, * over approved columns and integer literals.")
	}
	out := &queryplan.DerivedExpr{Op: operator}
	var columnTypes []string
	for _, child := range []*pg_query.Node{aExpr.GetLexpr(), aExpr.GetRexpr()} {
		operand, childTypes, err := l.lowerDerivedOperand(child, location, depth)
		if err != nil {
			return nil, nil, err
		}
		out.Operands = append(out.Operands, operand)
		columnTypes = append(columnTypes, childTypes...)
	}
	return out, columnTypes, nil
}

func (l *lowerer) lowerDerivedOperand(node *pg_query.Node, location int32, depth int) (queryplan.ArithOperand, []string, *Error) {
	if columnRef := node.GetColumnRef(); columnRef != nil {
		column, resolveErr := l.resolveColumn(columnRef, "SELECT")
		if resolveErr != nil {
			return queryplan.ArithOperand{}, nil, resolveErr
		}
		sqlType, typeErr := exposure.CanonicalSQLTypeV2(l.sources[column.Source].Product.ColumnTypes[column.Column])
		if typeErr != nil || !derivedExactType(sqlType) {
			return queryplan.ArithOperand{}, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
				"Derived arithmetic admits exact numeric column types only.", "SELECT", location,
				l.sources[column.Source].Product.Name,
				"Use smallint, integer, bigint, or numeric columns in arithmetic.")
		}
		return queryplan.ArithOperand{Column: l.planColumn(column), SQLType: sqlType}, []string{sqlType}, nil
	}
	if constant := node.GetAConst(); constant != nil {
		if integer := constant.GetIval(); integer != nil {
			return queryplan.ArithOperand{Literal: strconv.FormatInt(int64(integer.GetIval()), 10)}, nil, nil
		}
		return queryplan.ArithOperand{}, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
			"Derived arithmetic admits integer literals only.", "SELECT", location, "",
			"Use integer literals; floating literals are outside the exact fold profile.")
	}
	if node.GetAExpr() != nil {
		tree, childTypes, err := l.buildDerivedNode(node, location, depth+1)
		if err != nil {
			return queryplan.ArithOperand{}, nil, err
		}
		return queryplan.ArithOperand{Nested: tree}, childTypes, nil
	}
	return queryplan.ArithOperand{}, nil, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED",
		"A derived operand must be an approved column, an integer literal, or nested arithmetic.", "SELECT", location, "",
		"Use approved columns, integer literals, and binary +, -, *.")
}

func derivedExactType(sqlType string) bool {
	switch strings.TrimSpace(sqlType) {
	case "smallint", "integer", "bigint", "numeric":
		return true
	}
	return false
}

// applyDerivedType stamps the unified exact type onto every node and typed
// literal of the tree.
func applyDerivedType(expr *queryplan.DerivedExpr, sqlType string) {
	if expr == nil {
		return
	}
	expr.SQLType = sqlType
	for index := range expr.Operands {
		if expr.Operands[index].Literal != "" {
			expr.Operands[index].SQLType = sqlType
		}
		if expr.Operands[index].Nested != nil {
			applyDerivedType(expr.Operands[index].Nested, sqlType)
		}
	}
}

