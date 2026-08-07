package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// QueryExecutionBinding is the persisted description of what one query
// executed, together with the exposure pre-state its row limits derive from.
//
// Exactly one of Binding and BindingV2 is set. They are separate members rather
// than one polymorphic field for the same reason the receipt keeps them apart: a
// single field would have to be discriminated after decoding, and a row that
// decoded as neither -- or as both -- would be a state the type permitted.
//
// Both documents are stored and returned whole. They are covered by the receipt
// signature as documents, so a projection that reassembled them field by field
// could differ from what was signed while every individual field still looked
// right.
type QueryExecutionBinding struct {
	QueryID              string
	Binding              *querybinding.QueryExecutionBindingV1
	BindingV2            *querybinding.QueryExecutionBindingV2
	ExposureLedgerBefore querybinding.ExposureLedgerBeforeV1
	CreatedAt            time.Time
}

// Version is the stored document's own version string.
func (binding QueryExecutionBinding) Version() string {
	switch {
	case binding.Binding != nil:
		return querybinding.QueryExecutionBindingV1Version
	case binding.BindingV2 != nil:
		return querybinding.QueryExecutionBindingV2Version
	default:
		return ""
	}
}

// PathKind and SHA256 read whichever document is present, so callers that need
// only the denormalized facts do not have to branch on the version themselves.
func (binding QueryExecutionBinding) PathKind() querybinding.PathKind {
	switch {
	case binding.Binding != nil:
		return binding.Binding.PathKind
	case binding.BindingV2 != nil:
		return binding.BindingV2.PathKind
	default:
		return ""
	}
}

func (binding QueryExecutionBinding) SHA256() string {
	switch {
	case binding.Binding != nil:
		return binding.Binding.SHA256
	case binding.BindingV2 != nil:
		return binding.BindingV2.SHA256
	default:
		return ""
	}
}

// document is whichever of the two is present, for encoding.
func (binding QueryExecutionBinding) document() any {
	if binding.Binding != nil {
		return *binding.Binding
	}
	return *binding.BindingV2
}

// Validate rejects a binding that could not have described a real execution.
func (binding QueryExecutionBinding) Validate() error {
	if binding.QueryID == "" {
		return fmt.Errorf("execution binding names no query")
	}
	if (binding.Binding == nil) == (binding.BindingV2 == nil) {
		return fmt.Errorf("execution binding for %s carries %s; exactly one document is required",
			binding.QueryID, bothOrNeither(binding))
	}
	if err := binding.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("exposure ledger pre-state: %w", err)
	}
	var namedLedger string
	if binding.Binding != nil {
		if err := binding.Binding.Validate(); err != nil {
			return fmt.Errorf("query execution binding: %w", err)
		}
		namedLedger = binding.Binding.ExposureLedgerBeforeSHA256
	} else {
		if err := binding.BindingV2.Validate(); err != nil {
			return fmt.Errorf("query execution binding v2: %w", err)
		}
		namedLedger = binding.BindingV2.ExposureLedgerBeforeSHA256
	}
	// The binding must name the pre-state stored beside it. Without this the two
	// rows could describe different operations while each validated alone.
	if namedLedger != binding.ExposureLedgerBefore.SHA256 {
		return fmt.Errorf("the execution binding names exposure pre-state %s but the row carries %s",
			shortDigestValue(namedLedger), shortDigestValue(binding.ExposureLedgerBefore.SHA256))
	}
	return nil
}

func bothOrNeither(binding QueryExecutionBinding) string {
	if binding.Binding != nil {
		return "both a V1 and a V2 document"
	}
	return "no execution binding document"
}

func shortDigestValue(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// canonicalDocuments renders the two structures as the exact bytes that are
// stored and reloaded.
//
// Canonical JSON rather than encoding/json's output: the replay artifact must be
// the bytes, not merely a valid encoding of the values. Two encoders that order
// members differently, or that spell the same number differently, produce
// documents that are semantically equal and byte-different -- and a byte
// difference is what an idempotent replay and a recovery re-sign are compared
// on. Pinning the encoding here is what makes "the same binding" a decidable
// question.
func (binding QueryExecutionBinding) canonicalDocuments() (bindingJSON, ledgerJSON []byte, err error) {
	bindingJSON, err = canonicalDocument(binding.document())
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize query execution binding: %w", err)
	}
	ledgerJSON, err = canonicalDocument(binding.ExposureLedgerBefore)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize exposure ledger pre-state: %w", err)
	}
	return bindingJSON, ledgerJSON, nil
}

// canonicalDocument round-trips a value through encoding/json so that struct
// tags, omitempty and pointer nilness are applied, then canonicalizes the
// resulting I-JSON. approval.CanonicalJSON alone would reflect over the Go
// struct and miss the tag layer.
func canonicalDocument(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	return approval.CanonicalJSON(document)
}

const executionBindingSelect = `
SELECT query_id, binding_version, binding_json, binding_sha256, exposure_ledger_before_json,
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
	bindingJSON, ledgerJSON, err := binding.canonicalDocuments()
	if err != nil {
		return err
	}
	// ON CONFLICT DO NOTHING, not DO UPDATE. The table is immutable by trigger,
	// so an update would fail anyway; doing nothing makes a redundant write by a
	// retried settlement harmless while still refusing to change what was
	// recorded.
	result, err := tx.ExecContext(ctx, `
INSERT INTO query_execution_bindings
    (query_id, binding_version, binding_json, binding_sha256, exposure_ledger_before_json,
     exposure_ledger_before_sha256, path_kind, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (query_id) DO NOTHING`,
		binding.QueryID, binding.Version(), bindingJSON, binding.SHA256(),
		ledgerJSON, binding.ExposureLedgerBefore.SHA256,
		string(binding.PathKind()), dbTime(now))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	// Zero rows means a binding already exists for this query. "DO NOTHING"
	// swallowed the write, and until this check existed it also swallowed the
	// difference: a second settlement that described a DIFFERENT execution
	// returned success and left the first row in place, so the receipt signed
	// afterwards described an execution nobody had verified matched. Idempotency
	// has to mean "the same binding", not "some binding".
	return requireIdenticalStoredBindingTx(ctx, tx, binding, bindingJSON, ledgerJSON)
}

// requireIdenticalStoredBindingTx succeeds only when the stored row is exactly
// the binding being written.
//
// Exactly means: the stored row still validates on its own, both canonical
// documents are byte-identical, and every redundant column agrees. Comparing
// only the outer digest would accept a document whose members were changed and
// whose recorded digest was copied from the original -- the outer digest is a
// column, and a column is not a signature.
func requireIdenticalStoredBindingTx(ctx context.Context, tx *sql.Tx, binding QueryExecutionBinding,
	bindingJSON, ledgerJSON []byte) error {
	stored, storedBindingJSON, storedLedgerJSON, err := scanStoredExecutionBinding(
		tx.QueryRowContext(ctx, executionBindingSelect+` WHERE query_id=$1`, binding.QueryID))
	if err != nil {
		if isNoRows(err) {
			// The insert affected no row and no row is there. Nothing can make this
			// coherent, so it is a conflict rather than a retry.
			return fmt.Errorf("%w: the execution binding for %s was neither inserted nor found",
				ErrConflict, binding.QueryID)
		}
		return fmt.Errorf("%w: reload the stored execution binding: %v", ErrConflict, err)
	}
	if err := stored.Validate(); err != nil {
		return fmt.Errorf("%w: the stored execution binding for %s no longer validates: %v",
			ErrConflict, binding.QueryID, err)
	}
	// The version is compared before the documents. A stored V1 and an incoming
	// V2 for one query is not a retry that happens to differ in some bytes; it is
	// two descriptions of one execution under two contracts, and reporting it as a
	// document mismatch would bury the only fact that explains it.
	if stored.Version() != binding.Version() {
		return fmt.Errorf("%w: query %s already carries binding_version %q, not %q",
			ErrConflict, binding.QueryID, stored.Version(), binding.Version())
	}
	for _, document := range []struct {
		name           string
		stored, wanted []byte
	}{
		{"query execution binding", storedBindingJSON, bindingJSON},
		{"exposure ledger pre-state", storedLedgerJSON, ledgerJSON},
	} {
		if !bytes.Equal(document.stored, document.wanted) {
			return fmt.Errorf("%w: query %s already carries a different %s document",
				ErrConflict, binding.QueryID, document.name)
		}
	}
	for _, field := range []struct {
		name           string
		stored, wanted string
	}{
		{"binding_sha256", stored.SHA256(), binding.SHA256()},
		{"exposure_ledger_before_sha256", stored.ExposureLedgerBefore.SHA256, binding.ExposureLedgerBefore.SHA256},
		{"path_kind", string(stored.PathKind()), string(binding.PathKind())},
	} {
		if field.stored != field.wanted {
			return fmt.Errorf("%w: query %s already carries %s %q, not %q",
				ErrConflict, binding.QueryID, field.name, field.stored, field.wanted)
		}
	}
	return nil
}

// getQueryExecutionBindingTx reloads the binding inside the caller's
// transaction, so a settlement can read back what it has just written and a
// receipt can be signed over the persisted row rather than over a value held in
// memory.
func getQueryExecutionBindingTx(ctx context.Context, tx *sql.Tx, queryID string) (QueryExecutionBinding, error) {
	binding, err := scanQueryExecutionBinding(
		tx.QueryRowContext(ctx, executionBindingSelect+` WHERE query_id=$1`, queryID))
	if err != nil {
		return QueryExecutionBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return QueryExecutionBinding{}, err
	}
	return binding, nil
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
	binding, _, _, err := scanStoredExecutionBinding(row)
	return binding, err
}

// scanStoredExecutionBinding decodes a row and returns the stored bytes as well
// as the decoded structures.
//
// The bytes are returned because they, not the structures, are the replay
// artifact. A caller comparing two bindings must be able to compare what was
// stored rather than what a decoder made of it.
func scanStoredExecutionBinding(row rowScanner) (QueryExecutionBinding, []byte, []byte, error) {
	var (
		binding        QueryExecutionBinding
		bindingVersion string
		bindingJSON    []byte
		bindingSHA     string
		ledgerJSON     []byte
		ledgerSHA      string
		pathKind       string
		createdAt      time.Time
	)
	if err := row.Scan(&binding.QueryID, &bindingVersion, &bindingJSON, &bindingSHA, &ledgerJSON,
		&ledgerSHA, &pathKind, &createdAt); err != nil {
		return QueryExecutionBinding{}, nil, nil, err
	}
	// Strict dispatch. The row's version selects exactly one decoder and the
	// decoder refuses anything the document does not match, so a V1 row can only
	// ever be read as a V1 and a V2 row only as a V2. Trying both and taking
	// whichever parsed would make the stored version advisory, and a V2 document
	// missing its prepared operation would decode as a V1 with empty members
	// rather than as the corruption it is.
	switch bindingVersion {
	case querybinding.QueryExecutionBindingV1Version:
		var decoded querybinding.QueryExecutionBindingV1
		if err := strictUnmarshal(bindingJSON, &decoded); err != nil {
			return QueryExecutionBinding{}, nil, nil, fmt.Errorf("decode query execution binding: %w", err)
		}
		binding.Binding = &decoded
	case querybinding.QueryExecutionBindingV2Version:
		var decoded querybinding.QueryExecutionBindingV2
		if err := strictUnmarshal(bindingJSON, &decoded); err != nil {
			return QueryExecutionBinding{}, nil, nil, fmt.Errorf("decode query execution binding v2: %w", err)
		}
		binding.BindingV2 = &decoded
	default:
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored execution binding names version %q, which this build cannot read", bindingVersion)
	}
	if err := strictUnmarshal(ledgerJSON, &binding.ExposureLedgerBefore); err != nil {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf("decode exposure ledger pre-state: %w", err)
	}
	binding.CreatedAt = createdAt.UTC()
	// The denormalized columns are redundant with the documents on purpose. A
	// disagreement means a value changed outside the documents the signature
	// covers, which is precisely the tampering the redundancy exists to catch.
	// The version is included: the column selected the decoder above, so a
	// document whose own version member disagrees with it was read by a decoder
	// its author did not intend.
	if binding.Version() != bindingVersion {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored execution binding document is %s but its row records version %q",
			binding.Version(), bindingVersion)
	}
	if binding.SHA256() != bindingSHA {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored execution binding digests to %s but its row records %s",
			shortDigestValue(binding.SHA256()), shortDigestValue(bindingSHA))
	}
	if binding.ExposureLedgerBefore.SHA256 != ledgerSHA {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored exposure pre-state digests to %s but its row records %s",
			shortDigestValue(binding.ExposureLedgerBefore.SHA256), shortDigestValue(ledgerSHA))
	}
	if string(binding.PathKind()) != pathKind {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored execution binding is %s but its row records path_kind %q",
			binding.PathKind(), pathKind)
	}
	// Re-canonicalize what was decoded and require it to equal what was stored.
	//
	// Without this the stored bytes are only required to be *a* valid encoding of
	// the structures. Two encodings that differ in member order or number spelling
	// decode to the same values and hash to the same SHA256 -- the digest is
	// computed over the members, not over the bytes -- so both would pass every
	// other check here while producing two different replay artifacts for one
	// execution. An idempotent replay must return the same bytes it returned
	// before, and a recovery must re-sign the same document; neither is decidable
	// if the stored encoding is free.
	reBinding, reLedger, err := binding.canonicalDocuments()
	if err != nil {
		return QueryExecutionBinding{}, nil, nil, err
	}
	for _, document := range []struct {
		name           string
		stored, wanted []byte
	}{
		{"query execution binding", bindingJSON, reBinding},
		{"exposure ledger pre-state", ledgerJSON, reLedger},
	} {
		if !bytes.Equal(document.stored, document.wanted) {
			return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
				"the stored %s is not its canonical encoding; it would replay as different bytes than it was signed as",
				document.name)
		}
	}
	return binding, bindingJSON, ledgerJSON, nil
}

// strictUnmarshal refuses a document carrying a member the target does not have.
//
// The stored bytes are re-canonicalized and compared below, which already
// catches an unknown member. Refusing it here as well is what makes the failure
// say what is wrong: an unknown member reported as "not its canonical encoding"
// reads like an encoder problem rather than like a document from a version this
// build does not understand.
func strictUnmarshal(document []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// writeSettlementExecutionBindingTx persists the settlement's execution binding,
// if it carries one.
//
// Only a COMPLETED query may create one. The other terminal statuses are
// deliberately excluded rather than merely unreached: a released query never
// invoked the Connector, an indeterminate one cannot prove what completed, and a
// failed one cannot prove which of its targets ran. A binding written for any of
// them would assert a target sequence nothing observed.
func writeSettlementExecutionBindingTx(ctx context.Context, tx *sql.Tx, now time.Time,
	settlement BudgetSettlement, status QueryStatus, queryID string) error {
	if settlement.ExecutionBinding == nil {
		return nil
	}
	if status != QueryCompleted {
		return fmt.Errorf("%w: a %s query cannot carry an execution binding", ErrInvalid, status)
	}
	binding := *settlement.ExecutionBinding
	if binding.QueryID != queryID {
		return fmt.Errorf("%w: the execution binding names query %s but the settlement is for %s",
			ErrInvalid, binding.QueryID, queryID)
	}
	return putQueryExecutionBindingTx(ctx, tx, now, binding)
}
