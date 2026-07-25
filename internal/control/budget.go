package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if request.QueryID == "" || request.TaskID == "" || request.RequestID == "" || request.Actor == "" ||
		request.RequestDigest == "" || request.SQLFingerprint == "" || request.DatasourceID == "" ||
		!validSHA256Hex(request.CatalogDigest) || !validSHA256Hex(request.SchemaDigest) ||
		!validSHA256Hex(request.ManifestDigest) || !validSHA256Hex(request.GrantDigest) ||
		request.PolicyDecision == "" {
		return BudgetReservation{}, opErr(op, ErrInvalid, fmt.Errorf("query, task, request, policy, and datasource evidence are required"))
	}
	if request.RequestedRows < 0 || request.RequestedDBMS < 0 ||
		(request.Exposure != nil && (request.Exposure.EstimatedReleaseFacts < 0 || request.Exposure.EstimatedInfluenceFacts < 0)) {
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
	 catalog_digest, datasource_id, schema_digest, manifest_digest, grant_digest, policy_decision, status, reserved_rows, reserved_db_ms,
	 budget_before_json, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'RESERVED', $14, $15, $16, $17)`,
		request.QueryID, request.TaskID, request.RequestID, request.Actor, request.RequestDigest,
		request.SQLFingerprint, request.CatalogVersion, request.CatalogDigest, request.DatasourceID,
		request.SchemaDigest, request.ManifestDigest, request.GrantDigest, request.PolicyDecision,
		allowedRows, allowedDBMS, string(beforeJSON), dbTime(now))
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	exposureReservation, err := reserveExposureTx(ctx, tx, request.QueryID, request.TaskID, request.Exposure, now)
	if err != nil {
		kind := ErrConflict
		if errors.Is(err, ErrExposureEvidenceRequired) {
			kind = ErrExposureEvidenceRequired
		}
		return BudgetReservation{}, opErr(op, kind, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: request.TaskID, QueryID: request.QueryID, Actor: request.Actor, EventType: "QUERY_BUDGET_RESERVED",
		Payload: mustJSON(map[string]any{
			"request_id": request.RequestID, "rows": allowedRows, "db_ms": allowedDBMS,
			"catalog_digest": request.CatalogDigest, "datasource_id": request.DatasourceID,
			"schema_digest": request.SchemaDigest, "manifest_digest": request.ManifestDigest,
			"grant_digest": request.GrantDigest,
		}), OccurredAt: now,
	})
	if err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return BudgetReservation{}, opErr(op, ErrConflict, err)
	}
	return BudgetReservation{QueryID: request.QueryID, TaskID: request.TaskID, RequestID: request.RequestID, AllowedRows: allowedRows,
		AllowedDBMS: allowedDBMS, Before: before, After: after, Exposure: exposureReservation}, nil
}

// SettleBudget releases the reservation and atomically charges bounded actual
// use. Calling it again for an already-settled query returns the stored record.
func (s *Store) SettleBudget(ctx context.Context, settlement BudgetSettlement) (QueryRecord, error) {
	return s.settle(ctx, settlement, QueryCompleted, "")
}

// SettleBudgetWithReceipt settles a successful query and persists its signed
// receipt inside the same Control PG transaction.
func (s *Store) SettleBudgetWithReceipt(ctx context.Context, settlement BudgetSettlement, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, error) {
	return s.settleWithReceipt(ctx, settlement, QueryCompleted, "", builder)
}

// ReleaseBudget releases a reservation without charging it.
func (s *Store) ReleaseBudget(ctx context.Context, queryID, errorCode string) (QueryRecord, error) {
	return s.settle(ctx, BudgetSettlement{QueryID: queryID, ErrorCode: errorCode}, QueryReleased, "")
}

// ReleaseBudgetWithReceipt releases a reservation and persists its signed
// receipt inside the same Control PG transaction.
func (s *Store) ReleaseBudgetWithReceipt(ctx context.Context, queryID, errorCode string, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, error) {
	return s.settleWithReceipt(ctx, BudgetSettlement{QueryID: queryID, ErrorCode: errorCode}, QueryReleased, "", builder)
}

// FailBudget records a post-execution failure with bounded observed usage.
func (s *Store) FailBudget(ctx context.Context, settlement BudgetSettlement) (QueryRecord, error) {
	return s.settle(ctx, settlement, QueryFailed, "")
}

// FailBudgetWithReceipt records a post-execution failure and persists its
// signed receipt inside the same Control PG transaction.
func (s *Store) FailBudgetWithReceipt(ctx context.Context, settlement BudgetSettlement, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, error) {
	return s.settleWithReceipt(ctx, settlement, QueryFailed, "", builder)
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
		QueryID: queryID, Rows: record.ReservedRows, DBMS: record.ReservedDBMS, ObservedDBMS: record.ReservedDBMS, ErrorCode: errorCode,
	}, QueryIndeterminate, "")
}

// MarkIndeterminateWithReceipt conservatively charges the full reservation and
// persists its signed receipt inside the same Control PG transaction.
func (s *Store) MarkIndeterminateWithReceipt(ctx context.Context, queryID, errorCode string, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, error) {
	record, err := s.GetQuery(ctx, queryID)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, err
	}
	return s.settleWithReceipt(ctx, BudgetSettlement{
		QueryID: queryID, Rows: record.ReservedRows, DBMS: record.ReservedDBMS, ObservedDBMS: record.ReservedDBMS, ErrorCode: errorCode,
	}, QueryIndeterminate, "", builder)
}

func (s *Store) settle(ctx context.Context, settlement BudgetSettlement, status QueryStatus, resultHash string) (QueryRecord, error) {
	record, _, err := s.settleWithReceipt(ctx, settlement, status, resultHash, nil)
	return record, err
}

func (s *Store) settleWithReceipt(ctx context.Context, settlement BudgetSettlement, status QueryStatus, resultHash string, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, error) {
	const op = "settle budget"
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, err
	}
	if settlement.QueryID == "" || settlement.Rows < 0 || settlement.ChargeRows < 0 || settlement.DBMS < 0 || settlement.ObservedDBMS < 0 {
		return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, ErrInvalid, fmt.Errorf("query is required and use cannot be negative"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	now := s.now()
	var exposureCharge *ExposureCharge
	if status == QueryCompleted {
		exposureCharge, err = settleExposureTx(ctx, tx, now, settlement.QueryID, settlement.Exposure)
		if err != nil {
			return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, settlementErrorKind(err), err)
		}
	} else if err := releaseExposureReservationTx(ctx, tx, now, settlement.QueryID); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, settlementErrorKind(err), err)
	}
	record, audit, err := settleBudgetTx(ctx, tx, now, settlement, status, resultHash)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, settlementErrorKind(err), err)
	}
	var receipt PersistedQueryReceipt
	if builder != nil {
		receipt, err = persistTerminalReceiptTx(ctx, tx, now, QueryReceipt{Query: record, Audit: audit, Exposure: exposureCharge}, builder)
		if err != nil {
			return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, receiptErrorKind(err), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, opErr(op, ErrConflict, err)
	}
	return record, receipt, nil
}

func settlementErrorKind(err error) error {
	if isNoRows(err) || strings.Contains(err.Error(), "reservation not found") {
		return ErrReservationNotFound
	}
	if strings.Contains(err.Error(), "invalid settlement") {
		return ErrInvalid
	}
	if errors.Is(err, ErrExposureBudgetExhausted) {
		return ErrExposureBudgetExhausted
	}
	if errors.Is(err, ErrExposureEvidenceRequired) {
		return ErrExposureEvidenceRequired
	}
	return ErrConflict
}

func settleBudgetTx(ctx context.Context, tx *sql.Tx, now time.Time, settlement BudgetSettlement, status QueryStatus, resultHash string) (QueryRecord, AuditEvent, error) {
	record, err := scanQuery(tx.QueryRowContext(ctx, querySelect+` WHERE id=$1 FOR UPDATE`, settlement.QueryID))
	if err != nil {
		if isNoRows(err) {
			return QueryRecord{}, AuditEvent{}, fmt.Errorf("reservation not found: %w", err)
		}
		return QueryRecord{}, AuditEvent{}, err
	}
	if record.Status != QueryReserved {
		// A repeated delivery gets the durable first result. This is important
		// when a caller loses the HTTP response after the commit succeeded.
		if record.Status == status {
			audit, err := terminalAuditForQueryTx(ctx, tx, record.ID)
			return record, audit, err
		}
		return QueryRecord{}, AuditEvent{}, fmt.Errorf("reservation not found: query is %s", record.Status)
	}
	// Wall clocks can move backwards by a few microseconds under host time
	// synchronization. A terminal receipt must nevertheless preserve the causal
	// order established by the reservation and settlement transactions.
	now = notBefore(now, record.CreatedAt)
	if status != QueryCompleted && status != QueryReleased && status != QueryFailed && status != QueryIndeterminate {
		return QueryRecord{}, AuditEvent{}, fmt.Errorf("invalid settlement status %q", status)
	}
	budget, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE task_id=$1 FOR UPDATE`, record.TaskID))
	if err != nil {
		return QueryRecord{}, AuditEvent{}, err
	}
	if budget.Usage.ReservedQueries < 1 || budget.Usage.ReservedRows < record.ReservedRows || budget.Usage.ReservedDBMS < record.ReservedDBMS {
		return QueryRecord{}, AuditEvent{}, fmt.Errorf("reservation ledger is inconsistent")
	}
	chargeRows := settlement.ChargeRows
	if chargeRows == 0 {
		chargeRows = settlement.Rows
	}
	if settlement.Rows > record.ReservedRows || chargeRows > record.ReservedRows {
		return QueryRecord{}, AuditEvent{}, fmt.Errorf("invalid settlement: result/charged rows (%d/%d) exceed reserved rows %d", settlement.Rows, chargeRows, record.ReservedRows)
	}
	chargedQueries, chargedRows, chargedDBMS := int64(0), int64(0), int64(0)
	if status == QueryCompleted || status == QueryFailed || status == QueryIndeterminate {
		chargedQueries = 1
		chargedRows = chargeRows
		chargedDBMS = min64(settlement.DBMS, record.ReservedDBMS)
	}
	// observedDBMS is the raw connector-reported database time, preserved
	// untruncated (it may exceed the charged/clamped value). The ledger
	// invariant is still enforced via chargedDBMS above.
	observedDBMS := settlement.ObservedDBMS
	if observedDBMS == 0 && settlement.DBMS > 0 {
		observedDBMS = settlement.DBMS
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
		return QueryRecord{}, AuditEvent{}, fmt.Errorf("invalid settlement would exceed hard budget")
	}
	_, err = tx.ExecContext(ctx, `
UPDATE budget_ledger
SET used_queries=$1, used_rows=$2, used_db_ms=$3, reserved_queries=$4, reserved_rows=$5, reserved_db_ms=$6, updated_at=$7
WHERE task_id=$8`, after.Usage.UsedQueries, after.Usage.UsedRows, after.Usage.UsedDBMS,
		after.Usage.ReservedQueries, after.Usage.ReservedRows, after.Usage.ReservedDBMS, dbTime(now), record.TaskID)
	if err != nil {
		return QueryRecord{}, AuditEvent{}, err
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return QueryRecord{}, AuditEvent{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_records
SET status=$1, result_rows=$2, result_db_ms=$3, result_db_ms_observed=$4, charged_queries=$5, charged_rows=$6, charged_db_ms=$7,
    budget_after_json=$8, result_sha256=$9, error_code=$10, completed_at=$11
WHERE id=$12 AND status='RESERVED'`, status, settlement.Rows, settlement.DBMS, observedDBMS, chargedQueries, chargedRows,
		chargedDBMS, string(afterJSON), resultHash, settlement.ErrorCode, dbTime(now), settlement.QueryID)
	if err != nil {
		return QueryRecord{}, AuditEvent{}, err
	}
	eventType := "QUERY_BUDGET_RELEASED"
	if status == QueryCompleted {
		eventType = "QUERY_COMPLETED"
	} else if status == QueryFailed {
		eventType = "QUERY_FAILED"
	} else if status == QueryIndeterminate {
		eventType = "QUERY_INDETERMINATE"
	}
	audit, err := appendAuditTx(ctx, tx, AuditEvent{
		TaskID: record.TaskID, QueryID: record.ID, Actor: record.Actor, EventType: eventType, OccurredAt: now,
		Payload: mustJSON(map[string]any{
			"result_rows": settlement.Rows, "result_db_ms": settlement.DBMS, "result_db_ms_observed": observedDBMS,
			"charged_queries": chargedQueries, "charged_rows": chargedRows, "charged_db_ms": chargedDBMS,
			"result_sha256": resultHash, "error_code": settlement.ErrorCode,
			"catalog_digest": record.CatalogDigest, "datasource_id": record.DatasourceID,
			"schema_digest": record.SchemaDigest,
		}),
	})
	if err != nil {
		return QueryRecord{}, AuditEvent{}, err
	}
	if (status == QueryCompleted || status == QueryFailed || status == QueryIndeterminate) && (after.Usage.UsedQueries >= after.Limits.Queries || after.Usage.UsedRows >= after.Limits.Rows || after.Usage.UsedDBMS >= after.Limits.DBMS) {
		if err := archiveTaskTx(ctx, tx, record.TaskID, TerminalBudgetExhausted, "system", now); err != nil {
			return QueryRecord{}, AuditEvent{}, err
		}
	}
	record.Status = status
	record.ResultRows = settlement.Rows
	record.ResultDBMS = settlement.DBMS
	record.ResultObservedDBMS = observedDBMS
	record.ChargedQueries = chargedQueries
	record.ChargedRows = chargedRows
	record.ChargedDBMS = chargedDBMS
	record.BudgetAfter = &after
	record.ResultSHA256 = resultHash
	record.ErrorCode = settlement.ErrorCode
	completedAt := dbTime(now)
	record.CompletedAt = &completedAt
	return record, audit, nil
}

func notBefore(value, lowerBound time.Time) time.Time {
	if value.Before(lowerBound) {
		return lowerBound
	}
	return value
}

func terminalAuditForQueryTx(ctx context.Context, tx *sql.Tx, queryID string) (AuditEvent, error) {
	return scanAudit(tx.QueryRowContext(ctx, terminalAuditSelect, queryID))
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
catalog_digest, datasource_id, schema_digest, manifest_digest, grant_digest, policy_decision, status, reserved_rows, reserved_db_ms, result_rows, result_db_ms, result_db_ms_observed, charged_queries,
charged_rows, charged_db_ms, budget_before_json, budget_after_json, result_sha256, error_code,
created_at, completed_at FROM query_records`

func scanQuery(row rowScanner) (QueryRecord, error) {
	var record QueryRecord
	var before []byte
	var after []byte
	var created time.Time
	var completed sql.NullTime
	err := row.Scan(&record.ID, &record.TaskID, &record.RequestID, &record.Actor, &record.RequestDigest, &record.SQLFingerprint,
		&record.CatalogVersion, &record.CatalogDigest, &record.DatasourceID, &record.SchemaDigest,
		&record.ManifestDigest, &record.GrantDigest,
		&record.PolicyDecision, &record.Status, &record.ReservedRows, &record.ReservedDBMS,
		&record.ResultRows, &record.ResultDBMS, &record.ResultObservedDBMS, &record.ChargedQueries, &record.ChargedRows, &record.ChargedDBMS,
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

func (s *Store) ListQueriesPage(ctx context.Context, taskID, cursor string, limit int) (QueryRecordPage, error) {
	const op = "list queries page"
	if err := s.checkOpen(op); err != nil {
		return QueryRecordPage{}, err
	}
	if strings.TrimSpace(taskID) == "" {
		return QueryRecordPage{}, opErr(op, ErrInvalid, fmt.Errorf("task id is required"))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{taskID, limit + 1}
	predicate := ""
	if strings.TrimSpace(cursor) != "" {
		var cursorCreated time.Time
		if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM query_records WHERE task_id=$1 AND id=$2`, taskID, cursor).Scan(&cursorCreated); err != nil {
			if isNoRows(err) {
				return QueryRecordPage{}, opErr(op, ErrInvalid, fmt.Errorf("cursor does not belong to task"))
			}
			return QueryRecordPage{}, opErr(op, ErrConflict, err)
		}
		predicate = ` AND (created_at, id) > ($3, $4)`
		args = append(args, dbTime(cursorCreated), cursor)
	}
	rows, err := s.db.QueryContext(ctx, querySelect+` WHERE task_id=$1`+predicate+` ORDER BY created_at, id LIMIT $2`, args...)
	if err != nil {
		return QueryRecordPage{}, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	records := make([]QueryRecord, 0, limit)
	for rows.Next() {
		record, err := scanQuery(rows)
		if err != nil {
			return QueryRecordPage{}, opErr(op, ErrConflict, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return QueryRecordPage{}, opErr(op, ErrConflict, err)
	}
	next := ""
	if len(records) > limit {
		next = records[limit-1].ID
		records = records[:limit]
	}
	return QueryRecordPage{Records: records, NextCursor: next}, nil
}

const terminalAuditSelect = `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE query_id=$1 AND event_type IN ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_FAILED','QUERY_INDETERMINATE','QUERY_INTERRUPTED')
ORDER BY sequence DESC LIMIT 1`

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
	audit, err := scanAudit(s.db.QueryRowContext(ctx, terminalAuditSelect, queryID))
	if err != nil {
		if isNoRows(err) {
			return QueryReceipt{}, opErr(op, ErrNotFound, err)
		}
		return QueryReceipt{}, opErr(op, ErrConflict, err)
	}
	evidence := QueryReceipt{Query: query, Audit: audit}
	if query.Status == QueryCompleted {
		charge, chargeErr := s.GetExposureCharge(ctx, queryID)
		if chargeErr == nil {
			evidence.Exposure = &charge
		} else if !errors.Is(chargeErr, ErrNotFound) {
			return QueryReceipt{}, chargeErr
		}
	}
	receipt, err := scanPersistedQueryReceipt(s.db.QueryRowContext(ctx, receiptSelect+` WHERE query_id=$1`, queryID))
	if err == nil {
		evidence.Receipt = &receipt
		return evidence, nil
	}
	if !isNoRows(err) {
		return QueryReceipt{}, opErr(op, ErrConflict, err)
	}
	return evidence, nil
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
