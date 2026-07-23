package apierr

import (
	"errors"
	"fmt"
)

const (
	CodeInvalidRequest           = "INVALID_REQUEST"
	CodeUnauthenticated          = "UNAUTHENTICATED"
	CodeForbidden                = "FORBIDDEN"
	CodeNotFound                 = "NOT_FOUND"
	CodeConflict                 = "CONFLICT"
	CodeTaskNotActive            = "TASK_NOT_ACTIVE"
	CodePolicyDenied             = "POLICY_DENIED"
	CodeBudgetExhausted          = "BUDGET_EXHAUSTED"
	CodeExposureBudgetExhausted  = "EXPOSURE_BUDGET_EXHAUSTED"
	CodeExposureEvidenceRequired = "EXPOSURE_EVIDENCE_REQUIRED"
	CodeApprovalUnavailable      = "APPROVAL_UNAVAILABLE"
	CodeDataSourceUnavailable    = "DATA_SOURCE_UNAVAILABLE"
	CodeInternal                 = "INTERNAL_ERROR"
)

// Error contains a stable machine-readable code and a client-safe explanation.
// Cause is intentionally omitted from JSON responses.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) error { return &Error{Code: code, Message: message} }

func Wrap(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Public(err error) (code, message string) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code, appErr.Message
	}
	return CodeInternal, "请求处理失败；请使用 trace_id 联系管理员"
}
