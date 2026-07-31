package viewcompiler

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// DefinitionInspection is the discovery-safe syntactic result for exact
// pg_get_viewdef output. It performs no catalog lookup or type inference;
// Compile repeats and strengthens every check against its immutable snapshot.
type DefinitionInspection struct {
	References    []RelationName `json:"references"`
	RecursiveSelf bool           `json:"recursive_self"`
}

// InspectDefinition parses one exact pg_get_viewdef SELECT, validates the
// restricted expandable grammar, and returns its schema-qualified relation
// references. Discovery should union these references with pg_depend so
// pinned/self dependencies cannot disappear at the PostgreSQL catalog layer.
func InspectDefinition(owner RelationName, definition string) (DefinitionInspection, error) {
	if err := validateRelationName(owner); err != nil {
		return DefinitionInspection{}, reject(CodeInvalidRegistry, owner, "%v", err)
	}
	parsed, err := pg_query.Parse(definition)
	if err != nil || len(parsed.GetStmts()) != 1 || parsed.GetStmts()[0].GetStmt().GetSelectStmt() == nil {
		return DefinitionInspection{}, reject(CodeDefinitionUnsupported, owner, "definition must contain exactly one PostgreSQL SELECT")
	}
	statement := parsed.GetStmts()[0].GetStmt().GetSelectStmt()
	if hasRecursiveSelf(statement, owner) {
		return DefinitionInspection{References: []RelationName{owner}, RecursiveSelf: true}, nil
	}
	if err := validateViewSelectShape(owner, statement); err != nil {
		return DefinitionInspection{}, err
	}
	if len(statement.GetFromClause()) != 1 {
		return DefinitionInspection{}, reject(CodeDefinitionUnsupported, owner, "definition requires one relation or explicit JOIN tree")
	}
	references := make(map[RelationName]struct{})
	if err := inspectFrom(owner, statement.GetFromClause()[0], references, 0); err != nil {
		return DefinitionInspection{}, err
	}
	if err := inspectTargets(owner, statement.GetTargetList()); err != nil {
		return DefinitionInspection{}, err
	}
	if err := inspectWhere(owner, statement.GetWhereClause()); err != nil {
		return DefinitionInspection{}, err
	}
	for _, group := range statement.GetGroupClause() {
		if group.GetColumnRef() == nil {
			return DefinitionInspection{}, reject(CodeDefinitionUnsupported, owner, "GROUP BY accepts direct columns only")
		}
		if err := inspectColumnRef(owner, group.GetColumnRef()); err != nil {
			return DefinitionInspection{}, err
		}
	}
	result := DefinitionInspection{References: make([]RelationName, 0, len(references))}
	for reference := range references {
		result.References = append(result.References, reference)
		result.RecursiveSelf = result.RecursiveSelf || reference == owner
	}
	sortRelationNames(result.References)
	return result, nil
}

func hasRecursiveSelf(statement *pg_query.SelectStmt, owner RelationName) bool {
	with := statement.GetWithClause()
	if with == nil || !with.GetRecursive() {
		return false
	}
	for _, node := range with.GetCtes() {
		cte := node.GetCommonTableExpr()
		if cte != nil && cte.GetCtename() == owner.Name {
			return true
		}
	}
	return false
}

func inspectFrom(owner RelationName, node *pg_query.Node, references map[RelationName]struct{}, depth int) error {
	if node == nil {
		return reject(CodeDefinitionUnsupported, owner, "FROM item is missing")
	}
	if depth > queryplan.MaxJoinSources {
		return reject(CodeSourceLimit, owner, "JOIN tree exceeds the source limit")
	}
	if relation := node.GetRangeVar(); relation != nil {
		if relation.GetCatalogname() != "" || relation.GetSchemaname() == "" || !relation.GetInh() {
			return reject(CodeDefinitionUnsupported, owner, "discovered relations must be schema-qualified ordinary references")
		}
		name := RelationName{Schema: relation.GetSchemaname(), Name: relation.GetRelname()}
		if err := validateRelationName(name); err != nil {
			return reject(CodeDefinitionUnsupported, owner, "referenced relation %q is invalid", name)
		}
		if relation.GetAlias() != nil && len(relation.GetAlias().GetColnames()) != 0 {
			return reject(CodeDefinitionUnsupported, owner, "relation column alias lists are unsupported")
		}
		references[name] = struct{}{}
		return nil
	}
	join := node.GetJoinExpr()
	if join == nil {
		return reject(CodeDefinitionUnsupported, owner, "subqueries and relation functions are unsupported")
	}
	if join.GetJointype() != pg_query.JoinType_JOIN_INNER || join.GetIsNatural() || len(join.GetUsingClause()) != 0 ||
		join.GetJoinUsingAlias() != nil || join.GetAlias() != nil || join.GetQuals() == nil {
		return reject(CodeDefinitionUnsupported, owner, "only explicit INNER JOIN ... ON is supported")
	}
	if err := inspectFrom(owner, join.GetLarg(), references, depth+1); err != nil {
		return err
	}
	if err := inspectFrom(owner, join.GetRarg(), references, depth+1); err != nil {
		return err
	}
	terms, err := conjunctionTerms(join.GetQuals(), owner, "JOIN")
	if err != nil {
		return err
	}
	for _, term := range terms {
		expression := term.GetAExpr()
		if expression == nil || expression.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP || operatorName(expression.GetName()) != "=" ||
			expression.GetLexpr().GetColumnRef() == nil || expression.GetRexpr().GetColumnRef() == nil {
			return reject(CodeDefinitionUnsupported, owner, "JOIN accepts direct column equalities only")
		}
		if err := inspectColumnRef(owner, expression.GetLexpr().GetColumnRef()); err != nil {
			return err
		}
		if err := inspectColumnRef(owner, expression.GetRexpr().GetColumnRef()); err != nil {
			return err
		}
	}
	return nil
}

func inspectTargets(owner RelationName, targets []*pg_query.Node) error {
	if len(targets) == 0 {
		return reject(CodeDefinitionUnsupported, owner, "SELECT list is empty")
	}
	for _, node := range targets {
		target := node.GetResTarget()
		if target == nil || len(target.GetIndirection()) != 0 {
			return reject(CodeDefinitionUnsupported, owner, "projection must contain direct columns or approved aggregates")
		}
		if column := target.GetVal().GetColumnRef(); column != nil {
			if err := inspectColumnRef(owner, column); err != nil {
				return err
			}
			continue
		}
		function := target.GetVal().GetFuncCall()
		if function == nil {
			return reject(CodeDefinitionUnsupported, owner, "scalar functions, casts, and projection expressions are unsupported")
		}
		name := strings.ToLower(operatorName(function.GetFuncname()))
		if name != "count" && name != "sum" && name != "min" && name != "max" {
			return reject(CodeDefinitionUnsupported, owner, "function %q is outside the aggregate fragment", name)
		}
		if function.GetAggDistinct() || function.GetAggFilter() != nil || len(function.GetAggOrder()) != 0 ||
			function.GetOver() != nil || function.GetAggWithinGroup() || function.GetFuncVariadic() {
			return reject(CodeDefinitionUnsupported, owner, "aggregate modifiers are unsupported")
		}
		if function.GetAggStar() {
			if name != "count" || len(function.GetArgs()) != 0 {
				return reject(CodeDefinitionUnsupported, owner, "only COUNT(*) accepts star")
			}
			continue
		}
		if len(function.GetArgs()) != 1 || function.GetArgs()[0].GetColumnRef() == nil {
			return reject(CodeDefinitionUnsupported, owner, "aggregate requires one direct column")
		}
		if err := inspectColumnRef(owner, function.GetArgs()[0].GetColumnRef()); err != nil {
			return err
		}
	}
	return nil
}

func inspectWhere(owner RelationName, node *pg_query.Node) error {
	if node == nil {
		return nil
	}
	terms, err := conjunctionTerms(node, owner, "WHERE")
	if err != nil {
		return err
	}
	for _, term := range terms {
		expression := term.GetAExpr()
		if expression == nil {
			return reject(CodeDefinitionUnsupported, owner, "WHERE accepts column-to-literal predicates only")
		}
		switch expression.GetKind() {
		case pg_query.A_Expr_Kind_AEXPR_OP:
			op := operatorName(expression.GetName())
			if op != "=" && op != "<>" && op != "!=" && op != "<" && op != "<=" && op != ">" && op != ">=" {
				return reject(CodeDefinitionUnsupported, owner, "WHERE operator %q is unsupported", op)
			}
			column, literal := expression.GetLexpr().GetColumnRef(), expression.GetRexpr()
			if column == nil {
				column, literal = expression.GetRexpr().GetColumnRef(), expression.GetLexpr()
			}
			if column == nil {
				return reject(CodeDefinitionUnsupported, owner, "WHERE comparison requires one direct column")
			}
			if err := inspectColumnRef(owner, column); err != nil {
				return err
			}
			if _, _, err := literalValue(literal); err != nil {
				return reject(CodeDefinitionUnsupported, owner, "WHERE literal: %v", err)
			}
		case pg_query.A_Expr_Kind_AEXPR_LIKE:
			if operatorName(expression.GetName()) != "~~" || expression.GetLexpr().GetColumnRef() == nil {
				return reject(CodeDefinitionUnsupported, owner, "only positive column LIKE literal is supported")
			}
			if _, _, err := literalValue(expression.GetRexpr()); err != nil {
				return reject(CodeDefinitionUnsupported, owner, "LIKE literal: %v", err)
			}
		case pg_query.A_Expr_Kind_AEXPR_IN:
			if expression.GetLexpr().GetColumnRef() == nil {
				return reject(CodeDefinitionUnsupported, owner, "IN requires a direct column")
			}
			list := expression.GetRexpr().GetList()
			if list == nil || len(list.GetItems()) == 0 || len(list.GetItems()) > 100 {
				return reject(CodeDefinitionUnsupported, owner, "IN requires 1..100 literals")
			}
			for _, item := range list.GetItems() {
				if _, _, err := literalValue(item); err != nil {
					return reject(CodeDefinitionUnsupported, owner, "IN literal: %v", err)
				}
			}
		default:
			return reject(CodeDefinitionUnsupported, owner, "WHERE predicate is outside the restricted fragment")
		}
	}
	return nil
}

func inspectColumnRef(owner RelationName, reference *pg_query.ColumnRef) error {
	if reference == nil || len(reference.GetFields()) == 0 || len(reference.GetFields()) > 2 {
		return reject(CodeDefinitionUnsupported, owner, "column reference must be name or alias.name")
	}
	for _, field := range reference.GetFields() {
		if _, ok := stringNode(field); !ok {
			return reject(CodeDefinitionUnsupported, owner, "star projections are unsupported")
		}
	}
	return nil
}
