package experiment

import (
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
)

// resultHeavyCatalogPath is the activated Profile Catalog the Artifact cells
// run against. The finalizer loads it rather than trusting any carried digest.
func resultHeavyCatalogPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../../config/profiles/result-heavy.catalog.yaml")
	if err != nil {
		t.Fatalf("resolve catalog: %v", err)
	}
	return path
}

const (
	finalizerVisibleSQL   = `SELECT row_id, category FROM reporting.final_v5_result_heavy WHERE row_id <= 100 ORDER BY row_id LIMIT 100`
	finalizerCompanionSQL = `SELECT ordinal FROM taskgate_ordinal.final_v5_result_heavy_v1 WHERE row_id <= 100 LIMIT 101`
	finalizerOperationID  = "artifact-result-heavy-100x4-op-0001"
	finalizerContract     = "artifact/result-heavy/100x4"
)

// realSchemaFootprint qualifies the Stage N4 footprint against the REAL
// Result-heavy ExpectedSchema, so the finalizer's catalog-derived digest binds.
func realSchemaFootprint(t *testing.T) (AttestationFootprintV2, catalogschema.Result) {
	t.Helper()
	logicalCatalog, err := catalog.Load(resultHeavyCatalogPath(t))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		t.Fatalf("build ExpectedSchema: %v", err)
	}
	footprint, err := NewAttestationFootprintV2(built.Digest, built.Count,
		RequiredMeasurementEnvironment(), testRuntimeIdentity(), "finalizer-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopeConstructorOrColdPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeExplicitPreflightPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeSingleQueryTransaction: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopePairedQueryTransaction: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		})
	if err != nil {
		t.Fatalf("qualify footprint: %v", err)
	}
	return footprint, built
}

func finalizerInputs(t *testing.T) IndependentInputsV3 {
	t.Helper()
	footprint, _ := realSchemaFootprint(t)
	return IndependentInputsV3{
		CatalogPath: resultHeavyCatalogPath(t), Footprint: footprint,
		PostgreSQL: testRuntimeIdentity(), PathKind: PathPairedNovel,
		OperationID: finalizerOperationID, ContractIdentity: finalizerContract,
		VisibleSQL: finalizerVisibleSQL, CompanionSQL: finalizerCompanionSQL,
	}
}

// honestCarriedEvidence is what a correct Adapter produces: identical to what
// the finalizer independently derives.
func honestCarriedEvidence(t *testing.T, inputs IndependentInputsV3) CarriedEvidenceV3 {
	t.Helper()
	footprintDigest, err := inputs.Footprint.SHA256()
	if err != nil {
		t.Fatalf("footprint digest: %v", err)
	}
	operation := OperationIdentity{
		OperationID: inputs.OperationID, PathKind: inputs.PathKind,
		ContractIdentity:           inputs.ContractIdentity,
		ExpectedSchemaDigest:       inputs.Footprint.ExpectedSchemaDigest,
		AttestationFootprintSHA256: footprintDigest,
	}
	plan, err := planFor(inputs.PathKind, inputs.Footprint.ExpectedSchemaEntries,
		inputs.Footprint.ExpectedSchemaDigest, inputs.Footprint)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	targets, err := deriveTargets(inputs, StrictASTDigest)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	manifest, err := BuildClassifierManifest(inputs.Footprint, targets)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	classifier, err := CompileClassifier(operation, manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	var rows []ObserverStructuralRow
	for _, entry := range manifest.Entries {
		calls := plan.Expected()[entry.Class]
		if calls == 0 {
			continue
		}
		rows = append(rows, ObserverStructuralRow{
			StrictASTSHA256: entry.StrictASTSHA256, TopLevel: entry.RequiredTopLevel, Calls: calls,
		})
	}
	before := snapshotOf(t, "before", nil)
	after := snapshotOf(t, "after", rows)

	visibleStrict, _ := StrictASTDigest(inputs.VisibleSQL)
	companionStrict, _ := StrictASTDigest(inputs.CompanionSQL)
	companion := physicalquery.StatementIdentity{
		ExactSHA256:     physicalquery.ExactDigest(inputs.CompanionSQL),
		StrictASTSHA256: companionStrict, RowLimit: 101,
	}
	return CarriedEvidenceV3{
		Arm: ArmTaskGate, Operation: operation, Plan: plan,
		ClassifierManifestSHA256: classifier.ManifestSHA256(),
		ClassifierBindingSHA256:  classifier.BindingSHA256(),
		Window:                   ObserverWindowV2{Before: before, After: after},
		VisibleStatement: physicalquery.StatementIdentity{
			ExactSHA256:     physicalquery.ExactDigest(inputs.VisibleSQL),
			StrictASTSHA256: visibleStrict, RowLimit: 100,
		},
		CompanionStatement: &companion,
	}
}

func TestFinalizerAcceptsHonestEvidence(t *testing.T) {
	inputs := finalizerInputs(t)
	result, err := FinalizeObservationV3(honestCarriedEvidence(t, inputs), inputs)
	if err != nil {
		t.Fatalf("honest evidence was rejected: %v", err)
	}
	_, built := realSchemaFootprint(t)
	if result.ExpectedSchemaDigest != built.Digest {
		t.Fatalf("finalizer derived ExpectedSchema %s, the Catalog builds %s",
			result.ExpectedSchemaDigest, built.Digest)
	}
	if result.ExpectedSchemaEntries != built.Count {
		t.Fatalf("finalizer derived E=%d, the Catalog builds %d", result.ExpectedSchemaEntries, built.Count)
	}
}

// A baseline arm must never manufacture TaskGate observer evidence.
func TestBaselineArmsCannotCarryObserverEvidence(t *testing.T) {
	inputs := finalizerInputs(t)
	for _, arm := range []MeasurementArm{ArmDirectPostgres, ArmNativeProvSQL, ""} {
		carried := honestCarriedEvidence(t, inputs)
		carried.Arm = arm
		_, err := FinalizeObservationV3(carried, inputs)
		if err == nil {
			t.Fatalf("arm %q was allowed to carry observer evidence", arm)
		}
		if !strings.Contains(err.Error(), "cannot carry observer evidence") {
			t.Fatalf("error %q does not name the arm restriction", err)
		}
	}
}

// The finalizer must reject every Adapter claim that differs from its own
// derivation, rather than adopting it.
func TestFinalizerRejectsEveryAdapterDisagreement(t *testing.T) {
	inputs := finalizerInputs(t)
	for _, testCase := range []struct {
		name   string
		mutate func(*CarriedEvidenceV3)
		want   string
	}{
		{"a different operation id",
			func(c *CarriedEvidenceV3) { c.Operation.OperationID = "another-operation" },
			"operation identity differs"},
		{"a different path kind in the operation",
			func(c *CarriedEvidenceV3) { c.Operation.PathKind = PathSingleQuery },
			"operation identity differs"},
		{"a plan claiming another path",
			func(c *CarriedEvidenceV3) { c.Plan.PathKind = PathSingleQuery },
			"plan differs"},
		{"an edited internal expectation",
			func(c *CarriedEvidenceV3) {
				c.Plan.InternalExpectation = []InternalExpectation{
					{StrictASTSHA256: testInternalKeyB, Calls: 2},
				}
			}, "plan differs"},
		{"a substituted classifier manifest",
			func(c *CarriedEvidenceV3) { c.ClassifierManifestSHA256 = strings.Repeat("a", 64) },
			"classifier manifest is"},
		{"a substituted classifier binding",
			func(c *CarriedEvidenceV3) { c.ClassifierBindingSHA256 = strings.Repeat("b", 64) },
			"classifier binding is"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			carried := honestCarriedEvidence(t, inputs)
			testCase.mutate(&carried)
			_, err := FinalizeObservationV3(carried, inputs)
			if err == nil {
				t.Fatal("the finalizer adopted an Adapter claim it should have rejected")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the disagreement %q", err, testCase.want)
			}
		})
	}
}

// A constant-only mutation survives structural normalization; the exact digest
// is what catches it.
func TestConstantOnlyMutationIsDetected(t *testing.T) {
	inputs := finalizerInputs(t)
	carried := honestCarriedEvidence(t, inputs)
	// Same shape, different constant. pg_stat_statements would normalize both to
	// one entry, so no observer count changes.
	mutated := strings.Replace(finalizerVisibleSQL, "row_id <= 100", "row_id <= 999", 1)
	carried.VisibleStatement.ExactSHA256 = physicalquery.ExactDigest(mutated)
	structural, err := StrictASTDigest(mutated)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if structural != carried.VisibleStatement.StrictASTSHA256 {
		t.Skip("the strict AST digest already separates these statements; the exact digest is not the only defence here")
	}
	_, err = FinalizeObservationV3(carried, inputs)
	if err == nil {
		t.Fatal("a constant-only mutation was accepted")
	}
	if !strings.Contains(err.Error(), "executed bytes differ") {
		t.Fatalf("error %q does not name the constant-only mutation", err)
	}
}

// The companion must be present exactly when the path executes one.
func TestCompanionPresenceIsPathSpecific(t *testing.T) {
	inputs := finalizerInputs(t)
	carried := honestCarriedEvidence(t, inputs)
	carried.CompanionStatement = nil
	if _, err := FinalizeObservationV3(carried, inputs); err == nil {
		t.Fatal("a paired-novel operation was finalized without a signed companion")
	}
}

// A footprint qualified elsewhere must not finalize this deployment.
func TestFinalizerRejectsAFootprintFromAnotherDeployment(t *testing.T) {
	inputs := finalizerInputs(t)
	carried := honestCarriedEvidence(t, inputs)
	elsewhere := inputs
	elsewhere.PostgreSQL.Platform = "linux/arm64"
	if _, err := FinalizeObservationV3(carried, elsewhere); err == nil {
		t.Fatal("a footprint qualified on another runtime finalized this deployment")
	}
}

// The window's own runtime identity must be the qualified deployment.
func TestFinalizerRejectsAWindowFromAnotherRuntime(t *testing.T) {
	inputs := finalizerInputs(t)
	carried := honestCarriedEvidence(t, inputs)
	other := testRuntimeIdentity()
	other.ImageReference = "postgres@sha256:" + strings.Repeat("5", 64)
	carried.Window.Before.Runtime.PostgreSQL = other
	carried.Window.After.Runtime.PostgreSQL = other
	if _, err := FinalizeObservationV3(carried, inputs); err == nil {
		t.Fatal("a window from another PostgreSQL runtime was accepted")
	}
}

// The finalizer classifies with its OWN classifier: an unexpected statement
// fails even though the Adapter's evidence is otherwise self-consistent.
func TestFinalizerClassifiesWithItsOwnClassifier(t *testing.T) {
	inputs := finalizerInputs(t)
	carried := honestCarriedEvidence(t, inputs)
	carried.Window.After.Structural = append(carried.Window.After.Structural,
		ObserverStructuralRow{StrictASTSHA256: strings.Repeat("c", 64), TopLevel: true, Calls: 1})
	sortStructural(carried.Window.After.Structural)
	carried.Window.After.Total++
	if _, err := FinalizeObservationV3(carried, inputs); err == nil {
		t.Fatal("an unexpected statement survived independent finalization")
	}
}
