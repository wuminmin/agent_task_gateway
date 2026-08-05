package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// The domains below separate three digests that would otherwise be computed
// over overlapping material. A prepared target and the operation that contains
// it must never collide, or a target could be presented as its own operation.
const (
	preparedOperationDomain = "TASKGATE-PREPARED-OPERATION-BINDING-V1"
	preparedTargetDomain    = "TASKGATE-PREPARED-TARGET-BINDING-V1"
	sidecarGrantsDomain     = "TASKGATE-ORDINAL-SIDECAR-GRANTS-V1"
)

// preparedOperation is everything fixed before any row limit was known.
//
// It is the compiler's and the Catalog's half of the execution identity: which
// plan, compiled by which compiler, against which schema, dictionary and
// sidecar grants. The runtime half -- the limits, the rendered bytes, what
// actually ran -- lives in the target records.
//
// Every field here is read from a real source. None of them is a constant
// chosen to make a SHA-256 check pass; a binding whose required digests were
// filled with arbitrary values would validate and mean nothing.
type preparedOperation struct {
	PlanSHA256             string
	ExposureProfileVersion string
	GrantDigest            string
	ManifestDigest         string
	CatalogSHA256          string
	DatasourceID           string
	SchemaDigest           string
	ViewBindingDigest      string
	// OrdinalDictionarySetSHA256 and SidecarGrantsSHA256 are empty for an
	// exposure operation with no ordinal sidecar.
	OrdinalDictionarySetSHA256 string
	SidecarGrantsSHA256        string
	// VisibleSQL and CompanionSQL are the COMPILED statements, before sqlpolicy
	// rendered a row limit into them. They are digested, never stored.
	VisibleSQL   string
	CompanionSQL string

	compilerVersion    string
	compilerSHA256     string
	rendererVersion    string
	rendererSHA256     string
	visibleTargetSHA   string
	companionTargetSHA string
}

// newPreparedOperation resolves the compiler and renderer identities and
// computes the prepared-target digests.
func newPreparedOperation(base preparedOperation) (preparedOperation, error) {
	compilerSHA, err := queryplan.CompilerSHA256()
	if err != nil {
		return preparedOperation{}, fmt.Errorf("compiler identity: %w", err)
	}
	rendererSHA, err := sqlpolicy.RendererSHA256()
	if err != nil {
		return preparedOperation{}, fmt.Errorf("renderer identity: %w", err)
	}
	base.compilerVersion = queryplan.CompilerVersion
	base.compilerSHA256 = compilerSHA
	base.rendererVersion = sqlpolicy.RendererVersion
	base.rendererSHA256 = rendererSHA
	if strings.TrimSpace(base.VisibleSQL) == "" {
		return preparedOperation{}, errors.New("a prepared operation has no visible statement")
	}
	base.visibleTargetSHA = base.preparedTargetDigest(querybinding.RoleVisible, base.VisibleSQL)
	if base.CompanionSQL != "" {
		base.companionTargetSHA = base.preparedTargetDigest(querybinding.RoleCompanion, base.CompanionSQL)
	}
	return base, nil
}

// preparedTargetDigest identifies one compiled statement independently of the
// row limit it is later rendered with.
//
// The limit is deliberately excluded: the same prepared target is rendered with
// different limits as the budget moves, and a target binding that changed with
// the limit could not tie two executions to one compiled statement. The limit is
// covered by the target record's exact digest instead.
func (operation preparedOperation) preparedTargetDigest(role querybinding.TargetRole, preparedSQL string) string {
	hash := sha256.New()
	writeBindingField(hash, "domain", preparedTargetDomain)
	writeBindingField(hash, "role", string(role))
	writeBindingField(hash, "plan_sha256", operation.PlanSHA256)
	writeBindingField(hash, "compiler_version", operation.compilerVersion)
	writeBindingField(hash, "compiler_sha256", operation.compilerSHA256)
	writeBindingField(hash, "renderer_version", operation.rendererVersion)
	writeBindingField(hash, "renderer_sha256", operation.rendererSHA256)
	writeBindingField(hash, "prepared_sql_sha256", physicalquery.ExactDigest(preparedSQL))
	return hex.EncodeToString(hash.Sum(nil))
}

// digest is the operation-level identity the whole binding names.
func (operation preparedOperation) digest() string {
	hash := sha256.New()
	writeBindingField(hash, "domain", preparedOperationDomain)
	writeBindingField(hash, "plan_sha256", operation.PlanSHA256)
	writeBindingField(hash, "compiler_version", operation.compilerVersion)
	writeBindingField(hash, "compiler_sha256", operation.compilerSHA256)
	writeBindingField(hash, "renderer_version", operation.rendererVersion)
	writeBindingField(hash, "renderer_sha256", operation.rendererSHA256)
	writeBindingField(hash, "exposure_profile_version", operation.ExposureProfileVersion)
	writeBindingField(hash, "grant_digest", operation.GrantDigest)
	writeBindingField(hash, "manifest_digest", operation.ManifestDigest)
	writeBindingField(hash, "catalog_sha256", operation.CatalogSHA256)
	writeBindingField(hash, "datasource_id", operation.DatasourceID)
	writeBindingField(hash, "schema_digest", operation.SchemaDigest)
	writeBindingField(hash, "view_binding_digest", operation.ViewBindingDigest)
	writeBindingField(hash, "ordinal_dictionary_set_sha256", operation.OrdinalDictionarySetSHA256)
	writeBindingField(hash, "sidecar_grants_sha256", operation.SidecarGrantsSHA256)
	writeBindingField(hash, "visible_prepared_target_sha256", operation.visibleTargetSHA)
	writeBindingField(hash, "companion_prepared_target_sha256", operation.companionTargetSHA)
	return hex.EncodeToString(hash.Sum(nil))
}

// sidecarGrantsDigest identifies the least-privilege sidecar grants an ordinal
// execution was extended with.
//
// They widen what the policy engine admits, so an execution binding that did not
// name them could not distinguish a statement authorized against the task's own
// products from one authorized against a wider set.
func sidecarGrantsDigest(grants []sqlpolicy.ProductGrant) string {
	ordered := append([]sqlpolicy.ProductGrant(nil), grants...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].LogicalName < ordered[right].LogicalName
	})
	hash := sha256.New()
	writeBindingField(hash, "domain", sidecarGrantsDomain)
	writeBindingField(hash, "count", fmt.Sprintf("%d", len(ordered)))
	for _, grant := range ordered {
		writeBindingField(hash, "logical_name", grant.LogicalName)
		writeBindingField(hash, "physical_schema", grant.PhysicalSchema)
		writeBindingField(hash, "physical_view", grant.PhysicalView)
		writeBindingField(hash, "approved_columns", strings.Join(sortedCopy(grant.ApprovedColumns), "\x1f"))
		writeBindingField(hash, "allowed_functions", strings.Join(sortedCopy(grant.AllowedFunctions), "\x1f"))
		writeBindingField(hash, "allowed_aggregates", strings.Join(sortedCopy(grant.AllowedAggregates), "\x1f"))
		writeBindingField(hash, "allowed_operators", strings.Join(sortedCopy(grant.AllowedOperators), "\x1f"))
		scope := append([]sqlpolicy.ScopePredicate(nil), grant.MandatoryScope...)
		sort.SliceStable(scope, func(left, right int) bool {
			if scope[left].Column == scope[right].Column {
				return scope[left].Operator < scope[right].Operator
			}
			return scope[left].Column < scope[right].Column
		})
		writeBindingField(hash, "scope_count", fmt.Sprintf("%d", len(scope)))
		for _, predicate := range scope {
			writeBindingField(hash, "scope_column", predicate.Column)
			writeBindingField(hash, "scope_operator", string(predicate.Operator))
			writeBindingField(hash, "scope_values", strings.Join(predicate.Values, "\x1f"))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func writeBindingField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}

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

// buildQueryExecutionBinding assembles what the Gateway executed.
//
// executed says whether the Connector was invoked. It is a parameter rather
// than something inferred from the presence of a decision, because a semantic
// replay authorizes both targets in order to derive its key and then executes
// neither; inferring would make that path indistinguishable from a novel one.
func buildQueryExecutionBinding(queryID string, path querybinding.PathKind, operation preparedOperation,
	query derivedQuery, ledger querybinding.ExposureLedgerBeforeV1, budgetBefore control.BudgetSnapshot,
	executed bool) (control.QueryExecutionBinding, error) {
	budgetDigest, err := queryreceipt.BudgetStateSHA256(queryReceiptBudget(budgetBefore))
	if err != nil {
		return control.QueryExecutionBinding{}, fmt.Errorf("canonicalize budget pre-state: %w", err)
	}
	visible, err := targetRecord(querybinding.RoleVisible, operation, operation.visibleTargetSHA,
		query.derivation.Visible, query.visible, executed)
	if err != nil {
		return control.QueryExecutionBinding{}, err
	}
	binding := querybinding.QueryExecutionBindingV1{
		PathKind:                       path,
		PreparedOperationBindingSHA256: operation.digest(),
		ExposureProfileVersion:         operation.ExposureProfileVersion,
		UsesExpandedEvidence:           ledger.UsesExpandedEvidence,
		VisibleRowLimit:                query.derivation.Limits.VisibleRowLimit,
		BudgetBeforeSHA256:             budgetDigest,
		ExposureLedgerBeforeSHA256:     ledger.SHA256,
		PlanSHA256:                     operation.PlanSHA256,
		CompilerVersion:                operation.compilerVersion,
		CompilerSHA256:                 operation.compilerSHA256,
		OrdinalDictionarySetSHA256:     operation.OrdinalDictionarySetSHA256,
		SidecarGrantsSHA256:            operation.SidecarGrantsSHA256,
		Visible:                        visible,
	}
	if query.companion != nil {
		if query.derivation.Companion == nil {
			return control.QueryExecutionBinding{}, errors.New("a companion statement executed with no derived identity")
		}
		companion, companionErr := targetRecord(querybinding.RoleCompanion, operation, operation.companionTargetSHA,
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
		QueryID: queryID, Binding: sealed, ExposureLedgerBefore: ledger,
	}
	if err := result.Validate(); err != nil {
		return control.QueryExecutionBinding{}, err
	}
	return result, nil
}

func targetRecord(role querybinding.TargetRole, operation preparedOperation, preparedTargetSHA string,
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
		PolicyRendererVersion: operation.rendererVersion, PolicyRendererDigest: operation.rendererSHA256,
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
func (s *Service) prepareExecutionBinding(exposureContext *planExposureContext,
	state physicalquery.LedgerPreState, ledger control.ExposureLedgerSnapshot, budgetBefore control.BudgetSnapshot,
	visibleSQL, companionSQL string, protocolGrant domain.TaskGrantV1, grantDigest string,
	evidence datasourceEvidence) (preparedOperation, querybinding.ExposureLedgerBeforeV1, error) {
	if exposureContext == nil {
		return preparedOperation{}, querybinding.ExposureLedgerBeforeV1{}, nil
	}
	base := preparedOperation{
		PlanSHA256:             exposureContext.planDigest,
		ExposureProfileVersion: ledger.ProfileVersion,
		GrantDigest:            grantDigest,
		ManifestDigest:         protocolGrant.Core.ManifestDigest,
		CatalogSHA256:          s.catalog.SHA256,
		DatasourceID:           evidence.DatasourceID,
		SchemaDigest:           evidence.SchemaDigest,
		ViewBindingDigest:      protocolGrant.Core.ViewBindingDigest,
		VisibleSQL:             visibleSQL,
		CompanionSQL:           companionSQL,
	}
	if exposureContext.ordinal != nil {
		base.OrdinalDictionarySetSHA256 = exposureContext.ordinal.DictionarySetDigest
		base.SidecarGrantsSHA256 = sidecarGrantsDigest(exposureContext.ordinal.SidecarGrants)
	}
	operation, err := newPreparedOperation(base)
	if err != nil {
		return preparedOperation{}, querybinding.ExposureLedgerBeforeV1{}, err
	}
	sealed, err := exposureLedgerBefore(ledger, budgetBefore, state)
	if err != nil {
		return preparedOperation{}, querybinding.ExposureLedgerBeforeV1{}, err
	}
	return operation, sealed, nil
}

// executionBindingApplies reports whether this operation can produce a Query
// Execution Binding at all.
//
// The gate is not a policy choice; it is what the receipt contract permits. V9
// is V8 plus the execution evidence, and ValidateUnsigned requires V8's
// completed artifact intent for both. So a binding is only ever signable for an
// exposure-V5 operation running with result artifacts enabled. Building one
// anywhere else would persist a description no receipt could carry, and -- worse
// -- would trip the fail-closed check that refuses to emit a pre-V9 receipt for
// a query that has a binding.
//
// Exposure profiles V1--V4 keep their existing receipt versions untouched.
func (s *Service) executionBindingApplies(exposureContext *planExposureContext,
	ledger control.ExposureLedgerSnapshot) bool {
	return exposureContext != nil &&
		ledger.ProfileVersion == exposure.ProfileV5 &&
		exposureContext.planDigest != "" &&
		s.resultArtifacts != nil
}
