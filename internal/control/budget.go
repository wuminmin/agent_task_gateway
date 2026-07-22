package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *Store) GetBudget(ctx context.Context, taskID string) (BudgetSnapshot, error) {
	const op = "get budget"
	if err := s.checkOpen(op); err != nil {
		return BudgetSnapshot{}, err
	}
	budget, err := scanBudget(s.db.QueryRowContext(ctx, budgetSelect+` WHERE task_id=$1`, taskID))
	if err != nil {
		if isNoRows(err) {
			return BudgetSnapshot{}, opErr(op, ErrNotFound, err)
		}
		return BudgetSnapshot{}, opErr(op, ErrConflict, err)
	}
	return budget, nil
}

const budgetSelect = `SELECT task_id, max_queries, max_rows, max_db_ms, used_queries, used_rows, used_db_ms,
reserved_queries, reserved_rows, reserved_db_ms, updated_at FROM budget_ledger`

func scanBudget(row rowScanner) (BudgetSnapshot, error) {
	var budget BudgetSnapshot
	var updated time.Time
	err := row.Scan(&budget.TaskID, &budget.Limits.Queries, &budget.Limits.Rows, &budget.Limits.DBMS,
		&budget.Usage.UsedQueries, &budget.Usage.UsedRows, &budget.Usage.UsedDBMS,
		&budget.Usage.ReservedQueries, &budget.Usage.ReservedRows, &budget.Usage.ReservedDBMS, &updated)
	if err != nil {
		return BudgetSnapshot{}, err
	}
	budget.UpdatedAt = dbTime(updated)
	return budget, nil
}

func (s *Store) ReserveBudget(ctx context.Context, request ReserveRequest) (BudgetReservation, error) {
	const op = "reserve budget"
	if err := s.checkOpen(op); err != nil {
		return BudgetReservation{}, err
	}
	if request.QueryID == "" || request.TaskID == "" || request.RequestID == "" || request.Actor == "" || request.RequestDigest == "" || request.SQLFingerprint == "" {
		return BudgetReservation{}, opErr(op, ErrInvalid, fmt.Errorf("query, task, request id, actor, request digest, and SQL fingerprint are required"))
	}
	if request.RequestedRows < 0 || request.RequestedDBMS < 0 {
		return BudgetReservation{}, opErr(op, ErrInvalid, fmt.Errorf("requested budget cannot be negative"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)

	var state TaskState
	var expires sql.NullTime
	var catalogVersion string
	err = tx.QueryRowContext(ctx, `SELECT state, expires_at, catalog_version FROM tasks WHERE id=$1 FOR UPDATE`, request.TaskID).
		Scan(&state, &expires, &catalogVersion)
	if err != nil {
		if isNoRows(err) {
			return BudgetReservation{}, opErr(op, ErrNotFound, err)
		}
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	// The task row lock serializes both ordinary reservations and retries for a
	// task. Check the durable idempotency key before task-state and budget
	// checks: a retry is an observation of the first request, not a new query,
	// and remains observable after revocation, expiry, or budget exhaustion.
	existing, err := scanQuery(tx.QueryRowContext(ctx, querySelect+` WHERE task_id=$1 AND request_id=$2 FOR UPDATE`, request.TaskID, request.RequestID))
	if err == nil {
		if existing.RequestDigest != request.RequestDigest || existing.Actor != request.Actor {
			return BudgetReservation{}, opErr(op, ErrIdempotencyConflict, fmt.Errorf("request id is bound to another digest"))
		}
		return BudgetReservation{
			QueryID: existing.ID, TaskID: existing.TaskID, RequestID: existing.RequestID,
			AllowedRows: existing.ReservedRows, AllowedDBMS: existing.ReservedDBMS,
			Before: existing.BudgetBefore, Replay: true, Record: &existing,
		}, nil
	}
	if !isNoRows(err) {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	if state != TaskActive {
		return BudgetReservation{}, opErr(op, ErrTaskNotActive, fmt.Errorf("state is %s", state))
	}
	if expires.Valid {
		expiresAt := dbTime(expires.Time)
		if !s.now().Before(expiresAt) {
			if err := archiveTaskTx(ctx, tx, request.TaskID, TerminalExpired, "system", s.now()); err != nil {
				return BudgetReservation{}, opErr(op, ErrConflict, err)
			}
			if err := tx.Commit(); err != nil {
				return BudgetReservation{}, opErr(op, ErrConflict, err)
			}
			return BudgetReservation{}, opErr(op, ErrTaskExpired, nil)
		}
	}
	if request.CatalogVersion != "" && request.CatalogVersion != catalogVersion {
		return BudgetReservation{}, opErr(op, ErrConflict, fmt.Errorf("catalog version mismatch"))
	}
	if request.CatalogVersion == "" {
		request.CatalogVersion = catalogVersion
	}

	before, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE task_id=$1 FOR UPDATE`, request.TaskID))
	if err != nil {
		if isNoRows(err) {
			return BudgetReservation{}, opErr(op, ErrNotFound, fmt.Errorf("grant budget: %w", err))
		}
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	if before.Usage.ReservedQueries != 0 {
		return BudgetReservation{}, opErr(op, ErrQueryInProgress, nil)
	}
	remaining := before.Remaining()
	if remaining.Queries < 1 || remaining.Rows < 1 || remaining.DBMS < 1 {
		if err := archiveTaskTx(ctx, tx, request.TaskID, TerminalBudgetExhausted, "system", s.now()); err != nil {
			return BudgetReservation{}, opErr(op, ErrConflict, err)
		}
		if err := tx.Commit(); err != nil {
			return BudgetReservation{}, opErr(op, ErrConflict, err)
		}
		return BudgetReservation{}, opErr(op, ErrBudgetExhausted, nil)
	}
	allowedRows := request.RequestedRows
	if allowedRows == 0 || allowedRows > remaining.Rows {
		allowedRows = remaining.Rows
	}
	allowedDBMS := request.RequestedDBMS
	if allowedDBMS == 0 || allowedDBMS > remaining.DBMS {
		allowedDBMS = remaining.DBMS
	}
	now := s.now()
	after := before
	after.Usage.ReservedQueries++
	after.Usage.ReservedRows += allowedRows
	after.Usage.ReservedDBMS += allowedDBMS
	after.UpdatedAt = now
	beforeJSON, _ := json.Marshal(before)
	result, err := tx.ExecContext(ctx, `
UPDATE budget_ledger
SET reserved_queries=reserved_queries+1, reserved_rows=reserved_rows+$1, reserved_db_ms=reserved_db_ms+$2, updated_at=$3
WHERE task_id=$4 AND reserved_queries=0`, allowedRows, allowedDBMS, dbTime(now), request.TaskID)
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return BudgetReservation{}, opErr(op, ErrQueryInProgress, nil)
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO query_records(id, task_id, request_id, actor, request_digest, sql_fingerprint, catalog_version,
	 catalog_digest, manifest_digest, grant_digest, policy_decision, status, reserved_rows, reserved_db_ms,
	 budget_before_json, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'RESERVED', $12, $13, $14, $15)`,
		request.QueryID, request.TaskID, request.RequestID, request.Actor, request.RequestDigest,
		request.SQLFingerprint, request.CatalogVersion, request.CatalogDigest, request.ManifestDigest,
		request.GrantDigest, request.PolicyDecision, allowedRows, allowedDBMS, string(beforeJSON), dbTime(now))
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: request.TaskID, QueryID: request.QueryID, Actor: request.Actor, EventType: "QUERY_BUDGET_RESERVED",
		Payload: mustJSON(map[string]any{"request_id": request.RequestID, "rows": allowedRows, "db_ms": allowedDBMS}), OccurredAt: now,
	})
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	return BudgetReservation{QueryID: request.QueryID, TaskID: request.TaskID, RequestID: request.RequestID, AllowedRows: allowedRows,
		AllowedDBMS: allowedDBMS, Before: before, After: after}, nil
}

// SettleBudget releases the reservation and atomically charges bounded actual
// use. Calling it again for an already-settled query returns the stored record.
func (s *Store) SettleBudget(ctx context.Context, settlement BudgetSettlement) (QueryRecord, error) {
	return s.settle(ctx, settlement, QueryCompleted, "")
}

// ReleaseBudget releases a reservation without charging it.
func (s *Store) ReleaseBudget(ctx context.Context, queryID, errorCode string) (QueryRecord, error) {
	return s.settle(ctx, BudgetSettlement{QueryID: queryID, ErrorCode: errorCode}, QueryReleased, "")
}

// MarkIndeterminate conservatively charges the entire reservation when a
// connector invocation may have executed but its result cannot be proved. The
// associated request_id remains terminal and cannot be automatically retried.
func (s *Store) MarkIndeterminate(ctx context.Context, queryID, errorCode string) (QueryRecord, error) {
	record, err := s.GetQuery(ctx, queryID)
	if err != nil {
		return QueryRecord{}, err
	}
	return s.settle(ctx, BudgetSettlement{
		QueryID: queryID, Rows: record.ReservedRows, DBMS: record.ReservedDBMS, ErrorCode: errorCode,
	}, QueryIndeterminate, "")
}

func (s *Store) settle(ctx context.Context, settlement BudgetSettlement, status QueryStatus, resultHash string) (QueryRecord, error) {
	const op = "settle budget"
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, err
	}
	if settlement.QueryID == "" || settlement.Rows < 0 || settlement.DBMS < 0 {
		return QueryRecord{}, opErr(op, ErrInvalid, fmt.Errorf("query is required and use cannot be negative"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return QueryRecord{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	record, err := settleBudgetTx(ctx, tx, s.now(), settlement, status, resultHash)
	if err != nil {
		return QueryRecord{}, opErr(op, settlementErrorKind(err), err)
	}
	if err := tx.Commit(); err != nil {
		return QueryRecord{}, opErr(op, ErrConflict, err)
	}
	return record, nil
}

func settlementErrorKind(err error) error {
	if isNoRows(err) || strings.Contains(err.Error(), "reservation not found") {
		return ErrReservationNotFound
	}
	if strings.Contains(err.Error(), "invalid settlement") {
		return ErrInvalid
	}
	return ErrConflict
}

func settleBudgetTx(ctx context.Context, tx *sql.Tx, now time.Time, settlement BudgetSettlement, status QueryStatus, resultHash string) (QueryRecord, error) {
	record, err := scanQuery(tx.QueryRowContext(ctx, querySelect+` WHERE id=$1 FOR UPDATE`, settlement.QueryID))
	if err != nil {
		if isNoRows(err) {
			return QueryRecord{}, fmt.Errorf("reservation not found: %w", err)
		}
		return QueryRecord{}, err
	}
	if record.Status != QueryReserved {
		// A repeated delivery gets the durable first result. This is important
		// when a caller loses the HTTP response after the commit succeeded.
		if record.Status == status {
			return record, nil
		}
		return QueryRecord{}, fmt.Errorf("reservation not found: query is %s", record.Status)
	}
	if status != QueryCompleted && status != QueryReleased && status != QueryIndeterminate {
		return QueryRecord{}, fmt.Errorf("invalid settlement status %q", status)
	}
	budget, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE task_id=$1 FOR UPDATE`, record.TaskID))
	if err != nil {
		return QueryRecord{}, err
	}
	if budget.Usage.ReservedQueries < 1 || budget.Usage.ReservedRows < record.ReservedRows || budget.Usage.ReservedDBMS < record.ReservedDBMS {
		return QueryRecord{}, fmt.Errorf("reservation ledger is inconsistent")
	}
	if settlement.Rows > record.ReservedRows {
		return QueryRecord{}, fmt.Errorf("invalid settlement: result rows %d exceed reserved rows %d", settlement.Rows, record.ReservedRows)
	}
	chargedQueries, chargedRows, chargedDBMS := int64(0), int64(0), int64(0)
	if status == QueryCompleted || status == QueryIndeterminate {
		chargedQueries = 1
		chargedRows = settlement.Rows
		chargedDBMS = min64(settlement.DBMS, record.ReservedDBMS)
	}
	after := budget
	after.Usage.ReservedQueries--
	after.Usage.ReservedRows -= record.ReservedRows
	after.Usage.ReservedDBMS -= record.ReservedDBMS
	after.Usage.UsedQueries += chargedQueries
	after.Usage.UsedRows += chargedRows
	after.Usage.UsedDBMS += chargedDBMS
	after.UpdatedAt = now
	if after.Usage.UsedQueries > after.Limits.Queries || after.Usage.UsedRows > after.Limits.Rows || after.Usage.UsedDBMS > after.Limits.DBMS {
		return QueryRecord{}, fmt.Errorf("invalid settlement would exceed hard budget")
	}
	_, err = tx.ExecContext(ctx, `
UPDATE budget_ledger
SET used_queries=$1, used_rows=$2, used_db_ms=$3, reserved_queries=$4, reserved_rows=$5, reserved_db_ms=$6, updated_at=$7
WHERE task_id=$8`, after.Usage.UsedQueries, after.Usage.UsedRows, after.Usage.UsedDBMS,
		after.Usage.ReservedQueries, after.Usage.ReservedRows, after.Usage.ReservedDBMS, dbTime(now), record.TaskID)
	if err != nil {
		return QueryRecord{}, err
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return QueryRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_records
SET status=$1, result_rows=$2, result_db_ms=$3, charged_queries=$4, charged_rows=$5, charged_db_ms=$6,
    budget_after_json=$7, result_sha256=$8, error_code=$9, completed_at=$10
WHERE id=$11 AND status='RESERVED'`, status, settlement.Rows, settlement.DBMS, chargedQueries, chargedRows,
		chargedDBMS, string(afterJSON), resultHash, settlement.ErrorCode, dbTime(now), settlement.QueryID)
	if err != nil {
		return QueryRecord{}, err
	}
	eventType := "QUERY_BUDGET_RELEASED"
	if status == QueryCompleted {
		eventType = "QUERY_COMPLETED"
	} else if status == QueryIndeterminate {
		eventType = "QUERY_INDETERMINATE"
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: record.TaskID, QueryID: record.ID, Actor: record.Actor, EventType: eventType, OccurredAt: now,
		Payload: mustJSON(map[string]any{
			"result_rows": settlement.Rows, "result_db_ms": settlement.DBMS,
			"charged_queries": chargedQueries, "charged_rows": chargedRows, "charged_db_ms": chargedDBMS,
			"result_sha256": resultHash, "error_code": settlement.ErrorCode,
		}),
	})
	if err != nil {
		return QueryRecord{}, err
	}
	if (status == QueryCompleted || status == QueryIndeterminate) && (after.Usage.UsedQueries >= after.Limits.Queries || after.Usage.UsedRows >= after.Limits.Rows || after.Usage.UsedDBMS >= after.Limits.DBMS) {
		if err := archiveTaskTx(ctx, tx, record.TaskID, TerminalBudgetExhausted, "system", now); err != nil {
			return QueryRecord{}, err
		}
	}
	record.Status = status
	record.ResultRows = settlement.Rows
	record.ResultDBMS = settlement.DBMS
	record.ChargedQueries = chargedQueries
	record.ChargedRows = chargedRows
	record.ChargedDBMS = chargedDBMS
	record.BudgetAfter = &after
	record.ResultSHA256 = resultHash
	record.ErrorCode = settlement.ErrorCode
	completedAt := dbTime(now)
	record.CompletedAt = &completedAt
	return record, nil
}

func archiveTaskTx(ctx context.Context, tx *sql.Tx, taskID string, reason TerminalReason, actor string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
UPDATE tasks SET state='ARCHIVED', terminal_reason=$1, updated_at=$2
WHERE id=$3 AND state <> 'ARCHIVED'`, reason, dbTime(now), taskID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: taskID, Actor: actor, EventType: "TASK_STATE_CHANGED", OccurredAt: now,
		Payload: mustJSON(map[string]any{"to": TaskArchived, "terminal_reason": reason}),
	})
	return err
}

func (s *Store) GetQuery(ctx context.Context, queryID string) (QueryRecord, error) {
	const op = "get query"
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, err
	}
	record, err := scanQuery(s.db.QueryRowContext(ctx, querySelect+` WHERE id=$1`, queryID))
	if err != nil {
		if isNoRows(err) {
			return QueryRecord{}, opErr(op, ErrNotFound, err)
		}
		return QueryRecord{}, opErr(op, ErrConflict, err)
	}
	return record, nil
}

// GetQueryByRequestID resolves the durable per-task idempotency key.
func (s *Store) GetQueryByRequestID(ctx context.Context, taskID, requestID string) (QueryRecord, error) {
	const op = "get query by request id"
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, err
	}
	if taskID == "" || requestID == "" {
		return QueryRecord{}, opErr(op, ErrInvalid, fmt.Errorf("task and request id are required"))
	}
	record, err := scanQuery(s.db.QueryRowContext(ctx, querySelect+` WHERE task_id=$1 AND request_id=$2`, taskID, requestID))
	if err != nil {
		if isNoRows(err) {
			return QueryRecord{}, opErr(op, ErrNotFound, err)
		}
		return QueryRecord{}, opErr(op, ErrConflict, err)
	}
	return record, nil
}

const querySelect = `SELECT id, task_id, request_id, actor, request_digest, sql_fingerprint, catalog_version,
catalog_digest, manifest_digest, grant_digest, policy_decision, status, reserved_rows, reserved_db_ms, result_rows, result_db_ms, charged_queries,
charged_rows, charged_db_ms, budget_before_json, budget_after_json, result_sha256, error_code,
created_at, completed_at FROM query_records`

func scanQuery(row rowScanner) (QueryRecord, error) {
	var record QueryRecord
	var before []byte
	var after []byte
	var created time.Time
	var completed sql.NullTime
	err := row.Scan(&record.ID, &record.TaskID, &record.RequestID, &record.Actor, &record.RequestDigest, &record.SQLFingerprint,
		&record.CatalogVersion, &record.CatalogDigest, &record.ManifestDigest, &record.GrantDigest,
		&record.PolicyDecision, &record.Status, &record.ReservedRows, &record.ReservedDBMS,
		&record.ResultRows, &record.ResultDBMS, &record.ChargedQueries, &record.ChargedRows, &record.ChargedDBMS,
		&before, &after, &record.ResultSHA256, &record.ErrorCode, &created, &completed)
	if err != nil {
		return QueryRecord{}, err
	}
	if err := json.Unmarshal(before, &record.BudgetBefore); err != nil {
		return QueryRecord{}, err
	}
	if len(after) > 0 {
		var snapshot BudgetSnapshot
		if err := json.Unmarshal(after, &snapshot); err != nil {
			return QueryRecord{}, err
		}
		record.BudgetAfter = &snapshot
	}
	record.CreatedAt = dbTime(created)
	record.CompletedAt = scanNullableTime(completed)
	return record, nil
}

func (s *Store) ListQueries(ctx context.Context, taskID string, limit int) ([]QueryRecord, error) {
	const op = "list queries"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, querySelect+` WHERE task_id=$1 ORDER BY created_at, id LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var records []QueryRecord
	for rows.Next() {
		record, err := scanQuery(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return records, nil
}

// GetQueryReceipt returns the durable query evidence together with the hash
// chained completion event that authenticates it.
func (s *Store) GetQueryReceipt(ctx context.Context, queryID string) (QueryReceipt, error) {
	const op = "get query receipt"
	if err := s.checkOpen(op); err != nil {
		return QueryReceipt{}, err
	}
	query, err := s.GetQuery(ctx, queryID)
	if err != nil {
		return QueryReceipt{}, err
	}
	if query.Status == QueryReserved {
		return QueryReceipt{}, opErr(op, ErrNotFound, fmt.Errorf("query is %s", query.Status))
	}
	audit, err := scanAudit(s.db.QueryRowContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE query_id=$1 AND event_type IN ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')
ORDER BY sequence DESC LIMIT 1`, queryID))
	if err != nil {
		if isNoRows(err) {
			return QueryReceipt{}, opErr(op, ErrNotFound, err)
		}
		return QueryReceipt{}, opErr(op, ErrConflict, err)
	}
	return QueryReceipt{Query: query, Audit: audit}, nil
}

func (s *Store) ListQueryReceipts(ctx context.Context, taskID string, limit int) ([]QueryReceipt, error) {
	const op = "list query receipts"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM query_records WHERE task_id=$1 AND status<>'RESERVED' ORDER BY created_at, id LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	var queryIDs []string
	for rows.Next() {
		var queryID string
		if err := rows.Scan(&queryID); err != nil {
			_ = rows.Close()
			return nil, opErr(op, ErrConflict, err)
		}
		queryIDs = append(queryIDs, queryID)
	}
	if err := rows.Close(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	receipts := make([]QueryReceipt, 0, len(queryIDs))
	for _, queryID := range queryIDs {
		receipt, err := s.GetQueryReceipt(ctx, queryID)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}
