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
	now = dbTime(now)
	var expiresAt *time.Time
	if request.ExpiresAt != nil {
		expires := dbTime(*request.ExpiresAt)
		if !expires.After(now) {
			return OrdinalMaterialization{}, false, fmt.Errorf("materialization expiry must be in the future")
		}
		expiresAt = &expires
	}
	var materialization OrdinalMaterialization
	var status QueryStatus
	var encryptedHash string
	err := tx.QueryRowContext(ctx, `
SELECT query.task_id,query.status,query.grant_digest,query.catalog_digest,query.result_sha256,
 query.result_rows,reference.root_task_id,reference.observation_sha256,observation.dictionary_set_digest,
 encrypted.plaintext_sha256,encrypted.key_id
FROM query_records query
JOIN v4_query_observations reference ON reference.query_id=query.id
JOIN v4_observations observation ON observation.observation_sha256=reference.observation_sha256
JOIN encrypted_query_results encrypted ON encrypted.query_id=query.id
WHERE query.id=$1 FOR SHARE OF query,reference,observation,encrypted`, sourceQueryID).
		Scan(&materialization.TaskID, &status, &materialization.GrantDigest, &materialization.CatalogDigest,
			&materialization.ResultSHA256, &materialization.RowCount, &materialization.RootTaskID,
			&materialization.Observation.ObservationSHA256, &materialization.Observation.DictionarySetDigest,
			&encryptedHash, &materialization.ResultKeyID)
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	if status != QueryCompleted || materialization.ResultSHA256 == "" || encryptedHash != materialization.ResultSHA256 {
		return OrdinalMaterialization{}, false, fmt.Errorf("source query is not a committed encrypted V4 result")
	}
	materialization.CacheKeySHA256 = request.CacheKeySHA256
	materialization.SourceQueryID = sourceQueryID
	materialization.CreatedAt = now
	materialization.ExpiresAt = expiresAt
	result, err := tx.ExecContext(ctx, `
INSERT INTO v4_committed_materializations(cache_key_sha256,task_id,root_task_id,source_query_id,
 observation_sha256,dictionary_set_digest,grant_digest,catalog_digest,result_sha256,row_count,created_at,expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (cache_key_sha256) DO NOTHING`, materialization.CacheKeySHA256, materialization.TaskID,
		materialization.RootTaskID, materialization.SourceQueryID, materialization.Observation.ObservationSHA256,
		materialization.Observation.DictionarySetDigest, materialization.GrantDigest, materialization.CatalogDigest,
		materialization.ResultSHA256, materialization.RowCount, materialization.CreatedAt,
		nullableTime(materialization.ExpiresAt))
	if err != nil {
		return OrdinalMaterialization{}, false, err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		existing, err := getOrdinalMaterializationTx(ctx, tx, materialization.CacheKeySHA256, false, now)
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
// encrypted source result and active key still exist.
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
	var result OrdinalMaterialization
	var created time.Time
	var expires sql.NullTime
	err := s.db.QueryRowContext(ctx, `
SELECT cache.cache_key_sha256,cache.task_id,cache.root_task_id,cache.source_query_id,
 cache.observation_sha256,cache.dictionary_set_digest,cache.grant_digest,cache.catalog_digest,
 cache.result_sha256,encrypted.key_id,cache.row_count,cache.created_at,cache.expires_at
FROM v4_committed_materializations cache
JOIN query_records query ON query.id=cache.source_query_id AND query.status='COMPLETED'
JOIN encrypted_query_results encrypted ON encrypted.query_id=cache.source_query_id
    AND encrypted.plaintext_sha256=cache.result_sha256
JOIN result_encryption_keys key ON key.key_id=encrypted.key_id AND key.status='ACTIVE'
WHERE cache.cache_key_sha256=$1 AND cache.task_id=$2 AND cache.grant_digest=$3
  AND cache.catalog_digest=$4 AND cache.dictionary_set_digest=$5
  AND (cache.expires_at IS NULL OR cache.expires_at>$6)`, lookup.CacheKeySHA256, lookup.TaskID,
		lookup.GrantDigest, lookup.CatalogDigest, lookup.DictionarySetDigest, dbTime(s.now())).
		Scan(&result.CacheKeySHA256, &result.TaskID, &result.RootTaskID, &result.SourceQueryID,
			&result.Observation.ObservationSHA256, &result.Observation.DictionarySetDigest,
			&result.GrantDigest, &result.CatalogDigest, &result.ResultSHA256, &result.ResultKeyID,
			&result.RowCount, &created, &expires)
	if err != nil {
		if isNoRows(err) {
			return OrdinalMaterialization{}, opErr(op, ErrNotFound, err)
		}
		return OrdinalMaterialization{}, opErr(op, ErrConflict, err)
	}
	result.CreatedAt = dbTime(created)
	result.ExpiresAt = scanNullableTime(expires)
	return result, nil
}

// DeleteUnusableOrdinalMaterialization removes only an exactly bound cache
// row whose expiry, ciphertext retention, or key-erasure state makes replay
// impossible. A lookup binding mismatch is deliberately left untouched so a
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
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return false, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `
DELETE FROM v4_committed_materializations cache
WHERE cache.cache_key_sha256=$1 AND cache.task_id=$2 AND cache.grant_digest=$3
  AND cache.catalog_digest=$4 AND cache.dictionary_set_digest=$5
  AND (
    (cache.expires_at IS NOT NULL AND cache.expires_at <= $6)
    OR NOT EXISTS (
      SELECT 1
      FROM encrypted_query_results encrypted
      JOIN result_encryption_keys key ON key.key_id=encrypted.key_id AND key.status='ACTIVE'
      WHERE encrypted.query_id=cache.source_query_id
        AND encrypted.plaintext_sha256=cache.result_sha256
    )
  )`, lookup.CacheKeySHA256, lookup.TaskID, lookup.GrantDigest, lookup.CatalogDigest,
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

func getOrdinalMaterializationTx(ctx context.Context, tx *sql.Tx, cacheKey string, requireUsable bool,
	now time.Time) (OrdinalMaterialization, error) {
	query := `
SELECT cache.cache_key_sha256,cache.task_id,cache.root_task_id,cache.source_query_id,
 cache.observation_sha256,cache.dictionary_set_digest,cache.grant_digest,cache.catalog_digest,
 cache.result_sha256,encrypted.key_id,cache.row_count,cache.created_at,cache.expires_at
FROM v4_committed_materializations cache
JOIN encrypted_query_results encrypted ON encrypted.query_id=cache.source_query_id
WHERE cache.cache_key_sha256=$1`
	if requireUsable {
		query += ` AND (cache.expires_at IS NULL OR cache.expires_at>$2)`
	}
	var row *sql.Row
	if requireUsable {
		row = tx.QueryRowContext(ctx, query, cacheKey, dbTime(now))
	} else {
		row = tx.QueryRowContext(ctx, query, cacheKey)
	}
	var result OrdinalMaterialization
	var created time.Time
	var expires sql.NullTime
	if err := row.Scan(&result.CacheKeySHA256, &result.TaskID, &result.RootTaskID, &result.SourceQueryID,
		&result.Observation.ObservationSHA256, &result.Observation.DictionarySetDigest,
		&result.GrantDigest, &result.CatalogDigest, &result.ResultSHA256, &result.ResultKeyID,
		&result.RowCount, &created, &expires); err != nil {
		return OrdinalMaterialization{}, err
	}
	result.CreatedAt = dbTime(created)
	result.ExpiresAt = scanNullableTime(expires)
	return result, nil
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
	result, err := tx.ExecContext(ctx, `DELETE FROM v4_committed_materializations
WHERE task_id=$1 AND cache_key_sha256=$2`, taskID, cacheKey)
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
		left.RootTaskID == right.RootTaskID &&
		left.Observation == right.Observation && left.GrantDigest == right.GrantDigest &&
		left.CatalogDigest == right.CatalogDigest && left.RowCount == right.RowCount
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
