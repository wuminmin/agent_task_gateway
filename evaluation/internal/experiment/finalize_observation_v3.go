package experiment

import (
	"errors"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
)

// MeasurementArm names which experimental arm produced a sample.
//
// Only the TaskGate arm executes through the governed Connector, so only it can
// produce observer evidence. The direct-PostgreSQL and native-ProvSQL arms are
// baselines: they issue their own SQL by their own means, and a window collected
// around them would describe statements no TaskGate plan models. Letting them
// carry observer evidence would let a baseline manufacture the very accounting
// the TaskGate arm is being judged by.
type MeasurementArm string

const (
	ArmTaskGate       MeasurementArm = "taskgate"
	ArmDirectPostgres MeasurementArm = "direct_postgres"
	ArmNativeProvSQL  MeasurementArm = "native_provsql"
)

// CarriesObserverEvidence reports whether an arm may present a window.
func (arm MeasurementArm) CarriesObserverEvidence() bool { return arm == ArmTaskGate }

// CarriedEvidenceV3 is what the Adapter produced for one measured operation.
//
// The finalizer treats every field here as a claim. Nothing in it is used as an
// input to the finalizer's own derivation; it is only ever compared against what
// the finalizer derived independently.
type CarriedEvidenceV3 struct {
	Arm       MeasurementArm    `json:"arm"`
	Operation OperationIdentity `json:"operation"`
	// Plan is the Adapter's control plan.
	Plan GatewayControlPlanV3 `json:"plan"`
	// ClassifierManifestSHA256 and ClassifierBindingSHA256 are the Adapter's
	// classifier identities.
	ClassifierManifestSHA256 string `json:"classifier_manifest_sha256"`
	ClassifierBindingSHA256  string `json:"classifier_binding_sha256"`
	// Window is the observed before/after interval.
	Window ObserverWindowV2 `json:"window"`
	// VisibleStatement and CompanionStatement are the signed runtime execution
	// identities of what actually ran, for THIS request.
	//
	// Both are pointers so that absence is a state rather than a value. A
	// non-pointer visible identity made "this path executed no target" and "this
	// path executed a target whose every digest happened to be empty"
	// indistinguishable, and the two replay paths execute no target at all: for
	// them the only correct evidence is nil, and a zero-valued struct must not be
	// able to stand in for it. An idempotent replay in particular returns the
	// ORIGINAL receipt, whose signed binding describes the original execution;
	// nothing in it is a target of the current request.
	VisibleStatement   *physicalquery.StatementIdentity `json:"visible_statement,omitempty"`
	CompanionStatement *physicalquery.StatementIdentity `json:"companion_statement,omitempty"`
	// The prepared target bindings the Adapter read off the signed receipt.
	//
	// physicalquery.StatementIdentity has no member for these -- it describes a
	// statement, not its place in a compiled operation -- so they are carried
	// beside it. Without them the prepared target binding would be signed and
	// never compared, and a receipt re-sealed around a different prepared target
	// would be accepted: gates 18 and 19 caught exactly that.
	VisiblePreparedTargetBindingSHA256   string `json:"visible_prepared_target_binding_sha256,omitempty"`
	CompanionPreparedTargetBindingSHA256 string `json:"companion_prepared_target_binding_sha256,omitempty"`
}

// IndependentInputsV3 is everything the finalizer derives from, none of it
// supplied by the Adapter.
type IndependentInputsV3 struct {
	// CatalogPath is the activated Profile Catalog. The finalizer loads and
	// builds the ExpectedSchema itself rather than accepting a digest.
	CatalogPath string
	// Footprint is the qualified Attestation footprint from its own retained
	// evidence.
	Footprint AttestationFootprintV2
	// PostgreSQL is the runtime identity the deployment actually ran, read from
	// the running container rather than from the sample.
	PostgreSQL PostgreSQLRuntimeIdentity
	// PathKind is the execution path the finalizer determined for this
	// operation, from the Gateway's own signed receipt rather than from the
	// Adapter's plan. Do not infer it from the target count.
	PathKind GatewayPathKind
	// OperationID and ContractIdentity come from the frozen workload contract.
	OperationID      string
	ContractIdentity string
	// VisibleSQL and CompanionSQL are the authorized statements the finalizer
	// reproduced through internal/physicalquery from signed pre-state, using the
	// same production logic the Gateway executed.
	VisibleSQL   string
	CompanionSQL string
	// StrictAST computes structural identities; nil uses the package default.
	StrictAST physicalquery.StrictASTDigester
}

// FinalizationV3 is the finalizer's independently derived result.
type FinalizationV3 struct {
	Operation  OperationIdentity    `json:"operation"`
	Plan       GatewayControlPlanV3 `json:"plan"`
	PlanSHA256 string               `json:"plan_sha256"`
	// ReceiptSHA256 is the typed identity of the complete Gateway-signed receipt
	// this acceptance adjudicated. The runtime wrapper fills it only after receipt
	// validation and signature verification; retaining it makes another attempt's
	// coherent acceptance/window pair distinguishable from this sample's receipt.
	ReceiptSHA256            string                `json:"receipt_sha256"`
	ExpectedSchemaDigest     string                `json:"expected_schema_digest,omitempty"`
	ExpectedSchemaEntries    int64                 `json:"expected_schema_entries,omitempty"`
	ClassifierManifestSHA256 string                `json:"classifier_manifest_sha256"`
	ClassifierBindingSHA256  string                `json:"classifier_binding_sha256"`
	ObserverWindowID         string                `json:"observer_window_id"`
	ObserverWindowSHA256     string                `json:"observer_window_sha256"`
	Delta                    ObservedDelta         `json:"observed_delta"`
	InternalExpectation      []InternalExpectation `json:"internal_expectation,omitempty"`
	// OutcomeCandidateVerification is present only for a strict Scale operation
	// and is authored by the finalizer after exact member-level comparison.
	OutcomeCandidateVerification *OutcomeCandidateVerificationV1 `json:"outcome_candidate_verification,omitempty"`
}

// FinalizeObservationV3 independently rebuilds every expected identity and then
// compares the carried and observed evidence against it.
//
// The order is deliberate: the finalizer derives first and looks at the Adapter's
// claims only to reject them. At no point does a carried value feed a derivation.
//
// The derivation branches on whether the path reaches an ExpectedSchema at all,
// read off the shared dimension table rather than by naming a path. A path that
// performs no Attestation has no Catalog to load, no footprint to qualify, no
// ExpectedSchema to build and no target SQL to reproduce -- and demanding those
// of it is not strictness, it is asking for evidence of something that did not
// happen. What such a path gets instead is an all-zero plan and an entry-less
// manifest, under which every Business statement in the window is unclassified
// and therefore fatal.
func FinalizeObservationV3(carried CarriedEvidenceV3, inputs IndependentInputsV3) (FinalizationV3, error) {
	var result FinalizationV3

	// A baseline arm must never present observer evidence.
	if !carried.Arm.CarriesObserverEvidence() {
		return result, rejectTaskGateAt(fmt.Errorf("arm %q does not execute through the governed Connector and cannot carry observer evidence",
			carried.Arm), rejectionGateCarriedOperation, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence)
	}
	dimensions, known := dimensionsFor(inputs.PathKind)
	if !known {
		return result, rejectTaskGateAt(fmt.Errorf("path_kind %q is not a derivable execution path", inputs.PathKind),
			rejectionGateControlPlan, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceGatewayReceipt)
	}

	digester := inputs.StrictAST
	if digester == nil {
		digester = StrictASTDigest
	}

	var (
		derivedPlan       GatewayControlPlanV3
		derivedOperation  OperationIdentity
		manifestFootprint *AttestationFootprintV2
		targets           []ClassifierEntry
	)

	if dimensions.requiresSchema {
		// 1. ExpectedSchema, from the Catalog builder rather than from any digest
		// the sample carries.
		logicalCatalog, err := catalog.Load(inputs.CatalogPath)
		if err != nil {
			return result, rejectTaskGateAt(fmt.Errorf("load activated Profile Catalog: %w", err),
				rejectionGateExpectedSchema, rejectionFailureUnavailable,
				rejectionSourceActivatedProfile, rejectionSourceActivatedProfile)
		}
		built, err := catalogschema.Build(logicalCatalog)
		if err != nil {
			return result, rejectTaskGateAt(fmt.Errorf("build ExpectedSchema: %w", err),
				rejectionGateExpectedSchema, rejectionFailureInvalidValue,
				rejectionSourceFinalizerDerivation, rejectionSourceActivatedProfile)
		}
		result.ExpectedSchemaDigest, result.ExpectedSchemaEntries = built.Digest, built.Count

		// 2. The qualified footprint must be valid for this deployment.
		if err := inputs.Footprint.Require(built.Digest, built.Count,
			RequiredMeasurementEnvironment(), inputs.PostgreSQL); err != nil {
			return result, rejectTaskGateAt(fmt.Errorf("qualified footprint: %w", err),
				rejectionGateFootprintQualification, rejectionFailureMismatch,
				rejectionSourceFinalizerDerivation, rejectionSourceRetainedQualification)
		}

		// 3. The plan, from the path kind and the footprint.
		derivedPlan, err = planFor(inputs.PathKind, built.Count, built.Digest, inputs.Footprint)
		if err != nil {
			return result, rejectTaskGateAt(fmt.Errorf("derive control plan: %w", err),
				rejectionGateControlPlan, rejectionFailureInvalidValue,
				rejectionSourceFinalizerDerivation, rejectionSourceRetainedQualification)
		}

		// 4. The operation identity, from frozen contract material.
		footprintDigest, err := inputs.Footprint.SHA256()
		if err != nil {
			return result, rejectTaskGateAt(err, rejectionGateFootprintQualification,
				rejectionFailureInvalidValue, rejectionSourceRetainedQualification,
				rejectionSourceRetainedQualification)
		}
		derivedOperation = OperationIdentity{
			OperationID: inputs.OperationID, PathKind: inputs.PathKind,
			ContractIdentity:     inputs.ContractIdentity,
			ExpectedSchemaDigest: built.Digest, AttestationFootprintSHA256: footprintDigest,
		}

		// 5. The targets, rebuilt from the statements the finalizer reproduced
		// through shared production logic.
		targets, err = deriveTargets(inputs, digester)
		if err != nil {
			return result, rejectTaskGateAt(err, rejectionGateClassifierTargets,
				rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
				rejectionSourceFrozenContract)
		}
		footprint := inputs.Footprint
		manifestFootprint = &footprint
	} else {
		// A path that reaches Business PostgreSQL not at all. Every input that
		// only an attesting path can legitimately have must be absent, and
		// absent means absent: accepting a Catalog path "just in case" would let
		// a replay be finalized against schema material belonging to some other
		// request, and accepting a footprint would attach a qualification to a
		// window in which no Attestation occurred.
		if err := requireNoSchemaMaterial(inputs); err != nil {
			return result, rejectTaskGateAt(err, rejectionGateExpectedSchema,
				rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
				rejectionSourceFrozenContract)
		}
		var err error
		derivedPlan, err = planFor(inputs.PathKind, 0, "", AttestationFootprintV2{})
		if err != nil {
			return result, rejectTaskGateAt(fmt.Errorf("derive control plan: %w", err),
				rejectionGateControlPlan, rejectionFailureInvalidValue,
				rejectionSourceFinalizerDerivation, rejectionSourceGatewayReceipt)
		}
		derivedOperation = OperationIdentity{
			OperationID: inputs.OperationID, PathKind: inputs.PathKind,
			ContractIdentity: inputs.ContractIdentity,
		}
	}

	result.Plan, result.InternalExpectation = derivedPlan, derivedPlan.InternalExpectation
	if err := derivedOperation.Validate(); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("derive operation identity: %w", err),
			rejectionGateOperationIdentity, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceFrozenContract)
	}
	result.Operation = derivedOperation

	derivedManifest, err := BuildClassifierManifestV2(derivedPlan, manifestFootprint, targets)
	if err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("derive classifier manifest: %w", err),
			rejectionGateClassifierManifest, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceClassifierPlan)
	}
	classifier, err := CompileClassifierV2(derivedOperation, derivedPlan, derivedManifest)
	if err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("compile derived classifier: %w", err),
			rejectionGateClassifierBinding, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceClassifierPlan)
	}
	result.PlanSHA256 = classifier.PlanSHA256()
	result.ClassifierManifestSHA256 = classifier.ManifestSHA256()
	result.ClassifierBindingSHA256 = classifier.BindingSHA256()

	// Only now is the Adapter's evidence looked at, and only to reject it.
	if !carried.Operation.equalTo(derivedOperation) {
		return result, rejectTaskGateAt(fmt.Errorf("the Adapter's operation identity differs from the finalizer's derivation: %v",
			operationMismatches(carried.Operation, derivedOperation)),
			rejectionGateCarriedOperation, rejectionFailureMismatch,
			rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence)
	}
	if !carried.Plan.Equal(derivedPlan) {
		return result, rejectTaskGateAt(fmt.Errorf("the Adapter's plan differs from the finalizer's derivation on %v",
			derivedPlan.MismatchedFields(carried.Plan)),
			rejectionGateCarriedPlan, rejectionFailureMismatch,
			rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence)
	}
	if carried.ClassifierManifestSHA256 != classifier.ManifestSHA256() {
		return result, rejectTaskGateAt(fmt.Errorf("the Adapter's classifier manifest is %s, the finalizer derives %s",
			shortDigest(carried.ClassifierManifestSHA256), shortDigest(classifier.ManifestSHA256())),
			rejectionGateCarriedClassifierManifest, rejectionFailureMismatch,
			rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence,
			rejectionSHA256Pair(classifier.ManifestSHA256(), carried.ClassifierManifestSHA256)...)
	}
	if carried.ClassifierBindingSHA256 != classifier.BindingSHA256() {
		return result, rejectTaskGateAt(fmt.Errorf("the Adapter's classifier binding is %s, the finalizer derives %s",
			shortDigest(carried.ClassifierBindingSHA256), shortDigest(classifier.BindingSHA256())),
			rejectionGateCarriedClassifierBinding, rejectionFailureMismatch,
			rejectionSourceFinalizerDerivation, rejectionSourceCarriedEvidence,
			rejectionSHA256Pair(classifier.BindingSHA256(), carried.ClassifierBindingSHA256)...)
	}

	// 6. The runtime execution identity. The observer classifies structure; this
	// is what pins the constants a structural digest deliberately ignores.
	if err := requireStatementIdentities(carried, inputs, digester); err != nil {
		return result, rejectTaskGateAt(err, rejectionGateStatementIdentity,
			rejectionFailureMismatch, rejectionSourceFinalizerDerivation,
			rejectionSourceCarriedEvidence)
	}

	// 7. The window is classified with the FINALIZER's classifier and accepted
	// against the FINALIZER's plan.
	delta, err := carried.Window.Delta(classifier)
	if err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("classify observer window: %w", err),
			rejectionGateObserverSnapshotInterval, rejectionFailureInvalidValue,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow)
	}
	result.Delta = delta
	if err := delta.Accept(derivedPlan); err != nil {
		return result, rejectTaskGateAt(err, rejectionGateClosedWorldClasses,
			rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow)
	}
	windowSHA256, err := carried.Window.SHA256()
	if err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("digest accepted observer window: %w", err),
			rejectionGateObserverSnapshotInterval, rejectionFailureInvalidValue,
			rejectionSourceObserverWindow, rejectionSourceObserverWindow)
	}
	result.ObserverWindowID = carried.Window.Before.ObserverWindowID
	result.ObserverWindowSHA256 = windowSHA256

	// 8. The window's runtime identity must be the deployment the footprint was
	// qualified against.
	if carried.Window.After.Runtime.PostgreSQL != inputs.PostgreSQL {
		return result, rejectTaskGateAt(
			errors.New("the observer window ran against a different PostgreSQL runtime than the qualification"),
			rejectionGatePostgreSQLQualificationRuntime, rejectionFailureMismatch,
			rejectionSourceDeploymentRuntime, rejectionSourceObserverWindow)
	}
	return result, nil
}

// requireNoSchemaMaterial rejects finalizer inputs that carry evidence a
// non-attesting path cannot have produced.
//
// This is the finalizer refusing to derive from material that does not belong to
// the request in front of it. An exact request-ID replay returns before
// datasourceEvidence: it builds no ExpectedSchema, performs no Attestation and
// renders no target statement. Inputs claiming otherwise describe some other
// execution, and silently ignoring them would let a replay be finalized against
// a Catalog and a qualification that had nothing to do with it.
func requireNoSchemaMaterial(inputs IndependentInputsV3) error {
	if inputs.CatalogPath != "" {
		return fmt.Errorf("path_kind %s builds no ExpectedSchema, but a Profile Catalog was supplied to finalize it",
			inputs.PathKind)
	}
	if !inputs.Footprint.isZero() {
		return fmt.Errorf("path_kind %s performs no Attestation, but a qualified footprint was supplied to finalize it",
			inputs.PathKind)
	}
	if inputs.VisibleSQL != "" || inputs.CompanionSQL != "" {
		return fmt.Errorf("path_kind %s executes no target statement, but target SQL was reproduced for it",
			inputs.PathKind)
	}
	return nil
}

// deriveTargets rebuilds the target identities from the statements the finalizer
// reproduced, so a target the Adapter declared cannot enter the manifest.
func deriveTargets(inputs IndependentInputsV3, digester physicalquery.StrictASTDigester) ([]ClassifierEntry, error) {
	visible, companion, known := requiredTargets(inputs.PathKind)
	if !known {
		return nil, fmt.Errorf("path_kind %q is not a derivable execution path", inputs.PathKind)
	}
	var targets []ClassifierEntry
	if visible > 0 {
		if inputs.VisibleSQL == "" {
			return nil, fmt.Errorf("path_kind %s executes a visible statement but none was reproduced", inputs.PathKind)
		}
		entry, err := targetEntryWithDigester(V3TargetedVisible, inputs.VisibleSQL, inputs.ContractIdentity, digester)
		if err != nil {
			return nil, err
		}
		targets = append(targets, entry)
	}
	if companion > 0 {
		if inputs.CompanionSQL == "" {
			return nil, fmt.Errorf("path_kind %s executes a companion statement but none was reproduced", inputs.PathKind)
		}
		entry, err := targetEntryWithDigester(V3TargetedCompanion, inputs.CompanionSQL, inputs.ContractIdentity, digester)
		if err != nil {
			return nil, err
		}
		targets = append(targets, entry)
	}
	return targets, nil
}

func targetEntryWithDigester(class GatewayStatementClassV3, renderedSQL, operationContract string,
	digester physicalquery.StrictASTDigester) (ClassifierEntry, error) {
	contractIdentity, err := TargetContractIdentity(operationContract, class)
	if err != nil {
		return ClassifierEntry{}, err
	}
	digest, err := digester(renderedSQL)
	if err != nil {
		return ClassifierEntry{}, fmt.Errorf("strict AST digest for %s: %w", class, err)
	}
	return ClassifierEntry{
		Class: class, StrictASTSHA256: digest, RequiredTopLevel: true,
		SourceKind: SourceQueryContract, ContractIdentity: contractIdentity,
	}, nil
}

// requireStatementIdentities compares the signed runtime execution identities
// against the statements the finalizer reproduced, and requires their absence
// where the path executes nothing.
//
// Both digests are checked. The structural digest is what the observer keys on;
// the exact digest is what detects a constant-only mutation that
// pg_stat_statements has normalized away. The row limits are checked too, because
// they are rendered into the executable bytes.
//
// Absence is checked in both directions and for both roles. A replay executes no
// target of its own, so carrying ANY current execution identity for one is
// evidence of something the path cannot have done -- including the prepared
// target bindings, which name a compiled target rather than a statement and
// would otherwise survive unexamined.
func requireStatementIdentities(carried CarriedEvidenceV3, inputs IndependentInputsV3,
	digester physicalquery.StrictASTDigester) error {
	visible, companion, _ := requiredTargets(inputs.PathKind)
	for _, role := range []struct {
		name      string
		expected  int64
		statement *physicalquery.StatementIdentity
		prepared  string
		sql       string
	}{
		{"visible", visible, carried.VisibleStatement, carried.VisiblePreparedTargetBindingSHA256, inputs.VisibleSQL},
		{"companion", companion, carried.CompanionStatement, carried.CompanionPreparedTargetBindingSHA256, inputs.CompanionSQL},
	} {
		if role.expected == 0 {
			if role.statement != nil {
				return fmt.Errorf("path_kind %s executes no %s statement, but a %s execution identity was carried for this request",
					inputs.PathKind, role.name, role.name)
			}
			if role.prepared != "" {
				return fmt.Errorf("path_kind %s executes no %s statement, but a %s prepared target binding was carried for this request",
					inputs.PathKind, role.name, role.name)
			}
			continue
		}
		if role.statement == nil {
			return fmt.Errorf("path_kind %s executes a %s statement but none was signed", inputs.PathKind, role.name)
		}
		if err := compareStatement(role.name, *role.statement, role.sql, digester); err != nil {
			return err
		}
	}
	return nil
}

func compareStatement(role string, signed physicalquery.StatementIdentity, reproducedSQL string,
	digester physicalquery.StrictASTDigester) error {
	if exact := physicalquery.ExactDigest(reproducedSQL); signed.ExactSHA256 != exact {
		return fmt.Errorf("the signed %s statement is %s, the finalizer reproduces %s; "+
			"the executed bytes differ even if their structure does not",
			role, shortDigest(signed.ExactSHA256), shortDigest(exact))
	}
	strict, err := digester(reproducedSQL)
	if err != nil {
		return fmt.Errorf("strict AST digest for the reproduced %s statement: %w", role, err)
	}
	if signed.StrictASTSHA256 != "" && signed.StrictASTSHA256 != strict {
		return fmt.Errorf("the signed %s statement has structural identity %s, the finalizer reproduces %s",
			role, shortDigest(signed.StrictASTSHA256), shortDigest(strict))
	}
	return nil
}

func (identity OperationIdentity) equalTo(other OperationIdentity) bool { return identity == other }

func operationMismatches(carried, derived OperationIdentity) []string {
	var fields []string
	for name, pair := range map[string][2]string{
		"operation_id":                 {carried.OperationID, derived.OperationID},
		"path_kind":                    {string(carried.PathKind), string(derived.PathKind)},
		"contract_identity":            {carried.ContractIdentity, derived.ContractIdentity},
		"expected_schema_digest":       {carried.ExpectedSchemaDigest, derived.ExpectedSchemaDigest},
		"attestation_footprint_sha256": {carried.AttestationFootprintSHA256, derived.AttestationFootprintSHA256},
	} {
		if pair[0] != pair[1] {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}
