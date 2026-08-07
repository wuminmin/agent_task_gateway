package queryreceipt

import "fmt"

// validateExecutionBindingV2 is what the receipt requires of the execution
// binding a completed query carries.
//
// It is deliberately thin. Everything else -- that the binding names the
// pre-states carried beside it, that its limits reproduce from them, that the
// ledger identity agrees with the exposure charge -- lives in one place, beside
// the pre-state it is checked against. What is here is what the carried
// preparation makes possible and a digest would not.
func (r QueryReceiptV1) validateExecutionBindingV2() error {
	if r.ExecutionBindingV2 == nil {
		return fmt.Errorf("%w: a completed query requires the signed query execution binding",
			ErrInvalidReceipt)
	}
	binding := *r.ExecutionBindingV2
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("%w: query execution binding v2: %v", ErrInvalidReceipt, err)
	}
	// Which of the two shapes this operation is, read from the binding's own
	// signed member rather than from what the receipt happens to carry beside it.
	// The members that would otherwise be free to disagree are checked against it
	// next.
	accountsExposure := binding.ExposureProfileVersion != ""
	if err := r.requireExposureEvidenceShape(accountsExposure); err != nil {
		return err
	}
	// A semantic replay authorizes its targets and executes neither, so it settles
	// no rows. Every other path executed the visible statement, and the receipt
	// already signs the row count that came back. Requiring the count not to
	// exceed the limit the binding says was rendered is what stops a receipt from
	// reporting more rows than the statement could have returned.
	if r.RowCount > binding.VisibleRowLimit {
		return fmt.Errorf("%w: the receipt reports %d rows but the executed visible statement was rendered "+
			"with a row limit of %d", ErrInvalidReceipt, r.RowCount, binding.VisibleRowLimit)
	}
	bound := boundExecution{
		exposureProfileVersion:     binding.ExposureProfileVersion,
		usesExpandedEvidence:       binding.UsesExpandedEvidence(),
		exposureLedgerBeforeSHA256: binding.ExposureLedgerBeforeSHA256,
		budgetBeforeSHA256:         binding.BudgetBeforeSHA256,
		visibleRowLimit:            binding.VisibleRowLimit,
		companionEvidenceRows:      binding.CompanionEvidenceRows,
		hasCompanion:               binding.Companion != nil,
	}
	if !accountsExposure {
		return r.requireBindingAgreesWithBudgetAlone(bound)
	}
	if err := r.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("%w: exposure ledger pre-state: %v", ErrInvalidReceipt, err)
	}
	return r.requireBindingAgreesWithSignedPreState(bound)
}
