// Package sqlpolicy validates agent supplied PostgreSQL queries against a
// task-bound grant and rewrites accepted queries so that they can only see the
// grant's reporting views, columns, mandatory scopes, and remaining row budget.
package sqlpolicy

import "fmt"

// Code is a stable, externally safe policy rejection code.
type Code string

const (
	CodeInvalidSQL         Code = "SQL_INVALID"
	CodeMultipleStatements Code = "SQL_MULTIPLE_STATEMENTS"
	CodeNotSelect          Code = "SQL_NOT_SELECT"
	CodeWriteForbidden     Code = "SQL_WRITE_FORBIDDEN"
	CodeSelectInto         Code = "SQL_SELECT_INTO_FORBIDDEN"
	CodeLocking            Code = "SQL_LOCKING_FORBIDDEN"
	CodeRecursiveCTE       Code = "SQL_RECURSIVE_CTE_FORBIDDEN"
	CodeSystemObject       Code = "SQL_SYSTEM_OBJECT_FORBIDDEN"
	CodeObjectNotAllowed   Code = "SQL_OBJECT_NOT_ALLOWED"
	CodeWildcard           Code = "SQL_WILDCARD_FORBIDDEN"
	CodeFunctionNotAllowed Code = "SQL_FUNCTION_NOT_ALLOWED"
	CodeOperatorNotAllowed Code = "SQL_OPERATOR_NOT_ALLOWED"
	CodeParameter          Code = "SQL_PARAMETER_FORBIDDEN"
	CodeColumnNotAllowed   Code = "SQL_COLUMN_NOT_ALLOWED"
	CodeFeatureNotAllowed  Code = "SQL_FEATURE_NOT_ALLOWED"
	CodeBudgetExhausted    Code = "SQL_ROW_BUDGET_EXHAUSTED"
	CodeInvalidGrant       Code = "SQL_GRANT_INVALID"
)

var safeMessages = map[Code]string{
	CodeInvalidSQL:         "the SQL could not be parsed",
	CodeMultipleStatements: "exactly one SQL statement is required",
	CodeNotSelect:          "only a SELECT statement is allowed",
	CodeWriteForbidden:     "data-changing statements are not allowed",
	CodeSelectInto:         "SELECT INTO is not allowed",
	CodeLocking:            "row-locking clauses are not allowed",
	CodeRecursiveCTE:       "recursive common table expressions are not allowed",
	CodeSystemObject:       "system and schema-qualified objects are not allowed",
	CodeObjectNotAllowed:   "the query references an object outside the task grant",
	CodeWildcard:           "wildcard column selection is not allowed",
	CodeFunctionNotAllowed: "the query uses a function that is not allowed",
	CodeOperatorNotAllowed: "the query uses an operator that is not allowed",
	CodeParameter:          "client parameters and session variables are not allowed",
	CodeColumnNotAllowed:   "the query references a column outside the task grant",
	CodeFeatureNotAllowed:  "the query uses a SQL feature that is not allowed",
	CodeBudgetExhausted:    "the task has no remaining row budget",
	CodeInvalidGrant:       "the task grant cannot be used",
}

// PolicyError intentionally exposes neither physical object names nor parser
// diagnostics. The Code is suitable for an MCP error response.
type PolicyError struct {
	Code Code
}

func (e *PolicyError) Error() string {
	if e == nil {
		return ""
	}
	if message, ok := safeMessages[e.Code]; ok {
		return fmt.Sprintf("%s: %s", e.Code, message)
	}
	return string(e.Code)
}

// IsCode reports whether err is a policy rejection with the supplied code.
func IsCode(err error, code Code) bool {
	policyErr, ok := err.(*PolicyError)
	return ok && policyErr.Code == code
}

func reject(code Code) error {
	return &PolicyError{Code: code}
}

// ScopeOperator is deliberately closed: values are rendered by the policy and
// never copied into SQL as an arbitrary operator string.
type ScopeOperator string

const (
	ScopeEqual        ScopeOperator = "eq"
	ScopeNotEqual     ScopeOperator = "ne"
	ScopeLess         ScopeOperator = "lt"
	ScopeLessEqual    ScopeOperator = "lte"
	ScopeGreater      ScopeOperator = "gt"
	ScopeGreaterEqual ScopeOperator = "gte"
	ScopeIn           ScopeOperator = "in"
	ScopeNotIn        ScopeOperator = "not_in"
	ScopeIsNull       ScopeOperator = "is_null"
	ScopeIsNotNull    ScopeOperator = "is_not_null"
)

// ScopePredicate is a mandatory, trusted constraint derived from a TaskGrant.
// Values are data values, not SQL fragments.
type ScopePredicate struct {
	Column   string
	Operator ScopeOperator
	Values   []string
}

// ProductGrant maps the public logical product name exposed to an agent to a
// physical reporting view. Physical names never appear in a policy error.
type ProductGrant struct {
	LogicalName       string
	PhysicalSchema    string
	PhysicalView      string
	ApprovedColumns   []string
	AllowedFunctions  []string
	AllowedAggregates []string
	AllowedOperators  []string
	MandatoryScope    []ScopePredicate
}

// Grant is the immutable data portion of an approved task grant.
type Grant struct {
	Products []ProductGrant
}

// Request is one direct-SQL authorization request. RowLimit must already be
// clamped to the task's remaining cumulative row budget.
type Request struct {
	SQL      string
	Grant    Grant
	RowLimit int64
}

// Decision contains executable SQL and non-sensitive audit metadata.
type Decision struct {
	SQL                string
	CanonicalAgentSQL  string
	Fingerprint        string
	ReferencedProducts []string
	ReferencedColumns  []string
	RowLimit           int64
}

// Config controls the YAML/catalog-level expression allowlists. Empty slices
// select the package's conservative defaults.
type Config struct {
	AllowedFunctions  []string
	AllowedAggregates []string
	AllowedOperators  []string
}
