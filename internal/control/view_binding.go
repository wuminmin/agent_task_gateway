package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/viewbinding"
)

func validateGrantViewBinding(grant TaskGrant) error {
	if grant.ViewBindingDigest == "" {
		if grant.ViewBindingSet != nil {
			return fmt.Errorf("view binding evidence requires a digest")
		}
		return nil
	}
	if !validSHA256Hex(grant.ViewBindingDigest) {
		return fmt.Errorf("view binding digest must be lowercase SHA-256")
	}
	if grant.ViewBindingSet == nil {
		return fmt.Errorf("view binding set is required for a bound grant")
	}
	binding := grant.ViewBindingSet
	if binding.Digest != grant.ViewBindingDigest || !validSHA256Hex(binding.Digest) ||
		binding.ProfileVersion != viewbinding.Version || len(binding.CanonicalJSON) == 0 {
		return fmt.Errorf("view binding set is invalid or disagrees with the grant")
	}
	if !binding.CreatedAt.IsZero() && !grant.CreatedAt.IsZero() &&
		!dbTime(binding.CreatedAt).Equal(dbTime(grant.CreatedAt)) {
		return fmt.Errorf("view binding evidence time disagrees with the grant")
	}

	decoder := json.NewDecoder(bytes.NewReader(binding.CanonicalJSON))
	decoder.DisallowUnknownFields()
	var decoded viewbinding.Set
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode canonical view binding set: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode canonical view binding set: %w", err)
	}
	if decoded.Version != viewbinding.Version || decoded.Version != binding.ProfileVersion {
		return fmt.Errorf("view binding profile version is unsupported or inconsistent")
	}
	canonicalSet, err := viewbinding.New(decoded.Products)
	if err != nil {
		return fmt.Errorf("validate canonical view binding set: %w", err)
	}
	canonicalJSON, err := json.Marshal(canonicalSet)
	if err != nil {
		return fmt.Errorf("encode canonical view binding set: %w", err)
	}
	if !bytes.Equal(binding.CanonicalJSON, canonicalJSON) {
		return fmt.Errorf("view binding set is not in canonical encoding")
	}
	computedDigest, err := canonicalSet.Digest()
	if err != nil {
		return fmt.Errorf("digest canonical view binding set: %w", err)
	}
	if computedDigest != binding.Digest {
		return fmt.Errorf("canonical view binding set does not match its digest")
	}

	approved := make(map[string]struct{}, len(grant.ApprovedProducts))
	for _, product := range grant.ApprovedProducts {
		if strings.TrimSpace(product) == "" || strings.TrimSpace(product) != product || strings.ContainsRune(product, '\x00') {
			return fmt.Errorf("approved product must be a non-empty canonical string")
		}
		if _, duplicate := approved[product]; duplicate {
			return fmt.Errorf("duplicate approved product %q", product)
		}
		approved[product] = struct{}{}
	}
	boundProducts := make(map[string]struct{}, len(canonicalSet.Products))
	for _, contract := range canonicalSet.Products {
		boundProducts[contract.Product] = struct{}{}
	}
	// OA narrowing and child delegation may remove products, but cannot alter
	// the signed binding digest. Therefore every finally approved product must
	// be covered by the original binding set; extra original products are a
	// conservative dependency, not an authorization expansion.
	for product := range approved {
		if _, ok := boundProducts[product]; !ok {
			return fmt.Errorf("approved product %q is absent from the view binding", product)
		}
	}

	if len(binding.Dependencies) == 0 {
		return fmt.Errorf("at least one canonical view dependency is required")
	}
	var previous string
	covered := make(map[string]bool, len(boundProducts))
	for _, dependency := range binding.Dependencies {
		if dependency.Product == "" || dependency.Product != strings.TrimSpace(dependency.Product) ||
			dependency.DependencyKey == "" || dependency.DependencyKey != strings.TrimSpace(dependency.DependencyKey) ||
			strings.ContainsRune(dependency.Product, '\x00') || strings.ContainsRune(dependency.DependencyKey, '\x00') {
			return fmt.Errorf("view dependency product and key must be non-empty canonical strings")
		}
		if _, ok := boundProducts[dependency.Product]; !ok {
			return fmt.Errorf("view dependency product %q is not bound", dependency.Product)
		}
		key := dependency.Product + "\x00" + dependency.DependencyKey
		if previous != "" && key <= previous {
			return fmt.Errorf("view dependencies must be strictly sorted and unique")
		}
		covered[dependency.Product] = true
		previous = key
	}
	for product := range boundProducts {
		if !covered[product] {
			return fmt.Errorf("view dependency evidence is incomplete for product %q", product)
		}
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func insertTaskViewBindingTx(ctx context.Context, tx *sql.Tx, taskID string, binding ViewBindingSet, createdAt time.Time) error {
	result, err := tx.ExecContext(ctx, `
INSERT INTO view_binding_sets(digest, profile_version, canonical_json, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (digest) DO NOTHING`, binding.Digest, binding.ProfileVersion, []byte(binding.CanonicalJSON), createdAt)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		var profile string
		var stored []byte
		if err := tx.QueryRowContext(ctx, `
SELECT profile_version, canonical_json FROM view_binding_sets WHERE digest=$1`, binding.Digest).
			Scan(&profile, &stored); err != nil {
			return err
		}
		if profile != binding.ProfileVersion || !bytes.Equal(stored, binding.CanonicalJSON) {
			return fmt.Errorf("view binding digest collision for %s", binding.Digest)
		}
	}
	for _, dependency := range binding.Dependencies {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO task_view_dependencies(task_id, binding_digest, product, dependency_key)
VALUES ($1, $2, $3, $4)`, taskID, binding.Digest, dependency.Product, dependency.DependencyKey); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO task_view_binding_status(task_id, status, bound_digest)
VALUES ($1, 'ACTIVE', $2)`, taskID, binding.Digest)
	return err
}

// GetTaskViewBindingStatus returns the durable semantic-binding state for a
// Phase-B task. Legacy tasks have no row and return ErrNotFound.
func (s *Store) GetTaskViewBindingStatus(ctx context.Context, taskID string) (TaskViewBindingStatus, error) {
	const op = "get task view binding status"
	if err := s.checkOpen(op); err != nil {
		return TaskViewBindingStatus{}, err
	}
	if strings.TrimSpace(taskID) == "" {
		return TaskViewBindingStatus{}, opErr(op, ErrInvalid, fmt.Errorf("task id is required"))
	}
	status, err := scanTaskViewBindingStatus(s.db.QueryRowContext(ctx, `
SELECT task_id, status, bound_digest, observed_digest, detected_at
FROM task_view_binding_status WHERE task_id=$1`, taskID))
	if err != nil {
		if isNoRows(err) {
			return TaskViewBindingStatus{}, opErr(op, ErrNotFound, err)
		}
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	return status, nil
}

// MarkTaskViewSemanticChanged makes the first observed semantic mismatch
// durable together with its hash-chain audit evidence. Repeated detections are
// idempotent and retain the first mismatch that closed the task to new work.
func (s *Store) MarkTaskViewSemanticChanged(ctx context.Context, taskID, observedDigest string) (TaskViewBindingStatus, error) {
	const op = "mark task view semantic changed"
	if err := s.checkOpen(op); err != nil {
		return TaskViewBindingStatus{}, err
	}
	if strings.TrimSpace(taskID) == "" || !validSHA256Hex(observedDigest) {
		return TaskViewBindingStatus{}, opErr(op, ErrInvalid, fmt.Errorf("task id and observed view binding digest are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	var lockedTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&lockedTaskID); err != nil {
		if isNoRows(err) {
			return TaskViewBindingStatus{}, opErr(op, ErrNotFound, err)
		}
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	status, err := scanTaskViewBindingStatus(tx.QueryRowContext(ctx, `
SELECT task_id, status, bound_digest, observed_digest, detected_at
FROM task_view_binding_status WHERE task_id=$1 FOR UPDATE`, taskID))
	if err != nil {
		if isNoRows(err) {
			return TaskViewBindingStatus{}, opErr(op, ErrNotFound, err)
		}
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	if status.Status == TaskViewBindingRequireRebind || observedDigest == status.BoundDigest {
		if err := tx.Commit(); err != nil {
			return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
		}
		return status, nil
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `
UPDATE task_view_binding_status
SET status='REQUIRE_REBIND', observed_digest=$1, detected_at=$2
WHERE task_id=$3 AND status='ACTIVE'`, observedDigest, dbTime(now), taskID)
	if err != nil {
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, fmt.Errorf("view binding status changed concurrently"))
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: taskID, Actor: "system", EventType: "TASK_VIEW_SEMANTIC_CHANGED", OccurredAt: now,
		Payload: mustJSON(map[string]any{
			"bound_digest": status.BoundDigest, "observed_digest": observedDigest,
			"status": TaskViewBindingRequireRebind,
		}),
	})
	if err != nil {
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return TaskViewBindingStatus{}, opErr(op, ErrConflict, err)
	}
	status.Status = TaskViewBindingRequireRebind
	status.ObservedDigest = observedDigest
	status.DetectedAt = &now
	return status, nil
}

func scanTaskViewBindingStatus(row rowScanner) (TaskViewBindingStatus, error) {
	var status TaskViewBindingStatus
	var detected sql.NullTime
	if err := row.Scan(&status.TaskID, &status.Status, &status.BoundDigest, &status.ObservedDigest, &detected); err != nil {
		return TaskViewBindingStatus{}, err
	}
	status.DetectedAt = scanNullableTime(detected)
	return status, nil
}
