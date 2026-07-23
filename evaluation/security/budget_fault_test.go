package security

import (
	"context"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
)

// TestBudgetFault injects settlement-time faults and proves the budget ledger
// invariant (used+reserved <= limit on every axis) is preserved in each case.
// The experiment runs against the live PostgreSQL control store and is skipped
// without CONTROL_TEST_POSTGRES_DSN.
func TestBudgetFault(t *testing.T) {
	rec := &violationRecorder{}
	defer func() {
		writeExperimentSummary(t, "budget_fault", map[string]any{"budget_violations": rec.count})
	}()
	corpus := loadSecurityCorpus(t, "budget-faults.json")
	for _, c := range corpus.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			runBudgetFaultCase(t, c.ID, rec)
		})
	}
}

func runBudgetFaultCase(t *testing.T, id string, rec *violationRecorder) {
	t.Helper()
	switch id {
	case "overspend_settlement_rejected":
		overspendRejected(t, rec)
	case "dbms_observations_clamped_to_reservation":
		dbmsClamped(t, rec)
	case "exhausted_budget_blocks_new_reservation":
		exhaustionBlocks(t, rec)
	case "timeout_indeterminate_charges_full_reservation":
		indeterminateCharges(t, rec)
	case "repeated_settlement_is_idempotent":
		settlementIdempotent(t, rec)
	case "release_charges_zero":
		releaseZero(t, rec)
	default:
		t.Fatalf("unknown budget-fault case: %s", id)
	}
}

func budgetFaultStore(t *testing.T, limits control.BudgetLimits, taskID string) *control.Store {
	t.Helper()
	store := openSecurityStore(t)
	createApprovedTask(t, store, taskID, limits)
	return store
}

func overspendRejected(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 5000}, "task_overspend")
	reserveQuery(t, store, "task_overspend", "q_over", "req_over", 10, 500)
	_, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q_over", Rows: 1000, DBMS: 100,
	})
	if err == nil {
		rec.budget(t, "settlement exceeding reserved rows was accepted")
	}
	if !budgetInvariantHolds(t, store, "task_overspend") {
		rec.budget(t, "ledger invariant broken by rejected overspend")
	}
}

func dbmsClamped(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 500}, "task_clamp")
	reserveQuery(t, store, "task_clamp", "q_clamp", "req_clamp", 10, 500)
	record, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q_clamp", Rows: 3, DBMS: 500, ObservedDBMS: 100000,
	})
	if err != nil {
		t.Fatalf("SettleBudget: %v", err)
	}
	if record.ChargedDBMS != 500 {
		rec.budget(t, "observed DB time was not clamped to the reserved amount")
	}
	if record.ResultObservedDBMS != 100000 {
		rec.budget(t, "raw observed DB time was not preserved alongside the clamped charge")
	}
	if record.ChargedRows != 3 {
		rec.budget(t, "charged rows do not match the observed rows")
	}
	if !budgetInvariantHolds(t, store, "task_clamp") {
		rec.budget(t, "ledger invariant broken after DB-time clamp")
	}
}

func exhaustionBlocks(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 1, Rows: 100, DBMS: 5000}, "task_exhaust")
	reserveQuery(t, store, "task_exhaust", "q1", "r1", 10, 500)
	if _, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q1", Rows: 5, DBMS: 100,
	}); err != nil {
		t.Fatalf("SettleBudget q1: %v", err)
	}
	_, err := store.ReserveBudget(context.Background(), control.ReserveRequest{
		QueryID: "q2", TaskID: "task_exhaust", RequestID: "r2", Actor: "alice_task_exhaust",
		RequestDigest: securityDigest, SQLFingerprint: securityDigest, PolicyDecision: "ALLOW",
		CatalogVersion: "catalog-v1", CatalogDigest: securityDigest, DatasourceID: "taskgate-security",
		SchemaDigest: securityDigest, ManifestDigest: securityDigest, GrantDigest: securityDigest,
		RequestedRows: 10, RequestedDBMS: 500,
	})
	if err == nil {
		rec.budget(t, "a new reservation was accepted after the budget was exhausted")
	}
	if !budgetInvariantHolds(t, store, "task_exhaust") {
		rec.budget(t, "ledger invariant broken after exhaustion")
	}
}

func indeterminateCharges(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 5000}, "task_indet")
	reserveQuery(t, store, "task_indet", "q_indet", "req_indet", 25, 750)
	record, err := store.MarkIndeterminate(context.Background(), "q_indet", "QUERY_TIMEOUT")
	if err != nil {
		t.Fatalf("MarkIndeterminate: %v", err)
	}
	if record.Status != control.QueryIndeterminate || record.ChargedRows != 25 || record.ChargedDBMS != 750 {
		rec.budget(t, "INDETERMINATE query was not charged its full reservation")
	}
	if !budgetInvariantHolds(t, store, "task_indet") {
		rec.budget(t, "ledger invariant broken after indeterminate charge")
	}
}

func settlementIdempotent(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 5000}, "task_idem")
	reserveQuery(t, store, "task_idem", "q_idem", "req_idem", 10, 500)
	if _, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q_idem", Rows: 4, DBMS: 200,
	}); err != nil {
		t.Fatalf("first SettleBudget: %v", err)
	}
	usageAfterFirst := budgetUsage(t, store, "task_idem")
	if _, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q_idem", Rows: 4, DBMS: 200,
	}); err != nil {
		t.Fatalf("idempotent SettleBudget: %v", err)
	}
	usageAfterSecond := budgetUsage(t, store, "task_idem")
	if usageAfterFirst != usageAfterSecond {
		rec.budget(t, "repeated settlement changed the ledger charge")
	}
	if !budgetInvariantHolds(t, store, "task_idem") {
		rec.budget(t, "ledger invariant broken after repeated settlement")
	}
}

func releaseZero(t *testing.T, rec *violationRecorder) {
	store := budgetFaultStore(t, control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 5000}, "task_release")
	reserveQuery(t, store, "task_release", "q_rel", "req_rel", 10, 500)
	usageReserved := budgetUsage(t, store, "task_release")
	if usageReserved.ReservedQueries != 1 {
		t.Fatalf("expected one reserved query, got usage %+v", usageReserved)
	}
	if _, err := store.ReleaseBudget(context.Background(), "q_rel", "AUTHORIZATION_EXPIRED"); err != nil {
		t.Fatalf("ReleaseBudget: %v", err)
	}
	usageReleased := budgetUsage(t, store, "task_release")
	if usageReleased.UsedQueries != 0 || usageReleased.ReservedQueries != 0 {
		rec.budget(t, "released reservation was charged or left outstanding")
	}
	if !budgetInvariantHolds(t, store, "task_release") {
		rec.budget(t, "ledger invariant broken after release")
	}
}
