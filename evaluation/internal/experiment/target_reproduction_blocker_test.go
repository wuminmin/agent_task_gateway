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

// The v3 runtime cutover is BLOCKED, and this file pins the reason.
//
// Gate 22 is resolved, so the classifier no longer stands in the way. What does
// is a prerequisite that has never existed: the finalizer cannot independently
// reproduce the physical target statements a governed operation executed.
//
// FinalizeObservationV3 needs them for two separate things on every executing
// path:
//
//   - deriveTargets builds the manifest's target entries from them. Without
//     them the classifier has no visible or companion key, so gates 18, 19 and
//     20 -- "a wrong target", "another workload's target" -- have nothing to
//     compare against;
//   - requireStatementIdentities compares them against the Gateway's signed
//     execution binding. That comparison is the whole point: it is what catches
//     a receipt re-sealed around a different statement.
//
// Only three parties could supply them today:
//
//  1. the Gateway, which is the party being checked;
//  2. the Adapter, which TestAdapterCannotConstructTrustedInputs exists to
//     forbid -- an Adapter that supplied the target SQL would be supplying the
//     material its own claim is checked against;
//  3. a reimplementation in the evaluation tree, which is exactly what the
//     internal/physicalquery package doc forbids: "if the evaluation
//     reimplemented the derivation, the two would drift and the drift would look
//     like a measurement result".
//
// The derivation itself is unexported glue in internal/gateway. queryplan
// exports CompileOrdinal and CompileRelational, and physicalquery exports
// Derive, but the step BETWEEN them -- agent SQL plus Catalog products to an
// exposure plan to the compiled visible and companion statements -- lives in
// planExposureContext and buildRelationalExposureContext, which no other package
// can reach.
//
// The resolution is the same extraction that produced internal/physicalquery:
// lift that glue into a shared package both the Gateway and the finalizer call,
// so the finalizer reaches the same two statements from frozen contract material
// and signed pre-state rather than being told them. Until then the cutover
// cannot land without either weakening the Adapter guard or reimplementing the
// derivation, and both are the quiet relaxation this arc exists to prevent.
//
// See docs/final_v5_v3_runtime_integration_gates.md.

// gatewayExposureGlue names the unexported derivation the finalizer needs and
// cannot reach. Each is written out rather than discovered, so that renaming one
// fails here instead of silently emptying the check.
var gatewayExposureGlue = []string{
	"planExposureContext",
	"buildRelationalExposureContext",
	"derivePhysicalQuery",
}

// TestV3CutoverIsBlockedByTheUnsharedTargetDerivation fails the moment the
// derivation becomes shareable, which is when the real cutover gets written.
func TestV3CutoverIsBlockedByTheUnsharedTargetDerivation(t *testing.T) {
	// 1. The finalizer genuinely requires the reproduced statements. If this
	// stops being true the blocker is moot and the test must be rewritten.
	inputs := finalizerInputs(t)
	inputs.VisibleSQL, inputs.CompanionSQL = "", ""
	if _, err := deriveTargets(inputs, StrictASTDigest); err == nil {
		t.Fatal("the finalizer now derives paired-novel targets without reproduced statements; " +
			"the cutover blocker may be resolved -- write the real cutover")
	}

	// 2. And it has no way to obtain them: every step that turns agent SQL into
	// the compiled visible/companion pair is unexported inside internal/gateway.
	exported := exportedIdentifiers(t, filepath.Join(repositoryRoot(t), "internal", "gateway"))
	for _, name := range gatewayExposureGlue {
		if exported[name] {
			t.Fatalf("internal/gateway now exports %q; the target derivation may be shareable "+
				"-- write the real cutover and delete this blocker", name)
		}
		if !exported["!declared:"+name] {
			t.Fatalf("internal/gateway no longer declares %q; this blocker names a derivation "+
				"that has moved, and must be rewritten against wherever it went", name)
		}
	}

	// 3. Nothing in the active tree constructs the finalizer's trusted inputs,
	// which is the observable consequence: acceptance is reachable only from
	// tests.
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
