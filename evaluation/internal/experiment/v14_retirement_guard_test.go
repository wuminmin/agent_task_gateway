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
	required := map[string]bool{
		"evaluation/internal/experiment/strict_ast.go": false,
		"internal/approval/protocol.go":                false,
		"internal/sqlidentity/strict_ast.go":           false,
	}
	seen := map[string]bool{}
	for _, source := range observerRequiredSources {
		if seen[source] {
			t.Errorf("observer source closure lists %s twice", source)
		}
		seen[source] = true
		if strings.Contains(filepath.ToSlash(source), "/legacyv14/") ||
			source == "evaluation/internal/experiment/observer.go" {
			t.Errorf("v1.5 observer source closure retains legacy source %s", source)
		}
		if source == "evaluation/internal/experiment/observer_invocation_v3.go" {
			foundInvocation = true
		}
		if _, present := required[source]; present {
			required[source] = true
		}
	}
	if !foundInvocation {
		t.Fatal("v1.5 observer source closure omits observer_invocation_v3.go")
	}
	for source, present := range required {
		if !present {
			t.Errorf("observer source closure omits %s", source)
		}
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

// TestFinalizeObservationV3HasProductionCallers pins the two finalizer-side
// edges that make acceptance reachable without letting an Adapter construct
// trusted inputs. The runtime façade must enter the package-private TaskGate
// core, and that core must call the acceptance implementation exactly once.
func TestFinalizeObservationV3HasProductionCallers(t *testing.T) {
	root := repositoryRoot(t)
	requireProductionSymbolClosure(t, root, "finalizeTaskGateObservationV3Core", productionDeclaration{
		path: "evaluation/internal/experiment/finalize_taskgate_v3.go",
	}, []productionCallSite{
		{
			path:             "evaluation/internal/experiment/runtime_finalizer_v3.go",
			function:         "FinalizeTaskGateObservationV3",
			functionReceiver: "*RuntimeFinalizerV3",
			callForm:         "identifier",
			// The façade binds the result so its one final fail-closed boundary
			// can attach a credential-free rejection to any future core error
			// that was not classified at its own gate. The closed call set is
			// unchanged: there is still exactly one private-core edge.
			statement:     "assignment",
			resultBinding: "finalized,err:=direct",
		},
	})
	requireProductionSymbolClosure(t, root, "FinalizeObservationV3", productionDeclaration{
		path: "evaluation/internal/experiment/finalize_observation_v3.go",
	}, []productionCallSite{
		{
			path:          "evaluation/internal/experiment/finalize_taskgate_v3.go",
			function:      "finalizeTaskGateObservationV3Core",
			callForm:      "identifier",
			statement:     "assignment",
			resultBinding: "finalized,err:=direct",
		},
	})
}

// TestRuntimeFinalizerV3HasTaskGateProductionCallers separately pins the three
// workload façades. Adapter code may submit carried evidence to the runtime
// finalizer, but the finalizer-side test above owns the trusted acceptance
// chain.
func TestRuntimeFinalizerV3HasTaskGateProductionCallers(t *testing.T) {
	root := repositoryRoot(t)
	requireProductionSymbolClosure(t, root, "FinalizeTaskGateObservationV3", productionDeclaration{
		path:             "evaluation/internal/experiment/runtime_finalizer_v3.go",
		functionReceiver: "*RuntimeFinalizerV3",
	}, []productionCallSite{
		{
			path:             "evaluation/cmd/final-v5-adapter/artifact.go",
			function:         "executeResultHeavy",
			functionReceiver: "*artifactAdapter",
			callForm:         "selector",
			callReceiver:     "adapter.finalizer",
			statement:        "assignment",
			resultBinding:    "finalized,err:=direct",
		},
		{
			path:             "evaluation/cmd/final-v5-adapter/scale.go",
			function:         "executeDependencyE2E",
			functionReceiver: "*scaleAdapter",
			callForm:         "selector",
			callReceiver:     "finalizer",
			statement:        "assignment",
			resultBinding:    "finalized,err:=direct",
		},
		{
			path:             "evaluation/cmd/final-v5-adapter/provsql.go",
			function:         "executeProvSQLTaskGate",
			functionReceiver: "*provSQLAdapter",
			callForm:         "selector",
			callReceiver:     "finalizer",
			statement:        "assignment",
			resultBinding:    "finalized,err:=direct",
		},
	})
	requireProductionSymbolClosure(t, root, "retainTaskGateRejection", productionDeclaration{
		path: "evaluation/cmd/final-v5-adapter/adapter_bindings.go",
	}, []productionCallSite{
		{
			path: "evaluation/cmd/final-v5-adapter/artifact.go", function: "Execute",
			functionReceiver: "*artifactAdapter", callForm: "identifier",
			statement: "nested-control", resultBinding: "nested-control",
		},
		{
			path: "evaluation/cmd/final-v5-adapter/scale.go", function: "Execute",
			functionReceiver: "*scaleAdapter", callForm: "identifier",
			statement: "nested-control", resultBinding: "nested-control",
		},
		{
			path: "evaluation/cmd/final-v5-adapter/provsql.go", function: "Execute",
			functionReceiver: "*provSQLAdapter", callForm: "identifier",
			statement: "nested-control", resultBinding: "nested-control",
		},
	})
}

type productionCallSite struct {
	path             string
	function         string
	functionReceiver string
	callForm         string
	callReceiver     string
	statement        string
	resultBinding    string
	functionLiteral  bool
}

type productionDeclaration struct {
	path             string
	functionReceiver string
}

type productionSymbolReference struct {
	path string
	line int
}

func requireProductionSymbolClosure(t *testing.T, root, symbol string, declaration productionDeclaration,
	wantCalls []productionCallSite) {
	t.Helper()
	requireExactProductionCallSites(t, root, symbol, wantCalls)
	declarations, indirect := productionSymbolSurface(t, root, symbol)
	if len(declarations) != 1 || declarations[0] != declaration {
		t.Fatalf("production declaration of %s is %+v, want exactly %+v", symbol, declarations, declaration)
	}
	if len(indirect) != 0 {
		t.Fatalf("production references to %s outside a direct guarded call are %+v", symbol, indirect)
	}
}

func requireExactProductionCallSites(t *testing.T, root, callee string, want []productionCallSite) {
	t.Helper()
	got := productionCallSites(t, root, callee)
	sort.Slice(got, func(left, right int) bool {
		return productionCallSiteKey(got[left]) < productionCallSiteKey(got[right])
	})
	sort.Slice(want, func(left, right int) bool {
		return productionCallSiteKey(want[left]) < productionCallSiteKey(want[right])
	})
	if len(got) != len(want) {
		t.Fatalf("production calls to %s are %+v, want exact closed set %+v", callee, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("production calls to %s are %+v, want exact closed set %+v", callee, got, want)
		}
	}
}

func productionCallSites(t *testing.T, root, callee string) []productionCallSite {
	t.Helper()
	var sites []productionCallSite
	for _, path := range activeGoFiles(t, root) {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path for %s: %v", path, err)
		}
		relative = filepath.ToSlash(relative)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		recordCalls := func(rootNode ast.Node, function, functionReceiver, statement string) {
			var ancestors []ast.Node
			ast.Inspect(rootNode, func(node ast.Node) bool {
				if node == nil {
					ancestors = ancestors[:len(ancestors)-1]
					return true
				}
				if call, ok := node.(*ast.CallExpr); ok {
					site := productionCallSite{
						path: relative, function: function, functionReceiver: functionReceiver,
						statement: statement, resultBinding: productionResultBinding(rootNode, call),
					}
					for _, ancestor := range ancestors {
						if _, nestedFunction := ancestor.(*ast.FuncLit); nestedFunction {
							site.functionLiteral = true
							break
						}
					}
					matched := false
					switch called := call.Fun.(type) {
					case *ast.Ident:
						if called.Name == callee {
							site.callForm = "identifier"
							matched = true
						}
					case *ast.SelectorExpr:
						if called.Sel.Name == callee {
							site.callForm = "selector"
							site.callReceiver = expressionPath(called.X)
							matched = true
						}
					}
					if matched {
						sites = append(sites, site)
					}
				}
				ancestors = append(ancestors, node)
				return true
			})
		}

		for _, declaration := range parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				if typed.Body == nil {
					continue
				}
				receiver := ""
				if typed.Recv != nil && len(typed.Recv.List) == 1 {
					receiver = expressionPath(typed.Recv.List[0].Type)
				}
				for _, statement := range typed.Body.List {
					recordCalls(statement, typed.Name.Name, receiver, productionStatementKind(statement))
				}
			case *ast.GenDecl:
				for _, specification := range typed.Specs {
					value, ok := specification.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, expression := range value.Values {
						recordCalls(expression, "<package>", "", "initializer")
					}
				}
			}
		}
	}
	return sites
}

func productionSymbolSurface(t *testing.T, root, symbol string) ([]productionDeclaration,
	[]productionSymbolReference) {
	t.Helper()
	var declarations []productionDeclaration
	var indirect []productionSymbolReference
	for _, path := range activeGoFiles(t, root) {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("relative path for %s: %v", path, err)
		}
		relative = filepath.ToSlash(relative)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var ancestors []ast.Node
		ast.Inspect(parsed, func(node ast.Node) bool {
			if node == nil {
				ancestors = ancestors[:len(ancestors)-1]
				return true
			}
			var parent ast.Node
			if len(ancestors) != 0 {
				parent = ancestors[len(ancestors)-1]
			}
			switch typed := node.(type) {
			case *ast.SelectorExpr:
				if typed.Sel.Name == symbol && !isDirectCallCallee(parent, typed) {
					indirect = append(indirect, productionSymbolReference{
						path: relative, line: fileSet.Position(typed.Sel.Pos()).Line,
					})
				}
			case *ast.Ident:
				if typed.Name != symbol {
					break
				}
				if selector, ok := parent.(*ast.SelectorExpr); ok && selector.Sel == typed {
					break
				}
				if function, ok := parent.(*ast.FuncDecl); ok && function.Name == typed {
					receiver := ""
					if function.Recv != nil && len(function.Recv.List) == 1 {
						receiver = expressionPath(function.Recv.List[0].Type)
					}
					declarations = append(declarations, productionDeclaration{
						path: relative, functionReceiver: receiver,
					})
					break
				}
				if !isDirectCallCallee(parent, typed) {
					indirect = append(indirect, productionSymbolReference{
						path: relative, line: fileSet.Position(typed.Pos()).Line,
					})
				}
			}
			ancestors = append(ancestors, node)
			return true
		})
	}
	sort.Slice(declarations, func(left, right int) bool {
		if declarations[left].path != declarations[right].path {
			return declarations[left].path < declarations[right].path
		}
		return declarations[left].functionReceiver < declarations[right].functionReceiver
	})
	sort.Slice(indirect, func(left, right int) bool {
		if indirect[left].path != indirect[right].path {
			return indirect[left].path < indirect[right].path
		}
		return indirect[left].line < indirect[right].line
	})
	return declarations, indirect
}

func isDirectCallCallee(parent ast.Node, expression ast.Expr) bool {
	call, ok := parent.(*ast.CallExpr)
	return ok && call.Fun == expression
}

func productionCallSiteKey(site productionCallSite) string {
	literal := "top-level"
	if site.functionLiteral {
		literal = "function-literal"
	}
	return strings.Join([]string{
		site.path, site.function, site.functionReceiver, site.callForm, site.callReceiver, site.statement,
		site.resultBinding, literal,
	}, "\x00")
}

func productionResultBinding(root ast.Node, call *ast.CallExpr) string {
	switch typed := root.(type) {
	case *ast.ReturnStmt:
		if len(typed.Results) == 1 && typed.Results[0] == call {
			return "return-direct"
		}
		return "return-nested"
	case *ast.AssignStmt:
		var targets []string
		for _, target := range typed.Lhs {
			targets = append(targets, expressionPath(target))
		}
		position := "nested"
		if len(typed.Rhs) == 1 && typed.Rhs[0] == call {
			position = "direct"
		}
		return strings.Join(targets, ",") + typed.Tok.String() + position
	case *ast.ExprStmt:
		if typed.X == call {
			return "expression-direct"
		}
		return "expression-nested"
	case *ast.DeferStmt:
		if typed.Call == call {
			return "defer-direct"
		}
		return "defer-nested"
	case *ast.GoStmt:
		if typed.Call == call {
			return "go-direct"
		}
		return "go-nested"
	default:
		if root == call {
			return "initializer-direct"
		}
		return "nested-control"
	}
}

func expressionPath(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		prefix := expressionPath(typed.X)
		if prefix == "<unsupported>" {
			return prefix
		}
		return prefix + "." + typed.Sel.Name
	case *ast.ParenExpr:
		return expressionPath(typed.X)
	case *ast.StarExpr:
		return "*" + expressionPath(typed.X)
	default:
		return "<unsupported>"
	}
}

func productionStatementKind(statement ast.Stmt) string {
	switch statement.(type) {
	case *ast.AssignStmt:
		return "assignment"
	case *ast.ReturnStmt:
		return "return"
	case *ast.ExprStmt:
		return "expression"
	case *ast.DeferStmt:
		return "defer"
	case *ast.GoStmt:
		return "go"
	default:
		return "nested-control"
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
