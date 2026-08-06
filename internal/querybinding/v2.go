package querybinding

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// QueryExecutionBindingV2Version identifies the second execution binding.
//
// # What V1 could not say
//
// V1 named the preparation by a single digest, PreparedOperationBindingSHA256,
// and then repeated a handful of the compilation identities beside it -- the
// plan, the compiler, the dictionary set, the sidecar grants. That was adequate
// while the prepared binding was little more than those same identities.
//
// It stopped being adequate once preparation became the shared, sealed thing the
// finalizer independently reproduces. A prepared binding now measures the policy
// grant the statement is actually admitted against, the Publications each ordinal
// source resolved to, the predicate footprint, the View revision and the four
// semantic View identities, the estimated base facts, the whole canonicalized
// input set and the typed compiler identity. A receipt that signs only the digest
// of that document proves nothing about those members to a holder who was not
// handed the document: the digest is checkable only against a prepared binding
// someone else supplies, and supplying a different one is precisely the move the
// evidence exists to refuse.
//
// V2 therefore carries the complete sealed PreparedOperationBindingV1 rather
// than its digest, and drops every identity the prepared binding already
// contains, so no member has two sources that could disagree.
const QueryExecutionBindingV2Version = "taskgate-query-execution-binding-v2"

const executionDomainV2 = "TASKGATE-QUERY-EXECUTION-BINDING-V2"

// QueryExecutionBindingV2 is what one operation prepared, authorized and
// executed.
//
// It is not V1 plus a field. The identities V1 repeated -- PlanSHA256,
// CompilerVersion, CompilerSHA256, OrdinalDictionarySetSHA256,
// SidecarGrantsSHA256 -- are deliberately absent: each of them is a member of
// PreparedOperation, and carrying it twice would create a second place a reader
// could take the answer from. The one identity that is carried twice, the typed
// compiler identity, is required to equal the digest the prepared binding names,
// and is present only because the target records' renderer members are otherwise
// uncheckable against the preparation.
type QueryExecutionBindingV2 struct {
	Version  string   `json:"version"`
	PathKind PathKind `json:"path_kind"`

	// PreparedOperation is the complete sealed preparation, not a digest of it.
	// It carries no statement, no column name and no physical relation name, so
	// carrying it whole discloses nothing a digest withheld.
	PreparedOperation preparedbinding.PreparedOperationBindingV1 `json:"prepared_operation"`
	// Compiler is the identity PreparedOperation.CompilerIdentitySHA256 names.
	// Validate requires the two to agree, so this is a readable projection of a
	// value the preparation already pins rather than an independent assertion.
	Compiler preparedbinding.CompilerIdentityV1 `json:"compiler_identity"`

	// ExposureProfileVersion is the profile the row limits below were derived
	// under. The preparation does not carry it: preparation is what the compiler
	// produced, and the profile belongs to the ledger the limits came from.
	ExposureProfileVersion string `json:"exposure_profile_version"`

	// The three derived limits. They are signed rather than recomputed on
	// reading, so a finalizer that recomputes them is checking the Gateway
	// rather than agreeing with itself.
	VisibleRowLimit       int64 `json:"visible_row_limit"`
	CompanionEvidenceRows int64 `json:"companion_evidence_rows"`
	CompanionPolicyRows   int64 `json:"companion_policy_rows"`

	// The signed pre-states these limits were derived from.
	BudgetBeforeSHA256         string `json:"budget_before_sha256"`
	ExposureLedgerBeforeSHA256 string `json:"exposure_ledger_before_sha256"`

	Visible   TargetRecordV1  `json:"visible"`
	Companion *TargetRecordV1 `json:"companion,omitempty"`

	SHA256 string `json:"query_execution_binding_sha256"`
}

// UsesExpandedEvidence reports the expanded-evidence state of this operation.
//
// It is a method rather than a member. V1 carried the flag beside the prepared
// digest and had to check the two against each other; here the preparation is
// the only place it is written down, so there is nothing left to disagree.
func (binding QueryExecutionBindingV2) UsesExpandedEvidence() bool {
	return binding.PreparedOperation.ExpandedEvidence
}

// Seal fills SHA256 and validates the result.
func (binding QueryExecutionBindingV2) Seal() (QueryExecutionBindingV2, error) {
	binding.Version = QueryExecutionBindingV2Version
	binding.SHA256 = ""
	digest, err := binding.digest()
	if err != nil {
		return QueryExecutionBindingV2{}, err
	}
	binding.SHA256 = digest
	if err := binding.Validate(); err != nil {
		return QueryExecutionBindingV2{}, err
	}
	return binding, nil
}

// Validate rejects a binding that does not describe a coherent execution of the
// preparation it carries.
//
// The cross-checks are the point of V2. A binding that merely contained a valid
// preparation and a valid pair of target records would prove that two coherent
// documents were stapled together, not that the statements executed are the ones
// that preparation produced.
func (binding QueryExecutionBindingV2) Validate() error {
	if binding.Version != QueryExecutionBindingV2Version {
		return fmt.Errorf("query execution binding version %q is unsupported; want %s",
			binding.Version, QueryExecutionBindingV2Version)
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
	if err := binding.PreparedOperation.Validate(); err != nil {
		return fmt.Errorf("prepared operation: %w", err)
	}
	if err := binding.Compiler.Validate(); err != nil {
		return fmt.Errorf("compiler identity: %w", err)
	}
	if binding.Compiler.SHA256 != binding.PreparedOperation.CompilerIdentitySHA256 {
		return fmt.Errorf("the binding carries compiler identity %s but its preparation was compiled by %s",
			short(binding.Compiler.SHA256), short(binding.PreparedOperation.CompilerIdentitySHA256))
	}
	for name, digest := range map[string]string{
		"budget_before_sha256":          binding.BudgetBeforeSHA256,
		"exposure_ledger_before_sha256": binding.ExposureLedgerBeforeSHA256,
	} {
		if !validSHA256(digest) {
			return fmt.Errorf("%s is not a lowercase SHA-256", name)
		}
	}
	if strings.TrimSpace(binding.ExposureProfileVersion) == "" {
		return errors.New("the execution binding carries no exposure_profile_version")
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
	if err := binding.validatePreparedTargets(); err != nil {
		return err
	}
	if err := binding.validateRendererIdentities(); err != nil {
		return err
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

// validatePreparedTargets ties each executed statement to the prepared target it
// was rendered from.
//
// Without this a binding could carry a preparation of one operation and the
// target records of another, and every member of both would still be internally
// coherent.
func (binding QueryExecutionBindingV2) validatePreparedTargets() error {
	if binding.PreparedOperation.HasCompanion != (binding.Companion != nil) {
		return fmt.Errorf("the preparation prepares has_companion=%t but the binding carries %d companion targets",
			binding.PreparedOperation.HasCompanion, boolToCount(binding.Companion != nil))
	}
	visible, err := binding.PreparedOperation.TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		return err
	}
	if binding.Visible.PreparedTargetBindingSHA256 != visible {
		return fmt.Errorf("the visible target was rendered from prepared target %s but the preparation prepared %s",
			short(binding.Visible.PreparedTargetBindingSHA256), short(visible))
	}
	if binding.Companion == nil {
		return nil
	}
	companion, err := binding.PreparedOperation.TargetSHA256(preparedbinding.RoleCompanion)
	if err != nil {
		return err
	}
	if binding.Companion.PreparedTargetBindingSHA256 != companion {
		return fmt.Errorf("the companion target was rendered from prepared target %s but the preparation prepared %s",
			short(binding.Companion.PreparedTargetBindingSHA256), short(companion))
	}
	return nil
}

// validateRendererIdentities requires the renderer that produced each executed
// statement to be the renderer the preparation was sealed against.
//
// A renderer change alters the executable bytes without altering the plan. V1
// recorded the renderer on each target record and had nothing to check it
// against, so a binding could name a renderer that never touched the prepared
// operation and still validate.
func (binding QueryExecutionBindingV2) validateRendererIdentities() error {
	targets := []struct {
		role   TargetRole
		record TargetRecordV1
	}{{RoleVisible, binding.Visible}}
	if binding.Companion != nil {
		targets = append(targets, struct {
			role   TargetRole
			record TargetRecordV1
		}{RoleCompanion, *binding.Companion})
	}
	for _, target := range targets {
		if target.record.PolicyRendererVersion != binding.Compiler.PolicyRendererVersion {
			return fmt.Errorf("the %s target was rendered by %q but the preparation was compiled against %q",
				target.role, target.record.PolicyRendererVersion, binding.Compiler.PolicyRendererVersion)
		}
		if target.record.PolicyRendererDigest != binding.Compiler.PolicyRendererSHA256 {
			return fmt.Errorf("the %s target was rendered by %s but the preparation was compiled against %s",
				target.role, short(target.record.PolicyRendererDigest), short(binding.Compiler.PolicyRendererSHA256))
		}
	}
	return nil
}

// validatePathSemantics is V1's, unchanged, plus the preparation cross-check
// that V1 had no preparation to make.
func (binding QueryExecutionBindingV2) validatePathSemantics() error {
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

// validateLimits is V1's derivation, reading the expanded-evidence state from
// the preparation instead of from a flag beside it.
func (binding QueryExecutionBindingV2) validateLimits() error {
	if binding.VisibleRowLimit != binding.Visible.RowLimit {
		return fmt.Errorf("the binding's visible row limit is %d but its visible target rendered %d",
			binding.VisibleRowLimit, binding.Visible.RowLimit)
	}
	if binding.Companion == nil {
		if binding.CompanionEvidenceRows != 0 || binding.CompanionPolicyRows != 0 {
			return errors.New("companion row limits are set on a binding with no companion target")
		}
		// PreparedOperationBindingV1.Validate already refuses expanded evidence
		// without a prepared companion, and validatePreparedTargets already ties
		// the prepared companion to this one, so there is nothing left to check.
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
	if binding.UsesExpandedEvidence() {
		wantPolicyRows = binding.CompanionEvidenceRows + 1
	}
	if binding.CompanionPolicyRows != wantPolicyRows {
		return fmt.Errorf("companion policy rows are %d but %d evidence rows under expanded=%t derive %d",
			binding.CompanionPolicyRows, binding.CompanionEvidenceRows,
			binding.UsesExpandedEvidence(), wantPolicyRows)
	}
	return nil
}

// digest frames the binding the way V1 does, with the preparation and the
// compiler identity entering by their own sealed digests.
//
// Entering by digest rather than by member is not a weakening: Validate
// recomputes both seals from their members, so a member that was changed either
// moves the seal -- and with it this digest -- or fails validation before this
// digest is ever compared.
func (binding QueryExecutionBindingV2) digest() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(executionDomainV2))
	hash.Write([]byte{0})
	writeString(hash, "version", QueryExecutionBindingV2Version)
	if err := writeChecked(hash, "path_kind", string(binding.PathKind)); err != nil {
		return "", err
	}
	writeString(hash, "prepared_operation_sha256", binding.PreparedOperation.SHA256)
	writeString(hash, "compiler_identity_sha256", binding.Compiler.SHA256)
	if err := writeChecked(hash, "exposure_profile_version", binding.ExposureProfileVersion); err != nil {
		return "", err
	}
	writeInt(hash, "visible_row_limit", binding.VisibleRowLimit)
	writeInt(hash, "companion_evidence_rows", binding.CompanionEvidenceRows)
	writeInt(hash, "companion_policy_rows", binding.CompanionPolicyRows)
	writeString(hash, "budget_before_sha256", binding.BudgetBeforeSHA256)
	writeString(hash, "exposure_ledger_before_sha256", binding.ExposureLedgerBeforeSHA256)

	visible, err := binding.Visible.digest()
	if err != nil {
		return "", err
	}
	writeString(hash, "visible", visible)
	// The absent companion is framed explicitly rather than skipped, for the same
	// reason as in V1: skipping would let a binding with no companion collide
	// with one whose companion digested to the empty string.
	if binding.Companion == nil {
		writeString(hash, "companion", "")
	} else {
		companion, companionErr := binding.Companion.digest()
		if companionErr != nil {
			return "", companionErr
		}
		writeString(hash, "companion", companion)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Equal reports whether two bindings are the same execution.
func (binding QueryExecutionBindingV2) Equal(other QueryExecutionBindingV2) bool {
	return binding.SHA256 != "" && binding.SHA256 == other.SHA256
}

// RequirePreparedSame reports whether an independently reconstructed preparation
// is the one this binding was executed from.
//
// This is the comparison the finalizer makes. It is offered here so the finalizer
// does not reach into the binding, pull the member out and write its own
// comparison -- which is how a comparison comes to cover fewer members than the
// document has.
func (binding QueryExecutionBindingV2) RequirePreparedSame(
	reconstructed preparedbinding.PreparedOperationBindingV1) error {
	return binding.PreparedOperation.RequireSame(reconstructed)
}

func boolToCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
