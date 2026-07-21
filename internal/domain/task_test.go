package domain

import (
	"errors"
	"testing"
	"time"
)

func newTestTask(t *testing.T, now time.Time) Task {
	t.Helper()
	task, err := NewTask("task-1", "alice", "analyze expenses", "v1", now)
	if err != nil {
		t.Fatalf("NewTask returned error: %v", err)
	}
	return task
}

func TestTaskStateMachineHappyPath(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	task := newTestTask(t, now)
	if err := task.Transition(TaskEventSubmit, now.Add(time.Second)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := task.Transition(TaskEventApprove, now.Add(2*time.Second)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := task.Transition(TaskEventComplete, now.Add(3*time.Second)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if task.State != TaskStateArchived || task.TerminalReason != TerminalReasonCompleted {
		t.Fatalf("unexpected terminal task: %#v", task)
	}
}

func TestTaskStateMachineRejectsSkippedAndRepeatedTransitions(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	task := newTestTask(t, now)
	if err := task.Transition(TaskEventApprove, now.Add(time.Second)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("skipped approval error = %v", err)
	}
	if task.State != TaskStateAwaitingSubmission {
		t.Fatalf("failed transition mutated task state to %s", task.State)
	}
	if err := task.Transition(TaskEventSubmit, now.Add(time.Second)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := task.Transition(TaskEventSubmit, now.Add(2*time.Second)); !errors.Is(err, ErrDuplicateTransition) {
		t.Fatalf("duplicate submit error = %v", err)
	}
}

func TestTaskStateMachineRejectsTerminalChange(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	task := newTestTask(t, now)
	if err := task.Transition(TaskEventExpire, now.Add(time.Second)); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := task.Transition(TaskEventRevoke, now.Add(2*time.Second)); !errors.Is(err, ErrTerminalTask) {
		t.Fatalf("terminal mutation error = %v", err)
	}
}

func TestTaskStateMachineRejectsStaleAndExpiredCallback(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	task := newTestTask(t, now)
	if err := task.Transition(TaskEventSubmit, now.Add(time.Second)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := task.Transition(TaskEventApprove, now); !errors.Is(err, ErrStaleTransition) {
		t.Fatalf("stale callback error = %v", err)
	}
	task.ExpiresAt = now.Add(2 * time.Second)
	if err := task.Transition(TaskEventApprove, task.ExpiresAt); !errors.Is(err, ErrTransitionExpired) {
		t.Fatalf("expired callback error = %v", err)
	}
	if task.State != TaskStateAwaitingApproval {
		t.Fatalf("expired callback mutated task state to %s", task.State)
	}
}
