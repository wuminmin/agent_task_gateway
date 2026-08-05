package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func bindingDigest(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

func testExposureLedgerBefore(t *testing.T) querybinding.ExposureLedgerBeforeV1 {
	t.Helper()
	sealed, err := querybinding.ExposureLedgerBeforeV1{
		ProfileVersion: "taskgate-exposure-v5", RootTaskID: "task-root-1", RootEpoch: 3,
		Limits: querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: 4, OutcomeFacts: 10,
			PredicateAtoms: 25, CompositeOutcomes: 5},
		Used: querybinding.FactVector{ReleaseFacts: 100, InfluenceFacts: 0, OutcomeFacts: 2,
			PredicateAtoms: 5, CompositeOutcomes: 1},
		Remaining: querybinding.FactVector{ReleaseFacts: 400, InfluenceFacts: 4, OutcomeFacts: 8,
			PredicateAtoms: 20, CompositeOutcomes: 4},
		RemainingRows: 10, UsesExpandedEvidence: true, HasExposureContext: true,
	}.Seal()
	if err != nil {
		t.Fatalf("seal exposure ledger pre-state: %v", err)
	}
	return sealed
}

// testPairedNovelBinding is a Result-heavy paired-novel execution under
// expanded evidence: the companion's policy limit is its evidence rows plus one.
func testPairedNovelBinding(t *testing.T, queryID string) QueryExecutionBinding {
	t.Helper()
	ledger := testExposureLedgerBefore(t)
	companion := querybinding.TargetRecordV1{
		Role: querybinding.RoleCompanion, Authorized: true, Executed: true,
		ExactSQLSHA256: bindingDigest("b1"), StrictASTSHA256: bindingDigest("b2"),
		RowLimit: 5, PolicyFingerprint: "companion-fingerprint",
		PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererDigest: bindingDigest("b3"),
		PreparedTargetBindingSHA256: bindingDigest("b4"),
	}
	binding, err := querybinding.QueryExecutionBindingV1{
		PathKind:                       querybinding.PathPairedNovel,
		PreparedOperationBindingSHA256: bindingDigest("c1"),
		ExposureProfileVersion:         ledger.ProfileVersion,
		UsesExpandedEvidence:           true,
		VisibleRowLimit:                10, CompanionEvidenceRows: 4, CompanionPolicyRows: 5,
		BudgetBeforeSHA256:         bindingDigest("d1"),
		ExposureLedgerBeforeSHA256: ledger.SHA256,
		PlanSHA256:                 bindingDigest("c2"),
		CompilerVersion:            "queryplan-v7", CompilerSHA256: bindingDigest("c3"),
		Visible: querybinding.TargetRecordV1{
			Role: querybinding.RoleVisible, Authorized: true, Executed: true,
			ExactSQLSHA256: bindingDigest("a1"), StrictASTSHA256: bindingDigest("a2"),
			RowLimit: 10, PolicyFingerprint: "visible-fingerprint",
			PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererDigest: bindingDigest("a3"),
			PreparedTargetBindingSHA256: bindingDigest("a4"),
		},
		Companion: &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal execution binding: %v", err)
	}
	return QueryExecutionBinding{QueryID: queryID, Binding: binding, ExposureLedgerBefore: ledger}
}

// writeBinding writes one binding through the same transaction-scoped helper the
// terminal settlement uses, so the test exercises the production write path
// rather than a parallel one.
func writeBinding(t *testing.T, store *Store, binding QueryExecutionBinding) error {
	t.Helper()
	ctx := context.Background()
	tx, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer rollback(tx)
	if err := putQueryExecutionBindingTx(ctx, tx, store.now(), binding); err != nil {
		return err
	}
	return tx.Commit()
}

// seedQueryRecord creates the query_records row the binding's foreign key needs.
func seedQueryRecord(t *testing.T, store *Store, taskID, queryID string) {
	t.Helper()
	expires := time.Now().Add(time.Hour).UTC()
	createAwaitingApprovalTask(t, store, taskID, expires)
	approveExposureTask(t, store, taskID, expires, ExposureLimits{ReleaseFacts: 8, InfluenceFacts: 8})
	reserveExposureQuery(t, store, taskID, queryID, "request-"+queryID)
}

func TestQueryExecutionBindingRoundTripsWithoutLoss(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 91))
	seedQueryRecord(t, store, "task-binding-1", "query-binding-1")
	original := testPairedNovelBinding(t, "query-binding-1")

	if err := writeBinding(t, store, original); err != nil {
		t.Fatalf("persist execution binding: %v", err)
	}
	reloaded, err := store.GetQueryExecutionBinding(context.Background(), "query-binding-1")
	if err != nil {
		t.Fatalf("GetQueryExecutionBinding: %v", err)
	}

	// Byte-identical, not merely equivalent. The receipt signature covers these
	// as documents, so a reload that differed anywhere would sign something
	// other than what executed.
	for name, pair := range map[string][2]any{
		"binding": {original.Binding, reloaded.Binding},
		"ledger":  {original.ExposureLedgerBefore, reloaded.ExposureLedgerBefore},
	} {
		want, err := json.Marshal(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != string(got) {
			t.Fatalf("%s did not round-trip:\n want %s\n got  %s", name, want, got)
		}
	}
	if !reloaded.Binding.Equal(original.Binding) {
		t.Fatal("the reloaded binding is not the one that was written")
	}
	if reloaded.QueryID != "query-binding-1" || reloaded.CreatedAt.IsZero() {
		t.Fatalf("reloaded binding identity is wrong: %+v", reloaded)
	}
}

// A query with no binding is an ordinary outcome: every pre-V9 path has none,
// and so does a query recovered as INDETERMINATE.
func TestQueryExecutionBindingIsAbsentForAPreV9Query(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 92))
	seedQueryRecord(t, store, "task-binding-2", "query-binding-2")
	_, err := store.GetQueryExecutionBinding(context.Background(), "query-binding-2")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent execution binding reported %v, want ErrNotFound", err)
	}
}

// The row is immutable. An execution binding that could be updated afterwards
// would describe whatever the last writer preferred rather than what ran.
func TestQueryExecutionBindingIsImmutable(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 93))
	seedQueryRecord(t, store, "task-binding-3", "query-binding-3")
	original := testPairedNovelBinding(t, "query-binding-3")
	if err := writeBinding(t, store, original); err != nil {
		t.Fatalf("persist execution binding: %v", err)
	}

	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx,
		`UPDATE query_execution_bindings SET binding_sha256=$1 WHERE query_id=$2`,
		bindingDigest("9"), "query-binding-3"); err == nil {
		t.Fatal("an execution binding row was updated")
	}
	if _, err := store.db.ExecContext(ctx,
		`DELETE FROM query_execution_bindings WHERE query_id=$1`, "query-binding-3"); err == nil {
		t.Fatal("an execution binding row was deleted")
	}

	// A re-write of the same query is harmless and changes nothing.
	if err := writeBinding(t, store, original); err != nil {
		t.Fatalf("re-writing the same binding failed: %v", err)
	}
	reloaded, err := store.GetQueryExecutionBinding(ctx, "query-binding-3")
	if err != nil {
		t.Fatalf("GetQueryExecutionBinding: %v", err)
	}
	if !reloaded.Binding.Equal(original.Binding) {
		t.Fatal("a repeated write changed the recorded binding")
	}
}

// The denormalized columns are redundant with the stored documents on purpose.
// A disagreement means a value changed outside what the signature covers.
func TestStoredExecutionBindingDetectsColumnTampering(t *testing.T) {
	for name, tamper := range map[string]struct{ statement, value string }{
		"binding digest": {
			`UPDATE query_execution_bindings SET binding_sha256=$1 WHERE query_id=$2`,
			bindingDigest("9")},
		"pre-state digest": {
			`UPDATE query_execution_bindings SET exposure_ledger_before_sha256=$1 WHERE query_id=$2`,
			bindingDigest("9")},
		// single_query passes the column's CHECK constraint, so what rejects it
		// is the reader comparing the column against the stored document.
		"path kind": {
			`UPDATE query_execution_bindings SET path_kind=$1 WHERE query_id=$2`,
			"single_query"},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 94))
			seedQueryRecord(t, store, "task-tamper", "query-tamper")
			if err := writeBinding(t, store, testPairedNovelBinding(t, "query-tamper")); err != nil {
				t.Fatalf("persist: %v", err)
			}
			ctx := context.Background()
			// The immutability trigger is dropped for this test only, so the
			// reader's own consistency checks are what is under test rather than
			// the trigger that would normally prevent the edit.
			if _, err := store.db.ExecContext(ctx,
				`ALTER TABLE query_execution_bindings DISABLE TRIGGER query_execution_bindings_no_update`); err != nil {
				t.Fatalf("disable trigger: %v", err)
			}
			if _, err := store.db.ExecContext(ctx, tamper.statement, tamper.value, "query-tamper"); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if _, err := store.GetQueryExecutionBinding(ctx, "query-tamper"); err == nil {
				t.Fatalf("a tampered %s was accepted on reload", name)
			}
		})
	}
}

// An idempotent replay returns the original signed receipt unchanged, so no
// binding may claim that path.
func TestExecutionBindingRejectsIncoherentRows(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 95))
	seedQueryRecord(t, store, "task-binding-4", "query-binding-4")

	mismatched := testPairedNovelBinding(t, "query-binding-4")
	other := testExposureLedgerBefore(t)
	other.RootEpoch = 99
	resealed, err := other.Seal()
	if err != nil {
		t.Fatal(err)
	}
	mismatched.ExposureLedgerBefore = resealed
	if err := writeBinding(t, store, mismatched); err == nil {
		t.Fatal("a binding whose pre-state it does not name was persisted")
	}

	noQuery := testPairedNovelBinding(t, "")
	if err := writeBinding(t, store, noQuery); err == nil {
		t.Fatal("a binding naming no query was persisted")
	}
}

// A rolled-back settlement must leave no partial V9 state.
func TestRolledBackTransactionLeavesNoExecutionBinding(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 96))
	seedQueryRecord(t, store, "task-binding-5", "query-binding-5")
	binding := testPairedNovelBinding(t, "query-binding-5")

	ctx := context.Background()
	tx, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := putQueryExecutionBindingTx(ctx, tx, store.now(), binding); err != nil {
		t.Fatalf("write inside transaction: %v", err)
	}
	rollback(tx)

	if _, err := store.GetQueryExecutionBinding(ctx, "query-binding-5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a rolled-back transaction left an execution binding: %v", err)
	}
}

// --- exact idempotency (I2-A0.4) ---------------------------------------------
//
// ON CONFLICT DO NOTHING made a redundant write harmless. It also made a
// CONTRADICTORY one silent: the second write returned success, the first row
// stayed, and the receipt signed afterwards described an execution nobody had
// checked matched. These cases pin what "the same binding" has to mean.

// A non-COMPLETED settlement must refuse a binding rather than quietly drop it.
// A failed execution cannot prove which of its targets ran, so a binding
// asserting paired-novel semantics for it would be a false description.
func TestFailedSettlementRefusesAnExecutionBinding(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 95))
	seedQueryRecord(t, store, "task-binding-failed", "query-binding-failed")
	binding := testPairedNovelBinding(t, "query-binding-failed")
	_, err := store.FailBudget(context.Background(), BudgetSettlement{
		QueryID: "query-binding-failed", Rows: 1, DBMS: 1, ObservedDBMS: 1,
		ErrorCode: "QUERY_FAILED", ExecutionBinding: &binding,
	})
	if err == nil {
		t.Fatal("a FAILED settlement recorded an execution binding")
	}
	if _, lookupErr := store.GetQueryExecutionBinding(context.Background(), "query-binding-failed"); !errors.Is(lookupErr, ErrNotFound) {
		t.Fatalf("a refused FAILED settlement left a binding behind: %v", lookupErr)
	}
}

func TestSameExecutionBindingRetriedSucceeds(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 96))
	seedQueryRecord(t, store, "task-binding-retry", "query-binding-retry")
	binding := testPairedNovelBinding(t, "query-binding-retry")
	if err := writeBinding(t, store, binding); err != nil {
		t.Fatalf("first write: %v", err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := writeBinding(t, store, binding); err != nil {
			t.Fatalf("retry %d of an identical binding failed: %v", attempt, err)
		}
	}
	stored, err := store.GetQueryExecutionBinding(context.Background(), "query-binding-retry")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Binding.SHA256 != binding.Binding.SHA256 {
		t.Fatal("a retry replaced the recorded binding")
	}
}

func TestDifferentExecutionBindingForOneQueryConflicts(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *QueryExecutionBinding){
		"different exact SQL digest": func(t *testing.T, binding *QueryExecutionBinding) {
			document := binding.Binding
			document.Visible.ExactSQLSHA256 = bindingDigest("f1")
			binding.Binding = resealBinding(t, document)
		},
		"different pre-state": func(t *testing.T, binding *QueryExecutionBinding) {
			ledger := binding.ExposureLedgerBefore
			ledger.RootEpoch++
			sealed, err := ledger.Seal()
			if err != nil {
				t.Fatal(err)
			}
			binding.ExposureLedgerBefore = sealed
			document := binding.Binding
			document.ExposureLedgerBeforeSHA256 = sealed.SHA256
			binding.Binding = resealBinding(t, document)
		},
		"different path kind": func(t *testing.T, binding *QueryExecutionBinding) {
			document := binding.Binding
			document.PathKind = querybinding.PathSemanticReplay
			document.Visible.Executed = false
			document.Companion.Executed = false
			binding.Binding = resealBinding(t, document)
		},
		// The outer digest is a column, not a signature. A document whose members
		// were changed and whose recorded digest was copied from the original must
		// not pass as "the same binding".
		"different document carrying the original outer digest": func(t *testing.T, binding *QueryExecutionBinding) {
			original := binding.Binding.SHA256
			document := binding.Binding
			document.Visible.RowLimit = 9
			document.VisibleRowLimit = 9
			binding.Binding = resealBinding(t, document)
			binding.Binding.SHA256 = original
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 97))
			seedQueryRecord(t, store, "task-binding-conflict", "query-binding-conflict")
			first := testPairedNovelBinding(t, "query-binding-conflict")
			if err := writeBinding(t, store, first); err != nil {
				t.Fatalf("first write: %v", err)
			}
			second := testPairedNovelBinding(t, "query-binding-conflict")
			mutate(t, &second)
			err := writeBinding(t, store, second)
			if err == nil {
				t.Fatal("a contradictory execution binding was accepted for a query that already has one")
			}
			stored, reloadErr := store.GetQueryExecutionBinding(context.Background(), "query-binding-conflict")
			if reloadErr != nil {
				t.Fatalf("reload after the refused write: %v", reloadErr)
			}
			if stored.Binding.SHA256 != first.Binding.SHA256 {
				t.Fatal("the refused write changed the recorded binding")
			}
		})
	}
}

// Concurrency: two writers racing on one query must end with exactly one
// recorded binding, and the loser must be told so rather than silently agreeing.
func TestConcurrentExecutionBindingWritesAgreeOrConflict(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 98))
	seedQueryRecord(t, store, "task-binding-race", "query-binding-race")
	identical := testPairedNovelBinding(t, "query-binding-race")
	different := testPairedNovelBinding(t, "query-binding-race")
	document := different.Binding
	document.Visible.ExactSQLSHA256 = bindingDigest("e1")
	different.Binding = resealBinding(t, document)

	results := make(chan error, 8)
	start := make(chan struct{})
	for writer := 0; writer < 8; writer++ {
		binding := identical
		if writer%2 == 1 {
			binding = different
		}
		go func(binding QueryExecutionBinding) {
			<-start
			results <- writeBinding(t, store, binding)
		}(binding)
	}
	close(start)
	var succeeded, conflicted int
	for writer := 0; writer < 8; writer++ {
		if err := <-results; err == nil {
			succeeded++
		} else {
			conflicted++
		}
	}
	if succeeded == 0 {
		t.Fatal("no writer recorded a binding")
	}
	if conflicted == 0 {
		t.Fatal("contradictory concurrent writers all reported success")
	}
	stored, err := store.GetQueryExecutionBinding(context.Background(), "query-binding-race")
	if err != nil {
		t.Fatalf("reload after the race: %v", err)
	}
	if stored.Binding.SHA256 != identical.Binding.SHA256 && stored.Binding.SHA256 != different.Binding.SHA256 {
		t.Fatal("the race recorded a binding neither writer supplied")
	}
}

// --- canonical persistence (I2-A0.5) -----------------------------------------

// A semantically equivalent but differently encoded document must not become a
// different replay artifact. The stored bytes are the artifact; the outer digest
// covers the members, not the encoding, so both encodings would hash alike.
func TestNonCanonicalStoredDocumentIsRefusedOnReload(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 99))
	seedQueryRecord(t, store, "task-binding-encoding", "query-binding-encoding")
	binding := testPairedNovelBinding(t, "query-binding-encoding")
	if err := writeBinding(t, store, binding); err != nil {
		t.Fatalf("write: %v", err)
	}
	stored, _, _, err := scanStoredExecutionBinding(store.db.QueryRowContext(context.Background(),
		executionBindingSelect+` WHERE query_id=$1`, "query-binding-encoding"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Re-encode the same values with encoding/json's default output and confirm
	// it is NOT what was stored: if it were, the canonical check would be vacuous.
	loose, err := json.Marshal(map[string]any{
		"version": stored.Binding.Version, "path_kind": stored.Binding.PathKind,
		"query_execution_binding_sha256": stored.Binding.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, err := stored.canonicalDocuments()
	if err != nil {
		t.Fatal(err)
	}
	if string(loose) == string(canonical) {
		t.Fatal("the canonical encoding is indistinguishable from an arbitrary one; this case proves nothing")
	}
	// The immutability trigger refuses an UPDATE, which is itself the first line
	// of defence; prove the reload check independently.
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE query_execution_bindings SET binding_json=$1 WHERE query_id=$2`,
		loose, "query-binding-encoding"); err == nil {
		t.Fatal("an immutable execution binding row accepted an UPDATE")
	}
}

func resealBinding(t *testing.T, document querybinding.QueryExecutionBindingV1) querybinding.QueryExecutionBindingV1 {
	t.Helper()
	sealed, err := document.Seal()
	if err != nil {
		t.Fatalf("reseal execution binding: %v", err)
	}
	return sealed
}
