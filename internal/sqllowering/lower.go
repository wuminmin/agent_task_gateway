// Package sqllowering parses the closed taskgate-reporting-sql-v1 SQL profile
// with PostgreSQL's typed AST and lowers it to the same trusted QueryPlan used
// by structured callers. Raw SQL is never returned for execution.
package sqllowering

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// Result contains only the canonical trusted representation and the profile
// that defined the lowering. The caller may retain a separate raw-SQL digest
// for audit, but it must not use that digest as an exposure identity.
type Result struct {
	Plan           queryplan.QueryPlan `json:"plan"`
	Profile        string              `json:"profile"`
	DisplayColumns []string            `json:"display_columns"`
	ResultOrder    []int               `json:"result_order"`
}

type projectionSlot struct {
	Aggregate bool
	Index     int
}

type source struct {
	SQLAlias string
	Product  queryplan.Product
	Scan     queryplan.Scan
}

type resolvedColumn struct {
	Source int
	Column string
	ID     string
}

type joinQualification struct {
	Node                 *pg_query.Node
	LeftStart, LeftEnd   int
	RightStart, RightEnd int
}

type lowerer struct {
	products        map[string]queryplan.Product
	sources         []source
	sourceByAlias   map[string]int
	joinQuals       []joinQualification
	displayColumns  []string
	projectionOrder []projectionSlot
	multi           bool
}

// Lower parses one SELECT statement and losslessly lowers the closed reporting
// fragment to QueryPlan. Every returned Error is a pre-execution,
// pre-settlement rejection suitable for an Agent rewrite loop.
func Lower(sql string, products map[string]queryplan.Product) (Result, error) {
	parsed, err := pg_query.Parse(sql)
	if err != nil {
		return Result{}, reject(CodeSyntaxError, CodeSyntaxError, "PostgreSQL could not parse the SQL statement.", "SQL", -1, "", "Fix the reported PostgreSQL syntax and retry.")
	}
	if len(parsed.GetStmts()) != 1 {
		return Result{}, reject(CodeNotLowerable, "MULTIPLE_STATEMENTS_UNSUPPORTED", "The reporting SQL profile accepts exactly one statement.", "SQL", -1, "", "Submit one SELECT statement at a time.")
	}
	statement := parsed.GetStmts()[0].GetStmt()
	selectStmt := statement.GetSelectStmt()
	if selectStmt == nil {
		return Result{}, reject(CodeNotLowerable, "STATEMENT_TYPE_UNSUPPORTED", "The reporting SQL profile accepts SELECT statements only.", "SQL", parsed.GetStmts()[0].GetStmtLocation(), "", "Rewrite the request as one SELECT statement.")
	}

	l := &lowerer{products: products, sourceByAlias: make(map[string]int)}
	plan, lowerErr := l.lowerSelect(selectStmt)
	if lowerErr != nil {
		return Result{}, lowerErr
	}
	resultOrder := make([]int, len(l.projectionOrder))
	for index, slot := range l.projectionOrder {
		resultOrder[index] = slot.Index
		if slot.Aggregate {
			resultOrder[index] += len(plan.Columns)
		}
	}
	return Result{Plan: plan, Profile: Profile, DisplayColumns: append([]string(nil), l.displayColumns...), ResultOrder: resultOrder}, nil
}

func (l *lowerer) lowerSelect(statement *pg_query.SelectStmt) (queryplan.QueryPlan, *Error) {
	if err := validateSelectShape(statement); err != nil {
		return queryplan.QueryPlan{}, err
	}
	if len(statement.GetFromClause()) != 1 {
		return queryplan.QueryPlan{}, reject(CodeNotLowerable, "FROM_SHAPE_UNSUPPORTED", "The reporting SQL profile requires exactly one table or connected explicit JOIN tree.", "FROM", -1, "", "Use one approved product or connect approved products with INNER JOIN ... ON equality predicates.")
	}
	if err := l.collectFrom(statement.GetFromClause()[0]); err != nil {
		return queryplan.QueryPlan{}, err
	}
	if len(l.sources) == 0 {
		return queryplan.QueryPlan{}, reject(CodeNotLowerable, "FROM_REQUIRED", "A reporting query requires an approved data product.", "FROM", -1, "", "Add one approved product to FROM.")
	}
	l.multi = len(l.sources) > 1
	if l.multi && (len(statement.GetSortClause()) != 0 || statement.GetLimitCount() != nil || statement.GetLimitOffset() != nil) {
		return queryplan.QueryPlan{}, reject(CodeNotLowerable, "PAGINATION_UNSUPPORTED", "ORDER BY, LIMIT, and OFFSET are outside the multi-product exposure-accounted SQL profile.", "ORDER BY", -1, "", "Remove pagination or query an approved bounded reporting product.")
	}

	var graph JoinGraph
	if l.multi {
		var graphErr *Error
		graph, graphErr = l.buildJoinGraph()
		if graphErr != nil {
			return queryplan.QueryPlan{}, graphErr
		}
	}

	plan := queryplan.QueryPlan{}
	if l.multi {
		join, joinErr := graph.JoinMany(l.products)
		if joinErr != nil {
			return queryplan.QueryPlan{}, l.classifyCompilerError(joinErr, "FROM")
		}
		plan.From = &queryplan.From{JoinMany: &join}
	} else {
		plan.Product = l.sources[0].Product.Name
	}

	for _, targetNode := range statement.GetTargetList() {
		target := targetNode.GetResTarget()
		if target == nil || len(target.GetIndirection()) != 0 {
			return queryplan.QueryPlan{}, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED", "A projection must be an approved column or an approved aggregate.", "SELECT", nodeLocation(targetNode), "", "Project a column directly or use COUNT, SUM, MIN, or MAX.")
		}
		if columnRef := target.GetVal().GetColumnRef(); columnRef != nil {
			column, resolveErr := l.resolveColumn(columnRef, "SELECT")
			if resolveErr != nil {
				return queryplan.QueryPlan{}, resolveErr
			}
			l.projectionOrder = append(l.projectionOrder, projectionSlot{Index: len(plan.Columns)})
			plan.Columns = append(plan.Columns, l.planColumn(column))
			display := column.Column
			if target.GetName() != "" {
				display = target.GetName()
			}
			l.displayColumns = append(l.displayColumns, display)
			continue
		}
		function := target.GetVal().GetFuncCall()
		if function == nil {
			return queryplan.QueryPlan{}, reject(CodeNotLowerable, "PROJECTION_EXPRESSION_UNSUPPORTED", "A projection must be an approved column or an approved aggregate.", "SELECT", target.GetLocation(), "", "Project a column directly or use COUNT, SUM, MIN, or MAX.")
		}
		aggregate, aggregateErr := l.lowerAggregate(function, target)
		if aggregateErr != nil {
			return queryplan.QueryPlan{}, aggregateErr
		}
		l.projectionOrder = append(l.projectionOrder, projectionSlot{Aggregate: true, Index: len(plan.Aggregates)})
		plan.Aggregates = append(plan.Aggregates, aggregate)
		l.displayColumns = append(l.displayColumns, aggregate.Alias)
	}
	if len(plan.Columns)+len(plan.Aggregates) == 0 {
		return queryplan.QueryPlan{}, reject(CodeNotLowerable, "EMPTY_SELECT_LIST", "The query does not project a reporting value.", "SELECT", -1, "", "Select at least one approved column or aggregate.")
	}

	where, whereErr := l.lowerWhere(statement.GetWhereClause())
	if whereErr != nil {
		return queryplan.QueryPlan{}, whereErr
	}
	plan.Filters = where
	for _, groupNode := range statement.GetGroupClause() {
		columnRef := groupNode.GetColumnRef()
		if columnRef == nil {
			return queryplan.QueryPlan{}, reject(CodeNotLowerable, "GROUP_EXPRESSION_UNSUPPORTED", "GROUP BY accepts direct approved columns only.", "GROUP BY", nodeLocation(groupNode), "", "Group by a direct column reference.")
		}
		column, resolveErr := l.resolveColumn(columnRef, "GROUP BY")
		if resolveErr != nil {
			return queryplan.QueryPlan{}, resolveErr
		}
		plan.GroupBy = append(plan.GroupBy, l.planColumn(column))
	}
	if !l.multi {
		if paginationErr := l.lowerSinglePagination(statement, &plan); paginationErr != nil {
			return queryplan.QueryPlan{}, paginationErr
		}
	}

	if l.multi {
		canonical, canonicalErr := queryplan.CanonicalizeRelational(plan, l.products)
		if canonicalErr != nil {
			return queryplan.QueryPlan{}, l.classifyCompilerError(canonicalErr, "FROM")
		}
		plan = canonical
		if _, compileErr := queryplan.CompileRelational(plan, l.products); compileErr != nil {
			return queryplan.QueryPlan{}, l.classifyCompilerError(compileErr, "SQL")
		}
	} else {
		sortFilters(plan.Filters)
		if _, normalizeErr := queryplan.NormalizeV2(plan, l.sources[0].Product); normalizeErr != nil {
			return queryplan.QueryPlan{}, l.classifyCompilerError(normalizeErr, "SQL")
		}
	}
	return plan, nil
}

func validateSelectShape(statement *pg_query.SelectStmt) *Error {
	switch {
	case statement.GetOp() != pg_query.SetOperation_SETOP_NONE || statement.GetLarg() != nil || statement.GetRarg() != nil:
		return reject(CodeNotLowerable, "SET_OPERATION_UNSUPPORTED", "UNION, INTERSECT, and EXCEPT are outside this SQL profile.", "SELECT", -1, "", "Query an approved reporting product directly or submit a canonical plan through the advanced interface.")
	case statement.GetWithClause() != nil:
		return reject(CodeNotLowerable, "CTE_UNSUPPORTED", "WITH queries are outside this SQL profile.", "WITH", -1, "", "Inline a supported table reference and INNER JOIN graph.")
	case len(statement.GetDistinctClause()) != 0:
		return reject(CodeNotLowerable, "DISTINCT_UNSUPPORTED", "SELECT DISTINCT is outside this SQL profile.", "SELECT", -1, "", "Use an approved aggregate or reporting product with the required grain.")
	case statement.GetIntoClause() != nil:
		return reject(CodeNotLowerable, "SELECT_INTO_UNSUPPORTED", "SELECT INTO is not a reporting operation.", "INTO", -1, "", "Remove INTO and request a read-only result.")
	case statement.GetHavingClause() != nil:
		return reject(CodeNotLowerable, "HAVING_UNSUPPORTED", "HAVING predicates are outside this SQL profile.", "HAVING", nodeLocation(statement.GetHavingClause()), "", "Move an eligible base-column predicate to WHERE or query an approved aggregate product.")
	case statement.GetGroupDistinct():
		return reject(CodeNotLowerable, "GROUP_DISTINCT_UNSUPPORTED", "GROUP BY DISTINCT is outside this SQL profile.", "GROUP BY", -1, "", "Use a direct GROUP BY over approved columns.")
	case len(statement.GetWindowClause()) != 0:
		return reject(CodeNotLowerable, "WINDOW_UNSUPPORTED", "Window definitions are outside this SQL profile.", "WINDOW", -1, "", "Use GROUP BY with an approved aggregate.")
	case len(statement.GetValuesLists()) != 0:
		return reject(CodeNotLowerable, "VALUES_UNSUPPORTED", "VALUES is outside this SQL profile.", "FROM", -1, "", "Read from an approved data product.")
	case len(statement.GetLockingClause()) != 0:
		return reject(CodeNotLowerable, "LOCKING_UNSUPPORTED", "Row locking is not a reporting operation.", "SELECT", -1, "", "Remove the locking clause.")
	}
	return nil
}

func (l *lowerer) collectFrom(node *pg_query.Node) *Error {
	return l.collectFromAtDepth(node, 0)
}

func (l *lowerer) collectFromAtDepth(node *pg_query.Node, depth int) *Error {
	if node == nil {
		return reject(CodeNotLowerable, "FROM_SHAPE_UNSUPPORTED", "The FROM item is missing.", "FROM", -1, "", "Use an approved product name.")
	}
	if relation := node.GetRangeVar(); relation != nil {
		if len(l.sources) >= queryplan.MaxJoinSources {
			return reject(CodeNotLowerable, "JOIN_SOURCE_LIMIT_EXCEEDED", fmt.Sprintf("The reporting SQL profile accepts at most %d joined products per request.", queryplan.MaxJoinSources), "FROM", relation.GetLocation(), relation.GetRelname(), "Split the request or use an approved reporting product that prejoins part of the graph.")
		}
		if relation.GetCatalogname() != "" || relation.GetSchemaname() != "" {
			return reject(CodeNotLowerable, "QUALIFIED_PRODUCT_UNSUPPORTED", "Catalog- or schema-qualified product names are outside this profile.", "FROM", relation.GetLocation(), relation.GetRelname(), "Use the approved Catalog product name returned by describe_data_product.")
		}
		if !relation.GetInh() {
			return reject(CodeNotLowerable, "ONLY_UNSUPPORTED", "ONLY changes PostgreSQL inheritance semantics and cannot be represented by QueryPlan.", "FROM", relation.GetLocation(), relation.GetRelname(), "Remove ONLY or use an approved product whose Catalog definition has the required scope.")
		}
		product, present := l.products[relation.GetRelname()]
		if !present || product.Name != relation.GetRelname() {
			return reject(CodeProductNotApproved, CodeProductNotApproved, fmt.Sprintf("Data product %q is not approved for this task.", relation.GetRelname()), "FROM", relation.GetLocation(), relation.GetRelname(), "Call list_data_products and use an approved product name.")
		}
		alias := relation.GetRelname()
		if relation.GetAlias() != nil {
			alias = relation.GetAlias().GetAliasname()
			if len(relation.GetAlias().GetColnames()) != 0 {
				return reject(CodeNotLowerable, "COLUMN_ALIAS_LIST_UNSUPPORTED", "Column alias lists on data products are outside this profile.", "FROM", relation.GetLocation(), relation.GetRelname(), "Use the product's approved Catalog column names.")
			}
		}
		if alias == "" {
			return reject(CodeNotLowerable, "RELATION_ALIAS_INVALID", "The relation alias is empty.", "FROM", relation.GetLocation(), relation.GetRelname(), "Use a PostgreSQL identifier as the relation alias.")
		}
		if _, duplicate := l.sourceByAlias[alias]; duplicate {
			return reject(CodeNotLowerable, "RELATION_ALIAS_AMBIGUOUS", fmt.Sprintf("Relation alias %q is repeated.", alias), "FROM", relation.GetLocation(), relation.GetRelname(), "Use a distinct alias for each product.")
		}
		for _, existing := range l.sources {
			if existing.Product.Name == product.Name {
				return reject(CodeNotLowerable, "SELF_JOIN_UNSUPPORTED", "Repeated products and self-joins do not have a unique canonical source identity in this profile.", "FROM", relation.GetLocation(), relation.GetRelname(), "Use an approved reporting product with distinct stable source roles.")
			}
			if existing.Product.StableRole == product.StableRole {
				return reject(CodeNotLowerable, "STABLE_ROLE_COLLISION", "Two products share the same Catalog stable role.", "FROM", relation.GetLocation(), relation.GetRelname(), "Use products with distinct Catalog stable roles.")
			}
		}
		index := len(l.sources)
		l.sourceByAlias[alias] = index
		l.sources = append(l.sources, source{SQLAlias: alias, Product: product, Scan: queryplan.Scan{Product: product.Name, Role: product.StableRole}})
		return nil
	}
	if join := node.GetJoinExpr(); join != nil {
		if depth >= queryplan.MaxJoinSources {
			return reject(CodeNotLowerable, "JOIN_SOURCE_LIMIT_EXCEEDED", fmt.Sprintf("The reporting SQL profile accepts at most %d joined products per request.", queryplan.MaxJoinSources), "FROM", nodeLocation(join.GetQuals()), "", "Split the request or use an approved reporting product that prejoins part of the graph.")
		}
		if join.GetJointype() != pg_query.JoinType_JOIN_INNER {
			reason := strings.TrimPrefix(join.GetJointype().String(), "JOIN_") + "_JOIN_UNSUPPORTED"
			return reject(CodeJoinTypeUnsupported, reason, "Exposure accounting currently supports connected INNER equijoins only.", "FROM", nodeLocation(join.GetQuals()), "", "Use an INNER JOIN or query an approved prejoined reporting product.")
		}
		if join.GetIsNatural() {
			return reject(CodeJoinTypeUnsupported, "NATURAL_JOIN_UNSUPPORTED", "NATURAL JOIN cannot be lowered to explicit stable Catalog field identities.", "FROM", -1, "", "Use INNER JOIN ... ON with explicit approved columns.")
		}
		if len(join.GetUsingClause()) != 0 || join.GetJoinUsingAlias() != nil {
			return reject(CodeJoinTypeUnsupported, "JOIN_USING_UNSUPPORTED", "JOIN ... USING cannot be lowered without changing output-column semantics.", "FROM", -1, "", "Use INNER JOIN ... ON left.column = right.column.")
		}
		if join.GetAlias() != nil {
			return reject(CodeNotLowerable, "JOIN_ALIAS_UNSUPPORTED", "Aliases on a complete JOIN expression are outside this profile.", "FROM", -1, "", "Alias each approved product separately.")
		}
		leftStart := len(l.sources)
		if err := l.collectFromAtDepth(join.GetLarg(), depth+1); err != nil {
			return err
		}
		leftEnd := len(l.sources)
		rightStart := leftEnd
		if err := l.collectFromAtDepth(join.GetRarg(), depth+1); err != nil {
			return err
		}
		rightEnd := len(l.sources)
		if join.GetQuals() == nil {
			return reject(CodeJoinGraphDisconnected, "JOIN_PREDICATE_REQUIRED", "Every joined source must be connected by explicit equality predicates.", "FROM", -1, "", "Add ON left.approved_key = right.approved_key.")
		}
		l.joinQuals = append(l.joinQuals, joinQualification{
			Node: join.GetQuals(), LeftStart: leftStart, LeftEnd: leftEnd, RightStart: rightStart, RightEnd: rightEnd,
		})
		return nil
	}
	if node.GetRangeSubselect() != nil || node.GetSelectStmt() != nil {
		return reject(CodeSubqueryUnsupported, CodeSubqueryUnsupported, "Subqueries are outside the reporting SQL profile.", "FROM", -1, "", "Query approved products directly or use an approved prejoined reporting product.")
	}
	return reject(CodeNotLowerable, "FROM_ITEM_UNSUPPORTED", "This FROM item cannot be represented by the canonical reporting plan.", "FROM", -1, "", "Use approved products connected with INNER JOIN ... ON equality predicates.")
}

func (l *lowerer) buildJoinGraph() (JoinGraph, *Error) {
	if len(l.sources) < 2 {
		return JoinGraph{}, reject(CodeNotLowerable, "JOIN_SHAPE_INVALID", "A JOIN graph requires multiple approved products.", "FROM", -1, "", "Use a direct approved product scan.")
	}
	graph := JoinGraph{Nodes: make([]RelationNode, 0, len(l.sources))}
	for _, item := range l.sources {
		graph.Nodes = append(graph.Nodes, RelationNode{Relation: item.Scan.Role, Product: item.Scan.Product})
	}
	edges := make(map[string]int)
	for _, qualification := range l.joinQuals {
		terms, err := conjunctionTerms(qualification.Node, "FROM")
		if err != nil {
			return JoinGraph{}, err
		}
		for _, term := range terms {
			expression := term.GetAExpr()
			if expression == nil || expression.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP || operatorName(expression.GetName()) != "=" {
				return JoinGraph{}, reject(CodeNotLowerable, "JOIN_PREDICATE_UNSUPPORTED", "JOIN conditions must be conjunctions of column-to-column equality predicates.", "FROM", nodeLocation(term), "", "Use ON left.approved_key = right.approved_key joined with AND.")
			}
			leftRef, rightRef := expression.GetLexpr().GetColumnRef(), expression.GetRexpr().GetColumnRef()
			if leftRef == nil || rightRef == nil {
				return JoinGraph{}, reject(CodeNotLowerable, "JOIN_PREDICATE_UNSUPPORTED", "JOIN conditions must compare two approved columns.", "FROM", expression.GetLocation(), "", "Use ON left.approved_key = right.approved_key.")
			}
			left, leftErr := l.resolveColumn(leftRef, "FROM")
			if leftErr != nil {
				return JoinGraph{}, leftErr
			}
			right, rightErr := l.resolveColumn(rightRef, "FROM")
			if rightErr != nil {
				return JoinGraph{}, rightErr
			}
			if left.Source == right.Source {
				return JoinGraph{}, reject(CodeNotLowerable, "JOIN_PREDICATE_SCOPE_INVALID", "A JOIN equality must connect two distinct relations.", "FROM", expression.GetLocation(), l.sources[left.Source].Product.Name, "Compare approved keys from two distinct relations in the current JOIN scope.")
			}
			leftVisible := sourceInRange(left.Source, qualification.LeftStart, qualification.RightEnd)
			rightVisible := sourceInRange(right.Source, qualification.LeftStart, qualification.RightEnd)
			if !leftVisible || !rightVisible {
				return JoinGraph{}, reject(CodeNotLowerable, "JOIN_PREDICATE_SCOPE_INVALID", "A JOIN equality references a relation outside the current JOIN scope.", "FROM", expression.GetLocation(), "", "Move the equality to a JOIN where both relations are visible.")
			}
			leftType, leftTypeErr := exposure.CanonicalSQLTypeV2(l.sources[left.Source].Product.ColumnTypes[left.Column])
			rightType, rightTypeErr := exposure.CanonicalSQLTypeV2(l.sources[right.Source].Product.ColumnTypes[right.Column])
			if leftTypeErr != nil || rightTypeErr != nil || leftType != rightType {
				return JoinGraph{}, reject(CodeJoinKeyTypeMismatch, CodeJoinKeyTypeMismatch, fmt.Sprintf("JOIN keys %q and %q do not have the same canonical SQL type.", left.ID, right.ID), "FROM", expression.GetLocation(), "", "Join approved keys with identical Catalog SQL types.")
			}
			leftCollation, rightCollation := l.sources[left.Source].Product.ColumnCollations[left.Column], l.sources[right.Source].Product.ColumnCollations[right.Column]
			leftVersion, rightVersion := l.sources[left.Source].Product.CollationVersions[left.Column], l.sources[right.Source].Product.CollationVersions[right.Column]
			if leftCollation != rightCollation || leftVersion != rightVersion {
				return JoinGraph{}, reject(CodeCollationMismatch, CodeCollationMismatch, fmt.Sprintf("JOIN keys %q and %q do not share an identical deterministic collation profile.", left.ID, right.ID), "FROM", expression.GetLocation(), "", "Join keys with the same Catalog collation name and version.")
			}

			leftRelation, rightRelation := l.sources[left.Source].Scan.Role, l.sources[right.Source].Scan.Role
			leftColumn, rightColumn := left.Column, right.Column
			if rightRelation < leftRelation {
				leftRelation, rightRelation = rightRelation, leftRelation
				leftColumn, rightColumn = rightColumn, leftColumn
			}
			key := leftRelation + "\x00" + rightRelation
			index, present := edges[key]
			if !present {
				index = len(graph.Edges)
				edges[key] = index
				graph.Edges = append(graph.Edges, JoinEdge{LeftRelation: leftRelation, RightRelation: rightRelation})
			}
			graph.Edges[index].Predicates = append(graph.Edges[index].Predicates, EqualityPredicate{LeftColumn: leftColumn, RightColumn: rightColumn})
		}
	}
	canonical, err := graph.Canonical(l.products)
	if err != nil {
		return JoinGraph{}, l.classifyCompilerError(err, "FROM")
	}
	return canonical, nil
}

func sourceInRange(source, start, end int) bool {
	return source >= start && source < end
}

func (l *lowerer) lowerAggregate(function *pg_query.FuncCall, target *pg_query.ResTarget) (queryplan.Aggregate, *Error) {
	name := strings.ToLower(operatorName(function.GetFuncname()))
	if name != "count" && name != "sum" && name != "min" && name != "max" {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_UNSUPPORTED", fmt.Sprintf("Aggregate %q is outside the reporting SQL profile.", name), "SELECT", function.GetLocation(), "", "Use COUNT, SUM, MIN, or MAX when approved by the Catalog.")
	}
	if function.GetAggDistinct() || function.GetAggFilter() != nil || len(function.GetAggOrder()) != 0 || function.GetOver() != nil || function.GetAggWithinGroup() || function.GetFuncVariadic() {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_MODIFIER_UNSUPPORTED", "DISTINCT, FILTER, ORDER BY, window, and variadic aggregate modifiers are outside this profile.", "SELECT", function.GetLocation(), "", "Use a plain approved aggregate over one column.")
	}
	alias := target.GetName()
	if alias == "" {
		alias = name
	}
	if function.GetAggStar() {
		if name != "count" || len(function.GetArgs()) != 0 {
			return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_STAR_UNSUPPORTED", "Only COUNT(*) is supported with a star argument.", "SELECT", function.GetLocation(), "", "Use COUNT(*) or an approved aggregate over one column.")
		}
		for _, item := range l.sources {
			if _, approved := item.Product.AllowedAggregates[name]; !approved {
				return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_NOT_APPROVED", fmt.Sprintf("Aggregate %q is not approved by every input product.", name), "SELECT", function.GetLocation(), item.Product.Name, "Call describe_data_product and use an approved aggregate.")
			}
		}
		return queryplan.Aggregate{Function: name, Column: "*", Alias: alias}, nil
	}
	if len(function.GetArgs()) != 1 {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_ARITY_UNSUPPORTED", "An aggregate must have exactly one direct column argument.", "SELECT", function.GetLocation(), "", "Use an approved aggregate over one column.")
	}
	columnRef := function.GetArgs()[0].GetColumnRef()
	if columnRef == nil {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_EXPRESSION_UNSUPPORTED", "Aggregate expressions must be direct approved columns.", "SELECT", function.GetLocation(), "", "Apply the aggregate directly to an approved column.")
	}
	column, resolveErr := l.resolveColumn(columnRef, "SELECT")
	if resolveErr != nil {
		return queryplan.Aggregate{}, resolveErr
	}
	if _, approved := l.sources[column.Source].Product.AllowedAggregates[name]; !approved {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_NOT_APPROVED", fmt.Sprintf("Aggregate %q is not approved for product %q.", name, l.sources[column.Source].Product.Name), "SELECT", function.GetLocation(), l.sources[column.Source].Product.Name, "Call describe_data_product and use an approved aggregate.")
	}
	inputType, typeErr := exposure.CanonicalSQLTypeV2(l.sources[column.Source].Product.ColumnTypes[column.Column])
	if typeErr != nil || !exactAggregateTypeSupported(name, inputType) {
		return queryplan.Aggregate{}, reject(CodeNotLowerable, "AGGREGATE_TYPE_UNSUPPORTED", fmt.Sprintf("Aggregate %q over Catalog type %q is outside the exact accounting profile.", name, l.sources[column.Source].Product.ColumnTypes[column.Column]), "SELECT", function.GetLocation(), l.sources[column.Source].Product.Name, "Use an aggregate and column type advertised by get_sql_capabilities and describe_data_product.")
	}
	return queryplan.Aggregate{Function: name, Column: l.planColumn(column), Alias: alias}, nil
}

func exactAggregateTypeSupported(function, inputType string) bool {
	switch function {
	case "count":
		return true
	case "sum":
		return inputType == "smallint" || inputType == "integer" || inputType == "bigint" || inputType == "numeric"
	case "min", "max":
		switch inputType {
		case "smallint", "integer", "bigint", "numeric", "real", "double precision", "date", "time without time zone",
			"timestamp with time zone", "timestamp without time zone", "text", "character", "character varying":
			return true
		}
	}
	return false
}

func (l *lowerer) lowerWhere(node *pg_query.Node) ([]queryplan.Filter, *Error) {
	if node == nil {
		return nil, nil
	}
	terms, err := conjunctionTerms(node, "WHERE")
	if err != nil {
		return nil, err
	}
	result := make([]queryplan.Filter, 0, len(terms))
	for _, term := range terms {
		filter, filterErr := l.lowerFilter(term)
		if filterErr != nil {
			return nil, filterErr
		}
		result = append(result, filter)
	}
	sortFilters(result)
	return result, nil
}

func (l *lowerer) lowerFilter(node *pg_query.Node) (queryplan.Filter, *Error) {
	expression := node.GetAExpr()
	if expression == nil {
		return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_PREDICATE_UNSUPPORTED", "WHERE accepts conjunctions of column-to-literal predicates only.", "WHERE", nodeLocation(node), "", "Compare an approved column with a scalar literal or literal IN list.")
	}
	op := operatorName(expression.GetName())
	switch expression.GetKind() {
	case pg_query.A_Expr_Kind_AEXPR_OP:
		if op != "=" && op != "<>" && op != "!=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
			return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_OPERATOR_UNSUPPORTED", fmt.Sprintf("WHERE operator %q is outside this profile.", op), "WHERE", expression.GetLocation(), "", "Use =, <>, <, <=, >, or >= with a literal.")
		}
		columnRef, literalNode, reversed := expression.GetLexpr().GetColumnRef(), expression.GetRexpr(), false
		if columnRef == nil {
			columnRef, literalNode, reversed = expression.GetRexpr().GetColumnRef(), expression.GetLexpr(), true
		}
		if columnRef == nil {
			return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_PREDICATE_UNSUPPORTED", "WHERE comparisons require one approved column and one scalar literal.", "WHERE", expression.GetLocation(), "", "Compare an approved column with a scalar literal.")
		}
		value, literalErr := literalValue(literalNode)
		if literalErr != nil {
			return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_LITERAL_UNSUPPORTED", literalErr.Error(), "WHERE", expression.GetLocation(), "", "Use a string, boolean, or numeric literal.")
		}
		column, resolveErr := l.resolveColumn(columnRef, "WHERE")
		if resolveErr != nil {
			return queryplan.Filter{}, resolveErr
		}
		if reversed {
			op = reverseComparison(op)
		}
		if op == "!=" {
			op = "<>"
		}
		if valueErr := l.validateFilterValue(column, op, value, expression.GetLocation()); valueErr != nil {
			return queryplan.Filter{}, valueErr
		}
		return queryplan.Filter{Column: l.planColumn(column), Op: op, Value: value}, nil
	case pg_query.A_Expr_Kind_AEXPR_LIKE:
		if op != "~~" {
			return queryplan.Filter{}, reject(CodeNotLowerable, "LIKE_VARIANT_UNSUPPORTED", "Only positive case-sensitive LIKE is supported.", "WHERE", expression.GetLocation(), "", "Use column LIKE 'literal pattern'.")
		}
		columnRef := expression.GetLexpr().GetColumnRef()
		if columnRef == nil {
			return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_PREDICATE_UNSUPPORTED", "LIKE requires an approved column on the left.", "WHERE", expression.GetLocation(), "", "Use column LIKE 'literal pattern'.")
		}
		value, literalErr := literalValue(expression.GetRexpr())
		text, isString := value.(string)
		if literalErr != nil || !isString {
			return queryplan.Filter{}, reject(CodeNotLowerable, "LIKE_LITERAL_REQUIRED", "LIKE requires a string literal pattern.", "WHERE", expression.GetLocation(), "", "Use column LIKE 'literal pattern'.")
		}
		column, resolveErr := l.resolveColumn(columnRef, "WHERE")
		if resolveErr != nil {
			return queryplan.Filter{}, resolveErr
		}
		if valueErr := l.validateFilterValue(column, "LIKE", text, expression.GetLocation()); valueErr != nil {
			return queryplan.Filter{}, valueErr
		}
		return queryplan.Filter{Column: l.planColumn(column), Op: "LIKE", Value: text}, nil
	case pg_query.A_Expr_Kind_AEXPR_IN:
		columnRef := expression.GetLexpr().GetColumnRef()
		if columnRef == nil || (op != "=" && op != "<>") {
			return queryplan.Filter{}, reject(CodeNotLowerable, "IN_PREDICATE_UNSUPPORTED", "IN requires an approved column and a literal list.", "WHERE", expression.GetLocation(), "", "Use column IN (literal, ...), or column NOT IN (literal, ...).")
		}
		list := expression.GetRexpr().GetList()
		if list == nil || len(list.GetItems()) == 0 || len(list.GetItems()) > 100 {
			return queryplan.Filter{}, reject(CodeNotLowerable, "IN_LIST_INVALID", "IN requires between 1 and 100 scalar literals.", "WHERE", expression.GetLocation(), "", "Use a non-empty literal list containing at most 100 values.")
		}
		values := make([]any, 0, len(list.GetItems()))
		for _, item := range list.GetItems() {
			value, literalErr := literalValue(item)
			if literalErr != nil {
				return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_LITERAL_UNSUPPORTED", literalErr.Error(), "WHERE", nodeLocation(item), "", "Use string, boolean, or numeric literals in the IN list.")
			}
			values = append(values, value)
		}
		column, resolveErr := l.resolveColumn(columnRef, "WHERE")
		if resolveErr != nil {
			return queryplan.Filter{}, resolveErr
		}
		planOp := "IN"
		if op == "<>" {
			planOp = "NOT IN"
		}
		if valueErr := l.validateFilterValue(column, planOp, values, expression.GetLocation()); valueErr != nil {
			return queryplan.Filter{}, valueErr
		}
		return queryplan.Filter{Column: l.planColumn(column), Op: planOp, Value: values}, nil
	default:
		return queryplan.Filter{}, reject(CodeNotLowerable, "FILTER_PREDICATE_UNSUPPORTED", "This WHERE predicate is outside the closed reporting SQL profile.", "WHERE", expression.GetLocation(), "", "Use an AND-conjunction of approved column-to-literal predicates.")
	}
}

func (l *lowerer) validateFilterValue(column resolvedColumn, operator string, value any, offset int32) *Error {
	product := l.sources[column.Source].Product
	typeName, typeErr := exposure.CanonicalSQLTypeV2(product.ColumnTypes[column.Column])
	if typeErr != nil {
		return reject(CodeNotLowerable, "FILTER_FIELD_TYPE_UNSUPPORTED", fmt.Sprintf("Column %q has a Catalog type outside the reporting SQL profile.", column.ID), "WHERE", offset, product.Name, "Use a column with a supported exact Catalog type.")
	}
	if operator == "LIKE" && typeName != "text" && typeName != "character" && typeName != "character varying" {
		return reject(CodeNotLowerable, "FILTER_OPERATOR_TYPE_MISMATCH", fmt.Sprintf("LIKE cannot be applied to column %q with type %q.", column.ID, typeName), "WHERE", offset, product.Name, "Use LIKE with a Catalog text column.")
	}
	values := []any{value}
	if operator == "IN" || operator == "NOT IN" {
		values = value.([]any)
	}
	for _, item := range values {
		if _, err := exposure.CanonicalSQLValue(typeName, item); err != nil {
			return reject(CodeNotLowerable, "FILTER_LITERAL_TYPE_MISMATCH", fmt.Sprintf("The literal does not match Catalog type %q for column %q.", typeName, column.ID), "WHERE", offset, product.Name, "Use a literal of the column's Catalog SQL type.")
		}
	}
	return nil
}

func (l *lowerer) resolveColumn(reference *pg_query.ColumnRef, clause string) (resolvedColumn, *Error) {
	fields := reference.GetFields()
	if len(fields) == 0 || len(fields) > 2 {
		return resolvedColumn{}, reject(CodeColumnNotApproved, "COLUMN_REFERENCE_UNSUPPORTED", "Column references must be unqualified or relation-qualified names.", clause, reference.GetLocation(), "", "Use column or alias.column with approved Catalog names.")
	}
	names := make([]string, len(fields))
	for index, field := range fields {
		value, ok := stringNode(field)
		if !ok {
			return resolvedColumn{}, reject(CodeNotLowerable, "STAR_PROJECTION_UNSUPPORTED", "SELECT * and relation.* are outside the lossless reporting profile.", clause, reference.GetLocation(), "", "List each approved column explicitly.")
		}
		names[index] = value
	}
	if len(names) == 2 {
		index, present := l.sourceByAlias[names[0]]
		if !present {
			return resolvedColumn{}, reject(CodeColumnNotApproved, "RELATION_ALIAS_UNKNOWN", fmt.Sprintf("Relation alias %q is not present in FROM.", names[0]), clause, reference.GetLocation(), names[0], "Use an alias declared in FROM.")
		}
		if _, approved := l.sources[index].Product.Columns[names[1]]; !approved {
			return resolvedColumn{}, reject(CodeColumnNotApproved, CodeColumnNotApproved, fmt.Sprintf("Column %q is not approved for product %q.", names[1], l.sources[index].Product.Name), clause, reference.GetLocation(), l.sources[index].Product.Name, "Call describe_data_product and use an approved column.")
		}
		return resolvedColumn{Source: index, Column: names[1], ID: l.sources[index].Scan.Role + "." + names[1]}, nil
	}
	match := -1
	for index, item := range l.sources {
		if _, approved := item.Product.Columns[names[0]]; !approved {
			continue
		}
		if match >= 0 {
			return resolvedColumn{}, reject(CodeColumnNotApproved, "COLUMN_REFERENCE_AMBIGUOUS", fmt.Sprintf("Unqualified column %q exists in more than one input.", names[0]), clause, reference.GetLocation(), "", "Qualify the column with its SQL relation alias.")
		}
		match = index
	}
	if match < 0 {
		return resolvedColumn{}, reject(CodeColumnNotApproved, CodeColumnNotApproved, fmt.Sprintf("Column %q is not approved for any input product.", names[0]), clause, reference.GetLocation(), "", "Call describe_data_product and use an approved column.")
	}
	return resolvedColumn{Source: match, Column: names[0], ID: l.sources[match].Scan.Role + "." + names[0]}, nil
}

func (l *lowerer) planColumn(column resolvedColumn) string {
	if l.multi {
		return column.ID
	}
	return column.Column
}

func (l *lowerer) lowerSinglePagination(statement *pg_query.SelectStmt, plan *queryplan.QueryPlan) *Error {
	if statement.GetLimitOption() == pg_query.LimitOption_LIMIT_OPTION_WITH_TIES {
		return reject(CodeNotLowerable, "FETCH_WITH_TIES_UNSUPPORTED", "FETCH ... WITH TIES cannot be represented by QueryPlan pagination.", "LIMIT", -1, "", "Use LIMIT with a positive integer and no WITH TIES modifier.")
	}
	selectedColumns := make(map[string]struct{}, len(plan.Columns))
	aggregateAliases := make(map[string]struct{}, len(plan.Aggregates))
	for _, column := range plan.Columns {
		selectedColumns[column] = struct{}{}
	}
	for _, aggregate := range plan.Aggregates {
		aggregateAliases[aggregate.Alias] = struct{}{}
	}
	for _, sortNode := range statement.GetSortClause() {
		sortBy := sortNode.GetSortBy()
		if sortBy == nil || len(sortBy.GetUseOp()) != 0 {
			return reject(CodeNotLowerable, "ORDER_EXPRESSION_UNSUPPORTED", "ORDER BY accepts selected columns or aggregate aliases only.", "ORDER BY", nodeLocation(sortNode), "", "Order by a selected column or aggregate alias using ASC or DESC.")
		}
		if sortBy.GetSortbyNulls() != pg_query.SortByNulls_SORT_BY_NULLS_UNDEFINED && sortBy.GetSortbyNulls() != pg_query.SortByNulls_SORTBY_NULLS_DEFAULT {
			return reject(CodeNotLowerable, "NULL_ORDER_UNSUPPORTED", "Explicit NULLS FIRST or NULLS LAST cannot be represented by QueryPlan.", "ORDER BY", sortBy.GetLocation(), "", "Remove the explicit NULLS ordering.")
		}
		direction := "ASC"
		switch sortBy.GetSortbyDir() {
		case pg_query.SortByDir_SORT_BY_DIR_UNDEFINED, pg_query.SortByDir_SORTBY_DEFAULT, pg_query.SortByDir_SORTBY_ASC:
		case pg_query.SortByDir_SORTBY_DESC:
			direction = "DESC"
		default:
			return reject(CodeNotLowerable, "ORDER_OPERATOR_UNSUPPORTED", "ORDER BY USING is outside the reporting SQL profile.", "ORDER BY", sortBy.GetLocation(), "", "Use ASC or DESC.")
		}
		reference := sortBy.GetNode().GetColumnRef()
		if reference == nil {
			return reject(CodeNotLowerable, "ORDER_EXPRESSION_UNSUPPORTED", "ORDER BY accepts selected columns or aggregate aliases only.", "ORDER BY", sortBy.GetLocation(), "", "Order by a selected column or aggregate alias.")
		}
		fields := reference.GetFields()
		if len(fields) == 1 {
			if name, ok := stringNode(fields[0]); ok {
				if _, aggregate := aggregateAliases[name]; aggregate {
					plan.OrderBy = append(plan.OrderBy, queryplan.Order{Column: name, Direction: direction})
					continue
				}
			}
		}
		column, resolveErr := l.resolveColumn(reference, "ORDER BY")
		if resolveErr != nil {
			return resolveErr
		}
		if _, selected := selectedColumns[column.Column]; !selected {
			return reject(CodeNotLowerable, "ORDER_FIELD_NOT_SELECTED", fmt.Sprintf("ORDER BY column %q is not selected.", column.Column), "ORDER BY", reference.GetLocation(), l.sources[column.Source].Product.Name, "Add the column to SELECT or order by a selected aggregate alias.")
		}
		plan.OrderBy = append(plan.OrderBy, queryplan.Order{Column: column.Column, Direction: direction})
	}

	if statement.GetLimitCount() != nil {
		limit, err := nonnegativeIntegerLiteral(statement.GetLimitCount(), "LIMIT")
		if err != nil {
			return err
		}
		if limit == 0 {
			return reject(CodeNotLowerable, "ZERO_LIMIT_UNSUPPORTED", "LIMIT 0 cannot be represented losslessly by QueryPlan.", "LIMIT", nodeLocation(statement.GetLimitCount()), "", "Remove LIMIT 0 or request a positive integer limit.")
		}
		plan.Limit = limit
	}
	if statement.GetLimitOffset() != nil {
		offset, err := nonnegativeIntegerLiteral(statement.GetLimitOffset(), "OFFSET")
		if err != nil {
			return err
		}
		plan.Offset = offset
	}
	return nil
}

func nonnegativeIntegerLiteral(node *pg_query.Node, clause string) (int, *Error) {
	value, err := literalValue(node)
	if err != nil {
		return 0, reject(CodeNotLowerable, "PAGINATION_LITERAL_REQUIRED", clause+" requires a non-negative integer literal.", clause, nodeLocation(node), "", "Use a non-negative integer literal.")
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, reject(CodeNotLowerable, "PAGINATION_LITERAL_REQUIRED", clause+" requires a non-negative integer literal.", clause, nodeLocation(node), "", "Use a non-negative integer literal.")
	}
	parsed, parseErr := strconv.ParseInt(string(number), 10, 64)
	if parseErr != nil || parsed < 0 || int64(int(parsed)) != parsed {
		return 0, reject(CodeNotLowerable, "PAGINATION_LITERAL_INVALID", clause+" requires a non-negative integer literal in range.", clause, nodeLocation(node), "", "Use a smaller non-negative integer literal.")
	}
	return int(parsed), nil
}

func (l *lowerer) classifyCompilerError(err error, clause string) *Error {
	message := err.Error()
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "graph must be connected") || strings.Contains(lower, "graph cannot be compiled"):
		return reject(CodeJoinGraphDisconnected, CodeJoinGraphDisconnected, message, "FROM", -1, "", "Connect every product with approved equality predicates.")
	case strings.Contains(lower, "collation"):
		return reject(CodeCollationMismatch, CodeCollationMismatch, message, "FROM", -1, "", "Use keys with identical deterministic Catalog collation profiles.")
	case strings.Contains(lower, "join keys require identical types") || strings.Contains(lower, "join graph keys") && strings.Contains(lower, "require identical types"):
		return reject(CodeJoinKeyTypeMismatch, CodeJoinKeyTypeMismatch, message, "FROM", -1, "", "Use join keys with identical Catalog SQL types.")
	case strings.Contains(lower, "product") && strings.Contains(lower, "not approved"):
		return reject(CodeProductNotApproved, CodeProductNotApproved, message, clause, -1, "", "Use a product approved for this task.")
	case strings.Contains(lower, "column") && strings.Contains(lower, "not approved"):
		return reject(CodeColumnNotApproved, CodeColumnNotApproved, message, clause, -1, "", "Use an approved Catalog column.")
	default:
		return reject(CodeNotLowerable, "QUERYPLAN_VALIDATION_FAILED", message, clause, -1, "", "Rewrite the SQL using the advertised reporting SQL capabilities.")
	}
}

func conjunctionTerms(node *pg_query.Node, clause string) ([]*pg_query.Node, *Error) {
	if expression := node.GetBoolExpr(); expression != nil {
		if expression.GetBoolop() != pg_query.BoolExprType_AND_EXPR {
			return nil, reject(CodeNotLowerable, "BOOLEAN_OPERATOR_UNSUPPORTED", clause+" supports AND conjunctions only.", clause, expression.GetLocation(), "", "Rewrite the predicate as supported conjunctive column comparisons.")
		}
		var result []*pg_query.Node
		for _, argument := range expression.GetArgs() {
			terms, err := conjunctionTerms(argument, clause)
			if err != nil {
				return nil, err
			}
			result = append(result, terms...)
		}
		return result, nil
	}
	return []*pg_query.Node{node}, nil
}

func literalValue(node *pg_query.Node) (any, error) {
	if node == nil {
		return nil, fmt.Errorf("a scalar literal is required")
	}
	constant := node.GetAConst()
	if constant != nil {
		if constant.GetIsnull() {
			return nil, fmt.Errorf("NULL literals are outside this predicate profile")
		}
		switch {
		case constant.GetSval() != nil:
			return constant.GetSval().GetSval(), nil
		case constant.GetBoolval() != nil:
			return constant.GetBoolval().GetBoolval(), nil
		case constant.GetIval() != nil:
			return json.Number(strconv.FormatInt(int64(constant.GetIval().GetIval()), 10)), nil
		case constant.GetFval() != nil:
			return json.Number(constant.GetFval().GetFval()), nil
		default:
			return nil, fmt.Errorf("this literal type is outside the reporting SQL profile")
		}
	}
	if expression := node.GetAExpr(); expression != nil && expression.GetKind() == pg_query.A_Expr_Kind_AEXPR_OP && expression.GetLexpr() == nil {
		op := operatorName(expression.GetName())
		if op == "+" || op == "-" {
			value, err := literalValue(expression.GetRexpr())
			if err != nil {
				return nil, err
			}
			number, ok := value.(json.Number)
			if !ok {
				return nil, fmt.Errorf("unary signs require a numeric literal")
			}
			text := string(number)
			if op == "-" {
				if strings.HasPrefix(text, "-") {
					text = strings.TrimPrefix(text, "-")
				} else {
					text = "-" + text
				}
			}
			return json.Number(text), nil
		}
	}
	return nil, fmt.Errorf("a string, boolean, or numeric literal is required")
}

func sortFilters(filters []queryplan.Filter) {
	sort.SliceStable(filters, func(i, j int) bool {
		left, _ := json.Marshal(filters[i].Value)
		right, _ := json.Marshal(filters[j].Value)
		leftKey := filters[i].Column + "\x00" + strings.ToUpper(filters[i].Op) + "\x00" + string(left)
		rightKey := filters[j].Column + "\x00" + strings.ToUpper(filters[j].Op) + "\x00" + string(right)
		return leftKey < rightKey
	})
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

func nodeLocation(node *pg_query.Node) int32 {
	if node == nil {
		return -1
	}
	switch {
	case node.GetColumnRef() != nil:
		return node.GetColumnRef().GetLocation()
	case node.GetAExpr() != nil:
		return node.GetAExpr().GetLocation()
	case node.GetBoolExpr() != nil:
		return node.GetBoolExpr().GetLocation()
	case node.GetFuncCall() != nil:
		return node.GetFuncCall().GetLocation()
	case node.GetResTarget() != nil:
		return node.GetResTarget().GetLocation()
	case node.GetRangeVar() != nil:
		return node.GetRangeVar().GetLocation()
	case node.GetAConst() != nil:
		return node.GetAConst().GetLocation()
	default:
		return -1
	}
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
