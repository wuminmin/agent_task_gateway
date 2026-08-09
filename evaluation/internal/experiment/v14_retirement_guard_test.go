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

// The v1.4 accounting is retired from the active runtime. All three TaskGate
// call sites emit CarriedEvidenceV3 and reach acceptance through
// FinalizeTaskGateObservationV3; its historical schema and decoder live only in
// the import-isolated legacyv14 package.
//
// This remains a ratchet after reaching zero. A new reference to a retired
// symbol fails the build, and the explicit empty shape prevents a future change
// from reintroducing an allowance as though the migration were still underway.
//
// The canary prerequisite is that this set is EMPTY. See
// docs/final_v5_v3_runtime_integration_gates.md.
var retiredV14ActiveReferences = map[string][]string{}

// retiredV14Symbols is the closed set of v1.4 accounting identifiers that must
// not survive into the v3 runtime. It is written out rather than derived from
// the package, because deriving it from the package would make the guard agree
// with whatever the package currently declares.
var retiredV14Symbols = map[string]bool{
	"ObserverAccounting":         true,
	"ObserverAccountingVersion":  true,
	"DecodeObserverAccounting":   true,
	"ValidateObserverAccounting": true,
	"GatewayControlPlan":         true,
	"NewGatewayControlPlan":      true,
	"GatewayStatementCensus":     true,
	"NewGatewayStatementCensus":  true,
	"GatewayStatementClass":      true,
	"GatewayStatementClasses":    true,
	"ClassifyGatewayStatement":   true,
	"CensusFromTemplates":        true,
	"ObserverSnapshot":           true,
	"ObserverDelta":              true,
	"RunObserver":                true,
	"DecodeObserverSnapshot":     true,
	"DifferenceObserver":         true,
}

func TestP24V14RetirementRatchetIsEmpty(t *testing.T) {
	if len(retiredV14ActiveReferences) != 0 {
		t.Fatalf("P2.4 requires an empty v1.4 active-reference set, got %v", retiredV14ActiveReferences)
	}
}

const legacyV14ImportPath = "taskbound.local/agent-data-gateway/evaluation/internal/legacyv14"

// The archived decoder is not a compatibility fallback. Test files may import
// it to prove rejection, but no binary built from a production file may reach
// the legacy schema or its historical validation rules.
func TestNoProductionPackageImportsLegacyV14(t *testing.T) {
	root := repositoryRoot(t)
	for _, path := range activeGoFiles(t, root) {
		if importsPackage(t, path, legacyV14ImportPath) {
			relative, _ := filepath.Rel(root, path)
			t.Errorf("production file %s imports legacyv14; the archived v1.4 decoder must not be a runtime fallback",
				filepath.ToSlash(relative))
		}
	}
}

// legacyv14 is a current-module-free leaf. In particular it cannot import the
// current Sample/finalizer runtime and grow a conversion that accepts or emits
// current evidence under the historical schema.
func TestLegacyV14HasNoModuleDependencies(t *testing.T) {
	root := repositoryRoot(t)
	directory := filepath.Join(root, "evaluation", "internal", "legacyv14")
	files := 0
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imported := range parsed.Imports {
			name := strings.Trim(imported.Path.Value, `"`)
			if strings.HasPrefix(name, "taskbound.local/agent-data-gateway/") {
				relative, _ := filepath.Rel(root, path)
				t.Errorf("legacy file %s imports current module package %s",
					filepath.ToSlash(relative), name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("legacyv14 contains no archived production decoder")
	}
}

func TestObserverRuntimeSourceClosureExcludesLegacyV14(t *testing.T) {
	foundInvocation := false
	for _, source := range observerRequiredSources {
		if strings.Contains(filepath.ToSlash(source), "/legacyv14/") ||
			source == "evaluation/internal/experiment/observer.go" {
			t.Errorf("v1.5 observer source closure retains legacy source %s", source)
		}
		if source == "evaluation/internal/experiment/observer_invocation_v3.go" {
			foundInvocation = true
		}
	}
	if !foundInvocation {
		t.Fatal("v1.5 observer source closure omits observer_invocation_v3.go")
	}
}

func importsPackage(t *testing.T, path, importPath string) bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse imports in %s: %v", path, err)
	}
	for _, imported := range parsed.Imports {
		if strings.Trim(imported.Path.Value, `"`) == importPath {
			return true
		}
	}
	return false
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

	observed := map[string][]string{}
	for _, path := range activeGoFiles(t, root) {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path for %s: %v", path, err)
		}
		relative = filepath.ToSlash(relative)
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
		t.Log("the v1.4 active surface remains empty; the canary prerequisite on this ratchet is satisfied")
	}
}

// TestFinalizeObservationV3HasProductionCallers records the other half of the
// canary prerequisite: acceptance must actually be reached. All three source
// callers now exist. Artifact and Scale remain hard requirements here; the
// ProvSQL branch stays report-only in this generic guard until P2.5 promotes it,
// while its path-specific cutover guard already fails on a missing call.
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

	requiredNow := []string{
		"evaluation/cmd/final-v5-adapter/artifact.go",
		"evaluation/cmd/final-v5-adapter/scale.go",
	}
	for _, path := range requiredNow {
		if !callers[path] {
			t.Errorf("the v3 acceptance entry point has no production caller in %s", path)
		}
	}
	provSQL := "evaluation/cmd/final-v5-adapter/provsql.go"
	if !callers[provSQL] {
		// The ProvSQL path-specific cutover guard already fails on a missing
		// call. This generic report remains transitional until P2.5 promotes it
		// after the v1.4 schema and construction surface are gone.
		t.Logf("the v3 acceptance entry point has no production caller in %s; "+
			"the generic canary prerequisite remains report-only until P2.5", provSQL)
	}
}

func TestScaleV3CutoverHasProductionCaller(t *testing.T) {
	path := filepath.Join(repositoryRoot(t), "evaluation", "cmd", "final-v5-adapter", "scale.go")
	calls := functionCallCounts(t, path, "executeDependencyE2E")

	required := map[string]int{
		"OpenObserverWindowV3":          1,
		"captureBoundObserverV2":        2,
		"FinalizeTaskGateObservationV3": 1,
	}
	for name, want := range required {
		if got := calls[name]; got != want {
			t.Errorf("executeDependencyE2E calls %s %d times, want exactly %d", name, got, want)
		}
	}

	for _, name := range []string{"captureBoundObserver", "NewGatewayControlPlan", "applyObserverDelta"} {
		if got := calls[name]; got != 0 {
			t.Errorf("executeDependencyE2E still calls retired v1 accounting entry point %s %d times", name, got)
		}
	}
}

func functionCallCounts(t *testing.T, path, function string) map[string]int {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var body *ast.BlockStmt
	for _, declaration := range parsed.Decls {
		functionDeclaration, ok := declaration.(*ast.FuncDecl)
		if ok && functionDeclaration.Name.Name == function {
			body = functionDeclaration.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("%s has no function %s", path, function)
	}

	calls := map[string]int{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch called := call.Fun.(type) {
		case *ast.Ident:
			calls[called.Name]++
		case *ast.SelectorExpr:
			calls[called.Sel.Name]++
		}
		return true
	})
	return calls
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
