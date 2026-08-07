package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// QueryExecutionBinding is the persisted description of what one query
// executed, together with the exposure pre-state its row limits derive from.
//
// The document is stored and returned whole. It is covered by the receipt
// signature as a document, so a projection that reassembled it field by field
// could differ from what was signed while every individual field still looked
// right.
type QueryExecutionBinding struct {
	QueryID   string
	BindingV2 *querybinding.QueryExecutionBindingV2
	// ExposureLedgerBefore is present exactly when the binding derives its limits
	// under an exposure profile.
	//
	// It is a pointer rather than a value because an operation on a task with no
	// exposure grant reads no ledger, and the alternative is to store a zero one.
	// A zero ledger is not "no ledger": it is a document saying limits and used
	// counts were read and were zero, which is a stronger and false claim, and it
	// would be signed into the receipt as though it had been.
	ExposureLedgerBefore *querybinding.ExposureLedgerBeforeV1
	CreatedAt            time.Time
}

// Version is the stored document's own version string.
func (binding QueryExecutionBinding) Version() string {
	if binding.BindingV2 == nil {
		return ""
	}
	return querybinding.QueryExecutionBindingV2Version
}

// PathKind and SHA256 read the stored document, so callers that need only the
// denormalized facts do not have to reach into it themselves.
func (binding QueryExecutionBinding) PathKind() querybinding.PathKind {
	if binding.BindingV2 == nil {
		return ""
	}
	return binding.BindingV2.PathKind
}

func (binding QueryExecutionBinding) SHA256() string {
	if binding.BindingV2 == nil {
		return ""
	}
	return binding.BindingV2.SHA256
}

// LedgerSHA256 is the stored pre-state's digest, or the empty string when the
// operation accounted no exposure and read none.
func (binding QueryExecutionBinding) LedgerSHA256() string {
	if binding.ExposureLedgerBefore == nil {
		return ""
	}
	return binding.ExposureLedgerBefore.SHA256
}

// PreparedOperation is the sealed preparation the stored row describes, and
// whether the row describes one at all.
//
// Returning false rather than a zero binding is what keeps an absent row
// distinguishable -- a zero binding would compare unequal to everything and read
// like a mismatch rather than like an absence.
//
// It returns the canonical preparedbinding type directly. Reaching it through
// physicalquery's alias would make persistence depend on the compiler and the
// authorizer for a value that is a version, some flags, some counts and some
// digests.
func (binding QueryExecutionBinding) PreparedOperation() (preparedbinding.PreparedOperationBindingV1, bool) {
	if binding.BindingV2 == nil {
		return preparedbinding.PreparedOperationBindingV1{}, false
	}
	return binding.BindingV2.PreparedOperation, true
}

// document is the stored structure, for encoding.
func (binding QueryExecutionBinding) document() any {
	return *binding.BindingV2
}

// Validate rejects a binding that could not have described a real execution.
func (binding QueryExecutionBinding) Validate() error {
	if binding.QueryID == "" {
		return fmt.Errorf("execution binding names no query")
	}
	if binding.BindingV2 == nil {
		return fmt.Errorf("execution binding for %s carries no execution binding document", binding.QueryID)
	}
	if err := binding.BindingV2.Validate(); err != nil {
		return fmt.Errorf("query execution binding v2: %w", err)
	}
	// The document says whether this operation accounted exposure, and the stored
	// pre-state must be there exactly when it did. Deciding from the row's own
	// nilness instead would let the two disagree: a binding derived under a
	// profile could be stored beside no pre-state, and the limits would be
	// reproducible against nothing.
	if (binding.ExposureLedgerBefore != nil) != (binding.BindingV2.ExposureProfileVersion != "") {
		if binding.ExposureLedgerBefore == nil {
			return fmt.Errorf("the execution binding for %s derives under exposure profile %q but the row "+
				"carries no pre-state", binding.QueryID, binding.BindingV2.ExposureProfileVersion)
		}
		return fmt.Errorf("the execution binding for %s accounts no exposure but the row carries a "+
			"ledger pre-state", binding.QueryID)
	}
	if binding.ExposureLedgerBefore == nil {
		return nil
	}
	if err := binding.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("exposure ledger pre-state: %w", err)
	}
	namedLedger := binding.BindingV2.ExposureLedgerBeforeSHA256
	// The binding must name the pre-state stored beside it. Without this the two
	// rows could describe different operations while each validated alone.
	if namedLedger != binding.LedgerSHA256() {
		return fmt.Errorf("the execution binding names exposure pre-state %s but the row carries %s",
			shortDigestValue(namedLedger), shortDigestValue(binding.LedgerSHA256()))
	}
	return nil
}

// nullString and nullBytes send an absent pre-state as SQL NULL rather than as
// an empty value.
//
// The distinction is the whole point of the column being nullable: '' and a
// zero-length BYTEA are values a row can carry, and the CHECK constraints that
// keep a present pre-state well-formed would have to be relaxed to admit them.
// NULL is refused by those constraints only when the other column is non-NULL,
// which is exactly the pairing the schema states.
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
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
	// A nil ledger renders as nil bytes, not as the encoding of a zero ledger.
	// The column is NULL for the same reason the member is a pointer: the row
	// must say that no pre-state was read, not that one was read and was empty.
	if binding.ExposureLedgerBefore == nil {
		return bindingJSON, nil, nil
	}
	ledgerJSON, err = canonicalDocument(*binding.ExposureLedgerBefore)
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
		nullBytes(ledgerJSON), nullString(binding.LedgerSHA256()),
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
		{"exposure_ledger_before_sha256", stored.LedgerSHA256(), binding.LedgerSHA256()},
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
// Returns ErrNotFound when the query recorded no execution binding. That is an
// ordinary outcome, not a failure: a query the receipt contract does not bind an
// execution for records none, and neither does one recovered as INDETERMINATE,
// which never completed an execution to describe.
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
		ledgerSHA      sql.NullString
		pathKind       string
		createdAt      time.Time
	)
	if err := row.Scan(&binding.QueryID, &bindingVersion, &bindingJSON, &bindingSHA, &ledgerJSON,
		&ledgerSHA, &pathKind, &createdAt); err != nil {
		return QueryExecutionBinding{}, nil, nil, err
	}
	// The two pre-state columns are NULL together or neither. The schema states
	// it as a constraint; stating it again here is what keeps a row written by
	// some other client from decoding into a binding with a digest and no
	// document, which would then compare equal to nothing and read as corruption
	// rather than as the violation it is.
	if ledgerSHA.Valid != (len(ledgerJSON) > 0) {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"the stored execution binding for %s carries only half of its exposure pre-state",
			binding.QueryID)
	}
	// The stored version is checked rather than assumed, and a row naming
	// anything else is refused outright rather than decoded on the chance that it
	// parses. This build writes and reads exactly one execution-binding version;
	// a row naming another is a database this build must not sign receipts over.
	if bindingVersion != querybinding.QueryExecutionBindingV2Version {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored execution binding names version %q, which this build cannot read", bindingVersion)
	}
	var decoded querybinding.QueryExecutionBindingV2
	if err := strictUnmarshal(bindingJSON, &decoded); err != nil {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf("decode query execution binding v2: %w", err)
	}
	binding.BindingV2 = &decoded
	if len(ledgerJSON) > 0 {
		var ledger querybinding.ExposureLedgerBeforeV1
		if err := strictUnmarshal(ledgerJSON, &ledger); err != nil {
			return QueryExecutionBinding{}, nil, nil, fmt.Errorf("decode exposure ledger pre-state: %w", err)
		}
		binding.ExposureLedgerBefore = &ledger
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
	if binding.LedgerSHA256() != ledgerSHA.String {
		return QueryExecutionBinding{}, nil, nil, fmt.Errorf(
			"stored exposure pre-state digests to %s but its row records %s",
			shortDigestValue(binding.LedgerSHA256()), shortDigestValue(ledgerSHA.String))
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
