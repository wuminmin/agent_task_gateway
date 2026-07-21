package control

import (
	"context"
	"encoding/json"
)

type interruptedReservation struct {
	queryID string
	taskID  string
	actor   string
}

// Recover deterministically releases in-flight reservations, makes unfinished
// callback claims retryable, and expires stale tasks. It is safe to call more
// than once and is run automatically by Open/New.
func (s *Store) Recover(ctx context.Context) (RecoveryReport, error) {
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

	rows, err := tx.QueryContext(ctx, `SELECT id, task_id, actor FROM query_records WHERE status='RESERVED' ORDER BY created_at, id`)
	if err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	var interrupted []interruptedReservation
	for rows.Next() {
		var reservation interruptedReservation
		if err := rows.Scan(&reservation.queryID, &reservation.taskID, &reservation.actor); err != nil {
			_ = rows.Close()
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		interrupted = append(interrupted, reservation)
	}
	if err := rows.Close(); err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE budget_ledger
SET reserved_queries=0, reserved_rows=0, reserved_db_ms=0, updated_at=?
WHERE reserved_queries<>0 OR reserved_rows<>0 OR reserved_db_ms<>0`, formatTime(now)); err != nil {
		return RecoveryReport{}, opErr(op, ErrConflict, err)
	}
	for _, reservation := range interrupted {
		budget, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE task_id=?`, reservation.taskID))
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		budgetJSON, _ := json.Marshal(budget)
		_, err = tx.ExecContext(ctx, `
UPDATE query_records
SET status='INTERRUPTED', error_code='GATEWAY_RESTART', budget_after_json=?, completed_at=?
WHERE id=? AND status='RESERVED'`, budgetJSON, formatTime(now), reservation.queryID)
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			TaskID: reservation.taskID, QueryID: reservation.queryID, Actor: reservation.actor,
			EventType: "QUERY_INTERRUPTED", Payload: mustJSON(map[string]any{"reason": "gateway_restart"}), OccurredAt: now,
		})
		if err != nil {
			return RecoveryReport{}, opErr(op, ErrConflict, err)
		}
	}

	type expiringTask struct{ id string }
	expiringRows, err := tx.QueryContext(ctx, `
SELECT id FROM tasks
WHERE state <> 'ARCHIVED' AND expires_at IS NOT NULL AND expires_at <= ?
ORDER BY id`, formatTime(now))
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
