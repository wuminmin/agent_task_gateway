package catalog

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidCatalog       = errors.New("invalid catalog")
	ErrMissingField         = errors.New("missing catalog field")
	ErrDuplicateSource      = errors.New("duplicate catalog source")
	ErrDuplicateProduct     = errors.New("duplicate catalog product")
	ErrDuplicateField       = errors.New("duplicate product field")
	ErrInvalidReportingView = errors.New("invalid reporting view")
	ErrPlaintextPassword    = errors.New("plaintext password is forbidden")
	ErrMissingSecretRef     = errors.New("source secretRef is required")
	ErrInvalidSecretRef     = errors.New("invalid source secretRef")
	ErrInvalidApprovalRoute = errors.New("invalid approval route")
	ErrInvalidBudgetProfile = errors.New("invalid budget profile")
	ErrUnknownProduct       = errors.New("unknown data product")
)

// ValidationError identifies a safe configuration path without retaining a
// rejected value (which could itself be a credential).
type ValidationError struct {
	Path    string
	Message string
	Cause   error
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

func (e ValidationError) Unwrap() error {
	if e.Cause == nil {
		return ErrInvalidCatalog
	}
	return e.Cause
}

func (e ValidationError) Is(target error) bool {
	return target == ErrInvalidCatalog
}

// ValidationErrors preserves every independently detectable startup error.
type ValidationErrors []error

func (e ValidationErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, err := range e {
		parts = append(parts, err.Error())
	}
	return fmt.Sprintf("%s: %s", ErrInvalidCatalog, strings.Join(parts, "; "))
}

func (e ValidationErrors) Unwrap() []error {
	return []error(e)
}

func (e ValidationErrors) Is(target error) bool {
	return target == ErrInvalidCatalog
}

func fieldError(path, message string, cause error) error {
	return ValidationError{Path: path, Message: message, Cause: cause}
}
