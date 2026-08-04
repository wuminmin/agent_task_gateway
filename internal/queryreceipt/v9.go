package queryreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

// validateExecutionBinding enforces the V9 rules.
//
// V8 semantics are untouched. The only thing said about V8 here is that it must
// not carry V9 material: a receipt whose signature does not cover a field must
// not be allowed to carry that field, or a holder could attach an execution
// binding to a V8 receipt and present it as signed.
func (r QueryReceiptV1) validateExecutionBinding() error {
	if r.Version != VersionV9 {
		if r.ExecutionBinding != nil || r.ExposureLedgerBefore != nil {
			return fmt.Errorf("%w: a V%s receipt carries V9 execution evidence its signature does not cover",
				ErrInvalidReceipt, r.Version)
		}
		return nil
	}
	if r.ExposureLedgerBefore == nil {
		return fmt.Errorf("%w: V9 requires the signed exposure ledger pre-state", ErrInvalidReceipt)
	}
	if err := r.ExposureLedgerBefore.Validate(); err != nil {
		return fmt.Errorf("%w: exposure ledger pre-state: %v", ErrInvalidReceipt, err)
	}
	if r.ExecutionBinding == nil {
		return fmt.Errorf("%w: V9 requires the signed query execution binding", ErrInvalidReceipt)
	}
	binding := *r.ExecutionBinding
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("%w: query execution binding: %v", ErrInvalidReceipt, err)
	}
	// The binding must name the pre-state carried beside it. Without this the
	// receipt could sign a binding derived from one ledger and a pre-state from
	// another, and the limits would reproduce against neither.
	if binding.ExposureLedgerBeforeSHA256 != r.ExposureLedgerBefore.SHA256 {
		return fmt.Errorf("%w: the execution binding names exposure pre-state %s but the receipt carries %s",
			ErrInvalidReceipt, shortDigest(binding.ExposureLedgerBeforeSHA256),
			shortDigest(r.ExposureLedgerBefore.SHA256))
	}
	// Likewise for the budget: budget_before is already signed on the receipt, so
	// the binding must point at that exact state rather than at some other.
	budgetDigest, err := BudgetStateSHA256(r.BudgetBefore)
	if err != nil {
		return fmt.Errorf("%w: cannot canonicalize budget_before: %v", ErrInvalidReceipt, err)
	}
	if binding.BudgetBeforeSHA256 != budgetDigest {
		return fmt.Errorf("%w: the execution binding names budget pre-state %s but the receipt's budget_before "+
			"digests to %s", ErrInvalidReceipt, shortDigest(binding.BudgetBeforeSHA256), shortDigest(budgetDigest))
	}
	// The exposure profile the limits were derived under must be the profile the
	// ledger belongs to, and the expanded-evidence flag must agree: they are the
	// two inputs the companion's row limit branches on.
	if binding.ExposureProfileVersion != r.ExposureLedgerBefore.ProfileVersion {
		return fmt.Errorf("%w: the execution binding derives under profile %q but the pre-state is %q",
			ErrInvalidReceipt, binding.ExposureProfileVersion, r.ExposureLedgerBefore.ProfileVersion)
	}
	if binding.UsesExpandedEvidence != r.ExposureLedgerBefore.UsesExpandedEvidence {
		return fmt.Errorf("%w: the execution binding and its pre-state disagree about expanded evidence",
			ErrInvalidReceipt)
	}
	// The row limits must be reproducible from the signed pre-state. This is the
	// check that makes the binding evidence rather than assertion: a limit the
	// pre-state cannot derive is a limit nothing authorized.
	if err := requireReproducibleLimits(binding, *r.ExposureLedgerBefore); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	if r.Exposure != nil && r.Exposure.RootTaskID != r.ExposureLedgerBefore.RootTaskID {
		return fmt.Errorf("%w: the exposure evidence and the ledger pre-state name different root tasks",
			ErrInvalidReceipt)
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
func requireReproducibleLimits(binding querybinding.QueryExecutionBindingV1,
	ledger querybinding.ExposureLedgerBeforeV1) error {
	if binding.Companion == nil {
		return nil
	}
	// The authorized visible limit is what production feeds the companion, and
	// the policy engine may have lowered it below the requested one. So the
	// requested limit bounds it rather than equalling it.
	requested := ledger.RemainingRows
	if ledger.HasExposureContext && !ledger.UsesExpandedEvidence {
		requested = min64(requested, ledger.Limits.InfluenceFacts)
	}
	if binding.VisibleRowLimit > requested {
		return fmt.Errorf("the visible row limit is %d but the signed pre-state authorizes at most %d",
			binding.VisibleRowLimit, requested)
	}
	wantEvidenceRows := binding.VisibleRowLimit
	if ledger.UsesExpandedEvidence {
		wantEvidenceRows = ledger.Limits.InfluenceFacts
	}
	if binding.CompanionEvidenceRows != wantEvidenceRows {
		return fmt.Errorf("companion evidence rows are %d but the signed pre-state derives %d",
			binding.CompanionEvidenceRows, wantEvidenceRows)
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
