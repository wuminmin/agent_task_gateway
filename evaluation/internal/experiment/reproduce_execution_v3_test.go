package experiment

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

// The pre-state every receipt in this file is sealed against, and the budget it
// has to agree with. The row limits are DERIVED from these rather than chosen:
// without expanded evidence the influence limit bounds the visible one, so the
// visible statement is rendered with 4 and the companion with the same.
const (
	reproRemainingRows  = int64(10)
	reproInfluenceFacts = int64(4)
)

// retainedSnapshotArtifacts is the published artifact directory for the
// Result-heavy publication, retained in the repository as evidence.
//
// A V5 operation compiles an ordinal program and therefore binds the
// Catalog-wide dictionary universe, so it cannot be prepared at all without the
// published artifacts. That is not a testing inconvenience: it is the property
// that makes the reproduction meaningful, because the artifacts are immutable
// and the Catalog pins their digests, so both sides are bound to one universe.
func retainedSnapshotArtifacts(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "final-v5-wsl2", "raw",
		"diagnosis-attestation-footprint-qualification-02-20260804T154235Z-818c481ebe5b",
		"snapshot-index-artifacts-full"))
	if err != nil {
		t.Fatalf("resolve retained snapshot artifacts: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the retained snapshot artifacts are not present: %v", err)
	}
	return path
}

// reproMaterial is one operation's frozen contract material: a real activated
// Profile Catalog, the retained publication artifacts, a plan, and the approved
// surface it runs under.
//
// It is exposure V5 because that is the profile the frozen workloads run and the
// hardest one to reproduce: it compiles an ordinal program, binds snapshot
// sidecars into the companion statement and derives a predicate footprint. A
// reproduction that holds here holds for the simpler profiles by construction.
func reproMaterial(t *testing.T) FrozenOperationMaterialV3 {
	t.Helper()
	return FrozenOperationMaterialV3{
		CatalogPath:         resultHeavyCatalogPath(t),
		SnapshotArtifactDir: retainedSnapshotArtifacts(t),
		Plan: queryplan.QueryPlan{
			Product: "final_v5_result_heavy", Columns: []string{"row_id", "category"},
		},
		Grant: physicalquery.Grant{
			ApprovedProducts: []string{"final_v5_result_heavy"},
			ApprovedColumns:  map[string][]string{"final_v5_result_heavy": {"row_id", "category"}},
			ExposureProfile:  "taskgate-exposure-v5",
			MandatoryScope:   []byte(`{"category":"alpha"}`),
		},
	}
}

// gatewayReceiptFor plays the Gateway: it prepares the operation from the same
// frozen material, authorizes the prepared statements against the pre-state it
// is about to sign, and seals a receipt describing exactly that.
//
// It deliberately does NOT reuse ReproduceExecutionV3. A test in which the
// document and the reproduction come from one function proves only that the
// function is deterministic. This builds the receipt the way production builds
// it -- prepare, derive, record what was sent -- so that the agreement the test
// asserts is between two separate assemblies of the same material.
func gatewayReceiptFor(t *testing.T, material FrozenOperationMaterialV3) queryreceipt.QueryReceiptV1 {
	t.Helper()
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*logicalCatalog)
	if err != nil {
		t.Fatalf("catalog view: %v", err)
	}
	inputs := physicalquery.PreparationInputs{Plan: material.Plan, Grant: material.Grant, Catalog: view}
	if material.Grant.UsesOrdinalProgram() {
		// The dictionary universe is read from the same retained artifacts the
		// finalizer reads. That is the Gateway's own acquisition in this test --
		// production resolves it through a live registry -- and it is the one
		// input the two sides genuinely share, because it is the immutable
		// published artifact both are supposed to be bound to.
		bindings, bindingErr := snapshotBindingsFromArtifactsV3(*logicalCatalog, material.SnapshotArtifactDir)
		if bindingErr != nil {
			t.Fatalf("snapshot bindings: %v", bindingErr)
		}
		inputs.SnapshotBindings = bindings
	}
	prepared, err := physicalquery.Prepare(inputs)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		t.Fatalf("compiler identity: %v", err)
	}
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("executable statements: %v", err)
	}
	state := physicalquery.LedgerPreState{
		RemainingRows: reproRemainingRows, InfluenceFacts: reproInfluenceFacts,
		UsesExpandedEvidence: prepared.Binding().UsesExpandedEvidence(), HasExposureContext: true,
	}
	derivation, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
			Grant: prepared.PolicyGrant(), State: state})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	receipt := reproReceiptBase(t)
	ledger, err := querybinding.ExposureLedgerBeforeV1{
		ProfileVersion: receipt.Exposure.ProfileVersion, RootTaskID: receipt.Exposure.RootTaskID,
		RootEpoch:     receipt.Exposure.RootEpoch,
		Limits:        querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: reproInfluenceFacts, OutcomeFacts: 10},
		Used:          querybinding.FactVector{},
		Remaining:     querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: reproInfluenceFacts, OutcomeFacts: 10},
		RemainingRows: reproRemainingRows, UsesExpandedEvidence: state.UsesExpandedEvidence,
		HasExposureContext: true,
	}.Seal()
	if err != nil {
		t.Fatalf("seal ledger: %v", err)
	}
	receipt.ExposureLedgerBefore = &ledger
	budgetDigest, err := queryreceipt.BudgetStateSHA256(receipt.BudgetBefore)
	if err != nil {
		t.Fatalf("budget digest: %v", err)
	}
	visibleTarget, err := prepared.Binding().TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		t.Fatalf("prepared visible target: %v", err)
	}
	binding := querybinding.QueryExecutionBindingV2{
		PathKind: querybinding.PathPairedNovel, PreparedOperation: prepared.Binding(), Compiler: compiler,
		ExposureProfileVersion: ledger.ProfileVersion,
		VisibleRowLimit:        derivation.Limits.VisibleRowLimit,
		CompanionEvidenceRows:  derivation.Limits.CompanionEvidenceRows,
		CompanionPolicyRows:    derivation.Limits.CompanionPolicyRows,
		BudgetBeforeSHA256:     budgetDigest, ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible: reproTargetRecord(querybinding.RoleVisible, derivation.Visible,
			derivation.VisibleDecision, compiler, visibleTarget),
	}
	if derivation.CompanionDecision != nil {
		companionTarget, targetErr := prepared.Binding().TargetSHA256(preparedbinding.RoleCompanion)
		if targetErr != nil {
			t.Fatalf("prepared companion target: %v", targetErr)
		}
		record := reproTargetRecord(querybinding.RoleCompanion, *derivation.Companion,
			*derivation.CompanionDecision, compiler, companionTarget)
		binding.Companion = &record
	}
	sealed, err := binding.Seal()
	if err != nil {
		t.Fatalf("seal execution binding: %v", err)
	}
	receipt.ExecutionBindingV2 = &sealed

	signer, err := queryreceipt.NewSigner("repro-key",
		ed25519.NewKeyFromSeed([]byte(strings.Repeat("r", ed25519.SeedSize))))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func reproTargetRecord(role querybinding.TargetRole, identity physicalquery.StatementIdentity,
	decision sqlpolicy.Decision, compiler preparedbinding.CompilerIdentityV1, preparedTarget string) querybinding.TargetRecordV1 {
	return querybinding.TargetRecordV1{
		Role: role, Authorized: true, Executed: true,
		ExactSQLSHA256: identity.ExactSHA256, StrictASTSHA256: identity.StrictASTSHA256,
		RowLimit: decision.RowLimit, PolicyFingerprint: decision.Fingerprint,
		PolicyRendererVersion:       compiler.PolicyRendererVersion,
		PolicyRendererDigest:        compiler.PolicyRendererSHA256,
		PreparedTargetBindingSHA256: preparedTarget,
	}
}

func reproReceiptBase(t *testing.T) queryreceipt.QueryReceiptV1 {
	t.Helper()
	common := fmt.Sprintf("%064x", 1)
	created := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	completed := created.Add(time.Millisecond)
	signedAt := completed.Add(time.Millisecond)
	budget := queryreceipt.BudgetStateV1{
		Limits: queryreceipt.BudgetVectorV1{Queries: 2, Rows: reproRemainingRows, DBMS: 100},
	}
	after := budget
	after.Used = queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2}
	return queryreceipt.QueryReceiptV1{
		Version: queryreceipt.Version, ReceiptID: "query-repro-1", TaskID: "task-repro-1",
		QueryID: "query-repro-1", RequestID: "request-repro-1",
		ManifestDigest: common, GrantDigest: common, CatalogDigest: common, CatalogVersion: "catalog-v1",
		DatasourceID: "taskgate-repro", SchemaDigest: common, RequestDigest: common,
		SQLFingerprint: "repro-fingerprint", PolicyDecision: "ALLOW",
		BudgetBefore:   budget,
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: 4, DBMS: 50},
		BudgetCharged:  queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		BudgetAfter:    after,
		RowCount:       1, DatabaseMS: 2, ResultHash: common,
		Status: queryreceipt.StatusCompleted, CreatedAt: created, CompletedAt: completed,
		AuditSequence: 7, PreviousAuditHash: common, AuditHash: common, SignedAt: &signedAt,
		GatewayKeyID:       "repro-key",
		ResultDeliveryMode: queryreceipt.DeliveryInline,
		Exposure: &queryreceipt.ExposureEvidenceV1{
			RootTaskID: "task-repro-1", ProfileVersion: "taskgate-exposure-v5",
			ActualReleaseFacts: 2, ActualInfluenceFacts: 2,
			ActualOutcomeFacts: 2, ChargedOutcomeFacts: 2,
			ChargedReleaseFacts: 2, ChargedInfluenceFacts: 2,
			ObservationSHA256: common, RootEpoch: 1,
			DictionarySetSHA256: common, ReleaseSetSHA256: common,
			InfluenceSetSHA256: common, OutcomeSetSHA256: common,
			PredicateProfileVersion: "taskgate-predicate-footprint-v1",
			PredicateContextSHA256:  common, PredicateSetSHA256: common,
			ActualPredicateAtomCount: 1, ChargedPredicateAtomCount: 1,
			CompositeOutcomeSHA256: common, ActualCompositeCount: 1, ChargedCompositeCount: 1,
		},
	}
}

// The whole of T1e in one assertion: a finalizer holding only frozen contract
// material reaches the statements the Gateway signed, without being told them.
func TestFinalizerReproducesTheSignedExecutionFromFrozenMaterial(t *testing.T) {
	material := reproMaterial(t)
	receipt := gatewayReceiptFor(t, material)

	reproduced, err := ReproduceExecutionV3(receipt, material)
	if err != nil {
		t.Fatalf("the finalizer could not reproduce the signed execution: %v", err)
	}
	if reproduced.VisibleSQL == "" || reproduced.CompanionSQL == "" {
		t.Fatal("the reproduction yielded no statements")
	}
	// The reproduced bytes are the signed bytes. This is what the string fields
	// on TrustedInputsV3 used to assert by comment.
	binding := receipt.ExecutionBindingV2
	if got := physicalquery.ExactDigest(reproduced.VisibleSQL); got != binding.Visible.ExactSQLSHA256 {
		t.Fatalf("the reproduced visible statement digests to %s, the Gateway signed %s",
			got, binding.Visible.ExactSQLSHA256)
	}
	if got := physicalquery.ExactDigest(reproduced.CompanionSQL); got != binding.Companion.ExactSQLSHA256 {
		t.Fatalf("the reproduced companion statement digests to %s, the Gateway signed %s",
			got, binding.Companion.ExactSQLSHA256)
	}
	// And the row limits, which are rendered into those bytes.
	if reproduced.Limits.VisibleRowLimit != binding.VisibleRowLimit ||
		reproduced.Limits.CompanionPolicyRows != binding.CompanionPolicyRows {
		t.Fatalf("reproduced limits %+v, signed visible=%d companion=%d",
			reproduced.Limits, binding.VisibleRowLimit, binding.CompanionPolicyRows)
	}
}

// Every input the reproduction depends on is load-bearing: change one, and the
// finalizer stops agreeing with the signature rather than quietly agreeing
// anyway.
//
// This is the test that would have failed for the whole life of the string
// fields, because there was no derivation for a changed input to change.
func TestReproductionRejectsMaterialTheGatewayDidNotPrepareFrom(t *testing.T) {
	honest := reproMaterial(t)
	receipt := gatewayReceiptFor(t, honest)

	for name, corrupt := range map[string]func(*FrozenOperationMaterialV3){
		"a different projection": func(m *FrozenOperationMaterialV3) {
			m.Plan.Columns = []string{"row_id"}
		},
		"a different ordering": func(m *FrozenOperationMaterialV3) {
			m.Plan.OrderBy = []queryplan.Order{{Column: "category", Direction: "desc"}}
		},
		"a wider approved surface": func(m *FrozenOperationMaterialV3) {
			m.Grant.ApprovedColumns = map[string][]string{
				"final_v5_result_heavy": {"row_id", "category", "amount"},
			}
		},
		"a different mandatory scope": func(m *FrozenOperationMaterialV3) {
			m.Grant.MandatoryScope = []byte(`{"category":"beta"}`)
		},
		"a different exposure profile": func(m *FrozenOperationMaterialV3) {
			m.Grant.ExposureProfile = "taskgate-exposure-v4"
		},
	} {
		t.Run(name, func(t *testing.T) {
			material := honest
			corrupt(&material)
			_, err := ReproduceExecutionV3(receipt, material)
			if err == nil {
				t.Fatalf("the finalizer reproduced the signed execution from %s", name)
			}
			t.Logf("%s -> %v", name, err)
		})
	}
}

// A receipt whose signed limits do not follow from its own pre-state through the
// authorizer is refused, even though its arithmetic reproduces.
//
// The receipt validator's own bound on the visible limit is an inequality --
// "no more than the pre-state authorizes" -- so a receipt can under-claim and
// still validate. The executed bytes carry the limit, so under-claiming is a
// different execution, and only running the authorizer catches it.
func TestReproductionRejectsALimitTheAuthorizerDoesNotDerive(t *testing.T) {
	material := reproMaterial(t)
	receipt := gatewayReceiptFor(t, material)
	document := *receipt.ExecutionBindingV2
	document.VisibleRowLimit--
	document.Visible.RowLimit = document.VisibleRowLimit
	document.CompanionEvidenceRows = document.VisibleRowLimit
	document.CompanionPolicyRows = document.VisibleRowLimit
	document.Companion.RowLimit = document.VisibleRowLimit
	resealed, err := document.Seal()
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	receipt.ExecutionBindingV2 = &resealed
	if _, err := ReproduceExecutionV3(receipt, material); err == nil {
		t.Fatal("the finalizer accepted a visible row limit the authorizer does not derive")
	}
}

// preparedOperationV3 is one operation prepared and authorized the way the
// Gateway prepares and authorizes it, for the tests that need every value a
// signed receipt carries about it.
//
// It exists so the gate fixtures can seal a receipt around a REAL preparation.
// A receipt sealed around fixture digests is a document no independent
// derivation can ever equal, so the acceptance path could only be exercised by
// telling the finalizer its own answer -- which is the state this arc set out to
// end.
type preparedOperationV3 struct {
	Material     FrozenOperationMaterialV3
	Prepared     preparedbinding.PreparedOperationBindingV1
	Compiler     preparedbinding.CompilerIdentityV1
	Limits       physicalquery.Limits
	Visible      physicalquery.StatementIdentity
	Companion    *physicalquery.StatementIdentity
	VisibleSQL   string
	CompanionSQL string
	Fingerprints map[querybinding.TargetRole]string
	Targets      map[querybinding.TargetRole]string
}

// prepareOperationV3 prepares the frozen material and authorizes it against the
// receipt fixture's own signed pre-state, so a receipt built from the result
// validates for the reason under test rather than on a limit nobody derived.
func prepareOperationV3(t *testing.T) preparedOperationV3 {
	t.Helper()
	material := reproMaterial(t)
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*logicalCatalog)
	if err != nil {
		t.Fatalf("catalog view: %v", err)
	}
	bindings, err := snapshotBindingsFromArtifactsV3(*logicalCatalog, material.SnapshotArtifactDir)
	if err != nil {
		t.Fatalf("snapshot bindings: %v", err)
	}
	prepared, err := physicalquery.Prepare(physicalquery.PreparationInputs{
		Plan: material.Plan, Grant: material.Grant, Catalog: view, SnapshotBindings: bindings})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		t.Fatalf("compiler identity: %v", err)
	}
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("executable statements: %v", err)
	}
	derivation, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
			Grant: prepared.PolicyGrant(), State: fixture.PreState(prepared.Binding().UsesExpandedEvidence())})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	visibleTarget, err := prepared.Binding().TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		t.Fatalf("prepared visible target: %v", err)
	}
	operation := preparedOperationV3{
		Material: material, Prepared: prepared.Binding(), Compiler: compiler,
		Limits: derivation.Limits, Visible: derivation.Visible,
		VisibleSQL: derivation.VisibleDecision.SQL,
		Fingerprints: map[querybinding.TargetRole]string{
			querybinding.RoleVisible: derivation.VisibleDecision.Fingerprint},
		Targets: map[querybinding.TargetRole]string{querybinding.RoleVisible: visibleTarget},
	}
	if derivation.CompanionDecision != nil {
		companionTarget, targetErr := prepared.Binding().TargetSHA256(preparedbinding.RoleCompanion)
		if targetErr != nil {
			t.Fatalf("prepared companion target: %v", targetErr)
		}
		operation.Companion = derivation.Companion
		operation.CompanionSQL = derivation.CompanionDecision.SQL
		operation.Fingerprints[querybinding.RoleCompanion] = derivation.CompanionDecision.Fingerprint
		operation.Targets[querybinding.RoleCompanion] = companionTarget
	}
	return operation
}

// fixtureOptions is the receipt fixture bound to this real preparation.
func (operation preparedOperationV3) fixtureOptions() fixture.Options {
	return fixture.Options{
		Prepared: &operation.Prepared, Compiler: &operation.Compiler, Limits: &operation.Limits,
		Visible: fixture.Target{
			ExactSQLSHA256: operation.Visible.ExactSHA256, StrictASTSHA256: operation.Visible.StrictASTSHA256,
			PolicyFingerprint:           operation.Fingerprints[querybinding.RoleVisible],
			PreparedTargetBindingSHA256: operation.Targets[querybinding.RoleVisible],
		},
	}
}

// companionTarget is the companion half of the same preparation.
func (operation preparedOperationV3) companionTarget() *fixture.Target {
	if operation.Companion == nil {
		return nil
	}
	return &fixture.Target{
		ExactSQLSHA256: operation.Companion.ExactSHA256, StrictASTSHA256: operation.Companion.StrictASTSHA256,
		PolicyFingerprint:           operation.Fingerprints[querybinding.RoleCompanion],
		PreparedTargetBindingSHA256: operation.Targets[querybinding.RoleCompanion],
	}
}
