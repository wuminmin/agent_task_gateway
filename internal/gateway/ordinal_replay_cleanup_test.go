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
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

type ordinalReplayTestFixture struct {
	harness             *gatewayHarness
	task                control.Task
	grantDigest         string
	cacheKey            string
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
	outcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion,
		strings.Repeat("1", 64), strings.Repeat("2", 64), 1)
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
		dictionarySetDigest: dictionarySetDigest,
	}
	source := fixture.reserve(t, taskID+"-source", taskID+"-source-request")
	expires := harness.clock.value.Add(time.Hour)
	if _, _, _, err := harness.store.FinalizeOrdinalQueryMeasuredWithReceipt(context.Background(), control.BudgetSettlement{
		QueryID: source.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
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

func (fixture ordinalReplayTestFixture) reserve(t *testing.T, queryID, requestID string) control.BudgetReservation {
	t.Helper()
	reservation, err := fixture.harness.store.ReserveBudget(context.Background(), control.ReserveRequest{
		QueryID: queryID, TaskID: fixtureTaskID(fixture, queryID), RequestID: requestID,
		Actor: fixture.harness.alice.Subject, RequestDigest: strings.Repeat("6", 64),
		SQLFingerprint: strings.Repeat("7", 64), CatalogVersion: fixture.harness.catalog.CatalogVersion,
		CatalogDigest:  fixture.harness.catalog.SHA256,
		DatasourceID:   fixture.harness.connector.attestation.DatasourceID,
		SchemaDigest:   fixture.harness.connector.attestation.SchemaDigest,
		ManifestDigest: strings.Repeat("4", 64), GrantDigest: fixture.grantDigest, PolicyDecision: "ALLOW",
		RequestedRows: 1, RequestedDBMS: 10,
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
		fixture.dictionarySetDigest, reservation, map[string]float64{})
	if err != nil || outcome != ordinalReplayContinueNovel || result != nil {
		t.Fatalf("normal miss result=%#v outcome=%v err=%v", result, outcome, err)
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
		fixture.dictionarySetDigest, reservation, map[string]float64{})
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
		fixture.dictionarySetDigest, reservation, map[string]float64{})
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
		fixture.dictionarySetDigest, reservation, map[string]float64{})
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
				fixture.dictionarySetDigest, reservation, map[string]float64{}, factory)
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
		fixture.dictionarySetDigest, reservation, map[string]float64{}, factory)
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
