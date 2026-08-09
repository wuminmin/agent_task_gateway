package experiment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// This file is the finalizer's own execution of a governed operation.
//
// # What was missing, and why a comment was not enough
//
// TrustedInputsV3 has always documented VisibleSQL and CompanionSQL as "the
// statements the finalizer reproduced through internal/physicalquery from
// signed pre-state". Nothing reproduced them. They were string fields, and
// whoever filled them decided what the finalizer would compare the Gateway's
// signature against -- so the strongest check in the acceptance path, "the
// signed statement is the statement that must have been produced", was resting
// on a promise in a doc comment.
//
// T1d removed the reason it was a promise. The derivation used to be unexported
// glue inside internal/gateway, so the only parties who could produce the
// statements were the Gateway itself, the Adapter, or a second implementation --
// and each of those is disqualified for its own reason. internal/physicalquery
// is now the one implementation, exported, deriving from immutable inputs. This
// file is the finalizer calling it.
//
// # What "independent" means here, precisely
//
// Every input below is either frozen contract material the finalizer holds for
// itself, or a member of the receipt whose signature it has already verified.
// Nothing comes from the Adapter, and nothing comes from the Gateway except
// through a signature.
//
// The receipt is used as an input in exactly one place -- the exposure ledger
// pre-state the row limits derive from -- and that is not circular. The receipt
// validator has already required those limits to reproduce from that pre-state,
// and the pre-state to agree with the signed budget; the question this file
// answers is a different one, which no amount of internal coherence can settle:
// whether the statements that pre-state authorizes are the statements whose
// digests the Gateway signed.

// FrozenOperationMaterialV3 is everything the finalizer needs to prepare one
// operation for itself.
//
// It is deliberately not "the Gateway's inputs". The Gateway reads a loaded
// Catalog, a live snapshot registry and a Control Store row; the finalizer reads
// an activated Profile Catalog file, a retained artifact directory and the
// frozen workload contract. They are different acquisitions of the same
// immutable material, which is the only reason agreement between them means
// anything.
type FrozenOperationMaterialV3 struct {
	// CatalogPath is the activated Profile Catalog, read as a file. It is the
	// same path FinalizeObservationV3 builds the ExpectedSchema from, so a
	// finalization cannot prepare against one Catalog and attest another.
	CatalogPath string
	// SnapshotArtifactDir is the retained publication artifact directory, the
	// deployment's GATEWAY_SNAPSHOT_ARTIFACT_DIR. It is required exactly when the
	// exposure profile compiles an ordinal program, and read only for its bundle
	// manifests -- every value taken from one is checked against the Catalog.
	SnapshotArtifactDir string
	// Plan and Grant come from the frozen workload contract: the plan the
	// operation was defined to run and the approved surface it was defined to run
	// under. Neither is read back from the Gateway, and neither may be supplied
	// by the Adapter.
	Plan  queryplan.QueryPlan
	Grant physicalquery.Grant
}

// Validate rejects material that cannot prepare an operation at all.
func (material FrozenOperationMaterialV3) Validate() error {
	if strings.TrimSpace(material.CatalogPath) == "" {
		return errors.New("frozen operation material names no activated Profile Catalog")
	}
	if err := material.Grant.Validate(); err != nil {
		return fmt.Errorf("frozen grant: %w", err)
	}
	if material.Grant.UsesOrdinalProgram() && strings.TrimSpace(material.SnapshotArtifactDir) == "" {
		return fmt.Errorf("exposure profile %q compiles an ordinal program, so the finalizer needs the "+
			"retained snapshot artifact directory to prepare it", material.Grant.ExposureProfile)
	}
	if !material.Grant.UsesOrdinalProgram() && strings.TrimSpace(material.SnapshotArtifactDir) != "" {
		return fmt.Errorf("exposure profile %q compiles no ordinal program, but a snapshot artifact "+
			"directory was supplied", material.Grant.ExposureProfile)
	}
	return nil
}

// ReproducedExecutionV3 is what the finalizer produced by preparing and
// authorizing the operation itself.
type ReproducedExecutionV3 struct {
	// VisibleSQL and CompanionSQL are the executable statements, with the row
	// limits rendered in. CompanionSQL is empty when the operation prepares none.
	VisibleSQL   string
	CompanionSQL string
	// Visible is the authorizer's own StatementIdentity output. It is retained
	// directly from physicalquery.Derive, not reconstructed from VisibleSQL.
	Visible physicalquery.StatementIdentity
	// Companion is the authorizer's own optional StatementIdentity output. It is
	// retained directly from physicalquery.Derive, not inferred from CompanionSQL.
	Companion *physicalquery.StatementIdentity
	// Prepared is the sealed preparation the finalizer produced. It has already
	// been required to equal the one the receipt carries.
	Prepared preparedbinding.PreparedOperationBindingV1
	// Limits are the row limits the finalizer derived from the signed pre-state.
	Limits physicalquery.Limits
}

// ReproduceExecutionV3 prepares, authorizes and compares one governed operation.
//
// The receipt must already be verified and validated; this function reads its
// signed members and does not re-establish their authenticity. It returns the
// statements only if every comparison held, so a caller cannot accidentally use
// a reproduction that disagreed with the signature.
func ReproduceExecutionV3(receipt queryreceipt.QueryReceiptV1,
	material FrozenOperationMaterialV3) (ReproducedExecutionV3, error) {
	var result ReproducedExecutionV3
	binding := receipt.ExecutionBindingV2
	if binding == nil {
		return result, errors.New("the receipt describes no execution, so there is nothing to reproduce")
	}
	if err := material.Validate(); err != nil {
		return result, err
	}

	// 1. The compiler. The finalizer reproduces by RUNNING the compiler, so a
	// finalizer built from different code would produce different statements and
	// report the Gateway as wrong. Requiring the two identities to be equal makes
	// that case say what it is -- the wrong binary is finalizing -- instead of
	// surfacing as a statement mismatch nobody can act on.
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return result, fmt.Errorf("this finalizer cannot determine its own compiler identity: %w", err)
	}
	if compiler.SHA256 != binding.Compiler.SHA256 {
		return result, fmt.Errorf("the operation was compiled by %s but this finalizer is %s; "+
			"a reproduction by a different build proves nothing about this execution",
			shortDigest(binding.Compiler.SHA256), shortDigest(compiler.SHA256))
	}

	// 2. The preparation inputs, assembled finalizer-side.
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		return result, fmt.Errorf("load activated Profile Catalog: %w", err)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*logicalCatalog)
	if err != nil {
		return result, fmt.Errorf("build catalog view: %w", err)
	}
	inputs := physicalquery.PreparationInputs{
		Plan: material.Plan, Grant: material.Grant, Catalog: view,
	}
	if material.Grant.UsesOrdinalProgram() {
		bindings, bindingErr := snapshotBindingsFromArtifactsV3(*logicalCatalog, material.SnapshotArtifactDir)
		if bindingErr != nil {
			return result, bindingErr
		}
		inputs.SnapshotBindings = bindings
	}

	// 3. Prepare, and hold the result to its own inputs before comparing it with
	// anything. A preparation that does not describe the inputs it was built from
	// is not evidence about them.
	prepared, err := physicalquery.Prepare(inputs)
	if err != nil {
		return result, fmt.Errorf("the finalizer could not prepare this operation from frozen material: %w", err)
	}
	if err := prepared.RequireInputs(inputs, compiler); err != nil {
		return result, fmt.Errorf("the finalizer's preparation disagrees with its own inputs: %w", err)
	}

	// 4. The comparison this file exists for. RequirePreparedSame compares the
	// whole sealed document member by member rather than its digest, so a
	// difference names the member that moved.
	if err := binding.RequirePreparedSame(prepared.Binding()); err != nil {
		return result, fmt.Errorf("the Gateway signed a preparation the finalizer does not reproduce: %w", err)
	}

	// 5. The statements, authorized against the pre-state the receipt signs.
	//
	// The prepared statements carry no row limit; the limit is rendered by the
	// authorizer, from the ledger state the operation began at. Reproducing the
	// executed BYTES therefore needs both halves, which is why this does not stop
	// at ExecutableStatements.
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		return result, fmt.Errorf("the finalizer's preparation refuses to yield its statements: %w", err)
	}
	state, err := preStateFromReceiptV3(receipt, prepared.Binding())
	if err != nil {
		return result, err
	}
	derivation, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{
			VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
			Grant: prepared.PolicyGrant(), State: state,
		})
	if err != nil {
		return result, fmt.Errorf("the finalizer could not authorize the prepared statements: %w", err)
	}

	// 6. The derived limits must be the signed ones. The receipt has already
	// checked that its limits reproduce from its pre-state ARITHMETICALLY; this
	// checks that they reproduce from the pre-state THROUGH THE AUTHORIZER, which
	// is the step that can lower a limit and therefore the step that decides the
	// executed bytes.
	if err := requireDerivedLimitsSignedV3(derivation.Limits, *binding); err != nil {
		return result, err
	}

	result = ReproducedExecutionV3{
		VisibleSQL: derivation.VisibleDecision.SQL,
		Visible:    derivation.Visible,
		Companion:  derivation.Companion,
		Prepared:   prepared.Binding(),
		Limits:     derivation.Limits,
	}
	if derivation.CompanionDecision != nil {
		result.CompanionSQL = derivation.CompanionDecision.SQL
	}
	// The companion's presence is a property of the preparation, and the binding
	// already agrees with the preparation by step 4. Restating it against the
	// derivation catches an authorizer that dropped or invented a statement
	// between the two.
	if (result.CompanionSQL != "") != (binding.Companion != nil) {
		return result, fmt.Errorf("the finalizer derived %d companion statements but the Gateway signed %d",
			boolCountV3(result.CompanionSQL != ""), boolCountV3(binding.Companion != nil))
	}
	return result, nil
}

// preStateFromReceiptV3 reads the ledger state the operation began at off the
// receipt.
//
// The expanded-evidence flag is asked of the sealed preparation rather than of
// the ledger document, because the preparation is what the compilation decided
// and the ledger only records it. They are required to agree by the receipt's
// own validator, so taking either is correct; taking the preparation's keeps
// this file reading compilation facts from the compilation.
func preStateFromReceiptV3(receipt queryreceipt.QueryReceiptV1,
	prepared preparedbinding.PreparedOperationBindingV1) (physicalquery.LedgerPreState, error) {
	if receipt.ExposureLedgerBefore == nil {
		// A completed operation that accounted no exposure. It read no ledger, so
		// the row budget the receipt signs is the whole of its pre-state.
		remaining := receipt.BudgetBefore.Limits.Rows - receipt.BudgetBefore.Used.Rows -
			receipt.BudgetBefore.Reserved.Rows
		if remaining < 0 {
			return physicalquery.LedgerPreState{},
				fmt.Errorf("the signed budget pre-state leaves %d rows", remaining)
		}
		return physicalquery.LedgerPreState{RemainingRows: remaining}, nil
	}
	ledger := *receipt.ExposureLedgerBefore
	return physicalquery.LedgerPreState{
		RemainingRows:        ledger.RemainingRows,
		InfluenceFacts:       ledger.Limits.InfluenceFacts,
		UsesExpandedEvidence: prepared.UsesExpandedEvidence(),
		HasExposureContext:   ledger.HasExposureContext,
	}, nil
}

// requireDerivedLimitsSignedV3 compares the three derived limits with the three
// the Gateway signed.
//
// All three are checked even though the receipt already reproduces them from its
// pre-state, because the receipt reproduces the ARITHMETIC and this reproduces
// the AUTHORIZATION. A policy grant that lowered the visible limit would leave
// the receipt's own check satisfied -- its bound is an inequality -- while
// changing the bytes that ran.
func requireDerivedLimitsSignedV3(derived physicalquery.Limits,
	binding querybinding.QueryExecutionBindingV2) error {
	for _, limit := range []struct {
		name            string
		derived, signed int64
	}{
		{"visible row limit", derived.VisibleRowLimit, binding.VisibleRowLimit},
		{"companion evidence rows", derived.CompanionEvidenceRows, binding.CompanionEvidenceRows},
		{"companion policy rows", derived.CompanionPolicyRows, binding.CompanionPolicyRows},
	} {
		if limit.derived != limit.signed {
			return fmt.Errorf("the finalizer derives %s %d but the Gateway signed %d",
				limit.name, limit.derived, limit.signed)
		}
	}
	return nil
}

// snapshotBindingsFromArtifactsV3 rebuilds the dictionary universe from retained
// artifacts, with no live registry.
//
// The Gateway resolves these through a registry it loaded at startup. A
// finalizer has no such registry and must not be given one: a handle onto the
// running deployment's state is the Gateway's own view, and agreeing with it
// would prove nothing. What the finalizer reads instead is the bundle manifest
// each publication directory carries, which is immutable and self-describing.
//
// Every value taken from a manifest is then checked against the Catalog by
// PreparationInputs.Validate through SnapshotBinding.RequireCatalog, so a
// tampered manifest fails rather than silently preparing different SQL. Only the
// row count has no Catalog counterpart, and it is the one value a manifest is
// the authority on.
func snapshotBindingsFromArtifactsV3(logicalCatalog catalog.Catalog,
	directory string) (map[string]physicalquery.SnapshotBinding, error) {
	bindings := make(map[string]physicalquery.SnapshotBinding, len(logicalCatalog.SnapshotPublications))
	for _, publication := range logicalCatalog.SnapshotPublications {
		path := filepath.Join(directory, publication.Name, publication.Name+".bundle.json")
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open retained bundle manifest for publication %q: %w", publication.Name, err)
		}
		manifest, decodeErr := snapshotbundle.DecodeBundleManifest(file)
		closeErr := file.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode retained bundle manifest for publication %q: %w",
				publication.Name, decodeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if manifest.PublicationName != publication.Name {
			return nil, fmt.Errorf("the artifact directory for publication %q holds a manifest for %q",
				publication.Name, manifest.PublicationName)
		}
		bindings[publication.Name] = physicalquery.SnapshotBinding{
			PublicationName:  publication.Name,
			DictionaryDigest: manifest.DictionaryManifest.DictionaryDigest,
			ManifestDigest:   manifest.ManifestDigest,
			SidecarDigest:    manifest.DictionaryManifest.SidecarDigest,
			SourceNamespace:  manifest.DictionaryManifest.SourceNamespace,
			Snapshot:         manifest.DictionaryManifest.Snapshot,
			OrdinalSidecar:   manifest.OrdinalSidecar,
			RowCount:         manifest.RowCount,
		}
	}
	return bindings, nil
}

func boolCountV3(value bool) int {
	if value {
		return 1
	}
	return 0
}

// prepareNominalStatementsV3 derives the executed statements against a nominal
// ledger state, for the pre-registration step that has no real one yet.
//
// The state is nominal in its NUMBERS only. The statements it produces are the
// statements the operation will execute, differing at most in the row limits
// rendered into them -- and the strict AST digest, which is what the classifier
// keys on, normalizes those away. See OpenObserverWindowV3 for why that is
// sound and for what would happen if it were not.
func prepareNominalStatementsV3(material FrozenOperationMaterialV3) (string, string, error) {
	if err := material.Validate(); err != nil {
		return "", "", err
	}
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		return "", "", fmt.Errorf("load activated Profile Catalog: %w", err)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*logicalCatalog)
	if err != nil {
		return "", "", fmt.Errorf("build catalog view: %w", err)
	}
	inputs := physicalquery.PreparationInputs{Plan: material.Plan, Grant: material.Grant, Catalog: view}
	if material.Grant.UsesOrdinalProgram() {
		bindings, bindingErr := snapshotBindingsFromArtifactsV3(*logicalCatalog, material.SnapshotArtifactDir)
		if bindingErr != nil {
			return "", "", bindingErr
		}
		inputs.SnapshotBindings = bindings
	}
	prepared, err := physicalquery.Prepare(inputs)
	if err != nil {
		return "", "", fmt.Errorf("prepare the frozen operation: %w", err)
	}
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		return "", "", err
	}
	// One evidence row is the smallest state that renders both statements. It is
	// deliberately not the operation's real budget: using a plausible-looking one
	// would invite a reader to think the numbers matter.
	nominal := physicalquery.LedgerPreState{
		RemainingRows: 1, InfluenceFacts: 1,
		UsesExpandedEvidence: prepared.Binding().UsesExpandedEvidence(),
		HasExposureContext:   material.Grant.ExposureEnabled(),
	}
	derivation, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
			Grant: prepared.PolicyGrant(), State: nominal})
	if err != nil {
		return "", "", fmt.Errorf("authorize the frozen operation against a nominal pre-state: %w", err)
	}
	visible := derivation.VisibleDecision.SQL
	companion := ""
	if derivation.CompanionDecision != nil {
		companion = derivation.CompanionDecision.SQL
	}
	return visible, companion, nil
}

// preRegisterObservationV3 builds the classification the observer window commits
// to, before the operation runs.
//
// It is the same construction FinalizeObservationV3 performs after the fact,
// reached through the same functions, so the two agree by sharing code rather
// than by two implementations happening to match.
//
// It returns the whole commitment rather than only the manifest digest. The
// observer needs the digest, but the Adapter has to CARRY the commitment into
// its evidence, and it cannot derive one for itself: a classifier manifest is
// built from the rendered target statements, which is exactly the material an
// Adapter may not hold. See PreRegisteredObservationV3 for what the carried copy
// is then worth.
func preRegisterObservationV3(candidate frozenOperationCandidateV3, profile profileMaterialV3,
	footprint AttestationFootprintV2, visibleSQL, companionSQL string) (PreRegisteredObservationV3, error) {
	var registered PreRegisteredObservationV3
	logicalCatalog, err := catalog.Load(profile.CatalogPath)
	if err != nil {
		return registered, fmt.Errorf("load activated Profile Catalog: %w", err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return registered, fmt.Errorf("build ExpectedSchema: %w", err)
	}
	plan, err := planFor(candidate.PathKind, built.Count, built.Digest, footprint)
	if err != nil {
		return registered, fmt.Errorf("derive control plan: %w", err)
	}
	targets, err := deriveTargets(IndependentInputsV3{
		PathKind: candidate.PathKind, ContractIdentity: candidate.ContractIdentity,
		VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
	}, StrictASTDigest)
	if err != nil {
		return registered, err
	}
	manifest, err := BuildClassifierManifestV2(plan, &footprint, targets)
	if err != nil {
		return registered, fmt.Errorf("build classifier manifest: %w", err)
	}
	footprintDigest, err := footprint.SHA256()
	if err != nil {
		return registered, err
	}
	operation := OperationIdentity{
		OperationID: candidate.OperationID, PathKind: candidate.PathKind,
		ContractIdentity:     candidate.ContractIdentity,
		ExpectedSchemaDigest: built.Digest, AttestationFootprintSHA256: footprintDigest,
	}
	// Compiled rather than digested member by member: compilation is what requires
	// the operation, the plan and the manifest to describe one execution path and
	// one qualification, so a commitment that compiles is one finalization can
	// reach at all.
	classifier, err := CompileClassifierV2(operation, plan, manifest)
	if err != nil {
		return registered, fmt.Errorf("compile the pre-registered classifier: %w", err)
	}
	return PreRegisteredObservationV3{
		Operation: operation, Plan: plan,
		ClassifierManifestSHA256: classifier.ManifestSHA256(),
		ClassifierBindingSHA256:  classifier.BindingSHA256(),
	}, nil
}
