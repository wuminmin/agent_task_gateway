package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestResultArtifactLifecycleIsIdempotentAndAudited(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 123456000, time.UTC)
	cipher := testCipher(t, 61)
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(fixedClock{value: now}))
	artifact := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "lifecycle", now.Add(-time.Hour))

	insertResultArtifact(t, store, artifact, true)
	insertResultArtifact(t, store, artifact, false)

	byID, err := store.GetResultArtifact(ctx, artifact.ResultID)
	if err != nil || byID.Status != ResultArtifactPending || byID.ConsumedAt != nil || byID.DeletedAt != nil {
		t.Fatalf("GetResultArtifact = %+v, %v", byID, err)
	}
	byQuery, err := store.GetResultArtifactByQuery(ctx, artifact.QueryID)
	if err != nil || !sameResultArtifactEvidence(byID, byQuery) {
		t.Fatalf("GetResultArtifactByQuery = %+v, %v", byQuery, err)
	}
	pending, err := store.ListPendingResultArtifacts(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ResultID != artifact.ResultID {
		t.Fatalf("ListPendingResultArtifacts = %+v, %v", pending, err)
	}

	available, err := store.MarkResultArtifactAvailable(ctx, artifact.ResultID, "canonical-etag", "gateway")
	if err != nil || available.Status != ResultArtifactAvailable || available.ObjectETag != "canonical-etag" ||
		available.ConsumedAt == nil || !available.ConsumedAt.Equal(now) || available.DeletedAt != nil {
		t.Fatalf("MarkResultArtifactAvailable = %+v, %v", available, err)
	}
	consumedAt := *available.ConsumedAt
	replayed, err := store.MarkResultArtifactAvailable(ctx, artifact.ResultID, "canonical-etag", "retrying-gateway")
	if err != nil || replayed.ConsumedAt == nil || !replayed.ConsumedAt.Equal(consumedAt) {
		t.Fatalf("available replay = %+v, %v", replayed, err)
	}
	if _, err := store.MarkResultArtifactAvailable(ctx, artifact.ResultID, "different-etag", "gateway"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different canonical ETag = %v, want conflict", err)
	}
	pending, err = store.ListPendingResultArtifacts(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending artifacts after consumption = %+v, %v", pending, err)
	}

	deleting, err := store.MarkResultArtifactDeleting(ctx, artifact.ResultID, "retention")
	if err != nil || deleting.Status != ResultArtifactDeleting || deleting.ConsumedAt == nil || deleting.DeletedAt != nil {
		t.Fatalf("MarkResultArtifactDeleting = %+v, %v", deleting, err)
	}
	if _, err := store.SetResultRetentionHold(ctx, artifact.TaskID, "too late", "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("hold after deletion claim = %v, want conflict", err)
	}
	replayed, err = store.MarkResultArtifactDeleting(ctx, artifact.ResultID, "retention-retry")
	if err != nil || replayed.Status != ResultArtifactDeleting {
		t.Fatalf("deleting replay = %+v, %v", replayed, err)
	}
	deleted, err := store.MarkResultArtifactDeleted(ctx, artifact.ResultID, "retention")
	if err != nil || deleted.Status != ResultArtifactDeleted || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(now) {
		t.Fatalf("MarkResultArtifactDeleted = %+v, %v", deleted, err)
	}
	replayed, err = store.MarkResultArtifactDeleted(ctx, artifact.ResultID, "retention-retry")
	if err != nil || replayed.DeletedAt == nil || !replayed.DeletedAt.Equal(*deleted.DeletedAt) {
		t.Fatalf("deleted replay = %+v, %v", replayed, err)
	}
	if _, err := store.MarkResultArtifactAvailable(ctx, artifact.ResultID, "canonical-etag", "gateway"); !errors.Is(err, ErrInvalidStateChange) {
		t.Fatalf("deleted to available transition = %v, want invalid state", err)
	}

	for _, eventType := range []string{"QUERY_RESULT_CONSUMED", "QUERY_RESULT_DELETE_STARTED", "QUERY_RESULT_DELETED"} {
		events, err := store.ListAuditEvents(ctx, AuditFilter{QueryID: artifact.QueryID, EventType: eventType, Limit: 10})
		if err != nil || len(events) != 1 {
			t.Fatalf("%s events = %d, %v", eventType, len(events), err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE result_artifacts SET row_count=row_count+1 WHERE result_id=$1`, artifact.ResultID); err == nil {
		t.Fatal("raw artifact evidence mutation unexpectedly succeeded")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM result_artifacts WHERE result_id=$1`, artifact.ResultID); err == nil {
		t.Fatal("raw artifact evidence deletion unexpectedly succeeded")
	}
	if err := store.VerifyAuditChain(ctx); err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
}

func TestResultArtifactRegistrationRejectsDifferentEvidence(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cipher := testCipher(t, 62)
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(fixedClock{value: now}))
	artifact := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "conflict", now.Add(-time.Hour))
	insertResultArtifact(t, store, artifact, true)

	conflict := artifact
	conflict.ObjectSHA256 = strings.Repeat("c", 64)
	tx, err := beginTx(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := insertResultArtifactTx(context.Background(), tx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different evidence = %v, want conflict", err)
	}

	invalid := artifact
	invalid.ResultID = "res_invalid"
	invalid.QueryID = "query_missing"
	invalid.SchemaJSON = []byte(`[]`)
	tx2, err := beginTx(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx2)
	if _, err := insertResultArtifactTx(context.Background(), tx2, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("schema/column mismatch = %v, want invalid", err)
	}
}

func TestArtifactFinalizationReusesOriginalRegistrationAuditWhenReceiptIsRecovered(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 123456000, time.UTC)
	cipher := testCipher(t, 68)
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(fixedClock{value: now}))
	artifact := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "receipt_recovery", now)
	settlement := BudgetSettlement{QueryID: artifact.QueryID, Rows: artifact.RowCount, DBMS: 7, ObservedDBMS: 7}

	first, _, _, err := store.FinalizeQueryArtifactMeasuredWithReceipt(ctx, settlement, artifact, nil)
	if err != nil {
		t.Fatalf("initial artifact finalization: %v", err)
	}
	var captured QueryReceipt
	builderCalls := 0
	builder := func(evidence QueryReceipt) (SaveQueryReceiptRequest, error) {
		builderCalls++
		captured = evidence
		return SaveQueryReceiptRequest{
			QueryID: evidence.Query.ID, Version: "8", GatewayKeyID: "test-gateway-key",
			Signature: "test-signature", SignedAt: now,
			TerminalAuditSequence: evidence.Audit.Sequence, TerminalAuditHash: evidence.Audit.CurrentHash,
			ReceiptJSON: []byte(`{"version":"8"}`),
		}, nil
	}
	replayed, _, _, err := store.FinalizeQueryArtifactMeasuredWithReceipt(ctx, settlement, artifact, builder)
	if err != nil {
		t.Fatalf("receipt recovery finalization: %v", err)
	}
	if replayed.ID != first.ID || builderCalls != 1 {
		t.Fatalf("recovery replay = %+v, builder calls = %d", replayed, builderCalls)
	}
	normalizedArtifact, normalizeErr := normalizeResultArtifact(artifact)
	if normalizeErr != nil {
		t.Fatal(normalizeErr)
	}
	if captured.Artifact == nil || captured.ArtifactRegistrationAudit == nil ||
		!sameResultArtifactEvidence(*captured.Artifact, normalizedArtifact) {
		t.Fatalf("recovered receipt evidence is incomplete: %+v", captured)
	}
	registration := captured.ArtifactRegistrationAudit
	if registration.EventType != "QUERY_RESULT_OBJECT_REGISTERED" ||
		registration.Sequence != captured.Audit.Sequence+1 || registration.PreviousHash != captured.Audit.CurrentHash {
		t.Fatalf("recovered registration audit does not follow terminal audit: %+v", registration)
	}
	events, err := store.ListAuditEvents(ctx, AuditFilter{
		QueryID: artifact.QueryID, EventType: "QUERY_RESULT_OBJECT_REGISTERED", Limit: 10,
	})
	if err != nil || len(events) != 1 || events[0].CurrentHash != registration.CurrentHash {
		t.Fatalf("registration audits after recovery = %+v, %v", events, err)
	}
	payload := string(events[0].Payload)
	if strings.Contains(payload, artifact.ObjectKey) || strings.Contains(payload, artifact.StagingKey) ||
		!strings.Contains(payload, `"object_key_sha256"`) || !strings.Contains(payload, `"schema_sha256"`) ||
		!strings.Contains(payload, `"result_metadata_sha256":"`+plaintextHash(normalizedArtifact.ResultMetadataJSON)+`"`) ||
		!strings.Contains(payload, `"parquet_size"`) {
		t.Fatalf("registration payload leaks keys or omits complete intent projection: %s", payload)
	}
}

func TestListPendingResultArtifactsAfterDoesNotLetAnOldFailureStarveLaterRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cipher := testCipher(t, 67)
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(fixedClock{value: now}))
	for _, suffix := range []string{"c", "a", "b"} {
		artifact := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "page_"+suffix, now)
		artifact.ResultID = "res_page_" + suffix
		insertResultArtifact(t, store, artifact, true)
	}

	first, err := store.ListPendingResultArtifactsAfter(ctx, "", 2)
	if err != nil || len(first) != 2 || first[0].ResultID != "res_page_a" || first[1].ResultID != "res_page_b" {
		t.Fatalf("first pending page = %+v, %v", first, err)
	}
	second, err := store.ListPendingResultArtifactsAfter(ctx, first[len(first)-1].ResultID, 2)
	if err != nil || len(second) != 1 || second[0].ResultID != "res_page_c" {
		t.Fatalf("second pending page = %+v, %v", second, err)
	}
}

func TestListResultArtifactsForDeletionUsesCreationCutoffAndLegalHolds(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	cipher := testCipher(t, 63)
	store := openTestStore(t, testpostgres.SchemaDSN(t), cipher, WithClock(fixedClock{value: now}))
	old := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "retention_old", now.Add(-48*time.Hour))
	held := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "retention_held", now.Add(-48*time.Hour+time.Second))
	fresh := reserveAndBuildResultArtifact(t, store, cipher.KeyID(), "retention_fresh", now.Add(-time.Hour))
	for _, artifact := range []ResultArtifact{old, held, fresh} {
		insertResultArtifact(t, store, artifact, true)
		if _, err := store.MarkResultArtifactAvailable(ctx, artifact.ResultID, "canonical-"+artifact.ResultID, "gateway"); err != nil {
			t.Fatalf("make %s available: %v", artifact.ResultID, err)
		}
	}
	if _, err := store.MarkResultArtifactDeleting(ctx, old.ResultID, "retention"); err != nil {
		t.Fatalf("mark old artifact deleting: %v", err)
	}
	if _, err := store.SetResultRetentionHold(ctx, held.TaskID, "legal hold", "admin"); err != nil {
		t.Fatalf("SetResultRetentionHold: %v", err)
	}
	if _, err := store.MarkResultArtifactDeleting(ctx, held.ResultID, "retention"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete artifact under active hold = %v, want conflict", err)
	}

	eligible, err := store.ListResultArtifactsForDeletion(ctx, now.Add(-24*time.Hour), 10)
	if err != nil || len(eligible) != 1 || eligible[0].ResultID != old.ResultID ||
		eligible[0].Status != ResultArtifactDeleting {
		t.Fatalf("eligible artifacts = %+v, %v", eligible, err)
	}
	if _, err := store.ClearResultRetentionHold(ctx, held.TaskID, "admin"); err != nil {
		t.Fatalf("ClearResultRetentionHold: %v", err)
	}
	eligible, err = store.ListResultArtifactsForDeletion(ctx, now.Add(-24*time.Hour), 1)
	if err != nil || len(eligible) != 1 || eligible[0].ResultID != old.ResultID {
		t.Fatalf("limited eligible artifacts = %+v, %v", eligible, err)
	}
	eligible, err = store.ListResultArtifactsForDeletion(ctx, now.Add(-24*time.Hour), 10)
	if err != nil || len(eligible) != 2 || eligible[0].ResultID != old.ResultID || eligible[1].ResultID != held.ResultID {
		t.Fatalf("eligible artifacts after hold clear = %+v, %v", eligible, err)
	}
	if _, err := store.ListResultArtifactsForDeletion(ctx, time.Time{}, 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero retention cutoff = %v, want invalid", err)
	}
}

func reserveAndBuildResultArtifact(t *testing.T, store *Store, keyID, suffix string, created time.Time) ResultArtifact {
	t.Helper()
	ctx := context.Background()
	taskID := "task_artifact_" + suffix
	queryID := "query_artifact_" + suffix
	expires := created.Add(7 * 24 * time.Hour)
	if !expires.After(store.now()) {
		expires = store.now().Add(7 * 24 * time.Hour)
	}
	createAwaitingApprovalTask(t, store, taskID, expires)
	approveTask(t, store, taskID, expires, BudgetLimits{Queries: 2, Rows: 20, DBMS: 2000})
	if _, err := store.ReserveBudget(ctx, testReserveRequest(ReserveRequest{
		QueryID: queryID, TaskID: taskID, RequestID: "request-artifact-" + suffix,
		Actor: "alice", RequestDigest: "digest-artifact-" + suffix,
		SQLFingerprint: "sql-artifact-" + suffix, RequestedRows: 10, RequestedDBMS: 500,
	})); err != nil {
		t.Fatalf("ReserveBudget(%s): %v", suffix, err)
	}
	artifactExpires := created.Add(30 * 24 * time.Hour)
	return ResultArtifact{
		ResultID: "res_artifact_" + suffix, QueryID: queryID, TaskID: taskID, KeyID: keyID,
		Format: "parquet", Encryption: "chunked-aes-gcm-v1",
		StagingKey: "staging/" + suffix + ".parquet.enc", ObjectKey: "results/" + suffix + ".parquet.enc",
		ObjectETag:    "staging-etag-" + suffix,
		ParquetSHA256: strings.Repeat("a", 64), ObjectSHA256: strings.Repeat("b", 64),
		ParquetSize: 128, ObjectSize: 256, RowCount: 2, ColumnCount: 1,
		SchemaJSON:         []byte(`[ { "name": "amount", "data_type_oid": 1700 } ]`),
		ResultMetadataJSON: []byte(`{"limited":false,"display_columns":["amount"]}`),
		ACLJSON:            []byte(`{"subjects":["alice"]}`),
		Status:             ResultArtifactPending, CreatedAt: created, ExpiresAt: &artifactExpires,
	}
}

func insertResultArtifact(t *testing.T, store *Store, artifact ResultArtifact, wantCreated bool) {
	t.Helper()
	tx, err := beginTx(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	created, err := insertResultArtifactTx(context.Background(), tx, artifact)
	if err != nil || created != wantCreated {
		t.Fatalf("insertResultArtifactTx(%s) = %t, %v; want %t", artifact.ResultID, created, err, wantCreated)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit artifact %s: %v", artifact.ResultID, err)
	}
}
