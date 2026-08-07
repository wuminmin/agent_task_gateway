// Package querybinding defines the signed, SQL-free description of what one
// governed operation actually executed.
//
// # Why this exists
//
// A receipt that binds the authorization, the budget and the exposure
// accounting still says nothing about which physical statements ran. The
// evaluation used to fill that gap by re-deriving the statements after the fact
// and treating the result as though the Gateway had signed it. That is not
// evidence about the execution; it is a second opinion about it, and the two can
// differ in exactly the case the evidence exists to detect -- a constant-only
// mutation that pg_stat_statements has normalized away.
//
// QueryExecutionBindingV2 is constructed by the Gateway from the decisions it
// actually passes to the Connector, and is covered by the receipt signature.
//
// # Why there is no SQL here
//
// Every statement appears as a digest. The receipt is retained, replayed and
// handed to a finalizer that must not learn what was queried; carrying the text
// would put the governed SQL into every one of those places. The exact digest
// pins the executable bytes including constants, and the strict AST digest is
// the structural identity the observer classifies on, so both the "were these
// the same bytes" and the "was this the same statement shape" questions are
// answerable without the text.
//
// # Why the package is neutral
//
// It is imported by the Gateway, by the receipt, and by the evaluation. Placing
// it in any of those would make the other two depend on a package that carries
// unrelated concerns, and production must not depend on evaluation code.
package querybinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The ledger pre-state is versioned separately from the execution binding
// because it is meaningful on its own: it is what the finalizer needs to
// reproduce the row limits, and it is signed even when no statement executed.
const ExposureLedgerBeforeV1Version = "taskgate-exposure-ledger-before-v1"

const (
	exposureLedgerDomain = "TASKGATE-EXPOSURE-LEDGER-BEFORE-V1"
	targetDomain         = "TASKGATE-QUERY-EXECUTION-TARGET-V1"
)

// PathKind is the execution path the Gateway took.
//
// It is signed rather than inferred. Inferring it from the target count is what
// the Adapter used to do, and it cannot distinguish a semantic replay that
// authorized two targets without executing them from a paired-novel execution
// that ran both.
type PathKind string

const (
	PathPairedNovel      PathKind = "paired_novel"
	PathSingleQuery      PathKind = "single_query"
	PathSemanticReplay   PathKind = "semantic_replay"
	PathIdempotentReplay PathKind = "idempotent_replay"
)

// PathKinds returns the closed set.
func PathKinds() []PathKind {
	return []PathKind{PathPairedNovel, PathSingleQuery, PathSemanticReplay, PathIdempotentReplay}
}

func (kind PathKind) valid() bool {
	for _, known := range PathKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// TargetRole names which statement of the pair a record describes.
type TargetRole string

const (
	RoleVisible   TargetRole = "visible"
	RoleCompanion TargetRole = "companion"
)

// FactVector is the exposure accounting in one profile's fact dimensions.
//
// All five dimensions are carried even when a profile does not use them, so the
// canonical digest has a fixed shape and a profile upgrade cannot silently
// change what a digest covered.
type FactVector struct {
	ReleaseFacts      int64 `json:"release_facts"`
	InfluenceFacts    int64 `json:"influence_facts"`
	OutcomeFacts      int64 `json:"outcome_facts"`
	PredicateAtoms    int64 `json:"predicate_atoms"`
	CompositeOutcomes int64 `json:"composite_outcomes"`
}

func (vector FactVector) validate(name string) error {
	for label, value := range map[string]int64{
		"release_facts": vector.ReleaseFacts, "influence_facts": vector.InfluenceFacts,
		"outcome_facts": vector.OutcomeFacts, "predicate_atoms": vector.PredicateAtoms,
		"composite_outcomes": vector.CompositeOutcomes,
	} {
		if value < 0 {
			return fmt.Errorf("%s.%s is %d", name, label, value)
		}
	}
	return nil
}

func (vector FactVector) minus(other FactVector) FactVector {
	return FactVector{
		ReleaseFacts:      vector.ReleaseFacts - other.ReleaseFacts,
		InfluenceFacts:    vector.InfluenceFacts - other.InfluenceFacts,
		OutcomeFacts:      vector.OutcomeFacts - other.OutcomeFacts,
		PredicateAtoms:    vector.PredicateAtoms - other.PredicateAtoms,
		CompositeOutcomes: vector.CompositeOutcomes - other.CompositeOutcomes,
	}
}

// ExposureLedgerBeforeV1 is the exposure state an operation began from.
//
// It carries enough to reproduce the row-limit derivation and nothing that
// identifies what was exposed: no FactID, no bitmap member, no task payload and
// no SQL. Those would turn a budget record into a disclosure of the very facts
// the ledger exists to bound.
type ExposureLedgerBeforeV1 struct {
	Version string `json:"version"`
	// ProfileVersion is the exposure profile the limits belong to. The same
	// numbers mean different things under different profiles.
	ProfileVersion string `json:"profile_version"`
	// RootTaskID and RootEpoch identify the ledger this state was read from. An
	// epoch change resets the accounting, so a state without one could be
	// replayed against a ledger it never described.
	RootTaskID string `json:"root_task_id"`
	RootEpoch  int64  `json:"root_epoch"`
	// Limits, Used and Remaining are carried together and required to agree.
	// Remaining alone would be unverifiable; Limits and Used alone would make
	// every reader recompute it and risk disagreeing about how.
	Limits    FactVector `json:"limits"`
	Used      FactVector `json:"used"`
	Remaining FactVector `json:"remaining"`
	// RemainingRows is the task's row budget before the operation. It is a
	// budget dimension rather than an exposure one, and the visible row limit
	// derives from it, so reproducing the limits needs it.
	RemainingRows int64 `json:"remaining_rows"`
	// UsesExpandedEvidence and HasExposureContext are the two flags the limit
	// derivation branches on.
	UsesExpandedEvidence bool `json:"uses_expanded_evidence"`
	HasExposureContext   bool `json:"has_exposure_context"`
	// SHA256 is the canonical digest over every member above.
	SHA256 string `json:"sha256"`
}

// Seal fills SHA256 and validates the result.
func (ledger ExposureLedgerBeforeV1) Seal() (ExposureLedgerBeforeV1, error) {
	ledger.Version = ExposureLedgerBeforeV1Version
	ledger.SHA256 = ""
	digest, err := ledger.digest()
	if err != nil {
		return ExposureLedgerBeforeV1{}, err
	}
	ledger.SHA256 = digest
	if err := ledger.Validate(); err != nil {
		return ExposureLedgerBeforeV1{}, err
	}
	return ledger, nil
}

// Validate rejects a pre-state that cannot reproduce a row limit.
func (ledger ExposureLedgerBeforeV1) Validate() error {
	if ledger.Version != ExposureLedgerBeforeV1Version {
		return fmt.Errorf("exposure ledger pre-state version %q is unsupported; want %s",
			ledger.Version, ExposureLedgerBeforeV1Version)
	}
	if strings.TrimSpace(ledger.ProfileVersion) == "" {
		return errors.New("exposure ledger pre-state names no profile version")
	}
	if strings.TrimSpace(ledger.RootTaskID) == "" {
		return errors.New("exposure ledger pre-state names no root task")
	}
	if ledger.RootEpoch < 0 {
		return fmt.Errorf("exposure ledger pre-state root epoch is %d", ledger.RootEpoch)
	}
	if ledger.RemainingRows < 0 {
		return fmt.Errorf("exposure ledger pre-state remaining_rows is %d", ledger.RemainingRows)
	}
	for name, vector := range map[string]FactVector{
		"limits": ledger.Limits, "used": ledger.Used, "remaining": ledger.Remaining,
	} {
		if err := vector.validate(name); err != nil {
			return err
		}
	}
	// The three vectors must agree. A remaining vector that does not equal
	// limits minus used describes no ledger, and it is exactly the field a
	// caller would have to forge to widen a row limit.
	if computed := ledger.Limits.minus(ledger.Used); computed != ledger.Remaining {
		return fmt.Errorf("exposure ledger pre-state remaining %+v is not limits minus used %+v",
			ledger.Remaining, computed)
	}
	// Expanded evidence settles the companion against the influence budget, so
	// claiming it without an exposure context is incoherent.
	if ledger.UsesExpandedEvidence && !ledger.HasExposureContext {
		return errors.New("exposure ledger pre-state claims expanded evidence without an exposure context")
	}
	expected, err := ledger.digest()
	if err != nil {
		return err
	}
	if ledger.SHA256 != expected {
		return fmt.Errorf("exposure ledger pre-state digest is %s but its members digest to %s",
			short(ledger.SHA256), short(expected))
	}
	return nil
}

func (ledger ExposureLedgerBeforeV1) digest() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(exposureLedgerDomain))
	hash.Write([]byte{0})
	writeString(hash, "version", ExposureLedgerBeforeV1Version)
	if err := writeChecked(hash, "profile_version", ledger.ProfileVersion); err != nil {
		return "", err
	}
	if err := writeChecked(hash, "root_task_id", ledger.RootTaskID); err != nil {
		return "", err
	}
	writeInt(hash, "root_epoch", ledger.RootEpoch)
	writeVector(hash, "limits", ledger.Limits)
	writeVector(hash, "used", ledger.Used)
	writeVector(hash, "remaining", ledger.Remaining)
	writeInt(hash, "remaining_rows", ledger.RemainingRows)
	writeBool(hash, "uses_expanded_evidence", ledger.UsesExpandedEvidence)
	writeBool(hash, "has_exposure_context", ledger.HasExposureContext)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// TargetRecordV1 is one physical statement of an operation.
type TargetRecordV1 struct {
	Role TargetRole `json:"role"`
	// Authorized records that sqlpolicy admitted the statement. Executed records
	// that it was sent to the Connector. They are separate because a semantic
	// replay authorizes its targets in order to derive the semantic key and then
	// executes neither; collapsing them would make that path indistinguishable
	// from a novel execution.
	Authorized bool `json:"authorized"`
	Executed   bool `json:"executed"`
	// ExactSQLSHA256 digests the authorized executable bytes, constants
	// included; StrictASTSHA256 is the structural identity the observer keys on.
	// Both are carried: the structural digest cannot see a constant-only
	// mutation, and the exact digest cannot be compared against
	// pg_stat_statements.
	ExactSQLSHA256  string `json:"exact_sql_sha256"`
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// RowLimit is the limit rendered into those exact bytes.
	RowLimit int64 `json:"row_limit"`
	// PolicyFingerprint is sqlpolicy's own audit fingerprint.
	PolicyFingerprint string `json:"policy_fingerprint"`
	// PolicyRendererVersion and PolicyRendererDigest identify the renderer that
	// produced the bytes. A renderer change alters the executed statement
	// without altering the plan, so an unbound renderer would let the exact
	// digest move for a reason nothing recorded.
	PolicyRendererVersion string `json:"policy_renderer_version"`
	PolicyRendererDigest  string `json:"policy_renderer_digest"`
	// PreparedTargetBindingSHA256 ties this record to the prepared target the
	// compiler produced, so a statement cannot be swapped for another target's.
	PreparedTargetBindingSHA256 string `json:"prepared_target_binding_sha256"`
}

func (target TargetRecordV1) validate() error {
	if target.Role != RoleVisible && target.Role != RoleCompanion {
		return fmt.Errorf("target role %q is neither visible nor companion", target.Role)
	}
	// A statement cannot have executed without being authorized. This is the
	// invariant that makes "executed" meaningful rather than decorative.
	if target.Executed && !target.Authorized {
		return fmt.Errorf("the %s target is marked executed but not authorized", target.Role)
	}
	if !target.Authorized {
		return fmt.Errorf("the %s target is not authorized; an unauthorized target has no execution binding", target.Role)
	}
	for name, digest := range map[string]string{
		"exact_sql_sha256":               target.ExactSQLSHA256,
		"strict_ast_sha256":              target.StrictASTSHA256,
		"prepared_target_binding_sha256": target.PreparedTargetBindingSHA256,
		"policy_renderer_digest":         target.PolicyRendererDigest,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("the %s target's %s is not a lowercase SHA-256", target.Role, name)
		}
	}
	if target.RowLimit < 1 {
		return fmt.Errorf("the %s target's row limit is %d", target.Role, target.RowLimit)
	}
	for name, value := range map[string]string{
		"policy_fingerprint":      target.PolicyFingerprint,
		"policy_renderer_version": target.PolicyRendererVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("the %s target carries no %s", target.Role, name)
		}
	}
	return nil
}

func (target TargetRecordV1) digest() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(targetDomain))
	hash.Write([]byte{0})
	if err := writeChecked(hash, "role", string(target.Role)); err != nil {
		return "", err
	}
	writeBool(hash, "authorized", target.Authorized)
	writeBool(hash, "executed", target.Executed)
	writeString(hash, "exact_sql_sha256", target.ExactSQLSHA256)
	writeString(hash, "strict_ast_sha256", target.StrictASTSHA256)
	writeInt(hash, "row_limit", target.RowLimit)
	if err := writeChecked(hash, "policy_fingerprint", target.PolicyFingerprint); err != nil {
		return "", err
	}
	if err := writeChecked(hash, "policy_renderer_version", target.PolicyRendererVersion); err != nil {
		return "", err
	}
	writeString(hash, "policy_renderer_digest", target.PolicyRendererDigest)
	writeString(hash, "prepared_target_binding_sha256", target.PreparedTargetBindingSHA256)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// --- canonical framing -------------------------------------------------------
//
// Every member is written as name, length, value with NUL separators, so no two
// distinct member sets can produce one digest by concatenation.

type hashWriter interface{ Write([]byte) (int, error) }

func writeString(hash hashWriter, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}

// writeChecked refuses a value carrying a NUL, which is the only byte that
// could confuse the framing.
func writeChecked(hash hashWriter, name, value string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s contains a NUL byte", name)
	}
	writeString(hash, name, value)
	return nil
}

func writeInt(hash hashWriter, name string, value int64) {
	writeString(hash, name, fmt.Sprintf("%d", value))
}

func writeBool(hash hashWriter, name string, value bool) {
	writeString(hash, name, fmt.Sprintf("%t", value))
}

func writeVector(hash hashWriter, name string, vector FactVector) {
	writeInt(hash, name+".release_facts", vector.ReleaseFacts)
	writeInt(hash, name+".influence_facts", vector.InfluenceFacts)
	writeInt(hash, name+".outcome_facts", vector.OutcomeFacts)
	writeInt(hash, name+".predicate_atoms", vector.PredicateAtoms)
	writeInt(hash, name+".composite_outcomes", vector.CompositeOutcomes)
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}

func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
