package control

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const resultArtifactSelect = `SELECT result_id,query_id,task_id,key_id,format,encryption,
staging_key,object_key,object_etag,parquet_sha256,object_sha256,parquet_size,object_size,
row_count,column_count,schema_json,result_metadata_json,acl_json,status,created_at,expires_at,
consumed_at,deleted_at FROM result_artifacts`

// insertResultArtifactTx registers private staging-object evidence in the same
// transaction that settles the query. The row deliberately remains PENDING:
// only successful promotion to the canonical object key is a consumption event.
// An exact retry of the same immutable evidence is idempotent.
func insertResultArtifactTx(ctx context.Context, tx *sql.Tx, artifact ResultArtifact) (bool, error) {
	const op = "save result artifact"
	normalized, err := normalizeResultArtifact(artifact)
	if err != nil {
		return false, opErr(op, ErrInvalid, err)
	}
	artifact = normalized

	existing, err := scanResultArtifact(tx.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE query_id=$1 FOR UPDATE`, artifact.QueryID))
	if err == nil {
		if sameResultArtifactEvidence(existing, artifact) {
			return false, nil
		}
		return false, opErr(op, ErrConflict, fmt.Errorf("query already has different result artifact evidence"))
	}
	if !isNoRows(err) {
		return false, opErr(op, ErrConflict, err)
	}

	var queryTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM query_records WHERE id=$1`, artifact.QueryID).
		Scan(&queryTaskID); err != nil {
		if isNoRows(err) {
			return false, opErr(op, ErrNotFound, err)
		}
		return false, opErr(op, ErrConflict, err)
	}
	if queryTaskID != artifact.TaskID {
		return false, opErr(op, ErrInvalid, fmt.Errorf("artifact task does not match query task"))
	}
	if err := ensureActiveResultEncryptionKeyTx(ctx, tx, artifact.KeyID, artifact.CreatedAt); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO result_artifacts(result_id,query_id,task_id,key_id,format,encryption,staging_key,object_key,
 object_etag,parquet_sha256,object_sha256,parquet_size,object_size,row_count,column_count,schema_json,
 result_metadata_json,acl_json,status,created_at,expires_at,consumed_at,deleted_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NULL,NULL)`,
		artifact.ResultID, artifact.QueryID, artifact.TaskID, artifact.KeyID, artifact.Format,
		artifact.Encryption, artifact.StagingKey, artifact.ObjectKey, artifact.ObjectETag,
		artifact.ParquetSHA256, artifact.ObjectSHA256, artifact.ParquetSize, artifact.ObjectSize,
		artifact.RowCount, artifact.ColumnCount, string(artifact.SchemaJSON),
		string(artifact.ResultMetadataJSON), string(artifact.ACLJSON), artifact.Status,
		dbTime(artifact.CreatedAt), nullableTime(artifact.ExpiresAt))
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	return true, nil
}

// GetResultArtifact returns lifecycle and integrity metadata without accessing
// the object store or exposing any result bytes.
func (s *Store) GetResultArtifact(ctx context.Context, resultID string) (ResultArtifact, error) {
	const op = "get result artifact"
	if err := s.checkOpen(op); err != nil {
		return ResultArtifact{}, err
	}
	resultID = strings.TrimSpace(resultID)
	if resultID == "" {
		return ResultArtifact{}, opErr(op, ErrInvalid, fmt.Errorf("result_id is required"))
	}
	artifact, err := scanResultArtifact(s.db.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE result_id=$1`, resultID))
	if err != nil {
		if isNoRows(err) {
			return ResultArtifact{}, opErr(op, ErrNotFound, err)
		}
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	return artifact, nil
}

// GetResultArtifactByQuery resolves the one immutable artifact registered for a
// query. Both lookup paths return PENDING and tombstoned rows to lifecycle and
// recovery callers; consumers must require Status == AVAILABLE.
func (s *Store) GetResultArtifactByQuery(ctx context.Context, queryID string) (ResultArtifact, error) {
	const op = "get result artifact by query"
	if err := s.checkOpen(op); err != nil {
		return ResultArtifact{}, err
	}
	queryID = strings.TrimSpace(queryID)
	if queryID == "" {
		return ResultArtifact{}, opErr(op, ErrInvalid, fmt.Errorf("query_id is required"))
	}
	artifact, err := scanResultArtifact(s.db.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE query_id=$1`, queryID))
	if err != nil {
		if isNoRows(err) {
			return ResultArtifact{}, opErr(op, ErrNotFound, err)
		}
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	return artifact, nil
}

// GetResultArtifactByStagingKey is used by object garbage collection to prove
// that a private upload is not referenced by a committed Control intent.
func (s *Store) GetResultArtifactByStagingKey(ctx context.Context, stagingKey string) (ResultArtifact, error) {
	const op = "get result artifact by staging key"
	if err := s.checkOpen(op); err != nil {
		return ResultArtifact{}, err
	}
	stagingKey = strings.TrimSpace(stagingKey)
	if stagingKey == "" {
		return ResultArtifact{}, opErr(op, ErrInvalid, fmt.Errorf("staging_key is required"))
	}
	artifact, err := scanResultArtifact(s.db.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE staging_key=$1`, stagingKey))
	if err != nil {
		if isNoRows(err) {
			return ResultArtifact{}, opErr(op, ErrNotFound, err)
		}
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	return artifact, nil
}

// ListPendingResultArtifacts returns deterministic promotion/recovery work.
// A bounded page prevents a restart from loading an unbounded orphan set.
func (s *Store) ListPendingResultArtifacts(ctx context.Context, limit int) ([]ResultArtifact, error) {
	return s.ListPendingResultArtifactsAfter(ctx, "", limit)
}

// ListPendingResultArtifactsAfter pages recovery work by immutable result ID.
// A failed artifact therefore cannot keep later PENDING rows permanently
// hidden behind the first recovery batch.
func (s *Store) ListPendingResultArtifactsAfter(ctx context.Context, afterResultID string, limit int) ([]ResultArtifact, error) {
	const op = "list pending result artifacts"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, resultArtifactSelect+`
 WHERE status=$1 AND result_id > $2 ORDER BY result_id LIMIT $3`,
		ResultArtifactPending, strings.TrimSpace(afterResultID), limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	artifacts := make([]ResultArtifact, 0)
	for rows.Next() {
		artifact, err := scanResultArtifact(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return artifacts, nil
}

// ListResultArtifactsForDeletion returns bounded administrator-selected
// retention work by creation cutoff while honoring active task legal holds.
func (s *Store) ListResultArtifactsForDeletion(ctx context.Context, cutoff time.Time, limit int) ([]ResultArtifact, error) {
	const op = "list result artifacts for deletion"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if cutoff.IsZero() {
		return nil, opErr(op, ErrInvalid, fmt.Errorf("cutoff is required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, resultArtifactSelect+`
 WHERE status IN ($1,$2) AND created_at < $3
   AND NOT EXISTS (
     SELECT 1 FROM result_retention_holds hold
     WHERE hold.task_id=result_artifacts.task_id AND hold.released_at IS NULL
   )
 ORDER BY created_at,result_id LIMIT $4`, ResultArtifactAvailable, ResultArtifactDeleting,
		dbTime(cutoff), limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	artifacts := make([]ResultArtifact, 0)
	for rows.Next() {
		artifact, err := scanResultArtifact(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return artifacts, nil
}

// ListExpiredResultArtifacts uses the immutable per-artifact expiry published
// at result creation. Runtime configuration changes therefore cannot shorten
// or silently extend an existing artifact's retention contract.
func (s *Store) ListExpiredResultArtifacts(ctx context.Context, now time.Time, limit int) ([]ResultArtifact, error) {
	const op = "list expired result artifacts"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, opErr(op, ErrInvalid, fmt.Errorf("current time is required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, resultArtifactSelect+`
 WHERE status IN ($1,$2) AND expires_at IS NOT NULL AND expires_at <= $3
   AND NOT EXISTS (
     SELECT 1 FROM result_retention_holds hold
     WHERE hold.task_id=result_artifacts.task_id AND hold.released_at IS NULL
   )
 ORDER BY expires_at,result_id LIMIT $4`, ResultArtifactAvailable, ResultArtifactDeleting,
		dbTime(now), limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	artifacts := make([]ResultArtifact, 0)
	for rows.Next() {
		artifact, err := scanResultArtifact(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return artifacts, nil
}

// MarkResultArtifactAvailable records canonical object promotion. This is the
// consumption boundary: budget settlement was already committed with PENDING
// metadata, while no Agent needs to prove a subsequent byte download.
func (s *Store) MarkResultArtifactAvailable(ctx context.Context, resultID, canonicalETag, actor string) (ResultArtifact, error) {
	const op = "mark result artifact available"
	if err := s.checkOpen(op); err != nil {
		return ResultArtifact{}, err
	}
	resultID = strings.TrimSpace(resultID)
	canonicalETag = strings.TrimSpace(canonicalETag)
	actor = strings.TrimSpace(actor)
	if resultID == "" || canonicalETag == "" || actor == "" {
		return ResultArtifact{}, opErr(op, ErrInvalid, fmt.Errorf("result_id, canonical_etag, and actor are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	artifact, err := scanResultArtifact(tx.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE result_id=$1 FOR UPDATE`, resultID))
	if err != nil {
		return ResultArtifact{}, resultArtifactLookupError(op, err)
	}
	if artifact.Status == ResultArtifactAvailable {
		if artifact.ObjectETag != canonicalETag || artifact.ConsumedAt == nil {
			return ResultArtifact{}, opErr(op, ErrConflict, fmt.Errorf("available artifact evidence is inconsistent"))
		}
		if err := tx.Commit(); err != nil {
			return ResultArtifact{}, opErr(op, ErrConflict, err)
		}
		return artifact, nil
	}
	if artifact.Status != ResultArtifactPending {
		return ResultArtifact{}, invalidResultArtifactTransition(op, artifact.Status, ResultArtifactAvailable)
	}
	// Serialize canonical consumption with administrative key erasure. Key
	// erasure refuses outstanding PENDING artifacts, and this shared lock keeps
	// the key active through the AVAILABLE commit.
	if err := ensureActiveResultEncryptionKeyTx(ctx, tx, artifact.KeyID, artifact.CreatedAt); err != nil {
		return ResultArtifact{}, err
	}
	now := s.now()
	artifact, err = scanResultArtifact(tx.QueryRowContext(ctx, `
UPDATE result_artifacts
SET status=$2,object_etag=$3,consumed_at=COALESCE(consumed_at,$4)
WHERE result_id=$1 AND status=$5
RETURNING result_id,query_id,task_id,key_id,format,encryption,staging_key,object_key,object_etag,
 parquet_sha256,object_sha256,parquet_size,object_size,row_count,column_count,schema_json,
 result_metadata_json,acl_json,status,created_at,expires_at,consumed_at,deleted_at`,
		resultID, ResultArtifactAvailable, canonicalETag, dbTime(now), ResultArtifactPending))
	if err != nil {
		return ResultArtifact{}, resultArtifactUpdateError(op, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: artifact.TaskID, QueryID: artifact.QueryID, Actor: actor,
		EventType: "QUERY_RESULT_CONSUMED", OccurredAt: now,
		Payload: mustJSON(map[string]any{
			"result_id": artifact.ResultID, "result_sha256": artifact.ParquetSHA256,
			"object_sha256": artifact.ObjectSHA256, "format": artifact.Format,
			"status": artifact.Status, "consumed_at": artifact.ConsumedAt,
		}),
	})
	if err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	return artifact, nil
}

// MarkResultArtifactDeleting claims an AVAILABLE artifact for object-store
// deletion. It is safe to retry and does not erase the audit or query evidence.
func (s *Store) MarkResultArtifactDeleting(ctx context.Context, resultID, actor string) (ResultArtifact, error) {
	const op = "mark result artifact deleting"
	return s.transitionResultArtifact(ctx, op, resultID, actor, ResultArtifactAvailable,
		ResultArtifactDeleting, "QUERY_RESULT_DELETE_STARTED", false)
}

// MarkResultArtifactDeleted tombstones metadata only after the canonical object
// has been removed. The row remains available for receipts and reconciliation.
func (s *Store) MarkResultArtifactDeleted(ctx context.Context, resultID, actor string) (ResultArtifact, error) {
	const op = "mark result artifact deleted"
	return s.transitionResultArtifact(ctx, op, resultID, actor, ResultArtifactDeleting,
		ResultArtifactDeleted, "QUERY_RESULT_DELETED", true)
}

func (s *Store) transitionResultArtifact(ctx context.Context, op, resultID, actor string,
	from, to ResultArtifactStatus, eventType string, setDeletedAt bool) (ResultArtifact, error) {
	if err := s.checkOpen(op); err != nil {
		return ResultArtifact{}, err
	}
	resultID = strings.TrimSpace(resultID)
	actor = strings.TrimSpace(actor)
	if resultID == "" || actor == "" {
		return ResultArtifact{}, opErr(op, ErrInvalid, fmt.Errorf("result_id and actor are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	artifact, err := scanResultArtifact(tx.QueryRowContext(ctx,
		resultArtifactSelect+` WHERE result_id=$1 FOR UPDATE`, resultID))
	if err != nil {
		return ResultArtifact{}, resultArtifactLookupError(op, err)
	}
	if artifact.Status == to {
		if setDeletedAt && artifact.DeletedAt == nil {
			return ResultArtifact{}, opErr(op, ErrConflict, fmt.Errorf("deleted artifact is missing deleted_at"))
		}
		if err := tx.Commit(); err != nil {
			return ResultArtifact{}, opErr(op, ErrConflict, err)
		}
		return artifact, nil
	}
	if artifact.Status != from {
		return ResultArtifact{}, invalidResultArtifactTransition(op, artifact.Status, to)
	}
	if to == ResultArtifactDeleting {
		// Serialize the deletion claim with legal-hold creation on the task row.
		// Once DELETING is committed, SetResultRetentionHold rejects the task so
		// an administrator cannot receive a misleading successful hold while an
		// object-store delete is already in flight.
		if err := lockTaskForRetentionTx(ctx, tx, artifact.TaskID); err != nil {
			return ResultArtifact{}, opErr(op, retentionTaskLookupErrorKind(err), err)
		}
		var held bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM result_retention_holds WHERE task_id=$1 AND released_at IS NULL
)`, artifact.TaskID).Scan(&held); err != nil {
			return ResultArtifact{}, opErr(op, ErrConflict, err)
		}
		if held {
			return ResultArtifact{}, opErr(op, ErrConflict, fmt.Errorf("result task is under an active retention hold"))
		}
	}
	now := s.now()
	if setDeletedAt {
		artifact, err = scanResultArtifact(tx.QueryRowContext(ctx, `
UPDATE result_artifacts SET status=$2,deleted_at=COALESCE(deleted_at,$3)
WHERE result_id=$1 AND status=$4
RETURNING result_id,query_id,task_id,key_id,format,encryption,staging_key,object_key,object_etag,
 parquet_sha256,object_sha256,parquet_size,object_size,row_count,column_count,schema_json,
 result_metadata_json,acl_json,status,created_at,expires_at,consumed_at,deleted_at`,
			resultID, to, dbTime(now), from))
	} else {
		artifact, err = scanResultArtifact(tx.QueryRowContext(ctx, `
UPDATE result_artifacts SET status=$2
WHERE result_id=$1 AND status=$3
RETURNING result_id,query_id,task_id,key_id,format,encryption,staging_key,object_key,object_etag,
 parquet_sha256,object_sha256,parquet_size,object_size,row_count,column_count,schema_json,
 result_metadata_json,acl_json,status,created_at,expires_at,consumed_at,deleted_at`,
			resultID, to, from))
	}
	if err != nil {
		return ResultArtifact{}, resultArtifactUpdateError(op, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: artifact.TaskID, QueryID: artifact.QueryID, Actor: actor,
		EventType: eventType, OccurredAt: now,
		Payload: mustJSON(map[string]any{
			"result_id": artifact.ResultID, "result_sha256": artifact.ParquetSHA256,
			"from": from, "to": artifact.Status,
		}),
	})
	if err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ResultArtifact{}, opErr(op, ErrConflict, err)
	}
	return artifact, nil
}

func normalizeResultArtifact(artifact ResultArtifact) (ResultArtifact, error) {
	if artifact.Status == "" {
		artifact.Status = ResultArtifactPending
	}
	if strings.TrimSpace(artifact.ResultID) == "" || artifact.ResultID != strings.TrimSpace(artifact.ResultID) ||
		strings.TrimSpace(artifact.QueryID) == "" || artifact.QueryID != strings.TrimSpace(artifact.QueryID) ||
		strings.TrimSpace(artifact.TaskID) == "" || artifact.TaskID != strings.TrimSpace(artifact.TaskID) ||
		strings.TrimSpace(artifact.StagingKey) == "" || strings.TrimSpace(artifact.ObjectKey) == "" ||
		artifact.StagingKey == artifact.ObjectKey {
		return ResultArtifact{}, fmt.Errorf("result, query, task, staging, and canonical object keys are required")
	}
	keyID, err := normalizeResultEncryptionKeyID(artifact.KeyID)
	if err != nil {
		return ResultArtifact{}, err
	}
	artifact.KeyID = keyID
	if artifact.Format != "parquet" || artifact.Encryption != "chunked-aes-gcm-v1" ||
		strings.TrimSpace(artifact.ObjectETag) == "" ||
		!validSHA256Hex(artifact.ParquetSHA256) || !validSHA256Hex(artifact.ObjectSHA256) ||
		artifact.ParquetSize < 0 || artifact.ObjectSize <= 0 || artifact.RowCount < 0 || artifact.ColumnCount <= 0 ||
		artifact.Status != ResultArtifactPending || artifact.CreatedAt.IsZero() ||
		artifact.ConsumedAt != nil || artifact.DeletedAt != nil {
		return ResultArtifact{}, fmt.Errorf("invalid pending result artifact evidence")
	}
	artifact.CreatedAt = dbTime(artifact.CreatedAt)
	if artifact.ExpiresAt != nil {
		if artifact.ExpiresAt.IsZero() {
			return ResultArtifact{}, fmt.Errorf("artifact expiry is invalid")
		}
		expires := dbTime(*artifact.ExpiresAt)
		if !expires.After(artifact.CreatedAt) {
			return ResultArtifact{}, fmt.Errorf("artifact expiry must follow creation")
		}
		artifact.ExpiresAt = &expires
	}
	artifact.SchemaJSON, err = normalizeJSON(artifact.SchemaJSON, `[]`)
	if err != nil {
		return ResultArtifact{}, fmt.Errorf("invalid artifact schema: %w", err)
	}
	var schema []json.RawMessage
	if err := json.Unmarshal(artifact.SchemaJSON, &schema); err != nil || len(schema) != artifact.ColumnCount {
		return ResultArtifact{}, fmt.Errorf("artifact schema does not match column count")
	}
	artifact.ResultMetadataJSON, err = normalizeJSON(artifact.ResultMetadataJSON, `{}`)
	if err != nil {
		return ResultArtifact{}, fmt.Errorf("invalid result metadata: %w", err)
	}
	artifact.ACLJSON, err = normalizeJSON(artifact.ACLJSON, `{}`)
	if err != nil {
		return ResultArtifact{}, fmt.Errorf("invalid artifact ACL: %w", err)
	}
	return artifact, nil
}

func sameResultArtifactEvidence(left, right ResultArtifact) bool {
	return left.ResultID == right.ResultID && left.QueryID == right.QueryID && left.TaskID == right.TaskID &&
		left.KeyID == right.KeyID && left.Format == right.Format && left.Encryption == right.Encryption &&
		left.StagingKey == right.StagingKey && left.ObjectKey == right.ObjectKey &&
		left.ObjectETag == right.ObjectETag &&
		subtle.ConstantTimeCompare([]byte(left.ParquetSHA256), []byte(right.ParquetSHA256)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.ObjectSHA256), []byte(right.ObjectSHA256)) == 1 &&
		left.ParquetSize == right.ParquetSize && left.ObjectSize == right.ObjectSize &&
		left.RowCount == right.RowCount && left.ColumnCount == right.ColumnCount &&
		bytes.Equal(left.SchemaJSON, right.SchemaJSON) && bytes.Equal(left.ResultMetadataJSON, right.ResultMetadataJSON) &&
		bytes.Equal(left.ACLJSON, right.ACLJSON) && left.CreatedAt.Equal(right.CreatedAt) &&
		equalArtifactTime(left.ExpiresAt, right.ExpiresAt)
}

func equalArtifactTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func scanResultArtifact(row rowScanner) (ResultArtifact, error) {
	var artifact ResultArtifact
	var schema, metadata, acl []byte
	var status string
	var created time.Time
	var expires, consumed, deleted sql.NullTime
	err := row.Scan(&artifact.ResultID, &artifact.QueryID, &artifact.TaskID, &artifact.KeyID,
		&artifact.Format, &artifact.Encryption, &artifact.StagingKey, &artifact.ObjectKey,
		&artifact.ObjectETag, &artifact.ParquetSHA256, &artifact.ObjectSHA256,
		&artifact.ParquetSize, &artifact.ObjectSize, &artifact.RowCount, &artifact.ColumnCount,
		&schema, &metadata, &acl, &status, &created, &expires, &consumed, &deleted)
	if err != nil {
		return ResultArtifact{}, err
	}
	artifact.SchemaJSON, err = normalizeJSON(schema, `[]`)
	if err != nil {
		return ResultArtifact{}, err
	}
	artifact.ResultMetadataJSON, err = normalizeJSON(metadata, `{}`)
	if err != nil {
		return ResultArtifact{}, err
	}
	artifact.ACLJSON, err = normalizeJSON(acl, `{}`)
	if err != nil {
		return ResultArtifact{}, err
	}
	artifact.Status = ResultArtifactStatus(status)
	artifact.CreatedAt = dbTime(created)
	artifact.ExpiresAt = scanNullableTime(expires)
	artifact.ConsumedAt = scanNullableTime(consumed)
	artifact.DeletedAt = scanNullableTime(deleted)
	return artifact, nil
}

func resultArtifactLookupError(op string, err error) error {
	if isNoRows(err) {
		return opErr(op, ErrNotFound, err)
	}
	return opErr(op, ErrConflict, err)
}

func resultArtifactUpdateError(op string, err error) error {
	if isNoRows(err) {
		return opErr(op, ErrConflict, fmt.Errorf("artifact lifecycle changed concurrently"))
	}
	return opErr(op, ErrConflict, err)
}

func invalidResultArtifactTransition(op string, from, to ResultArtifactStatus) error {
	return opErr(op, ErrInvalidStateChange, fmt.Errorf("cannot transition result artifact from %s to %s", from, to))
}
