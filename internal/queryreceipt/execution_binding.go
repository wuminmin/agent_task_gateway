package queryreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// validateExecutionBinding holds a receipt to the execution evidence it carries.
//
// # The invariant this enforces, and why it is an equivalence
//
// A completed query carries an execution binding, and a query that carries one
// completed. Both directions are load-bearing and neither is the interesting one
// on its own.
//
// The forward direction is the claim the system exists to make: every query that
// returned rows says which physical statements produced them, in a form a
// finalizer can reconstruct from frozen inputs without being handed the SQL. It
// was the direction that stayed open while the Gateway was still growing into
// the shared preparation -- an absent binding read as "this path does not
// describe executions yet", which is a development state and not a contract. A
// contract that permits the evidence to be missing proves nothing about the
// receipts that do carry it, because nothing distinguishes a receipt whose path
// never bound one from a receipt whose binding was dropped.
//
// The reverse direction is what keeps the evidence honest. A released query
// never invoked the Connector, an indeterminate one cannot prove what completed,
// and a failed one cannot prove which of its targets ran. The Control Store
// refuses to persist a binding for any of them, and the receipt refuses to carry
// one for the same reason.
//
// The presence itself is signed -- signingPayload covers every member whether or
// not it is there -- so neither half can be stapled on or stripped after the
// fact.
func (r QueryReceiptV1) validateExecutionBinding() error {
	if (r.ExecutionBindingV2 != nil) != (r.Status == StatusCompleted) {
		if r.ExecutionBindingV2 == nil {
			return fmt.Errorf("%w: a completed query carries no execution binding; every completed "+
				"query states which physical statements produced its rows", ErrInvalidReceipt)
		}
		return fmt.Errorf("%w: a %s query carries execution evidence; only a completed query has an "+
			"execution to describe", ErrInvalidReceipt, r.Status)
	}
	if r.ExecutionBindingV2 == nil {
		// The pre-state travels with the binding it describes. Without the binding
		// there are no limits to reproduce, so a pre-state here would be a document
		// nothing is checked against -- and a document nothing checks is a place to
		// put whatever the holder prefers.
		if r.ExposureLedgerBefore != nil {
			return fmt.Errorf("%w: a %s query carries an exposure ledger pre-state but no execution "+
				"binding derived from it", ErrInvalidReceipt, r.Status)
		}
		return nil
	}
	return r.validateExecutionBindingV2()
}

// requireExposureEvidenceShape binds the three members that say, each in its own
// vocabulary, whether this operation accounted exposure.
//
// The binding names the profile its limits derived under, the pre-state carries
// the ledger they derived from, and the exposure block reports the charge that
// settled. A completed operation either did all three or none of them, and any
// mixture describes an operation that cannot have happened: a charge with no
// pre-state cannot be checked against what was authorized, a pre-state with no
// charge accounts an observation that never settled, and a profile with neither
// names rules held against nothing.
//
// The alternative -- coupling only the binding to the pre-state, as this did
// while every binding was an exposure binding -- has no way to express a
// completed non-exposure query at all. It would have to be given a fabricated
// empty ledger, which is a signed statement that an exposure ledger was read
// when none was.
func (r QueryReceiptV1) requireExposureEvidenceShape(accountsExposure bool) error {
	for _, member := range []struct {
		name    string
		present bool
	}{
		{"an exposure ledger pre-state", r.ExposureLedgerBefore != nil},
		{"exposure charge evidence", r.Exposure != nil},
	} {
		if member.present == accountsExposure {
			continue
		}
		if accountsExposure {
			return fmt.Errorf("%w: the execution binding derives its limits under exposure profile %q "+
				"but the receipt carries no %s", ErrInvalidReceipt,
				r.ExecutionBindingV2.ExposureProfileVersion, member.name)
		}
		return fmt.Errorf("%w: the execution binding accounts no exposure but the receipt carries %s",
			ErrInvalidReceipt, member.name)
	}
	return nil
}

// boundExecution is the part of an execution binding the receipt checks against
// its own signed pre-states.
//
// It is a projection rather than the binding itself so that these rules stay
// about what the receipt requires -- that the binding names the pre-states
// carried beside it and that its limits reproduce from them -- and not about how
// the binding happens to carry them.
type boundExecution struct {
	exposureProfileVersion     string
	usesExpandedEvidence       bool
	exposureLedgerBeforeSHA256 string
	budgetBeforeSHA256         string
	visibleRowLimit            int64
	companionEvidenceRows      int64
	hasCompanion               bool
}

// requireBindingNamesSignedBudget is the one pre-state check every binding
// makes.
//
// budget_before is already signed on the receipt and already says what the task
// had left, so the binding must point at that exact state rather than at some
// other. It is checked before the exposure half because it is what a
// non-exposure operation derives its whole visible limit from.
func (r QueryReceiptV1) requireBindingNamesSignedBudget(bound boundExecution) error {
	budgetDigest, err := BudgetStateSHA256(r.BudgetBefore)
	if err != nil {
		return fmt.Errorf("%w: cannot canonicalize budget_before: %v", ErrInvalidReceipt, err)
	}
	if bound.budgetBeforeSHA256 != budgetDigest {
		return fmt.Errorf("%w: the execution binding names budget pre-state %s but the receipt's budget_before "+
			"digests to %s", ErrInvalidReceipt, shortDigest(bound.budgetBeforeSHA256), shortDigest(budgetDigest))
	}
	return nil
}

// requireBindingAgreesWithBudgetAlone is the non-exposure operation's whole
// pre-state rule.
//
// There is no ledger to reproduce against and no charge to agree with, so what
// remains is the row budget: the visible statement was rendered with a limit,
// and the budget the receipt signs is the only thing that authorized it. A limit
// above what the budget left is a limit nothing authorized, which is the same
// failure the exposure path catches through its ledger -- reached here through
// the one pre-state this operation actually read.
func (r QueryReceiptV1) requireBindingAgreesWithBudgetAlone(bound boundExecution) error {
	if err := r.requireBindingNamesSignedBudget(bound); err != nil {
		return err
	}
	remaining, err := remainingBudgetRows(r.BudgetBefore)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	if bound.visibleRowLimit > remaining {
		return fmt.Errorf("%w: the visible row limit is %d but the signed budget pre-state leaves %d rows",
			ErrInvalidReceipt, bound.visibleRowLimit, remaining)
	}
	// The companion members are already refused by the binding's own shape rule;
	// restating them as numbers closes the case where a future member carries the
	// pair without carrying the target.
	if bound.hasCompanion || bound.companionEvidenceRows != 0 {
		return fmt.Errorf("%w: an operation that accounts no exposure has no provenance half, "+
			"but the binding carries %d companion evidence rows", ErrInvalidReceipt, bound.companionEvidenceRows)
	}
	return nil
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
	if err := r.requireBindingNamesSignedBudget(bound); err != nil {
		return err
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
	//
	// This used to be reached only when exposure evidence happened to be present,
	// which meant the strongest cross-check in the file was skipped for exactly
	// the receipt that carried a pre-state and no charge. requireExposureEvidenceShape
	// has now refused that receipt outright, so the charge is here and the check
	// is unconditional.
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
	remaining, err := remainingBudgetRows(budget)
	if err != nil {
		return err
	}
	if ledger.RemainingRows != remaining {
		return fmt.Errorf("the ledger pre-state claims %d remaining rows but budget_before leaves %d "+
			"(limits %d - used %d - reserved %d)", ledger.RemainingRows, remaining,
			budget.Limits.Rows, budget.Used.Rows, budget.Reserved.Rows)
	}
	return nil
}

// remainingBudgetRows is what the signed budget pre-state left, and the only
// place that arithmetic is written down.
//
// It is fail-closed by construction: a budget whose used plus reserved exceeds
// its limit yields a negative remainder, which is rejected here rather than
// silently clamped to zero, because clamping would turn an incoherent budget
// into a well-formed one and hand the operation a row limit of zero it would
// then appear to have honoured.
func remainingBudgetRows(budget BudgetStateV1) (int64, error) {
	if budget.Limits.Rows < 0 || budget.Used.Rows < 0 || budget.Reserved.Rows < 0 {
		return 0, fmt.Errorf("budget_before carries a negative row dimension (limits %d, used %d, reserved %d)",
			budget.Limits.Rows, budget.Used.Rows, budget.Reserved.Rows)
	}
	remaining := budget.Limits.Rows - budget.Used.Rows - budget.Reserved.Rows
	if remaining < 0 {
		return 0, fmt.Errorf("budget_before uses and reserves %d of %d rows, so it leaves %d remaining",
			budget.Used.Rows+budget.Reserved.Rows, budget.Limits.Rows, remaining)
	}
	return remaining, nil
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
