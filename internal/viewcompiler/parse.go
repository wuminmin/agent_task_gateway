package viewcompiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

type viewSource struct {
	alias    string
	relation RelationName
	fragment fragment
	outputs  map[string]outputBinding
}

type viewJoinQualification struct {
	node    *pg_query.Node
	visible map[int]struct{}
}

type viewLowerer struct {
	state     *compileState
	view      RelationName
	sources   []viewSource
	byAlias   map[string]int
	joinQuals []viewJoinQualification
}

type resolvedViewColumn struct {
	source  int
	binding outputBinding
}

func (state *compileState) parseDefinition(name RelationName, sql string) (*pg_query.SelectStmt, []RelationName, error) {
	parsed, err := pg_query.Parse(sql)
	if err != nil {
		return nil, nil, reject(CodeDefinitionUnsupported, name, "PostgreSQL could not parse pg_get_viewdef output")
	}
	if len(parsed.GetStmts()) != 1 || parsed.GetStmts()[0].GetStmt().GetSelectStmt() == nil {
		return nil, nil, reject(CodeDefinitionUnsupported, name, "definition must contain exactly one SELECT statement")
	}
	statement := parsed.GetStmts()[0].GetStmt().GetSelectStmt()
	if hasRecursiveSelf(statement, name) {
		return nil, nil, reject(CodeCycle, name, "recursive view self-reference is unsupported")
	}
	if shapeErr := validateViewSelectShape(name, statement); shapeErr != nil {
		return nil, nil, shapeErr
	}
	if len(statement.GetFromClause()) != 1 {
		return nil, nil, reject(CodeDefinitionUnsupported, name, "definition requires one relation or explicit JOIN tree")
	}
	references := make(map[RelationName]struct{})
	if collectErr := state.collectDefinitionReferences(name, statement.GetFromClause()[0], references); collectErr != nil {
		return nil, nil, collectErr
	}
	result := make([]RelationName, 0, len(references))
	for reference := range references {
		result = append(result, reference)
	}
	sortRelationNames(result)
	return statement, result, nil
}

func (state *compileState) collectDefinitionReferences(view RelationName, node *pg_query.Node, result map[RelationName]struct{}) error {
	return state.collectDefinitionReferencesAtDepth(view, node, result, 0)
}

func (state *compileState) collectDefinitionReferencesAtDepth(view RelationName, node *pg_query.Node, result map[RelationName]struct{}, depth int) error {
	if node == nil {
		return reject(CodeDefinitionUnsupported, view, "FROM item is missing")
	}
	if depth > queryplan.MaxJoinSources {
		return reject(CodeSourceLimit, view, "JOIN tree exceeds the source limit")
	}
	if relation := node.GetRangeVar(); relation != nil {
		resolved, err := state.resolveRelation(view, relation)
		if err != nil {
			return err
		}
		result[resolved] = struct{}{}
		return nil
	}
	if join := node.GetJoinExpr(); join != nil {
		if err := state.collectDefinitionReferencesAtDepth(view, join.GetLarg(), result, depth+1); err != nil {
			return err
		}
		return state.collectDefinitionReferencesAtDepth(view, join.GetRarg(), result, depth+1)
	}
	return reject(CodeDefinitionUnsupported, view, "FROM contains a subquery, function, or unsupported relation expression")
}

func (state *compileState) resolveRelation(view RelationName, relation *pg_query.RangeVar) (RelationName, error) {
	if relation.GetCatalogname() != "" {
		return RelationName{}, reject(CodeDefinitionUnsupported, view, "cross-database relation references are forbidden")
	}
	candidate := RelationName{Schema: relation.GetSchemaname(), Name: relation.GetRelname()}
	if candidate.Schema != "" {
		if _, present := state.compiler.snapshot.Relations[candidate]; !present {
			return RelationName{}, reject(CodeRelationNotFound, view, "referenced relation %q is absent", candidate)
		}
		return candidate, nil
	}
	if _, present := state.compiler.snapshot.Relations[candidate]; present {
		return candidate, nil
	}
	match := RelationName{}
	found := false
	for name := range state.compiler.snapshot.Relations {
		if name.Name != candidate.Name {
			continue
		}
		if found {
			return RelationName{}, reject(CodeDependencyMismatch, view, "unqualified relation %q is ambiguous", candidate.Name)
		}
		match, found = name, true
	}
	if !found {
		return RelationName{}, reject(CodeRelationNotFound, view, "referenced relation %q is absent", candidate.Name)
	}
	return match, nil
}

func (state *compileState) compileSelect(name RelationName, statement *pg_query.SelectStmt, relation Relation) (fragment, error) {
	lowerer := &viewLowerer{state: state, view: name, byAlias: make(map[string]int)}
	if err := lowerer.collectFrom(statement.GetFromClause()[0]); err != nil {
		return fragment{}, err
	}
	if len(lowerer.sources) == 0 {
		return fragment{}, reject(CodeDefinitionUnsupported, name, "definition has no resolved input")
	}
	result, mergeErr := lowerer.mergeSources()
	if mergeErr != nil {
		return fragment{}, mergeErr
	}

	groupedChild := false
	for _, source := range lowerer.sources {
		groupedChild = groupedChild || source.fragment.grouped
	}
	if groupedChild {
		if len(lowerer.sources) != 1 || len(lowerer.joinQuals) != 0 || statement.GetWhereClause() != nil ||
			len(statement.GetGroupClause()) != 0 {
			return fragment{}, reject(CodeAggregationBarrier, name, "an aggregate view may only be wrapped by a pure single-input projection")
		}
		outputs, outputErr := lowerer.lowerProjection(statement.GetTargetList(), relation, true, true)
		if outputErr != nil {
			return fragment{}, outputErr
		}
		result.outputs = outputs
		result.grouped = true
		return result, nil
	}

	for _, qualification := range lowerer.joinQuals {
		predicates, err := lowerer.lowerJoinQualification(qualification)
		if err != nil {
			return fragment{}, err
		}
		result.predicates = append(result.predicates, predicates...)
		if len(result.predicates) > MaxPredicates {
			return fragment{}, reject(CodeEdgeLimit, name, "expanded equality predicates exceed %d", MaxPredicates)
		}
	}
	filters, filterErr := lowerer.lowerWhere(statement.GetWhereClause())
	if filterErr != nil {
		return fragment{}, filterErr
	}
	result.filters = append(result.filters, filters...)
	if len(result.filters) > MaxPredicates {
		return fragment{}, reject(CodeEdgeLimit, name, "expanded literal predicates exceed %d", MaxPredicates)
	}

	groupBy := make([]string, 0, len(statement.GetGroupClause()))
	groupSeen := make(map[string]struct{}, len(statement.GetGroupClause()))
	for _, groupNode := range statement.GetGroupClause() {
		columnRef := groupNode.GetColumnRef()
		if columnRef == nil {
			return fragment{}, reject(CodeDefinitionUnsupported, name, "GROUP BY accepts direct input columns only")
		}
		column, err := lowerer.resolveColumn(columnRef)
		if err != nil {
			return fragment{}, err
		}
		if column.binding.Kind != OutputField {
			return fragment{}, reject(CodeAggregationBarrier, name, "GROUP BY cannot consume an aggregate output")
		}
		if _, duplicate := groupSeen[column.binding.FieldID]; duplicate {
			return fragment{}, reject(CodeDefinitionUnsupported, name, "GROUP BY repeats field %q", column.binding.FieldID)
		}
		groupSeen[column.binding.FieldID] = struct{}{}
		groupBy = append(groupBy, column.binding.FieldID)
	}
	sort.Strings(groupBy)

	hasAggregate := targetListHasAggregate(statement.GetTargetList())
	outputs, outputErr := lowerer.lowerProjection(statement.GetTargetList(), relation, hasAggregate || len(groupBy) != 0, false)
	if outputErr != nil {
		return fragment{}, outputErr
	}
	if hasAggregate || len(groupBy) != 0 {
		for _, output := range outputs {
			if output.Kind == OutputField {
				if _, grouped := groupSeen[output.FieldID]; !grouped {
					return fragment{}, reject(CodeDefinitionUnsupported, name, "selected field %q is not a GROUP BY key", output.FieldID)
				}
			}
		}
		result.grouped = true
		result.groupBy = groupBy
	}
	result.outputs = outputs
	return result, nil
}

func (lowerer *viewLowerer) collectFrom(node *pg_query.Node) error {
	_, err := lowerer.collectFromNode(node, 0)
	return err
}

func (lowerer *viewLowerer) collectFromNode(node *pg_query.Node, depth int) (map[int]struct{}, error) {
	if node == nil || depth > queryplan.MaxJoinSources {
		return nil, reject(CodeSourceLimit, lowerer.view, "JOIN tree exceeds the source limit")
	}
	if relation := node.GetRangeVar(); relation != nil {
		if !relation.GetInh() {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "ONLY relation references are unsupported")
		}
		if relation.GetAlias() != nil && len(relation.GetAlias().GetColnames()) != 0 {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "relation column alias lists are unsupported")
		}
		name, err := lowerer.state.resolveRelation(lowerer.view, relation)
		if err != nil {
			return nil, err
		}
		expanded, err := lowerer.state.compileRelation(name)
		if err != nil {
			return nil, err
		}
		alias := relation.GetRelname()
		if relation.GetAlias() != nil {
			alias = relation.GetAlias().GetAliasname()
		}
		if alias == "" {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "relation alias is empty")
		}
		if _, duplicate := lowerer.byAlias[alias]; duplicate {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "relation alias %q is repeated", alias)
		}
		outputs := make(map[string]outputBinding, len(expanded.outputs))
		for _, output := range expanded.outputs {
			outputs[output.Name] = output
		}
		index := len(lowerer.sources)
		lowerer.byAlias[alias] = index
		lowerer.sources = append(lowerer.sources, viewSource{alias: alias, relation: name, fragment: expanded, outputs: outputs})
		return map[int]struct{}{index: {}}, nil
	}
	if join := node.GetJoinExpr(); join != nil {
		if join.GetJointype() != pg_query.JoinType_JOIN_INNER {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "only INNER JOIN is supported")
		}
		if join.GetIsNatural() || len(join.GetUsingClause()) != 0 || join.GetJoinUsingAlias() != nil || join.GetAlias() != nil {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "NATURAL, USING, and whole-JOIN aliases are unsupported")
		}
		left, err := lowerer.collectFromNode(join.GetLarg(), depth+1)
		if err != nil {
			return nil, err
		}
		right, err := lowerer.collectFromNode(join.GetRarg(), depth+1)
		if err != nil {
			return nil, err
		}
		if join.GetQuals() == nil {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "every INNER JOIN requires explicit ON equalities")
		}
		visible := make(map[int]struct{}, len(left)+len(right))
		for index := range left {
			visible[index] = struct{}{}
		}
		for index := range right {
			visible[index] = struct{}{}
		}
		lowerer.joinQuals = append(lowerer.joinQuals, viewJoinQualification{node: join.GetQuals(), visible: visible})
		return visible, nil
	}
	return nil, reject(CodeDefinitionUnsupported, lowerer.view, "subqueries and relation functions are unsupported")
}

func (lowerer *viewLowerer) mergeSources() (fragment, error) {
	result := fragment{}
	products := make(map[string]struct{})
	roles := make(map[string]struct{})
	for _, source := range lowerer.sources {
		for _, scan := range source.fragment.sources {
			if _, duplicate := products[scan.Product]; duplicate {
				return fragment{}, reject(CodeStableRoleCollision, lowerer.view, "dependency occurrences repeat product %q", scan.Product)
			}
			if _, duplicate := roles[scan.Role]; duplicate {
				return fragment{}, reject(CodeStableRoleCollision, lowerer.view, "dependency occurrences repeat stable role %q", scan.Role)
			}
			products[scan.Product] = struct{}{}
			roles[scan.Role] = struct{}{}
			result.sources = append(result.sources, scan)
		}
		result.predicates = append(result.predicates, source.fragment.predicates...)
		result.filters = append(result.filters, source.fragment.filters...)
		if len(result.predicates) > MaxPredicates || len(result.filters) > MaxPredicates {
			return fragment{}, reject(CodeEdgeLimit, lowerer.view, "expanded predicates exceed %d", MaxPredicates)
		}
		if len(source.fragment.groupBy) != 0 {
			result.groupBy = append([]string(nil), source.fragment.groupBy...)
		}
		result.grouped = result.grouped || source.fragment.grouped
		if len(result.sources) > queryplan.MaxJoinSources {
			return fragment{}, reject(CodeSourceLimit, lowerer.view, "expanded source count exceeds %d", queryplan.MaxJoinSources)
		}
	}
	return result, nil
}

func (lowerer *viewLowerer) lowerJoinQualification(qualification viewJoinQualification) ([]queryplan.JoinPredicate, error) {
	terms, err := conjunctionTerms(qualification.node, lowerer.view, "JOIN")
	if err != nil {
		return nil, err
	}
	result := make([]queryplan.JoinPredicate, 0, len(terms))
	for _, term := range terms {
		expression := term.GetAExpr()
		if expression == nil || expression.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP || operatorName(expression.GetName()) != "=" {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN accepts column-to-column equality conjunctions only")
		}
		leftRef, rightRef := expression.GetLexpr().GetColumnRef(), expression.GetRexpr().GetColumnRef()
		if leftRef == nil || rightRef == nil {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN equality must compare two direct columns")
		}
		left, leftErr := lowerer.resolveColumn(leftRef)
		if leftErr != nil {
			return nil, leftErr
		}
		right, rightErr := lowerer.resolveColumn(rightRef)
		if rightErr != nil {
			return nil, rightErr
		}
		if _, visible := qualification.visible[left.source]; !visible {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN references a relation outside its scope")
		}
		if _, visible := qualification.visible[right.source]; !visible {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN references a relation outside its scope")
		}
		if left.binding.Kind != OutputField || right.binding.Kind != OutputField {
			return nil, reject(CodeAggregationBarrier, lowerer.view, "JOIN cannot consume an aggregate output")
		}
		leftRole, _, leftOK := splitFieldID(left.binding.FieldID)
		rightRole, _, rightOK := splitFieldID(right.binding.FieldID)
		if !leftOK || !rightOK || leftRole == rightRole {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN equality must connect two distinct stable roles")
		}
		if left.binding.SQLType != right.binding.SQLType || left.binding.Collation != right.binding.Collation ||
			left.binding.CollationVersion != right.binding.CollationVersion {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "JOIN equality fields have incompatible types or collations")
		}
		leftID, rightID := left.binding.FieldID, right.binding.FieldID
		if rightID < leftID {
			leftID, rightID = rightID, leftID
		}
		result = append(result, queryplan.JoinPredicate{Left: leftID, Right: rightID})
	}
	return result, nil
}

func (lowerer *viewLowerer) lowerProjection(targets []*pg_query.Node, relation Relation, grouped, transparent bool) ([]outputBinding, error) {
	if len(targets) == 0 || len(targets) != len(relation.Columns) {
		return nil, reject(CodeSchemaMismatch, lowerer.view, "SELECT output count %d does not match attested count %d", len(targets), len(relation.Columns))
	}
	result := make([]outputBinding, 0, len(targets))
	expressions := make(map[string]struct{}, len(targets))
	for index, targetNode := range targets {
		target := targetNode.GetResTarget()
		if target == nil || len(target.GetIndirection()) != 0 {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "projection must contain direct columns or approved aggregates")
		}
		expected := relation.Columns[index]
		var binding outputBinding
		inferredName := target.GetName()
		if columnRef := target.GetVal().GetColumnRef(); columnRef != nil {
			column, err := lowerer.resolveColumn(columnRef)
			if err != nil {
				return nil, err
			}
			binding = column.binding
			if inferredName == "" {
				fields := columnRef.GetFields()
				name, _ := stringNode(fields[len(fields)-1])
				inferredName = name
			}
		} else if function := target.GetVal().GetFuncCall(); function != nil {
			if transparent {
				return nil, reject(CodeAggregationBarrier, lowerer.view, "an aggregate wrapper may not introduce another aggregate")
			}
			var err error
			binding, err = lowerer.lowerAggregate(function)
			if err != nil {
				return nil, err
			}
			if inferredName == "" {
				inferredName = binding.Function
			}
		} else {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "projection expressions, casts, and scalar functions are unsupported")
		}
		if inferredName != expected.Name {
			return nil, reject(CodeSchemaMismatch, lowerer.view, "SELECT output %d name %q does not match attested name %q", index, inferredName, expected.Name)
		}
		if err := sameColumnType(expected, binding.SQLType, binding.Collation, binding.CollationVersion); err != nil {
			return nil, reject(CodeSchemaMismatch, lowerer.view, "SELECT output %q: %v", expected.Name, err)
		}
		if binding.Kind == OutputAggregate && !grouped {
			return nil, reject(CodePlanInvalid, lowerer.view, "aggregate output was not recognized as grouped")
		}
		key := "field\x00" + binding.FieldID
		if binding.Kind == OutputAggregate && binding.aggregate != nil {
			key = "aggregate\x00" + binding.aggregate.Function + "\x00" + binding.aggregate.Argument
		}
		if _, duplicate := expressions[key]; duplicate {
			return nil, reject(CodeDefinitionUnsupported, lowerer.view, "projection repeats one semantic expression")
		}
		expressions[key] = struct{}{}
		binding.Name = expected.Name
		binding.SQLType, _ = exposure.CanonicalSQLTypeV2(expected.SQLType)
		binding.Collation = expected.Collation
		binding.CollationVersion = expected.CollationVersion
		result = append(result, binding)
	}
	return result, nil
}

func (lowerer *viewLowerer) lowerAggregate(function *pg_query.FuncCall) (outputBinding, error) {
	name := strings.ToLower(operatorName(function.GetFuncname()))
	if name != "count" && name != "sum" && name != "min" && name != "max" {
		return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "aggregate %q is unsupported", name)
	}
	if function.GetAggDistinct() || function.GetAggFilter() != nil || len(function.GetAggOrder()) != 0 ||
		function.GetOver() != nil || function.GetAggWithinGroup() || function.GetFuncVariadic() {
		return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "aggregate modifiers are unsupported")
	}
	if function.GetAggStar() {
		if name != "count" || len(function.GetArgs()) != 0 {
			return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "only COUNT(*) accepts a star argument")
		}
		for _, source := range lowerer.sources {
			for _, scan := range source.fragment.sources {
				if _, approved := lowerer.state.compiler.products[scan.Product].AllowedAggregates[name]; !approved {
					return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "COUNT(*) is not approved by product %q", scan.Product)
				}
			}
		}
		return outputBinding{Output: Output{Kind: OutputAggregate, Function: name, Argument: "*", SQLType: "bigint"},
			aggregate: &aggregateBinding{Function: name, Argument: "*"}}, nil
	}
	if len(function.GetArgs()) != 1 || function.GetArgs()[0].GetColumnRef() == nil {
		return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "aggregate requires one direct column argument")
	}
	column, err := lowerer.resolveColumn(function.GetArgs()[0].GetColumnRef())
	if err != nil {
		return outputBinding{}, err
	}
	if column.binding.Kind != OutputField {
		return outputBinding{}, reject(CodeAggregationBarrier, lowerer.view, "aggregate cannot consume an aggregate output")
	}
	role, _, _ := splitFieldID(column.binding.FieldID)
	product, present := lowerer.productForRole(role)
	if !present {
		return outputBinding{}, reject(CodePlanInvalid, lowerer.view, "aggregate input role %q is absent", role)
	}
	if _, approved := product.AllowedAggregates[name]; !approved {
		return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "aggregate %q is not approved by product %q", name, product.Name)
	}
	outputType := aggregateOutputType(name, column.binding.SQLType)
	if outputType == "" {
		return outputBinding{}, reject(CodeDefinitionUnsupported, lowerer.view, "aggregate %q over %q is outside exact accounting", name, column.binding.SQLType)
	}
	result := outputBinding{Output: Output{Kind: OutputAggregate, Function: name, Argument: column.binding.FieldID, SQLType: outputType},
		aggregate: &aggregateBinding{Function: name, Argument: column.binding.FieldID}}
	if outputType == "text" || outputType == "character" || outputType == "character varying" {
		result.Collation = column.binding.Collation
		result.CollationVersion = column.binding.CollationVersion
	}
	return result, nil
}

func (lowerer *viewLowerer) productForRole(role string) (queryplan.Product, bool) {
	for _, source := range lowerer.sources {
		for _, scan := range source.fragment.sources {
			if scan.Role == role {
				product, present := lowerer.state.compiler.products[scan.Product]
				return product, present
			}
		}
	}
	return queryplan.Product{}, false
}

func (lowerer *viewLowerer) lowerWhere(node *pg_query.Node) ([]queryplan.Filter, error) {
	if node == nil {
		return nil, nil
	}
	terms, err := conjunctionTerms(node, lowerer.view, "WHERE")
	if err != nil {
		return nil, err
	}
	result := make([]queryplan.Filter, 0, len(terms))
	for _, term := range terms {
		filter, filterErr := lowerer.lowerFilter(term)
		if filterErr != nil {
			return nil, filterErr
		}
		result = append(result, filter)
	}
	return result, nil
}

func (lowerer *viewLowerer) lowerFilter(node *pg_query.Node) (queryplan.Filter, error) {
	expression := node.GetAExpr()
	if expression == nil {
		return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "WHERE accepts column-to-literal predicates only")
	}
	op := operatorName(expression.GetName())
	switch expression.GetKind() {
	case pg_query.A_Expr_Kind_AEXPR_OP:
		if op != "=" && op != "<>" && op != "!=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "WHERE operator %q is unsupported", op)
		}
		columnRef, literalNode, reversed := expression.GetLexpr().GetColumnRef(), expression.GetRexpr(), false
		if columnRef == nil {
			columnRef, literalNode, reversed = expression.GetRexpr().GetColumnRef(), expression.GetLexpr(), true
		}
		if columnRef == nil {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "WHERE comparison requires one direct column")
		}
		column, err := lowerer.resolveColumn(columnRef)
		if err != nil {
			return queryplan.Filter{}, err
		}
		if column.binding.Kind != OutputField {
			return queryplan.Filter{}, reject(CodeAggregationBarrier, lowerer.view, "WHERE cannot consume an aggregate output")
		}
		value, castType, literalErr := literalValue(literalNode)
		if literalErr != nil {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "%v", literalErr)
		}
		if reversed {
			op = reverseComparison(op)
		}
		if op == "!=" {
			op = "<>"
		}
		if err := validateFilterLiteral(column.binding.SQLType, value, castType); err != nil {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "WHERE literal: %v", err)
		}
		return queryplan.Filter{Column: column.binding.FieldID, Op: op, Value: value}, nil
	case pg_query.A_Expr_Kind_AEXPR_LIKE:
		if op != "~~" || expression.GetLexpr().GetColumnRef() == nil {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "only positive column LIKE literal is supported")
		}
		column, err := lowerer.resolveColumn(expression.GetLexpr().GetColumnRef())
		if err != nil {
			return queryplan.Filter{}, err
		}
		value, castType, literalErr := literalValue(expression.GetRexpr())
		text, ok := value.(string)
		if literalErr != nil || !ok || (column.binding.SQLType != "text" && column.binding.SQLType != "character" && column.binding.SQLType != "character varying") {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "LIKE requires a string literal and collatable string column")
		}
		if err := validateFilterLiteral(column.binding.SQLType, text, castType); err != nil {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "LIKE literal: %v", err)
		}
		return queryplan.Filter{Column: column.binding.FieldID, Op: "LIKE", Value: text}, nil
	case pg_query.A_Expr_Kind_AEXPR_IN:
		if expression.GetLexpr().GetColumnRef() == nil || (op != "=" && op != "<>") {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "IN requires a direct column and literal list")
		}
		list := expression.GetRexpr().GetList()
		if list == nil || len(list.GetItems()) == 0 || len(list.GetItems()) > 100 {
			return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "IN requires 1..100 literals")
		}
		column, err := lowerer.resolveColumn(expression.GetLexpr().GetColumnRef())
		if err != nil {
			return queryplan.Filter{}, err
		}
		values := make([]any, 0, len(list.GetItems()))
		for _, item := range list.GetItems() {
			value, castType, literalErr := literalValue(item)
			if literalErr != nil {
				return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "%v", literalErr)
			}
			if err := validateFilterLiteral(column.binding.SQLType, value, castType); err != nil {
				return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "IN literal: %v", err)
			}
			values = append(values, value)
		}
		planOp := "IN"
		if op == "<>" {
			planOp = "NOT IN"
		}
		return queryplan.Filter{Column: column.binding.FieldID, Op: planOp, Value: values}, nil
	default:
		return queryplan.Filter{}, reject(CodeDefinitionUnsupported, lowerer.view, "WHERE predicate is outside the restricted fragment")
	}
}

func (lowerer *viewLowerer) resolveColumn(reference *pg_query.ColumnRef) (resolvedViewColumn, error) {
	fields := reference.GetFields()
	if len(fields) == 0 || len(fields) > 2 {
		return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "column reference must be name or alias.name")
	}
	names := make([]string, len(fields))
	for index, field := range fields {
		name, ok := stringNode(field)
		if !ok {
			return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "star projections are unsupported")
		}
		names[index] = name
	}
	if len(names) == 2 {
		index, present := lowerer.byAlias[names[0]]
		if !present {
			return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "relation alias %q is unknown", names[0])
		}
		binding, present := lowerer.sources[index].outputs[names[1]]
		if !present {
			return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "column %q is not exposed by %q", names[1], names[0])
		}
		return resolvedViewColumn{source: index, binding: binding}, nil
	}
	match := -1
	var binding outputBinding
	for index, source := range lowerer.sources {
		candidate, present := source.outputs[names[0]]
		if !present {
			continue
		}
		if match >= 0 {
			return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "unqualified column %q is ambiguous", names[0])
		}
		match, binding = index, candidate
	}
	if match < 0 {
		return resolvedViewColumn{}, reject(CodeDefinitionUnsupported, lowerer.view, "column %q is absent", names[0])
	}
	return resolvedViewColumn{source: match, binding: binding}, nil
}

func validateViewSelectShape(view RelationName, statement *pg_query.SelectStmt) error {
	switch {
	case statement.GetOp() != pg_query.SetOperation_SETOP_NONE || statement.GetLarg() != nil || statement.GetRarg() != nil:
		return reject(CodeDefinitionUnsupported, view, "set operations are unsupported")
	case statement.GetWithClause() != nil:
		return reject(CodeDefinitionUnsupported, view, "CTEs are unsupported")
	case len(statement.GetDistinctClause()) != 0:
		return reject(CodeDefinitionUnsupported, view, "DISTINCT is unsupported")
	case statement.GetIntoClause() != nil:
		return reject(CodeDefinitionUnsupported, view, "SELECT INTO is unsupported")
	case statement.GetHavingClause() != nil:
		return reject(CodeDefinitionUnsupported, view, "HAVING is unsupported")
	case statement.GetGroupDistinct():
		return reject(CodeDefinitionUnsupported, view, "GROUP BY DISTINCT is unsupported")
	case len(statement.GetWindowClause()) != 0:
		return reject(CodeDefinitionUnsupported, view, "window expressions are unsupported")
	case len(statement.GetValuesLists()) != 0:
		return reject(CodeDefinitionUnsupported, view, "VALUES is unsupported")
	case len(statement.GetSortClause()) != 0 || statement.GetLimitCount() != nil || statement.GetLimitOffset() != nil:
		return reject(CodeDefinitionUnsupported, view, "ORDER BY, LIMIT, and OFFSET are unsupported in expandable views")
	case len(statement.GetLockingClause()) != 0:
		return reject(CodeDefinitionUnsupported, view, "row locking is unsupported")
	}
	return nil
}

func targetListHasAggregate(targets []*pg_query.Node) bool {
	for _, node := range targets {
		if target := node.GetResTarget(); target != nil && target.GetVal().GetFuncCall() != nil {
			return true
		}
	}
	return false
}

func conjunctionTerms(node *pg_query.Node, view RelationName, clause string) ([]*pg_query.Node, error) {
	if node == nil {
		return nil, reject(CodeDefinitionUnsupported, view, "%s predicate is missing", clause)
	}
	stack := []*pg_query.Node{node}
	result := make([]*pg_query.Node, 0)
	for len(stack) != 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if expression := current.GetBoolExpr(); expression != nil {
			if expression.GetBoolop() != pg_query.BoolExprType_AND_EXPR {
				return nil, reject(CodeDefinitionUnsupported, view, "%s supports AND conjunctions only", clause)
			}
			arguments := expression.GetArgs()
			for index := len(arguments) - 1; index >= 0; index-- {
				stack = append(stack, arguments[index])
			}
			continue
		}
		result = append(result, current)
		if len(result)+len(stack) > MaxPredicates {
			return nil, reject(CodeEdgeLimit, view, "%s predicates exceed %d", clause, MaxPredicates)
		}
	}
	return result, nil
}

func operatorName(nodes []*pg_query.Node) string {
	if len(nodes) != 1 {
		return ""
	}
	value, _ := stringNode(nodes[0])
	return value
}

func stringNode(node *pg_query.Node) (string, bool) {
	if node == nil || node.GetString_() == nil {
		return "", false
	}
	return node.GetString_().GetSval(), true
}

func literalValue(node *pg_query.Node) (any, string, error) {
	if node == nil {
		return nil, "", errors.New("a scalar literal is required")
	}
	if cast := node.GetTypeCast(); cast != nil {
		castType, err := canonicalCastType(cast.GetTypeName())
		if err != nil {
			return nil, "", err
		}
		value, nestedCast, err := literalValue(cast.GetArg())
		if err != nil {
			return nil, "", err
		}
		if nestedCast != "" {
			return nil, "", errors.New("nested literal casts are unsupported")
		}
		return value, castType, nil
	}
	if constant := node.GetAConst(); constant != nil {
		if constant.GetIsnull() {
			return nil, "", errors.New("NULL literals are unsupported")
		}
		switch {
		case constant.GetSval() != nil:
			return constant.GetSval().GetSval(), "", nil
		case constant.GetBoolval() != nil:
			return constant.GetBoolval().GetBoolval(), "", nil
		case constant.GetIval() != nil:
			return json.Number(strconv.FormatInt(int64(constant.GetIval().GetIval()), 10)), "", nil
		case constant.GetFval() != nil:
			return json.Number(constant.GetFval().GetFval()), "", nil
		}
	}
	if expression := node.GetAExpr(); expression != nil && expression.GetKind() == pg_query.A_Expr_Kind_AEXPR_OP && expression.GetLexpr() == nil {
		op := operatorName(expression.GetName())
		if op == "+" || op == "-" {
			value, castType, err := literalValue(expression.GetRexpr())
			if err != nil {
				return nil, "", err
			}
			number, ok := value.(json.Number)
			if !ok {
				return nil, "", errors.New("unary sign requires a numeric literal")
			}
			text := string(number)
			if op == "-" {
				if strings.HasPrefix(text, "-") {
					text = strings.TrimPrefix(text, "-")
				} else {
					text = "-" + text
				}
			}
			return json.Number(text), castType, nil
		}
	}
	return nil, "", errors.New("literal type is unsupported")
}

func canonicalCastType(name *pg_query.TypeName) (string, error) {
	if name == nil || name.GetSetof() || name.GetPctType() || len(name.GetTypmods()) != 0 || len(name.GetArrayBounds()) != 0 {
		return "", errors.New("literal cast has an unsupported type shape")
	}
	parts := make([]string, 0, len(name.GetNames()))
	for _, node := range name.GetNames() {
		part, ok := stringNode(node)
		if !ok {
			return "", errors.New("literal cast type is invalid")
		}
		parts = append(parts, strings.ToLower(part))
	}
	if len(parts) == 2 && parts[0] == "pg_catalog" {
		parts = parts[1:]
	}
	if len(parts) != 1 {
		return "", errors.New("literal cast must name a pg_catalog scalar type")
	}
	aliases := map[string]string{
		"bool": "boolean", "boolean": "boolean", "int2": "smallint", "smallint": "smallint",
		"int4": "integer", "integer": "integer", "int8": "bigint", "bigint": "bigint",
		"float4": "real", "real": "real", "float8": "double precision", "double precision": "double precision",
		"numeric": "numeric", "text": "text", "varchar": "character varying", "character varying": "character varying",
		"bpchar": "character", "character": "character", "date": "date", "time": "time without time zone",
		"time without time zone": "time without time zone", "timetz": "time with time zone", "time with time zone": "time with time zone",
		"timestamp": "timestamp without time zone", "timestamp without time zone": "timestamp without time zone",
		"timestamptz": "timestamp with time zone", "timestamp with time zone": "timestamp with time zone",
		"uuid": "uuid", "json": "json", "jsonb": "jsonb", "bytea": "bytea",
	}
	typeName, present := aliases[parts[0]]
	if !present {
		return "", fmt.Errorf("literal cast type %q is outside the canonical catalog types", parts[0])
	}
	return typeName, nil
}

func validateFilterLiteral(sqlType string, value any, castType string) error {
	typeName, err := exposure.CanonicalSQLTypeV2(sqlType)
	if err != nil {
		return fmt.Errorf("column SQL type %q is unsupported", sqlType)
	}
	if castType != "" && castType != typeName {
		return fmt.Errorf("literal cast type %q does not equal column type %q", castType, typeName)
	}
	if _, err := exposure.CanonicalSQLValue(sqlType, value); err != nil {
		return fmt.Errorf("literal does not match SQL type %q", sqlType)
	}
	return nil
}

func reverseComparison(operator string) string {
	switch operator {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default:
		return operator
	}
}

func aggregateOutputType(function, input string) string {
	typeName, err := exposure.CanonicalSQLTypeV2(input)
	if err != nil {
		return ""
	}
	switch function {
	case "count":
		return "bigint"
	case "sum":
		switch typeName {
		case "smallint", "integer":
			return "bigint"
		case "bigint", "numeric":
			return "numeric"
		}
	case "min", "max":
		switch typeName {
		case "smallint", "integer", "bigint", "numeric", "real", "double precision", "date", "time without time zone",
			"timestamp with time zone", "timestamp without time zone", "text", "character", "character varying":
			return typeName
		}
	}
	return ""
}
