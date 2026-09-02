package finalv5rls

import (
	"fmt"
	"strconv"
	"strings"
)

// Exported step constructors for the P9.C optimizing-adversary corpus
// (evaluation/finalv5adversary). They are thin wrappers over the frozen
// private constructors so the adversary corpus shares this package's exact
// fact encodings instead of re-implementing them (single-source rule); the
// embedded RLS corpus bytes and CorpusSHA256 are untouched.

// AdversaryCountProbeStep is a count(*) threshold probe
// (SELECT count(*) WHERE amount >= threshold) with the oracle observation the
// independent oracle assigns it: dept+amount dependency cells of the matching
// sales rows, one predicate atom, one composite, and one release aggregate
// fact keyed by (value, witness rows) WITHOUT the threshold literal.
// Production derives the aggregate's Release identity from the normalized
// expression, typed value, and witness commitment (algebra_v2
// materializeCellReleaseV2); the caller's literal lives in the predicate atom
// and composite, never in the Release fact, so two thresholds with the same
// count over the same rows reuse one Release fact. pilot-adversary-01 proved
// this live (probe charges 1,0,1,1,0,0 across six thresholds). The original
// releaseAggregate keys on the threshold too; the frozen RLS trace never has
// a same-(value,rows) collision across thresholds, so both models coincide
// there and the sealed RLS corpus bytes stay untouched.
func (manifest Manifest) AdversaryCountProbeStep(id string, threshold int64) (Step, error) {
	if err := manifest.Validate(); err != nil {
		return Step{}, err
	}
	step := manifest.countStep("adversary_probe", "count-threshold", id, threshold, Decision{})
	rows := manifest.amountRows(threshold)
	step.Oracle.Release = []string{releaseAggregateValueIdentity(int64(len(rows)), rows)}
	step.Oracle = normalizeObservation(step.Oracle)
	step.ExpectedSHA256 = ResultSHA256(step.ExpectedRows)
	return step, nil
}

// releaseAggregateValueIdentity mirrors the production aggregate Release
// identity surface for the adversary corpus: expression + value + witness,
// no predicate literal.
func releaseAggregateValueIdentity(value int64, rows []FixtureRow) string {
	receipts := make([]string, len(rows))
	for index, row := range rows {
		receipts[index] = row.ReceiptNo
	}
	return domainDigest("release-count-value", fmt.Sprintf("%d\x00%s", value, strings.Join(receipts, ",")))
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
