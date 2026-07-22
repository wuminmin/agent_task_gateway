package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const receiptSelect = `SELECT query_id, receipt_version, gateway_key_id, signature, signed_at,
terminal_audit_sequence, terminal_audit_hash, receipt_json, receipt_sha256, created_at FROM query_receipts`

func (s *Store) SaveQueryReceipt(ctx context.Context, request SaveQueryReceiptRequest) (PersistedQueryReceipt, error) {
	const op = "save query receipt"
	if err := s.checkOpen(op); err != nil {
		return PersistedQueryReceipt{}, err
	}
	if err := validateSaveQueryReceipt(request); err != nil {
		return PersistedQueryReceipt{}, opErr(op, ErrInvalid, err)
	}
	if request.SignedAt.IsZero() {
		request.SignedAt = s.now()
	} else {
		request.SignedAt = dbTime(request.SignedAt)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return PersistedQueryReceipt{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	receipt, err := saveQueryReceiptTx(ctx, tx, s.now(), request)
	if err != nil {
		return PersistedQueryReceipt{}, opErr(op, receiptErrorKind(err), err)
	}
	if err := tx.Commit(); err != nil {
		return PersistedQueryReceipt{}, opErr(op, ErrConflict, err)
	}
	return receipt, nil
}

func validateSaveQueryReceipt(request SaveQueryReceiptRequest) error {
	if request.QueryID == "" || request.Version == "" || request.GatewayKeyID == "" ||
		request.Signature == "" || request.TerminalAuditSequence <= 0 ||
		!validSHA256Hex(request.TerminalAuditHash) || len(request.ReceiptJSON) == 0 ||
		!json.Valid(request.ReceiptJSON) {
		return fmt.Errorf("query, signature, terminal audit evidence, and receipt JSON are required")
	}
	return nil
}

func receiptErrorKind(err error) error {
	if isNoRows(err) {
		return ErrNotFound
	}
	if bytes.Contains([]byte(err.Error()), []byte("invalid receipt")) {
		return ErrInvalid
	}
	return ErrConflict
}

func saveQueryReceiptTx(ctx context.Context, tx *sql.Tx, now time.Time, request SaveQueryReceiptRequest) (PersistedQueryReceipt, error) {
	record, err := scanQuery(tx.QueryRowContext(ctx, querySelect+` WHERE id=$1 FOR UPDATE`, request.QueryID))
	if err != nil {
		return PersistedQueryReceipt{}, err
	}
	if record.Status == QueryReserved || record.CompletedAt == nil {
		return PersistedQueryReceipt{}, fmt.Errorf("invalid receipt: query is not terminal")
	}
	audit, err := scanAudit(tx.QueryRowContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE sequence=$1 AND query_id=$2 AND current_hash=$3`,
		request.TerminalAuditSequence, request.QueryID, request.TerminalAuditHash))
	if err != nil {
		return PersistedQueryReceipt{}, err
	}
	if !isTerminalQueryAudit(audit.EventType) {
		return PersistedQueryReceipt{}, fmt.Errorf("invalid receipt: audit event %s is not terminal query evidence", audit.EventType)
	}
	hash := sha256.Sum256(request.ReceiptJSON)
	receipt := PersistedQueryReceipt{
		QueryID: request.QueryID, Version: request.Version, GatewayKeyID: request.GatewayKeyID,
		Signature: request.Signature, SignedAt: dbTime(request.SignedAt),
		TerminalAuditSequence: request.TerminalAuditSequence, TerminalAuditHash: request.TerminalAuditHash,
		ReceiptJSON: append([]byte(nil), request.ReceiptJSON...), ReceiptSHA256: hex.EncodeToString(hash[:]),
		CreatedAt: dbTime(now),
	}
	existing, err := scanPersistedQueryReceipt(tx.QueryRowContext(ctx, receiptSelect+` WHERE query_id=$1 FOR UPDATE`, request.QueryID))
	if err == nil {
		if samePersistedReceipt(existing, receipt) {
			return existing, nil
		}
		return PersistedQueryReceipt{}, fmt.Errorf("query receipt already persisted with different evidence")
	}
	if !isNoRows(err) {
		return PersistedQueryReceipt{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO query_receipts(query_id, receipt_version, gateway_key_id, signature, signed_at,
 terminal_audit_sequence, terminal_audit_hash, receipt_json, receipt_sha256, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		receipt.QueryID, receipt.Version, receipt.GatewayKeyID, receipt.Signature, dbTime(receipt.SignedAt),
		receipt.TerminalAuditSequence, receipt.TerminalAuditHash, receipt.ReceiptJSON, receipt.ReceiptSHA256,
		dbTime(receipt.CreatedAt))
	if err != nil {
		return PersistedQueryReceipt{}, err
	}
	return receipt, nil
}

func persistTerminalReceiptTx(ctx context.Context, tx *sql.Tx, now time.Time, evidence QueryReceipt, builder TerminalReceiptBuilder) (PersistedQueryReceipt, error) {
	if builder == nil {
		return PersistedQueryReceipt{}, fmt.Errorf("invalid receipt: builder is required")
	}
	existing, err := scanPersistedQueryReceipt(tx.QueryRowContext(ctx, receiptSelect+` WHERE query_id=$1 FOR UPDATE`, evidence.Query.ID))
	if err == nil {
		return existing, nil
	}
	if !isNoRows(err) {
		return PersistedQueryReceipt{}, err
	}
	request, err := builder(evidence)
	if err != nil {
		return PersistedQueryReceipt{}, err
	}
	if request.SignedAt.IsZero() {
		request.SignedAt = dbTime(now)
	} else {
		request.SignedAt = dbTime(request.SignedAt)
	}
	return saveQueryReceiptTx(ctx, tx, now, request)
}

func isTerminalQueryAudit(eventType string) bool {
	switch eventType {
	case "QUERY_COMPLETED", "QUERY_BUDGET_RELEASED", "QUERY_FAILED", "QUERY_INDETERMINATE", "QUERY_INTERRUPTED":
		return true
	default:
		return false
	}
}

func samePersistedReceipt(left, right PersistedQueryReceipt) bool {
	return left.QueryID == right.QueryID &&
		left.Version == right.Version &&
		left.GatewayKeyID == right.GatewayKeyID &&
		left.Signature == right.Signature &&
		left.TerminalAuditSequence == right.TerminalAuditSequence &&
		left.TerminalAuditHash == right.TerminalAuditHash &&
		left.ReceiptSHA256 == right.ReceiptSHA256 &&
		bytes.Equal(left.ReceiptJSON, right.ReceiptJSON)
}

func (s *Store) GetPersistedQueryReceipt(ctx context.Context, queryID string) (PersistedQueryReceipt, error) {
	const op = "get persisted query receipt"
	if err := s.checkOpen(op); err != nil {
		return PersistedQueryReceipt{}, err
	}
	receipt, err := scanPersistedQueryReceipt(s.db.QueryRowContext(ctx, receiptSelect+` WHERE query_id=$1`, queryID))
	if err != nil {
		if isNoRows(err) {
			return PersistedQueryReceipt{}, opErr(op, ErrNotFound, err)
		}
		return PersistedQueryReceipt{}, opErr(op, ErrConflict, err)
	}
	return receipt, nil
}

func scanPersistedQueryReceipt(row rowScanner) (PersistedQueryReceipt, error) {
	var receipt PersistedQueryReceipt
	var signedAt time.Time
	var createdAt time.Time
	err := row.Scan(&receipt.QueryID, &receipt.Version, &receipt.GatewayKeyID, &receipt.Signature, &signedAt,
		&receipt.TerminalAuditSequence, &receipt.TerminalAuditHash, &receipt.ReceiptJSON, &receipt.ReceiptSHA256, &createdAt)
	if err != nil {
		return PersistedQueryReceipt{}, err
	}
	receipt.ReceiptJSON = append([]byte(nil), receipt.ReceiptJSON...)
	receipt.SignedAt = dbTime(signedAt)
	receipt.CreatedAt = dbTime(createdAt)
	return receipt, nil
}
