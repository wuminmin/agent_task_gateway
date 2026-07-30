package control

import (
	"errors"
	"fmt"
)

// Stable sentinel errors returned by the control store. Callers should use
// errors.Is rather than matching database-driver error strings.
var (
	ErrNotFound = errors.New("control: not found")
	ErrConflict = errors.New("control: conflict")
	// ErrOrdinalCASConflict marks a retryable optimistic root-head epoch race.
	// The enclosing transaction has not committed when this error is returned.
	ErrOrdinalCASConflict = fmt.Errorf("%w: V4 root epoch changed", ErrConflict)
	// ErrMaterializationConflict is both a specific convergence failure and a
	// general conflict. It is returned only when one semantic cache key is
	// already committed to non-equivalent authorization/effect/result evidence.
	ErrMaterializationConflict  = fmt.Errorf("%w: semantic materialization evidence differs", ErrConflict)
	ErrInvalid                  = errors.New("control: invalid argument")
	ErrClosed                   = errors.New("control: store closed")
	ErrTaskNotActive            = errors.New("control: task is not active")
	ErrTaskExpired              = errors.New("control: task expired")
	ErrBudgetExhausted          = errors.New("control: budget exhausted")
	ErrExposureBudgetExhausted  = errors.New("control: exposure budget exhausted")
	ErrExposureEvidenceRequired = errors.New("control: exposure evidence required")
	ErrQueryInProgress          = errors.New("control: query already in progress")
	ErrReservationNotFound      = errors.New("control: budget reservation not found")
	ErrIdempotencyConflict      = errors.New("control: idempotency key reused with different payload")
	ErrCallbackInProgress       = errors.New("control: callback already in progress")
	ErrCipherUnavailable        = errors.New("control: result cipher unavailable")
	ErrCiphertextInvalid        = errors.New("control: encrypted result is invalid")
	ErrAuditChainBroken         = errors.New("control: audit hash chain is broken")
	ErrInvalidStateChange       = errors.New("control: invalid task state transition")
)

// ErrorCode is suitable for transport-layer error mapping.
type ErrorCode string

const (
	CodeInternal                 ErrorCode = "INTERNAL"
	CodeNotFound                 ErrorCode = "NOT_FOUND"
	CodeConflict                 ErrorCode = "CONFLICT"
	CodeInvalid                  ErrorCode = "INVALID_ARGUMENT"
	CodeClosed                   ErrorCode = "STORE_CLOSED"
	CodeTaskNotActive            ErrorCode = "TASK_NOT_ACTIVE"
	CodeTaskExpired              ErrorCode = "TASK_EXPIRED"
	CodeBudgetExhausted          ErrorCode = "BUDGET_EXHAUSTED"
	CodeExposureBudgetExhausted  ErrorCode = "EXPOSURE_BUDGET_EXHAUSTED"
	CodeExposureEvidenceRequired ErrorCode = "EXPOSURE_EVIDENCE_REQUIRED"
	CodeQueryInProgress          ErrorCode = "QUERY_IN_PROGRESS"
	CodeReservationMissing       ErrorCode = "RESERVATION_NOT_FOUND"
	CodeIdempotencyConflict      ErrorCode = "IDEMPOTENCY_CONFLICT"
	CodeCallbackInProgress       ErrorCode = "CALLBACK_IN_PROGRESS"
	CodeCipherUnavailable        ErrorCode = "CIPHER_UNAVAILABLE"
	CodeCiphertextInvalid        ErrorCode = "CIPHERTEXT_INVALID"
	CodeAuditChainBroken         ErrorCode = "AUDIT_CHAIN_BROKEN"
	CodeInvalidStateChange       ErrorCode = "INVALID_STATE_TRANSITION"
)

// OpError adds operation context while retaining a stable errors.Is target.
type OpError struct {
	Op    string
	Kind  error
	Cause error
}

func (e *OpError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return fmt.Sprintf("control %s: %v", e.Op, e.Kind)
	}
	return fmt.Sprintf("control %s: %v: %v", e.Op, e.Kind, e.Cause)
}

func (e *OpError) Unwrap() []error {
	if e == nil {
		return nil
	}
	if e.Cause == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Cause}
}

func opErr(op string, kind error, cause error) error {
	return &OpError{Op: op, Kind: kind, Cause: cause}
}

// CodeOf converts store errors into stable public codes.
func CodeOf(err error) ErrorCode {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrInvalid):
		return CodeInvalid
	case errors.Is(err, ErrClosed):
		return CodeClosed
	case errors.Is(err, ErrTaskNotActive):
		return CodeTaskNotActive
	case errors.Is(err, ErrTaskExpired):
		return CodeTaskExpired
	case errors.Is(err, ErrBudgetExhausted):
		return CodeBudgetExhausted
	case errors.Is(err, ErrExposureBudgetExhausted):
		return CodeExposureBudgetExhausted
	case errors.Is(err, ErrExposureEvidenceRequired):
		return CodeExposureEvidenceRequired
	case errors.Is(err, ErrQueryInProgress):
		return CodeQueryInProgress
	case errors.Is(err, ErrReservationNotFound):
		return CodeReservationMissing
	case errors.Is(err, ErrIdempotencyConflict):
		return CodeIdempotencyConflict
	case errors.Is(err, ErrCallbackInProgress):
		return CodeCallbackInProgress
	case errors.Is(err, ErrCipherUnavailable):
		return CodeCipherUnavailable
	case errors.Is(err, ErrCiphertextInvalid):
		return CodeCiphertextInvalid
	case errors.Is(err, ErrAuditChainBroken):
		return CodeAuditChainBroken
	case errors.Is(err, ErrInvalidStateChange):
		return CodeInvalidStateChange
	case errors.Is(err, ErrConflict):
		return CodeConflict
	default:
		return CodeInternal
	}
}
