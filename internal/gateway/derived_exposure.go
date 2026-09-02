package gateway

import (
	"fmt"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// P9.D: conversion from the plan's arithmetic trees to the exposure layer's
// evaluation specs. Projection derived columns keep their alias as the field
// ID; a derived SUM argument gets an internal mapped field that never
// appears in the visible field list.

func derivedAggregateFieldID(alias string) string { return "tg_derived_arg_" + alias }

func derivedFieldSpecs(plan queryplan.QueryPlan, namespace string) ([]exposure.DerivedFieldSpecV2, error) {
	specs := make([]exposure.DerivedFieldSpecV2, 0, len(plan.Derived))
	for _, derived := range plan.Derived {
		spec, err := derivedSpec(derived.Expr, derived.Alias, derived.SQLType, namespace)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	for _, aggregate := range plan.Aggregates {
		if aggregate.DerivedArg == nil {
			continue
		}
		spec, err := derivedSpec(aggregate.DerivedArg, derivedAggregateFieldID(aggregate.Alias),
			aggregate.DerivedArg.SQLType, namespace)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func derivedSpec(expr *queryplan.DerivedExpr, outputID, outputType, namespace string) (exposure.DerivedFieldSpecV2, error) {
	expression, err := queryplan.NormalizeArithmeticInNamespace(expr, namespace)
	if err != nil {
		return exposure.DerivedFieldSpecV2{}, err
	}
	tree, err := derivedTree(expr)
	if err != nil {
		return exposure.DerivedFieldSpecV2{}, err
	}
	return exposure.DerivedFieldSpecV2{OutputID: outputID, OutputType: outputType,
		Expression: expression, Tree: tree}, nil
}

func derivedTree(expr *queryplan.DerivedExpr) (*exposure.DerivedNodeV2, error) {
	if expr == nil || len(expr.Operands) != 2 {
		return nil, fmt.Errorf("derived expression is not binary")
	}
	node := &exposure.DerivedNodeV2{Op: expr.Op}
	children := make([]*exposure.DerivedNodeV2, 0, 2)
	for _, operand := range expr.Operands {
		switch {
		case operand.Nested != nil:
			child, err := derivedTree(operand.Nested)
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		case operand.Column != "":
			children = append(children, &exposure.DerivedNodeV2{Field: operand.Column})
		case operand.Literal != "":
			children = append(children, &exposure.DerivedNodeV2{Literal: operand.Literal})
		default:
			return nil, fmt.Errorf("derived operand is empty")
		}
	}
	node.Left, node.Right = children[0], children[1]
	return node, nil
}
