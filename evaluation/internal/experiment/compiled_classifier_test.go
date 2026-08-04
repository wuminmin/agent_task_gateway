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

func compiledTestManifest(t *testing.T, targets ...ClassifierEntry) ClassifierManifest {
	t.Helper()
	manifest, err := BuildClassifierManifest(qualifiedFootprint(t), targets)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return manifest
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
	classifier, err := CompileClassifier(testOperation(t, PathPairedNovel), compiledTestManifest(t, targets...))
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

// Target cardinality is path-specific. A manifest carrying a companion cannot
// classify for a path that issues none, even though every control entry matches.
func TestCompilationEnforcesPathSpecificTargetCardinality(t *testing.T) {
	paired := pairedTargets(t)
	visibleOnly := paired[:1]

	for _, testCase := range []struct {
		name    string
		kind    GatewayPathKind
		targets []ClassifierEntry
		wantErr string
	}{
		{"paired novel needs a companion", PathPairedNovel, visibleOnly, "companion target statements"},
		{"single query must not carry one", PathSingleQuery, paired, "companion target statements"},
		{"semantic replay executes no target", PathSemanticReplay, visibleOnly, "visible target statements"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := CompileClassifier(testOperation(t, testCase.kind), compiledTestManifest(t, testCase.targets...))
			if err == nil {
				t.Fatal("a manifest with the wrong target cardinality compiled")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not name the cardinality defect", err)
			}
		})
	}

	// The correct cardinalities must compile.
	if _, err := CompileClassifier(testOperation(t, PathSingleQuery), compiledTestManifest(t, visibleOnly...)); err != nil {
		t.Fatalf("single query with one visible target rejected: %v", err)
	}
	if _, err := CompileClassifier(testOperation(t, PathSemanticReplay), compiledTestManifest(t)); err != nil {
		t.Fatalf("semantic replay with no target rejected: %v", err)
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
	_, err = CompileClassifier(testOperation(t, PathPairedNovel), compiledTestManifest(t, foreign, companion))
	if err == nil {
		t.Fatal("another workload's target compiled into this operation's classifier")
	}
	if !strings.Contains(err.Error(), "the operation requires") {
		t.Fatalf("error %q does not name the contract binding", err)
	}
}

// Internal keys must come from the qualification the operation is bound to.
func TestInternalKeysFromAnotherQualificationAreRefused(t *testing.T) {
	// A manifest built from a different footprint carries a different
	// footprint digest on its internal entries.
	other := footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  1,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		AttestationScopePairedQueryTransaction: 5,
	})
	manifest, err := BuildClassifierManifest(other, pairedTargets(t))
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if _, err := CompileClassifier(testOperation(t, PathPairedNovel), manifest); err == nil {
		t.Fatal("a manifest from another qualification compiled")
	}
}

// The binding digest must move with every dimension of the operation, so a
// classifier cannot be presented for a different one.
func TestBindingDigestCoversTheWholeOperation(t *testing.T) {
	operation := testOperation(t, PathPairedNovel)
	manifest := compiledTestManifest(t, pairedTargets(t)...)
	classifier, err := CompileClassifier(operation, manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	manifestDigest, err := manifest.SHA256()
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	if err := classifier.RequireBinding(operation, manifestDigest); err != nil {
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
			if err := classifier.RequireBinding(mutated, manifestDigest); err == nil {
				t.Fatalf("the binding ignored a changed %s", name)
			}
		})
	}

	// And a different manifest under the same operation.
	if err := classifier.RequireBinding(operation, strings.Repeat("c", 64)); err == nil {
		t.Fatal("the binding ignored a substituted manifest")
	}
}

// Compilation freezes the table. A manifest mutated after compilation must not
// change what the compiled classifier accepts.
func TestCompilationFreezesTheTable(t *testing.T) {
	manifest := compiledTestManifest(t, pairedTargets(t)...)
	classifier, err := CompileClassifier(testOperation(t, PathPairedNovel), manifest)
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
	classifier, err := CompileClassifier(testOperation(t, PathPairedNovel), compiledTestManifest(t, pairedTargets(t)...))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	compiled := strings.Join(classifier.InternalKeys(), ",")
	qualified := strings.Join(footprint.InternalKeys(), ",")
	if compiled != qualified {
		t.Fatalf("compiled internal keys %q do not match the qualification %q", compiled, qualified)
	}
}
