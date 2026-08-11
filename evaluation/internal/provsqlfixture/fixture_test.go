package provsqlfixture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFrozenProvSQLScalesAndNonces(t *testing.T) {
	seen := map[int64]bool{}
	for _, scale := range []string{"1k", "10k", "45k"} {
		rows, err := ExpectedResultRows(scale)
		if err != nil || len(rows) != int(ExpectedRows) {
			t.Fatalf("ExpectedResultRows(%q) = %d, %v", scale, len(rows), err)
		}
		for warmup := 1; warmup <= 5; warmup++ {
			nonce, err := Nonce(scale, 1, warmup, true)
			if err != nil || seen[nonce] {
				t.Fatalf("warmup nonce %s/%d = %d, %v", scale, warmup, nonce, err)
			}
			seen[nonce] = true
		}
		for iteration := 1; iteration <= 30; iteration++ {
			nonce, err := Nonce(scale, 1, iteration, false)
			if err != nil || seen[nonce] {
				t.Fatalf("measured nonce %s/%d = %d, %v", scale, iteration, nonce, err)
			}
			seen[nonce] = true
			logical, err := LogicalSQL(scale, nonce)
			if err != nil || logical == "" {
				t.Fatalf("LogicalSQL(%s,%d) = %q, %v", scale, nonce, logical, err)
			}
		}
	}
	if len(seen) != 105 {
		t.Fatalf("nonce cardinality = %d, want 105", len(seen))
	}
	for _, invalid := range []struct {
		scale string
		proc  int
		iter  int
		warm  bool
	}{{"bad", 1, 1, false}, {"1k", 2, 1, false}, {"1k", 1, 31, false}, {"1k", 1, 6, true}} {
		if nonce, err := Nonce(invalid.scale, invalid.proc, invalid.iter, invalid.warm); err == nil {
			t.Fatalf("Nonce(%s) accepted as %d", fmt.Sprint(invalid), nonce)
		}
	}
}

func TestFixtureBindingsAreNonemptyAndStable(t *testing.T) {
	for name, value := range map[string]string{
		"fixture": FixtureSQLSHA256(), "enable": EnableSQLSHA256(), "physical": PhysicalSQLSHA256(),
		"dataset query": DatasetProbeSQLSHA256(), "dataset": ExpectedDatasetSHA256(),
	} {
		if len(value) != 64 {
			t.Fatalf("%s digest = %q", name, value)
		}
	}
	if DatasetRowCount != 301000 {
		t.Fatalf("dataset row count = %d", DatasetRowCount)
	}
}

func TestBusinessSQLChangesOnlyTheFixedRelations(t *testing.T) {
	logical, err := LogicalSQL("10k", 401)
	if err != nil {
		t.Fatal(err)
	}
	business, err := BusinessSQL("10k", 401)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"FROM provsql_orders AS o", "FROM reporting.provsql_orders AS o",
		"JOIN provsql_lineitem AS l", "JOIN reporting.provsql_lineitem AS l",
		"JOIN provsql_nonce AS nonce", "JOIN reporting.provsql_nonce AS nonce",
	).Replace(logical)
	if business != want {
		t.Fatalf("Business SQL differs outside fixed relations:\n%s", business)
	}
}

func TestEmbeddedFixtureEqualsBusinessInitBytes(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve fixture test source")
	}
	path := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "db", "init", "01-final-v5-provsql-fixture.sql")
	businessInit, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read business init fixture: %v", err)
	}
	if !bytes.Equal(businessInit, []byte(FixtureSQL)) {
		t.Fatal("embedded ProvSQL fixture differs from the sole Compose init-directory copy")
	}
}
