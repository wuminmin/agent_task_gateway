package experiment

import (
	"strings"
	"testing"
)

const testOperationContract = "artifact/result-heavy/100x4"

func testOperation(t *testing.T, kind GatewayPathKind) OperationIdentity {
	t.Helper()
	footprint := qualifiedFootprint(t)
	digest, err := footprint.SHA256()
	if err != nil {
		t.Fatalf("footprint digest: %v", err)
	}
	identity := OperationIdentity{
		OperationID: "operation-0001", PathKind: kind,
		ContractIdentity: testOperationContract,
	}
	if dimensions, _ := dimensionsFor(kind); dimensions.requiresSchema {
		identity.ExpectedSchemaDigest = schemaDigestFor(1)
		identity.AttestationFootprintSHA256 = digest
	}
	return identity
}

// testPlan is the independently derived plan for one path against the shared
// qualified footprint. Under manifest v2 it is the plan, not the manifest, that
// settles which classes exist in a path's closed world, so almost every
// classifier test needs one.
func testPlan(t *testing.T, kind GatewayPathKind) GatewayControlPlanV3 {
	t.Helper()
	return testPlanFrom(t, kind, qualifiedFootprint(t))
}

func testPlanFrom(t *testing.T, kind GatewayPathKind, footprint AttestationFootprintV2) GatewayControlPlanV3 {
	t.Helper()
	entries, digest := footprint.ExpectedSchemaEntries, footprint.ExpectedSchemaDigest
	if dimensions, _ := dimensionsFor(kind); !dimensions.requiresSchema {
		entries, digest, footprint = 0, "", AttestationFootprintV2{}
	}
	plan, err := planFor(kind, entries, digest, footprint)
	if err != nil {
		t.Fatalf("plan for %s: %v", kind, err)
	}
	return plan
}

// buildTestManifest supplies the qualified footprint exactly when the path
// attests, which is what BuildClassifierManifestV2 requires: a non-attesting
// path must present none at all.
func buildTestManifest(t *testing.T, plan GatewayControlPlanV3, footprint AttestationFootprintV2,
	targets ...ClassifierEntry) ClassifierManifest {
	t.Helper()
	var supplied *AttestationFootprintV2
	if dimensions, _ := dimensionsFor(plan.PathKind); dimensions.requiresSchema {
		supplied = &footprint
	}
	manifest, err := BuildClassifierManifestV2(plan, supplied, targets)
	if err != nil {
		t.Fatalf("build manifest for %s: %v", plan.PathKind, err)
	}
	return manifest
}

func compiledTestManifest(t *testing.T, kind GatewayPathKind, targets ...ClassifierEntry) ClassifierManifest {
	t.Helper()
	return buildTestManifest(t, testPlan(t, kind), qualifiedFootprint(t), targets...)
}

// compileTest is the three-document compile every test needs: one operation, the
// plan derived for its path, and the manifest built from that plan.
func compileTest(t *testing.T, kind GatewayPathKind, manifest ClassifierManifest) (*CompiledClassifier, error) {
	t.Helper()
	return CompileClassifierV2(testOperation(t, kind), testPlan(t, kind), manifest)
}

func pairedTargets(t *testing.T) []ClassifierEntry {
	t.Helper()
	visible, err := TargetEntry(V3TargetedVisible,
		`SELECT row_id FROM reporting.final_v5_result_heavy WHERE row_id <= $1`, testOperationContract)
	if err != nil {
		t.Fatalf("visible target: %v", err)
	}
	companion, err := TargetEntry(V3TargetedCompanion,
		`SELECT ordinal FROM taskgate_ordinal.final_v5_result_heavy_v1 WHERE row_id <= $1`, testOperationContract)
	if err != nil {
		t.Fatalf("companion target: %v", err)
	}
	return []ClassifierEntry{visible, companion}
}

func TestCompiledClassifierResolvesEveryDefinedKey(t *testing.T) {
	targets := pairedTargets(t)
	classifier, err := compileTest(t, PathPairedNovel, compiledTestManifest(t, PathPairedNovel, targets...))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, target := range targets {
		if got := classifier.Classify(target.StrictASTSHA256, true); got != target.Class {
			t.Errorf("target classified as %s, want %s", got, target.Class)
		}
	}
	// The internal key is nested by definition; the same shape at top level is a
	// different statement.
	internal := qualifiedFootprint(t).InternalKeys()[0]
	if got := classifier.Classify(internal, false); got != V3PostgreSQLInternalAttestation {
		t.Errorf("nested internal key classified as %s", got)
	}
	if got := classifier.Classify(internal, true); got != V3Unexpected {
		t.Errorf("a top-level statement with the internal shape classified as %s", got)
	}
	if got := classifier.Classify(strings.Repeat("f", 64), true); got != V3Unexpected {
		t.Errorf("an undefined key classified as %s, want unexpected", got)
	}
}

// Target cardinality is path-specific, and under v2 the plan is what states it.
// A manifest carrying a companion cannot be built for -- let alone compiled
// against -- a path that issues none, even though every control entry matches.
func TestCompilationEnforcesPathSpecificTargetCardinality(t *testing.T) {
	paired := pairedTargets(t)
	visibleOnly := paired[:1]

	for _, testCase := range []struct {
		name    string
		kind    GatewayPathKind
		targets []ClassifierEntry
		wantErr string
	}{
		{"paired novel needs a companion", PathPairedNovel, visibleOnly, "targeted_companion"},
		{"single query must not carry one", PathSingleQuery, paired, "targeted_companion"},
		{"semantic replay executes no target", PathSemanticReplay, visibleOnly, "targeted_visible"},
		{"idempotent replay executes no target", PathIdempotentReplay, visibleOnly, "targeted_visible"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := testPlan(t, testCase.kind)
			var footprint *AttestationFootprintV2
			if dimensions, _ := dimensionsFor(testCase.kind); dimensions.requiresSchema {
				qualified := qualifiedFootprint(t)
				footprint = &qualified
			}
			_, err := BuildClassifierManifestV2(plan, footprint, testCase.targets)
			if err == nil {
				t.Fatal("a manifest with the wrong target cardinality was built")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not name the cardinality defect", err)
			}
		})
	}

	// The correct cardinalities must compile.
	for _, testCase := range []struct {
		kind    GatewayPathKind
		targets []ClassifierEntry
	}{
		{PathPairedNovel, paired},
		{PathSingleQuery, visibleOnly},
		{PathSemanticReplay, nil},
		{PathIdempotentReplay, nil},
	} {
		t.Run(string(testCase.kind), func(t *testing.T) {
			if _, err := compileTest(t, testCase.kind,
				compiledTestManifest(t, testCase.kind, testCase.targets...)); err != nil {
				t.Fatalf("the honest %s manifest was rejected: %v", testCase.kind, err)
			}
		})
	}
}

// A manifest is only meaningful for one execution path. One built for a path
// that executes must not compile for a path that does not, and the reverse.
func TestAManifestCannotBePresentedForAnotherPath(t *testing.T) {
	for _, testCase := range []struct {
		built, compiled GatewayPathKind
		targets         []ClassifierEntry
	}{
		{PathPairedNovel, PathSingleQuery, pairedTargets(t)},
		{PathSingleQuery, PathPairedNovel, pairedTargets(t)[:1]},
		{PathSemanticReplay, PathIdempotentReplay, nil},
		{PathIdempotentReplay, PathSemanticReplay, nil},
	} {
		t.Run(string(testCase.built)+" as "+string(testCase.compiled), func(t *testing.T) {
			manifest := compiledTestManifest(t, testCase.built, testCase.targets...)
			if _, err := compileTest(t, testCase.compiled, manifest); err == nil {
				t.Fatalf("a %s manifest compiled for %s", testCase.built, testCase.compiled)
			}
		})
	}
}

// A target belonging to another workload must not compile, even though it is a
// perfectly well-formed target entry and changes no class count.
func TestAnotherWorkloadsTargetIsRefused(t *testing.T) {
	foreign, err := TargetEntry(V3TargetedVisible,
		`SELECT expense_id FROM reporting.expense_detail WHERE expense_id <= $1`,
		"artifact/expense-detail/100x4")
	if err != nil {
		t.Fatalf("foreign target: %v", err)
	}
	companion := pairedTargets(t)[1]
	_, err = compileTest(t, PathPairedNovel, compiledTestManifest(t, PathPairedNovel, foreign, companion))
	if err == nil {
		t.Fatal("another workload's target compiled into this operation's classifier")
	}
	if !strings.Contains(err.Error(), "the operation requires") {
		t.Fatalf("error %q does not name the contract binding", err)
	}
}

// Internal keys must come from the qualification the plan was derived under, and
// the operation must be bound to that same qualification.
func TestInternalKeysFromAnotherQualificationAreRefused(t *testing.T) {
	other := footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  1,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		AttestationScopePairedQueryTransaction: 5,
	})

	// A footprint the plan was not derived under cannot build its manifest at
	// all: the internal keys would be measured by one qualification and expected
	// by another.
	if _, err := BuildClassifierManifestV2(testPlan(t, PathPairedNovel), &other, pairedTargets(t)); err == nil {
		t.Fatal("a manifest was built from a qualification the plan was not derived under")
	}

	// And a manifest that IS internally coherent with its own plan cannot be
	// compiled for an operation bound to a different qualification.
	otherPlan := testPlanFrom(t, PathPairedNovel, other)
	manifest := buildTestManifest(t, otherPlan, other, pairedTargets(t)...)
	if _, err := CompileClassifierV2(testOperation(t, PathPairedNovel), otherPlan, manifest); err == nil {
		t.Fatal("an operation compiled against a plan from another qualification")
	}
}

// The binding digest must move with every dimension of the operation, so a
// classifier cannot be presented for a different one.
func TestBindingDigestCoversTheWholeOperation(t *testing.T) {
	operation := testOperation(t, PathPairedNovel)
	plan := testPlan(t, PathPairedNovel)
	manifest := compiledTestManifest(t, PathPairedNovel, pairedTargets(t)...)
	classifier, err := CompileClassifierV2(operation, plan, manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	planDigest, err := plan.SHA256()
	if err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	manifestDigest, err := manifest.SHA256()
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	if err := classifier.RequireBinding(operation, planDigest, manifestDigest); err != nil {
		t.Fatalf("a classifier must satisfy its own binding: %v", err)
	}

	for name, mutate := range map[string]func(*OperationIdentity){
		"operation id":     func(o *OperationIdentity) { o.OperationID = "operation-0002" },
		"path kind":        func(o *OperationIdentity) { o.PathKind = PathSingleQuery },
		"contract":         func(o *OperationIdentity) { o.ContractIdentity = "artifact/expense-detail/100x4" },
		"ExpectedSchema":   func(o *OperationIdentity) { o.ExpectedSchemaDigest = strings.Repeat("a", 64) },
		"footprint digest": func(o *OperationIdentity) { o.AttestationFootprintSHA256 = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := operation
			mutate(&mutated)
			if err := classifier.RequireBinding(mutated, planDigest, manifestDigest); err == nil {
				t.Fatalf("the binding ignored a changed %s", name)
			}
		})
	}

	// A different manifest under the same operation and plan.
	if err := classifier.RequireBinding(operation, planDigest, strings.Repeat("c", 64)); err == nil {
		t.Fatal("the binding ignored a substituted manifest")
	}
	// And a different plan under the same operation and manifest, which is what
	// the v2 binding adds: the class set is the plan's to settle, so a
	// substituted plan must not go unnoticed.
	if err := classifier.RequireBinding(operation, strings.Repeat("e", 64), manifestDigest); err == nil {
		t.Fatal("the binding ignored a substituted control plan")
	}
	other, err := testPlanFrom(t, PathPairedNovel, footprintFor(t, 2)).SHA256()
	if err != nil {
		t.Fatalf("another plan digest: %v", err)
	}
	if err := classifier.RequireBinding(operation, other, manifestDigest); err == nil {
		t.Fatal("the binding ignored a plan derived under another ExpectedSchema")
	}
}

// Compilation freezes the table. A manifest mutated after compilation must not
// change what the compiled classifier accepts.
func TestCompilationFreezesTheTable(t *testing.T) {
	manifest := compiledTestManifest(t, PathPairedNovel, pairedTargets(t)...)
	classifier, err := compileTest(t, PathPairedNovel, manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	smuggled := strings.Repeat("d", 64)
	if got := classifier.Classify(smuggled, true); got != V3Unexpected {
		t.Fatalf("precondition: %s already classifies as %s", smuggled[:8], got)
	}
	manifest.Entries = append(manifest.Entries, ClassifierEntry{
		Class: V3TargetedVisible, StrictASTSHA256: smuggled, RequiredTopLevel: true,
		SourceKind: SourceQueryContract, ContractIdentity: testOperationContract + "#visible",
	})
	if got := classifier.Classify(smuggled, true); got != V3Unexpected {
		t.Fatal("an entry appended after compilation took effect")
	}
}

func TestOperationIdentityPresenceCoupling(t *testing.T) {
	attesting := testOperation(t, PathPairedNovel)
	attesting.AttestationFootprintSHA256 = ""
	if err := attesting.Validate(); err == nil {
		t.Fatal("an attesting operation with no qualified footprint was accepted")
	}
	attesting = testOperation(t, PathPairedNovel)
	attesting.ExpectedSchemaDigest = ""
	if err := attesting.Validate(); err == nil {
		t.Fatal("an attesting operation with no ExpectedSchema was accepted")
	}

	replay := testOperation(t, PathIdempotentReplay)
	if err := replay.Validate(); err != nil {
		t.Fatalf("an idempotent replay operation was rejected: %v", err)
	}
	replay.ExpectedSchemaDigest = schemaDigestFor(1)
	if err := replay.Validate(); err == nil {
		t.Fatal("a non-attesting operation was allowed to carry an ExpectedSchema")
	}

	for name, mutate := range map[string]func(*OperationIdentity){
		"no operation id": func(o *OperationIdentity) { o.OperationID = "" },
		"no contract":     func(o *OperationIdentity) { o.ContractIdentity = "" },
		"unknown path":    func(o *OperationIdentity) { o.PathKind = "paired_novel_v2" },
	} {
		t.Run(name, func(t *testing.T) {
			identity := testOperation(t, PathPairedNovel)
			mutate(&identity)
			if err := identity.Validate(); err == nil {
				t.Fatal("an incomplete operation identity was accepted")
			}
		})
	}
}

// The compiled internal keys must be exactly the qualification's, so the
// finalizer can compare them.
func TestCompiledInternalKeysMatchTheQualification(t *testing.T) {
	footprint := qualifiedFootprint(t)
	classifier, err := compileTest(t, PathPairedNovel, compiledTestManifest(t, PathPairedNovel, pairedTargets(t)...))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	compiled := strings.Join(classifier.InternalKeys(), ",")
	qualified := strings.Join(footprint.InternalKeys(), ",")
	if compiled != qualified {
		t.Fatalf("compiled internal keys %q do not match the qualification %q", compiled, qualified)
	}
}
