package control

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

func resultAAD(taskID, queryID string) []byte {
	return []byte("taskbound-result-v1\x00" + taskID + "\x00" + queryID)
}

func plaintextHash(plaintext []byte) string {
	digest := sha256.Sum256(plaintext)
	return hex.EncodeToString(digest[:])
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
	SettlementStore time.Duration
}

// FinalizeQueryMeasured is the measured form of FinalizeQuery. The returned
// timings are observational only and do not alter the atomic transaction.
func (s *Store) FinalizeQueryMeasured(ctx context.Context, settlement BudgetSettlement, plaintext []byte) (QueryRecord, FinalizeQueryMetrics, error) {
	const op = "finalize query"
	var metrics FinalizeQueryMetrics
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, metrics, err
	}
	if s.cipher == nil {
		return QueryRecord{}, metrics, opErr(op, ErrCipherUnavailable, nil)
	}
	if settlement.QueryID == "" || settlement.Rows < 0 || settlement.DBMS < 0 {
		return QueryRecord{}, metrics, opErr(op, ErrInvalid, fmt.Errorf("invalid settlement"))
	}
	current, err := s.GetQuery(ctx, settlement.QueryID)
	if err != nil {
		return QueryRecord{}, metrics, err
	}
	if current.Status == QueryReleased || current.Status == QueryInterrupted {
		return QueryRecord{}, metrics, opErr(op, ErrReservationNotFound, fmt.Errorf("query is %s", current.Status))
	}
	encryptionStarted := time.Now()
	hash := plaintextHash(plaintext)
	nonce, ciphertext, err := s.cipher.Encrypt(plaintext, resultAAD(current.TaskID, current.ID))
	metrics.Encryption = time.Since(encryptionStarted)
	if err != nil {
		return QueryRecord{}, metrics, opErr(op, ErrCipherUnavailable, err)
	}
	settlementStarted := time.Now()
	now := s.now()
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return QueryRecord{}, metrics, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	record, err := settleBudgetTx(ctx, tx, now, settlement, QueryCompleted, hash)
	if err != nil {
		return QueryRecord{}, metrics, opErr(op, settlementErrorKind(err), err)
	}
	created, err := insertEncryptedResultTx(ctx, tx, EncryptedResult{
		QueryID: current.ID, TaskID: current.TaskID, Nonce: nonce, Ciphertext: ciphertext, SHA256: hash, CreatedAt: now,
	})
	if err != nil {
		return QueryRecord{}, metrics, err
	}
	if record.ResultSHA256 != "" && record.ResultSHA256 != hash {
		return QueryRecord{}, metrics, opErr(op, ErrConflict, fmt.Errorf("query already finalized with a different result"))
	}
	if record.ResultSHA256 == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE query_records SET result_sha256=$1 WHERE id=$2`, hash, record.ID); err != nil {
			return QueryRecord{}, metrics, opErr(op, ErrConflict, err)
		}
		record.ResultSHA256 = hash
	}
	if created {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			TaskID: record.TaskID, QueryID: record.ID, Actor: record.Actor, EventType: "QUERY_RESULT_STORED",
			Payload: mustJSON(map[string]any{"result_sha256": hash, "cipher": "AES-256-GCM"}), OccurredAt: now,
		})
		if err != nil {
			return QueryRecord{}, metrics, opErr(op, ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return QueryRecord{}, metrics, opErr(op, ErrConflict, err)
	}
	metrics.SettlementStore = time.Since(settlementStarted)
	return record, metrics, nil
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
	nonce, ciphertext, err := s.cipher.Encrypt(plaintext, resultAAD(taskID, queryID))
	if err != nil {
		return EncryptedResult{}, opErr(op, ErrCipherUnavailable, err)
	}
	result := EncryptedResult{QueryID: queryID, TaskID: taskID, Nonce: nonce, Ciphertext: ciphertext,
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
			Payload: mustJSON(map[string]any{"result_sha256": result.SHA256, "cipher": "AES-256-GCM"}), OccurredAt: result.CreatedAt,
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
	err := tx.QueryRowContext(ctx, `SELECT plaintext_sha256 FROM encrypted_query_results WHERE query_id=$1 FOR UPDATE`, result.QueryID).Scan(&existingHash)
	if err == nil {
		if subtle.ConstantTimeCompare([]byte(existingHash), []byte(result.SHA256)) == 1 {
			return false, nil
		}
		return false, opErr("save encrypted result", ErrConflict, fmt.Errorf("different result already stored"))
	}
	if !isNoRows(err) {
		return false, opErr("save encrypted result", ErrConflict, err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO encrypted_query_results(query_id, task_id, nonce, ciphertext, plaintext_sha256, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, result.QueryID, result.TaskID, result.Nonce, result.Ciphertext, result.SHA256,
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
	err := s.db.QueryRowContext(ctx, `
SELECT query_id, task_id, nonce, ciphertext, plaintext_sha256, created_at
FROM encrypted_query_results WHERE query_id=$1 AND task_id=$2`, queryID, taskID).
		Scan(&result.QueryID, &result.TaskID, &result.Nonce, &result.Ciphertext, &result.SHA256, &created)
	if err != nil {
		if isNoRows(err) {
			return EncryptedResult{}, nil, opErr(op, ErrNotFound, err)
		}
		return EncryptedResult{}, nil, opErr(op, ErrConflict, err)
	}
	result.CreatedAt = dbTime(created)
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
