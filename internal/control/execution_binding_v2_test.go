package control

import (
	"context"
	"strings"
	"testing"

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

// testPairedNovelBindingV2 is the same execution testPairedNovelBinding
// describes, carried as a V2. Keeping them the same operation is what makes the
// version the only difference the store can be reacting to.
func testPairedNovelBindingV2(t *testing.T, queryID string) QueryExecutionBinding {
	t.Helper()
	v1 := testPairedNovelBinding(t, queryID)
	compiler := testCompilerIdentity(t)

	visible := v1.Binding.Visible
	visible.PolicyRendererVersion = compiler.PolicyRendererVersion
	visible.PolicyRendererDigest = compiler.PolicyRendererSHA256
	companion := *v1.Binding.Companion
	companion.PolicyRendererVersion = compiler.PolicyRendererVersion
	companion.PolicyRendererDigest = compiler.PolicyRendererSHA256

	sealed, err := querybinding.QueryExecutionBindingV2{
		PathKind:                   v1.Binding.PathKind,
		PreparedOperation:          testPreparedOperation(t),
		Compiler:                   compiler,
		ExposureProfileVersion:     v1.Binding.ExposureProfileVersion,
		VisibleRowLimit:            v1.Binding.VisibleRowLimit,
		CompanionEvidenceRows:      v1.Binding.CompanionEvidenceRows,
		CompanionPolicyRows:        v1.Binding.CompanionPolicyRows,
		BudgetBeforeSHA256:         v1.Binding.BudgetBeforeSHA256,
		ExposureLedgerBeforeSHA256: v1.Binding.ExposureLedgerBeforeSHA256,
		Visible:                    visible,
		Companion:                  &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal execution binding v2: %v", err)
	}
	return QueryExecutionBinding{
		QueryID: queryID, BindingV2: &sealed, ExposureLedgerBefore: v1.ExposureLedgerBefore,
	}
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
	original := testPairedNovelBindingV2(t, "query-v2-roundtrip")
	if err := writeBinding(t, store, original); err != nil {
		t.Fatalf("write V2 binding: %v", err)
	}
	reloaded, err := store.GetQueryExecutionBinding(context.Background(), "query-v2-roundtrip")
	if err != nil {
		t.Fatalf("reload V2 binding: %v", err)
	}
	if reloaded.Binding != nil {
		t.Fatal("a V2 row reloaded as a V1 document")
	}
	if reloaded.BindingV2 == nil {
		t.Fatal("a V2 row reloaded with no document")
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
		if err := writeBinding(t, store, testPairedNovelBindingV2(t, "query-v2-first")); err != nil {
			t.Fatalf("the first V2 write failed: %v", err)
		}
	})

	t.Run("an identical canonical V2 is idempotent", func(t *testing.T) {
		store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 122))
		seedQueryRecord(t, store, "task-v2-retry", "query-v2-retry")
		binding := testPairedNovelBindingV2(t, "query-v2-retry")
		if err := writeBinding(t, store, binding); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if err := writeBinding(t, store, testPairedNovelBindingV2(t, "query-v2-retry")); err != nil {
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
				first := testPairedNovelBindingV2(t, "query-v2-conflict")
				if err := writeBinding(t, store, first); err != nil {
					t.Fatalf("first write: %v", err)
				}
				second := testPairedNovelBindingV2(t, "query-v2-conflict")
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

// A query has one execution, described under one contract. Writing the other
// version's document for a query that already has one is not a retry.
func TestOneQueryCannotCarryBothBindingVersions(t *testing.T) {
	for name, order := range map[string][2]func(*testing.T, string) QueryExecutionBinding{
		"V1 first, then V2": {testPairedNovelBinding, testPairedNovelBindingV2},
		"V2 first, then V1": {testPairedNovelBindingV2, testPairedNovelBinding},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 124))
			seedQueryRecord(t, store, "task-v2-mixed", "query-v2-mixed")
			if err := writeBinding(t, store, order[0](t, "query-v2-mixed")); err != nil {
				t.Fatalf("first write: %v", err)
			}
			err := writeBinding(t, store, order[1](t, "query-v2-mixed"))
			if err == nil {
				t.Fatal("a query was allowed to carry both binding versions")
			}
			if !strings.Contains(err.Error(), "binding_version") {
				t.Fatalf("the conflict did not name the version: %v", err)
			}
		})
	}
}

// A binding carrying two documents, or none, is not a state the store may
// persist. The type permits it; Validate does not.
func TestExecutionBindingRequiresExactlyOneDocument(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 125))
	seedQueryRecord(t, store, "task-v2-shape", "query-v2-shape")

	both := testPairedNovelBindingV2(t, "query-v2-shape")
	both.Binding = testPairedNovelBinding(t, "query-v2-shape").Binding
	if err := writeBinding(t, store, both); err == nil {
		t.Fatal("a binding carrying both documents was written")
	}

	neither := QueryExecutionBinding{
		QueryID: "query-v2-shape", ExposureLedgerBefore: testExposureLedgerBefore(t),
	}
	if err := writeBinding(t, store, neither); err == nil {
		t.Fatal("a binding carrying no document was written")
	}
}

// A row's version column selects the decoder. A V2 document under a V1 version
// column, or the reverse, must be refused rather than decoded into whichever
// type happens to accept it.
func TestStoredExecutionBindingDecodesOnlyAsItsRecordedVersion(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]struct {
		write   func(*testing.T, string) QueryExecutionBinding
		claimed string
	}{
		"a V2 document relabelled as V1": {testPairedNovelBindingV2, querybinding.QueryExecutionBindingV1Version},
		"a V1 document relabelled as V2": {testPairedNovelBinding, querybinding.QueryExecutionBindingV2Version},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 126))
			seedQueryRecord(t, store, "task-v2-relabel", "query-v2-relabel")
			if err := writeBinding(t, store, setup.write(t, "query-v2-relabel")); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Relabel the row behind the store's back. This is the tampering the
			// redundant column exists to catch, and it is only reachable by writing
			// SQL directly because the table is immutable by trigger; the trigger is
			// dropped for the duration and restored afterwards.
			relabelBindingVersion(t, store, "query-v2-relabel", setup.claimed)
			if _, err := store.GetQueryExecutionBinding(ctx, "query-v2-relabel"); err == nil {
				t.Fatal("a relabelled execution binding row was read")
			}
		})
	}
}

func relabelBindingVersion(t *testing.T, store *Store, queryID, version string) {
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
	if _, err := store.db.ExecContext(ctx,
		`UPDATE query_execution_bindings SET binding_version=$1 WHERE query_id=$2`, version, queryID); err != nil {
		t.Fatalf("relabel binding version: %v", err)
	}
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
		testPairedNovelBindingV2(t, "query-v2-rollback")); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}
	rollback(tx)
	if _, err := store.GetQueryExecutionBinding(ctx, "query-v2-rollback"); err == nil {
		t.Fatal("a rolled-back V2 binding is readable")
	}
}
