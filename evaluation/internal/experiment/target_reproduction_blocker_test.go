package experiment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The v3 runtime cutover is BLOCKED, and this file pins the reason. What the
// reason IS changed at T1d, and the change is recorded here rather than by
// deleting the blocker.
//
// FinalizeObservationV3 needs the physical target statements a governed
// operation executed, on every executing path:
//
//   - deriveTargets builds the manifest's target entries from them. Without
//     them the classifier has no visible or companion key, so gates 18, 19 and
//     20 -- "a wrong target", "another workload's target" -- have nothing to
//     compare against;
//   - requireStatementIdentities compares them against the Gateway's signed
//     execution binding. That comparison is the whole point: it is what catches
//     a receipt re-sealed around a different statement.
//
// # What was blocking, and is not any more
//
// Until T1d the derivation was unexported glue in internal/gateway --
// planExposureContext, buildRelationalExposureContext and the ordinal sidecar
// binder -- so only three parties could have supplied the statements: the
// Gateway, which is the party being checked; the Adapter, which
// TestAdapterCannotConstructTrustedInputs forbids; or a reimplementation in the
// evaluation tree, which the internal/physicalquery package doc forbids because
// "the two would drift and the drift would look like a measurement result".
//
// That is resolved. internal/physicalquery.Prepare and PrepareSemanticView are
// exported, derive both statements from immutable inputs, and are the ONLY
// implementation: the Gateway calls them on its production path and its own
// derivation is deleted. A finalizer holding frozen contract material can now
// reach the same two statements without asking the Gateway and without writing
// a second derivation.
//
// # What T1e delivered
//
// ReproduceExecutionV3 is the finalizer doing it. Given frozen contract material
// -- an activated Profile Catalog read as a file, a retained snapshot artifact
// directory, the contract's plan and approved surface -- it prepares through
// physicalquery.Prepare, requires the sealed result to equal the preparation the
// receipt signed, authorizes both statements against the receipt's own signed
// pre-state, and requires the derived row limits to be the signed ones. Its
// tests show every one of those inputs is load-bearing: change the projection,
// the ordering, the approved columns, the mandatory scope or the profile, and
// the reproduction stops agreeing with the signature.
//
// So "the statements the finalizer reproduced", which TrustedInputsV3 has
// asserted in a doc comment since it was written, is now a thing that happens.
//
// # What is still blocking
//
// The acceptance wrapper does not call it yet: FinalizeTaskGateObservationV3
// still takes VisibleSQL and CompanionSQL as strings, so the reproduction exists
// beside the acceptance path rather than inside it. And nothing in the active
// tree constructs TrustedInputsV3 or IndependentInputsV3, so acceptance remains
// reachable only from tests.
//
// Wiring the wrapper means the gate fixtures must seal their receipts around a
// REAL preparation rather than fixture digests -- otherwise the reproduction has
// nothing it can agree with -- which is the next contiguous piece of work.
//
// See docs/final_v5_v3_runtime_integration_gates.md.

// sharedTargetDerivation names what internal/physicalquery must keep exported
// for a finalizer to reproduce a preparation at all.
//
// Each is written out rather than discovered, so that hiding or renaming one
// fails here instead of silently emptying the check. Re-hiding any of them would
// put the cutover back behind the barrier T1d removed.
var sharedTargetDerivation = []string{
	"Prepare",
	"PrepareWith",
	"PrepareSemanticView",
	"PrepareSemanticViewWith",
	"PreparedOperation",
	"PreparationInputs",
	"SemanticViewPreparationInputsV1",
}

// gatewayDeletedDerivation names the Gateway-local derivation the extraction
// removed. None of them may come back: a second implementation in the running
// system is the outcome the whole arc exists to avoid, and it would be invisible
// to a differential that compares the surviving one with itself.
var gatewayDeletedDerivation = []string{
	"buildPlanExposureContext",
	"buildRelationalExposureContext",
	"bindOrdinalSidecars",
	"ordinalQueryProduct",
	"configureV2",
	"configurePredicateFootprintV5",
	"extendGrant",
	"extendOrdinalPolicyGrant",
}

// TestV3CutoverIsBlockedByTheUnwiredFinalizer fails the moment the finalizer is
// wired, which is when the real cutover gets written.
func TestV3CutoverIsBlockedByTheUnwiredFinalizer(t *testing.T) {
	// 1. The finalizer genuinely requires the reproduced statements. If this
	// stops being true the blocker is moot and the test must be rewritten.
	inputs := finalizerInputs(t)
	inputs.VisibleSQL, inputs.CompanionSQL = "", ""
	if _, err := deriveTargets(inputs, StrictASTDigest); err == nil {
		t.Fatal("the finalizer now derives paired-novel targets without reproduced statements; " +
			"the cutover blocker may be resolved -- write the real cutover")
	}

	// 2. The shared derivation exists and is reachable. This is the prerequisite
	// T1d delivered, asserted so a regression that re-hid it is reported as the
	// cutover becoming impossible again rather than as an unrelated failure.
	root := repositoryRoot(t)
	shared := exportedIdentifiers(t, filepath.Join(root, "internal", "physicalquery"))
	for _, name := range sharedTargetDerivation {
		if !shared[name] {
			t.Fatalf("internal/physicalquery no longer exports %q; the finalizer can no longer "+
				"reproduce a preparation, and the cutover is blocked on that again rather than "+
				"on being unwired", name)
		}
	}

	// 3. And the Gateway has not grown its own derivation back beside it.
	gateway := exportedIdentifiers(t, filepath.Join(root, "internal", "gateway"))
	for _, name := range gatewayDeletedDerivation {
		if gateway["!declared:"+name] {
			t.Fatalf("internal/gateway declares %q again; production would hold two derivations "+
				"of one statement, which is what the extraction removed", name)
		}
	}

	// 4. Nothing in the active tree constructs the finalizer's trusted inputs,
	// which is the observable consequence of the remaining gap: acceptance is
	// reachable only from tests.
	if callers := trustedInputConstructors(t); len(callers) > 0 {
		t.Fatalf("%v now construct the finalizer's trusted inputs; the cutover may be "+
			"unblocked -- write it and delete this blocker", callers)
	}
}

// exportedIdentifiers reports the exported top-level names a package declares,
// plus a "!declared:<name>" marker for every top-level name including unexported
// ones, so a caller can tell "unexported" from "gone".
func exportedIdentifiers(t *testing.T, directory string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				record(names, typed.Name.Name)
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						record(names, typeSpec.Name.Name)
					}
				}
			}
		}
	}
	return names
}

func record(names map[string]bool, name string) {
	names["!declared:"+name] = true
	if ast.IsExported(name) {
		names[name] = true
	}
}

// trustedInputConstructors lists active files that name the finalizer's own
// input types. A production caller of acceptance must name one of them.
func trustedInputConstructors(t *testing.T) []string {
	t.Helper()
	root := repositoryRoot(t)
	wanted := map[string]bool{"TrustedInputsV3": true, "IndependentInputsV3": true}
	var files []string
	for _, path := range activeGoFiles(t, root) {
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		// The declarations and the wrapper's own use of them are not callers.
		if relative == "evaluation/internal/experiment/finalize_observation_v3.go" ||
			relative == "evaluation/internal/experiment/finalize_taskgate_v3.go" {
			continue
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		found := false
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Name == "experiment" &&
				wanted[selector.Sel.Name] {
				found = true
			}
			return true
		})
		if found {
			files = append(files, relative)
		}
	}
	sort.Strings(files)
	return files
}
