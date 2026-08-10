package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPublicationReviewStaysPreRunAndDoesNotDuplicatePreparation(t *testing.T) {
	files := []string{"main.go", "review.go"}
	forbiddenSelectors := map[string]bool{"Prepare": true, "PrepareContext": true, "Derive": true}
	forbiddenImports := []string{
		"internal/gateway", "internal/physicalquery", "internal/queryplan", "internal/sqllowering",
		"internal/sqlidentity", "internal/sqlpolicy", "evaluation/internal/experiment",
		"evaluation/internal/finalv5binding", "pg_query_go", "vitess", "sqlparser",
	}
	set := token.NewFileSet()
	for _, name := range files {
		parsed, err := parser.ParseFile(set, name, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range forbiddenImports {
				if strings.Contains(path, forbidden) {
					t.Fatalf("%s imports forbidden preparation/oracle dependency %q", name, path)
				}
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && forbiddenSelectors[selector.Sel.Name] {
				t.Errorf("%s contains forbidden %s call at %s", name, selector.Sel.Name, set.Position(call.Pos()))
			}
			return true
		})
	}
}

func TestClosedSetVerificationPrecedesEveryProductionRead(t *testing.T) {
	value, err := os.ReadFile("review.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(value)
	provSQLClosedSet := strings.Index(source, "VerifyProvSQLNonceJoinManifestSet")
	scaleClosedSet := strings.Index(source, "VerifyExposureScaleDependencyManifestSet")
	firstAttestation := strings.Index(source, "attestExposureSource(ctx")
	firstSnapshotScan := strings.Index(source, "snapshotbundle.ScanPostgresSnapshot")
	if provSQLClosedSet < 0 || scaleClosedSet < 0 || firstAttestation < 0 || firstSnapshotScan < 0 ||
		provSQLClosedSet >= firstAttestation || provSQLClosedSet >= firstSnapshotScan ||
		scaleClosedSet >= firstAttestation || scaleClosedSet >= firstSnapshotScan {
		t.Fatalf("complete oracle closed-set verification must precede attestation and snapshot reads: ProvSQL=%d Scale=%d attest=%d scan=%d",
			provSQLClosedSet, scaleClosedSet, firstAttestation, firstSnapshotScan)
	}
}

func TestPublicationReviewCLIHasNoSQLOrDSNInputSurface(t *testing.T) {
	value, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`flags.String("sql"`, `flags.String("sql-file"`, `flags.String("dsn"`,
		`flags.String("query"`, `flags.String("statement"`} {
		if strings.Contains(string(value), forbidden) {
			t.Fatalf("publication review CLI exposes forbidden arbitrary input %q", forbidden)
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "..", "internal", "physicalquery")); err != nil {
		t.Fatalf("repository layout changed, boundary test no longer proves the unique preparation implementation: %v", err)
	}
}
