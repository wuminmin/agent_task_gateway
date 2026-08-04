// Package querybinding defines the signed, SQL-free description of what one
// governed operation actually executed.
//
// # Why this exists
//
// Query Receipt V8 binds the authorization, the budget and the exposure
// accounting, but nothing in it says which physical statements ran. The
// evaluation filled that gap by re-deriving the statements after the fact and
// treating the result as though the Gateway had signed it. That is not evidence
// about the execution; it is a second opinion about it, and the two can differ
// in exactly the case the evidence exists to detect -- a constant-only mutation
// that pg_stat_statements has normalized away.
//
// QueryExecutionBindingV1 is constructed by the Gateway from the decisions it
// actually passes to the Connector, and is covered by the V9 receipt signature.
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

// Versions of the two structures. They are separate because the ledger pre-state
// is meaningful on its own: it is what the finalizer needs to reproduce the row
// limits, and it is signed even when no statement executed.
const (
	ExposureLedgerBeforeV1Version  = "taskgate-exposure-ledger-before-v1"
	QueryExecutionBindingV1Version = "taskgate-query-execution-binding-v1"
)

const (
	exposureLedgerDomain = "TASKGATE-EXPOSURE-LEDGER-BEFORE-V1"
	executionDomain      = "TASKGATE-QUERY-EXECUTION-BINDING-V1"
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

// QueryExecutionBindingV1 is what one operation executed.
type QueryExecutionBindingV1 struct {
	Version  string   `json:"version"`
	PathKind PathKind `json:"path_kind"`
	// PreparedOperationBindingSHA256 ties the whole binding to the operation the
	// compiler prepared.
	PreparedOperationBindingSHA256 string `json:"prepared_operation_binding_sha256"`
	ExposureProfileVersion         string `json:"exposure_profile_version"`
	UsesExpandedEvidence           bool   `json:"uses_expanded_evidence"`
	// The three derived limits. They are signed rather than recomputed on
	// reading, so a finalizer that recomputes them is checking the Gateway
	// rather than agreeing with itself.
	VisibleRowLimit       int64 `json:"visible_row_limit"`
	CompanionEvidenceRows int64 `json:"companion_evidence_rows"`
	CompanionPolicyRows   int64 `json:"companion_policy_rows"`
	// The signed pre-state these limits were derived from.
	BudgetBeforeSHA256         string `json:"budget_before_sha256"`
	ExposureLedgerBeforeSHA256 string `json:"exposure_ledger_before_sha256"`
	// The compilation identities. Together they say which plan, compiled by
	// which compiler, against which dictionary and sidecar grants, produced the
	// statements below.
	PlanSHA256                 string `json:"plan_sha256"`
	CompilerVersion            string `json:"compiler_version"`
	CompilerSHA256             string `json:"compiler_sha256"`
	OrdinalDictionarySetSHA256 string `json:"ordinal_dictionary_set_sha256,omitempty"`
	SidecarGrantsSHA256        string `json:"sidecar_grants_sha256,omitempty"`

	Visible   TargetRecordV1  `json:"visible"`
	Companion *TargetRecordV1 `json:"companion,omitempty"`

	SHA256 string `json:"query_execution_binding_sha256"`
}

// Seal fills SHA256 and validates the result.
func (binding QueryExecutionBindingV1) Seal() (QueryExecutionBindingV1, error) {
	binding.Version = QueryExecutionBindingV1Version
	binding.SHA256 = ""
	digest, err := binding.digest()
	if err != nil {
		return QueryExecutionBindingV1{}, err
	}
	binding.SHA256 = digest
	if err := binding.Validate(); err != nil {
		return QueryExecutionBindingV1{}, err
	}
	return binding, nil
}

// Validate rejects a binding that does not describe a coherent execution.
//
// The path semantics are enforced here rather than left to the caller, because
// every consumer would otherwise have to re-derive them and one of them would
// eventually get it wrong in the permissive direction.
func (binding QueryExecutionBindingV1) Validate() error {
	if binding.Version != QueryExecutionBindingV1Version {
		return fmt.Errorf("query execution binding version %q is unsupported; want %s",
			binding.Version, QueryExecutionBindingV1Version)
	}
	if !binding.PathKind.valid() {
		return fmt.Errorf("path_kind %q is not one of %v", binding.PathKind, PathKinds())
	}
	// An idempotent replay executes nothing and creates no new binding; it
	// returns the original signed receipt. A binding claiming that path is
	// therefore a contradiction.
	if binding.PathKind == PathIdempotentReplay {
		return errors.New("idempotent_replay produces no new execution binding; " +
			"the original signed receipt is returned unchanged")
	}
	for name, digest := range map[string]string{
		"prepared_operation_binding_sha256": binding.PreparedOperationBindingSHA256,
		"budget_before_sha256":              binding.BudgetBeforeSHA256,
		"exposure_ledger_before_sha256":     binding.ExposureLedgerBeforeSHA256,
		"plan_sha256":                       binding.PlanSHA256,
		"compiler_sha256":                   binding.CompilerSHA256,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("%s is not a lowercase SHA-256", name)
		}
	}
	for name, value := range map[string]string{
		"exposure_profile_version": binding.ExposureProfileVersion,
		"compiler_version":         binding.CompilerVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("the execution binding carries no %s", name)
		}
	}
	for name, digest := range map[string]string{
		"ordinal_dictionary_set_sha256": binding.OrdinalDictionarySetSHA256,
		"sidecar_grants_sha256":         binding.SidecarGrantsSHA256,
	} {
		if digest != "" && !validSHA256(digest) {
			return fmt.Errorf("%s is not a lowercase SHA-256", name)
		}
	}
	if binding.Visible.Role != RoleVisible {
		return fmt.Errorf("the visible target carries role %q", binding.Visible.Role)
	}
	if err := binding.Visible.validate(); err != nil {
		return err
	}
	if binding.Companion != nil {
		if binding.Companion.Role != RoleCompanion {
			return fmt.Errorf("the companion target carries role %q", binding.Companion.Role)
		}
		if err := binding.Companion.validate(); err != nil {
			return err
		}
	}
	if err := binding.validatePathSemantics(); err != nil {
		return err
	}
	if err := binding.validateLimits(); err != nil {
		return err
	}
	expected, err := binding.digest()
	if err != nil {
		return err
	}
	if binding.SHA256 != expected {
		return fmt.Errorf("query_execution_binding_sha256 is %s but the binding's members digest to %s",
			short(binding.SHA256), short(expected))
	}
	return nil
}

func (binding QueryExecutionBindingV1) validatePathSemantics() error {
	switch binding.PathKind {
	case PathPairedNovel:
		if binding.Companion == nil {
			return errors.New("paired_novel executes a companion statement but none is bound")
		}
		if !binding.Visible.Executed || !binding.Companion.Executed {
			return errors.New("paired_novel executes both targets; one is bound as not executed")
		}
	case PathSingleQuery:
		if binding.Companion != nil {
			return errors.New("single_query executes no companion statement but one is bound")
		}
		if !binding.Visible.Executed {
			return errors.New("single_query executes its visible statement; it is bound as not executed")
		}
	case PathSemanticReplay:
		// The targets may be authorized -- deriving the semantic key requires
		// authorizing them -- but nothing runs, so the observer must see a zero
		// target delta. A replay that executed is not a replay.
		if binding.Visible.Executed {
			return errors.New("semantic_replay executes no statement but the visible target is bound as executed")
		}
		if binding.Companion != nil && binding.Companion.Executed {
			return errors.New("semantic_replay executes no statement but the companion target is bound as executed")
		}
	}
	return nil
}

func (binding QueryExecutionBindingV1) validateLimits() error {
	if binding.VisibleRowLimit != binding.Visible.RowLimit {
		return fmt.Errorf("the binding's visible row limit is %d but its visible target rendered %d",
			binding.VisibleRowLimit, binding.Visible.RowLimit)
	}
	if binding.Companion == nil {
		if binding.CompanionEvidenceRows != 0 || binding.CompanionPolicyRows != 0 {
			return errors.New("companion row limits are set on a binding with no companion target")
		}
		if binding.UsesExpandedEvidence {
			return errors.New("expanded evidence settles against the companion, but no companion is bound")
		}
		return nil
	}
	if binding.CompanionPolicyRows != binding.Companion.RowLimit {
		return fmt.Errorf("the binding's companion policy rows are %d but its companion target rendered %d",
			binding.CompanionPolicyRows, binding.Companion.RowLimit)
	}
	if binding.CompanionEvidenceRows < 1 {
		return fmt.Errorf("companion evidence rows are %d; the exposure budget left no evidence row",
			binding.CompanionEvidenceRows)
	}
	// Under expanded evidence the policy limit is one more than the evidence
	// rows, so a truncated result is distinguishable from a complete one.
	// Otherwise the two are the same number.
	wantPolicyRows := binding.CompanionEvidenceRows
	if binding.UsesExpandedEvidence {
		wantPolicyRows = binding.CompanionEvidenceRows + 1
	}
	if binding.CompanionPolicyRows != wantPolicyRows {
		return fmt.Errorf("companion policy rows are %d but %d evidence rows under expanded=%t derive %d",
			binding.CompanionPolicyRows, binding.CompanionEvidenceRows,
			binding.UsesExpandedEvidence, wantPolicyRows)
	}
	return nil
}

func (binding QueryExecutionBindingV1) digest() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(executionDomain))
	hash.Write([]byte{0})
	writeString(hash, "version", QueryExecutionBindingV1Version)
	if err := writeChecked(hash, "path_kind", string(binding.PathKind)); err != nil {
		return "", err
	}
	writeString(hash, "prepared_operation_binding_sha256", binding.PreparedOperationBindingSHA256)
	if err := writeChecked(hash, "exposure_profile_version", binding.ExposureProfileVersion); err != nil {
		return "", err
	}
	writeBool(hash, "uses_expanded_evidence", binding.UsesExpandedEvidence)
	writeInt(hash, "visible_row_limit", binding.VisibleRowLimit)
	writeInt(hash, "companion_evidence_rows", binding.CompanionEvidenceRows)
	writeInt(hash, "companion_policy_rows", binding.CompanionPolicyRows)
	writeString(hash, "budget_before_sha256", binding.BudgetBeforeSHA256)
	writeString(hash, "exposure_ledger_before_sha256", binding.ExposureLedgerBeforeSHA256)
	writeString(hash, "plan_sha256", binding.PlanSHA256)
	if err := writeChecked(hash, "compiler_version", binding.CompilerVersion); err != nil {
		return "", err
	}
	writeString(hash, "compiler_sha256", binding.CompilerSHA256)
	writeString(hash, "ordinal_dictionary_set_sha256", binding.OrdinalDictionarySetSHA256)
	writeString(hash, "sidecar_grants_sha256", binding.SidecarGrantsSHA256)

	visible, err := binding.Visible.digest()
	if err != nil {
		return "", err
	}
	writeString(hash, "visible", visible)
	// The absent companion is framed explicitly rather than skipped. Skipping it
	// would let a binding with no companion collide with one whose companion
	// digested to the empty string.
	if binding.Companion == nil {
		writeString(hash, "companion", "")
	} else {
		companion, err := binding.Companion.digest()
		if err != nil {
			return "", err
		}
		writeString(hash, "companion", companion)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Equal reports whether two bindings are the same execution.
//
// Compared through the canonical digest rather than field by field, so a member
// added later cannot be silently excluded from the comparison.
func (binding QueryExecutionBindingV1) Equal(other QueryExecutionBindingV1) bool {
	return binding.SHA256 != "" && binding.SHA256 == other.SHA256
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
