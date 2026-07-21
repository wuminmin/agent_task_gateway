package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const auditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

type auditHashMaterial struct {
	PreviousHash string          `json:"previous_hash"`
	EventID      string          `json:"event_id"`
	TaskID       string          `json:"task_id"`
	QueryID      string          `json:"query_id"`
	Actor        string          `json:"actor"`
	EventType    string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload"`
	OccurredAt   string          `json:"occurred_at"`
}

func hashAudit(previous string, event AuditEvent) (string, error) {
	payload, err := normalizeJSON(event.Payload, `{}`)
	if err != nil {
		return "", err
	}
	material, err := json.Marshal(auditHashMaterial{
		PreviousHash: previous,
		EventID:      event.EventID,
		TaskID:       event.TaskID,
		QueryID:      event.QueryID,
		Actor:        event.Actor,
		EventType:    event.EventType,
		Payload:      payload,
		OccurredAt:   formatTime(event.OccurredAt),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
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
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	payload, err := normalizeJSON(event.Payload, `{}`)
	if err != nil {
		return AuditEvent{}, fmt.Errorf("invalid audit payload: %w", err)
	}
	event.Payload = payload
	previous := auditGenesisHash
	err = tx.QueryRowContext(ctx, `SELECT current_hash FROM audit_events ORDER BY sequence DESC LIMIT 1`).Scan(&previous)
	if err != nil && !isNoRows(err) {
		return AuditEvent{}, err
	}
	event.PreviousHash = previous
	event.CurrentHash, err = hashAudit(previous, event)
	if err != nil {
		return AuditEvent{}, err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO audit_events(event_id, task_id, query_id, actor, event_type, payload_json, occurred_at, previous_hash, current_hash)
VALUES (?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
		event.EventID, event.TaskID, event.QueryID, event.Actor, event.EventType, []byte(event.Payload),
		formatTime(event.OccurredAt), event.PreviousHash, event.CurrentHash)
	if err != nil {
		return AuditEvent{}, err
	}
	event.Sequence, err = result.LastInsertId()
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
WHERE sequence > ? AND (? = '' OR task_id = ?) AND (? = '' OR actor = ?) AND (? = '' OR event_type = ?)
ORDER BY sequence LIMIT ?`, filter.After, filter.TaskID, filter.TaskID, filter.Actor, filter.Actor,
		filter.EventType, filter.EventType, limit)
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

func scanAudit(row rowScanner) (AuditEvent, error) {
	var event AuditEvent
	var payload []byte
	var occurred string
	err := row.Scan(&event.Sequence, &event.EventID, &event.TaskID, &event.QueryID, &event.Actor,
		&event.EventType, &payload, &occurred, &event.PreviousHash, &event.CurrentHash)
	if err != nil {
		return AuditEvent{}, err
	}
	event.Payload = append(json.RawMessage(nil), payload...)
	event.OccurredAt, err = parseTime(occurred)
	return event, err
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
	previous := auditGenesisHash
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
	return nil
}
