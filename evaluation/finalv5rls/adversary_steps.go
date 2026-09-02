package finalv5rls

import (
	"fmt"
	"strconv"
)

// Exported step constructors for the P9.C optimizing-adversary corpus
// (evaluation/finalv5adversary). They are thin wrappers over the frozen
// private constructors so the adversary corpus shares this package's exact
// fact encodings instead of re-implementing them (single-source rule); the
// embedded RLS corpus bytes and CorpusSHA256 are untouched.

// AdversaryCountProbeStep is a count(*) threshold probe
// (SELECT count(*) WHERE amount >= threshold) with the oracle observation the
// independent oracle assigns it: one release aggregate fact per distinct
// threshold, dept+amount dependency cells of the matching sales rows, one
// predicate atom, and one composite.
func (manifest Manifest) AdversaryCountProbeStep(id string, threshold int64) (Step, error) {
	if err := manifest.Validate(); err != nil {
		return Step{}, err
	}
	step := manifest.countStep("adversary_probe", "count-threshold", id, threshold, Decision{})
	step.ExpectedSHA256 = ResultSHA256(step.ExpectedRows)
	return step, nil
}

// AdversaryListingStep lists the sales rows matching amount >= threshold
// (equivalent-predicate family: releases the matching receipt_no cells and
// depends on dept+receipt_no+amount evidence cells per matching row).
func (manifest Manifest) AdversaryListingStep(id string, threshold int64) (Step, error) {
	if err := manifest.Validate(); err != nil {
		return Step{}, err
	}
	rows := manifest.amountRows(threshold)
	atom := outcomeAtom("amount", ">=", strconv.FormatInt(threshold, 10))
	step := manifest.rowStep("equivalent_predicate", "adversary-listing", id,
		fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail WHERE amount >= %d ORDER BY receipt_no ASC", threshold),
		rows, []string{atom}, fmt.Sprintf("amount-ge|%d", threshold))
	step.ExpectedSHA256 = ResultSHA256(step.ExpectedRows)
	return step, nil
}

// AdversarySalesRows exposes the frozen policy-visible fixture rows so the
// adversary corpus can derive its a-priori expectations (hidden target, row
// order by amount) from the same source of truth.
func (manifest Manifest) AdversarySalesRows() ([]FixtureRow, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	rows := manifest.salesRows()
	out := make([]FixtureRow, len(rows))
	copy(out, rows)
	return out, nil
}
