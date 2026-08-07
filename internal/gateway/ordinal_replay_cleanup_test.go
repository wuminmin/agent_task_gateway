package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

type ordinalReplayTestFixture struct {
	harness             *gatewayHarness
	task                control.Task
	grantDigest         string
	cacheKey            string
	dictionaryDigest    string
	manifestDigest      string
	dictionarySetDigest string
}

func newOrdinalReplayTestFixture(t *testing.T, taskID string, sourcePlaintext []byte) ordinalReplayTestFixture {
	t.Helper()
	harness := newGatewayHarness(t)
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil,
		control.ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10}, exposure.ProfileV4)

	product := compactOrdinalProduct()
	compilation, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: product.Name, Columns: []string{"amount"}}, product)
	if err != nil {
		t.Fatal(err)
	}
	dictionaryFixture := newOrdinalDerivationFixture(t, compilation.OrdinalProgram,
		[]map[string]any{{"id": int64(1), "amount": int64(10)}})
	manifestDigest := dictionaryFixture.artifact.Hot.ManifestDigest()
	if err := harness.store.PutOrdinalSnapshotPublication(context.Background(), manifestDigest,
		dictionaryFixture.artifact.Hot, nil); err != nil {
		t.Fatal(err)
	}
	dictionarySet, err := ordinal.NewDictionarySetManifest(harness.catalog.SHA256, ordinal.DictionarySetMember{
		PublicationName: "replay-cleanup-publication", DictionaryDigest: dictionaryFixture.artifact.Hot.DictionaryDigest(),
		ManifestDigest: manifestDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.store.PutOrdinalDictionarySet(context.Background(), dictionarySet); err != nil {
		t.Fatal(err)
	}
	dictionarySetDigest, err := dictionarySet.Digest()
	if err != nil {
		t.Fatal(err)
	}
	sourceRows := int64(1)
	if stored, decodeErr := decodeStoredQueryResult(sourcePlaintext); decodeErr == nil && stored.RowCount > 0 {
		sourceRows = stored.RowCount
	}
	outcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion,
		strings.Repeat("1", 64), strings.Repeat("2", 64), sourceRows)
	if err != nil {
		t.Fatal(err)
	}
	outcomeHash, err := outcome.Hash()
	if err != nil {
		t.Fatal(err)
	}
	outcomePayload, err := outcome.CanonicalPayload()
	if err != nil {
		t.Fatal(err)
	}
	observation := control.OrdinalExposureObservation{
		ProfileVersion: exposure.ProfileV4, DictionarySetDigest: dictionarySetDigest,
		Outcome: control.OrdinalHybridSet{DynamicFacts: []control.OrdinalDynamicFact{{
			SHA256: outcomeHash, Kind: control.OrdinalDynamicOutcome, CanonicalPayload: outcomePayload,
		}}},
	}
	fixture := ordinalReplayTestFixture{
		harness: harness, grantDigest: strings.Repeat("3", 64), cacheKey: strings.Repeat("5", 64),
		dictionaryDigest: dictionaryFixture.artifact.Hot.DictionaryDigest(), manifestDigest: manifestDigest,
		dictionarySetDigest: dictionarySetDigest,
	}
	source := fixture.reserveRows(t, taskID+"-source", taskID+"-source-request", sourceRows)
	expires := harness.clock.value.Add(time.Hour)
	if _, _, _, err := harness.store.FinalizeOrdinalQueryMeasuredWithReceipt(context.Background(), control.BudgetSettlement{
		QueryID: source.QueryID, Rows: sourceRows, DBMS: 1, OrdinalExposure: &observation,
	}, sourcePlaintext, &control.OrdinalMaterializationPublish{CacheKeySHA256: fixture.cacheKey, ExpiresAt: &expires}, nil); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func validOrdinalReplaySource(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(storedQueryResult{
		Columns: []dataconnector.Column{{Name: "amount", DataTypeOID: 20}},
		Rows:    [][]any{{int64(10)}}, RowCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func exactNumberOrdinalReplaySource() []byte {
	return []byte(`{"columns":[{"name":"scaled","data_type_oid":1700},{"name":"large","data_type_oid":20}],"rows":[[12.50,9007199254740993]],"row_count":1,"database_ms":7,"limited":false}`)
}

func twoRowOrdinalReplaySource(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(storedQueryResult{
		Columns:  []dataconnector.Column{{Name: "amount", DataTypeOID: 20}},
		Rows:     [][]any{{int64(10)}, {int64(20)}},
		RowCount: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func (fixture ordinalReplayTestFixture) reserve(t *testing.T, queryID, requestID string) control.BudgetReservation {
	return fixture.reserveRows(t, queryID, requestID, 1)
}

func (fixture ordinalReplayTestFixture) reserveRows(t *testing.T, queryID, requestID string,
	requestedRows int64) control.BudgetReservation {
	t.Helper()
	reservation, err := fixture.harness.store.ReserveBudget(context.Background(), control.ReserveRequest{
		QueryID: queryID, TaskID: fixtureTaskID(fixture, queryID), RequestID: requestID,
		Actor: fixture.harness.alice.Subject, RequestDigest: strings.Repeat("6", 64),
		SQLFingerprint: strings.Repeat("7", 64), CatalogVersion: fixture.harness.catalog.CatalogVersion,
		CatalogDigest:  fixture.harness.catalog.SHA256,
		DatasourceID:   fixture.harness.connector.attestation.DatasourceID,
		SchemaDigest:   fixture.harness.connector.attestation.SchemaDigest,
		ManifestDigest: strings.Repeat("4", 64), GrantDigest: fixture.grantDigest, PolicyDecision: "ALLOW",
		RequestedRows: requestedRows, RequestedDBMS: 10,
		Exposure: &control.ExposureReservationRequest{ProfileVersion: exposure.ProfileV4, EstimatedOutcomeFacts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func fixtureTaskID(fixture ordinalReplayTestFixture, queryID string) string {
	if fixture.task.ID != "" {
		return fixture.task.ID
	}
	return strings.TrimSuffix(queryID, "-source")
}

func assertReplayReservationStatus(t *testing.T, fixture ordinalReplayTestFixture, queryID string,
	want control.QueryStatus) control.QueryRecord {
	t.Helper()
	record, err := fixture.harness.store.GetQuery(context.Background(), queryID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != want {
		t.Fatalf("query %s status = %s, want %s", queryID, record.Status, want)
	}
	budget, err := fixture.harness.store.GetBudget(context.Background(), record.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if want != control.QueryReserved && (budget.Usage.ReservedQueries != 0 || budget.Usage.ReservedRows != 0 ||
		budget.Usage.ReservedDBMS != 0) {
		t.Fatalf("terminal replay left reservation: %+v", budget.Usage)
	}
	return record
}

func TestOrdinalSemanticReplayNormalMissKeepsReservationForNovelPath(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-normal-miss", validOrdinalReplaySource(t))
	reservation := fixture.reserve(t, "query-v4-replay-normal-miss", "request-v4-replay-normal-miss")
	missingKey := strings.Repeat("8", 64)

	result, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, missingKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, nil)
	if err != nil || outcome != ordinalReplayContinueNovel || result != nil {
		t.Fatalf("normal miss result=%#v outcome=%v err=%v", result, outcome, err)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReserved)
	if _, err := fixture.harness.store.ReleaseBudget(context.Background(), reservation.QueryID, "TEST_CLEANUP"); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinalSemanticReplayRejectsChangedPublicationBindingEvenWithReusedCacheKey(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-publication-change", validOrdinalReplaySource(t))
	changedSet, err := ordinal.NewDictionarySetManifest(fixture.harness.catalog.SHA256, ordinal.DictionarySetMember{
		PublicationName: "replay-cleanup-publication-v2", DictionaryDigest: fixture.dictionaryDigest,
		ManifestDigest: fixture.manifestDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.harness.store.PutOrdinalDictionarySet(context.Background(), changedSet); err != nil {
		t.Fatal(err)
	}
	changedDigest, err := changedSet.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == fixture.dictionarySetDigest {
		t.Fatal("publication identity change did not partition the dictionary-set binding")
	}

	reservation := fixture.reserve(t, "query-v4-replay-publication-change", "request-v4-replay-publication-change")
	result, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		changedDigest, reservation, map[string]float64{}, nil)
	if err != nil || outcome != ordinalReplayContinueNovel || result != nil {
		t.Fatalf("changed-publication replay result=%#v outcome=%v err=%v", result, outcome, err)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReserved)
	if _, err := fixture.harness.store.LookupOrdinalMaterialization(context.Background(), control.OrdinalMaterializationLookup{
		CacheKeySHA256: fixture.cacheKey, TaskID: fixture.task.ID, GrantDigest: fixture.grantDigest,
		CatalogDigest: fixture.harness.catalog.SHA256, DictionarySetDigest: fixture.dictionarySetDigest,
	}); err != nil {
		t.Fatalf("changed binding disturbed the original committed materialization: %v", err)
	}
	if _, err := fixture.harness.store.ReleaseBudget(context.Background(), reservation.QueryID, "TEST_CLEANUP"); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinalSemanticReplayPreservesExactJSONNumbers(t *testing.T) {
	sourcePlaintext := exactNumberOrdinalReplaySource()
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-exact-numbers", sourcePlaintext)
	reservation := fixture.reserve(t, "query-v4-replay-exact-numbers", "request-v4-replay-exact-numbers")

	result, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, replaySemanticBinding(t, reservation))
	if err != nil || outcome != ordinalReplayCompleted || result["semantic_replay"] != true {
		t.Fatalf("exact-number replay outcome=%v result=%#v err=%v", outcome, result, err)
	}
	wantRows := `[[12.50,9007199254740993]]`
	assertExactJSONRows(t, result["rows"], wantRows)

	_, replayPlaintext, err := fixture.harness.store.GetEncryptedResult(context.Background(), fixture.task.ID,
		reservation.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := decodeStoredQueryResult(replayPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	assertExactJSONRows(t, replayed.Rows, wantRows)
	if !bytes.Contains(replayPlaintext, []byte(`"rows":[[12.50,9007199254740993]]`)) {
		t.Fatalf("replayed plaintext changed exact numbers: %s", replayPlaintext)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryCompleted)
}

func TestOrdinalSemanticReplayDoesNotReuseResultAboveFreshRowAllowance(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-row-allowance", twoRowOrdinalReplaySource(t))
	reservation := fixture.reserve(t, "query-v4-replay-row-allowance", "request-v4-replay-row-allowance")
	if reservation.AllowedRows != 1 {
		t.Fatalf("fresh replay allowance = %d, want 1", reservation.AllowedRows)
	}

	result, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, nil)
	if err != nil || outcome != ordinalReplayContinueNovel || result != nil {
		t.Fatalf("over-allowance replay result=%#v outcome=%v err=%v", result, outcome, err)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReserved)
	if _, err := fixture.harness.store.ReleaseBudget(context.Background(), reservation.QueryID, "TEST_CLEANUP"); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinalSemanticReplayInvalidMaterializationEvictsAndKeepsReservationForNovelPath(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-invalid-source", []byte(`{"row_count":1`))
	reservation := fixture.reserve(t, "query-v4-replay-invalid-source", "request-v4-replay-invalid-source")

	result, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, nil)
	if err != nil || outcome != ordinalReplayContinueNovel || result != nil {
		t.Fatalf("invalid materialization result=%#v outcome=%v err=%v", result, outcome, err)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReserved)
	if _, err := fixture.harness.store.LookupOrdinalMaterialization(context.Background(), control.OrdinalMaterializationLookup{
		CacheKeySHA256: fixture.cacheKey, TaskID: fixture.task.ID, GrantDigest: fixture.grantDigest,
		CatalogDigest: fixture.harness.catalog.SHA256, DictionarySetDigest: fixture.dictionarySetDigest,
	}); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("invalid materialization lookup after eviction = %v, want not found", err)
	}
	if _, err := fixture.harness.store.ReleaseBudget(context.Background(), reservation.QueryID, "TEST_CLEANUP"); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinalSemanticReplayOperationalLookupErrorReleasesReservation(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-lookup-error", validOrdinalReplaySource(t))
	reservation := fixture.reserve(t, "query-v4-replay-lookup-error", "request-v4-replay-lookup-error")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(canceled, fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, nil)
	if err == nil || outcome != ordinalReplayTerminated {
		t.Fatalf("lookup error outcome=%v err=%v", outcome, err)
	}
	record := assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReleased)
	if record.ErrorCode != ordinalReplayPreparationFailed {
		t.Fatalf("lookup error code=%q, want %q", record.ErrorCode, ordinalReplayPreparationFailed)
	}
}

func TestOrdinalSemanticReplayEvictionErrorReleasesReservation(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-evict-error", []byte(`{"row_count":1`))
	reservation := fixture.reserve(t, "query-v4-replay-evict-error", "request-v4-replay-evict-error")
	if _, err := fixture.harness.store.DB().ExecContext(context.Background(), `
CREATE FUNCTION force_replay_eviction_failure_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'forced replay eviction failure';
END;
$$;
CREATE TRIGGER force_replay_eviction_failure
BEFORE DELETE ON v4_committed_materializations
FOR EACH ROW EXECUTE FUNCTION force_replay_eviction_failure_fn()`); err != nil {
		t.Fatal(err)
	}

	_, outcome, err := fixture.harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, nil)
	if err == nil || outcome != ordinalReplayTerminated {
		t.Fatalf("eviction error outcome=%v err=%v", outcome, err)
	}
	assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryReleased)
}

type failingOrdinalReplaySpool struct {
	bytes.Buffer
	phase  string
	cancel context.CancelFunc
}

func (spool *failingOrdinalReplaySpool) Write(value []byte) (int, error) {
	if spool.phase == "write" {
		return 0, errors.New("forced replay spool write failure")
	}
	return spool.Buffer.Write(value)
}

func (spool *failingOrdinalReplaySpool) Seal() error {
	if spool.phase == "seal" {
		return errors.New("forced replay spool seal failure")
	}
	return nil
}

func (spool *failingOrdinalReplaySpool) Spilled() bool { return spool.phase == "open" }

func (spool *failingOrdinalReplaySpool) Open() (io.ReadCloser, error) {
	if spool.phase == "open" {
		return nil, errors.New("forced replay spool open failure")
	}
	return io.NopCloser(bytes.NewReader(spool.Buffer.Bytes())), nil
}

func (spool *failingOrdinalReplaySpool) Bytes() ([]byte, error) {
	if spool.cancel != nil {
		spool.cancel()
	}
	if spool.phase == "bytes" {
		return nil, errors.New("forced replay spool bytes failure")
	}
	return append([]byte(nil), spool.Buffer.Bytes()...), nil
}

func (*failingOrdinalReplaySpool) Close() error { return nil }

func TestOrdinalSemanticReplaySpoolErrorsFailReservation(t *testing.T) {
	for _, phase := range []string{"create", "write", "seal", "open", "bytes"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-spool-"+phase, validOrdinalReplaySource(t))
			reservation := fixture.reserve(t, "query-v4-replay-spool-"+phase, "request-v4-replay-spool-"+phase)
			factory := func(_, _, _ string, _ int64) (ordinalReplaySpool, error) {
				if phase == "create" {
					return nil, errors.New("forced replay spool creation failure")
				}
				return &failingOrdinalReplaySpool{phase: phase}, nil
			}

			_, outcome, err := fixture.harness.service.tryOrdinalSemanticReplayWithSpool(context.Background(), fixture.task,
				reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
				fixture.dictionarySetDigest, reservation, map[string]float64{}, factory, nil)
			if err == nil || outcome != ordinalReplayTerminated {
				t.Fatalf("spool %s outcome=%v err=%v", phase, outcome, err)
			}
			record := assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryFailed)
			wantCode := resultEncodingFailed
			if phase == "open" || phase == "bytes" {
				wantCode = resultFinalizationFailed
			}
			requireChargedUsage(t, record, 1, 1, wantCode)
			if _, _, resultErr := fixture.harness.store.GetEncryptedResult(context.Background(), fixture.task.ID,
				reservation.QueryID); !errors.Is(resultErr, control.ErrNotFound) {
				t.Fatalf("spool %s persisted result: %v", phase, resultErr)
			}
		})
	}
}

func TestOrdinalSemanticReplayPostCommitReadbackErrorDoesNotCleanCompletedQuery(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-replay-post-commit", validOrdinalReplaySource(t))
	reservation := fixture.reserve(t, "query-v4-replay-post-commit", "request-v4-replay-post-commit")
	replayCtx, cancel := context.WithCancel(context.Background())
	factory := func(_, _, _ string, _ int64) (ordinalReplaySpool, error) {
		return &failingOrdinalReplaySpool{cancel: cancel}, nil
	}

	_, outcome, err := fixture.harness.service.tryOrdinalSemanticReplayWithSpool(replayCtx, fixture.task,
		reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
		fixture.dictionarySetDigest, reservation, map[string]float64{}, factory,
		replaySemanticBinding(t, reservation))
	if err == nil || outcome != ordinalReplayCompleted {
		t.Fatalf("post-commit readback outcome=%v err=%v", outcome, err)
	}
	record := assertReplayReservationStatus(t, fixture, reservation.QueryID, control.QueryCompleted)
	requireChargedUsage(t, record, 1, 1, "")
	if _, _, resultErr := fixture.harness.store.GetEncryptedResult(context.Background(), fixture.task.ID,
		reservation.QueryID); resultErr != nil {
		t.Fatalf("committed replay result unavailable: %v", resultErr)
	}
}

// replaySemanticBinding is the execution binding a semantic-replay settlement
// carries, for the tests that drive tryOrdinalSemanticReplay directly.
//
// It is built from the reservation's own authoritative pre-states -- the same
// budget snapshot and exposure ledger production reads under the task lock --
// through the same exposureLedgerBefore that production uses, so the receipt's
// limit reproduction is checked against real numbers rather than fixture ones.
// Only the preparation is synthetic: these fixtures materialize an observation
// directly in the store and never compile a statement.
func replaySemanticBinding(t *testing.T, reservation control.BudgetReservation) *control.QueryExecutionBinding {
	t.Helper()
	if reservation.ExposureLedgerBefore == nil {
		t.Fatal("the reservation carries no exposure ledger pre-state")
	}
	fixture := func(seed string) string { return strings.Repeat(seed, 64/len(seed)) }
	compiler, err := preparedbinding.CompilerIdentityV1{
		QueryPlanVersion: "queryplan-v7", QueryPlanSHA256: fixture("c2"),
		PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererSHA256: fixture("a3"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal compiler identity: %v", err)
	}
	prepared, err := preparedbinding.PreparedOperationBindingV1{
		Grouped: true, VisibleFieldCount: 2, FactFieldCount: 1,
		VisibleFieldsSHA256:     fixture("11"),
		FactFieldsSHA256:        fixture("12"),
		PreparationInputsSHA256: fixture("14"),
		GrantSHA256:             fixture("15"),
		CatalogSHA256:           fixture("16"),
		PlanSHA256:              fixture("18"),
		CompilerIdentitySHA256:  compiler.SHA256,
		PolicyGrantSHA256:       fixture("19"),
		VisibleTargetSHA256:     fixture("a4"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal prepared operation: %v", err)
	}
	ledgerSnapshot := *reservation.ExposureLedgerBefore
	remainingRows := reservation.Before.Remaining().Rows
	state := physicalquery.LedgerPreState{
		RemainingRows: remainingRows, HasExposureContext: true,
		InfluenceFacts:       ledgerSnapshot.Limits.InfluenceFacts,
		UsesExpandedEvidence: prepared.UsesExpandedEvidence(),
	}
	ledger, err := exposureLedgerBefore(ledgerSnapshot, reservation.Before, state)
	if err != nil {
		t.Fatalf("seal exposure ledger pre-state: %v", err)
	}
	budgetDigest, err := queryreceipt.BudgetStateSHA256(queryReceiptBudget(reservation.Before))
	if err != nil {
		t.Fatalf("budget digest: %v", err)
	}
	// The limit the signed pre-state authorizes, derived the way the receipt
	// re-derives it: the row budget, narrowed by the influence limit when the
	// evidence is not expanded.
	visibleRowLimit := remainingRows
	if !state.UsesExpandedEvidence && ledgerSnapshot.Limits.InfluenceFacts < visibleRowLimit {
		visibleRowLimit = ledgerSnapshot.Limits.InfluenceFacts
	}
	document, err := querybinding.QueryExecutionBindingV2{
		PathKind: querybinding.PathSemanticReplay, PreparedOperation: prepared, Compiler: compiler,
		ExposureProfileVersion: ledger.ProfileVersion,
		VisibleRowLimit:        visibleRowLimit,
		BudgetBeforeSHA256:     budgetDigest, ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible: querybinding.TargetRecordV1{
			// Authorized but not executed: deriving the replay key requires
			// authorizing the statement, and nothing reaches the Connector.
			Role: querybinding.RoleVisible, Authorized: true, Executed: false,
			ExactSQLSHA256: fixture("a1"), StrictASTSHA256: fixture("a2"),
			RowLimit: visibleRowLimit, PolicyFingerprint: "replay-fingerprint",
			PolicyRendererVersion:       compiler.PolicyRendererVersion,
			PolicyRendererDigest:        compiler.PolicyRendererSHA256,
			PreparedTargetBindingSHA256: fixture("a4"),
		},
	}.Seal()
	if err != nil {
		t.Fatalf("seal semantic replay binding: %v", err)
	}
	binding := &control.QueryExecutionBinding{
		QueryID: reservation.QueryID, BindingV2: &document, ExposureLedgerBefore: &ledger,
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("the fixture replay binding does not validate: %v", err)
	}
	return binding
}
