package queryreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// validateExecutionBinding dispatches on the execution-binding version the
// receipt's capabilities require.
//
// The dispatch is strict in both directions. A version that requires V1 forbids
// V2 and vice versa, and a version that requires neither forbids both: a receipt
// whose signature does not cover a field must not be allowed to carry that
// field, or a holder could attach an execution binding to a V8 receipt and
// present it as signed.
func (r QueryReceiptV1) validateExecutionBinding(capabilities Capabilities) error {
	if r.ExecutionBinding != nil && !capabilities.RequiresExecutionBindingV1() {
		return fmt.Errorf("%w: a V%s receipt carries a QueryExecutionBindingV1 its signature does not cover",
			ErrInvalidReceipt, r.Version)
	}
	if r.ExecutionBindingV2 != nil && !capabilities.RequiresExecutionBindingV2() {
		return fmt.Errorf("%w: a V%s receipt carries a QueryExecutionBindingV2 its signature does not cover",
			ErrInvalidReceipt, r.Version)
	}
	if !capabilities.RequiresExposureLedgerBefore {
		if r.ExposureLedgerBefore != nil {
			return fmt.Errorf("%w: a V%s receipt carries an exposure ledger pre-state its signature does not cover",
				ErrInvalidReceipt, r.Version)
		}
		return nil
	}
	if r.ExposureLedgerBefore == nil {
		return fmt.Errorf("%w: a V%s receipt requires the signed exposure ledger pre-state",
			ErrInvalidReceipt, r.Version)
	}
	// Only a completed query has an execution to describe. A released one never
	// invoked the Connector, an indeterminate one cannot prove what completed, and
	// a failed one cannot prove which of its targets ran; the control store
	// refuses to persist a binding for any of them, and the receipt must refuse to
	// carry one for the same reason.
	//
	// Under V8 and V9 this was implied by the unconditional artifact intent, which
	// already required COMPLETED. V10 lifted that requirement for inline delivery
	// and would have taken this with it, leaving an inline V10 free to report
	// FAILED while carrying a description of what it executed.
	if r.Status != StatusCompleted {
		return fmt.Errorf("%w: a %s query carries execution evidence; only a completed query has an "+
			"execution to describe", ErrInvalidReceipt, r.Status)
	}
	if err := r.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("%w: exposure ledger pre-state: %v", ErrInvalidReceipt, err)
	}
	switch capabilities.ExecutionBindingVersion {
	case 1:
		return r.validateExecutionBindingV1()
	case 2:
		return r.validateExecutionBindingV2()
	default:
		// A version requiring a pre-state but no binding would leave the pre-state
		// describing nothing. Nothing in the table does that; saying so beats
		// falling through silently if something later does.
		return fmt.Errorf("%w: V%s requires an exposure ledger pre-state but no execution binding",
			ErrInvalidReceipt, r.Version)
	}
}

// validateExecutionBindingV1 is V9's rule set, unchanged.
func (r QueryReceiptV1) validateExecutionBindingV1() error {
	if r.ExecutionBinding == nil {
		return fmt.Errorf("%w: V9 requires the signed query execution binding", ErrInvalidReceipt)
	}
	binding := *r.ExecutionBinding
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("%w: query execution binding: %v", ErrInvalidReceipt, err)
	}
	return r.requireBindingAgreesWithSignedPreState(boundExecution{
		exposureProfileVersion:     binding.ExposureProfileVersion,
		usesExpandedEvidence:       binding.UsesExpandedEvidence,
		exposureLedgerBeforeSHA256: binding.ExposureLedgerBeforeSHA256,
		budgetBeforeSHA256:         binding.BudgetBeforeSHA256,
		visibleRowLimit:            binding.VisibleRowLimit,
		companionEvidenceRows:      binding.CompanionEvidenceRows,
		hasCompanion:               binding.Companion != nil,
	})
}

// boundExecution is the part of an execution binding the receipt checks against
// its own signed pre-states.
//
// It exists so the rules below have exactly one implementation. V1 and V2 differ
// in how they carry these values -- V2 reads the expanded-evidence state out of
// the preparation rather than from a flag beside it -- but not in what the
// receipt requires of them, and a second copy of these checks written for V2
// would be a second thing to keep in step with the first.
type boundExecution struct {
	exposureProfileVersion     string
	usesExpandedEvidence       bool
	exposureLedgerBeforeSHA256 string
	budgetBeforeSHA256         string
	visibleRowLimit            int64
	companionEvidenceRows      int64
	hasCompanion               bool
}

func (r QueryReceiptV1) requireBindingAgreesWithSignedPreState(bound boundExecution) error {
	// The binding must name the pre-state carried beside it. Without this the
	// receipt could sign a binding derived from one ledger and a pre-state from
	// another, and the limits would reproduce against neither.
	if bound.exposureLedgerBeforeSHA256 != r.ExposureLedgerBefore.SHA256 {
		return fmt.Errorf("%w: the execution binding names exposure pre-state %s but the receipt carries %s",
			ErrInvalidReceipt, shortDigest(bound.exposureLedgerBeforeSHA256),
			shortDigest(r.ExposureLedgerBefore.SHA256))
	}
	// Likewise for the budget: budget_before is already signed on the receipt, so
	// the binding must point at that exact state rather than at some other.
	budgetDigest, err := BudgetStateSHA256(r.BudgetBefore)
	if err != nil {
		return fmt.Errorf("%w: cannot canonicalize budget_before: %v", ErrInvalidReceipt, err)
	}
	if bound.budgetBeforeSHA256 != budgetDigest {
		return fmt.Errorf("%w: the execution binding names budget pre-state %s but the receipt's budget_before "+
			"digests to %s", ErrInvalidReceipt, shortDigest(bound.budgetBeforeSHA256), shortDigest(budgetDigest))
	}
	// The exposure profile the limits were derived under must be the profile the
	// ledger belongs to, and the expanded-evidence flag must agree: they are the
	// two inputs the companion's row limit branches on.
	if bound.exposureProfileVersion != r.ExposureLedgerBefore.ProfileVersion {
		return fmt.Errorf("%w: the execution binding derives under profile %q but the pre-state is %q",
			ErrInvalidReceipt, bound.exposureProfileVersion, r.ExposureLedgerBefore.ProfileVersion)
	}
	if bound.usesExpandedEvidence != r.ExposureLedgerBefore.UsesExpandedEvidence {
		return fmt.Errorf("%w: the execution binding and its pre-state disagree about expanded evidence",
			ErrInvalidReceipt)
	}
	// The pre-state's row budget is not an independent assertion. budget_before is
	// already signed on the receipt and already says what the task had left, so a
	// remaining_rows that does not equal limits minus used minus reserved
	// describes a budget the receipt does not carry -- and remaining_rows is
	// precisely the field a forger would raise to widen the visible row limit.
	if err := requireRemainingRowsMatchBudget(r.BudgetBefore, *r.ExposureLedgerBefore); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	// Nor is the ledger identity independent. The exposure evidence names the
	// ledger the operation settled against; the pre-state must name the same one.
	if r.Exposure != nil {
		for _, field := range []struct {
			name              string
			receipt, prestate string
		}{
			{"root task", r.Exposure.RootTaskID, r.ExposureLedgerBefore.RootTaskID},
			{"profile version", r.Exposure.ProfileVersion, r.ExposureLedgerBefore.ProfileVersion},
		} {
			if field.receipt != field.prestate {
				return fmt.Errorf("%w: the exposure evidence and the ledger pre-state name different %ss (%q and %q)",
					ErrInvalidReceipt, field.name, field.receipt, field.prestate)
			}
		}
		// The epoch is bound as an ordering rather than an equality, and the
		// difference is not a weakening -- it is what the epoch means.
		//
		// ExposureLedgerBefore.RootEpoch is the epoch the operation was AUTHORIZED
		// against; Exposure.RootEpoch is the epoch its charge LANDED at. A novel
		// observation advances the root head by one as it settles, so requiring the
		// two to be equal would reject every novel paired execution -- that is,
		// every case the execution evidence exists to describe. The epoch is
		// monotonic, so requiring the pre-state's to be no later than the charge's
		// still leaves it unforgeable in the direction that matters: an older epoch
		// cannot be claimed to have authorized a charge that settled against a
		// newer one.
		if r.ExposureLedgerBefore.RootEpoch > r.Exposure.RootEpoch {
			return fmt.Errorf("%w: the ledger pre-state was read at root epoch %d but the charge settled at "+
				"epoch %d; the epoch is monotonic, so the pre-state cannot postdate the charge",
				ErrInvalidReceipt, r.ExposureLedgerBefore.RootEpoch, r.Exposure.RootEpoch)
		}
	}
	// The row limits must be reproducible from the signed pre-state. This is the
	// check that makes the binding evidence rather than assertion: a limit the
	// pre-state cannot derive is a limit nothing authorized.
	if err := requireReproducibleLimits(bound, *r.ExposureLedgerBefore); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	return nil
}

// requireRemainingRowsMatchBudget cross-binds the two pre-states the receipt
// carries.
//
// It is fail-closed by construction: a budget whose used plus reserved exceeds
// its limit yields a negative remainder, which no valid pre-state can equal
// because ExposureLedgerBeforeV1.Validate already refuses a negative
// remaining_rows. Such a receipt is rejected here rather than silently clamped
// to zero, because clamping would turn an incoherent ledger into a well-formed
// one.
func requireRemainingRowsMatchBudget(budget BudgetStateV1, ledger querybinding.ExposureLedgerBeforeV1) error {
	if budget.Limits.Rows < 0 || budget.Used.Rows < 0 || budget.Reserved.Rows < 0 {
		return fmt.Errorf("budget_before carries a negative row dimension (limits %d, used %d, reserved %d)",
			budget.Limits.Rows, budget.Used.Rows, budget.Reserved.Rows)
	}
	remaining := budget.Limits.Rows - budget.Used.Rows - budget.Reserved.Rows
	if remaining < 0 {
		return fmt.Errorf("budget_before uses and reserves %d of %d rows, so it leaves %d remaining",
			budget.Used.Rows+budget.Reserved.Rows, budget.Limits.Rows, remaining)
	}
	if ledger.RemainingRows != remaining {
		return fmt.Errorf("the ledger pre-state claims %d remaining rows but budget_before leaves %d "+
			"(limits %d - used %d - reserved %d)", ledger.RemainingRows, remaining,
			budget.Limits.Rows, budget.Used.Rows, budget.Reserved.Rows)
	}
	return nil
}

// requireReproducibleLimits recomputes the row limits from the signed pre-state
// through the shared production derivation.
//
// physicalquery is deliberately not imported here: it depends on sqlpolicy, and
// the receipt must remain a description rather than acquire the ability to
// authorize. The arithmetic reproduced is the part that does not need an
// authorizer -- the requested visible limit and the companion pair -- which is
// exactly what the pre-state is carried to support.
func requireReproducibleLimits(bound boundExecution,
	ledger querybinding.ExposureLedgerBeforeV1) error {
	// The authorized visible limit is what production feeds the companion, and
	// the policy engine may have lowered it below the requested one. So the
	// requested limit bounds it rather than equalling it.
	requested := ledger.RemainingRows
	if ledger.HasExposureContext && !ledger.UsesExpandedEvidence {
		requested = min64(requested, ledger.Limits.InfluenceFacts)
	}
	// This bound holds whether or not a companion executed. It used to be skipped
	// for a single-query binding, which left the one limit that path does render
	// into executable SQL unchecked against the state that authorized it.
	if bound.visibleRowLimit > requested {
		return fmt.Errorf("the visible row limit is %d but the signed pre-state authorizes at most %d",
			bound.visibleRowLimit, requested)
	}
	if !bound.hasCompanion {
		return nil
	}
	wantEvidenceRows := bound.visibleRowLimit
	if ledger.UsesExpandedEvidence {
		wantEvidenceRows = ledger.Limits.InfluenceFacts
	}
	if bound.companionEvidenceRows != wantEvidenceRows {
		return fmt.Errorf("companion evidence rows are %d but the signed pre-state derives %d",
			bound.companionEvidenceRows, wantEvidenceRows)
	}
	return nil
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// BudgetStateSHA256 is the canonical digest of a signed budget pre-state.
//
// The execution binding names the budget it derived limits from by digest
// rather than repeating the numbers, so the binding and the receipt cannot
// describe two different budgets while both looking well-formed.
func BudgetStateSHA256(state BudgetStateV1) (string, error) {
	canonical, err := approval.CanonicalJSON(map[string]any{
		"domain": "TASKGATE-QUERY-RECEIPT-BUDGET-STATE-V1",
		"limits": map[string]int64{
			"queries": state.Limits.Queries, "rows": state.Limits.Rows, "db_ms": state.Limits.DBMS,
		},
		"used": map[string]int64{
			"queries": state.Used.Queries, "rows": state.Used.Rows, "db_ms": state.Used.DBMS,
		},
		"reserved": map[string]int64{
			"queries": state.Reserved.Queries, "rows": state.Reserved.Rows, "db_ms": state.Reserved.DBMS,
		},
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
