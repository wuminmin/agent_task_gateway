package finalv5linker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLinkerRegularSourceHasNoPreparationOrDerivationDependency(t *testing.T) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not locate linker package")
	}
	directory := filepath.Dir(source)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenImports := []string{
		"database/sql", "pgx", "sqlparser", "internal/physicalquery", "internal/queryplan",
		"internal/sqllowering", "internal/sqlpolicy",
	}
	forbiddenSelectors := map[string]bool{"Derive": true, "Prepare": true, "PrepareContext": true}
	regularFiles := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		regularFiles++
		path := filepath.Join(directory, entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, imported := range file.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", entry.Name(), err)
			}
			for _, forbidden := range forbiddenImports {
				if strings.Contains(value, forbidden) {
					t.Errorf("regular source %s imports forbidden preparation/SQL dependency %q", entry.Name(), value)
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && forbiddenSelectors[selector.Sel.Name] {
				t.Errorf("regular source %s references forbidden selector %s", entry.Name(), selector.Sel.Name)
			}
			return true
		})
	}
	if regularFiles == 0 {
		t.Fatal("linker boundary scan found no regular Go source")
	}
}
