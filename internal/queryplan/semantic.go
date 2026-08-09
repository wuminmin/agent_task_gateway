package queryplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CompileSemantic is the single production path from a relational QueryPlan
// to executable SQL and its typed algebra identity. View expansion, online
// exposure accounting, and replay must share this function rather than invent
// a second plan digest.
func CompileSemantic(plan QueryPlan, products map[string]Product) (RelationalCompilation, AlgebraNormalFormV2, error) {
	compilation, err := CompileRelational(plan, products)
	if err != nil {
		return RelationalCompilation{}, AlgebraNormalFormV2{}, err
	}
	normal, err := SemanticNormalForm(plan, compilation, products)
	if err != nil {
		return RelationalCompilation{}, AlgebraNormalFormV2{}, err
	}
	return compilation, normal, nil
}

// SemanticNormalForm derives the typed algebra identity from an already
// compiled relational plan. The supplied compilation must have come from the
// same plan and product snapshot.
func SemanticNormalForm(plan QueryPlan, compilation RelationalCompilation, products map[string]Product) (AlgebraNormalFormV2, error) {
	algebra, err := semanticAlgebraPlan(plan, compilation, products)
	if err != nil {
		return AlgebraNormalFormV2{}, err
	}
	return NormalizeAlgebraV2(algebra)
}

// SemanticNormalFormV4 is the V5 typed-literal variant. The supported
// relational algebra is unchanged; only its versioned literal identity and
// hash domain differ.
func SemanticNormalFormV4(plan QueryPlan, compilation RelationalCompilation, products map[string]Product) (AlgebraNormalFormV2, error) {
	algebra, err := semanticAlgebraPlan(plan, compilation, products)
	if err != nil {
		return AlgebraNormalFormV2{}, err
	}
	return NormalizeAlgebraV4(algebra)
}

func semanticAlgebraPlan(plan QueryPlan, compilation RelationalCompilation, products map[string]Product) (AlgebraPlanV2, error) {
	makeScan := func(source RelationalSource, semanticRole string) (AlgebraPlanV2, error) {
		product, present := products[source.Product]
		if !present {
			return AlgebraPlanV2{}, fmt.Errorf("semantic source product %q is absent", source.Product)
		}
		schema := make([]AlgebraFieldV2, 0, len(source.EvidenceFields))
		for _, field := range source.EvidenceFields {
			typeName, present := product.ColumnTypes[field]
			if !present {
				return AlgebraPlanV2{}, fmt.Errorf("semantic evidence field %q is absent", field)
			}
			schema = append(schema, AlgebraFieldV2{ID: semanticRole + "." + field, SQLType: typeName,
				Collation: product.ColumnCollations[field], CollationVersion: product.CollationVersions[field],
				CollationDeterministic: product.ColumnCollations[field] != ""})
		}
		node := AlgebraPlanV2{Op: "scan", SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot,
			LineageDigest: product.LineageDigest, StableRole: product.StableRole, Schema: schema}
		if len(source.Filters) != 0 {
			predicates, err := semanticSourceFilters(source.Filters, semanticRole, product)
			if err != nil {
				return AlgebraPlanV2{}, err
			}
			input := node
			node = AlgebraPlanV2{Op: "select", Input: &input, Predicates: predicates}
		}
		return node, nil
	}

	var node AlgebraPlanV2
	switch compilation.Kind {
	case "scan":
		if len(compilation.Sources) != 1 {
			return node, errors.New("scan compilation requires one source")
		}
		var err error
		node, err = makeScan(compilation.Sources[0], compilation.Sources[0].Role)
		if err != nil {
			return node, err
		}
	case "join":
		if len(compilation.Sources) < 2 {
			return node, errors.New("join compilation requires at least two sources")
		}
		left, err := makeScan(compilation.Sources[0], compilation.Sources[0].Role)
		if err != nil {
			return node, err
		}
		node = left
		joinedRoles := map[string]struct{}{compilation.Sources[0].Role: {}}
		usedPredicates := 0
		for _, source := range compilation.Sources[1:] {
			right, scanErr := makeScan(source, source.Role)
			if scanErr != nil {
				return node, scanErr
			}
			predicates := make([]AlgebraJoinPredicateV2, 0)
			for _, predicate := range compilation.JoinPredicates {
				leftRole, _, leftOK := splitFieldID(predicate.Left)
				rightRole, _, rightOK := splitFieldID(predicate.Right)
				if !leftOK || !rightOK {
					return node, errors.New("join predicate contains an invalid field")
				}
				switch {
				case rightRole == source.Role:
					if _, present := joinedRoles[leftRole]; present {
						predicates = append(predicates, AlgebraJoinPredicateV2{LeftField: predicate.Left, RightField: predicate.Right})
						usedPredicates++
					}
				case leftRole == source.Role:
					if _, present := joinedRoles[rightRole]; present {
						predicates = append(predicates, AlgebraJoinPredicateV2{LeftField: predicate.Right, RightField: predicate.Left})
						usedPredicates++
					}
				}
			}
			if len(predicates) == 0 {
				return node, errors.New("join source is disconnected from the compiled component")
			}
			input := node
			node = AlgebraPlanV2{Op: "join", Left: &input, Right: &right, JoinPredicates: predicates}
			joinedRoles[source.Role] = struct{}{}
		}
		if usedPredicates != len(compilation.JoinPredicates) {
			return node, errors.New("join graph contains an unbound equality")
		}
	case "union_distinct":
		if plan.From == nil || plan.From.UnionDistinct == nil || len(compilation.Sources) != 2 {
			return node, errors.New("union compilation is incomplete")
		}
		role := plan.From.UnionDistinct.Role
		left, err := makeScan(compilation.Sources[0], role)
		if err != nil {
			return node, err
		}
		right, err := makeScan(compilation.Sources[1], role)
		if err != nil {
			return node, err
		}
		fields := make([]string, 0, len(compilation.UnionColumns))
		for _, field := range compilation.UnionColumns {
			fields = append(fields, role+"."+field)
		}
		leftInput, rightInput := left, right
		left = AlgebraPlanV2{Op: "project", Input: &leftInput, Fields: fields}
		right = AlgebraPlanV2{Op: "project", Input: &rightInput, Fields: fields}
		node = AlgebraPlanV2{Op: "union", Left: &left, Right: &right}
	default:
		return node, errors.New("unknown relational operator")
	}

	if len(plan.Filters) != 0 {
		predicates, err := semanticOuterFilters(plan.Filters, products, compilation)
		if err != nil {
			return node, err
		}
		input := node
		node = AlgebraPlanV2{Op: "select", Input: &input, Predicates: predicates}
	}
	if len(plan.GroupBy) != 0 || len(plan.Aggregates) != 0 {
		aggregates := make([]AlgebraAggregateV2, 0, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			outputType, err := semanticAggregateType(aggregate, products, compilation)
			if err != nil {
				return node, err
			}
			resultEncoding, encodingErr := canonicalAggregateResultEncoding(aggregate.Function, outputType, aggregate.ResultEncoding)
			if encodingErr != nil {
				return node, encodingErr
			}
			aggregates = append(aggregates, AlgebraAggregateV2{Function: strings.ToLower(aggregate.Function),
				Field: aggregate.Column, OutputType: outputType, ResultEncoding: resultEncoding})
		}
		input := node
		node = AlgebraPlanV2{Op: "group", Input: &input, GroupBy: append([]string(nil), plan.GroupBy...), Aggregates: aggregates}
	}
	fields := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		fields = append(fields, strings.ToLower(strings.TrimSpace(aggregate.Function))+"("+aggregate.Column+")")
	}
	if len(fields) != 0 {
		input := node
		node = AlgebraPlanV2{Op: "project", Input: &input, Fields: fields}
	}
	if len(plan.OrderBy) != 0 {
		aliases := make(map[string]string, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			aliases[aggregate.Alias] = strings.ToLower(strings.TrimSpace(aggregate.Function)) + "(" + aggregate.Column + ")"
		}
		orders := make([]NormalizedOrder, 0, len(plan.OrderBy))
		for _, order := range plan.OrderBy {
			expression := order.Column
			if aggregateExpression := aliases[order.Column]; aggregateExpression != "" {
				expression = aggregateExpression
			}
			orders = append(orders, NormalizedOrder{Expression: expression, Direction: order.Direction})
		}
		input := node
		node = AlgebraPlanV2{Op: "page", Input: &input, OrderBy: orders}
	}
	return node, nil
}

func semanticSourceFilters(filters []Filter, role string, product Product) ([]NormalizedFilter, error) {
	result := make([]NormalizedFilter, 0, len(filters))
	for _, filter := range filters {
		typeName, present := product.ColumnTypes[filter.Column]
		if !present {
			return nil, fmt.Errorf("filter field %q is absent", filter.Column)
		}
		value, err := json.Marshal(filter.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, NormalizedFilter{Column: role + "." + filter.Column, SQLType: typeName, Op: filter.Op, Value: value})
	}
	return result, nil
}

func semanticOuterFilters(filters []Filter, products map[string]Product, compilation RelationalCompilation) ([]NormalizedFilter, error) {
	result := make([]NormalizedFilter, 0, len(filters))
	for _, filter := range filters {
		role, column, ok := splitFieldID(filter.Column)
		if !ok {
			return nil, fmt.Errorf("invalid field %q", filter.Column)
		}
		typeName := ""
		for _, source := range compilation.Sources {
			semanticRole := source.Role
			if compilation.Kind == "union_distinct" {
				semanticRole = role
			}
			if semanticRole == role {
				typeName = products[source.Product].ColumnTypes[column]
				if typeName != "" {
					break
				}
			}
		}
		if typeName == "" {
			return nil, fmt.Errorf("filter field %q is absent", filter.Column)
		}
		value, err := json.Marshal(filter.Value)
		if err != nil {
			return nil, err
		}
		result = append(result, NormalizedFilter{Column: filter.Column, SQLType: typeName, Op: filter.Op, Value: value})
	}
	return result, nil
}

func semanticAggregateType(aggregate Aggregate, products map[string]Product, compilation RelationalCompilation) (string, error) {
	function := strings.ToLower(strings.TrimSpace(aggregate.Function))
	if aggregate.Column == "*" {
		if function == "count" {
			return "bigint", nil
		}
		return "", errors.New("only count accepts star")
	}
	role, column, ok := splitFieldID(aggregate.Column)
	if !ok {
		return "", errors.New("aggregate field is invalid")
	}
	for _, source := range compilation.Sources {
		semanticRole := source.Role
		if compilation.Kind == "union_distinct" {
			semanticRole = role
		}
		if semanticRole == role {
			if typeName := products[source.Product].ColumnTypes[column]; typeName != "" {
				output := AggregateOutputType(function, typeName)
				if output == "" {
					return "", errors.New("aggregate input type is unsupported")
				}
				return output, nil
			}
		}
	}
	return "", errors.New("aggregate field is absent")
}
