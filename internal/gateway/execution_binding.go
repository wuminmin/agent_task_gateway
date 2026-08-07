package gateway

import (
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// exposureLedgerBefore seals the pre-state the limits were derived from.
//
// The V5 ledger accounts predicate atoms and the composite outcome inside its
// outcome dimension rather than as separate budgets, so those two members of the
// fact vector are zero here. They are carried anyway: the vector has a fixed
// shape so a profile that later separates them cannot silently change what a
// digest covered.
func exposureLedgerBefore(ledger control.ExposureLedgerSnapshot, budgetBefore control.BudgetSnapshot,
	state physicalquery.LedgerPreState) (querybinding.ExposureLedgerBeforeV1, error) {
	limits := querybinding.FactVector{
		ReleaseFacts: ledger.Limits.ReleaseFacts, InfluenceFacts: ledger.Limits.InfluenceFacts,
		OutcomeFacts: ledger.Limits.OutcomeFacts,
	}
	used := querybinding.FactVector{
		ReleaseFacts: ledger.Used.ReleaseFacts, InfluenceFacts: ledger.Used.InfluenceFacts,
		OutcomeFacts: ledger.Used.OutcomeFacts,
	}
	remaining := querybinding.FactVector{
		ReleaseFacts:   limits.ReleaseFacts - used.ReleaseFacts,
		InfluenceFacts: limits.InfluenceFacts - used.InfluenceFacts,
		OutcomeFacts:   limits.OutcomeFacts - used.OutcomeFacts,
	}
	// The row budget is not restated: it is recomputed from the same budget
	// snapshot the receipt signs, so the two cannot disagree.
	remainingRows := budgetBefore.Limits.Rows - budgetBefore.Usage.UsedRows - budgetBefore.Usage.ReservedRows
	if remainingRows != state.RemainingRows {
		return querybinding.ExposureLedgerBeforeV1{}, fmt.Errorf(
			"the budget pre-state leaves %d rows but the derivation used %d", remainingRows, state.RemainingRows)
	}
	return querybinding.ExposureLedgerBeforeV1{
		ProfileVersion: ledger.ProfileVersion, RootTaskID: ledger.RootTaskID, RootEpoch: ledger.RootEpoch,
		Limits: limits, Used: used, Remaining: remaining, RemainingRows: remainingRows,
		UsesExpandedEvidence: state.UsesExpandedEvidence, HasExposureContext: state.HasExposureContext,
	}.Seal()
}

// preparedExecution is the compile-time half of one Query Execution Binding V2.
//
// It is three values, where V1 needed a struct of eleven. That is the point of
// the version: V1 had to restate the plan, the compiler, the dictionary set, the
// sidecar grants, the Catalog, the datasource and the schema beside a digest of
// a preparation nobody could check, so each of them was a second place the same
// fact was written down. V2 carries the sealed preparation itself, and every one
// of those members is inside it -- or, for the datasource and schema, a
// top-level member of the receipt that signs it.
type preparedExecution struct {
	// binding is the sealed preparation, exactly as physicalquery produced it.
	binding preparedbinding.PreparedOperationBindingV1
	// compiler is the identity binding.CompilerIdentitySHA256 names. V2 requires
	// the two to agree, and it is what the target records' renderer members are
	// checked against.
	compiler preparedbinding.CompilerIdentityV1
	// profile is the exposure profile the row limits were derived under. It
	// belongs to the ledger rather than to the preparation, which is why the
	// preparation does not carry it.
	profile string
}

// buildQueryExecutionBinding assembles what the Gateway executed.
//
// executed says whether the Connector was invoked. It is a parameter rather
// than something inferred from the presence of a decision, because a semantic
// replay authorizes both targets in order to derive its key and then executes
// neither; inferring would make that path indistinguishable from a novel one.
func buildQueryExecutionBinding(queryID string, path querybinding.PathKind, operation preparedExecution,
	query derivedQuery, ledger querybinding.ExposureLedgerBeforeV1, budgetBefore control.BudgetSnapshot,
	executed bool) (control.QueryExecutionBinding, error) {
	budgetDigest, err := queryreceipt.BudgetStateSHA256(queryReceiptBudget(budgetBefore))
	if err != nil {
		return control.QueryExecutionBinding{}, fmt.Errorf("canonicalize budget pre-state: %w", err)
	}
	// The prepared target digests are asked of the preparation rather than
	// recomputed here. V2's own Validate asks the same question of the same
	// value, so a Gateway that computed them a second way would be checked
	// against itself.
	visibleTarget, err := operation.binding.TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		return control.QueryExecutionBinding{}, err
	}
	visible, err := targetRecord(querybinding.RoleVisible, operation, visibleTarget,
		query.derivation.Visible, query.visible, executed)
	if err != nil {
		return control.QueryExecutionBinding{}, err
	}
	binding := querybinding.QueryExecutionBindingV2{
		PathKind:                   path,
		PreparedOperation:          operation.binding,
		Compiler:                   operation.compiler,
		ExposureProfileVersion:     operation.profile,
		VisibleRowLimit:            query.derivation.Limits.VisibleRowLimit,
		BudgetBeforeSHA256:         budgetDigest,
		ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible:                    visible,
	}
	if query.companion != nil {
		if query.derivation.Companion == nil {
			return control.QueryExecutionBinding{}, errors.New("a companion statement executed with no derived identity")
		}
		companionTarget, targetErr := operation.binding.TargetSHA256(preparedbinding.RoleCompanion)
		if targetErr != nil {
			return control.QueryExecutionBinding{}, targetErr
		}
		companion, companionErr := targetRecord(querybinding.RoleCompanion, operation, companionTarget,
			*query.derivation.Companion, *query.companion, executed)
		if companionErr != nil {
			return control.QueryExecutionBinding{}, companionErr
		}
		binding.Companion = &companion
		binding.CompanionEvidenceRows = query.derivation.Limits.CompanionEvidenceRows
		binding.CompanionPolicyRows = query.derivation.Limits.CompanionPolicyRows
	}
	sealed, err := binding.Seal()
	if err != nil {
		return control.QueryExecutionBinding{}, err
	}
	result := control.QueryExecutionBinding{
		QueryID: queryID, BindingV2: &sealed, ExposureLedgerBefore: ledger,
	}
	if err := result.Validate(); err != nil {
		return control.QueryExecutionBinding{}, err
	}
	return result, nil
}

func targetRecord(role querybinding.TargetRole, operation preparedExecution, preparedTargetSHA string,
	identity physicalquery.StatementIdentity, decision sqlpolicy.Decision, executed bool) (querybinding.TargetRecordV1, error) {
	if identity.StrictASTSHA256 == "" {
		// The digester is never nil on this path, so an empty structural digest
		// means the statement has no identity in the space the observer classifies
		// on. Signing it would produce a binding nothing could ever match.
		return querybinding.TargetRecordV1{}, fmt.Errorf("the %s statement has no strict AST identity", role)
	}
	if identity.ExactSHA256 != physicalquery.ExactDigest(decision.SQL) {
		return querybinding.TargetRecordV1{}, fmt.Errorf(
			"the %s statement identity does not digest the decision handed to the Connector", role)
	}
	return querybinding.TargetRecordV1{
		Role: role, Authorized: true, Executed: executed,
		ExactSQLSHA256: identity.ExactSHA256, StrictASTSHA256: identity.StrictASTSHA256,
		RowLimit: decision.RowLimit, PolicyFingerprint: decision.Fingerprint,
		PolicyRendererVersion:       operation.compiler.PolicyRendererVersion,
		PolicyRendererDigest:        operation.compiler.PolicyRendererSHA256,
		PreparedTargetBindingSHA256: preparedTargetSHA,
	}, nil
}

// prepareExecutionBinding resolves the compile-time identities and seals the
// pre-state, before anything executes.
//
// It returns an error rather than a zero value when the operation has no
// exposure context: a plain query has no plan digest, no exposure profile and no
// ledger, so it has nothing a Query Execution Binding could describe. Callers
// check for an exposure context first.
//
// Since T1d it resolves nothing about the preparation. The sealed binding it
// carries is the one physicalquery produced and the Gateway executed from, so
// the finalizer compares against a document rather than against a digest of one
// it was never handed.
func (s *Service) prepareExecutionBinding(exposureContext *planExposureContext,
	state physicalquery.LedgerPreState, ledger control.ExposureLedgerSnapshot,
	budgetBefore control.BudgetSnapshot) (preparedExecution, querybinding.ExposureLedgerBeforeV1, error) {
	if exposureContext == nil {
		return preparedExecution{}, querybinding.ExposureLedgerBeforeV1{}, nil
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return preparedExecution{}, querybinding.ExposureLedgerBeforeV1{}, err
	}
	operation := preparedExecution{
		binding: exposureContext.prepared.Binding(), compiler: compiler, profile: ledger.ProfileVersion,
	}
	// The preparation must have been compiled by this binary. It was -- Prepare
	// seals the local identity and preparePlan already required it -- so this is
	// the statement of that fact at the point where the two are signed together,
	// not a second chance to notice.
	if operation.binding.CompilerIdentitySHA256 != compiler.SHA256 {
		return preparedExecution{}, querybinding.ExposureLedgerBeforeV1{}, fmt.Errorf(
			"this operation was prepared by compiler %s but this binary is %s",
			operation.binding.CompilerIdentitySHA256[:12], compiler.SHA256[:12])
	}
	sealed, err := exposureLedgerBefore(ledger, budgetBefore, state)
	if err != nil {
		return preparedExecution{}, querybinding.ExposureLedgerBeforeV1{}, err
	}
	return operation, sealed, nil
}

// executionBindingApplies reports whether this operation can produce a Query
// Execution Binding at all.
//
// The gate is what the receipt contract permits, not a policy choice. Under V9
// it also required result artifacts to be enabled, because V9 is V8 plus the
// execution evidence and V8 requires a completed artifact intent -- so an inline
// delivery could carry no execution binding at all. V10 states the delivery mode
// instead of requiring an artifact, which is what removes that condition: an
// inline V5 execution now describes itself as one rather than going undescribed.
//
// Exposure profiles V1--V4 keep their existing receipt versions untouched.
func (s *Service) executionBindingApplies(exposureContext *planExposureContext,
	ledger control.ExposureLedgerSnapshot) bool {
	return exposureContext != nil &&
		ledger.ProfileVersion == exposure.ProfileV5 &&
		exposureContext.planDigest != ""
}
