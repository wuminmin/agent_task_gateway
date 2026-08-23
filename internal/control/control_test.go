package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

func TestNotBeforePreservesCausalTimestampOrder(t *testing.T) {
	created := time.Date(2026, 7, 25, 12, 0, 0, 500, time.UTC)
	if got := notBefore(created.Add(-time.Microsecond), created); !got.Equal(created) {
		t.Fatalf("regressed timestamp = %s, want lower bound %s", got, created)
	}
	later := created.Add(time.Microsecond)
	if got := notBefore(later, created); !got.Equal(later) {
		t.Fatalf("monotonic timestamp = %s, want %s", got, later)
	}
}

const controlTestDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testCipher(t *testing.T, fill byte) *AES256GCM {
	t.Helper()
	cipher, err := NewAES256GCM(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCM: %v", err)
	}
	return cipher
}

func openTestStore(t *testing.T, path string, cipher ResultCipher, options ...Option) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, cipher, options...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createAwaitingApprovalTask(t *testing.T, store *Store, taskID string, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	principalID := "principal_" + taskID
	if err := store.CreatePrincipal(ctx, Principal{ID: principalID, Subject: "alice_" + taskID, Role: "requester"}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := store.CreateTask(ctx, Task{
		ID: taskID, PrincipalID: principalID, Objective: "analyze travel spend", State: TaskAwaitingApproval,
		CatalogVersion: "catalog-v1",
		RequestContext: []byte(`{"products":["expense_summary"],"scope":{"department":"sales"}}`),
		ExpiresAt:      &expires,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func approveTask(t *testing.T, store *Store, taskID string, expires time.Time, limits BudgetLimits) ApprovalCallback {
	t.Helper()
	callback := ApprovalCallback{
		EventID:       "oa_" + taskID,
		RawPayload:    []byte(`{"decision":"approved"}`),
		ExpectedState: TaskAwaitingApproval,
		NewState:      TaskActive,
		Response:      []byte(`{"ok":true}`),
		Event: ApprovalEvent{
			TaskID: taskID, Actor: "bob", Decision: "approved", Payload: []byte(`{"route":"manual"}`),
		},
		Grant: &TaskGrant{
			TaskID: taskID, Subject: "alice_" + taskID, Purpose: "travel analysis",
			ApprovedProducts: []string{"expense_summary"},
			ApprovedColumns:  map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope:   []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget: limits, ExpiresAt: expires, CatalogVersion: "catalog-v1", CatalogDigest: controlTestDigest,
			DatasourceID: "taskgate-test-expenses", SchemaDigest: controlTestDigest, ApprovalReceipt: "receipt_" + taskID,
		},
	}
	claim, err := store.ApplyApprovalCallback(context.Background(), callback)
	if err != nil {
		t.Fatalf("ApplyApprovalCallback: %v", err)
	}
	if !claim.Claimed || claim.Status != CallbackCompleted {
		t.Fatalf("unexpected callback claim: %+v", claim)
	}
	return callback
}

func testReserveRequest(request ReserveRequest) ReserveRequest {
	if request.CatalogDigest == "" {
		request.CatalogDigest = controlTestDigest
	}
	if request.DatasourceID == "" {
		request.DatasourceID = "taskgate-test-expenses"
	}
	if request.SchemaDigest == "" {
		request.SchemaDigest = controlTestDigest
	}
	if request.ManifestDigest == "" {
		request.ManifestDigest = controlTestDigest
	}
	if request.GrantDigest == "" {
		request.GrantDigest = controlTestDigest
	}
	if request.PolicyDecision == "" {
		request.PolicyDecision = "ALLOW"
	}
	return request
}

func TestMigrationsAndTaskRequestContext(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 1), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_context", expires)

	task, err := store.GetTask(context.Background(), "task_context")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	var gotContext, wantContext any
	if err := json.Unmarshal(task.RequestContext, &gotContext); err != nil {
		t.Fatalf("decode request context: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"products":["expense_summary"],"scope":{"department":"sales"}}`), &wantContext); err != nil {
		t.Fatalf("decode expected request context: %v", err)
	}
	if !reflect.DeepEqual(gotContext, wantContext) {
		t.Fatalf("request context was not preserved: %s", task.RequestContext)
	}
	for _, relation := range []string{
		"principals", "tasks", "task_grants", "grants", "approval_events", "budget_ledger",
		"query_records", "result_encryption_keys", "encrypted_query_results", "encrypted_query_result_chunks", "encrypted_results", "result_retention_holds", "audit_events", "query_receipts", "callback_idempotency",
		"view_binding_sets", "task_view_dependencies", "task_view_binding_status",
	} {
		var exists bool
		if err := store.DB().QueryRowContext(context.Background(), `
SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists); err != nil {
			t.Fatalf("inspect %s: %v", relation, err)
		}
		if !exists {
			t.Errorf("relation %s missing", relation)
		}
	}
}

func TestApprovalCallbackReplayAndConflict(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 2), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_callback", expires)
	callback := approveTask(t, store, "task_callback", expires, BudgetLimits{Queries: 2, Rows: 10, DBMS: 1000})

	replay, err := store.ApplyApprovalCallback(context.Background(), callback)
	if err != nil {
		t.Fatalf("callback replay: %v", err)
	}
	if !replay.Replay || !bytes.Equal(replay.Response, callback.Response) {
		t.Fatalf("replay did not return durable response: %+v", replay)
	}
	callback.RawPayload = []byte(`{"decision":"rejected"}`)
	if _, err := store.ApplyApprovalCallback(context.Background(), callback); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same event ID with another body: got %v", err)
	}
	events, err := store.ListApprovalEvents(context.Background(), "task_callback")
	if err != nil || len(events) != 1 {
		t.Fatalf("approval events = %d, err=%v", len(events), err)
	}
	if _, err := store.GetGrant(context.Background(), "task_callback"); err != nil {
		t.Fatalf("GetGrant: %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE task_grants SET purpose='tampered' WHERE task_id='task_callback'`); err == nil {
		t.Fatal("immutable grant update unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM task_grants WHERE task_id='task_callback'`); err == nil {
		t.Fatal("immutable grant delete unexpectedly succeeded")
	}
}

func TestApprovalCallbackPhaseTimingIsOffByDefaultAndCompleteWhenEnabled(t *testing.T) {
	clock := fixedClock{value: time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC)}
	expires := clock.value.Add(time.Hour)

	var disabled bytes.Buffer
	disabledStore := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 44), WithClock(clock))
	createCallbackTimingTask(t, disabledStore, "task_callback_timing_off", expires)
	submitCallbackTimingTask(t, disabledStore, "task_callback_timing_off")
	if disabled.Len() != 0 || disabledStore.callbackPhaseTiming != nil {
		t.Fatalf("default-disabled callback timing emitted output or installed a recorder: bytes=%d recorder=%v", disabled.Len(), disabledStore.callbackPhaseTiming != nil)
	}

	var enabled bytes.Buffer
	enabledStore := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 45),
		WithClock(clock), WithCallbackPhaseTiming(&enabled))
	createCallbackTimingTask(t, enabledStore, "task_callback_timing_on", expires)
	submitCallbackTimingTask(t, enabledStore, "task_callback_timing_on")
	approveTask(t, enabledStore, "task_callback_timing_on", expires, BudgetLimits{Queries: 2, Rows: 10, DBMS: 1000})
	var record CallbackPhaseTimingV1
	if err := json.NewDecoder(&enabled).Decode(&record); err != nil {
		t.Fatalf("decode callback phase timing: %v", err)
	}
	if record.SchemaVersion != 1 || record.Record != CallbackPhaseTimingV1Record ||
		record.TaskID != "task_callback_timing_on" || record.TaskIDSHA256 != CallbackPhaseTaskIDSHA256(record.TaskID) ||
		record.EventID != "oa_submitted_task_callback_timing_on" || record.FinalResult != "committed" ||
		record.ErrorClass != "none" || len(record.Phases) != len(callbackPhaseOrder) {
		t.Fatalf("unexpected callback phase timing record: %+v", record)
	}
	for index, phase := range record.Phases {
		if phase.Name != callbackPhaseOrder[index] || !phase.Attempted || phase.Result != "ok" ||
			phase.StartedOffsetMS < 0 || phase.FinishedOffsetMS < phase.StartedOffsetMS || phase.DurationMS < 0 {
			t.Fatalf("callback phase[%d] is incomplete: %+v", index, phase)
		}
	}
	if enabled.Len() != 0 {
		t.Fatalf("non-submitted approval callback emitted an extra timing record: %q", enabled.String())
	}
}

func createCallbackTimingTask(t *testing.T, store *Store, taskID string, expires time.Time) {
	t.Helper()
	ctx := context.Background()
	principalID := "principal_" + taskID
	if err := store.CreatePrincipal(ctx, Principal{ID: principalID, Subject: "alice_" + taskID, Role: "requester"}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := store.CreateTask(ctx, Task{ID: taskID, PrincipalID: principalID, Objective: "callback timing",
		State: TaskAwaitingSubmission, CatalogVersion: "catalog-v1", ExpiresAt: &expires}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func submitCallbackTimingTask(t *testing.T, store *Store, taskID string) {
	t.Helper()
	claim, err := store.ApplyApprovalCallback(context.Background(), ApprovalCallback{
		EventID: "oa_submitted_" + taskID, RawPayload: []byte(`{"status":"submitted"}`),
		Event:         ApprovalEvent{TaskID: taskID, Actor: "oa", Decision: "submitted"},
		ExpectedState: TaskAwaitingSubmission, NewState: TaskAwaitingApproval, Response: []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("ApplyApprovalCallback submitted: %v", err)
	}
	if !claim.Claimed || claim.Status != CallbackCompleted {
		t.Fatalf("unexpected submitted callback claim: %+v", claim)
	}
}

func TestConcurrentAuditAppendProducesContinuousChain(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 8))
	const count = 24
	errorsCh := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.AppendAuditEvent(context.Background(), AuditEvent{
				EventID: fmt.Sprintf("audit_concurrent_%02d", index), Actor: "test", EventType: "CONCURRENT_APPEND",
				Payload: mustJSON(map[string]any{"index": index}),
			})
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent audit append: %v", err)
		}
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("VerifyAuditChain after concurrent appends: %v", err)
	}
	events, err := store.ListAuditEvents(context.Background(), AuditFilter{Limit: count})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != count {
		t.Fatalf("audit event count = %d, want %d", len(events), count)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("audit sequence[%d] = %d, want %d", index, event.Sequence, index+1)
		}
	}
}

func TestBudgetSerializationFinalizationAndAuditChain(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 3), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_budget", expires)
	approveTask(t, store, "task_budget", expires, BudgetLimits{Queries: 5, Rows: 5, DBMS: 1000})

	requests := []ReserveRequest{
		testReserveRequest(ReserveRequest{QueryID: "query_a", TaskID: "task_budget", RequestID: "request-a", Actor: "alice", RequestDigest: "req-a", SQLFingerprint: "sql-a", RequestedRows: 100, RequestedDBMS: 500}),
		testReserveRequest(ReserveRequest{QueryID: "query_b", TaskID: "task_budget", RequestID: "request-b", Actor: "alice", RequestDigest: "req-b", SQLFingerprint: "sql-b", RequestedRows: 100, RequestedDBMS: 500}),
	}
	type reserveResult struct {
		reservation BudgetReservation
		err         error
	}
	results := make(chan reserveResult, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			reservation, err := store.ReserveBudget(context.Background(), request)
			results <- reserveResult{reservation: reservation, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var winner BudgetReservation
	var successes, inProgress int
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			winner = result.reservation
		case errors.Is(result.err, ErrQueryInProgress):
			inProgress++
		default:
			t.Fatalf("unexpected reserve error: %v", result.err)
		}
	}
	if successes != 1 || inProgress != 1 || winner.AllowedRows != 5 {
		t.Fatalf("reservation results: successes=%d inProgress=%d winner=%+v", successes, inProgress, winner)
	}

	plaintext := []byte(`{"columns":["month","amount"],"rows":[["2026-07",42]]}`)
	record, err := store.FinalizeQuery(context.Background(), BudgetSettlement{
		QueryID: winner.QueryID, Rows: winner.AllowedRows, DBMS: 37,
	}, plaintext)
	if err != nil {
		t.Fatalf("FinalizeQuery: %v", err)
	}
	if record.ChargedRows != 5 || record.ChargedQueries != 1 || record.ResultSHA256 == "" {
		t.Fatalf("unexpected query record: %+v", record)
	}
	_, decrypted, err := store.GetEncryptedResult(context.Background(), "task_budget", winner.QueryID)
	if err != nil {
		t.Fatalf("GetEncryptedResult: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted result differs: %s", decrypted)
	}
	var ciphertext []byte
	if err := store.DB().QueryRowContext(context.Background(), `SELECT ciphertext FROM encrypted_query_results WHERE query_id=$1`, winner.QueryID).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(ciphertext, []byte("2026-07")) {
		t.Fatal("plaintext leaked into stored ciphertext")
	}
	budget, err := store.GetBudget(context.Background(), "task_budget")
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if budget.Usage.UsedRows != 5 || budget.Usage.ReservedQueries != 0 {
		t.Fatalf("unexpected final budget: %+v", budget)
	}
	task, err := store.GetTask(context.Background(), "task_budget")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskArchived || task.TerminalReason != TerminalBudgetExhausted {
		t.Fatalf("task not auto-archived: %+v", task)
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	receipt, err := store.GetQueryReceipt(context.Background(), winner.QueryID)
	if err != nil {
		t.Fatalf("GetQueryReceipt: %v", err)
	}
	if receipt.Query.ResultSHA256 == "" || receipt.Audit.PreviousHash == "" || receipt.Audit.CurrentHash == "" {
		t.Fatalf("incomplete query receipt: %+v", receipt)
	}
}

func TestSettlementPreservesObservedDBMSWhenChargedIsClamped(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 18), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_observed_dbms", expires)
	approveTask(t, store, "task_observed_dbms", expires, BudgetLimits{Queries: 4, Rows: 100, DBMS: 1000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_observed_dbms", TaskID: "task_observed_dbms", RequestID: "request-observed-dbms",
		Actor: "alice", RequestDigest: "digest-observed-dbms", SQLFingerprint: "sql-observed-dbms",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}

	record, err := store.SettleBudget(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 3, DBMS: reservation.AllowedDBMS, ObservedDBMS: 100000,
	})
	if err != nil {
		t.Fatalf("SettleBudget: %v", err)
	}
	if record.ResultDBMS != reservation.AllowedDBMS || record.ResultObservedDBMS != 100000 ||
		record.ChargedDBMS != reservation.AllowedDBMS {
		t.Fatalf("returned settlement did not preserve observed/clamped DBMS: %+v", record)
	}
	persisted, err := store.GetQuery(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQuery: %v", err)
	}
	if persisted.ResultDBMS != record.ResultDBMS || persisted.ResultObservedDBMS != record.ResultObservedDBMS ||
		persisted.ChargedDBMS != record.ChargedDBMS {
		t.Fatalf("persisted settlement differs: returned=%+v persisted=%+v", record, persisted)
	}
	budget, err := store.GetBudget(context.Background(), "task_observed_dbms")
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if budget.Usage.UsedDBMS != reservation.AllowedDBMS || budget.Usage.ReservedDBMS != 0 ||
		budget.Usage.UsedDBMS+budget.Usage.ReservedDBMS > budget.Limits.DBMS {
		t.Fatalf("ledger invariant broken after observed DBMS clamp: %+v", budget)
	}
	events, err := store.ListAuditEvents(context.Background(), AuditFilter{
		TaskID: "task_observed_dbms", EventType: "QUERY_COMPLETED", Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("QUERY_COMPLETED events = %d, want 1", len(events))
	}
	var payload struct {
		ResultObservedDBMS int64 `json:"result_db_ms_observed"`
		ChargedDBMS        int64 `json:"charged_db_ms"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	if payload.ResultObservedDBMS != 100000 || payload.ChargedDBMS != reservation.AllowedDBMS {
		t.Fatalf("audit omitted observed/clamped DBMS: %+v", payload)
	}
}

func TestPurgeEncryptedResultsBeforeErasesCiphertextOnly(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 15), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_retention", expires)
	approveTask(t, store, "task_retention", expires, BudgetLimits{Queries: 3, Rows: 30, DBMS: 3000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_retention", TaskID: "task_retention", RequestID: "request-retention",
		Actor: "alice", RequestDigest: "digest-retention", SQLFingerprint: "sql-retention",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	plaintext := []byte(`{"columns":["month"],"rows":[["2026-07"]]}`)
	record, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: reservation.QueryID, Rows: 1, DBMS: 5}, plaintext)
	if err != nil {
		t.Fatalf("FinalizeQuery: %v", err)
	}
	if _, decrypted, err := store.GetEncryptedResult(context.Background(), "task_retention", reservation.QueryID); err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("GetEncryptedResult before purge = %q, %v", decrypted, err)
	}
	hold, err := store.SetResultRetentionHold(context.Background(), "task_retention", "litigation hold", "admin")
	if err != nil {
		t.Fatalf("SetResultRetentionHold: %v", err)
	}
	if hold.TaskID != "task_retention" || hold.Reason != "litigation hold" || hold.ReleasedAt != nil {
		t.Fatalf("unexpected active hold: %+v", hold)
	}
	purged, err := store.PurgeEncryptedResultsBefore(context.Background(), clock.value.Add(time.Second))
	if err != nil {
		t.Fatalf("PurgeEncryptedResultsBefore: %v", err)
	}
	if purged != 0 {
		t.Fatalf("purged rows under legal hold = %d, want 0", purged)
	}
	if _, decrypted, err := store.GetEncryptedResult(context.Background(), "task_retention", reservation.QueryID); err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("GetEncryptedResult after held purge = %q, %v", decrypted, err)
	}
	cleared, err := store.ClearResultRetentionHold(context.Background(), "task_retention", "admin")
	if err != nil {
		t.Fatalf("ClearResultRetentionHold: %v", err)
	}
	if cleared.ReleasedAt == nil || cleared.ReleasedBy != "admin" {
		t.Fatalf("unexpected cleared hold: %+v", cleared)
	}
	if _, err := store.GetResultRetentionHold(context.Background(), "task_retention"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetResultRetentionHold after clear = %v, want not found", err)
	}
	purged, err = store.PurgeEncryptedResultsBefore(context.Background(), clock.value.Add(time.Second))
	if err != nil {
		t.Fatalf("PurgeEncryptedResultsBefore after clear: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged rows = %d, want 1", purged)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_retention", reservation.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEncryptedResult after purge = %v, want not found", err)
	}
	retained, err := store.GetQuery(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQuery after purge: %v", err)
	}
	if retained.Status != QueryCompleted || retained.ResultSHA256 != record.ResultSHA256 {
		t.Fatalf("purge changed terminal query evidence: before=%+v after=%+v", record, retained)
	}
	evidence, err := store.GetQueryReceipt(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQueryReceipt after purge: %v", err)
	}
	if evidence.Query.ID != reservation.QueryID || evidence.Audit.QueryID != reservation.QueryID {
		t.Fatalf("purge removed receipt/audit evidence: %+v", evidence)
	}
	holdEvents, err := store.ListAuditEvents(context.Background(), AuditFilter{TaskID: "task_retention", EventType: "RETENTION_HOLD_SET", Limit: 10})
	if err != nil || len(holdEvents) != 1 {
		t.Fatalf("RETENTION_HOLD_SET events = %d, err=%v", len(holdEvents), err)
	}
	clearEvents, err := store.ListAuditEvents(context.Background(), AuditFilter{TaskID: "task_retention", EventType: "RETENTION_HOLD_CLEARED", Limit: 10})
	if err != nil || len(clearEvents) != 1 {
		t.Fatalf("RETENTION_HOLD_CLEARED events = %d, err=%v", len(clearEvents), err)
	}
	purgeEvents, err := store.ListAuditEvents(context.Background(), AuditFilter{EventType: "RETENTION_PURGE_RESULTS", Limit: 10})
	if err != nil || len(purgeEvents) != 1 {
		t.Fatalf("RETENTION_PURGE_RESULTS events = %d, err=%v", len(purgeEvents), err)
	}
	if again, err := store.PurgeEncryptedResultsBefore(context.Background(), clock.value.Add(2*time.Second)); err != nil || again != 0 {
		t.Fatalf("second purge = %d, %v; want 0, nil", again, err)
	}
	if _, err := store.PurgeEncryptedResultsBefore(context.Background(), time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero cutoff error = %v, want invalid", err)
	}
}

func TestEraseResultEncryptionKeyMakesCiphertextUnreadableButEvidenceRemains(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)}
	keyID := "result-key-erasure-test"
	cipher, err := NewAES256GCMWithKeyID(keyID, bytes.Repeat([]byte{0x16}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCMWithKeyID: %v", err)
	}
	store := openTestStore(t, path, cipher, WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_key_erasure", expires)
	approveTask(t, store, "task_key_erasure", expires, BudgetLimits{Queries: 3, Rows: 30, DBMS: 3000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_key_erasure", TaskID: "task_key_erasure", RequestID: "request-key-erasure",
		Actor: "alice", RequestDigest: "digest-key-erasure", SQLFingerprint: "sql-key-erasure",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	plaintext := []byte(`{"rows":[["redacted"]]}`)
	record, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: reservation.QueryID, Rows: 1, DBMS: 5}, plaintext)
	if err != nil {
		t.Fatalf("FinalizeQuery: %v", err)
	}
	stored, decrypted, err := store.GetEncryptedResult(context.Background(), "task_key_erasure", reservation.QueryID)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("GetEncryptedResult before key erasure = %q, %v", decrypted, err)
	}
	if stored.KeyID != keyID {
		t.Fatalf("stored key id = %q, want %q", stored.KeyID, keyID)
	}
	key, err := store.GetResultEncryptionKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("GetResultEncryptionKey: %v", err)
	}
	if key.Status != ResultEncryptionKeyActive || key.ErasedAt != nil {
		t.Fatalf("unexpected active key state: %+v", key)
	}
	erased, err := store.EraseResultEncryptionKey(context.Background(), keyID, "privacy-admin")
	if err != nil {
		t.Fatalf("EraseResultEncryptionKey: %v", err)
	}
	if erased.Status != ResultEncryptionKeyErased || erased.ErasedAt == nil || erased.ErasedBy != "privacy-admin" {
		t.Fatalf("unexpected erased key state: %+v", erased)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_key_erasure", reservation.QueryID); !errors.Is(err, ErrCipherUnavailable) {
		t.Fatalf("GetEncryptedResult after key erasure = %v, want cipher unavailable", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `
UPDATE result_encryption_keys SET status='ACTIVE', erased_at=NULL, erased_by='' WHERE key_id=$1`, keyID); err == nil {
		t.Fatal("erased result encryption key was reactivated")
	}
	retained, err := store.GetQuery(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQuery after key erasure: %v", err)
	}
	if retained.Status != QueryCompleted || retained.ResultSHA256 != record.ResultSHA256 {
		t.Fatalf("key erasure changed terminal query evidence: before=%+v after=%+v", record, retained)
	}
	evidence, err := store.GetQueryReceipt(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQueryReceipt after key erasure: %v", err)
	}
	if evidence.Query.ID != reservation.QueryID || evidence.Audit.QueryID != reservation.QueryID {
		t.Fatalf("key erasure removed receipt/audit evidence: %+v", evidence)
	}
	var resultRows int
	if err := store.DB().QueryRowContext(context.Background(), `
SELECT count(*) FROM encrypted_query_results WHERE query_id=$1 AND key_id=$2`, reservation.QueryID, keyID).Scan(&resultRows); err != nil {
		t.Fatalf("count encrypted result rows: %v", err)
	}
	if resultRows != 1 {
		t.Fatalf("encrypted result rows after key erasure = %d, want 1", resultRows)
	}
	events, err := store.ListAuditEvents(context.Background(), AuditFilter{EventType: "RESULT_ENCRYPTION_KEY_ERASED", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("RESULT_ENCRYPTION_KEY_ERASED events = %d, err=%v", len(events), err)
	}
	again, err := store.EraseResultEncryptionKey(context.Background(), keyID, "privacy-admin")
	if err != nil {
		t.Fatalf("EraseResultEncryptionKey replay: %v", err)
	}
	if again.ErasedAt == nil || !again.ErasedAt.Equal(*erased.ErasedAt) {
		t.Fatalf("erasure replay changed timestamp: first=%+v again=%+v", erased, again)
	}
}

func TestActiveResultEncryptionKeyUsesSharedSettlementLockAndBlocksErasure(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 42))
	ctx := context.Background()
	first, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(first)
	if err := ensureActiveResultEncryptionKeyTx(ctx, first, DefaultResultEncryptionKeyID, store.now()); err != nil {
		t.Fatal(err)
	}
	second, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(second)
	secondLock := make(chan error, 1)
	go func() {
		secondLock <- ensureActiveResultEncryptionKeyTx(ctx, second, DefaultResultEncryptionKeyID, store.now())
	}()
	select {
	case err := <-secondLock:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active result-key checks serialized concurrent settlements")
	}

	erasure := make(chan error, 1)
	go func() {
		_, err := store.EraseResultEncryptionKey(ctx, DefaultResultEncryptionKeyID, "privacy-admin")
		erasure <- err
	}()
	select {
	case err := <-erasure:
		t.Fatalf("key erasure bypassed active settlement locks: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-erasure:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("key erasure did not resume after settlements released shared locks")
	}
}

func TestConcurrentFirstUseOfCustomResultEncryptionKeyIsIdempotent(t *testing.T) {
	const keyID = "concurrent-first-use-key"
	cipher, err := NewAES256GCMWithKeyID(keyID, bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher)
	ctx := context.Background()
	expires := time.Now().UTC().Add(time.Hour)
	tasks := []string{"task_concurrent_key_a", "task_concurrent_key_b"}
	queries := []string{"query_concurrent_key_a", "query_concurrent_key_b"}
	for index, queryID := range queries {
		createAwaitingApprovalTask(t, store, tasks[index], expires)
		approveTask(t, store, tasks[index], expires, BudgetLimits{Queries: 1, Rows: 10, DBMS: 100})
		if _, err := store.ReserveBudget(ctx, testReserveRequest(ReserveRequest{
			QueryID: queryID, TaskID: tasks[index], RequestID: fmt.Sprintf("request-concurrent-key-%d", index),
			Actor: "alice", RequestDigest: fmt.Sprintf("request-digest-%d", index),
			SQLFingerprint: fmt.Sprintf("sql-fingerprint-%d", index), CatalogVersion: "catalog-v1",
			RequestedRows: 1, RequestedDBMS: 10,
		})); err != nil {
			t.Fatal(err)
		}
	}

	// SHARE permits both SELECT ... FOR SHARE no-row checks, but blocks their
	// subsequent INSERTs. Waiting until both INSERTs arrive makes the first-use
	// primary-key race deterministic rather than scheduler-dependent.
	blocker, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(ctx, `LOCK TABLE result_encryption_keys IN SHARE MODE`); err != nil {
		rollback(blocker)
		t.Fatal(err)
	}
	errorsSeen := make([]error, len(queries))
	var wait sync.WaitGroup
	for index, queryID := range queries {
		wait.Add(1)
		go func(index int, queryID string) {
			defer wait.Done()
			_, errorsSeen[index] = store.FinalizeQuery(ctx, BudgetSettlement{
				QueryID: queryID, Rows: 1, DBMS: 1,
			}, []byte(fmt.Sprintf(`{"query":%d}`, index)))
		}(index, queryID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock'
  AND query LIKE '%INSERT INTO result_encryption_keys%'`).Scan(&waiting); err != nil {
			rollback(blocker)
			wait.Wait()
			t.Fatal(err)
		}
		if waiting >= len(queries) {
			break
		}
		if time.Now().After(deadline) {
			rollback(blocker)
			wait.Wait()
			t.Fatal("concurrent finalizers did not both reach custom-key publication")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	for index, err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent custom-key finalize %d: %v", index, err)
		}
	}
	var keyRows, resultRows int
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM result_encryption_keys WHERE key_id=$1),
 (SELECT status FROM result_encryption_keys WHERE key_id=$1),
 (SELECT count(*) FROM encrypted_query_results WHERE key_id=$1)`, keyID).
		Scan(&keyRows, &status, &resultRows); err != nil {
		t.Fatal(err)
	}
	if keyRows != 1 || ResultEncryptionKeyStatus(status) != ResultEncryptionKeyActive || resultRows != len(queries) {
		t.Fatalf("custom-key publication rows=%d status=%s results=%d", keyRows, status, resultRows)
	}
}

func TestPersistedQueryReceiptIsIdempotentAndImmutable(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 13), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_receipt", expires)
	approveTask(t, store, "task_receipt", expires, BudgetLimits{Queries: 3, Rows: 30, DBMS: 3000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_receipt", TaskID: "task_receipt", RequestID: "request-receipt",
		Actor: "alice", RequestDigest: "digest-receipt", SQLFingerprint: "sql-receipt",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil || reservation.QueryID == "" {
		t.Fatalf("ReserveBudget: %+v, %v", reservation, err)
	}
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: reservation.QueryID, Rows: 2, DBMS: 7}, []byte(`{"rows":[[1],[2]]}`)); err != nil {
		t.Fatalf("FinalizeQuery: %v", err)
	}
	evidence, err := store.GetQueryReceipt(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQueryReceipt before save: %v", err)
	}
	if evidence.Receipt != nil {
		t.Fatalf("receipt unexpectedly persisted before save: %+v", evidence.Receipt)
	}
	request := SaveQueryReceiptRequest{
		QueryID: reservation.QueryID, Version: "2", GatewayKeyID: "gateway-key-1", Signature: "signature",
		SignedAt: clock.value.Add(time.Second), TerminalAuditSequence: evidence.Audit.Sequence,
		TerminalAuditHash: evidence.Audit.CurrentHash,
		ReceiptJSON:       []byte(`{"gateway_key_id":"gateway-key-1","query_id":"query_receipt","signature":"signature","version":"2"}`),
	}
	saved, err := store.SaveQueryReceipt(context.Background(), request)
	if err != nil {
		t.Fatalf("SaveQueryReceipt: %v", err)
	}
	if saved.ReceiptSHA256 == "" || !bytes.Equal(saved.ReceiptJSON, request.ReceiptJSON) {
		t.Fatalf("unexpected persisted receipt: %+v", saved)
	}
	replayed, err := store.SaveQueryReceipt(context.Background(), request)
	if err != nil {
		t.Fatalf("SaveQueryReceipt replay: %v", err)
	}
	if !bytes.Equal(replayed.ReceiptJSON, saved.ReceiptJSON) || replayed.ReceiptSHA256 != saved.ReceiptSHA256 {
		t.Fatalf("receipt replay changed evidence: saved=%+v replay=%+v", saved, replayed)
	}
	changed := request
	changed.ReceiptJSON = []byte(`{"gateway_key_id":"gateway-key-1","query_id":"query_receipt","signature":"changed","version":"2"}`)
	if _, err := store.SaveQueryReceipt(context.Background(), changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed receipt save error = %v, want conflict", err)
	}
	loaded, err := store.GetQueryReceipt(context.Background(), reservation.QueryID)
	if err != nil {
		t.Fatalf("GetQueryReceipt after save: %v", err)
	}
	if loaded.Receipt == nil || !bytes.Equal(loaded.Receipt.ReceiptJSON, request.ReceiptJSON) {
		t.Fatalf("stored receipt not returned: %+v", loaded.Receipt)
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE query_records SET result_rows=999 WHERE id=$1`, reservation.QueryID); err == nil {
		t.Fatal("terminal query record update unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM query_records WHERE id=$1`, reservation.QueryID); err == nil {
		t.Fatal("terminal query record delete unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE query_receipts SET signature='tampered' WHERE query_id=$1`, reservation.QueryID); err == nil {
		t.Fatal("immutable query receipt update unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM query_receipts WHERE query_id=$1`, reservation.QueryID); err == nil {
		t.Fatal("immutable query receipt delete unexpectedly succeeded")
	}
}

func TestTerminalSettlementWithReceiptPersistsAtomically(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 16), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	ctx := context.Background()
	builder := func(evidence QueryReceipt) (SaveQueryReceiptRequest, error) {
		return SaveQueryReceiptRequest{
			QueryID: evidence.Query.ID, Version: "test-v3", GatewayKeyID: "gateway-test-key",
			Signature: "signature-" + evidence.Query.ID, SignedAt: clock.value.Add(time.Second),
			TerminalAuditSequence: evidence.Audit.Sequence, TerminalAuditHash: evidence.Audit.CurrentHash,
			ReceiptJSON: []byte(fmt.Sprintf(`{"query_id":%q,"sequence":%d}`, evidence.Query.ID, evidence.Audit.Sequence)),
		}, nil
	}
	reserve := func(taskID, queryID string) {
		t.Helper()
		createAwaitingApprovalTask(t, store, taskID, expires)
		approveTask(t, store, taskID, expires, BudgetLimits{Queries: 5, Rows: 50, DBMS: 5000})
		if _, err := store.ReserveBudget(ctx, testReserveRequest(ReserveRequest{
			QueryID: queryID, TaskID: taskID, RequestID: "request-" + queryID,
			Actor: "alice", RequestDigest: "digest-" + queryID, SQLFingerprint: "sql-" + queryID,
			RequestedRows: 10, RequestedDBMS: 500,
		})); err != nil {
			t.Fatalf("ReserveBudget %s: %v", queryID, err)
		}
	}
	requireReceipt := func(queryID string, status QueryStatus, receipt PersistedQueryReceipt) {
		t.Helper()
		if receipt.QueryID != queryID || receipt.TerminalAuditSequence <= 0 || receipt.TerminalAuditHash == "" {
			t.Fatalf("incomplete persisted receipt for %s: %+v", queryID, receipt)
		}
		stored, err := store.GetPersistedQueryReceipt(ctx, queryID)
		if err != nil {
			t.Fatalf("GetPersistedQueryReceipt %s: %v", queryID, err)
		}
		if !bytes.Equal(stored.ReceiptJSON, receipt.ReceiptJSON) || stored.TerminalAuditSequence != receipt.TerminalAuditSequence {
			t.Fatalf("stored receipt changed for %s: returned=%+v stored=%+v", queryID, receipt, stored)
		}
		evidence, err := store.GetQueryReceipt(ctx, queryID)
		if err != nil {
			t.Fatalf("GetQueryReceipt %s: %v", queryID, err)
		}
		if evidence.Query.Status != status || evidence.Receipt == nil ||
			evidence.Audit.Sequence != receipt.TerminalAuditSequence || evidence.Audit.CurrentHash != receipt.TerminalAuditHash {
			t.Fatalf("receipt evidence mismatch for %s: evidence=%+v receipt=%+v", queryID, evidence, receipt)
		}
	}

	reserve("task_atomic_completed", "query_atomic_completed")
	completed, completedReceipt, _, err := store.FinalizeQueryMeasuredWithReceipt(ctx,
		BudgetSettlement{QueryID: "query_atomic_completed", Rows: 2, DBMS: 9},
		[]byte(`{"rows":[[1],[2]]}`), builder)
	if err != nil || completed.Status != QueryCompleted {
		t.Fatalf("FinalizeQueryMeasuredWithReceipt = %+v, %v", completed, err)
	}
	requireReceipt("query_atomic_completed", QueryCompleted, completedReceipt)

	reserve("task_atomic_released", "query_atomic_released")
	released, releasedReceipt, err := store.ReleaseBudgetWithReceipt(ctx, "query_atomic_released", "AUTHORIZATION_EXPIRED", builder)
	if err != nil || released.Status != QueryReleased {
		t.Fatalf("ReleaseBudgetWithReceipt = %+v, %v", released, err)
	}
	requireReceipt("query_atomic_released", QueryReleased, releasedReceipt)

	reserve("task_atomic_failed", "query_atomic_failed")
	failed, failedReceipt, err := store.FailBudgetWithReceipt(ctx,
		BudgetSettlement{QueryID: "query_atomic_failed", Rows: 3, DBMS: 11, ErrorCode: "RESULT_ENCODING_FAILED"}, builder)
	if err != nil || failed.Status != QueryFailed {
		t.Fatalf("FailBudgetWithReceipt = %+v, %v", failed, err)
	}
	requireReceipt("query_atomic_failed", QueryFailed, failedReceipt)

	reserve("task_atomic_indeterminate", "query_atomic_indeterminate")
	indeterminate, indeterminateReceipt, err := store.MarkIndeterminateWithReceipt(ctx, "query_atomic_indeterminate", "QUERY_TIMEOUT", builder)
	if err != nil || indeterminate.Status != QueryIndeterminate {
		t.Fatalf("MarkIndeterminateWithReceipt = %+v, %v", indeterminate, err)
	}
	requireReceipt("query_atomic_indeterminate", QueryIndeterminate, indeterminateReceipt)

	reserve("task_atomic_rollback", "query_atomic_rollback")
	_, _, _, err = store.FinalizeQueryMeasuredWithReceipt(ctx,
		BudgetSettlement{QueryID: "query_atomic_rollback", Rows: 1, DBMS: 5},
		[]byte(`{"rows":[[1]]}`), func(QueryReceipt) (SaveQueryReceiptRequest, error) {
			return SaveQueryReceiptRequest{}, errors.New("forced receipt builder failure")
		})
	if err == nil {
		t.Fatal("FinalizeQueryMeasuredWithReceipt succeeded despite receipt builder failure")
	}
	rolledBack, err := store.GetQuery(ctx, "query_atomic_rollback")
	if err != nil {
		t.Fatalf("GetQuery rollback: %v", err)
	}
	if rolledBack.Status != QueryReserved || rolledBack.CompletedAt != nil {
		t.Fatalf("receipt failure did not roll back terminal transition: %+v", rolledBack)
	}
	if _, err := store.GetPersistedQueryReceipt(ctx, "query_atomic_rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt exists after rollback = %v, want not found", err)
	}
	if _, _, err := store.GetEncryptedResult(ctx, "task_atomic_rollback", "query_atomic_rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("encrypted result exists after rollback = %v, want not found", err)
	}
	budget, err := store.GetBudget(ctx, "task_atomic_rollback")
	if err != nil {
		t.Fatalf("GetBudget rollback: %v", err)
	}
	if budget.Usage.ReservedQueries != 1 || budget.Usage.UsedQueries != 0 {
		t.Fatalf("receipt failure changed budget ledger: %+v", budget)
	}
}

func TestRequestIDReplayIsAtomicAndDigestBound(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 9), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_request_id", expires)
	approveTask(t, store, "task_request_id", expires, BudgetLimits{Queries: 3, Rows: 30, DBMS: 3000})

	request := testReserveRequest(ReserveRequest{
		QueryID: "query_request_id", TaskID: "task_request_id", RequestID: "client-request-1",
		Actor: "alice", RequestDigest: "digest-a", SQLFingerprint: "sql-a",
		RequestedRows: 10, RequestedDBMS: 500,
	})
	first, err := store.ReserveBudget(context.Background(), request)
	if err != nil || first.Replay {
		t.Fatalf("first ReserveBudget = %+v, %v", first, err)
	}
	retry := request
	retry.QueryID = "query_must_not_be_inserted"
	replayed, err := store.ReserveBudget(context.Background(), retry)
	if err != nil {
		t.Fatalf("replayed ReserveBudget: %v", err)
	}
	if !replayed.Replay || replayed.QueryID != first.QueryID || replayed.Record == nil || replayed.Record.Status != QueryReserved {
		t.Fatalf("replayed reservation = %+v", replayed)
	}
	conflict := retry
	conflict.RequestDigest = "digest-b"
	if _, err := store.ReserveBudget(context.Background(), conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting request id error = %v", err)
	}
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: first.QueryID, Rows: 2, DBMS: 7}, []byte(`{"rows":[[1],[2]]}`)); err != nil {
		t.Fatalf("FinalizeQuery: %v", err)
	}
	replayed, err = store.ReserveBudget(context.Background(), retry)
	if err != nil || replayed.Record == nil || replayed.Record.Status != QueryCompleted {
		t.Fatalf("terminal replay = %+v, %v", replayed, err)
	}
	queries, err := store.ListQueries(context.Background(), "task_request_id", 10)
	if err != nil || len(queries) != 1 {
		t.Fatalf("queries = %#v, %v; want exactly one", queries, err)
	}
	budget, err := store.GetBudget(context.Background(), "task_request_id")
	if err != nil || budget.Usage.UsedQueries != 1 || budget.Usage.ReservedQueries != 0 {
		t.Fatalf("budget = %+v, %v", budget, err)
	}
}

func TestListQueriesPageTraversesBeyondFiveHundredRecords(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 14), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_query_pages", expires)
	approveTask(t, store, "task_query_pages", expires, BudgetLimits{Queries: 1, Rows: 1, DBMS: 1})
	const total = 505
	for index := 0; index < total; index++ {
		queryID := fmt.Sprintf("query_page_%03d", index)
		if _, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
			QueryID: queryID, TaskID: "task_query_pages", RequestID: fmt.Sprintf("request-page-%03d", index),
			Actor: "alice", RequestDigest: fmt.Sprintf("digest-page-%03d", index),
			SQLFingerprint: "sql-page", RequestedRows: 1, RequestedDBMS: 1,
		})); err != nil {
			t.Fatalf("ReserveBudget %d: %v", index, err)
		}
		if _, err := store.ReleaseBudget(context.Background(), queryID, "TEST_RELEASE"); err != nil {
			t.Fatalf("ReleaseBudget %d: %v", index, err)
		}
	}
	seen := make(map[string]struct{}, total)
	cursor := ""
	pages := 0
	for {
		page, err := store.ListQueriesPage(context.Background(), "task_query_pages", cursor, 100)
		if err != nil {
			t.Fatalf("ListQueriesPage cursor %q: %v", cursor, err)
		}
		pages++
		if len(page.Records) > 100 {
			t.Fatalf("page contains %d records, want <= 100", len(page.Records))
		}
		for _, record := range page.Records {
			if record.Status != QueryReleased {
				t.Fatalf("record %s status = %s, want RELEASED", record.ID, record.Status)
			}
			if _, duplicate := seen[record.ID]; duplicate {
				t.Fatalf("duplicate query record %s", record.ID)
			}
			seen[record.ID] = struct{}{}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("paged query count = %d, want %d across %d pages", len(seen), total, pages)
	}
	if pages < 6 {
		t.Fatalf("expected traversal beyond five 100-record pages, got %d", pages)
	}
}

func TestRestartRecoveryAndCallbackRetry(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	cipher := testCipher(t, 4)
	store := openTestStore(t, path, cipher, WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_restart", expires)
	approveTask(t, store, "task_restart", expires, BudgetLimits{Queries: 3, Rows: 20, DBMS: 1000})
	if _, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_interrupted", TaskID: "task_restart", RequestID: "request-restart", Actor: "alice", RequestDigest: "request",
		SQLFingerprint: "select", RequestedRows: 10, RequestedDBMS: 500,
	})); err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	payload := []byte(`{"task_id":"task_restart","decision":"approved"}`)
	if _, err := store.ClaimCallback(context.Background(), "callback_interrupted", payload); err != nil {
		t.Fatalf("ClaimCallback: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	restarted, err := Open(context.Background(), path, cipher, WithClock(clock))
	if err != nil {
		t.Fatalf("restart Open: %v", err)
	}
	defer restarted.Close()
	record, err := restarted.GetQuery(context.Background(), "query_interrupted")
	if err != nil {
		t.Fatalf("GetQuery after restart: %v", err)
	}
	if record.Status != QueryIndeterminate || record.ErrorCode != "GATEWAY_RESTART" ||
		record.ChargedQueries != 1 || record.ChargedRows != 10 || record.ChargedDBMS != 500 ||
		record.ResultObservedDBMS != 500 {
		t.Fatalf("query was not recovered: %+v", record)
	}
	if record.BudgetAfter == nil || record.BudgetAfter.Usage.UsedQueries != 1 ||
		record.BudgetAfter.Usage.UsedRows != 10 || record.BudgetAfter.Usage.UsedDBMS != 500 {
		t.Fatalf("query does not contain recovered budget: %+v", record.BudgetAfter)
	}
	budget, err := restarted.GetBudget(context.Background(), "task_restart")
	if err != nil {
		t.Fatalf("GetBudget after restart: %v", err)
	}
	if budget.Usage.ReservedQueries != 0 || budget.Usage.ReservedRows != 0 || budget.Usage.ReservedDBMS != 0 ||
		budget.Usage.UsedQueries != 1 || budget.Usage.UsedRows != 10 || budget.Usage.UsedDBMS != 500 {
		t.Fatalf("reservation was not conservatively charged: %+v", budget)
	}
	claim, err := restarted.ClaimCallback(context.Background(), "callback_interrupted", payload)
	if err != nil {
		t.Fatalf("retry ClaimCallback: %v", err)
	}
	if !claim.Claimed || claim.Replay {
		t.Fatalf("callback was not reclaimed: %+v", claim)
	}
	response := []byte(`{"accepted":true}`)
	if err := restarted.CompleteCallback(context.Background(), "callback_interrupted", payload, response); err != nil {
		t.Fatalf("CompleteCallback: %v", err)
	}
	replay, err := restarted.ClaimCallback(context.Background(), "callback_interrupted", payload)
	if err != nil {
		t.Fatalf("replay ClaimCallback: %v", err)
	}
	if !replay.Replay || !bytes.Equal(replay.Response, response) {
		t.Fatalf("callback response not replayed: %+v", replay)
	}
	if err := restarted.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("VerifyAuditChain after restart: %v", err)
	}
}

func TestStartupRecoveryCanPersistIndeterminateReceipt(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	cipher := testCipher(t, 17)
	store := openTestStore(t, path, cipher, WithClock(clock), WithoutStartupRecovery())
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_recovery_receipt", expires)
	approveTask(t, store, "task_recovery_receipt", expires, BudgetLimits{Queries: 4, Rows: 10, DBMS: 1000})
	if _, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_recovery_receipt", TaskID: "task_recovery_receipt", RequestID: "request-recovery-receipt",
		Actor: "alice", RequestDigest: "request-recovery-receipt", SQLFingerprint: "select",
		RequestedRows: 10, RequestedDBMS: 400,
	})); err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	builder := func(evidence QueryReceipt) (SaveQueryReceiptRequest, error) {
		return SaveQueryReceiptRequest{
			QueryID: evidence.Query.ID, Version: "test-v3", GatewayKeyID: "gateway-recovery-key",
			Signature: "signature-" + evidence.Query.ID, SignedAt: clock.value.Add(time.Second),
			TerminalAuditSequence: evidence.Audit.Sequence, TerminalAuditHash: evidence.Audit.CurrentHash,
			ReceiptJSON: []byte(fmt.Sprintf(`{"query_id":%q,"recovered":true}`, evidence.Query.ID)),
		}, nil
	}
	restarted, err := Open(context.Background(), path, cipher, WithClock(clock), WithRecoveryReceiptBuilder(builder))
	if err != nil {
		t.Fatalf("restart Open with recovery receipt builder: %v", err)
	}
	defer restarted.Close()
	record, err := restarted.GetQuery(context.Background(), "query_recovery_receipt")
	if err != nil {
		t.Fatalf("GetQuery after recovery: %v", err)
	}
	if record.Status != QueryIndeterminate || record.ErrorCode != "GATEWAY_RESTART" {
		t.Fatalf("query was not recovered as indeterminate: %+v", record)
	}
	if record.ResultObservedDBMS != record.ReservedDBMS || record.ChargedDBMS != record.ReservedDBMS {
		t.Fatalf("recovery did not preserve observed reserved DBMS: %+v", record)
	}
	evidence, err := restarted.GetQueryReceipt(context.Background(), "query_recovery_receipt")
	if err != nil {
		t.Fatalf("GetQueryReceipt after recovery: %v", err)
	}
	if evidence.Receipt == nil || evidence.Receipt.TerminalAuditSequence != evidence.Audit.Sequence ||
		evidence.Audit.EventType != "QUERY_INDETERMINATE" {
		t.Fatalf("recovery receipt was not persisted with terminal evidence: %+v", evidence)
	}
}

func TestRecoveryChargesReservationAndArchivesAtHardLimit(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 8), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_recovery_limit", expires)
	approveTask(t, store, "task_recovery_limit", expires, BudgetLimits{Queries: 4, Rows: 10, DBMS: 1000})
	if _, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_recovery_limit", TaskID: "task_recovery_limit", RequestID: "request-recovery-limit", Actor: "alice",
		RequestDigest: "request", SQLFingerprint: "select", RequestedRows: 10, RequestedDBMS: 400,
	})); err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}

	report, err := store.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if report.InterruptedQueries != 1 {
		t.Fatalf("recovery report = %+v", report)
	}
	record, err := store.GetQuery(context.Background(), "query_recovery_limit")
	if err != nil {
		t.Fatalf("GetQuery: %v", err)
	}
	if record.Status != QueryIndeterminate || record.ChargedQueries != 1 || record.ChargedRows != 10 || record.ChargedDBMS != 400 {
		t.Fatalf("unexpected interrupted query: %+v", record)
	}
	budget, err := store.GetBudget(context.Background(), "task_recovery_limit")
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if budget.Usage != (BudgetUsage{UsedQueries: 1, UsedRows: 10, UsedDBMS: 400}) {
		t.Fatalf("unexpected recovered budget: %+v", budget)
	}
	task, err := store.GetTask(context.Background(), "task_recovery_limit")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskArchived || task.TerminalReason != TerminalBudgetExhausted {
		t.Fatalf("task was not archived at the hard limit: %+v", task)
	}

	second, err := store.Recover(context.Background())
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if second.InterruptedQueries != 0 {
		t.Fatalf("second recovery charged query again: %+v", second)
	}
	afterSecond, err := store.GetBudget(context.Background(), "task_recovery_limit")
	if err != nil {
		t.Fatalf("GetBudget after second recovery: %v", err)
	}
	if afterSecond.Usage != budget.Usage {
		t.Fatalf("second recovery changed budget: before=%+v after=%+v", budget.Usage, afterSecond.Usage)
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
}

func TestAES256GCMValidationAndAuthentication(t *testing.T) {
	if _, err := NewAES256GCM(make([]byte, 31)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("31-byte key error = %v", err)
	}
	if _, err := NewAES256GCMWithKeyID(" bad ", bytes.Repeat([]byte{0x7a}, 32)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid key id error = %v, want invalid", err)
	}
	key := bytes.Repeat([]byte{0x7a}, 32)
	cipher, err := NewAES256GCM(key)
	if err != nil {
		t.Fatal(err)
	}
	if cipher.KeyID() != DefaultResultEncryptionKeyID {
		t.Fatalf("default key id = %q, want %q", cipher.KeyID(), DefaultResultEncryptionKeyID)
	}
	nonce, ciphertext, err := cipher.Encrypt([]byte("secret result"), []byte("task/query"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := cipher.Decrypt(nonce, ciphertext, []byte("task/query")); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("tampered ciphertext error = %v", err)
	}
	encoded := fmt.Sprintf("%x", key)
	parsed, err := ParseAES256Key(encoded)
	if err != nil || !bytes.Equal(parsed, key) {
		t.Fatalf("ParseAES256Key: %v", err)
	}
}

func TestAuditChainDetectsTampering(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	store := openTestStore(t, path, testCipher(t, 5))
	if _, err := store.AppendAuditEvent(context.Background(), AuditEvent{
		EventID: "audit_test", Actor: "carol", EventType: "AUDIT_READ", Payload: []byte(`{"task_id":"t"}`),
	}); err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE audit_events SET payload_json='{}' WHERE event_id='audit_test'`); err == nil {
		t.Fatal("immutable audit update unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `DELETE FROM audit_events WHERE event_id='audit_test'`); err == nil {
		t.Fatal("immutable audit delete unexpectedly succeeded")
	}
	if _, err := store.DB().ExecContext(context.Background(), `DROP TRIGGER audit_events_no_update ON audit_events`); err != nil {
		t.Fatalf("drop test trigger: %v", err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `UPDATE audit_events SET payload_json='{}' WHERE event_id='audit_test'`); err != nil {
		t.Fatalf("tamper test row: %v", err)
	}
	if err := store.VerifyAuditChain(context.Background()); !errors.Is(err, ErrAuditChainBroken) {
		t.Fatalf("tampered chain error = %v", err)
	}
}

func TestCallbackPhaseTraceInFlightSnapshotsAndShutdownDump(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	recorder := newCallbackPhaseTimingRecorder(&out, func() sql.DBStats {
		return sql.DBStats{MaxOpenConnections: 32, OpenConnections: 32, InUse: 31, Idle: 1, WaitCount: 7, WaitDuration: 3 * time.Second}
	})
	recorder.now = func() time.Time { return now }
	store := &Store{callbackPhaseTiming: recorder}
	// drain returns every JSON line written since the previous call.
	drain := func() []string {
		text := strings.TrimSpace(out.String())
		out.Reset()
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
	decodeTrace := func(line string) CallbackPhaseTimingV1 {
		var record CallbackPhaseTimingV1
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode callback phase record: %v", err)
		}
		return record
	}

	trace := store.newCallbackPhaseTrace("task_in_flight", "evt_in_flight")
	finishClaim := trace.begin(callbackPhaseClaim)
	now = now.Add(time.Millisecond)
	finishClaim(nil)
	trace.begin(callbackPhaseAuditChainHead) // never finished: the hung phase

	// Below the stall threshold nothing is emitted by the watchdog path.
	now = now.Add(5 * time.Second)
	recorder.snapshotStalled(now)
	if lines := drain(); len(lines) != 0 {
		t.Fatalf("trace below the stall threshold was snapshotted: %v", lines)
	}
	// Past the threshold one snapshot names the in-progress phase with its
	// elapsed time; a second sweep inside the repeat cadence stays silent.
	now = now.Add(6 * time.Second)
	recorder.snapshotStalled(now)
	recorder.snapshotStalled(now.Add(time.Second))
	lines := drain()
	if len(lines) != 1 {
		t.Fatalf("stall sweeps emitted %d records, want 1: %v", len(lines), lines)
	}
	snapshot := decodeTrace(lines[0])
	if snapshot.FinalResult != callbackPhaseFinalInFlight || snapshot.SnapshotReason != "stall_threshold" ||
		snapshot.CurrentPhase != callbackPhaseAuditChainHead || snapshot.SnapshotIndex != 1 ||
		snapshot.InFlightAgeMS < 11_000 || snapshot.TaskIDSHA256 != CallbackPhaseTaskIDSHA256("task_in_flight") ||
		len(snapshot.Phases) != len(callbackPhaseOrder) ||
		snapshot.Phases[0].Result != "ok" || snapshot.Phases[2].Result != callbackPhaseResultInProgress ||
		snapshot.Phases[2].DurationMS < 10_990 || snapshot.Phases[3].Result != "not_attempted" {
		t.Fatalf("unexpected in-flight snapshot: %+v", snapshot)
	}

	// The explicit shutdown path dumps the trace again plus a pool snapshot.
	now = now.Add(2 * time.Second)
	if dumped := store.SnapshotInflightCallbackPhases("shutdown"); dumped != 1 {
		t.Fatalf("shutdown dumped %d traces, want 1", dumped)
	}
	lines = drain()
	if len(lines) != 2 {
		t.Fatalf("shutdown emitted %d records, want trace + pool: %v", len(lines), lines)
	}
	shutdown := decodeTrace(lines[0])
	var pool CallbackPoolSnapshotV1
	if err := json.Unmarshal([]byte(lines[1]), &pool); err != nil {
		t.Fatalf("decode pool snapshot: %v", err)
	}
	if shutdown.SnapshotReason != "shutdown" || shutdown.SnapshotIndex != 2 || shutdown.CurrentPhase != callbackPhaseAuditChainHead ||
		pool.Record != CallbackPoolSnapshotV1Record || pool.Reason != "shutdown" || pool.PoolInUse != 31 ||
		pool.PoolMaxOpen != 32 || pool.PoolWaitCount != 7 || pool.InFlightCallbacks != 1 || pool.StalledInFlight != 1 ||
		pool.InFlightCurrentPhase[callbackPhaseAuditChainHead] != 1 || pool.OldestInFlightAgeMS < 13_000 {
		t.Fatalf("unexpected shutdown records: trace=%+v pool=%+v", shutdown, pool)
	}

	// Finishing removes the trace from the registry and keeps the final record
	// shape: the still-open phase is closed as an error, no snapshot fields.
	trace.finish(nil, false)
	lines = drain()
	if len(lines) != 1 {
		t.Fatalf("finish emitted %d records, want 1: %v", len(lines), lines)
	}
	final := decodeTrace(lines[0])
	if final.FinalResult != "committed" || final.SnapshotReason != "" || final.CurrentPhase != "" ||
		final.InFlightAgeMS != 0 || final.Phases[2].Result != "error" || len(recorder.inflightTraces()) != 0 {
		t.Fatalf("unexpected final record after in-flight snapshots: %+v inflight=%d", final, len(recorder.inflightTraces()))
	}
	// Stopping a recorder whose watchdog never started must not block; it
	// emits only a final pool snapshot because nothing is in flight.
	recorder.stopWatchdog("test")
	if lines = drain(); len(lines) != 1 {
		t.Fatalf("stopWatchdog emitted %d records, want the pool snapshot only: %v", len(lines), lines)
	}
}
