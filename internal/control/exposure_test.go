package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

const controlExposureProfile = "taskgate-exposure-v1"

func TestExposureSettlementIsNovelFactIdempotent(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 21))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_exposure_novel", expires)
	approveExposureTask(t, store, "task_exposure_novel", expires, ExposureLimits{ReleaseFacts: 4, InfluenceFacts: 4})

	observation := exposureObservation(t, "row-1", "amount", 10)
	first := reserveExposureQuery(t, store, "task_exposure_novel", "query_exposure_1", "request-1")
	settlement := BudgetSettlement{QueryID: first.QueryID, Rows: 1, DBMS: 1, Exposure: &observation}
	if _, err := store.FinalizeQuery(context.Background(), settlement, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatalf("FinalizeQuery first: %v", err)
	}
	if _, err := store.FinalizeQuery(context.Background(), settlement, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatalf("FinalizeQuery retry: %v", err)
	}

	second := reserveExposureQuery(t, store, "task_exposure_novel", "query_exposure_2", "request-2")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{
		QueryID: second.QueryID, Rows: 1, DBMS: 1, Exposure: &observation,
	}, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatalf("FinalizeQuery duplicate fact: %v", err)
	}
	ledger, err := store.GetExposureLedger(context.Background(), "task_exposure_novel")
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1}) {
		t.Fatalf("used exposure = %+v, want one fact in each ledger", ledger.Used)
	}
	charge, err := store.GetExposureCharge(context.Background(), second.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if charge.ActualReleaseFacts != 1 || charge.ActualInfluenceFacts != 1 || charge.ChargedReleaseFacts != 0 || charge.ChargedInfluenceFacts != 0 {
		t.Fatalf("duplicate charge = %+v", charge)
	}
}

func TestExposureOverBudgetDoesNotStoreOrChargeResult(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 22))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_exposure_limit", expires)
	approveExposureTask(t, store, "task_exposure_limit", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})

	firstObservation := exposureObservation(t, "row-1", "amount", 10)
	first := reserveExposureQuery(t, store, "task_exposure_limit", "query_limit_1", "request-1")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: first.QueryID, Rows: 1, DBMS: 1, Exposure: &firstObservation}, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatal(err)
	}

	secondObservation := exposureObservation(t, "row-2", "amount", 20)
	second := reserveExposureQuery(t, store, "task_exposure_limit", "query_limit_2", "request-2")
	settlement := BudgetSettlement{QueryID: second.QueryID, Rows: 1, DBMS: 1, Exposure: &secondObservation}
	if _, err := store.FinalizeQuery(context.Background(), settlement, []byte(`{"rows":[[20]]}`)); !errors.Is(err, ErrExposureBudgetExhausted) {
		t.Fatalf("FinalizeQuery error = %v, want ErrExposureBudgetExhausted", err)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_exposure_limit", second.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("over-budget result lookup = %v, want not found", err)
	}
	settlement.ErrorCode = "EXPOSURE_BUDGET_EXHAUSTED"
	if _, err := store.FailBudget(context.Background(), settlement); err != nil {
		t.Fatalf("FailBudget after rejection: %v", err)
	}
	ledger, err := store.GetExposureLedger(context.Background(), "task_exposure_limit")
	if err != nil || ledger.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1}) {
		t.Fatalf("ledger after rejection = %+v, %v", ledger, err)
	}
}

func TestDelegatedTasksShareRootAccountingState(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 23))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_root", expires)
	approveExposureTask(t, store, "task_root", expires, ExposureLimits{ReleaseFacts: 2, InfluenceFacts: 2})
	createDelegatedExposureTask(t, store, "task_child", "task_root", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})

	observation := exposureObservation(t, "shared-row", "amount", 10)
	rootQuery := reserveExposureQuery(t, store, "task_root", "query_root", "request-root")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: rootQuery.QueryID, Rows: 1, DBMS: 1, Exposure: &observation}, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatal(err)
	}
	childQuery := reserveExposureQuery(t, store, "task_child", "query_child", "request-child")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: childQuery.QueryID, Rows: 1, DBMS: 1, Exposure: &observation}, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatal(err)
	}
	charge, err := store.GetExposureCharge(context.Background(), childQuery.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if charge.RootTaskID != "task_root" || charge.ChargedReleaseFacts != 0 || charge.ChargedInfluenceFacts != 0 {
		t.Fatalf("child charge = %+v", charge)
	}
	ledger, err := store.GetExposureLedger(context.Background(), "task_child")
	if err != nil || ledger.RootTaskID != "task_root" || ledger.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1}) {
		t.Fatalf("shared ledger = %+v, %v", ledger, err)
	}
}

func TestDelegatedTaskCannotAddFactsBeyondNarrowedFamilyCeiling(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 25))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_narrow_root", expires)
	approveExposureTask(t, store, "task_narrow_root", expires, ExposureLimits{ReleaseFacts: 3, InfluenceFacts: 3})
	createDelegatedExposureTask(t, store, "task_narrow_child", "task_narrow_root", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})

	accounted := exposureObservation(t, "accounted-row", "amount", 10)
	rootQuery := reserveExposureQuery(t, store, "task_narrow_root", "query_narrow_root", "request-root")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{
		QueryID: rootQuery.QueryID, Rows: 1, DBMS: 1, Exposure: &accounted,
	}, []byte(`{"rows":[[10]]}`)); err != nil {
		t.Fatal(err)
	}

	novel := exposureObservation(t, "novel-row", "amount", 20)
	childQuery := reserveExposureQuery(t, store, "task_narrow_child", "query_narrow_child", "request-child")
	settlement := BudgetSettlement{QueryID: childQuery.QueryID, Rows: 1, DBMS: 1, Exposure: &novel}
	if _, err := store.FinalizeQuery(context.Background(), settlement, []byte(`{"rows":[[20]]}`)); !errors.Is(err, ErrExposureBudgetExhausted) {
		t.Fatalf("child settlement error = %v, want ErrExposureBudgetExhausted", err)
	}
	settlement.ErrorCode = "EXPOSURE_BUDGET_EXHAUSTED"
	if _, err := store.FailBudget(context.Background(), settlement); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.GetExposureLedger(context.Background(), "task_narrow_child")
	if err != nil || ledger.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1}) {
		t.Fatalf("narrowed family ledger = %+v, %v", ledger, err)
	}
}

func TestConcurrentTaskFamilySettlementCannotOverspend(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 24))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_concurrent_root", expires)
	approveExposureTask(t, store, "task_concurrent_root", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})
	createDelegatedExposureTask(t, store, "task_concurrent_a", "task_concurrent_root", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})
	createDelegatedExposureTask(t, store, "task_concurrent_b", "task_concurrent_root", expires, ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1})

	queries := []BudgetReservation{
		reserveExposureQuery(t, store, "task_concurrent_a", "query_concurrent_a", "request-a"),
		reserveExposureQuery(t, store, "task_concurrent_b", "query_concurrent_b", "request-b"),
	}
	observations := []exposure.Observation{
		exposureObservation(t, "row-a", "amount", 10),
		exposureObservation(t, "row-b", "amount", 20),
	}
	errorsSeen := make([]error, len(queries))
	var wait sync.WaitGroup
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errorsSeen[index] = store.FinalizeQuery(context.Background(), BudgetSettlement{
				QueryID: queries[index].QueryID, Rows: 1, DBMS: 1, Exposure: &observations[index],
			}, []byte(fmt.Sprintf(`{"row":%d}`, index)))
		}(index)
	}
	wait.Wait()
	passed, rejected := 0, 0
	for _, err := range errorsSeen {
		switch {
		case err == nil:
			passed++
		case errors.Is(err, ErrExposureBudgetExhausted):
			rejected++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if passed != 1 || rejected != 1 {
		t.Fatalf("concurrent settlements: passed=%d rejected=%d", passed, rejected)
	}
	ledger, err := store.GetExposureLedger(context.Background(), "task_concurrent_root")
	if err != nil || ledger.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 1}) {
		t.Fatalf("concurrent ledger = %+v, %v", ledger, err)
	}
}

func approveExposureTask(t *testing.T, store *Store, taskID string, expires time.Time, limits ExposureLimits) {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.GetPrincipal(context.Background(), task.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	callback := ApprovalCallback{
		EventID: "oa_exposure_" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		ExpectedState: TaskAwaitingApproval, NewState: TaskActive, Response: []byte(`{"ok":true}`),
		Event: ApprovalEvent{TaskID: taskID, Actor: "bob", Decision: "approved", Payload: []byte(`{"route":"manual"}`)},
		Grant: &TaskGrant{TaskID: taskID, Subject: principal.Subject, Purpose: "travel analysis",
			ApprovedProducts: []string{"expense_summary"}, ApprovedColumns: map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope: []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget:    BudgetLimits{Queries: 5, Rows: 10, DBMS: 1000},
			Exposure:  ExposureGrant{Limits: limits, ProfileVersion: controlExposureProfile},
			ExpiresAt: expires, CatalogVersion: "catalog-v1", CatalogDigest: controlTestDigest,
			DatasourceID: "taskgate-test-expenses", SchemaDigest: controlTestDigest,
			ApprovalReceipt: "receipt_" + taskID},
	}
	if _, err := store.ApplyApprovalCallback(context.Background(), callback); err != nil {
		t.Fatalf("approve exposure task: %v", err)
	}
}

func createDelegatedExposureTask(t *testing.T, store *Store, taskID, parentTaskID string, expires time.Time, limits ExposureLimits) {
	t.Helper()
	parent, err := store.GetTask(context.Background(), parentTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), Task{ID: taskID, PrincipalID: parent.PrincipalID,
		Objective: "delegated analysis", State: TaskAwaitingApproval, CatalogVersion: parent.CatalogVersion,
		RequestedBudget: []byte(`{}`), RequestContext: []byte(`{}`), ParentTaskID: parentTaskID, ExpiresAt: &expires}); err != nil {
		t.Fatalf("create delegated task: %v", err)
	}
	approveExposureTask(t, store, taskID, expires, limits)
}

func reserveExposureQuery(t *testing.T, store *Store, taskID, queryID, requestID string) BudgetReservation {
	t.Helper()
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: queryID, TaskID: taskID, RequestID: requestID, Actor: "alice", RequestDigest: "digest-" + requestID,
		SQLFingerprint: "fingerprint-" + requestID, CatalogVersion: "catalog-v1", RequestedRows: 1, RequestedDBMS: 10,
		Exposure: &ExposureReservationRequest{ProfileVersion: controlExposureProfile, EstimatedReleaseFacts: 1, EstimatedInfluenceFacts: 1},
	}))
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	return reservation
}

func exposureObservation(t *testing.T, entity, field string, value any) exposure.Observation {
	t.Helper()
	fact, err := exposure.NewFact("expense_detail", "snapshot-1", entity, field, value)
	if err != nil {
		t.Fatal(err)
	}
	return exposure.Observation{ProfileVersion: controlExposureProfile, Release: []exposure.FactID{fact}, Influence: []exposure.FactID{fact}}
}
