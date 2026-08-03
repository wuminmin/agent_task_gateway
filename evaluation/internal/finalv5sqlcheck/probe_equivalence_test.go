package finalv5sqlcheck

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
)

// v1.3 renames one CTE identifier inside the benchmark probe, from the reserved
// keyword `collation` to `collation_info`. A reviewer has to be able to see that
// this changed nothing observable, not take it on trust.
//
// The reference form here is the *original* query with the identifier quoted --
// `WITH "collation" AS ...` -- which is what the v1.2 author meant and what
// PostgreSQL would have accepted. It is constructed in the test and is
// deliberately not a contract artifact: it exists only to be compared against.
//
// See contracts/AMENDMENT-v1.3.md.
func TestBenchmarkProbeRenameIsSemanticsPreserving(t *testing.T) {
	adminDSN := strings.TrimSpace(os.Getenv(AdminDSNEnv))
	if adminDSN == "" {
		t.Skipf("%s is required for the probe semantic-equivalence check", AdminDSNEnv)
	}
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatalf("contract bridge: %v", err)
	}
	released, err := runtime.DatasetProbeSQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(released, "\ncollation AS (") {
		t.Fatal("the released probe still uses the reserved keyword as a bare CTE identifier")
	}
	if !strings.Contains(released, "collation_info AS (") {
		t.Fatal("the released probe does not use the renamed CTE")
	}

	// Reconstruct the pre-rename query by undoing exactly the rename, then
	// quoting the reserved identifier so PostgreSQL will accept it. If the
	// released bytes differ from the original in any other way, this
	// reconstruction cannot produce an equal result and the test fails.
	reference := strings.ReplaceAll(released, "collation_info", `"collation"`)
	if reference == released {
		t.Fatal("the reference reconstruction did not differ from the released probe")
	}

	generator, err := runtime.DatasetGeneratorSQL()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := Provision(ctx, adminDSN, generator)
	if err != nil {
		t.Fatalf("provision benchmark database: %v", err)
	}
	defer func() { _ = database.Drop(context.Background()) }()

	releasedValue, releasedColumn, err := database.ScalarQuery(ctx, released)
	if err != nil {
		t.Fatalf("released probe: %v", err)
	}
	referenceValue, referenceColumn, err := database.ScalarQuery(ctx, reference)
	if err != nil {
		t.Fatalf("quoted-original reference probe: %v", err)
	}

	if releasedColumn != referenceColumn {
		t.Fatalf("column name changed: %q -> %q", referenceColumn, releasedColumn)
	}
	if err := ValidateProbeOutput(releasedValue, releasedColumn); err != nil {
		t.Fatalf("released probe output: %v", err)
	}
	if Digest([]byte(releasedValue)) != Digest([]byte(referenceValue)) {
		t.Fatalf("V1.3 PROBE SEMANTICS CHANGED: canonical output digest differs\n  reference %s\n  released  %s",
			Digest([]byte(referenceValue)), Digest([]byte(releasedValue)))
	}

	// Compare the decoded structure too, so an identical digest cannot be the
	// result of both sides failing in the same way.
	var releasedDocument, referenceDocument map[string]any
	if err := json.Unmarshal([]byte(releasedValue), &releasedDocument); err != nil {
		t.Fatalf("released probe output is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(referenceValue), &referenceDocument); err != nil {
		t.Fatalf("reference probe output is not JSON: %v", err)
	}
	if len(releasedDocument) != len(referenceDocument) {
		t.Fatalf("V1.3 PROBE SEMANTICS CHANGED: member count %d -> %d",
			len(referenceDocument), len(releasedDocument))
	}
	for key := range referenceDocument {
		if _, present := releasedDocument[key]; !present {
			t.Fatalf("V1.3 PROBE SEMANTICS CHANGED: released output dropped %q", key)
		}
	}
	// The output key stays "collation": the rename was internal to the query.
	if _, present := releasedDocument["collation"]; !present {
		t.Fatal("V1.3 PROBE SEMANTICS CHANGED: the collation output key was renamed")
	}
	t.Logf("probe fingerprint identical across both forms: %s", Digest([]byte(releasedValue))[:16])
}

// The gate must fail on SQL that cannot parse, otherwise it proves nothing.
// This is the defect v1.3 corrects, reproduced against a live server.
func TestReservedKeywordCTEIsRejectedByPostgreSQL(t *testing.T) {
	adminDSN := strings.TrimSpace(os.Getenv(AdminDSNEnv))
	if adminDSN == "" {
		t.Skipf("%s is required", AdminDSNEnv)
	}
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	generator, err := runtime.DatasetGeneratorSQL()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := Provision(ctx, adminDSN, generator)
	if err != nil {
		t.Fatalf("provision benchmark database: %v", err)
	}
	defer func() { _ = database.Drop(context.Background()) }()

	if _, _, err := database.ScalarQuery(ctx,
		`WITH collation AS (SELECT 1 AS x) SELECT x FROM collation`); err == nil {
		t.Fatal("PostgreSQL accepted a bare reserved keyword as a CTE identifier")
	}
	value, _, err := database.ScalarQuery(ctx,
		`WITH "collation" AS (SELECT 1 AS x) SELECT x FROM "collation"`)
	if err != nil || value != "1" {
		t.Fatalf("quoting the reserved identifier did not fix it: %q, %v", value, err)
	}
}
