package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	outcome := grant.Limits.OutcomeFacts
	if release < 0 || influence < 0 || outcome < 0 {
		return fmt.Errorf("exposure limits cannot be negative")
	}
	if (release == 0) != (influence == 0) {
		return fmt.Errorf("release and influence limits must both be enabled or disabled")
	}
	if release > 0 && strings.TrimSpace(grant.ProfileVersion) == "" {
		return fmt.Errorf("exposure profile version is required")
	}
	if release > 0 && grant.ProfileVersion != exposure.ProfileV1 && grant.ProfileVersion != exposure.ProfileV2 && grant.ProfileVersion != exposure.ProfileV3 && grant.ProfileVersion != exposure.ProfileV4 && grant.ProfileVersion != exposure.ProfileV5 {
		return fmt.Errorf("unsupported exposure profile version")
	}
	if (grant.ProfileVersion == exposure.ProfileV3 || grant.ProfileVersion == exposure.ProfileV4 || grant.ProfileVersion == exposure.ProfileV5) && outcome <= 0 {
		return fmt.Errorf("V3/V4/V5 requires a positive outcome limit")
	}
	if grant.ProfileVersion != exposure.ProfileV3 && grant.ProfileVersion != exposure.ProfileV4 && grant.ProfileVersion != exposure.ProfileV5 && outcome != 0 {
		return fmt.Errorf("outcome limit requires V3/V4/V5")
	}
	if release == 0 && grant.ProfileVersion != "" {
		return fmt.Errorf("exposure profile requires positive limits")
	}
	if grant.ProfileVersion == exposure.ProfileV5 {
		limits := grant.PredicateFootprint
		if limits == nil || limits.Version != exposure.PredicateFootprintVersion ||
			limits.MaxRawLiteralsPerQuery <= 0 || limits.MaxUniqueAtomsPerQuery <= 0 ||
			limits.MaxUniqueAtomsPerQuery > 65536 || limits.MaxRawLiteralsPerQuery < limits.MaxUniqueAtomsPerQuery ||
			limits.MaxAtomPayloadBytes <= 0 || limits.MaxTotalAtomPayloadBytes < limits.MaxAtomPayloadBytes {
			return fmt.Errorf("V5 requires valid predicate footprint limits")
		}
	} else if grant.PredicateFootprint != nil {
		return fmt.Errorf("predicate footprint limits require V5")
	}
	return nil
}

func ensureExposureLedgerTx(ctx context.Context, tx *sql.Tx, taskID string, grant ExposureGrant, now time.Time) error {
	if err := validateExposureGrant(grant); err != nil {
		return err
	}
	if grant.ProfileVersion == exposure.ProfileV4 {
		return ensureOrdinalExposureHeadTx(ctx, tx, taskID, grant, now)
	}
	if grant.ProfileVersion == exposure.ProfileV5 {
		return ensureV5ExposureHeadTx(ctx, tx, taskID, grant, now)
	}
	var rootTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM tasks WHERE id=$1 FOR SHARE`, taskID).Scan(&rootTaskID); err != nil {
		return err
	}
	var existing ExposureLedgerSnapshot
	err := tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts, max_outcome_facts,
       used_release_facts, used_influence_facts, used_outcome_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, rootTaskID).
		Scan(&existing.RootTaskID, &existing.ProfileVersion, &existing.Limits.ReleaseFacts,
			&existing.Limits.InfluenceFacts, &existing.Limits.OutcomeFacts, &existing.Used.ReleaseFacts, &existing.Used.InfluenceFacts, &existing.Used.OutcomeFacts,
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
			grant.Limits.InfluenceFacts > existing.Limits.InfluenceFacts || grant.Limits.OutcomeFacts > existing.Limits.OutcomeFacts {
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
INSERT INTO exposure_ledgers(root_task_id, profile_version, max_release_facts, max_influence_facts, max_outcome_facts, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)`, rootTaskID, grant.ProfileVersion, grant.Limits.ReleaseFacts,
		grant.Limits.InfluenceFacts, grant.Limits.OutcomeFacts, dbTime(now))
	return err
}

func (s *Store) GetExposureLedger(ctx context.Context, taskID string) (ExposureLedgerSnapshot, error) {
	const op = "get exposure ledger"
	if err := s.checkOpen(op); err != nil {
		return ExposureLedgerSnapshot{}, err
	}
	result, err := getV5ExposureLedger(ctx, s.db, taskID)
	if err == nil {
		return result, nil
	}
	if !isNoRows(err) {
		return ExposureLedgerSnapshot{}, opErr(op, ErrConflict, err)
	}
	result, err = getOrdinalExposureLedger(ctx, s.db, taskID)
	if err == nil {
		return result, nil
	}
	if !isNoRows(err) {
		return ExposureLedgerSnapshot{}, opErr(op, ErrConflict, err)
	}
	result, err = getLegacyExposureLedger(ctx, s.db, taskID)
	if err != nil {
		if isNoRows(err) {
			return ExposureLedgerSnapshot{}, opErr(op, ErrNotFound, err)
		}
		return ExposureLedgerSnapshot{}, opErr(op, ErrConflict, err)
	}
	return result, nil
}

// getLegacyExposureLedger reads the V1--V3 ledger, which has no root head and
// therefore no epoch.
func getLegacyExposureLedger(ctx context.Context, source rowQueryer, taskID string) (ExposureLedgerSnapshot, error) {
	var result ExposureLedgerSnapshot
	var updated time.Time
	err := source.QueryRowContext(ctx, `
SELECT l.root_task_id, l.profile_version, l.max_release_facts, l.max_influence_facts, l.max_outcome_facts,
       l.used_release_facts, l.used_influence_facts, l.used_outcome_facts, l.updated_at
FROM tasks t JOIN exposure_ledgers l ON l.root_task_id=t.root_task_id
	WHERE t.id=$1`, taskID).Scan(&result.RootTaskID, &result.ProfileVersion, &result.Limits.ReleaseFacts,
		&result.Limits.InfluenceFacts, &result.Limits.OutcomeFacts, &result.Used.ReleaseFacts,
		&result.Used.InfluenceFacts, &result.Used.OutcomeFacts, &updated)
	if err != nil {
		return ExposureLedgerSnapshot{}, err
	}
	result.UpdatedAt = dbTime(updated)
	return result, nil
}

func reserveExposureTx(ctx context.Context, tx *sql.Tx, queryID, taskID string, request *ExposureReservationRequest, now time.Time) (*ExposureReservation, error) {
	var taskProfile string
	if err := tx.QueryRowContext(ctx, `SELECT exposure_profile_version FROM task_grants WHERE task_id=$1`, taskID).Scan(&taskProfile); err != nil {
		return nil, err
	}
	if taskProfile == exposure.ProfileV4 {
		return reserveOrdinalExposureTx(ctx, tx, queryID, taskID, request, now)
	}
	if taskProfile == exposure.ProfileV5 {
		return reserveV5ExposureTx(ctx, tx, queryID, taskID, request, now)
	}
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
		request.EstimatedReleaseFacts < 0 || request.EstimatedInfluenceFacts < 0 || request.EstimatedOutcomeFacts < 0 ||
		(profile == exposure.ProfileV3 && request.EstimatedOutcomeFacts == 0) ||
		(profile != exposure.ProfileV3 && request.EstimatedOutcomeFacts != 0) {
		return nil, fmt.Errorf("invalid exposure reservation")
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO query_exposure_reservations(query_id, task_id, root_task_id, profile_version, status,
       estimated_release_facts, estimated_influence_facts, estimated_outcome_facts, created_at)
VALUES ($1, $2, $3, $4, 'RESERVED', $5, $6, $7, $8)`, queryID, taskID, rootTaskID, profile,
		request.EstimatedReleaseFacts, request.EstimatedInfluenceFacts, request.EstimatedOutcomeFacts, dbTime(now))
	if err != nil {
		return nil, err
	}
	return &ExposureReservation{QueryID: queryID, TaskID: taskID, RootTaskID: rootTaskID,
		ProfileVersion: profile, EstimatedReleaseFacts: request.EstimatedReleaseFacts,
		EstimatedInfluenceFacts: request.EstimatedInfluenceFacts, EstimatedOutcomeFacts: request.EstimatedOutcomeFacts}, nil
}

type exposureSettlementMetrics struct {
	ReservationLock time.Duration
	LedgerLock      time.Duration
	FactStore       time.Duration
	OutcomeRadix    OutcomeRadixTelemetryV5
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
       estimated_influence_facts, estimated_outcome_facts, status, actual_release_facts, actual_influence_facts, actual_outcome_facts,
       charged_release_facts, charged_influence_facts, charged_outcome_facts, observation_sha256
FROM query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &reservation.EstimatedOutcomeFacts, &status,
			&actual.ReleaseFacts, &actual.InfluenceFacts, &actual.OutcomeFacts, &charged.ReleaseFacts, &charged.InfluenceFacts, &charged.OutcomeFacts,
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
	if (reservation.ProfileVersion == exposure.ProfileV3 && len(normalized.Outcome) != 1) ||
		(reservation.ProfileVersion != exposure.ProfileV3 && len(normalized.Outcome) != 0) {
		return nil, metrics, fmt.Errorf("exposure observation has an invalid outcome fact count")
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
			ActualInfluenceFacts: actual.InfluenceFacts, ActualOutcomeFacts: actual.OutcomeFacts, ChargedReleaseFacts: charged.ReleaseFacts,
			ChargedInfluenceFacts: charged.InfluenceFacts, ChargedOutcomeFacts: charged.OutcomeFacts, ObservationSHA256: digest}, metrics, nil
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("exposure reservation is %s", status)
	}

	var ledger ExposureLedgerSnapshot
	ledgerLockStarted := time.Now()
	err = tx.QueryRowContext(ctx, `
SELECT root_task_id, profile_version, max_release_facts, max_influence_facts, max_outcome_facts,
       used_release_facts, used_influence_facts, used_outcome_facts, updated_at
FROM exposure_ledgers WHERE root_task_id=$1 FOR UPDATE`, reservation.RootTaskID).
		Scan(&ledger.RootTaskID, &ledger.ProfileVersion, &ledger.Limits.ReleaseFacts,
			&ledger.Limits.InfluenceFacts, &ledger.Limits.OutcomeFacts, &ledger.Used.ReleaseFacts, &ledger.Used.InfluenceFacts, &ledger.Used.OutcomeFacts,
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
	if err != nil {
		return nil, metrics, err
	}
	newOutcome, err := insertNovelFactsTx(ctx, tx, reservation.RootTaskID, "OUTCOME", queryID, normalized.Outcome, now)
	metrics.FactStore = time.Since(factStoreStarted)
	if err != nil {
		return nil, metrics, err
	}
	for _, observed := range []struct {
		kind  string
		facts []exposure.FactID
	}{
		{kind: "RELEASE", facts: normalized.Release},
		{kind: "INFLUENCE", facts: normalized.Influence},
		{kind: "OUTCOME", facts: normalized.Outcome},
	} {
		if err := linkObservedFactsTx(ctx, tx, reservation.RootTaskID, queryID, observed.kind, observed.facts); err != nil {
			return nil, metrics, err
		}
	}
	var taskLimits ExposureLimits
	if err := tx.QueryRowContext(ctx, `
	SELECT max_release_facts, max_influence_facts, max_outcome_facts FROM task_grants WHERE task_id=$1`, reservation.TaskID).
		Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts, &taskLimits.OutcomeFacts); err != nil {
		return nil, metrics, err
	}
	// A narrowed descendant may add facts only within its signed absolute
	// family ceiling. Zero-novelty replay remains safe after an ancestor has
	// already moved the shared root ledger beyond that lower ceiling.
	if (newRelease > 0 && ledger.Used.ReleaseFacts+newRelease > taskLimits.ReleaseFacts) ||
		(newInfluence > 0 && ledger.Used.InfluenceFacts+newInfluence > taskLimits.InfluenceFacts) ||
		(newOutcome > 0 && ledger.Used.OutcomeFacts+newOutcome > taskLimits.OutcomeFacts) {
		return nil, metrics, ErrExposureBudgetExhausted
	}
	result, err := tx.ExecContext(ctx, `
UPDATE exposure_ledgers
SET used_release_facts=used_release_facts+$1,
    used_influence_facts=used_influence_facts+$2,
    used_outcome_facts=used_outcome_facts+$3,
    updated_at=$4
WHERE root_task_id=$5
  AND used_release_facts+$1 <= max_release_facts
  AND used_influence_facts+$2 <= max_influence_facts
  AND used_outcome_facts+$3 <= max_outcome_facts`, newRelease, newInfluence, newOutcome, dbTime(now), reservation.RootTaskID)
	if err != nil {
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, ErrExposureBudgetExhausted
	}
	_, err = tx.ExecContext(ctx, `
UPDATE query_exposure_reservations
SET status='SETTLED', actual_release_facts=$1, actual_influence_facts=$2,
    actual_outcome_facts=$3, charged_release_facts=$4, charged_influence_facts=$5,
    charged_outcome_facts=$6, observation_sha256=$7, settled_at=$8
WHERE query_id=$9 AND status='RESERVED'`, len(normalized.Release), len(normalized.Influence), len(normalized.Outcome),
		newRelease, newInfluence, newOutcome, digest, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: reservation.TaskID, QueryID: queryID, Actor: "system", EventType: "QUERY_EXPOSURE_SETTLED",
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID, "profile_version": reservation.ProfileVersion,
			"actual_release_facts": len(normalized.Release), "actual_influence_facts": len(normalized.Influence), "actual_outcome_facts": len(normalized.Outcome),
			"charged_release_facts": newRelease, "charged_influence_facts": newInfluence, "charged_outcome_facts": newOutcome,
			"observation_sha256": digest}), OccurredAt: now,
	})
	if err != nil {
		return nil, metrics, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID,
		ProfileVersion: reservation.ProfileVersion, ActualReleaseFacts: int64(len(normalized.Release)),
		ActualInfluenceFacts: int64(len(normalized.Influence)), ActualOutcomeFacts: int64(len(normalized.Outcome)), ChargedReleaseFacts: newRelease,
		ChargedInfluenceFacts: newInfluence, ChargedOutcomeFacts: newOutcome, ObservationSHA256: digest}, metrics, nil
}

func linkObservedFactsTx(ctx context.Context, tx *sql.Tx, rootTaskID, queryID, kind string, facts []exposure.FactID) error {
	const chunkSize = 5000
	for start := 0; start < len(facts); start += chunkSize {
		end := start + chunkSize
		if end > len(facts) {
			end = len(facts)
		}
		var statement strings.Builder
		statement.WriteString(`INSERT INTO query_exposure_facts(root_task_id, query_id, ledger_kind, fact_sha256) VALUES `)
		args := make([]any, 0, (end-start)*4)
		for index, fact := range facts[start:end] {
			hash, err := fact.Hash()
			if err != nil {
				return err
			}
			if index != 0 {
				statement.WriteString(",")
			}
			base := len(args) + 1
			fmt.Fprintf(&statement, "($%d,$%d,$%d,$%d)", base, base+1, base+2, base+3)
			args = append(args, rootTaskID, queryID, kind, hash)
		}
		statement.WriteString(` ON CONFLICT DO NOTHING`)
		if _, err := tx.ExecContext(ctx, statement.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func insertNovelFactsTx(ctx context.Context, tx *sql.Tx, rootTaskID, kind, queryID string, facts []exposure.FactID, now time.Time) (int64, error) {
	const chunkSize = 5000
	type encodedFact struct {
		hash     string
		identity []byte
		payload  []byte
	}
	var inserted int64
	for start := 0; start < len(facts); start += chunkSize {
		end := start + chunkSize
		if end > len(facts) {
			end = len(facts)
		}
		encodedFacts := make([]encodedFact, 0, end-start)
		for _, fact := range facts[start:end] {
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
			encodedFacts = append(encodedFacts, encodedFact{hash: hash, identity: identity, payload: payload})
		}
		// ON CONFLICT is safe only after comparing every semantic payload. Do
		// that in one fixed-shape array query per chunk before attempting writes.
		// The caller holds the root ledger FOR UPDATE, so a same-root settlement
		// cannot appear between this read and the following insert.
		expected := make(map[string]encodedFact, len(encodedFacts))
		hashes := make([]string, 0, len(encodedFacts))
		for _, fact := range encodedFacts {
			hashes = append(hashes, fact.hash)
			expected[fact.hash] = fact
		}
		storedRows, err := tx.QueryContext(ctx, `SELECT fact_sha256, identity_json, canonical_payload FROM exposure_facts
	WHERE root_task_id=$1 AND ledger_kind=$2 AND fact_sha256 = ANY($3)`, rootTaskID, kind, hashes)
		if err != nil {
			return 0, err
		}
		seen := make(map[string]struct{}, len(encodedFacts))
		for storedRows.Next() {
			var hash string
			var storedIdentity, storedPayload []byte
			if err := storedRows.Scan(&hash, &storedIdentity, &storedPayload); err != nil {
				storedRows.Close()
				return 0, err
			}
			fact, present := expected[hash]
			if !present || (storedPayload != nil && !bytes.Equal(storedPayload, fact.payload)) ||
				(storedPayload == nil && !sameJSON(storedIdentity, fact.identity)) {
				storedRows.Close()
				return 0, fmt.Errorf("fact hash collision for %s", hash)
			}
			seen[hash] = struct{}{}
		}
		if err := storedRows.Err(); err != nil {
			storedRows.Close()
			return 0, err
		}
		if err := storedRows.Close(); err != nil {
			return 0, err
		}
		missing := make([]encodedFact, 0, len(encodedFacts)-len(seen))
		for _, fact := range encodedFacts {
			if _, present := seen[fact.hash]; !present {
				missing = append(missing, fact)
			}
		}
		if len(missing) == 0 {
			continue
		}
		var statement strings.Builder
		statement.WriteString(`INSERT INTO exposure_facts(root_task_id, ledger_kind, fact_sha256, identity_json, canonical_payload, first_query_id, first_seen_at) VALUES `)
		args := make([]any, 0, len(missing)*7)
		for index, fact := range missing {
			if index != 0 {
				statement.WriteString(",")
			}
			base := len(args) + 1
			fmt.Fprintf(&statement, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4, base+5, base+6)
			args = append(args, rootTaskID, kind, fact.hash, string(fact.identity), fact.payload, queryID, dbTime(now))
		}
		statement.WriteString(` ON CONFLICT DO NOTHING RETURNING fact_sha256`)
		rows, err := tx.QueryContext(ctx, statement.String(), args...)
		if err != nil {
			return 0, err
		}
		var chunkInserted int
		for rows.Next() {
			var ignored string
			if err := rows.Scan(&ignored); err != nil {
				rows.Close()
				return 0, err
			}
			chunkInserted++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
		if chunkInserted != len(missing) {
			return 0, errors.New("fact insertion conflicted despite the root ledger lock")
		}
		inserted += int64(chunkInserted)
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
	charge, err := getOrdinalExposureCharge(ctx, s.db, queryID)
	if err == nil {
		return charge, nil
	}
	if !isNoRows(err) {
		return ExposureCharge{}, opErr(op, ErrConflict, err)
	}
	charge, err = getV5ExposureCharge(ctx, s.db, queryID)
	if err == nil {
		return charge, nil
	}
	if !isNoRows(err) {
		return ExposureCharge{}, opErr(op, ErrConflict, err)
	}
	charge = ExposureCharge{}
	var status string
	err = s.db.QueryRowContext(ctx, `
SELECT query_id, root_task_id, profile_version, status, actual_release_facts,
       actual_influence_facts, actual_outcome_facts, charged_release_facts, charged_influence_facts, charged_outcome_facts, observation_sha256
FROM query_exposure_reservations WHERE query_id=$1`, queryID).
		Scan(&charge.QueryID, &charge.RootTaskID, &charge.ProfileVersion, &status,
			&charge.ActualReleaseFacts, &charge.ActualInfluenceFacts, &charge.ActualOutcomeFacts, &charge.ChargedReleaseFacts,
			&charge.ChargedInfluenceFacts, &charge.ChargedOutcomeFacts, &charge.ObservationSHA256)
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
