package control

import (
	"context"
	"fmt"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func testCompilerIdentity(t *testing.T) preparedbinding.CompilerIdentityV1 {
	t.Helper()
	sealed, err := preparedbinding.CompilerIdentityV1{
		QueryPlanVersion: "queryplan-v7", QueryPlanSHA256: bindingDigest("c2"),
		PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererSHA256: bindingDigest("a3"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal compiler identity: %v", err)
	}
	return sealed
}

func testPreparedOperation(t *testing.T) preparedbinding.PreparedOperationBindingV1 {
	t.Helper()
	sealed, err := preparedbinding.PreparedOperationBindingV1{
		HasCompanion: true, Grouped: true, ExpandedEvidence: true,
		VisibleFieldCount: 4, FactFieldCount: 2, ProvenanceFieldCount: 3,
		VisibleFieldsSHA256:      bindingDigest("11"),
		FactFieldsSHA256:         bindingDigest("12"),
		ProvenanceFieldsSHA256:   bindingDigest("13"),
		PreparationInputsSHA256:  bindingDigest("14"),
		GrantSHA256:              bindingDigest("15"),
		CatalogSHA256:            bindingDigest("16"),
		SnapshotBindingSetSHA256: bindingDigest("17"),
		PlanSHA256:               bindingDigest("18"),
		CompilerIdentitySHA256:   testCompilerIdentity(t).SHA256,
		PolicyGrantSHA256:        bindingDigest("19"),
		NormalFormSHA256:         bindingDigest("1a"),
		OrdinalProgramSHA256:     bindingDigest("1b"),
		DictionarySetSHA256:      bindingDigest("1c"),
		SourcePublicationsSHA256: bindingDigest("1e"),
		PredicateFootprintSHA256: bindingDigest("26"),
		EstimatedBaseFacts:       4096,
		VisibleTargetSHA256:      bindingDigest("a4"),
		CompanionTargetSHA256:    bindingDigest("b4"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal prepared operation binding: %v", err)
	}
	return sealed
}

func resealBindingV2(t *testing.T, document querybinding.QueryExecutionBindingV2) *querybinding.QueryExecutionBindingV2 {
	t.Helper()
	sealed, err := document.Seal()
	if err != nil {
		t.Fatalf("reseal execution binding v2: %v", err)
	}
	return &sealed
}

func TestQueryExecutionBindingV2RoundTripsWithoutLoss(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 120))
	seedQueryRecord(t, store, "task-v2-roundtrip", "query-v2-roundtrip")
	original := testPairedNovelBinding(t, "query-v2-roundtrip")
	if err := writeBinding(t, store, original); err != nil {
		t.Fatalf("write V2 binding: %v", err)
	}
	reloaded, err := store.GetQueryExecutionBinding(context.Background(), "query-v2-roundtrip")
	if err != nil {
		t.Fatalf("reload V2 binding: %v", err)
	}
	if reloaded.BindingV2 == nil {
		t.Fatal("a stored row reloaded with no document")
	}
	if !reloaded.BindingV2.Equal(*original.BindingV2) {
		t.Fatal("the reloaded V2 binding is not the one that was written")
	}
	if reloaded.Version() != querybinding.QueryExecutionBindingV2Version {
		t.Fatalf("the reloaded row reports version %q", reloaded.Version())
	}
	// The whole preparation must survive, not merely its digest. A store that
	// dropped a member and re-sealed would produce a coherent document describing
	// a different preparation.
	if err := reloaded.BindingV2.PreparedOperation.RequireSame(original.BindingV2.PreparedOperation); err != nil {
		t.Fatalf("the reloaded preparation is not the one that was written: %v", err)
	}
}

// The four persistence rules, stated one per subtest.
func TestQueryExecutionBindingV2WriteRules(t *testing.T) {
	t.Run("first write succeeds", func(t *testing.T) {
		store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 121))
		seedQueryRecord(t, store, "task-v2-first", "query-v2-first")
		if err := writeBinding(t, store, testPairedNovelBinding(t, "query-v2-first")); err != nil {
			t.Fatalf("the first V2 write failed: %v", err)
		}
	})

	t.Run("an identical canonical V2 is idempotent", func(t *testing.T) {
		store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 122))
		seedQueryRecord(t, store, "task-v2-retry", "query-v2-retry")
		binding := testPairedNovelBinding(t, "query-v2-retry")
		if err := writeBinding(t, store, binding); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if err := writeBinding(t, store, testPairedNovelBinding(t, "query-v2-retry")); err != nil {
			t.Fatalf("an identical V2 retry was refused: %v", err)
		}
		stored, err := store.GetQueryExecutionBinding(context.Background(), "query-v2-retry")
		if err != nil {
			t.Fatal(err)
		}
		if stored.SHA256() != binding.SHA256() {
			t.Fatal("a retry replaced the recorded V2 binding")
		}
	})

	t.Run("a different V2 fails closed", func(t *testing.T) {
		for name, mutate := range map[string]func(*testing.T, *QueryExecutionBinding){
			"a different prepared operation": func(t *testing.T, binding *QueryExecutionBinding) {
				document := *binding.BindingV2
				prepared := document.PreparedOperation
				prepared.PolicyGrantSHA256 = bindingDigest("f1")
				resealed, err := prepared.Seal()
				if err != nil {
					t.Fatal(err)
				}
				document.PreparedOperation = resealed
				binding.BindingV2 = resealBindingV2(t, document)
			},
			"a different visible statement": func(t *testing.T, binding *QueryExecutionBinding) {
				document := *binding.BindingV2
				document.Visible.ExactSQLSHA256 = bindingDigest("f2")
				binding.BindingV2 = resealBindingV2(t, document)
			},
			"a different path kind": func(t *testing.T, binding *QueryExecutionBinding) {
				document := *binding.BindingV2
				document.PathKind = querybinding.PathSemanticReplay
				document.Visible.Executed = false
				companion := *document.Companion
				companion.Executed = false
				document.Companion = &companion
				binding.BindingV2 = resealBindingV2(t, document)
			},
			// The outer digest is a column, not a signature.
			"a different document carrying the original outer digest": func(t *testing.T, binding *QueryExecutionBinding) {
				original := binding.BindingV2.SHA256
				document := *binding.BindingV2
				document.VisibleRowLimit = 9
				document.Visible.RowLimit = 9
				binding.BindingV2 = resealBindingV2(t, document)
				binding.BindingV2.SHA256 = original
			},
		} {
			t.Run(name, func(t *testing.T) {
				store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 123))
				seedQueryRecord(t, store, "task-v2-conflict", "query-v2-conflict")
				first := testPairedNovelBinding(t, "query-v2-conflict")
				if err := writeBinding(t, store, first); err != nil {
					t.Fatalf("first write: %v", err)
				}
				second := testPairedNovelBinding(t, "query-v2-conflict")
				mutate(t, &second)
				err := writeBinding(t, store, second)
				if err == nil {
					t.Fatalf("%s was accepted as the same binding", name)
				}
				stored, getErr := store.GetQueryExecutionBinding(context.Background(), "query-v2-conflict")
				if getErr != nil {
					t.Fatal(getErr)
				}
				if stored.SHA256() != first.SHA256() {
					t.Fatal("the conflicting write replaced the recorded binding")
				}
			})
		}
	})
}

// A binding carrying no document is not a state the store may persist. The type
// permits it; Validate does not.
func TestExecutionBindingRequiresItsDocument(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 125))
	seedQueryRecord(t, store, "task-v2-shape", "query-v2-shape")

	neither := QueryExecutionBinding{
		QueryID: "query-v2-shape", ExposureLedgerBefore: testExposureLedgerBefore(t),
	}
	if err := writeBinding(t, store, neither); err == nil {
		t.Fatal("a binding carrying no document was written")
	}
}

// A row's version column is checked, not assumed -- in the schema and again in
// the decoder.
//
// The schema is the first line: the CHECK constraint accepts exactly the version
// this build writes, so the tampering below cannot even be stored. The decoder
// is the second, tested directly beneath, because a constraint is a property of
// the database a build happens to be pointed at and the decoder must not depend
// on having been pointed at the right one.
func TestStoredExecutionBindingVersionCannotBeRelabelled(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 126))
	seedQueryRecord(t, store, "task-v2-relabel", "query-v2-relabel")
	if err := writeBinding(t, store, testPairedNovelBinding(t, "query-v2-relabel")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, claimed := range []string{"taskgate-query-execution-binding-v1", "taskgate-query-execution-binding-v3", ""} {
		t.Run("relabelled as "+claimed, func(t *testing.T) {
			// Relabel the row behind the store's back. This is only reachable by
			// writing SQL directly because the table is immutable by trigger; the
			// trigger is dropped for the duration and restored afterwards.
			if err := relabelBindingVersion(t, store, "query-v2-relabel", claimed); err == nil {
				t.Fatal("the schema accepted an execution binding version this build cannot write")
			}
		})
	}
}

// And the decoder refuses it on its own, without help from the schema.
//
// The row is supplied directly rather than through the database, because the
// point is exactly the case the constraint above has already made unreachable
// there: a build pointed at a database whose schema does not agree with it.
func TestTheDecoderRefusesAnExecutionBindingVersionItCannotRead(t *testing.T) {
	binding := testPairedNovelBinding(t, "query-decoder-version")
	document, ledger, err := binding.canonicalDocuments()
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	for _, claimed := range []string{"taskgate-query-execution-binding-v1", "taskgate-query-execution-binding-v3", ""} {
		t.Run("row naming "+claimed, func(t *testing.T) {
			row := storedBindingRow{
				queryID: binding.QueryID, version: claimed,
				bindingJSON: document, bindingSHA256: binding.SHA256(),
				ledgerJSON: ledger, ledgerSHA256: binding.ExposureLedgerBefore.SHA256,
				pathKind: string(binding.PathKind()), createdAt: time.Now().UTC(),
			}
			if _, _, _, err := scanStoredExecutionBinding(row); err == nil {
				t.Fatal("the decoder read a row naming a version this build cannot read")
			}
		})
	}
}

// storedBindingRow is one query_execution_bindings row, supplied to the decoder
// without a database.
type storedBindingRow struct {
	queryID, version       string
	bindingJSON            []byte
	bindingSHA256          string
	ledgerJSON             []byte
	ledgerSHA256, pathKind string
	createdAt              time.Time
}

func (row storedBindingRow) Scan(dest ...any) error {
	values := []any{row.queryID, row.version, row.bindingJSON, row.bindingSHA256,
		row.ledgerJSON, row.ledgerSHA256, row.pathKind, row.createdAt}
	if len(dest) != len(values) {
		return fmt.Errorf("the decoder scanned %d columns; the row has %d", len(dest), len(values))
	}
	for index, value := range values {
		switch target := dest[index].(type) {
		case *string:
			*target = value.(string)
		case *[]byte:
			*target = value.([]byte)
		case *time.Time:
			*target = value.(time.Time)
		default:
			return fmt.Errorf("column %d has unexpected destination %T", index, dest[index])
		}
	}
	return nil
}

func relabelBindingVersion(t *testing.T, store *Store, queryID, version string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx,
		`ALTER TABLE query_execution_bindings DISABLE TRIGGER query_execution_bindings_no_update`); err != nil {
		t.Fatalf("disable immutability trigger: %v", err)
	}
	defer func() {
		if _, err := store.db.ExecContext(ctx,
			`ALTER TABLE query_execution_bindings ENABLE TRIGGER query_execution_bindings_no_update`); err != nil {
			t.Fatalf("restore immutability trigger: %v", err)
		}
	}()
	_, err := store.db.ExecContext(ctx,
		`UPDATE query_execution_bindings SET binding_version=$1 WHERE query_id=$2`, version, queryID)
	return err
}

// A rolled-back settlement leaves no V2 row, exactly as it leaves no V1 row.
func TestRolledBackTransactionLeavesNoV2ExecutionBinding(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 127))
	seedQueryRecord(t, store, "task-v2-rollback", "query-v2-rollback")
	ctx := context.Background()
	tx, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := putQueryExecutionBindingTx(ctx, tx, store.now(),
		testPairedNovelBinding(t, "query-v2-rollback")); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}
	rollback(tx)
	if _, err := store.GetQueryExecutionBinding(ctx, "query-v2-rollback"); err == nil {
		t.Fatal("a rolled-back V2 binding is readable")
	}
}
