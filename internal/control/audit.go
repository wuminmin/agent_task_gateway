package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
)

func hashAudit(previous string, event AuditEvent) (string, error) {
	return auditchain.Hash(previous, event)
}

func appendAuditTx(ctx context.Context, tx *sql.Tx, event AuditEvent) (AuditEvent, error) {
	if strings.TrimSpace(event.EventID) == "" {
		generated, err := randomID("audit_")
		if err != nil {
			return AuditEvent{}, err
		}
		event.EventID = generated
	}
	if strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.EventType) == "" {
		return AuditEvent{}, fmt.Errorf("actor and event type are required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = dbTime(time.Now())
	} else {
		event.OccurredAt = dbTime(event.OccurredAt)
	}
	payload, err := normalizeJSON(event.Payload, `{}`)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("invalid audit payload: %w", err)
	}
	event.Payload = payload
	var sequence int64
	previous := auditchain.GenesisHash
	err = tx.QueryRowContext(ctx, `
SELECT last_sequence, last_hash
FROM audit_chain_head
WHERE singleton=TRUE
FOR UPDATE`).Scan(&sequence, &previous)
	if err != nil {
		return AuditEvent{}, err
	}
	event.Sequence = sequence + 1
	event.PreviousHash = previous
	event.CurrentHash, err = hashAudit(previous, event)
	if err != nil {
		return AuditEvent{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO audit_events(sequence, event_id, task_id, query_id, actor, event_type, payload_json, occurred_at, previous_hash, current_hash)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9, $10)`,
		event.Sequence,
		event.EventID, event.TaskID, event.QueryID, event.Actor, event.EventType, string(event.Payload),
		dbTime(event.OccurredAt), event.PreviousHash, event.CurrentHash)
	if err != nil {
		return AuditEvent{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE audit_chain_head SET last_sequence=$1, last_hash=$2 WHERE singleton=TRUE`,
		event.Sequence, event.CurrentHash)
	return event, err
}

func (s *Store) AppendAuditEvent(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	const op = "append audit event"
	if err := s.checkOpen(op); err != nil {
		return AuditEvent{}, err
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.now()
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return AuditEvent{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	created, err := appendAuditTx(ctx, tx, event)
	if err != nil {
		return AuditEvent{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return AuditEvent{}, opErr(op, ErrConflict, err)
	}
	return created, nil
}

func (s *Store) ListAuditEvents(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	const op = "list audit events"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE sequence > $1
  AND ($2 = '' OR task_id = $3)
  AND ($4 = '' OR query_id = $5)
  AND ($6 = '' OR actor = $7)
  AND ($8 = '' OR event_type = $9)
ORDER BY sequence LIMIT $10`, filter.After, filter.TaskID, filter.TaskID, filter.QueryID, filter.QueryID,
		filter.Actor, filter.Actor, filter.EventType, filter.EventType, limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return events, nil
}

type rowScanner interface{ Scan(...any) error }

func (s *Store) ListAuditEventsForQuery(ctx context.Context, queryID string) ([]AuditEvent, error) {
	const op = "list query audit events"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if strings.TrimSpace(queryID) == "" {
		return nil, opErr(op, ErrInvalid, fmt.Errorf("query id is required"))
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE query_id=$1
ORDER BY sequence`, queryID)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return events, nil
}

func (s *Store) ListAuditEventsRange(ctx context.Context, after, through int64) ([]AuditEvent, error) {
	const op = "list audit event range"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	if after < 0 || through < after {
		return nil, opErr(op, ErrInvalid, fmt.Errorf("invalid audit range"))
	}
	if through == after {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE sequence > $1 AND sequence <= $2
ORDER BY sequence`, after, through)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return events, nil
}

func (s *Store) GetAuditEvent(ctx context.Context, sequence int64) (AuditEvent, error) {
	const op = "get audit event"
	if err := s.checkOpen(op); err != nil {
		return AuditEvent{}, err
	}
	if sequence <= 0 {
		return AuditEvent{}, opErr(op, ErrInvalid, fmt.Errorf("sequence must be positive"))
	}
	event, err := scanAudit(s.db.QueryRowContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events
WHERE sequence=$1`, sequence))
	if err != nil {
		if isNoRows(err) {
			return AuditEvent{}, opErr(op, ErrNotFound, err)
		}
		return AuditEvent{}, opErr(op, ErrConflict, err)
	}
	return event, nil
}

func (s *Store) AuditCheckpoint(ctx context.Context) (AuditCheckpoint, error) {
	const op = "audit checkpoint"
	if err := s.checkOpen(op); err != nil {
		return AuditCheckpoint{}, err
	}
	var checkpoint AuditCheckpoint
	err := s.db.QueryRowContext(ctx, `SELECT last_sequence, last_hash FROM audit_chain_head WHERE singleton=TRUE`).
		Scan(&checkpoint.Sequence, &checkpoint.Hash)
	if err != nil {
		return AuditCheckpoint{}, opErr(op, ErrConflict, err)
	}
	return checkpoint, nil
}

func scanAudit(row rowScanner) (AuditEvent, error) {
	var event AuditEvent
	var payload []byte
	var occurred time.Time
	err := row.Scan(&event.Sequence, &event.EventID, &event.TaskID, &event.QueryID, &event.Actor,
		&event.EventType, &payload, &occurred, &event.PreviousHash, &event.CurrentHash)
	if err != nil {
		return AuditEvent{}, err
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	event.OccurredAt = dbTime(occurred)
	return event, nil
}

func (s *Store) VerifyAuditChain(ctx context.Context) error {
	const op = "verify audit chain"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, event_id, COALESCE(task_id,''), COALESCE(query_id,''), actor, event_type,
       payload_json, occurred_at, previous_hash, current_hash
FROM audit_events ORDER BY sequence`)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	previous := auditchain.GenesisHash
	var expectedSequence int64 = 1
	for rows.Next() {
		event, err := scanAudit(rows)
		if err != nil {
			return opErr(op, ErrAuditChainBroken, err)
		}
		if event.Sequence != expectedSequence || event.PreviousHash != previous {
			return opErr(op, ErrAuditChainBroken, fmt.Errorf("sequence %d has invalid predecessor", event.Sequence))
		}
		expected, err := hashAudit(previous, event)
		if err != nil {
			return opErr(op, ErrAuditChainBroken, err)
		}
		if !strings.EqualFold(expected, event.CurrentHash) {
			return opErr(op, ErrAuditChainBroken, fmt.Errorf("sequence %d hash mismatch", event.Sequence))
		}
		previous = event.CurrentHash
		expectedSequence++
	}
	if err := rows.Err(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	var headSequence int64
	var headHash string
	if err := s.db.QueryRowContext(ctx, `SELECT last_sequence, last_hash FROM audit_chain_head WHERE singleton=TRUE`).Scan(&headSequence, &headHash); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if headSequence != expectedSequence-1 || headHash != previous {
		return opErr(op, ErrAuditChainBroken, fmt.Errorf("audit chain head does not match the final event"))
	}
	return nil
}
