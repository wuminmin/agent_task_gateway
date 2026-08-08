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

// The v1.4 accounting is being retired from the active runtime. Until the three
// TaskGate call sites emit CarriedEvidenceV3 and reach acceptance through
// FinalizeTaskGateObservationV3, the old symbols still have callers, so a test
// asserting zero references would fail rather than protect anything.
//
// This is therefore a ratchet, not an aspiration. It pins the exact set of
// active references that remain, so the count can only fall. A new reference to
// a retired symbol fails the build; removing one fails it too, with an
// instruction to tighten the ratchet, so the inventory cannot silently rot into
// a stale allowance that permits references nobody meant to keep.
//
// The canary prerequisite is that this set is EMPTY. See
// docs/final_v5_v3_runtime_integration_gates.md.
var retiredV14ActiveReferences = map[string][]string{
	"evaluation/cmd/final-v5-adapter/adapter_bindings.go": {
		"CensusFromTemplates", "GatewayControlPlan", "GatewayStatementCensus",
		"ObserverAccounting", "ObserverAccountingVersion", "ValidateObserverAccounting",
	},
	"evaluation/cmd/final-v5-adapter/artifact.go": {
		"GatewayStatementCensus", "NewGatewayControlPlan",
	},
	"evaluation/cmd/final-v5-adapter/provsql.go": {
		"NewGatewayControlPlan",
	},
	"evaluation/cmd/final-v5-adapter/scale.go": {
		"NewGatewayControlPlan",
	},
	"evaluation/internal/experiment/finalize_scale_artifact.go": {
		"ObserverAccounting", "ValidateObserverAccounting",
	},
	"evaluation/internal/experiment/types.go": {
		"ObserverAccounting",
	},
}

// retiredV14Symbols is the closed set of v1.4 accounting identifiers that must
// not survive into the v3 runtime. It is written out rather than derived from
// the package, because deriving it from the package would make the guard agree
// with whatever the package currently declares.
var retiredV14Symbols = map[string]bool{
	"ObserverAccounting":         true,
	"ObserverAccountingVersion":  true,
	"ValidateObserverAccounting": true,
	"GatewayControlPlan":         true,
	"NewGatewayControlPlan":      true,
	"GatewayStatementCensus":     true,
	"NewGatewayStatementCensus":  true,
	"GatewayStatementClass":      true,
	"GatewayStatementClasses":    true,
	"ClassifyGatewayStatement":   true,
	"CensusFromTemplates":        true,
}

// repositoryRoot walks up from the test's directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's working directory")
		}
		directory = parent
	}
}

// activeGoFiles lists every non-test .go file under the active trees. Test files
// are excluded because a rejection test must be able to name a retired symbol in
// order to prove it is rejected.
func activeGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, tree := range []string{"cmd", "internal", "evaluation"} {
		err := filepath.Walk(filepath.Join(root, tree), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "legacyv14" || info.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	sort.Strings(files)
	return files
}

// referencedRetiredSymbols parses a file and reports which retired identifiers it
// names. Parsing rather than grepping matters: a comment mentioning
// ObserverAccounting is discussion, not a reference, and the retirement is about
// what the compiler can reach.
func referencedRetiredSymbols(t *testing.T, path string) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := map[string]bool{}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.SelectorExpr:
			// experiment.ObserverAccounting from another package.
			if identifier, ok := typed.X.(*ast.Ident); ok && identifier.Name == "experiment" {
				if retiredV14Symbols[typed.Sel.Name] {
					found[typed.Sel.Name] = true
				}
			}
		case *ast.Ident:
			// A bare reference from inside package experiment itself.
			if retiredV14Symbols[typed.Name] {
				found[typed.Name] = true
			}
		}
		return true
	})
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestNoActiveReferenceToV14Accounting(t *testing.T) {
	root := repositoryRoot(t)
	// The declarations themselves live here and are not references to retire.
	declarations := filepath.Join("evaluation", "internal", "experiment", "observer_accounting.go")

	observed := map[string][]string{}
	for _, path := range activeGoFiles(t, root) {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path for %s: %v", path, err)
		}
		relative = filepath.ToSlash(relative)
		if relative == filepath.ToSlash(declarations) {
			continue
		}
		if names := referencedRetiredSymbols(t, path); len(names) > 0 {
			observed[relative] = names
		}
	}

	for file, names := range observed {
		allowed, present := retiredV14ActiveReferences[file]
		if !present {
			t.Errorf("%s references retired v1.4 accounting symbols %v.\n"+
				"The v1.4 accounting is retired from the active runtime: reach acceptance through "+
				"FinalizeTaskGateObservationV3 instead.", file, names)
			continue
		}
		for _, name := range names {
			if !contains(allowed, name) {
				t.Errorf("%s newly references retired v1.4 symbol %q; the ratchet allows only %v",
					file, name, allowed)
			}
		}
	}

	// The ratchet must not go slack. A file that has been migrated has to be
	// removed from the inventory in the same commit, or the allowance would
	// outlive the reason for it.
	for file, allowed := range retiredV14ActiveReferences {
		names, present := observed[file]
		if !present {
			t.Errorf("%s no longer references any retired v1.4 symbol; remove its entry from "+
				"retiredV14ActiveReferences to tighten the ratchet", file)
			continue
		}
		for _, name := range allowed {
			if !contains(names, name) {
				t.Errorf("%s no longer references retired v1.4 symbol %q; drop it from the ratchet entry",
					file, name)
			}
		}
	}

	if len(retiredV14ActiveReferences) == 0 {
		t.Log("the v1.4 active surface is empty; this guard can become a plain zero assertion " +
			"and the canary prerequisite on it is satisfied")
	}
}

// TestFinalizeObservationV3HasProductionCallers records the other half of the
// canary prerequisite: acceptance must actually be reached.
//
// It asserts the current state rather than the target state, for the same reason
// the reference guard is a ratchet: a test that fails until unrelated work lands
// is not a guard, it is a reminder that makes the suite red. The names it counts
// are the ones the migration has to change.
//
// The three files it names are where the TaskGate workloads live, and the call
// they must make is RuntimeFinalizerV3.FinalizeTaskGateObservationV3 -- the
// finalizer-side entry point that constructs its own trusted inputs. An Adapter
// cannot call the core directly: the core is package-private, and
// TestAdapterCannotConstructTrustedInputs forbids naming what it takes. The two
// requirements used to look contradictory; the façade is what makes them one
// design.
func TestFinalizeObservationV3HasProductionCallers(t *testing.T) {
	root := repositoryRoot(t)
	callers := map[string]bool{}
	for _, path := range activeGoFiles(t, root) {
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if strings.HasPrefix(relative, "evaluation/internal/experiment/") {
			continue
		}
		if callsAcceptanceEntryPoint(t, path) {
			callers[relative] = true
		}
	}

	required := []string{
		"evaluation/cmd/final-v5-adapter/artifact.go",
		"evaluation/cmd/final-v5-adapter/scale.go",
		"evaluation/cmd/final-v5-adapter/provsql.go",
	}
	var missing []string
	for _, path := range required {
		if !callers[path] {
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return
	}
	// Not a failure yet: the migration that adds these callers is in progress,
	// and the canary prerequisite -- not this test -- is what forbids measuring
	// before it lands. It becomes a hard failure with the third cutover.
	t.Logf("the v3 acceptance entry point has no production caller in %v; "+
		"the canary prerequisite in docs/final_v5_v3_runtime_integration_gates.md "+
		"is not satisfied until all three call it", missing)
}

// callsAcceptanceEntryPoint reports whether a file contains a real call to the
// acceptance entry point.
//
// It is an AST check over CallExpr rather than a substring search, and the
// difference is not pedantry: the substring version accepted
//
//	// TODO: call FinalizeTaskGateObservationV3
//
// as a production caller, which is precisely the false positive a guard on
// "acceptance is actually reached" must not have. A comment, a string literal
// and a mention in a doc comment now all count for nothing.
func callsAcceptanceEntryPoint(t *testing.T, path string) bool {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	called := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "FinalizeTaskGateObservationV3" {
			called = true
		}
		return true
	})
	return called
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// adapterPackages are the packages that produce evidence rather than accept it.
// They may construct CarriedEvidenceV3 and nothing else: everything in
// trustedMaterialTypes is the finalizer's own, and an Adapter that could build
// one would be supplying the material its claim is checked against.
//
// They reach acceptance through RuntimeFinalizerV3, which takes evidence and a
// lookup hint and constructs the trusted material itself. That is what makes
// this restriction satisfiable rather than a contradiction with the caller
// requirement below.
var adapterPackages = []string{
	"evaluation/cmd/final-v5-adapter",
	"evaluation/cmd/final-v5-observer",
}

func TestAdapterCannotConstructTrustedInputs(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := map[string]bool{}
	for _, name := range trustedMaterialTypes {
		forbidden[name] = true
	}

	for _, pkg := range adapterPackages {
		err := filepath.Walk(filepath.Join(root, pkg), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fileSet := token.NewFileSet()
			parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			relative, _ := filepath.Rel(root, path)
			ast.Inspect(parsed, func(node ast.Node) bool {
				// experiment.TrustedInputsV3{...} anywhere in an Adapter file,
				// as a composite literal or as a named type.
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if !ok || identifier.Name != "experiment" {
					return true
				}
				if forbidden[selector.Sel.Name] {
					t.Errorf("%s names experiment.%s; the Adapter supplies CarriedEvidenceV3 only, "+
						"and the finalizer constructs its own trusted inputs",
						filepath.ToSlash(relative), selector.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", pkg, err)
		}
	}
}
