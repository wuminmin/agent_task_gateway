package queryreceipt

import "fmt"

// validateExecutionBindingV2 is V10's rule set.
//
// It is deliberately thin. Everything the receipt requires of an execution
// binding -- that it names the pre-states carried beside it, that its limits
// reproduce from them, that the ledger identity agrees with the exposure charge
// -- is the same for V9 and V10 and lives in one place. What V10 adds is the
// document V2 carries and V1 did not, and the two cross-checks that document
// makes possible.
func (r QueryReceiptV1) validateExecutionBindingV2() error {
	if r.ExecutionBindingV2 == nil {
		return fmt.Errorf("%w: V10 requires the signed query execution binding V2", ErrInvalidReceipt)
	}
	binding := *r.ExecutionBindingV2
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("%w: query execution binding v2: %v", ErrInvalidReceipt, err)
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
	return r.requireBindingAgreesWithSignedPreState(boundExecution{
		exposureProfileVersion:     binding.ExposureProfileVersion,
		usesExpandedEvidence:       binding.UsesExpandedEvidence(),
		exposureLedgerBeforeSHA256: binding.ExposureLedgerBeforeSHA256,
		budgetBeforeSHA256:         binding.BudgetBeforeSHA256,
		visibleRowLimit:            binding.VisibleRowLimit,
		companionEvidenceRows:      binding.CompanionEvidenceRows,
		hasCompanion:               binding.Companion != nil,
	})
}
