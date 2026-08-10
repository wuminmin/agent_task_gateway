package snapshotbundle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestSnapshotScannerPinsSimpleProtocolAndRejectsPreparedStatements(t *testing.T) {
	value, err := os.ReadFile("postgres.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(value), "pg_prepared_statements") {
		t.Fatal("snapshot scanner no longer proves the session prepared-statement count")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "postgres.go", value, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	pinned := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.ASSIGN || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		left, leftOK := assignment.Lhs[0].(*ast.SelectorExpr)
		right, rightOK := assignment.Rhs[0].(*ast.SelectorExpr)
		if leftOK && rightOK && left.Sel.Name == "DefaultQueryExecMode" && right.Sel.Name == "QueryExecModeSimpleProtocol" {
			pinned = true
		}
		return true
	})
	if !pinned {
		t.Fatal("snapshot scanner no longer pins pgx.QueryExecModeSimpleProtocol")
	}
}
