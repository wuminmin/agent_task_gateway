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
	// identities of what actually ran.
	VisibleStatement   physicalquery.StatementIdentity  `json:"visible_statement"`
	CompanionStatement *physicalquery.StatementIdentity `json:"companion_statement,omitempty"`
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
	Operation                OperationIdentity     `json:"operation"`
	Plan                     GatewayControlPlanV3  `json:"plan"`
	ExpectedSchemaDigest     string                `json:"expected_schema_digest"`
	ExpectedSchemaEntries    int64                 `json:"expected_schema_entries"`
	ClassifierManifestSHA256 string                `json:"classifier_manifest_sha256"`
	ClassifierBindingSHA256  string                `json:"classifier_binding_sha256"`
	Delta                    ObservedDelta         `json:"observed_delta"`
	InternalExpectation      []InternalExpectation `json:"internal_expectation"`
}

// FinalizeObservationV3 independently rebuilds every expected identity and then
// compares the carried and observed evidence against it.
//
// The order is deliberate: the finalizer derives first and looks at the Adapter's
// claims only to reject them. At no point does a carried value feed a derivation.
func FinalizeObservationV3(carried CarriedEvidenceV3, inputs IndependentInputsV3) (FinalizationV3, error) {
	var result FinalizationV3

	// A baseline arm must never present observer evidence.
	if !carried.Arm.CarriesObserverEvidence() {
		return result, fmt.Errorf("arm %q does not execute through the governed Connector and cannot carry observer evidence",
			carried.Arm)
	}

	// 1. ExpectedSchema, from the Catalog builder rather than from any digest
	// the sample carries.
	logicalCatalog, err := catalog.Load(inputs.CatalogPath)
	if err != nil {
		return result, fmt.Errorf("load activated Profile Catalog: %w", err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return result, fmt.Errorf("build ExpectedSchema: %w", err)
	}
	result.ExpectedSchemaDigest, result.ExpectedSchemaEntries = built.Digest, built.Count

	// 2. The qualified footprint must be valid for this deployment.
	if err := inputs.Footprint.Require(built.Digest, built.Count,
		RequiredMeasurementEnvironment(), inputs.PostgreSQL); err != nil {
		return result, fmt.Errorf("qualified footprint: %w", err)
	}

	// 3. The plan, from the path kind and the footprint.
	derivedPlan, err := planFor(inputs.PathKind, built.Count, built.Digest, inputs.Footprint)
	if err != nil {
		return result, fmt.Errorf("derive control plan: %w", err)
	}
	result.Plan, result.InternalExpectation = derivedPlan, derivedPlan.InternalExpectation

	// 4. The operation identity, from frozen contract material.
	footprintDigest, err := inputs.Footprint.SHA256()
	if err != nil {
		return result, err
	}
	derivedOperation := OperationIdentity{
		OperationID: inputs.OperationID, PathKind: inputs.PathKind,
		ContractIdentity: inputs.ContractIdentity,
	}
	if dimensions, _ := dimensionsFor(inputs.PathKind); dimensions.requiresSchema {
		derivedOperation.ExpectedSchemaDigest = built.Digest
		derivedOperation.AttestationFootprintSHA256 = footprintDigest
	}
	if err := derivedOperation.Validate(); err != nil {
		return result, fmt.Errorf("derive operation identity: %w", err)
	}
	result.Operation = derivedOperation

	// 5. The classifier, from the footprint and the statements the finalizer
	// reproduced through shared production logic.
	digester := inputs.StrictAST
	if digester == nil {
		digester = StrictASTDigest
	}
	targets, err := deriveTargets(inputs, digester)
	if err != nil {
		return result, err
	}
	derivedManifest, err := BuildClassifierManifest(inputs.Footprint, targets)
	if err != nil {
		return result, fmt.Errorf("derive classifier manifest: %w", err)
	}
	classifier, err := CompileClassifier(derivedOperation, derivedManifest)
	if err != nil {
		return result, fmt.Errorf("compile derived classifier: %w", err)
	}
	result.ClassifierManifestSHA256 = classifier.ManifestSHA256()
	result.ClassifierBindingSHA256 = classifier.BindingSHA256()

	// Only now is the Adapter's evidence looked at, and only to reject it.
	if !carried.Operation.equalTo(derivedOperation) {
		return result, fmt.Errorf("the Adapter's operation identity differs from the finalizer's derivation: %v",
			operationMismatches(carried.Operation, derivedOperation))
	}
	if !carried.Plan.Equal(derivedPlan) {
		return result, fmt.Errorf("the Adapter's plan differs from the finalizer's derivation on %v",
			derivedPlan.MismatchedFields(carried.Plan))
	}
	if carried.ClassifierManifestSHA256 != classifier.ManifestSHA256() {
		return result, fmt.Errorf("the Adapter's classifier manifest is %s, the finalizer derives %s",
			shortDigest(carried.ClassifierManifestSHA256), shortDigest(classifier.ManifestSHA256()))
	}
	if carried.ClassifierBindingSHA256 != classifier.BindingSHA256() {
		return result, fmt.Errorf("the Adapter's classifier binding is %s, the finalizer derives %s",
			shortDigest(carried.ClassifierBindingSHA256), shortDigest(classifier.BindingSHA256()))
	}

	// 6. The runtime execution identity. The observer classifies structure; this
	// is what pins the constants a structural digest deliberately ignores.
	if err := requireStatementIdentities(carried, inputs, digester); err != nil {
		return result, err
	}

	// 7. The window is classified with the FINALIZER's classifier and accepted
	// against the FINALIZER's plan.
	delta, err := carried.Window.Delta(classifier)
	if err != nil {
		return result, fmt.Errorf("classify observer window: %w", err)
	}
	result.Delta = delta
	if err := delta.Accept(derivedPlan); err != nil {
		return result, err
	}

	// 8. The window's runtime identity must be the deployment the footprint was
	// qualified against.
	if carried.Window.After.Runtime.PostgreSQL != inputs.PostgreSQL {
		return result, errors.New("the observer window ran against a different PostgreSQL runtime than the qualification")
	}
	return result, nil
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
// against the statements the finalizer reproduced.
//
// Both digests are checked. The structural digest is what the observer keys on;
// the exact digest is what detects a constant-only mutation that
// pg_stat_statements has normalized away. The row limits are checked too, because
// they are rendered into the executable bytes.
func requireStatementIdentities(carried CarriedEvidenceV3, inputs IndependentInputsV3,
	digester physicalquery.StrictASTDigester) error {
	visible, companion, _ := requiredTargets(inputs.PathKind)
	if visible > 0 {
		if err := compareStatement("visible", carried.VisibleStatement, inputs.VisibleSQL, digester); err != nil {
			return err
		}
	}
	switch {
	case companion > 0:
		if carried.CompanionStatement == nil {
			return fmt.Errorf("path_kind %s executes a companion statement but none was signed", inputs.PathKind)
		}
		if err := compareStatement("companion", *carried.CompanionStatement, inputs.CompanionSQL, digester); err != nil {
			return err
		}
	case carried.CompanionStatement != nil:
		return fmt.Errorf("path_kind %s executes no companion statement but one was signed", inputs.PathKind)
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
