package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

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
		CatalogVersion: "catalog-v1", RequestedBudget: []byte(`{"queries":2}`),
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
			Budget: limits, ExpiresAt: expires, CatalogVersion: "catalog-v1", ApprovalReceipt: "receipt_" + taskID,
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
		"query_records", "encrypted_query_results", "encrypted_results", "audit_events", "callback_idempotency",
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
		{QueryID: "query_a", TaskID: "task_budget", RequestID: "request-a", Actor: "alice", RequestDigest: "req-a", SQLFingerprint: "sql-a", RequestedRows: 100, RequestedDBMS: 500},
		{QueryID: "query_b", TaskID: "task_budget", RequestID: "request-b", Actor: "alice", RequestDigest: "req-b", SQLFingerprint: "sql-b", RequestedRows: 100, RequestedDBMS: 500},
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

func TestRequestIDReplayIsAtomicAndDigestBound(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 9), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_request_id", expires)
	approveTask(t, store, "task_request_id", expires, BudgetLimits{Queries: 3, Rows: 30, DBMS: 3000})

	request := ReserveRequest{
		QueryID: "query_request_id", TaskID: "task_request_id", RequestID: "client-request-1",
		Actor: "alice", RequestDigest: "digest-a", SQLFingerprint: "sql-a",
		RequestedRows: 10, RequestedDBMS: 500,
	}
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

func TestRestartRecoveryAndCallbackRetry(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	cipher := testCipher(t, 4)
	store := openTestStore(t, path, cipher, WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_restart", expires)
	approveTask(t, store, "task_restart", expires, BudgetLimits{Queries: 3, Rows: 20, DBMS: 1000})
	if _, err := store.ReserveBudget(context.Background(), ReserveRequest{
		QueryID: "query_interrupted", TaskID: "task_restart", RequestID: "request-restart", Actor: "alice", RequestDigest: "request",
		SQLFingerprint: "select", RequestedRows: 10, RequestedDBMS: 500,
	}); err != nil {
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
		record.ChargedQueries != 1 || record.ChargedRows != 10 || record.ChargedDBMS != 500 {
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

func TestRecoveryChargesReservationAndArchivesAtHardLimit(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 8), WithClock(clock))
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_recovery_limit", expires)
	approveTask(t, store, "task_recovery_limit", expires, BudgetLimits{Queries: 4, Rows: 10, DBMS: 1000})
	if _, err := store.ReserveBudget(context.Background(), ReserveRequest{
		QueryID: "query_recovery_limit", TaskID: "task_recovery_limit", RequestID: "request-recovery-limit", Actor: "alice",
		RequestDigest: "request", SQLFingerprint: "select", RequestedRows: 10, RequestedDBMS: 400,
	}); err != nil {
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
	key := bytes.Repeat([]byte{0x7a}, 32)
	cipher, err := NewAES256GCM(key)
	if err != nil {
		t.Fatal(err)
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
