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

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	exposureReserved = "RESERVED"
	exposureSettled  = "SETTLED"
	exposureReleased = "RELEASED"
)

func validateExposureGrant(grant ExposureGrant) error {
	release := grant.Limits.ReleaseFacts
	influence := grant.Limits.InfluenceFacts
	if release < 0 || influence < 0 {
		return fmt.Errorf("exposure limits cannot be negative")
	}
	if (release == 0) != (influence == 0) {
		return fmt.Errorf("release and influence limits must both be enabled or disabled")
	}
	if release > 0 && strings.TrimSpace(grant.ProfileVersion) == "" {
		return fmt.Errorf("exposure profile version is required")
	}
	if release == 0 && grant.ProfileVersion != "" {
		return fmt.Errorf("exposure profile requires positive limits")
	}
	return nil
}

func ensureExposureLedgerTx(ctx context.Context, tx *sql.Tx, taskID string, grant ExposureGrant, now time.Time) error {
	if err := validateExposureGrant(grant); err != nil {
		return err
	}
	var rootTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM tasks WHERE id=$1 FOR SHARE`, taskID).Scan(&rootTaskID); err != nil {
		return err
	}
	var existing ExposureLedgerSnapshot
	err := tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts,
       used_release_facts, used_influence_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, rootTaskID).
		Scan(&existing.RootTaskID, &existing.ProfileVersion, &existing.Limits.ReleaseFacts,
			&existing.Limits.InfluenceFacts, &existing.Used.ReleaseFacts, &existing.Used.InfluenceFacts,
			&existing.UpdatedAt)
	if !grant.Enabled() {
		if err == nil {
			return fmt.Errorf("delegated task cannot disable its root exposure ledger")
		}
		if isNoRows(err) {
			return nil
		}
		return err
	}
	if err == nil {
		if existing.ProfileVersion != grant.ProfileVersion || grant.Limits.ReleaseFacts > existing.Limits.ReleaseFacts ||
			grant.Limits.InfluenceFacts > existing.Limits.InfluenceFacts {
			return fmt.Errorf("delegated exposure grant expands or changes its root ledger")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	if rootTaskID != taskID {
		return fmt.Errorf("delegated task has no root exposure ledger")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO exposure_ledgers(root_task_id, profile_version, max_release_facts, max_influence_facts, updated_at)
VALUES ($1, $2, $3, $4, $5)`, rootTaskID, grant.ProfileVersion, grant.Limits.ReleaseFacts,
		grant.Limits.InfluenceFacts, dbTime(now))
	return err
}

func (s *Store) GetExposureLedger(ctx context.Context, taskID string) (ExposureLedgerSnapshot, error) {
	const op = "get exposure ledger"
	if err := s.checkOpen(op); err != nil {
		return ExposureLedgerSnapshot{}, err
	}
	var result ExposureLedgerSnapshot
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT l.root_task_id, l.profile_version, l.max_release_facts, l.max_influence_facts,
       l.used_release_facts, l.used_influence_facts, l.updated_at
FROM tasks t JOIN exposure_ledgers l ON l.root_task_id=t.root_task_id
WHERE t.id=$1`, taskID).Scan(&result.RootTaskID, &result.ProfileVersion, &result.Limits.ReleaseFacts,
		&result.Limits.InfluenceFacts, &result.Used.ReleaseFacts, &result.Used.InfluenceFacts, &updated)
	if err != nil {
		if isNoRows(err) {
			return ExposureLedgerSnapshot{}, opErr(op, ErrNotFound, err)
		}
		return ExposureLedgerSnapshot{}, opErr(op, ErrConflict, err)
	}
	result.UpdatedAt = dbTime(updated)
	return result, nil
}

func reserveExposureTx(ctx context.Context, tx *sql.Tx, queryID, taskID string, request *ExposureReservationRequest, now time.Time) (*ExposureReservation, error) {
	var rootTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM tasks WHERE id=$1`, taskID).Scan(&rootTaskID); err != nil {
		return nil, err
	}
	var profile string
	err := tx.QueryRowContext(ctx, `SELECT profile_version FROM exposure_ledgers WHERE root_task_id=$1`, rootTaskID).Scan(&profile)
	if isNoRows(err) {
		if request != nil {
			return nil, fmt.Errorf("exposure reservation supplied for a resource-only task")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, ErrExposureEvidenceRequired
	}
	if strings.TrimSpace(request.ProfileVersion) == "" || request.ProfileVersion != profile ||
		request.EstimatedReleaseFacts < 0 || request.EstimatedInfluenceFacts < 0 {
		return nil, fmt.Errorf("invalid exposure reservation")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO query_exposure_reservations(query_id, task_id, root_task_id, profile_version, status,
       estimated_release_facts, estimated_influence_facts, created_at)
VALUES ($1, $2, $3, $4, 'RESERVED', $5, $6, $7)`, queryID, taskID, rootTaskID, profile,
		request.EstimatedReleaseFacts, request.EstimatedInfluenceFacts, dbTime(now))
	if err != nil {
		return nil, err
	}
	return &ExposureReservation{QueryID: queryID, TaskID: taskID, RootTaskID: rootTaskID,
		ProfileVersion: profile, EstimatedReleaseFacts: request.EstimatedReleaseFacts,
		EstimatedInfluenceFacts: request.EstimatedInfluenceFacts}, nil
}

func settleExposureTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string, observation *exposure.Observation) (*ExposureCharge, error) {
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	err := tx.QueryRowContext(ctx, `
SELECT query_id, task_id, root_task_id, profile_version, estimated_release_facts,
       estimated_influence_facts, status, actual_release_facts, actual_influence_facts,
       charged_release_facts, charged_influence_facts, observation_sha256
FROM query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &status,
			&actual.ReleaseFacts, &actual.InfluenceFacts, &charged.ReleaseFacts, &charged.InfluenceFacts,
			&storedDigest)
	if isNoRows(err) {
		if observation != nil {
			return nil, fmt.Errorf("exposure evidence supplied without a reservation")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if observation == nil {
		return nil, ErrExposureEvidenceRequired
	}
	normalized, err := observation.Normalize()
	if err != nil {
		return nil, err
	}
	if normalized.ProfileVersion != reservation.ProfileVersion {
		return nil, fmt.Errorf("exposure observation uses a different profile")
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	if status == exposureSettled {
		if storedDigest != digest {
			return nil, fmt.Errorf("query already settled with different exposure evidence")
		}
		return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
			ProfileVersion: reservation.ProfileVersion, ActualReleaseFacts: actual.ReleaseFacts,
			ActualInfluenceFacts: actual.InfluenceFacts, ChargedReleaseFacts: charged.ReleaseFacts,
			ChargedInfluenceFacts: charged.InfluenceFacts, ObservationSHA256: digest}, nil
	}
	if status != exposureReserved {
		return nil, fmt.Errorf("exposure reservation is %s", status)
	}

	var ledger ExposureLedgerSnapshot
	err = tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts,
       used_release_facts, used_influence_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, reservation.RootTaskID).
		Scan(&ledger.RootTaskID, &ledger.ProfileVersion, &ledger.Limits.ReleaseFacts,
			&ledger.Limits.InfluenceFacts, &ledger.Used.ReleaseFacts, &ledger.Used.InfluenceFacts,
			&ledger.UpdatedAt)
	if err != nil {
		return nil, err
	}
	newRelease, err := insertNovelFactsTx(ctx, tx, reservation.RootTaskID, "RELEASE", queryID, normalized.Release, now)
	if err != nil {
		return nil, err
	}
	newInfluence, err := insertNovelFactsTx(ctx, tx, reservation.RootTaskID, "INFLUENCE", queryID, normalized.Influence, now)
	if err != nil {
		return nil, err
	}
	var taskLimits ExposureLimits
	if err := tx.QueryRowContext(ctx, `
	SELECT max_release_facts, max_influence_facts FROM task_grants WHERE task_id=$1`, reservation.TaskID).
		Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts); err != nil {
		return nil, err
	}
	// A narrowed descendant may add facts only within its signed absolute
	// family ceiling. Zero-novelty replay remains safe after an ancestor has
	// already moved the shared root ledger beyond that lower ceiling.
	if (newRelease > 0 && ledger.Used.ReleaseFacts+newRelease > taskLimits.ReleaseFacts) ||
		(newInfluence > 0 && ledger.Used.InfluenceFacts+newInfluence > taskLimits.InfluenceFacts) {
		return nil, ErrExposureBudgetExhausted
	}
	result, err := tx.ExecContext(ctx, `
UPDATE exposure_ledgers
SET used_release_facts=used_release_facts+$1,
    used_influence_facts=used_influence_facts+$2,
    updated_at=$3
WHERE root_task_id=$4
  AND used_release_facts+$1 <= max_release_facts
  AND used_influence_facts+$2 <= max_influence_facts`, newRelease, newInfluence, dbTime(now), reservation.RootTaskID)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrExposureBudgetExhausted
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_exposure_reservations
SET status='SETTLED', actual_release_facts=$1, actual_influence_facts=$2,
    charged_release_facts=$3, charged_influence_facts=$4,
    observation_sha256=$5, settled_at=$6
WHERE query_id=$7 AND status='RESERVED'`, len(normalized.Release), len(normalized.Influence),
		newRelease, newInfluence, digest, dbTime(now), queryID)
	if err != nil {
		return nil, err
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: reservation.TaskID, QueryID: queryID, Actor: "system", EventType: "QUERY_EXPOSURE_SETTLED",
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID, "profile_version": reservation.ProfileVersion,
			"actual_release_facts": len(normalized.Release), "actual_influence_facts": len(normalized.Influence),
			"charged_release_facts": newRelease, "charged_influence_facts": newInfluence,
			"observation_sha256": digest}), OccurredAt: now,
	})
	if err != nil {
		return nil, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
		ProfileVersion: reservation.ProfileVersion, ActualReleaseFacts: int64(len(normalized.Release)),
		ActualInfluenceFacts: int64(len(normalized.Influence)), ChargedReleaseFacts: newRelease,
		ChargedInfluenceFacts: newInfluence, ObservationSHA256: digest}, nil
}

func insertNovelFactsTx(ctx context.Context, tx *sql.Tx, rootTaskID, kind, queryID string, facts []exposure.FactID, now time.Time) (int64, error) {
	const chunkSize = 200
	var inserted int64
	for start := 0; start < len(facts); start += chunkSize {
		end := start + chunkSize
		if end > len(facts) {
			end = len(facts)
		}
		var statement strings.Builder
		statement.WriteString(`INSERT INTO exposure_facts(root_task_id, ledger_kind, fact_sha256, identity_json, first_query_id, first_seen_at) VALUES `)
		args := make([]any, 0, (end-start)*6)
		for index, fact := range facts[start:end] {
			if index != 0 {
				statement.WriteString(",")
			}
			hash, err := fact.Hash()
			if err != nil {
				return 0, err
			}
			identity, err := json.Marshal(fact)
			if err != nil {
				return 0, err
			}
			base := len(args) + 1
			fmt.Fprintf(&statement, "($%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5)
			args = append(args, rootTaskID, kind, hash, string(identity), queryID, dbTime(now))
		}
		statement.WriteString(` ON CONFLICT DO NOTHING RETURNING fact_sha256`)
		rows, err := tx.QueryContext(ctx, statement.String(), args...)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var ignored string
			if err := rows.Scan(&ignored); err != nil {
				rows.Close()
				return 0, err
			}
			inserted++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}
	return inserted, nil
}

func releaseExposureReservationTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string) error {
	var taskID, rootTaskID, status string
	err := tx.QueryRowContext(ctx, `
SELECT task_id, root_task_id, status FROM query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&taskID, &rootTaskID, &status)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == exposureReleased {
		return nil
	}
	if status != exposureReserved {
		return fmt.Errorf("cannot release %s exposure reservation", status)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_exposure_reservations SET status='RELEASED', settled_at=$1
WHERE query_id=$2 AND status='RESERVED'`, dbTime(now), queryID)
	if err != nil {
		return err
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: taskID, QueryID: queryID, Actor: "system",
		EventType: "QUERY_EXPOSURE_RELEASED", Payload: mustJSON(map[string]any{"root_task_id": rootTaskID}), OccurredAt: now})
	return err
}

func (s *Store) GetExposureCharge(ctx context.Context, queryID string) (ExposureCharge, error) {
	const op = "get exposure charge"
	if err := s.checkOpen(op); err != nil {
		return ExposureCharge{}, err
	}
	var charge ExposureCharge
	var status string
	err := s.db.QueryRowContext(ctx, `
SELECT query_id, root_task_id, profile_version, status, actual_release_facts,
       actual_influence_facts, charged_release_facts, charged_influence_facts, observation_sha256
FROM query_exposure_reservations WHERE query_id=$1`, queryID).
		Scan(&charge.QueryID, &charge.RootTaskID, &charge.ProfileVersion, &status,
			&charge.ActualReleaseFacts, &charge.ActualInfluenceFacts, &charge.ChargedReleaseFacts,
			&charge.ChargedInfluenceFacts, &charge.ObservationSHA256)
	if err != nil {
		if isNoRows(err) {
			return ExposureCharge{}, opErr(op, ErrNotFound, err)
		}
		return ExposureCharge{}, opErr(op, ErrConflict, err)
	}
	if status != exposureSettled {
		return ExposureCharge{}, opErr(op, ErrNotFound, fmt.Errorf("exposure reservation is %s", status))
	}
	return charge, nil
}
