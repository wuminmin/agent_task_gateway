package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestFinalizeQueryStreamRoundTripAndRetentionCompatibility(t *testing.T) {
	stageDirectory := t.TempDir()
	t.Setenv("TMPDIR", stageDirectory)
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 47))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_stream_result", expires)
	approveTask(t, store, "task_stream_result", expires, BudgetLimits{Queries: 2, Rows: 20, DBMS: 2000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_stream_result", TaskID: "task_stream_result", RequestID: "request-stream-result",
		Actor: "alice", RequestDigest: "digest-stream-result", SQLFingerprint: "sql-stream-result",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), resultStreamChunkSize/8+17)
	record, _, metrics, err := store.FinalizeQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 2, DBMS: 9,
	}, bytes.NewReader(payload), nil)
	if err != nil || record.Status != QueryCompleted || metrics.Encryption <= 0 || metrics.SettlementStore <= 0 {
		t.Fatalf("FinalizeQueryStreamMeasuredWithReceipt = %+v metrics=%+v err=%v", record, metrics, err)
	}
	metadata, actual, err := store.GetEncryptedResult(context.Background(), "task_stream_result", reservation.QueryID)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("GetEncryptedResult length=%d err=%v", len(actual), err)
	}
	if metadata.StorageFormat != resultStorageChunked || metadata.ChunkCount != 3 ||
		metadata.PlaintextSize == nil || *metadata.PlaintextSize != int64(len(payload)) {
		t.Fatalf("chunked metadata = %+v", metadata)
	}
	var chunks int
	if err := store.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1`, reservation.QueryID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != int(metadata.ChunkCount) {
		t.Fatalf("stored chunks = %d, want %d", chunks, metadata.ChunkCount)
	}
	purged, err := store.PurgeEncryptedResultsBefore(context.Background(), time.Now().UTC().Add(time.Minute))
	if err != nil || purged != 1 {
		t.Fatalf("PurgeEncryptedResultsBefore = %d, %v", purged, err)
	}
	if err := store.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1`, reservation.QueryID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Fatalf("retention purge left %d chunk rows", chunks)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_stream_result", reservation.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEncryptedResult after purge = %v, want not found", err)
	}
	assertEmptyDirectory(t, stageDirectory)
}

func TestSingleCiphertextRowsRemainReadableWithoutPostMigrationSize(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 46))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_single_result_compat", expires)
	approveTask(t, store, "task_single_result_compat", expires, BudgetLimits{Queries: 1, Rows: 10, DBMS: 1000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_single_result_compat", TaskID: "task_single_result_compat", RequestID: "request-single-result-compat",
		Actor: "alice", RequestDigest: "digest-single-result-compat", SQLFingerprint: "sql-single-result-compat",
		RequestedRows: 10, RequestedDBMS: 100,
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"rows":[["legacy-compatible"]]}`)
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1,
	}, payload); err != nil {
		t.Fatal(err)
	}
	// Rows created before migration 015 have no plaintext_size; the migration
	// assigns the single format and zero chunks without rewriting ciphertext.
	if _, err := store.db.ExecContext(context.Background(), `
UPDATE encrypted_query_results SET plaintext_size=NULL WHERE query_id=$1`, reservation.QueryID); err != nil {
		t.Fatal(err)
	}
	metadata, actual, err := store.GetEncryptedResult(context.Background(), "task_single_result_compat", reservation.QueryID)
	if err != nil || metadata.StorageFormat != resultStorageSingle || metadata.PlaintextSize != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("legacy-compatible read metadata=%+v payload=%q err=%v", metadata, actual, err)
	}
}

func TestFinalizeQueryStreamReceiptFailureRollsBackEverything(t *testing.T) {
	stageDirectory := t.TempDir()
	t.Setenv("TMPDIR", stageDirectory)
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 48))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_stream_rollback", expires)
	approveTask(t, store, "task_stream_rollback", expires, BudgetLimits{Queries: 2, Rows: 20, DBMS: 2000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_stream_rollback", TaskID: "task_stream_rollback", RequestID: "request-stream-rollback",
		Actor: "alice", RequestDigest: "digest-stream-rollback", SQLFingerprint: "sql-stream-rollback",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("rollback"), resultStreamChunkSize/4)
	_, _, _, err = store.FinalizeQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 2, DBMS: 9,
	}, bytes.NewReader(payload), func(QueryReceipt) (SaveQueryReceiptRequest, error) {
		return SaveQueryReceiptRequest{}, errors.New("forced receipt failure")
	})
	if err == nil {
		t.Fatal("stream finalize succeeded despite receipt failure")
	}
	record, err := store.GetQuery(context.Background(), reservation.QueryID)
	if err != nil || record.Status != QueryReserved || record.CompletedAt != nil {
		t.Fatalf("query after rollback = %+v err=%v", record, err)
	}
	var results, chunks int
	if err := store.db.QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM encrypted_query_results WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1)`, reservation.QueryID).Scan(&results, &chunks); err != nil {
		t.Fatal(err)
	}
	if results != 0 || chunks != 0 {
		t.Fatalf("partial stream result survived rollback: results=%d chunks=%d", results, chunks)
	}
	var terminalAudits int
	if err := store.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM audit_events
WHERE query_id=$1 AND event_type IN ('QUERY_COMPLETED','QUERY_RESULT_STORED')`, reservation.QueryID).Scan(&terminalAudits); err != nil {
		t.Fatal(err)
	}
	if terminalAudits != 0 {
		t.Fatalf("receipt failure left %d terminal audit events", terminalAudits)
	}
	budget, err := store.GetBudget(context.Background(), "task_stream_rollback")
	if err != nil || budget.Usage.UsedQueries != 0 || budget.Usage.ReservedQueries != 1 {
		t.Fatalf("budget after rollback = %+v err=%v", budget, err)
	}
	assertEmptyDirectory(t, stageDirectory)
}

func TestFinalizeQueryStreamReadFailureLeavesOnlyReservation(t *testing.T) {
	stageDirectory := t.TempDir()
	t.Setenv("TMPDIR", stageDirectory)
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 49))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_stream_read_failure", expires)
	approveTask(t, store, "task_stream_read_failure", expires, BudgetLimits{Queries: 2, Rows: 20, DBMS: 2000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_stream_read_failure", TaskID: "task_stream_read_failure", RequestID: "request-stream-read-failure",
		Actor: "alice", RequestDigest: "digest-stream-read-failure", SQLFingerprint: "sql-stream-read-failure",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	reader := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), 1024)), errorReader{})
	if _, _, _, err := store.FinalizeQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1,
	}, reader, nil); err == nil {
		t.Fatal("stream finalize accepted a failed plaintext source")
	}
	record, err := store.GetQuery(context.Background(), reservation.QueryID)
	if err != nil || record.Status != QueryReserved {
		t.Fatalf("query after source failure = %+v err=%v", record, err)
	}
	var results int
	if err := store.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM encrypted_query_results WHERE query_id=$1`, reservation.QueryID).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if results != 0 {
		t.Fatalf("source failure stored %d result rows", results)
	}
	assertEmptyDirectory(t, stageDirectory)
}

func TestGetEncryptedChunkedResultRejectsTampering(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 50))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_stream_tamper", expires)
	approveTask(t, store, "task_stream_tamper", expires, BudgetLimits{Queries: 2, Rows: 20, DBMS: 2000})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_stream_tamper", TaskID: "task_stream_tamper", RequestID: "request-stream-tamper",
		Actor: "alice", RequestDigest: "digest-stream-tamper", SQLFingerprint: "sql-stream-tamper",
		RequestedRows: 10, RequestedDBMS: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.FinalizeQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1,
	}, bytes.NewReader(bytes.Repeat([]byte("tamper"), resultStreamChunkSize/3)), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `
UPDATE encrypted_query_result_chunks
SET ciphertext=set_byte(ciphertext, octet_length(ciphertext)-1,
 ((get_byte(ciphertext,octet_length(ciphertext)-1)#255)))
WHERE query_id=$1 AND chunk_ordinal=0`, reservation.QueryID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_stream_tamper", reservation.QueryID); !errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("tampered chunk read = %v, want ciphertext invalid", err)
	}
}

func TestOrdinalChunkedResultPublishesReplayableMaterialization(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 51))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_stream_materialization", expires)
	approveOrdinalTask(t, store, "task_v4_stream_materialization", expires,
		ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20})
	reservation := reserveOrdinalQuery(t, store, "task_v4_stream_materialization",
		"query_v4_stream_materialization", "request-v4-stream-materialization")
	observation := testOrdinalObservation(t, 0, 0, "stream-materialization-outcome")
	cacheKey := "abababababababababababababababababababababababababababababababab"
	payload := bytes.Repeat([]byte("materialized-result"), resultStreamChunkSize/4)
	if _, _, _, err := store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, bytes.NewReader(payload), &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey}, nil); err != nil {
		t.Fatal(err)
	}
	materialization, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_stream_materialization", GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	})
	if err != nil || materialization.SourceQueryID != reservation.QueryID {
		t.Fatalf("chunked materialization = %+v err=%v", materialization, err)
	}
	metadata, actual, err := store.GetEncryptedResult(context.Background(), "task_v4_stream_materialization", materialization.SourceQueryID)
	if err != nil || metadata.StorageFormat != resultStorageChunked || !bytes.Equal(actual, payload) {
		t.Fatalf("chunked materialization source format=%q length=%d err=%v", metadata.StorageFormat, len(actual), err)
	}

	replay := reserveOrdinalQuery(t, store, "task_v4_stream_materialization",
		"query_v4_stream_materialization_replay", "request-v4-stream-materialization-replay")
	if _, _, _, err := store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: replay.QueryID, Rows: 1, DBMS: 1,
		OrdinalObservationRef: &OrdinalObservationReference{
			ObservationSHA256: materialization.Observation.ObservationSHA256, DictionarySetDigest: testOrdinalSet,
		},
	}, bytes.NewReader(actual), nil, nil); err != nil {
		t.Fatal(err)
	}
	charge, err := store.GetExposureCharge(context.Background(), replay.QueryID)
	if err != nil || charge.ChargedReleaseFacts != 0 || charge.ChargedInfluenceFacts != 0 || charge.ChargedOutcomeFacts != 0 {
		t.Fatalf("chunked materialization replay charge = %+v err=%v", charge, err)
	}

	headBefore, err := store.GetOrdinalRootHead(context.Background(), "task_v4_stream_materialization")
	if err != nil {
		t.Fatal(err)
	}
	rollback := reserveOrdinalQuery(t, store, "task_v4_stream_materialization",
		"query_v4_stream_materialization_rollback", "request-v4-stream-materialization-rollback")
	rollbackObservation := testOrdinalObservation(t, 1, 0, "stream-materialization-rollback-outcome")
	if _, _, _, err := store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: rollback.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &rollbackObservation,
	}, bytes.NewReader(payload), nil, func(QueryReceipt) (SaveQueryReceiptRequest, error) {
		return SaveQueryReceiptRequest{}, errors.New("forced V4 stream receipt failure")
	}); err == nil {
		t.Fatal("V4 chunked finalize succeeded despite receipt failure")
	}
	headAfter, err := store.GetOrdinalRootHead(context.Background(), "task_v4_stream_materialization")
	if err != nil || headAfter != headBefore {
		t.Fatalf("V4 stream rollback changed root head: before=%+v after=%+v err=%v", headBefore, headAfter, err)
	}
	var partialResults, partialChunks, queryReferences, terminalAudits int
	if err := store.db.QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM encrypted_query_results WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1),
 (SELECT count(*) FROM v4_query_observations WHERE query_id=$1),
 (SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type IN ('QUERY_COMPLETED','QUERY_RESULT_STORED'))`,
		rollback.QueryID).Scan(&partialResults, &partialChunks, &queryReferences, &terminalAudits); err != nil {
		t.Fatal(err)
	}
	if partialResults != 0 || partialChunks != 0 || queryReferences != 0 || terminalAudits != 0 {
		t.Fatalf("V4 stream rollback left result=%d chunks=%d observation_refs=%d terminal_audits=%d",
			partialResults, partialChunks, queryReferences, terminalAudits)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("forced source failure") }

func assertEmptyDirectory(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("encrypted staging directory retained %d entries", len(entries))
	}
}
