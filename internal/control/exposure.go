package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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
	if release > 0 && grant.ProfileVersion != exposure.ProfileV1 && grant.ProfileVersion != exposure.ProfileV2 {
		return fmt.Errorf("unsupported exposure profile version")
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

type exposureSettlementMetrics struct {
	ReservationLock time.Duration
	LedgerLock      time.Duration
	FactStore       time.Duration
}

func settleExposureTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string, observation *exposure.Observation) (*ExposureCharge, error) {
	charge, _, err := settleExposureMeasuredTx(ctx, tx, now, queryID, observation)
	return charge, err
}

func settleExposureMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string, observation *exposure.Observation) (*ExposureCharge, exposureSettlementMetrics, error) {
	var metrics exposureSettlementMetrics
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	reservationLockStarted := time.Now()
	err := tx.QueryRowContext(ctx, `
SELECT query_id, task_id, root_task_id, profile_version, estimated_release_facts,
       estimated_influence_facts, status, actual_release_facts, actual_influence_facts,
       charged_release_facts, charged_influence_facts, observation_sha256
FROM query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &status,
			&actual.ReleaseFacts, &actual.InfluenceFacts, &charged.ReleaseFacts, &charged.InfluenceFacts,
			&storedDigest)
	metrics.ReservationLock = time.Since(reservationLockStarted)
	if isNoRows(err) {
		if observation != nil {
			return nil, metrics, fmt.Errorf("exposure evidence supplied without a reservation")
		}
		return nil, metrics, nil
	}
	if err != nil {
		return nil, metrics, err
	}
	if observation == nil {
		return nil, metrics, ErrExposureEvidenceRequired
	}
	normalized, err := observation.Normalize()
	if err != nil {
		return nil, metrics, err
	}
	if normalized.ProfileVersion != reservation.ProfileVersion {
		return nil, metrics, fmt.Errorf("exposure observation uses a different profile")
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, metrics, err
	}
	digestBytes := sha256.Sum256(encoded)
	digest := hex.EncodeToString(digestBytes[:])
	if status == exposureSettled {
		if storedDigest != digest {
			return nil, metrics, fmt.Errorf("query already settled with different exposure evidence")
		}
		return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
			ProfileVersion: reservation.ProfileVersion, ActualReleaseFacts: actual.ReleaseFacts,
			ActualInfluenceFacts: actual.InfluenceFacts, ChargedReleaseFacts: charged.ReleaseFacts,
			ChargedInfluenceFacts: charged.InfluenceFacts, ObservationSHA256: digest}, metrics, nil
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("exposure reservation is %s", status)
	}

	var ledger ExposureLedgerSnapshot
	ledgerLockStarted := time.Now()
	err = tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts,
       used_release_facts, used_influence_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, reservation.RootTaskID).
		Scan(&ledger.RootTaskID, &ledger.ProfileVersion, &ledger.Limits.ReleaseFacts,
			&ledger.Limits.InfluenceFacts, &ledger.Used.ReleaseFacts, &ledger.Used.InfluenceFacts,
			&ledger.UpdatedAt)
	metrics.LedgerLock = time.Since(ledgerLockStarted)
	if err != nil {
		return nil, metrics, err
	}
	factStoreStarted := time.Now()
	newRelease, err := insertNovelFactsTx(ctx, tx, reservation.RootTaskID, "RELEASE", queryID, normalized.Release, now)
	if err != nil {
		return nil, metrics, err
	}
	newInfluence, err := insertNovelFactsTx(ctx, tx, reservation.RootTaskID, "INFLUENCE", queryID, normalized.Influence, now)
	metrics.FactStore = time.Since(factStoreStarted)
	if err != nil {
		return nil, metrics, err
	}
	var taskLimits ExposureLimits
	if err := tx.QueryRowContext(ctx, `
	SELECT max_release_facts, max_influence_facts FROM task_grants WHERE task_id=$1`, reservation.TaskID).
		Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts); err != nil {
		return nil, metrics, err
	}
	// A narrowed descendant may add facts only within its signed absolute
	// family ceiling. Zero-novelty replay remains safe after an ancestor has
	// already moved the shared root ledger beyond that lower ceiling.
	if (newRelease > 0 && ledger.Used.ReleaseFacts+newRelease > taskLimits.ReleaseFacts) ||
		(newInfluence > 0 && ledger.Used.InfluenceFacts+newInfluence > taskLimits.InfluenceFacts) {
		return nil, metrics, ErrExposureBudgetExhausted
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
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, ErrExposureBudgetExhausted
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_exposure_reservations
SET status='SETTLED', actual_release_facts=$1, actual_influence_facts=$2,
    charged_release_facts=$3, charged_influence_facts=$4,
    observation_sha256=$5, settled_at=$6
WHERE query_id=$7 AND status='RESERVED'`, len(normalized.Release), len(normalized.Influence),
		newRelease, newInfluence, digest, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: reservation.TaskID, QueryID: queryID, Actor: "system", EventType: "QUERY_EXPOSURE_SETTLED",
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID, "profile_version": reservation.ProfileVersion,
			"actual_release_facts": len(normalized.Release), "actual_influence_facts": len(normalized.Influence),
			"charged_release_facts": newRelease, "charged_influence_facts": newInfluence,
			"observation_sha256": digest}), OccurredAt: now,
	})
	if err != nil {
		return nil, metrics, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
		ProfileVersion: reservation.ProfileVersion, ActualReleaseFacts: int64(len(normalized.Release)),
		ActualInfluenceFacts: int64(len(normalized.Influence)), ChargedReleaseFacts: newRelease,
		ChargedInfluenceFacts: newInfluence, ObservationSHA256: digest}, metrics, nil
}

func planAndSettleExposureTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string, request RepresentationPlanningRequest) (exposure.ExactPlan, *ExposureCharge, error) {
	if len(request.Candidates) == 0 || !validSHA256Hex(request.CandidatesSHA256) || !validSHA256Hex(request.SnapshotBundleSHA256) {
		return exposure.ExactPlan{}, nil, fmt.Errorf("invalid V2 representation planning request")
	}
	var taskID, rootTaskID, profile, status string
	if err := tx.QueryRowContext(ctx, `
SELECT task_id, root_task_id, profile_version, status
FROM query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&taskID, &rootTaskID, &profile, &status); err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	if profile != exposure.ProfileV2 || status != exposureReserved {
		return exposure.ExactPlan{}, nil, fmt.Errorf("V2 planner requires a reserved %s ledger", exposure.ProfileV2)
	}
	var ledger ExposureLedgerSnapshot
	if err := tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts,
       used_release_facts, used_influence_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, rootTaskID).
		Scan(&ledger.RootTaskID, &ledger.ProfileVersion, &ledger.Limits.ReleaseFacts,
			&ledger.Limits.InfluenceFacts, &ledger.Used.ReleaseFacts, &ledger.Used.InfluenceFacts,
			&ledger.UpdatedAt); err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	history, err := exposureHistoryTx(ctx, tx, rootTaskID)
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	var taskLimits ExposureLimits
	if err := tx.QueryRowContext(ctx, `SELECT max_release_facts, max_influence_facts FROM task_grants WHERE task_id=$1`, taskID).
		Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts); err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	releaseBudget := min64(ledger.Limits.ReleaseFacts-ledger.Used.ReleaseFacts, taskLimits.ReleaseFacts-ledger.Used.ReleaseFacts)
	influenceBudget := min64(ledger.Limits.InfluenceFacts-ledger.Used.InfluenceFacts, taskLimits.InfluenceFacts-ledger.Used.InfluenceFacts)
	if releaseBudget < 0 {
		releaseBudget = 0
	}
	if influenceBudget < 0 {
		influenceBudget = 0
	}
	plan, err := exposure.OptimizeEffects(request.Candidates, history, releaseBudget, influenceBudget, request.Weights)
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	charge, err := settleExposureTx(ctx, tx, now, queryID, &plan.UnionEffect)
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	selectedJSON, err := json.Marshal(plan.Selected)
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	selectedDigestBytes := sha256.Sum256(selectedJSON)
	selectedDigest := hex.EncodeToString(selectedDigestBytes[:])
	effects := make([]map[string]any, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		effectDigest, digestErr := exposure.ObservationDigest(candidate.Effect)
		if digestErr != nil {
			return exposure.ExactPlan{}, nil, digestErr
		}
		effects = append(effects, map[string]any{"id": candidate.ID, "requirement": candidate.Requirement,
			"plan_digest": candidate.PlanDigest, "effect_digest": effectDigest})
	}
	sort.Slice(effects, func(i, j int) bool { return effects[i]["id"].(string) < effects[j]["id"].(string) })
	effectsJSON, err := json.Marshal(effects)
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO representation_plans(query_id, task_id, root_task_id, profile_version, planner_version,
 snapshot_bundle_sha256, candidates_sha256, candidate_effects_json, selected_json, selected_sha256,
 union_effect_sha256, release_facts, influence_facts, utility, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, queryID, taskID, rootTaskID,
		exposure.ProfileV2, plan.PlannerVersion, request.SnapshotBundleSHA256, request.CandidatesSHA256,
		string(effectsJSON), string(selectedJSON), selectedDigest, plan.UnionEffectHash, plan.ReleaseCost, plan.InfluenceCost,
		plan.Utility, dbTime(now))
	if err != nil {
		return exposure.ExactPlan{}, nil, err
	}
	charge.PlannerVersion = plan.PlannerVersion
	charge.CandidatesSHA256 = request.CandidatesSHA256
	charge.SelectedSHA256 = selectedDigest
	charge.UnionEffectSHA256 = plan.UnionEffectHash
	charge.SnapshotBundleSHA256 = request.SnapshotBundleSHA256
	return plan, charge, nil
}

func exposureHistoryTx(ctx context.Context, tx *sql.Tx, rootTaskID string) (exposure.Observation, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT ledger_kind, identity_json FROM exposure_facts
WHERE root_task_id=$1 ORDER BY ledger_kind, fact_sha256`, rootTaskID)
	if err != nil {
		return exposure.Observation{}, err
	}
	defer rows.Close()
	history := exposure.Observation{ProfileVersion: exposure.ProfileV2}
	for rows.Next() {
		var kind string
		var identity []byte
		if err := rows.Scan(&kind, &identity); err != nil {
			return exposure.Observation{}, err
		}
		var fact exposure.FactID
		if err := json.Unmarshal(identity, &fact); err != nil {
			return exposure.Observation{}, err
		}
		if !fact.IsV2() {
			return exposure.Observation{}, fmt.Errorf("V2 root ledger contains a V1 fact")
		}
		if kind == "RELEASE" {
			history.Release = append(history.Release, fact)
		} else if kind == "INFLUENCE" {
			history.Influence = append(history.Influence, fact)
		}
	}
	if err := rows.Err(); err != nil {
		return exposure.Observation{}, err
	}
	return history.Normalize()
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
		statement.WriteString(`INSERT INTO exposure_facts(root_task_id, ledger_kind, fact_sha256, identity_json, canonical_payload, first_query_id, first_seen_at) VALUES `)
		args := make([]any, 0, (end-start)*7)
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
			payload, err := fact.CanonicalPayload()
			if err != nil {
				return 0, err
			}
			base := len(args) + 1
			fmt.Fprintf(&statement, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5, base+6)
			args = append(args, rootTaskID, kind, hash, string(identity), payload, queryID, dbTime(now))
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
		// ON CONFLICT is safe only after comparing the semantic payload. A
		// same-hash/different-payload row is a fail-closed collision.
		for _, fact := range facts[start:end] {
			hash, _ := fact.Hash()
			expectedPayload, _ := fact.CanonicalPayload()
			var storedIdentity []byte
			var storedPayload []byte
			if err := tx.QueryRowContext(ctx, `
SELECT identity_json, canonical_payload FROM exposure_facts
WHERE root_task_id=$1 AND ledger_kind=$2 AND fact_sha256=$3`, rootTaskID, kind, hash).
				Scan(&storedIdentity, &storedPayload); err != nil {
				return 0, err
			}
			if storedPayload != nil {
				if !bytes.Equal(storedPayload, expectedPayload) {
					return 0, fmt.Errorf("fact hash collision for %s", hash)
				}
				continue
			}
			expectedIdentity, _ := json.Marshal(fact)
			if !sameJSON(storedIdentity, expectedIdentity) {
				return 0, fmt.Errorf("fact hash collision for %s", hash)
			}
		}
	}
	return inserted, nil
}

func sameJSON(left, right []byte) bool {
	var leftValue, rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	if leftDecoder.Decode(&leftValue) != nil || rightDecoder.Decode(&rightValue) != nil {
		return false
	}
	leftCanonical, _ := json.Marshal(leftValue)
	rightCanonical, _ := json.Marshal(rightValue)
	return bytes.Equal(leftCanonical, rightCanonical)
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
	planErr := s.db.QueryRowContext(ctx, `
SELECT planner_version, candidates_sha256, selected_sha256, union_effect_sha256, snapshot_bundle_sha256
FROM representation_plans WHERE query_id=$1`, queryID).Scan(&charge.PlannerVersion, &charge.CandidatesSHA256,
		&charge.SelectedSHA256, &charge.UnionEffectSHA256, &charge.SnapshotBundleSHA256)
	if planErr != nil && !isNoRows(planErr) {
		return ExposureCharge{}, opErr(op, ErrConflict, planErr)
	}
	return charge, nil
}

func (s *Store) GetRepresentationPlan(ctx context.Context, queryID string) (RepresentationPlanRecord, error) {
	const op = "get representation plan"
	if err := s.checkOpen(op); err != nil {
		return RepresentationPlanRecord{}, err
	}
	var result RepresentationPlanRecord
	var selectedJSON []byte
	var created time.Time
	err := s.db.QueryRowContext(ctx, `
SELECT query_id, task_id, root_task_id, profile_version, planner_version,
       snapshot_bundle_sha256, candidates_sha256, selected_json, union_effect_sha256,
       release_facts, influence_facts, utility, created_at
FROM representation_plans WHERE query_id=$1`, queryID).Scan(&result.QueryID, &result.TaskID,
		&result.RootTaskID, &result.ProfileVersion, &result.PlannerVersion, &result.SnapshotBundleSHA256,
		&result.CandidatesSHA256, &selectedJSON, &result.UnionEffectSHA256, &result.ReleaseFacts,
		&result.InfluenceFacts, &result.Utility, &created)
	if err != nil {
		if isNoRows(err) {
			return RepresentationPlanRecord{}, opErr(op, ErrNotFound, err)
		}
		return RepresentationPlanRecord{}, opErr(op, ErrConflict, err)
	}
	if err := json.Unmarshal(selectedJSON, &result.Selected); err != nil {
		return RepresentationPlanRecord{}, opErr(op, ErrConflict, err)
	}
	result.CreatedAt = dbTime(created)
	return result, nil
}
