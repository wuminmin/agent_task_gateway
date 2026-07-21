package control

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestLookupCallbackIsReadOnlyAcrossStatuses(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "control.db"), testCipher(t, 9))
	ctx := context.Background()
	payload := []byte(`{"task_id":"task_lookup","decision":"approved"}`)

	if _, err := store.LookupCallback(ctx, "missing", payload); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing callback lookup error = %v, want ErrNotFound", err)
	}
	if _, err := store.LookupCallback(ctx, "", payload); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid callback lookup error = %v, want ErrInvalid", err)
	}
	var callbackCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM callback_idempotency`).Scan(&callbackCount); err != nil {
		t.Fatalf("count callbacks after missing lookup: %v", err)
	}
	if callbackCount != 0 {
		t.Fatalf("missing lookup created %d callback rows", callbackCount)
	}

	claim, err := store.ClaimCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("ClaimCallback: %v", err)
	}
	if !claim.Claimed || claim.Status != CallbackProcessing {
		t.Fatalf("unexpected initial claim: %+v", claim)
	}
	var auditsBefore int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&auditsBefore); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	processing, err := store.LookupCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("lookup processing callback: %v", err)
	}
	if processing.Status != CallbackProcessing || processing.Claimed || processing.Replay || len(processing.Response) != 0 {
		t.Fatalf("unexpected processing lookup: %+v", processing)
	}
	if _, err := store.LookupCallback(ctx, "event_lookup", []byte(`{"different":true}`)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload lookup error = %v, want ErrIdempotencyConflict", err)
	}
	assertCallbackLookupDidNotWrite(t, store, ctx, CallbackProcessing, auditsBefore)

	if err := store.FailCallback(ctx, "event_lookup", payload, "temporary failure"); err != nil {
		t.Fatalf("FailCallback: %v", err)
	}
	retryable, err := store.LookupCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("lookup retryable callback: %v", err)
	}
	if retryable.Status != CallbackRetryable || retryable.Claimed || retryable.Replay {
		t.Fatalf("unexpected retryable lookup: %+v", retryable)
	}
	assertCallbackLookupDidNotWrite(t, store, ctx, CallbackRetryable, auditsBefore)

	reclaimed, err := store.RetryCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("RetryCallback: %v", err)
	}
	if !reclaimed.Claimed || reclaimed.Status != CallbackProcessing {
		t.Fatalf("unexpected reclaimed callback: %+v", reclaimed)
	}
	response := []byte(`{"ok":true}`)
	if err := store.CompleteCallback(ctx, "event_lookup", payload, response); err != nil {
		t.Fatalf("CompleteCallback: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&auditsBefore); err != nil {
		t.Fatalf("count completed audit events: %v", err)
	}
	completed, err := store.LookupCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("lookup completed callback: %v", err)
	}
	if completed.Status != CallbackCompleted || !completed.Replay || completed.Claimed || !bytes.Equal(completed.Response, response) {
		t.Fatalf("unexpected completed lookup: %+v", completed)
	}
	assertCallbackLookupDidNotWrite(t, store, ctx, CallbackCompleted, auditsBefore)

	completed.Response[0] = 'X'
	again, err := store.LookupCallback(ctx, "event_lookup", payload)
	if err != nil {
		t.Fatalf("second completed lookup: %v", err)
	}
	if !bytes.Equal(again.Response, response) {
		t.Fatalf("caller mutated durable callback response: got %q want %q", again.Response, response)
	}
}

func assertCallbackLookupDidNotWrite(t *testing.T, store *Store, ctx context.Context, wantStatus CallbackStatus, wantAudits int) {
	t.Helper()
	var status CallbackStatus
	if err := store.DB().QueryRowContext(ctx, `SELECT status FROM callback_idempotency WHERE event_id='event_lookup'`).Scan(&status); err != nil {
		t.Fatalf("load callback status: %v", err)
	}
	if status != wantStatus {
		t.Fatalf("lookup changed callback status: got %s want %s", status, wantStatus)
	}
	var auditCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit events after lookup: %v", err)
	}
	if auditCount != wantAudits {
		t.Fatalf("lookup wrote audit events: before=%d after=%d", wantAudits, auditCount)
	}
}
