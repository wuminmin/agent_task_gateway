package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// QueryExecutionBinding is the persisted description of what one query
// executed, together with the exposure pre-state its row limits derive from.
//
// Both are stored and returned as whole canonical documents. They are covered
// by the receipt signature as documents, so a projection that reassembled them
// field by field could differ from what was signed while every individual field
// still looked right.
type QueryExecutionBinding struct {
	QueryID              string
	Binding              querybinding.QueryExecutionBindingV1
	ExposureLedgerBefore querybinding.ExposureLedgerBeforeV1
	CreatedAt            time.Time
}

// Validate rejects a binding that could not have described a real execution.
func (binding QueryExecutionBinding) Validate() error {
	if binding.QueryID == "" {
		return fmt.Errorf("execution binding names no query")
	}
	if err := binding.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("exposure ledger pre-state: %w", err)
	}
	if err := binding.Binding.Validate(); err != nil {
		return fmt.Errorf("query execution binding: %w", err)
	}
	// The binding must name the pre-state stored beside it. Without this the two
	// rows could describe different operations while each validated alone.
	if binding.Binding.ExposureLedgerBeforeSHA256 != binding.ExposureLedgerBefore.SHA256 {
		return fmt.Errorf("the execution binding names exposure pre-state %s but the row carries %s",
			shortDigestValue(binding.Binding.ExposureLedgerBeforeSHA256),
			shortDigestValue(binding.ExposureLedgerBefore.SHA256))
	}
	return nil
}

func shortDigestValue(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

const executionBindingSelect = `
SELECT query_id, binding_json, binding_sha256, exposure_ledger_before_json,
       exposure_ledger_before_sha256, path_kind, created_at
FROM query_execution_bindings`

// putQueryExecutionBindingTx writes the binding inside the caller's transaction.
//
// It is a transaction-scoped helper rather than a Store method because it is
// only ever correct to write this atomically with the terminal query evidence:
// a settled query must have both rows or neither, or a reload would produce a
// receipt describing an execution the query records do not agree happened.
func putQueryExecutionBindingTx(ctx context.Context, tx *sql.Tx, now time.Time,
	binding QueryExecutionBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	bindingJSON, err := json.Marshal(binding.Binding)
	if err != nil {
		return fmt.Errorf("canonicalize query execution binding: %w", err)
	}
	ledgerJSON, err := json.Marshal(binding.ExposureLedgerBefore)
	if err != nil {
		return fmt.Errorf("canonicalize exposure ledger pre-state: %w", err)
	}
	// ON CONFLICT DO NOTHING, not DO UPDATE. The table is immutable by trigger,
	// so an update would fail anyway; doing nothing makes a redundant write by a
	// retried settlement harmless while still refusing to change what was
	// recorded.
	_, err = tx.ExecContext(ctx, `
INSERT INTO query_execution_bindings
    (query_id, binding_json, binding_sha256, exposure_ledger_before_json,
     exposure_ledger_before_sha256, path_kind, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (query_id) DO NOTHING`,
		binding.QueryID, bindingJSON, binding.Binding.SHA256,
		ledgerJSON, binding.ExposureLedgerBefore.SHA256,
		string(binding.Binding.PathKind), dbTime(now))
	return err
}

// GetQueryExecutionBinding reloads the binding recorded for one query.
//
// Returns ErrNotFound when the query executed under a pre-V9 path. That is an
// ordinary outcome, not a failure: every receipt version before 9 describes no
// execution binding, and so does a query recovered as INDETERMINATE, which never
// completed an execution to describe.
func (s *Store) GetQueryExecutionBinding(ctx context.Context, queryID string) (QueryExecutionBinding, error) {
	const op = "get query execution binding"
	if err := s.checkOpen(op); err != nil {
		return QueryExecutionBinding{}, err
	}
	binding, err := scanQueryExecutionBinding(
		s.db.QueryRowContext(ctx, executionBindingSelect+` WHERE query_id=$1`, queryID))
	if err != nil {
		if isNoRows(err) {
			return QueryExecutionBinding{}, opErr(op, ErrNotFound, err)
		}
		return QueryExecutionBinding{}, opErr(op, ErrConflict, err)
	}
	// Validated on the way out as well as on the way in. A row that decoded but
	// no longer describes a coherent execution must not reach a signer, and the
	// database is exactly where a value can change between the two.
	if err := binding.Validate(); err != nil {
		return QueryExecutionBinding{}, opErr(op, ErrConflict, err)
	}
	return binding, nil
}

func scanQueryExecutionBinding(row rowScanner) (QueryExecutionBinding, error) {
	var (
		binding     QueryExecutionBinding
		bindingJSON []byte
		bindingSHA  string
		ledgerJSON  []byte
		ledgerSHA   string
		pathKind    string
		createdAt   time.Time
	)
	if err := row.Scan(&binding.QueryID, &bindingJSON, &bindingSHA, &ledgerJSON,
		&ledgerSHA, &pathKind, &createdAt); err != nil {
		return QueryExecutionBinding{}, err
	}
	if err := json.Unmarshal(bindingJSON, &binding.Binding); err != nil {
		return QueryExecutionBinding{}, fmt.Errorf("decode query execution binding: %w", err)
	}
	if err := json.Unmarshal(ledgerJSON, &binding.ExposureLedgerBefore); err != nil {
		return QueryExecutionBinding{}, fmt.Errorf("decode exposure ledger pre-state: %w", err)
	}
	binding.CreatedAt = createdAt.UTC()
	// The denormalized columns are redundant with the documents on purpose. A
	// disagreement means a value changed outside the documents the signature
	// covers, which is precisely the tampering the redundancy exists to catch.
	if binding.Binding.SHA256 != bindingSHA {
		return QueryExecutionBinding{}, fmt.Errorf(
			"stored execution binding digests to %s but its row records %s",
			shortDigestValue(binding.Binding.SHA256), shortDigestValue(bindingSHA))
	}
	if binding.ExposureLedgerBefore.SHA256 != ledgerSHA {
		return QueryExecutionBinding{}, fmt.Errorf(
			"stored exposure pre-state digests to %s but its row records %s",
			shortDigestValue(binding.ExposureLedgerBefore.SHA256), shortDigestValue(ledgerSHA))
	}
	if string(binding.Binding.PathKind) != pathKind {
		return QueryExecutionBinding{}, fmt.Errorf(
			"stored execution binding is %s but its row records path_kind %q",
			binding.Binding.PathKind, pathKind)
	}
	return binding, nil
}
