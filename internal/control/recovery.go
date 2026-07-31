package control

import (
	"context"
	"encoding/json"
	"fmt"
)

type interruptedReservation struct {
	queryID           string
	taskID            string
	actor             string
	viewBindingDigest string
	reservedRows      int64
	reservedDBMS      int64
}

// Recover deterministically charges the full reservation for indeterminate
// queries, makes unfinished callback claims retryable, and expires stale tasks.
// It is safe to call more than once and is run automatically by Open/New.
func (s *Store) Recover(ctx context.Context) (RecoveryReport, error) {
	return s.recover(ctx, nil)
}

func (s *Store) recover(ctx context.Context, receiptBuilder TerminalReceiptBuilder) (RecoveryReport, error) {
	const op = "recover store"
	if err := s.checkOpen(op); err != nil {
		return RecoveryReport{}, err
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	now := s.now()

	rows, err := tx.QueryContext(ctx, `
SELECT id, task_id, actor, view_binding_digest, reserved_rows, reserved_db_ms
FROM query_records WHERE status='RESERVED' ORDER BY created_at, id FOR UPDATE`)
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	var interrupted []interruptedReservation
	for rows.Next() {
		var reservation interruptedReservation
		if err := rows.Scan(&reservation.queryID, &reservation.taskID, &reservation.actor, &reservation.viewBindingDigest,
			&reservation.reservedRows, &reservation.reservedDBMS); err != nil {
			_ = rows.Close()
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		interrupted = append(interrupted, reservation)
	}
	if err := rows.Close(); err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	for _, reservation := range interrupted {
		// Match MarkIndeterminate: resource reservations are conservatively
		// charged, while uncommitted exposure reservations are released. This
		// also prevents a restarted V4 query from leaving a forever-RESERVED
		// ordinal row that could later be settled against a terminal query.
		if err := releaseAnyExposureReservationTx(ctx, tx, now, reservation.queryID); err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		before, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE task_id=$1 FOR UPDATE`, reservation.taskID))
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		if before.Usage.ReservedQueries < 1 || before.Usage.ReservedRows < reservation.reservedRows || before.Usage.ReservedDBMS < reservation.reservedDBMS {
			return RecoveryReport{}, opErr(op, ErrConflict, fmt.Errorf("reservation ledger is inconsistent for query %s", reservation.queryID))
		}
		after := before
		after.Usage.ReservedQueries--
		after.Usage.ReservedRows -= reservation.reservedRows
		after.Usage.ReservedDBMS -= reservation.reservedDBMS
		after.Usage.UsedQueries++
		after.Usage.UsedRows += reservation.reservedRows
		after.Usage.UsedDBMS += reservation.reservedDBMS
		after.UpdatedAt = now
		if after.Usage.UsedQueries > after.Limits.Queries || after.Usage.UsedRows > after.Limits.Rows || after.Usage.UsedDBMS > after.Limits.DBMS {
			return RecoveryReport{}, opErr(op, ErrConflict, fmt.Errorf("recovered reservation would exceed hard budget for query %s", reservation.queryID))
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE budget_ledger
SET used_queries=$1, used_rows=$2, used_db_ms=$3, reserved_queries=$4, reserved_rows=$5, reserved_db_ms=$6, updated_at=$7
WHERE task_id=$8`, after.Usage.UsedQueries, after.Usage.UsedRows, after.Usage.UsedDBMS,
			after.Usage.ReservedQueries, after.Usage.ReservedRows, after.Usage.ReservedDBMS,
			dbTime(now), reservation.taskID); err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		budgetJSON, err := json.Marshal(after)
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		result, err := tx.ExecContext(ctx, `
UPDATE query_records
	SET status='INDETERMINATE', result_rows=$1, result_db_ms=$2, result_db_ms_observed=$2,
	    charged_queries=1, charged_rows=$1, charged_db_ms=$2,
	    error_code='GATEWAY_RESTART', budget_after_json=$3, completed_at=$4
	WHERE id=$5 AND status='RESERVED'`, reservation.reservedRows, reservation.reservedDBMS,
			string(budgetJSON), dbTime(now), reservation.queryID)
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return RecoveryReport{}, opErr(op, ErrConflict, fmt.Errorf("reservation changed during recovery for query %s", reservation.queryID))
		}
		audit, err := appendAuditTx(ctx, tx, AuditEvent{
			TaskID: reservation.taskID, QueryID: reservation.queryID, Actor: reservation.actor,
			EventType: "QUERY_INDETERMINATE", Payload: mustJSON(map[string]any{
				"reason": "gateway_restart", "status": QueryIndeterminate, "charged_queries": int64(1),
				"charged_rows": reservation.reservedRows, "charged_db_ms": reservation.reservedDBMS,
				"result_db_ms_observed": reservation.reservedDBMS,
				"view_binding_digest":   reservation.viewBindingDigest,
			}), OccurredAt: now,
		})
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		if receiptBuilder != nil {
			record, err := scanQuery(tx.QueryRowContext(ctx, querySelect+` WHERE id=$1 FOR UPDATE`, reservation.queryID))
			if err != nil {
				return RecoveryReport{}, opErr(op, ErrConflict, err)
			}
			if _, err := persistTerminalReceiptTx(ctx, tx, now, QueryReceipt{Query: record, Audit: audit}, receiptBuilder); err != nil {
				return RecoveryReport{}, opErr(op, receiptErrorKind(err), err)
			}
		}
		if after.Usage.UsedQueries >= after.Limits.Queries || after.Usage.UsedRows >= after.Limits.Rows || after.Usage.UsedDBMS >= after.Limits.DBMS {
			if err := archiveTaskTx(ctx, tx, reservation.taskID, TerminalBudgetExhausted, "system", now); err != nil {
				return RecoveryReport{}, opErr(op, ErrConflict, err)
			}
		}
	}

	type expiringTask struct{ id string }
	expiringRows, err := tx.QueryContext(ctx, `
SELECT id FROM tasks
WHERE state <> 'ARCHIVED' AND expires_at IS NOT NULL AND expires_at <= $1
ORDER BY id FOR UPDATE`, dbTime(now))
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	var expiring []expiringTask
	for expiringRows.Next() {
		var task expiringTask
		if err := expiringRows.Scan(&task.id); err != nil {
			_ = expiringRows.Close()
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		expiring = append(expiring, task)
	}
	if err := expiringRows.Close(); err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	for _, task := range expiring {
		if err := archiveTaskTx(ctx, tx, task.id, TerminalExpired, "system", now); err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
	}

	callbackResult, err := tx.ExecContext(ctx, `
UPDATE callback_idempotency
SET status='RETRYABLE', last_error='gateway_restart', completed_at=NULL
WHERE status='PROCESSING'`)
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	retryableCallbacks, err := callbackResult.RowsAffected()
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	if retryableCallbacks > 0 {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			Actor: "system", EventType: "CALLBACKS_RECOVERED", OccurredAt: now,
			Payload: mustJSON(map[string]any{"retryable_count": retryableCallbacks}),
		})
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	return RecoveryReport{InterruptedQueries: len(interrupted), ExpiredTasks: len(expiring), RetryableCallbacks: int(retryableCallbacks)}, nil
}
