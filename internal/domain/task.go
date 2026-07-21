package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type TaskState string

const (
	TaskStateAwaitingSubmission TaskState = "AWAITING_SUBMISSION"
	TaskStateAwaitingApproval   TaskState = "AWAITING_APPROVAL"
	TaskStateActive             TaskState = "ACTIVE"
	TaskStateArchived           TaskState = "ARCHIVED"
)

type TerminalReason string

const (
	TerminalReasonCompleted       TerminalReason = "completed"
	TerminalReasonBudgetExhausted TerminalReason = "budget_exhausted"
	TerminalReasonRejected        TerminalReason = "rejected"
	TerminalReasonExpired         TerminalReason = "expired"
	TerminalReasonRevoked         TerminalReason = "revoked"
	TerminalReasonFailed          TerminalReason = "failed"
)

type TaskEvent string

const (
	TaskEventSubmit          TaskEvent = "submit"
	TaskEventApprove         TaskEvent = "approve"
	TaskEventReject          TaskEvent = "reject"
	TaskEventComplete        TaskEvent = "complete"
	TaskEventBudgetExhausted TaskEvent = "budget_exhausted"
	TaskEventExpire          TaskEvent = "expire"
	TaskEventRevoke          TaskEvent = "revoke"
	TaskEventFail            TaskEvent = "fail"
)

var (
	ErrInvalidTask         = errors.New("invalid task")
	ErrInvalidTaskState    = errors.New("invalid task state")
	ErrInvalidTransition   = errors.New("invalid task transition")
	ErrDuplicateTransition = errors.New("duplicate task transition")
	ErrTerminalTask        = errors.New("task is terminal")
	ErrStaleTransition     = errors.New("stale task transition")
	ErrTransitionExpired   = errors.New("task transition after expiry")
)

// Task is the durable state-machine aggregate. State changes must go through
// Transition so that skipped, repeated, stale, and post-terminal events fail
// closed.
type Task struct {
	ID                string         `json:"id"`
	Subject           string         `json:"subject"`
	Purpose           string         `json:"purpose"`
	RequestedProducts []string       `json:"requested_products"`
	Sensitivity       Sensitivity    `json:"sensitivity"`
	CatalogVersion    string         `json:"catalog_version"`
	State             TaskState      `json:"state"`
	TerminalReason    TerminalReason `json:"terminal_reason,omitempty"`
	LastEvent         TaskEvent      `json:"last_event,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	ExpiresAt         time.Time      `json:"expires_at,omitempty"`
}

func NewTask(id, subject, purpose, catalogVersion string, now time.Time) (Task, error) {
	task := Task{
		ID:             id,
		Subject:        subject,
		Purpose:        purpose,
		CatalogVersion: catalogVersion,
		State:          TaskStateAwaitingSubmission,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := task.Validate(); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s TaskState) Validate() error {
	switch s {
	case TaskStateAwaitingSubmission, TaskStateAwaitingApproval, TaskStateActive, TaskStateArchived:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidTaskState, s)
	}
}

func (r TerminalReason) Validate() error {
	switch r {
	case TerminalReasonCompleted, TerminalReasonBudgetExhausted, TerminalReasonRejected,
		TerminalReasonExpired, TerminalReasonRevoked, TerminalReasonFailed:
		return nil
	default:
		return fmt.Errorf("%w: invalid terminal reason %q", ErrInvalidTask, r)
	}
}

func (t Task) Validate() error {
	if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.Subject) == "" ||
		strings.TrimSpace(t.Purpose) == "" || strings.TrimSpace(t.CatalogVersion) == "" {
		return fmt.Errorf("%w: id, subject, purpose, and catalog_version are required", ErrInvalidTask)
	}
	if t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() || t.UpdatedAt.Before(t.CreatedAt) {
		return fmt.Errorf("%w: invalid timestamps", ErrInvalidTask)
	}
	if err := t.State.Validate(); err != nil {
		return err
	}
	if t.State == TaskStateArchived {
		if err := t.TerminalReason.Validate(); err != nil {
			return err
		}
	} else if t.TerminalReason != "" {
		return fmt.Errorf("%w: non-terminal task has terminal_reason", ErrInvalidTask)
	}
	return nil
}

func (t Task) IsTerminal() bool {
	return t.State == TaskStateArchived
}

// Transition applies one legal business event atomically to the in-memory
// aggregate. On error the receiver is unchanged.
func (t *Task) Transition(event TaskEvent, at time.Time) error {
	if t == nil {
		return fmt.Errorf("%w: nil task", ErrInvalidTask)
	}
	if at.IsZero() {
		return fmt.Errorf("%w: event time is required", ErrInvalidTransition)
	}
	if event == t.LastEvent && event != "" {
		return fmt.Errorf("%w: %s", ErrDuplicateTransition, event)
	}
	if t.IsTerminal() {
		return ErrTerminalTask
	}
	if !t.UpdatedAt.IsZero() && at.Before(t.UpdatedAt) {
		return ErrStaleTransition
	}
	if event != TaskEventExpire && !t.ExpiresAt.IsZero() && !at.Before(t.ExpiresAt) {
		return ErrTransitionExpired
	}

	next, reason, err := transitionFor(t.State, event)
	if err != nil {
		return err
	}
	updated := *t
	updated.State = next
	updated.TerminalReason = reason
	updated.LastEvent = event
	updated.UpdatedAt = at
	*t = updated
	return nil
}

func transitionFor(state TaskState, event TaskEvent) (TaskState, TerminalReason, error) {
	switch event {
	case TaskEventSubmit:
		if state != TaskStateAwaitingSubmission {
			return "", "", invalidTransition(state, event)
		}
		return TaskStateAwaitingApproval, "", nil
	case TaskEventApprove:
		if state != TaskStateAwaitingApproval {
			return "", "", invalidTransition(state, event)
		}
		return TaskStateActive, "", nil
	case TaskEventReject:
		if state != TaskStateAwaitingApproval {
			return "", "", invalidTransition(state, event)
		}
		return TaskStateArchived, TerminalReasonRejected, nil
	case TaskEventComplete:
		if state != TaskStateActive {
			return "", "", invalidTransition(state, event)
		}
		return TaskStateArchived, TerminalReasonCompleted, nil
	case TaskEventBudgetExhausted:
		if state != TaskStateActive {
			return "", "", invalidTransition(state, event)
		}
		return TaskStateArchived, TerminalReasonBudgetExhausted, nil
	case TaskEventExpire:
		return TaskStateArchived, TerminalReasonExpired, nil
	case TaskEventRevoke:
		return TaskStateArchived, TerminalReasonRevoked, nil
	case TaskEventFail:
		return TaskStateArchived, TerminalReasonFailed, nil
	default:
		return "", "", invalidTransition(state, event)
	}
}

func invalidTransition(state TaskState, event TaskEvent) error {
	return fmt.Errorf("%w: event %q from %q", ErrInvalidTransition, event, state)
}
