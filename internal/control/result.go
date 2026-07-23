package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func resultAAD(taskID, queryID string) []byte {
	return []byte("taskbound-result-v1\x00" + taskID + "\x00" + queryID)
}

func plaintextHash(plaintext []byte) string {
	digest := sha256.Sum256(plaintext)
	return hex.EncodeToString(digest[:])
}

func resultCipherKeyID(cipher ResultCipher) (string, error) {
	if keyed, ok := cipher.(ResultCipherKeyer); ok {
		return normalizeResultEncryptionKeyID(keyed.KeyID())
	}
	return DefaultResultEncryptionKeyID, nil
}

func normalizeResultEncryptionKeyID(keyID string) (string, error) {
	trimmed := strings.TrimSpace(keyID)
	if trimmed == "" {
		return "", fmt.Errorf("result encryption key id is required")
	}
	if trimmed != keyID {
		return "", fmt.Errorf("result encryption key id must not have surrounding whitespace")
	}
	return keyID, nil
}

// FinalizeQuery atomically settles budget usage and stores the encrypted
// result. It is the preferred success path for query execution.
func (s *Store) FinalizeQuery(ctx context.Context, settlement BudgetSettlement, plaintext []byte) (QueryRecord, error) {
	record, _, err := s.FinalizeQueryMeasured(ctx, settlement, plaintext)
	return record, err
}

// FinalizeQueryMetrics separates local authenticated encryption from the
// Control PostgreSQL settlement-and-persistence transaction for evaluation.
type FinalizeQueryMetrics struct {
	Encryption      time.Duration
	ReceiptSigning  time.Duration
	SettlementStore time.Duration
}

// FinalizeQueryMeasured is the measured form of FinalizeQuery. The returned
// timings are observational only and do not alter the atomic transaction.
func (s *Store) FinalizeQueryMeasured(ctx context.Context, settlement BudgetSettlement, plaintext []byte) (QueryRecord, FinalizeQueryMetrics, error) {
	record, _, metrics, err := s.FinalizeQueryMeasuredWithReceipt(ctx, settlement, plaintext, nil)
	return record, metrics, err
}

// FinalizeQueryMeasuredWithReceipt atomically settles budget usage, stores the
// encrypted result, appends terminal audit evidence, and persists the signed
// query receipt in one Control PG transaction.
func (s *Store) FinalizeQueryMeasuredWithReceipt(ctx context.Context, settlement BudgetSettlement, plaintext []byte, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, FinalizeQueryMetrics, error) {
	const op = "finalize query"
	var metrics FinalizeQueryMetrics
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, err
	}
	if s.cipher == nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrCipherUnavailable, nil)
	}
	if settlement.QueryID == "" || settlement.Rows < 0 || settlement.DBMS < 0 || settlement.ObservedDBMS < 0 {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrInvalid, fmt.Errorf("invalid settlement"))
	}
	current, err := s.GetQuery(ctx, settlement.QueryID)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, err
	}
	if current.Status == QueryReleased || current.Status == QueryInterrupted {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrReservationNotFound, fmt.Errorf("query is %s", current.Status))
	}
	keyID, err := resultCipherKeyID(s.cipher)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrInvalid, err)
	}
	encryptionStarted := time.Now()
	hash := plaintextHash(plaintext)
	nonce, ciphertext, err := s.cipher.Encrypt(plaintext, resultAAD(current.TaskID, current.ID))
	metrics.Encryption = time.Since(encryptionStarted)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrCipherUnavailable, err)
	}
	settlementStarted := time.Now()
	now := s.now()
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	record, audit, err := settleBudgetTx(ctx, tx, now, settlement, QueryCompleted, hash)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, settlementErrorKind(err), err)
	}
	created, err := insertEncryptedResultTx(ctx, tx, EncryptedResult{
		QueryID: current.ID, TaskID: current.TaskID, KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext, SHA256: hash, CreatedAt: now,
	})
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, err
	}
	if record.ResultSHA256 != "" && record.ResultSHA256 != hash {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrConflict, fmt.Errorf("query already finalized with a different result"))
	}
	if record.ResultSHA256 == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE query_records SET result_sha256=$1 WHERE id=$2`, hash, record.ID); err != nil {
			return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrConflict, err)
		}
		record.ResultSHA256 = hash
	}
	if created {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			TaskID: record.TaskID, QueryID: record.ID, Actor: record.Actor, EventType: "QUERY_RESULT_STORED",
			Payload: mustJSON(map[string]any{"result_sha256": hash, "cipher": "AES-256-GCM", "key_id": keyID}), OccurredAt: now,
		})
		if err != nil {
			return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrConflict, err)
		}
	}
	var receipt PersistedQueryReceipt
	if builder != nil {
		signingStarted := time.Now()
		receipt, err = persistTerminalReceiptTx(ctx, tx, now, QueryReceipt{Query: record, Audit: audit}, builder)
		metrics.ReceiptSigning = time.Since(signingStarted)
		if err != nil {
			return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, receiptErrorKind(err), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrConflict, err)
	}
	metrics.SettlementStore = time.Since(settlementStarted)
	return record, receipt, metrics, nil
}

// SaveEncryptedResult stores a result for an already completed query. Prefer
// FinalizeQuery when settlement and storage can happen together.
func (s *Store) SaveEncryptedResult(ctx context.Context, taskID, queryID string, plaintext []byte) (EncryptedResult, error) {
	const op = "save encrypted result"
	if err := s.checkOpen(op); err != nil {
		return EncryptedResult{}, err
	}
	if s.cipher == nil {
		return EncryptedResult{}, opErr(op, ErrCipherUnavailable, nil)
	}
	if taskID == "" || queryID == "" {
		return EncryptedResult{}, opErr(op, ErrInvalid, fmt.Errorf("task and query are required"))
	}
	keyID, err := resultCipherKeyID(s.cipher)
	if err != nil {
		return EncryptedResult{}, opErr(op, ErrInvalid, err)
	}
	nonce, ciphertext, err := s.cipher.Encrypt(plaintext, resultAAD(taskID, queryID))
	if err != nil {
		return EncryptedResult{}, opErr(op, ErrCipherUnavailable, err)
	}
	result := EncryptedResult{QueryID: queryID, TaskID: taskID, KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext,
		SHA256: plaintextHash(plaintext), CreatedAt: s.now()}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return EncryptedResult{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	var queryTask string
	var status QueryStatus
	var existingHash string
	if err := tx.QueryRowContext(ctx, `SELECT task_id, status, result_sha256 FROM query_records WHERE id=$1 FOR UPDATE`, queryID).
		Scan(&queryTask, &status, &existingHash); err != nil {
		if isNoRows(err) {
			return EncryptedResult{}, opErr(op, ErrNotFound, err)
		}
		return EncryptedResult{}, opErr(op, ErrConflict, err)
	}
	if queryTask != taskID {
		return EncryptedResult{}, opErr(op, ErrNotFound, fmt.Errorf("query does not belong to task"))
	}
	if status != QueryCompleted {
		return EncryptedResult{}, opErr(op, ErrInvalid, fmt.Errorf("query is %s", status))
	}
	created, err := insertEncryptedResultTx(ctx, tx, result)
	if err != nil {
		return EncryptedResult{}, err
	}
	if existingHash != "" && existingHash != result.SHA256 {
		return EncryptedResult{}, opErr(op, ErrConflict, fmt.Errorf("query already has a different result hash"))
	}
	if existingHash == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE query_records SET result_sha256=$1 WHERE id=$2`, result.SHA256, queryID); err != nil {
			return EncryptedResult{}, opErr(op, ErrConflict, err)
		}
	}
	if created {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			TaskID: taskID, QueryID: queryID, Actor: "system", EventType: "QUERY_RESULT_STORED",
			Payload: mustJSON(map[string]any{"result_sha256": result.SHA256, "cipher": "AES-256-GCM", "key_id": keyID}), OccurredAt: result.CreatedAt,
		})
		if err != nil {
			return EncryptedResult{}, opErr(op, ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EncryptedResult{}, opErr(op, ErrConflict, err)
	}
	if !created {
		stored, _, err := s.GetEncryptedResult(ctx, taskID, queryID)
		return stored, err
	}
	return result, nil
}

// insertEncryptedResultTx returns true when a row was inserted and false for
// an idempotent replay of the same plaintext hash.
func insertEncryptedResultTx(ctx context.Context, tx *sql.Tx, result EncryptedResult) (bool, error) {
	var existingHash string
	err := tx.QueryRowContext(ctx, `SELECT plaintext_sha256 FROM encrypted_query_results WHERE query_id=$1 FOR UPDATE`, result.QueryID).
		Scan(&existingHash)
	if err == nil {
		if subtle.ConstantTimeCompare([]byte(existingHash), []byte(result.SHA256)) == 1 {
			return false, nil
		}
		return false, opErr("save encrypted result", ErrConflict, fmt.Errorf("different result already stored"))
	}
	if !isNoRows(err) {
		return false, opErr("save encrypted result", ErrConflict, err)
	}
	if err := ensureActiveResultEncryptionKeyTx(ctx, tx, result.KeyID, result.CreatedAt); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO encrypted_query_results(query_id, task_id, key_id, nonce, ciphertext, plaintext_sha256, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, result.QueryID, result.TaskID, result.KeyID, result.Nonce, result.Ciphertext, result.SHA256,
		dbTime(result.CreatedAt))
	if err != nil {
		return false, opErr("save encrypted result", ErrConflict, err)
	}
	return true, nil
}

// GetEncryptedResult returns encrypted metadata and authenticated plaintext.
func (s *Store) GetEncryptedResult(ctx context.Context, taskID, queryID string) (EncryptedResult, []byte, error) {
	const op = "get encrypted result"
	if err := s.checkOpen(op); err != nil {
		return EncryptedResult{}, nil, err
	}
	if s.cipher == nil {
		return EncryptedResult{}, nil, opErr(op, ErrCipherUnavailable, nil)
	}
	var result EncryptedResult
	var created time.Time
	var keyStatus sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT result.query_id, result.task_id, result.key_id, result.nonce, result.ciphertext,
       result.plaintext_sha256, result.created_at, key.status
FROM encrypted_query_results result
LEFT JOIN result_encryption_keys key ON key.key_id = result.key_id
WHERE result.query_id=$1 AND result.task_id=$2`, queryID, taskID).
		Scan(&result.QueryID, &result.TaskID, &result.KeyID, &result.Nonce, &result.Ciphertext, &result.SHA256, &created, &keyStatus)
	if err != nil {
		if isNoRows(err) {
			return EncryptedResult{}, nil, opErr(op, ErrNotFound, err)
		}
		return EncryptedResult{}, nil, opErr(op, ErrConflict, err)
	}
	result.CreatedAt = dbTime(created)
	if !keyStatus.Valid {
		return EncryptedResult{}, nil, opErr(op, ErrCipherUnavailable, fmt.Errorf("result encryption key %q is not registered", result.KeyID))
	}
	if ResultEncryptionKeyStatus(keyStatus.String) == ResultEncryptionKeyErased {
		return EncryptedResult{}, nil, opErr(op, ErrCipherUnavailable, fmt.Errorf("result encryption key %q is erased", result.KeyID))
	}
	if ResultEncryptionKeyStatus(keyStatus.String) != ResultEncryptionKeyActive {
		return EncryptedResult{}, nil, opErr(op, ErrCipherUnavailable, fmt.Errorf("result encryption key %q is %s", result.KeyID, keyStatus.String))
	}
	plaintext, err := s.cipher.Decrypt(result.Nonce, result.Ciphertext, resultAAD(taskID, queryID))
	if err != nil {
		return EncryptedResult{}, nil, opErr(op, ErrCiphertextInvalid, err)
	}
	actualHash := plaintextHash(plaintext)
	if subtle.ConstantTimeCompare([]byte(actualHash), []byte(result.SHA256)) != 1 {
		return EncryptedResult{}, nil, opErr(op, ErrCiphertextInvalid, fmt.Errorf("plaintext digest mismatch"))
	}
	return result, plaintext, nil
}

func ensureActiveResultEncryptionKeyTx(ctx context.Context, tx *sql.Tx, keyID string, createdAt time.Time) error {
	const op = "ensure result encryption key"
	keyID, err := normalizeResultEncryptionKeyID(keyID)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	var status string
	err = tx.QueryRowContext(ctx, `
SELECT status FROM result_encryption_keys WHERE key_id=$1 FOR UPDATE`, keyID).Scan(&status)
	if err == nil {
		if ResultEncryptionKeyStatus(status) == ResultEncryptionKeyActive {
			return nil
		}
		return opErr(op, ErrCipherUnavailable, fmt.Errorf("result encryption key %q is %s", keyID, status))
	}
	if !isNoRows(err) {
		return opErr(op, ErrConflict, err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO result_encryption_keys(key_id, status, created_at, erased_by, erased_at)
VALUES ($1, $2, $3, '', NULL)`, keyID, ResultEncryptionKeyActive, dbTime(createdAt))
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func (s *Store) GetResultEncryptionKey(ctx context.Context, keyID string) (ResultEncryptionKey, error) {
	const op = "get result encryption key"
	if err := s.checkOpen(op); err != nil {
		return ResultEncryptionKey{}, err
	}
	keyID, err := normalizeResultEncryptionKeyID(keyID)
	if err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrInvalid, err)
	}
	key, err := scanResultEncryptionKey(s.db.QueryRowContext(ctx, `
SELECT key_id, status, created_at, erased_at, erased_by
FROM result_encryption_keys
WHERE key_id=$1`, keyID))
	if err != nil {
		if isNoRows(err) {
			return ResultEncryptionKey{}, opErr(op, ErrNotFound, err)
		}
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	return key, nil
}

func (s *Store) EraseResultEncryptionKey(ctx context.Context, keyID, actor string) (ResultEncryptionKey, error) {
	const op = "erase result encryption key"
	if err := s.checkOpen(op); err != nil {
		return ResultEncryptionKey{}, err
	}
	keyID, err := normalizeResultEncryptionKeyID(keyID)
	if err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrInvalid, err)
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return ResultEncryptionKey{}, opErr(op, ErrInvalid, fmt.Errorf("actor is required"))
	}
	now := s.now()
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	key, err := scanResultEncryptionKey(tx.QueryRowContext(ctx, `
SELECT key_id, status, created_at, erased_at, erased_by
FROM result_encryption_keys
WHERE key_id=$1 FOR UPDATE`, keyID))
	if err != nil {
		if isNoRows(err) {
			return ResultEncryptionKey{}, opErr(op, ErrNotFound, err)
		}
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	if key.Status == ResultEncryptionKeyErased {
		if err := tx.Commit(); err != nil {
			return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
		}
		return key, nil
	}
	var affected int64
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM encrypted_query_results WHERE key_id=$1`, keyID).Scan(&affected); err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	key, err = scanResultEncryptionKey(tx.QueryRowContext(ctx, `
UPDATE result_encryption_keys
SET status=$2, erased_at=$3, erased_by=$4
WHERE key_id=$1 AND status=$5
RETURNING key_id, status, created_at, erased_at, erased_by`,
		keyID, ResultEncryptionKeyErased, now, actor, ResultEncryptionKeyActive))
	if err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		Actor: actor, EventType: "RESULT_ENCRYPTION_KEY_ERASED",
		Payload: mustJSON(map[string]any{"key_id": keyID, "affected_results": affected}), OccurredAt: now,
	})
	if err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ResultEncryptionKey{}, opErr(op, ErrConflict, err)
	}
	return key, nil
}

func scanResultEncryptionKey(scanner rowScanner) (ResultEncryptionKey, error) {
	var key ResultEncryptionKey
	var status string
	var erased sql.NullTime
	if err := scanner.Scan(&key.KeyID, &status, &key.CreatedAt, &erased, &key.ErasedBy); err != nil {
		return ResultEncryptionKey{}, err
	}
	key.Status = ResultEncryptionKeyStatus(status)
	key.CreatedAt = dbTime(key.CreatedAt)
	if erased.Valid {
		value := dbTime(erased.Time)
		key.ErasedAt = &value
	}
	return key, nil
}

// PurgeEncryptedResultsBefore deletes retained result ciphertext older than
// cutoff unless the result's task is under an active legal hold. Query records,
// receipt rows, and audit evidence remain intact.
func (s *Store) PurgeEncryptedResultsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	const op = "purge encrypted results"
	if err := s.checkOpen(op); err != nil {
		return 0, err
	}
	if cutoff.IsZero() {
		return 0, opErr(op, ErrInvalid, fmt.Errorf("cutoff is required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return 0, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `
DELETE FROM encrypted_query_results result
WHERE result.created_at < $1
  AND NOT EXISTS (
    SELECT 1 FROM result_retention_holds hold
    WHERE hold.task_id = result.task_id
      AND hold.released_at IS NULL
  )`, dbTime(cutoff))
	if err != nil {
		return 0, opErr(op, ErrConflict, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, opErr(op, ErrConflict, err)
	}
	if count > 0 {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			Actor: "system", EventType: "RETENTION_PURGE_RESULTS",
			Payload: mustJSON(map[string]any{"cutoff": dbTime(cutoff), "purged_results": count}), OccurredAt: s.now(),
		})
		if err != nil {
			return 0, opErr(op, ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, opErr(op, ErrConflict, err)
	}
	return count, nil
}

// SetResultRetentionHold prevents encrypted result ciphertext for a task from
// retention purge until ClearResultRetentionHold releases the hold.
func (s *Store) SetResultRetentionHold(ctx context.Context, taskID, reason, actor string) (ResultRetentionHold, error) {
	const op = "set result retention hold"
	if err := s.checkOpen(op); err != nil {
		return ResultRetentionHold{}, err
	}
	taskID = strings.TrimSpace(taskID)
	reason = strings.TrimSpace(reason)
	actor = strings.TrimSpace(actor)
	if taskID == "" || reason == "" || actor == "" {
		return ResultRetentionHold{}, opErr(op, ErrInvalid, fmt.Errorf("task_id, reason, and actor are required"))
	}
	now := s.now()
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if err := lockTaskForRetentionTx(ctx, tx, taskID); err != nil {
		return ResultRetentionHold{}, opErr(op, retentionTaskLookupErrorKind(err), err)
	}
	hold, err := scanResultRetentionHold(tx.QueryRowContext(ctx, `
INSERT INTO result_retention_holds(task_id, reason, created_by, created_at, released_by, released_at)
VALUES ($1, $2, $3, $4, '', NULL)
ON CONFLICT (task_id) DO UPDATE
SET reason=EXCLUDED.reason,
    created_by=EXCLUDED.created_by,
    created_at=EXCLUDED.created_at,
    released_by='',
    released_at=NULL
RETURNING task_id, reason, created_by, created_at, released_by, released_at`,
		taskID, reason, actor, now))
	if err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: taskID, Actor: actor, EventType: "RETENTION_HOLD_SET",
		Payload: mustJSON(map[string]any{"reason": reason}), OccurredAt: now,
	})
	if err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	return hold, nil
}

// ClearResultRetentionHold releases an active retention hold. Historical hold
// state remains represented in the audit chain.
func (s *Store) ClearResultRetentionHold(ctx context.Context, taskID, actor string) (ResultRetentionHold, error) {
	const op = "clear result retention hold"
	if err := s.checkOpen(op); err != nil {
		return ResultRetentionHold{}, err
	}
	taskID = strings.TrimSpace(taskID)
	actor = strings.TrimSpace(actor)
	if taskID == "" || actor == "" {
		return ResultRetentionHold{}, opErr(op, ErrInvalid, fmt.Errorf("task_id and actor are required"))
	}
	now := s.now()
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if err := lockTaskForRetentionTx(ctx, tx, taskID); err != nil {
		return ResultRetentionHold{}, opErr(op, retentionTaskLookupErrorKind(err), err)
	}
	hold, err := scanResultRetentionHold(tx.QueryRowContext(ctx, `
UPDATE result_retention_holds
SET released_by=$2, released_at=$3
WHERE task_id=$1 AND released_at IS NULL
RETURNING task_id, reason, created_by, created_at, released_by, released_at`,
		taskID, actor, now))
	if err != nil {
		if isNoRows(err) {
			return ResultRetentionHold{}, opErr(op, ErrNotFound, err)
		}
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: taskID, Actor: actor, EventType: "RETENTION_HOLD_CLEARED",
		Payload: mustJSON(map[string]any{"reason": hold.Reason}), OccurredAt: now,
	})
	if err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	return hold, nil
}

func (s *Store) GetResultRetentionHold(ctx context.Context, taskID string) (ResultRetentionHold, error) {
	const op = "get result retention hold"
	if err := s.checkOpen(op); err != nil {
		return ResultRetentionHold{}, err
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ResultRetentionHold{}, opErr(op, ErrInvalid, fmt.Errorf("task_id is required"))
	}
	hold, err := scanResultRetentionHold(s.db.QueryRowContext(ctx, `
SELECT task_id, reason, created_by, created_at, released_by, released_at
FROM result_retention_holds
WHERE task_id=$1 AND released_at IS NULL`, taskID))
	if err != nil {
		if isNoRows(err) {
			return ResultRetentionHold{}, opErr(op, ErrNotFound, err)
		}
		return ResultRetentionHold{}, opErr(op, ErrConflict, err)
	}
	return hold, nil
}

func scanResultRetentionHold(scanner rowScanner) (ResultRetentionHold, error) {
	var hold ResultRetentionHold
	var released sql.NullTime
	if err := scanner.Scan(&hold.TaskID, &hold.Reason, &hold.CreatedBy, &hold.CreatedAt, &hold.ReleasedBy, &released); err != nil {
		return ResultRetentionHold{}, err
	}
	hold.CreatedAt = dbTime(hold.CreatedAt)
	if released.Valid {
		value := dbTime(released.Time)
		hold.ReleasedAt = &value
	}
	return hold, nil
}

func lockTaskForRetentionTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&id)
	if err != nil {
		return err
	}
	return nil
}

func retentionTaskLookupErrorKind(err error) error {
	if isNoRows(err) {
		return ErrNotFound
	}
	return ErrConflict
}
