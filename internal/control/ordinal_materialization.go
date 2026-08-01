package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PublishOrdinalMaterialization publishes an entry for an already committed
// V4 query. FinalizeOrdinalQueryWithReceipt is preferred because it publishes
// in the result/ledger transaction.
func (s *Store) PublishOrdinalMaterialization(ctx context.Context, sourceQueryID string,
	request OrdinalMaterializationPublish) (OrdinalMaterialization, error) {
	const op = "publish ordinal materialization"
	if err := s.checkOpen(op); err != nil {
		return OrdinalMaterialization{}, err
	}
	if sourceQueryID == "" {
		return OrdinalMaterialization{}, opErr(op, ErrInvalid, fmt.Errorf("source query is required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	materialization, _, err := publishOrdinalMaterializationTx(ctx, tx, s.now(), sourceQueryID, request)
	if err != nil {
		return OrdinalMaterialization{}, opErr(op, materializationErrorKind(err), err)
	}
	if err := tx.Commit(); err != nil {
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	return materialization, nil
}

func publishOrdinalMaterializationTx(ctx context.Context, tx *sql.Tx, now time.Time, sourceQueryID string,
	request OrdinalMaterializationPublish) (OrdinalMaterialization, bool, error) {
	if !validSHA256Hex(request.CacheKeySHA256) {
		return OrdinalMaterialization{}, false, fmt.Errorf("invalid materialization cache key")
	}
	profile, cacheTable, queryObservationTable, observationTable, err := ordinalMaterializationTables(request.ProfileVersion)
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	now = dbTime(now)
	var expiresAt *time.Time
	if request.ExpiresAt != nil {
		expires := dbTime(*request.ExpiresAt)
		if !expires.After(now) {
			return OrdinalMaterialization{}, false, fmt.Errorf("materialization expiry must be in the future")
		}
		expiresAt = &expires
	}
	materialization := OrdinalMaterialization{ProfileVersion: profile}
	var status QueryStatus
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`
SELECT query.task_id,query.status,query.grant_digest,query.catalog_digest,query.result_sha256,
 query.result_rows,reference.root_task_id,reference.observation_sha256,observation.dictionary_set_digest,
 query.id
FROM query_records query
JOIN %s reference ON reference.query_id=query.id
JOIN %s observation ON observation.observation_sha256=reference.observation_sha256
WHERE query.id=$1 FOR SHARE OF query,reference,observation`, queryObservationTable, observationTable), sourceQueryID).
		Scan(&materialization.TaskID, &status, &materialization.GrantDigest, &materialization.CatalogDigest,
			&materialization.ResultSHA256, &materialization.RowCount, &materialization.RootTaskID,
			&materialization.Observation.ObservationSHA256, &materialization.Observation.DictionarySetDigest,
			&materialization.SourceQueryID)
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	if status != QueryCompleted || materialization.ResultSHA256 == "" {
		return OrdinalMaterialization{}, false, fmt.Errorf("source query is not a committed %s result", profile)
	}
	materialization.ResultKeyID, err = ordinalMaterializationSourceKeyTx(ctx, tx,
		materialization.SourceQueryID, materialization.TaskID, materialization.ResultSHA256, false, now)
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	materialization.CacheKeySHA256 = request.CacheKeySHA256
	materialization.CreatedAt = now
	materialization.ExpiresAt = expiresAt
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s(cache_key_sha256,task_id,root_task_id,source_query_id,
 observation_sha256,dictionary_set_digest,grant_digest,catalog_digest,result_sha256,row_count,created_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (cache_key_sha256) DO NOTHING`, cacheTable), materialization.CacheKeySHA256, materialization.TaskID,
		materialization.RootTaskID, materialization.SourceQueryID, materialization.Observation.ObservationSHA256,
		materialization.Observation.DictionarySetDigest, materialization.GrantDigest, materialization.CatalogDigest,
		materialization.ResultSHA256, materialization.RowCount, materialization.CreatedAt,
		nullableTime(materialization.ExpiresAt))
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		existing, err := getOrdinalMaterializationTx(ctx, tx, materialization.CacheKeySHA256, profile, false, now)
		if err != nil {
			return OrdinalMaterialization{}, false, err
		}
		if !equivalentOrdinalMaterializationEvidence(existing, materialization) {
			return OrdinalMaterialization{}, false, fmt.Errorf("%w for cache key %s",
				ErrMaterializationConflict, materialization.CacheKeySHA256)
		}
		// Two distinct requests may both miss before either has published. The
		// first committed source remains the immutable cache target; an
		// equivalent contender converges on it while still committing its own
		// query result, receipt and zero/novel exposure charge. The observation's
		// Release/Outcome commitment plus row count is the semantic result
		// evidence. ResultSHA256 covers the complete stored envelope (including
		// request-local timing metrics), so it and expiry are deliberately
		// first-writer cache metadata rather than convergence identity.
		if existing.SourceQueryID != materialization.SourceQueryID {
			_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: materialization.TaskID,
				QueryID: materialization.SourceQueryID, Actor: "system",
				EventType: "ORDINAL_MATERIALIZATION_CONVERGED", OccurredAt: now,
				Payload: mustJSON(map[string]any{"cache_key_sha256": materialization.CacheKeySHA256,
					"committed_source_query_id": existing.SourceQueryID,
					"contender_query_id":        materialization.SourceQueryID,
					"observation_sha256":        materialization.Observation.ObservationSHA256,
					"committed_result_sha256":   existing.ResultSHA256,
					"contender_result_sha256":   materialization.ResultSHA256})})
			if err != nil {
				return OrdinalMaterialization{}, false, err
			}
		}
		return existing, false, nil
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: materialization.TaskID,
		QueryID: materialization.SourceQueryID, Actor: "system", EventType: "ORDINAL_MATERIALIZATION_COMMITTED",
		OccurredAt: now, Payload: mustJSON(map[string]any{"cache_key_sha256": materialization.CacheKeySHA256,
			"root_task_id":          materialization.RootTaskID,
			"observation_sha256":    materialization.Observation.ObservationSHA256,
			"dictionary_set_digest": materialization.Observation.DictionarySetDigest,
			"grant_digest":          materialization.GrantDigest, "catalog_digest": materialization.CatalogDigest,
			"result_sha256": materialization.ResultSHA256})})
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	return materialization, true, nil
}

// LookupOrdinalMaterialization requires every authorization/snapshot binding
// independently of the semantic cache key. It returns only entries whose
// legacy ciphertext, or AVAILABLE Parquet artifact, and active key still
// exist. A PENDING artifact may be published but cannot be replayed yet.
func (s *Store) LookupOrdinalMaterialization(ctx context.Context,
	lookup OrdinalMaterializationLookup) (OrdinalMaterialization, error) {
	const op = "lookup ordinal materialization"
	if err := s.checkOpen(op); err != nil {
		return OrdinalMaterialization{}, err
	}
	if !validSHA256Hex(lookup.CacheKeySHA256) || lookup.TaskID == "" ||
		!validSHA256Hex(lookup.GrantDigest) || !validSHA256Hex(lookup.CatalogDigest) ||
		!validSHA256Hex(lookup.DictionarySetDigest) {
		return OrdinalMaterialization{}, opErr(op, ErrInvalid, fmt.Errorf("complete cache and authorization binding is required"))
	}
	profile, cacheTable, _, _, profileErr := ordinalMaterializationTables(lookup.ProfileVersion)
	if profileErr != nil {
		return OrdinalMaterialization{}, opErr(op, ErrInvalid, profileErr)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	now := s.now()
	result := OrdinalMaterialization{ProfileVersion: profile}
	var created time.Time
	var expires sql.NullTime
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`
SELECT cache.cache_key_sha256,cache.task_id,cache.root_task_id,cache.source_query_id,
 cache.observation_sha256,cache.dictionary_set_digest,cache.grant_digest,cache.catalog_digest,
 cache.result_sha256,cache.row_count,cache.created_at,cache.expires_at
FROM %s cache
JOIN query_records source_query ON source_query.id=cache.source_query_id AND source_query.status='COMPLETED'
WHERE cache.cache_key_sha256=$1 AND cache.task_id=$2 AND cache.grant_digest=$3
  AND cache.catalog_digest=$4 AND cache.dictionary_set_digest=$5
  AND (cache.expires_at IS NULL OR cache.expires_at>$6)
FOR SHARE OF cache,source_query`, cacheTable), lookup.CacheKeySHA256, lookup.TaskID, lookup.GrantDigest,
		lookup.CatalogDigest, lookup.DictionarySetDigest, dbTime(now)).
		Scan(&result.CacheKeySHA256, &result.TaskID, &result.RootTaskID, &result.SourceQueryID,
			&result.Observation.ObservationSHA256, &result.Observation.DictionarySetDigest,
			&result.GrantDigest, &result.CatalogDigest, &result.ResultSHA256,
			&result.RowCount, &created, &expires)
	if err != nil {
		if isNoRows(err) {
			return OrdinalMaterialization{}, opErr(op, ErrNotFound, err)
		}
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	result.ResultKeyID, err = ordinalMaterializationSourceKeyTx(ctx, tx, result.SourceQueryID,
		result.TaskID, result.ResultSHA256, true, now)
	if err != nil {
		if isNoRows(err) {
			return OrdinalMaterialization{}, opErr(op, ErrNotFound, err)
		}
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	result.CreatedAt = dbTime(created)
	result.ExpiresAt = scanNullableTime(expires)
	if err := tx.Commit(); err != nil {
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	return result, nil
}

// DeleteUnusableOrdinalMaterialization removes only an exactly bound cache
// row whose expiry, result retention, or key-erasure state makes replay
// impossible. PENDING artifacts are retained because promotion/recovery is a
// normal publication window, even though lookup does not expose them until
// AVAILABLE. A lookup binding mismatch is deliberately left untouched so a
// cache-key collision still fails closed when a contender tries to publish.
// Ledger, observation, query, receipt, and audit evidence are never removed.
func (s *Store) DeleteUnusableOrdinalMaterialization(ctx context.Context,
	lookup OrdinalMaterializationLookup) (bool, error) {
	const op = "delete unusable ordinal materialization"
	if err := s.checkOpen(op); err != nil {
		return false, err
	}
	if !validSHA256Hex(lookup.CacheKeySHA256) || lookup.TaskID == "" ||
		!validSHA256Hex(lookup.GrantDigest) || !validSHA256Hex(lookup.CatalogDigest) ||
		!validSHA256Hex(lookup.DictionarySetDigest) {
		return false, opErr(op, ErrInvalid, fmt.Errorf("complete cache and authorization binding is required"))
	}
	_, cacheTable, _, _, profileErr := ordinalMaterializationTables(lookup.ProfileVersion)
	if profileErr != nil {
		return false, opErr(op, ErrInvalid, profileErr)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s cache
WHERE cache.cache_key_sha256=$1 AND cache.task_id=$2 AND cache.grant_digest=$3
  AND cache.catalog_digest=$4 AND cache.dictionary_set_digest=$5
  AND (
    (cache.expires_at IS NOT NULL AND cache.expires_at <= $6)
    OR (
      NOT EXISTS (
      SELECT 1
      FROM encrypted_query_results encrypted
      JOIN result_encryption_keys key ON key.key_id=encrypted.key_id AND key.status='ACTIVE'
      WHERE encrypted.query_id=cache.source_query_id
        AND encrypted.task_id=cache.task_id
        AND encrypted.plaintext_sha256=cache.result_sha256
      )
      AND NOT EXISTS (
        SELECT 1
        FROM result_artifacts artifact
        JOIN result_encryption_keys key ON key.key_id=artifact.key_id AND key.status='ACTIVE'
        WHERE artifact.query_id=cache.source_query_id
          AND artifact.task_id=cache.task_id
          AND artifact.parquet_sha256=cache.result_sha256
          AND (
            artifact.status='PENDING'
            OR (artifact.status='AVAILABLE' AND (artifact.expires_at IS NULL OR artifact.expires_at>$6))
          )
      )
    )
	  )`, cacheTable), lookup.CacheKeySHA256, lookup.TaskID, lookup.GrantDigest, lookup.CatalogDigest,
		lookup.DictionarySetDigest, dbTime(s.now()))
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	if affected == 0 {
		if err := tx.Commit(); err != nil {
			return false, opErr(op, ErrConflict, err)
		}
		return false, nil
	}
	if affected != 1 {
		return false, opErr(op, ErrConflict, fmt.Errorf("cache key removed more than one materialization"))
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: lookup.TaskID, Actor: "system",
		EventType: "ORDINAL_MATERIALIZATION_UNUSABLE_EVICTED", OccurredAt: s.now(),
		Payload: mustJSON(map[string]any{"cache_key_sha256": lookup.CacheKeySHA256})})
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	return true, nil
}

func getOrdinalMaterializationTx(ctx context.Context, tx *sql.Tx, cacheKey, profileVersion string, requireUsable bool,
	now time.Time) (OrdinalMaterialization, error) {
	profile, cacheTable, _, _, err := ordinalMaterializationTables(profileVersion)
	if err != nil {
		return OrdinalMaterialization{}, err
	}
	query := fmt.Sprintf(`
SELECT cache.cache_key_sha256,cache.task_id,cache.root_task_id,cache.source_query_id,
 cache.observation_sha256,cache.dictionary_set_digest,cache.grant_digest,cache.catalog_digest,
 cache.result_sha256,cache.row_count,cache.created_at,cache.expires_at
FROM %s cache
JOIN query_records source_query ON source_query.id=cache.source_query_id AND source_query.status='COMPLETED'
WHERE cache.cache_key_sha256=$1`, cacheTable)
	if requireUsable {
		query += ` AND (cache.expires_at IS NULL OR cache.expires_at>$2)`
	}
	query += ` FOR SHARE OF cache,source_query`
	var row *sql.Row
	if requireUsable {
		row = tx.QueryRowContext(ctx, query, cacheKey, dbTime(now))
	} else {
		row = tx.QueryRowContext(ctx, query, cacheKey)
	}
	result := OrdinalMaterialization{ProfileVersion: profile}
	var created time.Time
	var expires sql.NullTime
	if err := row.Scan(&result.CacheKeySHA256, &result.TaskID, &result.RootTaskID, &result.SourceQueryID,
		&result.Observation.ObservationSHA256, &result.Observation.DictionarySetDigest,
		&result.GrantDigest, &result.CatalogDigest, &result.ResultSHA256,
		&result.RowCount, &created, &expires); err != nil {
		return OrdinalMaterialization{}, err
	}
	keyID, err := ordinalMaterializationSourceKeyTx(ctx, tx, result.SourceQueryID,
		result.TaskID, result.ResultSHA256, requireUsable, now)
	if err != nil {
		return OrdinalMaterialization{}, err
	}
	result.ResultKeyID = keyID
	result.CreatedAt = dbTime(created)
	result.ExpiresAt = scanNullableTime(expires)
	return result, nil
}

// ordinalMaterializationSourceKeyTx resolves the immutable result digest to
// either the new artifact metadata or the legacy PostgreSQL ciphertext. During
// publication, a PENDING artifact is valid because object promotion follows
// the Control transaction. During replay, only AVAILABLE artifacts backed by
// an ACTIVE key are returned. Legacy results retain their former behavior.
func ordinalMaterializationSourceKeyTx(ctx context.Context, tx *sql.Tx, queryID, taskID, resultSHA256 string,
	requireUsable bool, now time.Time) (string, error) {
	artifactQuery := `
SELECT artifact.key_id
FROM result_artifacts artifact`
	if requireUsable {
		artifactQuery += `
JOIN result_encryption_keys encryption_key
  ON encryption_key.key_id=artifact.key_id AND encryption_key.status='ACTIVE'`
	}
	artifactQuery += `
WHERE artifact.query_id=$1 AND artifact.task_id=$2 AND artifact.parquet_sha256=$3`
	if requireUsable {
		artifactQuery += ` AND artifact.status='AVAILABLE'
  AND (artifact.expires_at IS NULL OR artifact.expires_at>$4)
FOR SHARE OF artifact,encryption_key`
	} else {
		artifactQuery += ` AND artifact.status IN ('PENDING','AVAILABLE')
FOR SHARE OF artifact`
	}
	var keyID string
	arguments := []any{queryID, taskID, resultSHA256}
	if requireUsable {
		arguments = append(arguments, dbTime(now))
	}
	err := tx.QueryRowContext(ctx, artifactQuery, arguments...).Scan(&keyID)
	if err == nil {
		return keyID, nil
	}
	if !isNoRows(err) {
		return "", err
	}

	legacyQuery := `
SELECT encrypted.key_id
FROM encrypted_query_results encrypted`
	if requireUsable {
		legacyQuery += `
JOIN result_encryption_keys encryption_key
  ON encryption_key.key_id=encrypted.key_id AND encryption_key.status='ACTIVE'`
	}
	legacyQuery += `
WHERE encrypted.query_id=$1 AND encrypted.task_id=$2 AND encrypted.plaintext_sha256=$3`
	if requireUsable {
		legacyQuery += ` FOR SHARE OF encrypted,encryption_key`
	} else {
		legacyQuery += ` FOR SHARE OF encrypted`
	}
	err = tx.QueryRowContext(ctx, legacyQuery, queryID, taskID, resultSHA256).Scan(&keyID)
	if err != nil {
		return "", err
	}
	return keyID, nil
}

// DeleteOrdinalMaterialization evicts cache state only; the source query,
// observation, ledger and audit evidence remain intact.
func (s *Store) DeleteOrdinalMaterialization(ctx context.Context, taskID, cacheKey string) error {
	const op = "delete ordinal materialization"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if taskID == "" || !validSHA256Hex(cacheKey) {
		return opErr(op, ErrInvalid, fmt.Errorf("task and cache key are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	var v4Exists, v5Exists bool
	if err := tx.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM v4_committed_materializations WHERE task_id=$1 AND cache_key_sha256=$2),
 EXISTS(SELECT 1 FROM v5_committed_materializations WHERE task_id=$1 AND cache_key_sha256=$2)`,
		taskID, cacheKey).Scan(&v4Exists, &v5Exists); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if v4Exists == v5Exists {
		if !v4Exists {
			return opErr(op, ErrNotFound, sql.ErrNoRows)
		}
		return opErr(op, ErrMaterializationConflict, errors.New("cache key exists in both V4 and V5"))
	}
	table := "v4_committed_materializations"
	if v5Exists {
		table = "v5_committed_materializations"
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s
WHERE task_id=$1 AND cache_key_sha256=$2`, table), taskID, cacheKey)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return opErr(op, ErrNotFound, sql.ErrNoRows)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: taskID, Actor: "system",
		EventType: "ORDINAL_MATERIALIZATION_EVICTED", OccurredAt: s.now(),
		Payload: mustJSON(map[string]any{"cache_key_sha256": cacheKey})})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func equivalentOrdinalMaterializationEvidence(left, right OrdinalMaterialization) bool {
	return left.CacheKeySHA256 == right.CacheKeySHA256 && left.TaskID == right.TaskID &&
		left.ProfileVersion == right.ProfileVersion && left.RootTaskID == right.RootTaskID &&
		left.Observation == right.Observation && left.GrantDigest == right.GrantDigest &&
		left.CatalogDigest == right.CatalogDigest && left.RowCount == right.RowCount
}

func ordinalMaterializationTables(profile string) (normalized, cache, queryObservation, observation string, err error) {
	if profile == "" {
		profile = "taskgate-exposure-v4"
	}
	switch profile {
	case "taskgate-exposure-v4":
		return profile, "v4_committed_materializations", "v4_query_observations", "v4_observations", nil
	case "taskgate-exposure-v5":
		return profile, "v5_committed_materializations", "v5_query_observations", "v5_observations", nil
	default:
		return "", "", "", "", fmt.Errorf("unsupported ordinal materialization profile %q", profile)
	}
}

func materializationErrorKind(err error) error {
	if errors.Is(err, ErrMaterializationConflict) {
		return ErrMaterializationConflict
	}
	if isNoRows(err) {
		return ErrNotFound
	}
	if err != nil && (err.Error() == "invalid materialization cache key" ||
		err.Error() == "materialization expiry must be in the future") {
		return ErrInvalid
	}
	return ErrConflict
}
