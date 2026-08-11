package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

func TestOracleCLIPreRunBoundary(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate oracle CLI source")
	}
	directory := filepath.Dir(source)
	root := filepath.Clean(filepath.Join(directory, "..", "..", ".."))
	command := exec.Command("go", "list", "-deps", "./evaluation/cmd/final-v5-oracle")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list oracle CLI dependencies: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, forbidden := range []string{
			"taskbound.local/agent-data-gateway/evaluation/internal/experiment",
			"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding",
			"taskbound.local/agent-data-gateway/internal/exposure",
			"taskbound.local/agent-data-gateway/internal/control",
			"taskbound.local/agent-data-gateway/internal/gateway",
			"taskbound.local/agent-data-gateway/internal/physicalquery",
			"taskbound.local/agent-data-gateway/internal/preparedbinding",
			"taskbound.local/agent-data-gateway/internal/queryplan",
			"taskbound.local/agent-data-gateway/internal/sqlpolicy",
			"taskbound.local/agent-data-gateway/internal/sqlidentity",
			"taskbound.local/agent-data-gateway/internal/sqllowering",
			"github.com/pganalyze/pg_query_go",
		} {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("oracle CLI imports forbidden preparation/SQL dependency %q", dependency)
			}
		}
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "Prepare", "PrepareContext", "Derive":
				t.Errorf("oracle CLI calls forbidden semantic/prepared API %s in %s", selector.Sel.Name, entry.Name())
			}
			return true
		})
	}
}

func TestFullDatasetAdapterRejectsCallerSQLAndConnectionMaterial(t *testing.T) {
	for _, arguments := range [][]string{
		{"dataset-fingerprint-live", "--sql", "SELECT 1"},
		{"dataset-fingerprint-live", "--dsn", "postgres://forbidden"},
		{"dataset-fingerprint-live", "--query-file", "query.sql"},
	} {
		code, stdout, stderr := invokeCLI(arguments, "")
		if code == 0 || stdout != "" || stderr == "" {
			t.Fatalf("forbidden full Dataset adapter arguments %v: code=%d stdout=%q stderr=%q",
				arguments, code, stdout, stderr)
		}
	}
}

func TestExposureScaleAdapterUsesTheSharedFixedReadOnlyQuery(t *testing.T) {
	want := "SELECT member_rank, metric, family_id, partition_key\n" +
		"FROM reporting.final_v5_exposure_scale\n" +
		"ORDER BY member_rank"
	definition, err := finalv5dataset.ProductDefinitionFor(finalv5oracle.ExposureScaleProductID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Query != want {
		t.Fatalf("fixed exposure-scale Dataset query = %q", definition.Query)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate oracle CLI boundary test")
	}
	value, err := os.ReadFile(filepath.Join(filepath.Dir(source), "scale.go"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "scale.go", value, 0)
	if err != nil {
		t.Fatal(err)
	}
	queryCalls, streamCalls, preparedCountCalls := 0, 0, 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Query", "QueryRow":
			queryCalls++
		case "ProductStream":
			streamCalls++
		case "PreparedStatementCount":
			preparedCountCalls++
		}
		return true
	})
	if queryCalls != 0 || streamCalls != 1 || preparedCountCalls != 1 {
		t.Fatalf("Scale adapter has direct queries/shared streams/prepared checks = %d/%d/%d; want 0/1/1",
			queryCalls, streamCalls, preparedCountCalls)
	}
	for _, arguments := range [][]string{
		{"scale-dataset-agreement", "--sql", "SELECT 1"},
		{"scale-dataset-agreement", "--dsn", "postgres://forbidden"},
		{"scale-manifests", "--sql-file", "query.sql"},
	} {
		code, stdout, stderr := invokeCLI(arguments, "")
		if code == 0 || stdout != "" || stderr == "" {
			t.Fatalf("forbidden adapter arguments %v: code=%d stdout=%q stderr=%q", arguments, code, stdout, stderr)
		}
	}
}

func TestProvSQLAdapterUsesTheThreeSharedFixedReadOnlyQueries(t *testing.T) {
	wantOrders := "SELECT orderkey, status, partition_key\n" +
		"FROM reporting.provsql_orders\n" +
		"ORDER BY orderkey"
	wantLineitem := "SELECT orderkey, linenumber, extendedprice, partition_key\n" +
		"FROM reporting.provsql_lineitem\n" +
		"ORDER BY orderkey, linenumber"
	wantNonce := "SELECT nonce_id, partition_key\n" +
		"FROM reporting.provsql_nonce\n" +
		"ORDER BY nonce_id"
	orders, ordersErr := finalv5dataset.ProductDefinitionFor(finalv5oracle.ProvSQLOrdersProductID)
	lineitem, lineitemErr := finalv5dataset.ProductDefinitionFor(finalv5oracle.ProvSQLLineitemProductID)
	nonce, nonceErr := finalv5dataset.ProductDefinitionFor(finalv5oracle.ProvSQLNonceProductID)
	if ordersErr != nil || lineitemErr != nil || nonceErr != nil {
		t.Fatalf("load shared ProvSQL Product definitions: %v/%v/%v", ordersErr, lineitemErr, nonceErr)
	}
	if orders.Query != wantOrders || lineitem.Query != wantLineitem || nonce.Query != wantNonce {
		t.Fatalf("fixed ProvSQL Dataset queries changed: orders=%q lineitem=%q nonce=%q",
			orders.Query, lineitem.Query, nonce.Query)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate oracle CLI boundary test")
	}
	value, err := os.ReadFile(filepath.Join(filepath.Dir(source), "provsql.go"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "provsql.go", value, 0)
	if err != nil {
		t.Fatal(err)
	}
	queryCalls, streamCalls, preparedCountCalls := 0, 0, 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Query", "QueryRow":
			queryCalls++
		case "ProductStream":
			streamCalls++
		case "PreparedStatementCount":
			preparedCountCalls++
		}
		return true
	})
	if queryCalls != 0 || streamCalls != 1 || preparedCountCalls != 1 {
		t.Fatalf("ProvSQL adapter has direct queries/shared streams/prepared checks = %d/%d/%d; want 0/1/1",
			queryCalls, streamCalls, preparedCountCalls)
	}
	for _, arguments := range [][]string{
		{"provsql-dataset-agreement", "--sql", "SELECT 1"},
		{"provsql-dataset-agreement", "--dsn", "postgres://forbidden"},
		{"provsql-manifests", "--sql-file", "query.sql"},
	} {
		code, stdout, stderr := invokeCLI(arguments, "")
		if code == 0 || stdout != "" || stderr == "" {
			t.Fatalf("forbidden ProvSQL adapter arguments %v: code=%d stdout=%q stderr=%q",
				arguments, code, stdout, stderr)
		}
	}
}
