package security

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
)

// TestConcurrency runs the reservation/replay/audit/settle concurrency matrix
// against the live PostgreSQL control store and asserts the gateway's
// serialization invariants hold under contention. Skipped without
// CONTROL_TEST_POSTGRES_DSN.
func TestConcurrency(t *testing.T) {
	rec := &violationRecorder{}
	defer func() {
		writeExperimentSummary(t, "concurrency", map[string]any{"budget_violations": rec.count})
	}()
	corpus := loadSecurityCorpus(t, "concurrency.json")
	for _, c := range corpus.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			runConcurrencyCase(t, c.ID, rec)
		})
	}
}

func runConcurrencyCase(t *testing.T, id string, rec *violationRecorder) {
	t.Helper()
	switch id {
	case "concurrent_reserve_single_winner":
		concurrentReserveSingleWinner(t, rec)
	case "concurrent_replay_single_insert":
		concurrentReplaySingleInsert(t, rec)
	case "concurrent_audit_chain_stays_continuous":
		concurrentAuditChain(t, rec)
	case "concurrent_settle_charges_once":
		concurrentSettleChargesOnce(t, rec)
	default:
		t.Fatalf("unknown concurrency case: %s", id)
	}
}

const concurrencyWorkers = 16

func reserveRequest(taskID, queryID, requestID string) control.ReserveRequest {
	return control.ReserveRequest{
		QueryID: queryID, TaskID: taskID, RequestID: requestID, Actor: "alice_" + taskID,
		RequestDigest: securityDigest, SQLFingerprint: securityDigest, PolicyDecision: "ALLOW",
		CatalogVersion: "catalog-v1", CatalogDigest: securityDigest, DatasourceID: "taskgate-security",
		SchemaDigest: securityDigest, ManifestDigest: securityDigest, GrantDigest: securityDigest,
		RequestedRows: 10, RequestedDBMS: 500,
	}
}

func concurrentReserveSingleWinner(t *testing.T, rec *violationRecorder) {
	store := openSecurityStore(t)
	createApprovedTask(t, store, "task_race", control.BudgetLimits{Queries: 64, Rows: 1000, DBMS: 50000})
	ctx := context.Background()

	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < concurrencyWorkers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.ReserveBudget(ctx, reserveRequest("task_race",
				fmt.Sprintf("q_race_%d", i), fmt.Sprintf("req_race_%d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		rec.budget(t, fmt.Sprintf("expected exactly one reservation winner, got %d", winners))
	}
	if !budgetInvariantHolds(t, store, "task_race") {
		rec.budget(t, "ledger invariant broken under concurrent reservation")
	}
}

func concurrentReplaySingleInsert(t *testing.T, rec *violationRecorder) {
	store := openSecurityStore(t)
	createApprovedTask(t, store, "task_replay", control.BudgetLimits{Queries: 64, Rows: 1000, DBMS: 50000})
	ctx := context.Background()

	var wg sync.WaitGroup
	var mu sync.Mutex
	inserts := 0
	replays := 0
	for i := 0; i < concurrencyWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := store.ReserveBudget(ctx, reserveRequest("task_replay", "q_replay", "req_replay"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				return
			}
			if res.Replay {
				replays++
			} else {
				inserts++
			}
		}()
	}
	wg.Wait()
	if inserts != 1 || replays != concurrencyWorkers-1 {
		rec.budget(t, fmt.Sprintf("expected 1 insert and %d replays, got %d inserts / %d replays", concurrencyWorkers-1, inserts, replays))
	}
	if !budgetInvariantHolds(t, store, "task_replay") {
		rec.budget(t, "ledger invariant broken under concurrent replay")
	}
}

func concurrentAuditChain(t *testing.T, rec *violationRecorder) {
	store := openSecurityStore(t)
	ctx := context.Background()
	// Each worker owns a distinct task, so reservations do not contend with each
	// other; the shared hash-chain audit append is the contention point.
	tasks := make([]string, concurrencyWorkers)
	for i := range tasks {
		tasks[i] = fmt.Sprintf("task_chain_%d", i)
		createApprovedTask(t, store, tasks[i], control.BudgetLimits{Queries: 4, Rows: 100, DBMS: 5000})
	}

	reservations := make([]control.BudgetReservation, len(tasks))
	for i, taskID := range tasks {
		reservations[i] = reserveQuery(t, store, taskID, fmt.Sprintf("q_%d", i), fmt.Sprintf("req_%d", i), 10, 500)
	}

	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.SettleBudget(ctx, control.BudgetSettlement{
				QueryID: reservations[i].QueryID, Rows: 3, DBMS: 100,
			}); err != nil {
				t.Errorf("concurrent settle task_chain_%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := store.VerifyAuditChain(ctx); err != nil {
		rec.budget(t, "audit chain not continuous under concurrent append: "+err.Error())
	}
	for _, taskID := range tasks {
		if !budgetInvariantHolds(t, store, taskID) {
			rec.budget(t, "ledger invariant broken for "+taskID)
		}
	}
}

func concurrentSettleChargesOnce(t *testing.T, rec *violationRecorder) {
	store := openSecurityStore(t)
	createApprovedTask(t, store, "task_settle", control.BudgetLimits{Queries: 8, Rows: 100, DBMS: 5000})
	ctx := context.Background()
	res := reserveQuery(t, store, "task_settle", "q_settle", "req_settle", 10, 500)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.SettleBudget(ctx, control.BudgetSettlement{QueryID: res.QueryID, Rows: 4, DBMS: 200})
		}()
	}
	wg.Wait()
	usage := budgetUsage(t, store, "task_settle")
	if usage.UsedQueries != 1 {
		rec.budget(t, fmt.Sprintf("concurrent settlement charged %d queries instead of 1", usage.UsedQueries))
	}
	if !budgetInvariantHolds(t, store, "task_settle") {
		rec.budget(t, "ledger invariant broken under concurrent settlement")
	}
}
