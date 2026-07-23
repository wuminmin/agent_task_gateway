package security

import (
	"context"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
)

// TestCrashRecovery exercises the deterministic crash-recovery path: a query
// left RESERVED by a gateway crash is conservatively charged its full
// reservation and marked INDETERMINATE on restart, the audit chain stays
// continuous, and the budget ledger invariant is preserved. Each case runs
// against its own isolated schema. The experiment reuses the live PostgreSQL
// control store (skipped without CONTROL_TEST_POSTGRES_DSN).
func TestCrashRecovery(t *testing.T) {
	rec := &violationRecorder{}
	defer func() {
		writeExperimentSummary(t, "crash_recovery", map[string]any{"budget_violations": rec.count})
	}()
	corpus := loadSecurityCorpus(t, "crash-recovery.json")
	for _, c := range corpus.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			runCrashRecoveryCase(t, c.ID, rec)
		})
	}
}

func runCrashRecoveryCase(t *testing.T, id string, rec *violationRecorder) {
	t.Helper()
	switch id {
	case "orphan_reserved_charged_indeterminate_on_restart":
		orphanChargedIndeterminate(t, rec)
	case "completed_before_crash_not_recharged":
		completedNotRecharged(t, rec)
	case "audit_chain_continuous_after_recovery":
		auditChainContinuous(t, rec)
	case "reservation_at_hard_limit_charged_on_recovery":
		hardLimitCharged(t, rec)
	default:
		t.Fatalf("unknown crash-recovery case: %s", id)
	}
}

func orphanChargedIndeterminate(t *testing.T, rec *violationRecorder) {
	dsn := securitySchemaDSN(t)
	store := openSecurityStoreOnDSN(t, dsn)
	limits := control.BudgetLimits{Queries: 8, Rows: 1000, DBMS: 5000}
	createApprovedTask(t, store, "task_orphan", limits)
	reserveQuery(t, store, "task_orphan", "q_orphan", "req_orphan", 10, 500)
	if err := store.Close(); err != nil {
		t.Fatalf("close before crash: %v", err)
	}

	restarted := openSecurityStoreOnDSN(t, dsn) // startup recovery runs here
	record, err := restarted.GetQuery(context.Background(), "q_orphan")
	if err != nil {
		t.Fatalf("GetQuery after restart: %v", err)
	}
	if record.Status != control.QueryIndeterminate {
		rec.budget(t, "orphan not marked INDETERMINATE on restart")
	}
	if record.ChargedQueries != 1 || record.ChargedRows != 10 || record.ChargedDBMS != 500 {
		rec.budget(t, "orphan not charged its full reservation on restart")
	}
	if !budgetInvariantHolds(t, restarted, "task_orphan") {
		rec.budget(t, "ledger invariant broken after orphan recovery")
	}
}

func completedNotRecharged(t *testing.T, rec *violationRecorder) {
	dsn := securitySchemaDSN(t)
	store := openSecurityStoreOnDSN(t, dsn)
	limits := control.BudgetLimits{Queries: 8, Rows: 1000, DBMS: 5000}
	createApprovedTask(t, store, "task_done", limits)
	reserveQuery(t, store, "task_done", "q_done", "req_done", 10, 500)
	if _, err := store.SettleBudget(context.Background(), control.BudgetSettlement{
		QueryID: "q_done", Rows: 5, DBMS: 120,
	}); err != nil {
		t.Fatalf("SettleBudget before crash: %v", err)
	}
	usageBeforeCrash := budgetUsage(t, store, "task_done")
	if err := store.Close(); err != nil {
		t.Fatalf("close before crash: %v", err)
	}

	restarted := openSecurityStoreOnDSN(t, dsn)
	record, err := restarted.GetQuery(context.Background(), "q_done")
	if err != nil {
		t.Fatalf("GetQuery after restart: %v", err)
	}
	if record.Status != control.QueryCompleted || record.ChargedRows != 5 || record.ChargedDBMS != 120 {
		rec.budget(t, "completed query mutated by recovery")
	}
	usageAfterRecovery := budgetUsage(t, restarted, "task_done")
	if usageAfterRecovery != usageBeforeCrash {
		rec.budget(t, "recovery recharged a terminal query")
	}
	if !budgetInvariantHolds(t, restarted, "task_done") {
		rec.budget(t, "ledger invariant broken after terminal recovery")
	}
}

func auditChainContinuous(t *testing.T, rec *violationRecorder) {
	dsn := securitySchemaDSN(t)
	store := openSecurityStoreOnDSN(t, dsn)
	limits := control.BudgetLimits{Queries: 8, Rows: 1000, DBMS: 5000}
	createApprovedTask(t, store, "task_chain", limits)
	reserveQuery(t, store, "task_chain", "q_chain", "req_chain", 10, 500)
	if err := store.Close(); err != nil {
		t.Fatalf("close before crash: %v", err)
	}

	restarted := openSecurityStoreOnDSN(t, dsn)
	if err := restarted.VerifyAuditChain(context.Background()); err != nil {
		rec.budget(t, "audit chain not continuous after recovery: "+err.Error())
	}
}

func hardLimitCharged(t *testing.T, rec *violationRecorder) {
	dsn := securitySchemaDSN(t)
	store := openSecurityStoreOnDSN(t, dsn)
	limits := control.BudgetLimits{Queries: 1, Rows: 10, DBMS: 500}
	createApprovedTask(t, store, "task_hard", limits)
	reserveQuery(t, store, "task_hard", "q_hard", "req_hard", 10, 500)
	if err := store.Close(); err != nil {
		t.Fatalf("close before crash: %v", err)
	}

	restarted := openSecurityStoreOnDSN(t, dsn)
	usage := budgetUsage(t, restarted, "task_hard")
	if usage.UsedQueries != limits.Queries || usage.UsedRows != limits.Rows || usage.UsedDBMS != limits.DBMS {
		rec.budget(t, "recovery did not charge the orphaned reservation to the hard limit")
	}
	if usage.ReservedQueries != 0 {
		rec.budget(t, "recovery left a reservation outstanding")
	}
	if !budgetInvariantHolds(t, restarted, "task_hard") {
		rec.budget(t, "ledger invariant broken when recovery reached the hard limit")
	}
}
