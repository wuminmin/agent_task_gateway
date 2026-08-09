package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func parseProvSQLAdapter(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the Adapter source")
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, filepath.Join(filepath.Dir(testFile), "provsql.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse provsql.go: %v", err)
	}
	return fileSet, parsed
}

func provSQLFunction(t *testing.T, parsed *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("provsql.go has no function %s", name)
	return nil
}

func callsInProvSQLFunction(function *ast.FuncDecl) map[string][]token.Pos {
	calls := map[string][]token.Pos{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch called := call.Fun.(type) {
		case *ast.Ident:
			name = called.Name
		case *ast.SelectorExpr:
			name = called.Sel.Name
		}
		if name != "" {
			calls[name] = append(calls[name], call.Pos())
		}
		return true
	})
	for _, positions := range calls {
		sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
	}
	return calls
}

func TestProvSQLV3CutoverHasExactProductionCallShape(t *testing.T) {
	_, parsed := parseProvSQLAdapter(t)
	calls := callsInProvSQLFunction(provSQLFunction(t, parsed, "executeProvSQLTaskGate"))
	for name, want := range map[string]int{
		"OpenObserverWindowV3": 1, "captureBoundObserverV2": 2,
		"FinalizeTaskGateObservationV3": 1,
	} {
		if got := len(calls[name]); got != want {
			t.Errorf("executeProvSQLTaskGate calls %s %d times, want exactly %d", name, got, want)
		}
	}
	for _, name := range []string{
		"captureBoundObserver", "gatewayStatementCensus", "NewGatewayControlPlan", "applyObserverDelta",
	} {
		if got := len(calls[name]); got != 0 {
			t.Errorf("executeProvSQLTaskGate still calls retired v1 accounting entry point %s %d times", name, got)
		}
	}
}

func TestProvSQLV3CutoverIsTaskGateOnly(t *testing.T) {
	_, parsed := parseProvSQLAdapter(t)
	wantOwners := map[string]map[string]bool{
		"OpenDeploymentFinalizerV3":     {"provSQLFinalizer": true},
		"provSQLFinalizer":              {"executeProvSQLTaskGate": true},
		"OpenObserverWindowV3":          {"executeProvSQLTaskGate": true},
		"captureBoundObserverV2":        {"executeProvSQLTaskGate": true},
		"FinalizeTaskGateObservationV3": {"executeProvSQLTaskGate": true},
	}
	seen := map[string]map[string]int{}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		for called, positions := range callsInProvSQLFunction(function) {
			owners, sensitive := wantOwners[called]
			if !sensitive || len(positions) == 0 {
				continue
			}
			if !owners[function.Name.Name] {
				t.Errorf("ProvSQL v3 entry point %s is reachable from unexpected function %s", called, function.Name.Name)
			}
			if seen[called] == nil {
				seen[called] = map[string]int{}
			}
			seen[called][function.Name.Name] += len(positions)
		}
	}
	for called, owners := range wantOwners {
		for owner := range owners {
			if seen[called][owner] == 0 {
				t.Errorf("expected %s to call %s", owner, called)
			}
		}
	}
}

func TestProvSQLTaskGatePreregistersAndMeasuresInOrder(t *testing.T) {
	_, parsed := parseProvSQLAdapter(t)
	function := provSQLFunction(t, parsed, "executeProvSQLTaskGate")
	calls := callsInProvSQLFunction(function)
	require := func(name string, count int) []token.Pos {
		t.Helper()
		positions := calls[name]
		if len(positions) != count {
			t.Fatalf("executeProvSQLTaskGate calls %s %d times, want exactly %d", name, len(positions), count)
		}
		return positions
	}

	provision := require("provisionBoundTask", 1)[0]
	openFinalizer := require("provSQLFinalizer", 1)[0]
	selector := require("provSQLContractSelector", 1)[0]
	openWindow := require("OpenObserverWindowV3", 1)[0]
	roots := require("rootLedgerSnapshot", 2)
	business := require("businessSQLSnapshotFor", 2)
	captures := require("captureBoundObserverV2", 2)
	query := require("call", 1)[0]
	complete := require("completeTaskgateSample", 1)[0]
	resource := require("ResourceDelta", 1)[0]
	validateResult := require("validateBoundSampleResult", 1)[0]
	carried := require("carriedProvSQLEvidence", 1)[0]
	finalize := require("FinalizeTaskGateObservationV3", 1)[0]

	var requestID token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, expression := range assignment.Lhs {
			identifier, ok := expression.(*ast.Ident)
			if ok && identifier.Name == "requestID" {
				requestID = assignment.Pos()
			}
		}
		return true
	})
	if requestID == token.NoPos {
		t.Fatal("executeProvSQLTaskGate does not bind its request ID before pre-registration")
	}

	var openErrorGuard token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok || statement.Pos() <= openWindow || statement.Pos() >= roots[0] {
			return true
		}
		condition, ok := statement.Cond.(*ast.BinaryExpr)
		if !ok || condition.Op != token.NEQ {
			return true
		}
		identifier, ok := condition.X.(*ast.Ident)
		if !ok || identifier.Name != "err" {
			return true
		}
		hasReturn := false
		ast.Inspect(statement.Body, func(child ast.Node) bool {
			if _, ok := child.(*ast.ReturnStmt); ok {
				hasReturn = true
			}
			return true
		})
		if hasReturn {
			openErrorGuard = statement.Pos()
		}
		return true
	})
	if openErrorGuard == token.NoPos {
		t.Fatal("OpenObserverWindowV3 is not fail-closed before measurement")
	}

	if !(provision < requestID && requestID < openFinalizer && openFinalizer < selector && selector < openWindow &&
		openWindow < openErrorGuard && openErrorGuard < roots[0] && roots[0] < business[0] &&
		business[0] < captures[0] && captures[0] < query && query < business[1] && business[1] < roots[1] &&
		roots[1] < complete && complete < captures[1] && captures[1] < resource && resource < validateResult &&
		validateResult < carried && carried < finalize) {
		t.Fatalf("unsafe ProvSQL order: provision=%d request=%d finalizer=%d selector=%d open=%d guard=%d roots=%v business=%v captures=%v query=%d complete=%d resource=%d validate=%d carried=%d finalize=%d",
			provision, requestID, openFinalizer, selector, openWindow, openErrorGuard, roots, business, captures,
			query, complete, resource, validateResult, carried, finalize)
	}
}
