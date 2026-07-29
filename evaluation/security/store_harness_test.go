package security

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// securityDigest is a fixed 64-hex placeholder for digest fields that the
// control store validates as well-formed SHA-256 during reservation. The
// end-to-end security experiments exercise budget/recovery/connector
// enforcement, not signature verification, so a stable placeholder suffices.
const securityDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// violationRecorder is the per-experiment breach counter. Each experiment owns
// its own recorder (a closed-over local int) so counts stay isolated even though
// run-corpus.sh runs every security test in a single process. A breach both
// increments the count and fails the subtest, so a passing run implies zero.
type violationRecorder struct {
	count int
}

func (r *violationRecorder) budget(t *testing.T, reason string) {
	t.Helper()
	r.count++
	t.Fatalf("budget violation observed: %s", reason)
}

func (r *violationRecorder) crossing(t *testing.T, reason string) {
	t.Helper()
	r.count++
	t.Fatalf("unauthorized crossing observed: %s", reason)
}

// securitySchemaDSN creates one isolated, migrated schema for a test and returns
// its DSN. Crash-recovery experiments call this once and then control.Open the
// same DSN multiple times so the second open observes the persisted state.
func securitySchemaDSN(t *testing.T) string {
	t.Helper()
	return testpostgres.SchemaDSN(t)
}

// openSecurityStore opens an isolated, migrated control store against the
// shared test Postgres. The test is skipped when no DSN is configured, matching
// the rest of the database-backed test suite.
func openSecurityStore(t *testing.T, options ...control.Option) *control.Store {
	t.Helper()
	return openSecurityStoreOnDSN(t, securitySchemaDSN(t), options...)
}

// openSecurityStoreOnDSN opens (and migrates) a control store on an existing
// isolated-schema DSN. Startup recovery runs by default, which is exactly what
// the crash-recovery experiments rely on.
func openSecurityStoreOnDSN(t *testing.T, dsn string, options ...control.Option) *control.Store {
	t.Helper()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x07}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCM: %v", err)
	}
	store, err := control.Open(context.Background(), dsn, cipher, options...)
	if err != nil {
		t.Fatalf("control.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// createApprovedTask stands up a principal, an ACTIVE task, and a signed grant
// with the given budget limits, mirroring the gateway approval path.
func createApprovedTask(t *testing.T, store *control.Store, taskID string, limits control.BudgetLimits) {
	t.Helper()
	ctx := context.Background()
	principalID := "principal_" + taskID
	if err := store.CreatePrincipal(ctx, control.Principal{ID: principalID, Subject: "alice_" + taskID, Role: "requester"}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	if err := store.CreateTask(ctx, control.Task{
		ID: taskID, PrincipalID: principalID, Objective: "security experiment", State: control.TaskAwaitingApproval,
		CatalogVersion: "catalog-v1",
		RequestContext: []byte(`{"products":["expense_summary"],"scope":{"department":"sales"}}`),
		ExpiresAt:      &expires,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	callback := control.ApprovalCallback{
		EventID:       "oa_" + taskID,
		RawPayload:    []byte(`{"decision":"approved"}`),
		ExpectedState: control.TaskAwaitingApproval,
		NewState:      control.TaskActive,
		Response:      []byte(`{"ok":true}`),
		Event:         control.ApprovalEvent{TaskID: taskID, Actor: "approver", Decision: "approved", Payload: []byte(`{}`)},
		Grant: &control.TaskGrant{
			TaskID: taskID, Subject: "alice_" + taskID, Purpose: "security experiment",
			ApprovedProducts: []string{"expense_summary"},
			ApprovedColumns:  map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope:   []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget: limits, ExpiresAt: expires, CatalogVersion: "catalog-v1", CatalogDigest: securityDigest,
			DatasourceID: "taskgate-security", SchemaDigest: securityDigest, ApprovalReceipt: "receipt_" + taskID,
		},
	}
	claim, err := store.ApplyApprovalCallback(ctx, callback)
	if err != nil {
		t.Fatalf("ApplyApprovalCallback: %v", err)
	}
	if !claim.Claimed || claim.Status != control.CallbackCompleted {
		t.Fatalf("approval callback not completed: %+v", claim)
	}
}

// reserveQuery performs a budget reservation for a single in-flight query. The
// single-in-flight invariant means a task has at most one RESERVED query at a
// time; tests settle or recover before reserving the next one.
func reserveQuery(t *testing.T, store *control.Store, taskID, queryID, requestID string, rows, dbms int64) control.BudgetReservation {
	t.Helper()
	reservation, err := store.ReserveBudget(context.Background(), control.ReserveRequest{
		QueryID: queryID, TaskID: taskID, RequestID: requestID, Actor: "alice_" + taskID,
		RequestDigest: securityDigest, SQLFingerprint: securityDigest, PolicyDecision: "ALLOW",
		CatalogVersion: "catalog-v1", CatalogDigest: securityDigest, DatasourceID: "taskgate-security",
		SchemaDigest: securityDigest, ManifestDigest: securityDigest, GrantDigest: securityDigest,
		RequestedRows: rows, RequestedDBMS: dbms,
	})
	if err != nil {
		t.Fatalf("ReserveBudget: %v", err)
	}
	if reservation.Replay {
		t.Fatalf("ReserveBudget returned a replay for a fresh request_id")
	}
	return reservation
}

func budgetUsage(t *testing.T, store *control.Store, taskID string) control.BudgetUsage {
	t.Helper()
	snapshot, err := store.GetBudget(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	return snapshot.Usage
}

// budgetInvariantHolds reports whether used+reserved stays within the limit on
// every axis — the core ledger property the budget-fault, concurrency, and
// crash-recovery experiments must preserve. It does not fail the test; callers
// route a false result through their violationRecorder.
func budgetInvariantHolds(t *testing.T, store *control.Store, taskID string) bool {
	t.Helper()
	snapshot, err := store.GetBudget(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	u := snapshot.Usage
	return u.UsedQueries+u.ReservedQueries <= snapshot.Limits.Queries &&
		u.UsedRows+u.ReservedRows <= snapshot.Limits.Rows &&
		u.UsedDBMS+u.ReservedDBMS <= snapshot.Limits.DBMS
}

// writeExperimentSummary persists the measured violation count for an
// experiment so verify.py can report an integer (not None). When
// SECURITY_SUMMARY_DIR is unset (local runs), the assertion-only path still
// fails the test on any breach; the summary is best-effort.
func writeExperimentSummary(t *testing.T, name string, payload map[string]any) {
	t.Helper()
	dir := os.Getenv("SECURITY_SUMMARY_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir summary dir: %v", err)
	}
	payload["schema_version"] = 1
	payload["experiment"] = name
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write summary: %v", err)
	}
}

// securityCase is the uniform corpus entry every end-to-end experiment uses.
// verify.py enumerates the expected subtest set from these IDs (mirroring the
// attack-corpus contract), so the IDs must stay stable and unique.
type securityCase struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

type securityCorpus struct {
	SchemaVersion int            `json:"schema_version"`
	Experiment    string         `json:"experiment"`
	Cases         []securityCase `json:"cases"`
}

// loadSecurityCorpus reads and validates an experiment corpus from the package
// directory. Go runs each package's tests with the package directory as the
// working directory, so a bare filename resolves correctly.
func loadSecurityCorpus(t *testing.T, name string) securityCorpus {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read corpus %s: %v", name, err)
	}
	var corpus securityCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil || corpus.SchemaVersion != 1 || len(corpus.Cases) == 0 {
		t.Fatalf("invalid corpus %s: schema_version=1 and a non-empty cases array are required (%v)", name, err)
	}
	seen := make(map[string]bool, len(corpus.Cases))
	for _, c := range corpus.Cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("invalid or duplicate case id in %s: %q", name, c.ID)
		}
		seen[c.ID] = true
	}
	return corpus
}
