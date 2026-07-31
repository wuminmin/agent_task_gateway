package sqllowering

import "fmt"

const (
	Profile = "taskgate-reporting-sql-v1"

	CodeSyntaxError           = "SQL_SYNTAX_ERROR"
	CodeProductNotApproved    = "PRODUCT_NOT_APPROVED"
	CodeColumnNotApproved     = "COLUMN_NOT_APPROVED"
	CodeNotLowerable          = "SQL_NOT_LOWERABLE"
	CodeSubqueryUnsupported   = "SUBQUERY_UNSUPPORTED"
	CodeJoinTypeUnsupported   = "JOIN_TYPE_UNSUPPORTED"
	CodeJoinGraphDisconnected = "JOIN_GRAPH_DISCONNECTED"
	CodeJoinKeyTypeMismatch   = "JOIN_KEY_TYPE_MISMATCH"
	CodeCollationMismatch     = "COLLATION_MISMATCH"
)

// Location is a stable, parser-derived place that an Agent can use when
// rewriting rejected SQL. Offset is the zero-based byte offset in the SQL, or
// -1 when PostgreSQL did not provide one.
type Location struct {
	Clause   string `json:"clause,omitempty"`
	Relation string `json:"relation,omitempty"`
	Offset   int32  `json:"offset"`
}

// Error is the machine-repairable rejection returned before execution or
// exposure-budget settlement.
type Error struct {
	Code        string   `json:"code"`
	Reason      string   `json:"reason"`
	Message     string   `json:"message"`
	Location    Location `json:"location"`
	Alternative string   `json:"supported_alternative,omitempty"`
	Retryable   bool     `json:"retryable_after_rewrite"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Reason == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

func reject(code, reason, message, clause string, offset int32, relation, alternative string) *Error {
	return &Error{
		Code:        code,
		Reason:      reason,
		Message:     message,
		Location:    Location{Clause: clause, Relation: relation, Offset: offset},
		Alternative: alternative,
		Retryable:   true,
	}
}
