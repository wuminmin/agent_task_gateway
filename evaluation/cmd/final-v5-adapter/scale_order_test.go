package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// A dependency history prefill mutates the task root and executes Business SQL.
// The committed exposure-scale profile is intentionally unroutable, so the v3
// finalizer must resolve and reject that profile before prefill can run. Task
// provisioning is the one permitted predecessor because the observer ticket is
// bound to the resulting task ID. The prefill then remains outside the observer
// interval, before the first V2 snapshot.
func TestDependencyE2EPreregistersBeforeHistoryPrefill(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the Adapter source")
	}
	scalePath := filepath.Join(filepath.Dir(testFile), "scale.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, scalePath, nil, 0)
	if err != nil {
		t.Fatalf("parse scale.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "executeDependencyE2E" {
			body = function.Body
			break
		}
	}
	if body == nil {
		t.Fatal("scale.go has no executeDependencyE2E implementation")
	}

	calls := map[string][]token.Pos{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch function := call.Fun.(type) {
		case *ast.Ident:
			name = function.Name
		case *ast.SelectorExpr:
			name = function.Sel.Name
		}
		if name != "" {
			calls[name] = append(calls[name], call.Pos())
		}
		return true
	})

	requireCalls := func(name string, count int) []token.Pos {
		t.Helper()
		positions := calls[name]
		if len(positions) != count {
			t.Fatalf("executeDependencyE2E calls %s %d times, want exactly %d", name, len(positions), count)
		}
		return positions
	}
	provision := requireCalls("provisionBoundTask", 1)[0]
	open := requireCalls("OpenObserverWindowV3", 1)[0]
	prefill := requireCalls("prefillDependencyHistory", 1)[0]
	captures := requireCalls("captureBoundObserverV2", 2)
	query := requireCalls("call", 1)[0]

	// Merely placing prefill text after Open is insufficient: the Open error must
	// be checked first, otherwise an unroutable profile could still fall through
	// into the mutation. Require an intervening `if err != nil { return ... }`.
	var openErrorGuard token.Pos
	ast.Inspect(body, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok || statement.Pos() <= open || statement.Pos() >= prefill {
			return true
		}
		binary, ok := statement.Cond.(*ast.BinaryExpr)
		if !ok || binary.Op != token.NEQ {
			return true
		}
		identifier, ok := binary.X.(*ast.Ident)
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
		t.Fatal("OpenObserverWindowV3 is not fail-closed before history prefill")
	}
	if !(provision < open && open < openErrorGuard && openErrorGuard < prefill &&
		prefill < captures[0] && captures[0] < query && query < captures[1]) {
		t.Fatalf("unsafe dependency order: provision=%d open=%d guard=%d prefill=%d captures=%v query=%d",
			provision, open, openErrorGuard, prefill, captures, query)
	}
}
