package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestOrdinalMaterializationPendingArtifactPublishesButDoesNotReplay(t *testing.T) {
	fixture, materialization := newOrdinalMaterializationArtifactFixture(t, true)
	if materialization.SourceQueryID != fixture.queryID || materialization.ResultSHA256 != fixture.resultSHA256 ||
		materialization.ResultKeyID != fixture.keyID {
		t.Fatalf("pending artifact materialization = %+v", materialization)
	}

	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending artifact lookup = %v, want not found", err)
	}
	if deleted, err := fixture.store.DeleteUnusableOrdinalMaterialization(context.Background(), fixture.lookup()); err != nil || deleted {
		t.Fatalf("pending artifact cleanup = %v, %v; want retained", deleted, err)
	}
	if _, err := fixture.store.EraseResultEncryptionKey(context.Background(), fixture.keyID, "privacy-admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("erase key with pending artifact = %v, want conflict", err)
	}

	if _, err := fixture.store.MarkResultArtifactAvailable(context.Background(), fixture.resultID,
		"canonical-etag", "gateway"); err != nil {
		t.Fatalf("promote pending artifact: %v", err)
	}
	available, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup())
	if err != nil {
		t.Fatalf("LookupOrdinalMaterialization after promotion: %v", err)
	}
	if available.SourceQueryID != fixture.queryID || available.ResultSHA256 != fixture.resultSHA256 ||
		available.ResultKeyID != fixture.keyID {
		t.Fatalf("available artifact materialization = %+v", available)
	}
	if _, err := fixture.store.EraseResultEncryptionKey(context.Background(), fixture.keyID, "privacy-admin"); err != nil {
		t.Fatalf("erase artifact result key: %v", err)
	}
	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("erased-key artifact lookup = %v, want not found", err)
	}
	if deleted, err := fixture.store.DeleteUnusableOrdinalMaterialization(context.Background(), fixture.lookup()); err != nil || !deleted {
		t.Fatalf("erased-key artifact cleanup = %v, %v; want evicted", deleted, err)
	}
}

func TestOrdinalMaterializationArtifactCleanupRequiresReplayableSource(t *testing.T) {
	fixture, _ := newOrdinalMaterializationArtifactFixture(t, false)
	if _, err := fixture.store.MarkResultArtifactAvailable(context.Background(), fixture.resultID,
		"canonical-etag", "gateway"); err != nil {
		t.Fatalf("promote artifact before publication: %v", err)
	}
	if _, err := fixture.store.PublishOrdinalMaterialization(context.Background(), fixture.queryID,
		OrdinalMaterializationPublish{CacheKeySHA256: fixture.cacheKey}); err != nil {
		t.Fatalf("PublishOrdinalMaterialization with available artifact: %v", err)
	}
	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup()); err != nil {
		t.Fatalf("available artifact lookup: %v", err)
	}
	if deleted, err := fixture.store.DeleteUnusableOrdinalMaterialization(context.Background(), fixture.lookup()); err != nil || deleted {
		t.Fatalf("available artifact cleanup = %v, %v; want retained", deleted, err)
	}

	if _, err := fixture.store.MarkResultArtifactDeleting(context.Background(), fixture.resultID, "retention"); err != nil {
		t.Fatalf("mark artifact deleting: %v", err)
	}
	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting artifact lookup = %v, want not found", err)
	}
	deleted, err := fixture.store.DeleteUnusableOrdinalMaterialization(context.Background(), fixture.lookup())
	if err != nil || !deleted {
		t.Fatalf("deleting artifact cleanup = %v, %v; want evicted", deleted, err)
	}
	if err := fixture.store.DeleteOrdinalMaterialization(context.Background(), fixture.taskID, fixture.cacheKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unusable artifact materialization remains after cleanup: %v", err)
	}
}

func TestOrdinalMaterializationExpiredArtifactIsNotReplayable(t *testing.T) {
	fixture, _ := newOrdinalMaterializationArtifactFixture(t, false)
	if _, err := fixture.store.MarkResultArtifactAvailable(context.Background(), fixture.resultID,
		"canonical-etag", "gateway"); err != nil {
		t.Fatalf("promote artifact: %v", err)
	}
	if _, err := fixture.store.PublishOrdinalMaterialization(context.Background(), fixture.queryID,
		OrdinalMaterializationPublish{CacheKeySHA256: fixture.cacheKey}); err != nil {
		t.Fatalf("publish materialization: %v", err)
	}
	fixture.clock.value = fixture.clock.value.Add(2 * time.Hour)
	if _, err := fixture.store.LookupOrdinalMaterialization(context.Background(), fixture.lookup()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired artifact lookup = %v, want not found", err)
	}
	if deleted, err := fixture.store.DeleteUnusableOrdinalMaterialization(context.Background(), fixture.lookup()); err != nil || !deleted {
		t.Fatalf("expired artifact cleanup = %v, %v; want evicted", deleted, err)
	}
}

type ordinalMaterializationArtifactFixture struct {
	store        *Store
	taskID       string
	queryID      string
	resultID     string
	cacheKey     string
	resultSHA256 string
	keyID        string
	clock        *mutableArtifactClock
}

type mutableArtifactClock struct{ value time.Time }

func (clock *mutableArtifactClock) Now() time.Time { return clock.value }

func newOrdinalMaterializationArtifactFixture(t *testing.T,
	publish bool) (ordinalMaterializationArtifactFixture, OrdinalMaterialization) {
	t.Helper()
	cipher := testCipher(t, 57)
	clock := &mutableArtifactClock{value: time.Now().UTC()}
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(clock))
	publishOrdinalTestDictionary(t, store)
	expires := clock.value.Add(time.Hour)
	suffix := "available"
	if publish {
		suffix = "pending"
	}
	taskID := "task_v4_artifact_" + suffix
	queryID := "query_v4_artifact_" + suffix
	resultID := "result_v4_artifact_" + suffix
	createAwaitingApprovalTask(t, store, taskID, expires)
	approveOrdinalTask(t, store, taskID, expires,
		ExposureLimits{ReleaseFacts: 4, InfluenceFacts: 4, OutcomeFacts: 4})
	reservation := reserveOrdinalQuery(t, store, taskID, queryID, "request-"+queryID)
	observation := testOrdinalObservation(t, 0, 0, "outcome-"+queryID)
	createdAt := clock.value
	resultSHA256 := strings.Repeat("c", 64)
	artifact := ResultArtifact{
		ResultID: resultID, QueryID: queryID, TaskID: taskID, KeyID: cipher.KeyID(),
		Format: "parquet", Encryption: "chunked-aes-gcm-v1",
		StagingKey: "staging/" + resultID, ObjectKey: "objects/" + resultID, ObjectETag: "staging-etag",
		ParquetSHA256: resultSHA256, ObjectSHA256: strings.Repeat("a", 64),
		ParquetSize: 4, ObjectSize: 32, RowCount: 1, ColumnCount: 1,
		SchemaJSON: []byte(`[{"name":"amount","type":"INT64"}]`), ResultMetadataJSON: []byte(`{}`),
		ACLJSON: []byte(`{"subject":"alice"}`), Status: ResultArtifactPending, CreatedAt: createdAt,
		ExpiresAt: &expires,
	}
	var request *OrdinalMaterializationPublish
	cacheKey := strings.Repeat("7", 64)
	if publish {
		request = &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey}
	}
	record, _, _, err := store.FinalizeOrdinalQueryArtifactMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, artifact, request, nil)
	if err != nil {
		t.Fatalf("finalize artifact source query: %v", err)
	}
	if record.ResultSHA256 != resultSHA256 {
		t.Fatalf("artifact query result hash = %q, want %q", record.ResultSHA256, resultSHA256)
	}
	fixture := ordinalMaterializationArtifactFixture{
		store: store, taskID: taskID, queryID: queryID, resultID: resultID,
		cacheKey: cacheKey, resultSHA256: record.ResultSHA256, keyID: cipher.KeyID(), clock: clock,
	}
	if !publish {
		return fixture, OrdinalMaterialization{}
	}
	materialization, err := store.PublishOrdinalMaterialization(context.Background(), queryID,
		OrdinalMaterializationPublish{CacheKeySHA256: cacheKey})
	if err != nil {
		t.Fatalf("idempotently republish pending artifact materialization: %v", err)
	}
	return fixture, materialization
}

func (fixture ordinalMaterializationArtifactFixture) lookup() OrdinalMaterializationLookup {
	return OrdinalMaterializationLookup{
		CacheKeySHA256:      fixture.cacheKey,
		TaskID:              fixture.taskID,
		GrantDigest:         controlTestDigest,
		CatalogDigest:       controlTestDigest,
		DictionarySetDigest: testOrdinalSet,
	}
}
