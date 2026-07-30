package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestGatewaySpilledResultCommitsBeforeReleaseAndReplays(t *testing.T) {
	harness := newGatewayHarness(t)
	spoolDirectory := t.TempDir()
	harness.service.spoolDirectory = spoolDirectory
	harness.service.spoolThreshold = 64
	harness.createActiveSummaryTask(t, "task-spilled-result")
	harness.connector.result = dataconnector.Result{
		Columns:  []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:     [][]any{{"2026-01-" + string(bytes.Repeat([]byte("x"), 256)), 123.45}},
		RowCount: 1,
	}
	arguments := map[string]any{
		"task_id": "task-spilled-result", "request_id": "spilled-result-request", "sql": testSummarySQL,
	}
	first := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", arguments)
	queryID := first["query_id"].(string)
	var format string
	var chunks int64
	if err := harness.store.DB().QueryRowContext(context.Background(), `
SELECT storage_format,chunk_count FROM encrypted_query_results WHERE query_id=$1`, queryID).Scan(&format, &chunks); err != nil {
		t.Fatal(err)
	}
	if format != "chunked-aes-gcm-v1" || chunks <= 0 {
		t.Fatalf("spilled result storage format=%q chunks=%d", format, chunks)
	}
	replay := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", arguments)
	if replay["idempotent_replay"] != true || replay["query_id"] != queryID {
		t.Fatalf("spilled result replay = %#v", replay)
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("spilled idempotent replay executed connector %d times", len(harness.connector.requests))
	}
	entries, err := os.ReadDir(spoolDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("gateway retained query-private spool entries: %v, %v", entries, err)
	}
}

func TestGatewayResultIsNotReleasedUntilCommitCompletes(t *testing.T) {
	fixture := newOrdinalReplayTestFixture(t, "task-v4-commit-before-release", validOrdinalReplaySource(t))
	harness := fixture.harness
	reservation := fixture.reserve(t, "query-v4-commit-before-release", "request-v4-commit-before-release")
	database := harness.store.DB()
	var advisoryKey int64
	if err := database.QueryRowContext(context.Background(), `
SELECT oid::bigint FROM pg_namespace WHERE nspname=current_schema()`).Scan(&advisoryKey); err != nil {
		t.Fatal(err)
	}
	locker, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, advisoryKey); err != nil {
		_ = locker.Close()
		t.Fatal(err)
	}
	unlocked := false
	unlock := func() {
		if unlocked {
			return
		}
		unlocked = true
		if _, unlockErr := locker.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryKey); unlockErr != nil {
			t.Errorf("unlock deferred commit: %v", unlockErr)
		}
		if closeErr := locker.Close(); closeErr != nil {
			t.Errorf("close commit locker: %v", closeErr)
		}
	}
	defer unlock()
	if _, err := database.ExecContext(context.Background(), `
CREATE FUNCTION block_result_commit_until_test_unlock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock((SELECT oid::bigint FROM pg_namespace WHERE nspname=current_schema()));
  RETURN NEW;
END;
$$;
CREATE CONSTRAINT TRIGGER block_result_commit_until_test_unlock
AFTER INSERT ON encrypted_query_results
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION block_result_commit_until_test_unlock()`); err != nil {
		t.Fatal(err)
	}

	type toolOutcome struct {
		result  map[string]any
		outcome ordinalReplayOutcome
		err     error
	}
	completed := make(chan toolOutcome, 1)
	go func() {
		result, replayOutcome, callErr := harness.service.tryOrdinalSemanticReplay(context.Background(), fixture.task,
			reservation.RequestID, reservation.QueryID, fixture.grantDigest, fixture.cacheKey,
			fixture.dictionarySetDigest, reservation, map[string]float64{})
		completed <- toolOutcome{result: result, outcome: replayOutcome, err: callErr}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case outcome := <-completed:
			t.Fatalf("Gateway returned before its V4 result transaction could commit: result=%#v outcome=%v err=%v",
				outcome.result, outcome.outcome, outcome.err)
		default:
		}
		var waiting int
		if err := database.QueryRowContext(context.Background(), `
SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory'
  AND upper(trim(query))='COMMIT'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Gateway finalizer did not reach the deferred COMMIT barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case outcome := <-completed:
		t.Fatalf("Gateway released a V4 result while COMMIT was blocked: result=%#v outcome=%v err=%v",
			outcome.result, outcome.outcome, outcome.err)
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case outcome := <-completed:
		if outcome.err != nil || outcome.outcome != ordinalReplayCompleted ||
			outcome.result["status"] != control.QueryCompleted || outcome.result["semantic_replay"] != true {
			t.Fatalf("Gateway V4 result after COMMIT = %#v, outcome=%v err=%v",
				outcome.result, outcome.outcome, outcome.err)
		}
		if _, _, err := harness.store.GetEncryptedResult(context.Background(), fixture.task.ID,
			reservation.QueryID); err != nil {
			t.Fatalf("released result is not committed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Gateway did not release the result after COMMIT completed")
	}
}

func TestCommittedResultReadPathsPreserveExactJSONNumbers(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-exact-result-numbers")
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows: [][]any{
			{"2026-01", json.Number("12.50")},
			{"2026-02", json.Number("9007199254740993")},
		},
		RowCount: 2,
	}
	arguments := map[string]any{
		"task_id": "task-exact-result-numbers", "request_id": "exact-result-numbers-request", "sql": testSummarySQL,
	}
	first := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", arguments)
	wantRows := `[["2026-01",12.50],["2026-02",9007199254740993]]`
	assertExactJSONRows(t, first["rows"], wantRows)

	idempotent := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", arguments)
	if idempotent["idempotent_replay"] != true {
		t.Fatalf("same-request result was not an idempotent replay: %#v", idempotent)
	}
	assertExactJSONRows(t, idempotent["rows"], wantRows)

	stored := mustCallGatewayTool(t, harness.service, harness.alice, "get_query_result", map[string]any{
		"task_id": "task-exact-result-numbers", "query_id": first["query_id"],
	})
	assertExactJSONRows(t, stored["rows"], wantRows)
	if len(harness.connector.requests) != 1 {
		t.Fatalf("committed result reads executed connector %d times", len(harness.connector.requests))
	}
}

func TestOrdinalSemanticReplayReadsChunkedMaterializationAndWritesFreshChunkedResult(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.service.spoolDirectory = t.TempDir()
	harness.service.spoolThreshold = 64
	harness.createSummaryTaskWithGrantAndExposureProfile(t, "task-v4-chunked-replay", nil,
		control.ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10}, exposure.ProfileV4)

	product := compactOrdinalProduct()
	compilation, err := queryplan.CompileOrdinal(queryplan.QueryPlan{Product: product.Name, Columns: []string{"amount"}}, product)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newOrdinalDerivationFixture(t, compilation.OrdinalProgram,
		[]map[string]any{{"id": int64(1), "amount": int64(10)}})
	manifestDigest := fixture.artifact.Hot.ManifestDigest()
	if err := harness.store.PutOrdinalSnapshotPublication(context.Background(), manifestDigest, fixture.artifact.Hot, nil); err != nil {
		t.Fatal(err)
	}
	dictionarySet, err := ordinal.NewDictionarySetManifest(harness.catalog.SHA256, ordinal.DictionarySetMember{
		PublicationName: "chunked-replay-publication", DictionaryDigest: fixture.artifact.Hot.DictionaryDigest(),
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
	outcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion, strings.Repeat("1", 64), strings.Repeat("2", 64), 1)
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
	grantDigest := strings.Repeat("3", 64)
	manifest := strings.Repeat("4", 64)
	cacheKey := strings.Repeat("5", 64)
	reserve := func(queryID, requestID string) control.BudgetReservation {
		t.Helper()
		reservation, err := harness.store.ReserveBudget(context.Background(), control.ReserveRequest{
			QueryID: queryID, TaskID: "task-v4-chunked-replay", RequestID: requestID, Actor: harness.alice.Subject,
			RequestDigest: strings.Repeat("6", 64), SQLFingerprint: strings.Repeat("7", 64),
			CatalogVersion: harness.catalog.CatalogVersion, CatalogDigest: harness.catalog.SHA256,
			DatasourceID: harness.connector.attestation.DatasourceID, SchemaDigest: harness.connector.attestation.SchemaDigest,
			ManifestDigest: manifest, GrantDigest: grantDigest, PolicyDecision: "ALLOW",
			RequestedRows: 1, RequestedDBMS: 10,
			Exposure: &control.ExposureReservationRequest{ProfileVersion: exposure.ProfileV4,
				EstimatedOutcomeFacts: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return reservation
	}

	sourceReservation := reserve("query-v4-chunked-source", "request-v4-chunked-source")
	source := storedQueryResult{
		Columns: []dataconnector.Column{{Name: "amount", DataTypeOID: 20}},
		Rows:    [][]any{{strings.Repeat("materialized", 128)}}, RowCount: 1,
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	expires := harness.clock.value.Add(time.Hour)
	if _, _, _, err := harness.store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(context.Background(), control.BudgetSettlement{
		QueryID: sourceReservation.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, bytes.NewReader(encoded), &control.OrdinalMaterializationPublish{CacheKeySHA256: cacheKey, ExpiresAt: &expires}, nil); err != nil {
		t.Fatal(err)
	}

	replayReservation := reserve("query-v4-chunked-replay", "request-v4-chunked-replay")
	task, err := harness.store.GetTask(context.Background(), "task-v4-chunked-replay")
	if err != nil {
		t.Fatal(err)
	}
	result, replayOutcome, err := harness.service.tryOrdinalSemanticReplay(context.Background(), task,
		"request-v4-chunked-replay", replayReservation.QueryID, grantDigest, cacheKey,
		dictionarySetDigest, replayReservation, map[string]float64{})
	if err != nil || replayOutcome != ordinalReplayCompleted || result["semantic_replay"] != true {
		t.Fatalf("chunked semantic replay outcome=%v result=%#v err=%v", replayOutcome, result, err)
	}
	var sourceFormat, replayFormat string
	if err := harness.store.DB().QueryRowContext(context.Background(), `SELECT
 (SELECT storage_format FROM encrypted_query_results WHERE query_id=$1),
 (SELECT storage_format FROM encrypted_query_results WHERE query_id=$2)`, sourceReservation.QueryID,
		replayReservation.QueryID).Scan(&sourceFormat, &replayFormat); err != nil {
		t.Fatal(err)
	}
	if sourceFormat != "chunked-aes-gcm-v1" || replayFormat != "chunked-aes-gcm-v1" {
		t.Fatalf("semantic replay formats source=%q replay=%q", sourceFormat, replayFormat)
	}
	if len(harness.connector.requests) != 0 {
		t.Fatalf("semantic replay executed Business connector %d times", len(harness.connector.requests))
	}
}

func TestEncryptedQuerySpoolStaysInMemoryAtThreshold(t *testing.T) {
	base := t.TempDir()
	spool, err := newEncryptedQuerySpool(base, "task", "query", 16)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	payload := []byte("0123456789abcdef")
	if _, err := spool.Write(payload); err != nil {
		t.Fatal(err)
	}
	if spool.Spilled() {
		t.Fatal("payload at threshold unexpectedly spilled")
	}
	actual, err := spool.Bytes()
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("Bytes = %q, %v", actual, err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("in-memory spool left filesystem entries: %v, %v", entries, err)
	}
}

func TestEncryptedQuerySpoolCrossingThresholdAuthenticatesAndCleans(t *testing.T) {
	base := t.TempDir()
	spool, err := newEncryptedQuerySpool(base, "task", "query", 15)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), querySpoolChunkSize/16+3)
	for offset := 0; offset < len(payload); {
		end := offset + 7919
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := spool.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if !spool.Spilled() || spool.chunks != 1 {
		t.Fatalf("spilled=%v chunks=%d before seal", spool.Spilled(), spool.chunks)
	}
	reader, err := spool.Open()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("spool round trip length=%d err=%v", len(actual), err)
	}
	path, directory := spool.path, spool.dir
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ciphertext file remains after cleanup: %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("private directory remains after cleanup: %v", err)
	}
}

func TestEncryptedQuerySpoolRejectsTampering(t *testing.T) {
	spool, err := newEncryptedQuerySpool(t.TempDir(), "task", "query", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if _, err := spool.Write([]byte("sensitive result")); err != nil {
		t.Fatal(err)
	}
	if err := spool.Seal(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(spool.path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	position := info.Size() - 1
	last := []byte{0}
	if _, err := file.ReadAt(last, position); err != nil {
		t.Fatal(err)
	}
	last[0] ^= 0xff
	if _, err := file.WriteAt(last, position); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := spool.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("tampered spool was accepted")
	}
	_ = reader.Close()
}

func TestWriteStoredQueryResultMatchesJSONMarshal(t *testing.T) {
	tests := []storedQueryResult{
		{},
		{
			Columns:  []dataconnector.Column{{Name: "amount", DataTypeOID: 1700}},
			Rows:     [][]any{{json.Number("12.50"), "酒店", nil}, {true, []byte{0, 1}, "line\nbreak"}},
			RowCount: 2, DatabaseMS: 7,
			ComponentMS: map[string]float64{"z": 3.5, "a": 1}, Limited: true,
		},
		{Columns: []dataconnector.Column{}, Rows: [][]any{}, ComponentMS: map[string]float64{}},
	}
	for index, test := range tests {
		var actual bytes.Buffer
		if err := writeStoredQueryResult(&actual, test); err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		expected, err := json.Marshal(test)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual.Bytes(), expected) {
			t.Fatalf("case %d:\nactual   %s\nexpected %s", index, actual.Bytes(), expected)
		}
	}
}

func TestDecodeStoredQueryResultPreservesNumbersAndRejectsTrailingJSON(t *testing.T) {
	encoded := []byte(`{"columns":[],"rows":[[12.50,9007199254740993]],"row_count":1,"database_ms":0,"limited":false}`)
	decoded, err := decodeStoredQueryResult(append(append([]byte(nil), encoded...), '\n', ' ', '\t'))
	if err != nil {
		t.Fatal(err)
	}
	assertExactJSONRows(t, decoded.Rows, `[[12.50,9007199254740993]]`)
	for _, trailing := range []string{`{}`, `null`, `garbage`} {
		if _, err := decodeStoredQueryResult(append(append([]byte(nil), encoded...), []byte(trailing)...)); err == nil {
			t.Fatalf("accepted trailing JSON %q", trailing)
		}
	}
}

func assertExactJSONRows(t *testing.T, rows any, want string) {
	t.Helper()
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("row JSON = %s, want %s", encoded, want)
	}
}

func TestEncryptedQuerySpoolUsesPrivateModes(t *testing.T) {
	spool, err := newEncryptedQuerySpool(t.TempDir(), "task", "query", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if _, err := spool.Write([]byte("spill")); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Stat(spool.dir)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(filepath.Join(spool.dir, "payload.spool"))
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 || file.Mode().Perm() != 0o600 {
		t.Fatalf("spool modes directory=%o file=%o", directory.Mode().Perm(), file.Mode().Perm())
	}
}
