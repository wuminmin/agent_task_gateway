package finalv5binding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

func testTask(columns int) BoundTaskRequest {
	approved := make([]string, columns)
	for index := range approved {
		approved[index] = fmt.Sprintf("column_%02d", index+1)
	}
	return BoundTaskRequest{Objective: "reviewed unit-test oracle", DataProducts: []string{"result_heavy"},
		Columns: map[string][]string{"result_heavy": approved}, Scopes: map[string][]string{},
		VisibleRelation: "reporting.result_heavy", CompanionRelation: "taskgate_ordinal.result_heavy_v1"}
}

func testQuery(sql string, rows, dependencies int64, columns int, marker string) BoundQueryExpectation {
	return BoundQueryExpectation{SQL: sql, ExpectedRows: rows, ExpectedColumns: columns,
		ExpectedResultSHA256: shaBytes([]byte("result/" + marker)), DependencyFacts: dependencies,
		DependencySetSHA256: shaBytes([]byte("dependency/" + marker))}
}

func completeTestBinding(t *testing.T) []byte {
	t.Helper()
	section := Section{SchemaVersion: 1,
		Scale:    &ScaleBinding{DependencyE2E: map[string]DependencyCellBinding{}, EnableOutcomeMerkle: true},
		Artifact: &ArtifactBinding{ResultHeavy: map[string]ArtifactCellBinding{}},
		ProvSQL:  completeTestProvSQL(t),
	}
	for _, prefix := range []struct {
		name  string
		facts int64
	}{{"10k", 10_000}, {"100k", 100_000}, {"1035000", 1_035_000}} {
		for _, overlap := range []int64{0, 50, 90, 100} {
			scale := fmt.Sprintf("%s-overlap-%d", prefix.name, overlap)
			cell := DependencyCellBinding{Task: testTask(1), Candidate: testQuery(
				"SELECT column_01 FROM result_heavy ORDER BY column_01", 1, prefix.facts, 1, scale+"/candidate")}
			if overlap != 0 {
				history := testQuery("SELECT column_01 FROM result_heavy ORDER BY column_01 LIMIT 1", 1,
					prefix.facts*overlap/100, 1, scale+"/history")
				cell.History = &history
			}
			section.Scale.DependencyE2E[scale] = cell
		}
	}
	for _, spec := range []struct {
		scale   string
		rows    int64
		columns int
	}{{"100x4", 100, 4}, {"10k-x4", 10_000, 4}, {"100k-x4", 100_000, 4},
		{"100x16", 100, 16}, {"10k-x16", 10_000, 16}, {"100k-x16", 100_000, 16}} {
		task := testTask(spec.columns)
		section.Artifact.ResultHeavy[spec.scale] = ArtifactCellBinding{Task: task,
			Query: testQuery("SELECT "+strings.Join(task.Columns["result_heavy"], ",")+
				" FROM result_heavy ORDER BY column_01", spec.rows, spec.rows, spec.columns, spec.scale)}
	}
	value, err := json.Marshal(map[string]any{
		"dataset_sha256": shaBytes([]byte("dataset")), "catalog_sha256": shaBytes([]byte("catalog")),
		SectionName: section,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func completeTestProvSQL(t *testing.T) *ProvSQLBinding {
	t.Helper()
	task := BoundTaskRequest{Objective: "reviewed ProvSQL oracle",
		DataProducts: []string{"provsql_orders", "provsql_lineitem", "provsql_nonce"},
		Columns: map[string][]string{
			"provsql_orders":   {"orderkey", "status", "partition_key"},
			"provsql_lineitem": {"orderkey", "linenumber", "extendedprice", "partition_key"},
			"provsql_nonce":    {"nonce_id", "partition_key"},
		}, Scopes: map[string][]string{"partition_key": {"1"}},
		VisibleRelation: "reporting.provsql_orders", CompanionRelation: "taskgate_ordinal.provsql_orders_v1"}
	binding := &ProvSQLBinding{FixtureVersion: provsqlfixture.Version,
		FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(), EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(),
		DatasetSHA256: provsqlfixture.ExpectedDatasetSHA256(), DatasetProbeSQLSHA256: provsqlfixture.DatasetProbeSQLSHA256(),
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(), Task: task,
		TaskGate: map[string]BoundQueryExpectation{}}
	for _, scale := range []string{"1k", "10k", "45k"} {
		rows, err := provsqlfixture.ExpectedResultRows(scale)
		if err != nil {
			t.Fatal(err)
		}
		resultSHA, err := canonicalResultHash(rows)
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []struct {
			warmup bool
			count  int
		}{{true, 5}, {false, 30}} {
			for iteration := 1; iteration <= phase.count; iteration++ {
				nonce, err := provsqlfixture.Nonce(scale, 1, iteration, phase.warmup)
				if err != nil {
					t.Fatal(err)
				}
				logical, err := provsqlfixture.LogicalSQL(scale, nonce)
				if err != nil {
					t.Fatal(err)
				}
				key := ProvSQLBindingKey(scale, nonce)
				binding.TaskGate[key] = BoundQueryExpectation{SQL: logical, ExpectedRows: provsqlfixture.ExpectedRows,
					ExpectedColumns: provsqlfixture.ExpectedColumns, ExpectedResultSHA256: resultSHA,
					DependencyFacts: int64(1 + len(binding.TaskGate)), DependencySetSHA256: shaBytes([]byte(key)),
					ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1}
			}
		}
	}
	return binding
}

func TestParseRequiresCompleteExactPublicationBinding(t *testing.T) {
	value := completeTestBinding(t)
	binding, err := Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Section.Scale.DependencyE2E) != 12 || len(binding.Section.Artifact.ResultHeavy) != 6 ||
		len(binding.Section.ProvSQL.TaskGate) != 105 || !ValidDigest(binding.SectionSHA256) ||
		!ValidDigest(binding.FileSHA256) {
		t.Fatalf("incomplete validation result: %+v", binding)
	}

	var indented any
	if err := json.Unmarshal(value, &indented); err != nil {
		t.Fatal(err)
	}
	pretty, _ := json.MarshalIndent(indented, "", "  ")
	formatted, err := Parse(pretty)
	if err != nil {
		t.Fatal(err)
	}
	if formatted.SectionSHA256 != binding.SectionSHA256 || formatted.FileSHA256 == binding.FileSHA256 {
		t.Fatal("canonical section identity or exact file identity does not distinguish formatting correctly")
	}
}

func TestLoadFileRequiresPrivate0600Mode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(path, completeTestBinding(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatalf("0600 private binding rejected: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("world-readable private binding was accepted")
	}
}

func TestParseRejectsUnknownForbiddenAndAmbiguousFields(t *testing.T) {
	value := completeTestBinding(t)
	var top map[string]any
	if err := json.Unmarshal(value, &top); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		key  string
		val  any
	}{
		{"unknown top", "notes", "not reviewed"},
		{"derived volume", "deployment_volume_id_sha256", strings.Repeat("a", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := make(map[string]any, len(top)+1)
			for key, child := range top {
				mutated[key] = child
			}
			mutated[test.key] = test.val
			encoded, _ := json.Marshal(mutated)
			if _, err := Parse(encoded); err == nil {
				t.Fatalf("field %q was accepted", test.key)
			}
		})
	}
	section := top[SectionName].(map[string]any)
	for _, field := range []string{"dataset_probe_sql", "observer", "password"} {
		section[field] = "forbidden"
		encoded, _ := json.Marshal(top)
		if _, err := Parse(encoded); err == nil {
			t.Fatalf("private section field %q was accepted", field)
		}
		delete(section, field)
	}

	duplicateTop := strings.Replace(string(value), `"dataset_sha256":`, `"dataset_sha256":"`+
		strings.Repeat("f", 64)+`","dataset_sha256":`, 1)
	if _, err := Parse([]byte(duplicateTop)); err == nil {
		t.Fatal("duplicate top-level key was accepted")
	}
	duplicateNested := strings.Replace(string(value), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)
	if _, err := Parse([]byte(duplicateNested)); err == nil {
		t.Fatal("duplicate nested key was accepted")
	}
}

func TestParseRejectsMissingAndExtraFrozenCells(t *testing.T) {
	value := completeTestBinding(t)
	var top map[string]any
	_ = json.Unmarshal(value, &top)
	tests := []struct {
		name    string
		section string
		cells   string
		key     string
	}{
		{"scale", "scale", "dependency_e2e", "10k-overlap-0"},
		{"artifact", "artifact", "result_heavy", "100x4"},
		{"provsql", "provsql", "taskgate", "1k/1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyBytes, _ := json.Marshal(top)
			var mutated map[string]any
			_ = json.Unmarshal(copyBytes, &mutated)
			cells := mutated[SectionName].(map[string]any)[test.section].(map[string]any)[test.cells].(map[string]any)
			delete(cells, test.key)
			encoded, _ := json.Marshal(mutated)
			if _, err := Parse(encoded); err == nil {
				t.Fatal("missing frozen cell was accepted")
			}
			cells["unfrozen-cell"] = map[string]any{}
			encoded, _ = json.Marshal(mutated)
			if _, err := Parse(encoded); err == nil {
				t.Fatal("extra frozen cell was accepted")
			}
		})
	}
}

func TestValidateProvSQLBindingRequiresOneVisibleAndOneCompanionCall(t *testing.T) {
	binding := completeTestProvSQL(t)
	if err := ValidateProvSQLBinding(binding); err != nil {
		t.Fatalf("exact one-visible/one-companion binding was rejected: %v", err)
	}

	key := ProvSQLBindingKey("1k", 1)
	original := binding.TaskGate[key]
	for _, test := range []struct {
		name      string
		visible   int64
		companion int64
	}{
		{name: "missing visible", visible: 0, companion: 1},
		{name: "extra visible", visible: 2, companion: 1},
		{name: "missing companion", visible: 1, companion: 0},
		{name: "extra companion", visible: 1, companion: 2},
		{name: "nonzero total with wrong split", visible: 2, companion: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := original
			mutated.ExpectedVisibleCalls = test.visible
			mutated.ExpectedCompanionCalls = test.companion
			binding.TaskGate[key] = mutated
			t.Cleanup(func() { binding.TaskGate[key] = original })
			if err := ValidateProvSQLBinding(binding); err == nil {
				t.Fatalf("visible=%d companion=%d was accepted", test.visible, test.companion)
			}
		})
	}
}

func TestBoundSQLUsesPostgreSQLASTAndApprovedTaskGrant(t *testing.T) {
	for _, sqlText := range []string{
		"INSERT INTO result_heavy(column_01) VALUES (1)",
		"SELECT column_01 FROM result_heavy; DELETE FROM result_heavy",
		"SELECT column_01 INTO copied FROM result_heavy",
		"WITH changed AS (DELETE FROM result_heavy RETURNING column_01) SELECT column_01 FROM changed",
		"SELECT column_01 FROM result_heavy FOR UPDATE",
	} {
		query := testQuery(sqlText, 1, 1, 1, sqlText)
		if err := ValidateBoundQuery(query); err == nil {
			t.Fatalf("unsafe SQL was accepted: %s", sqlText)
		}
	}
	task := testTask(1)
	for _, sqlText := range []string{
		"SELECT forbidden FROM result_heavy",
		"SELECT column_01 FROM unapproved_product",
		"SELECT * FROM result_heavy",
	} {
		if err := validateBoundQueryForTask(testQuery(sqlText, 1, 1, 1, sqlText), task); err == nil {
			t.Fatalf("SQL outside approved task was accepted: %s", sqlText)
		}
	}
}

func TestCompiledDatasetProbeMatchesFreshDeploymentSource(t *testing.T) {
	path := filepath.Join("..", "..", "final-v5-wsl2", "sql", "datasets", "default-fingerprint.sql")
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fromFile := strings.TrimSuffix(strings.TrimSpace(string(value)), ";")
	if fromFile != DatasetProbeSQL {
		t.Fatal("compiled dataset probe differs from the SQL executed by fresh deployment")
	}
}

func TestCatalogAwareGateRejectsUnknownProductsColumnsAndInsufficientBudget(t *testing.T) {
	path := filepath.Join("..", "..", "..", "config", "catalog.yaml")
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := catalog.Parse(value)
	if err != nil {
		t.Fatal(err)
	}

	prov := completeTestProvSQL(t)
	one := prov.TaskGate[ProvSQLBindingKey("1k", 1)]
	if err := validateTaskCatalogCapacity(parsed, prov.Task, []BoundQueryExpectation{one}); err != nil {
		t.Fatalf("one frozen ProvSQL operation should fit its exact route: %v", err)
	}
	if err := validateTaskCatalogCapacity(parsed, prov.Task, []BoundQueryExpectation{one, one}); err == nil ||
		!strings.Contains(err.Error(), "max_queries") {
		t.Fatalf("two-query task was not rejected by the one-query route: %v", err)
	}
	policy, err := parsed.ResolveTaskPolicy(prov.Task.DataProducts)
	if err != nil {
		t.Fatal(err)
	}
	for index := range parsed.BudgetProfiles {
		if parsed.BudgetProfiles[index].Name == policy.BudgetProfile {
			parsed.BudgetProfiles[index].MaxQueries = 2
			parsed.BudgetProfiles[index].MaxRows = one.ExpectedRows
		}
	}
	if err := validateTaskCatalogCapacity(parsed, prov.Task, []BoundQueryExpectation{one, one}); err == nil ||
		!strings.Contains(err.Error(), "cumulative budget max_rows") {
		t.Fatalf("two individually fitting queries escaped the cumulative row budget: %v", err)
	}

	unknown := testTask(1)
	unknown.DataProducts = []string{"not_a_catalog_product"}
	unknown.Columns = map[string][]string{"not_a_catalog_product": {"column_01"}}
	if err := validateTaskCatalogCapacity(parsed, unknown, []BoundQueryExpectation{
		testQuery("SELECT column_01 FROM not_a_catalog_product", 1, 1, 1, "unknown")}); err == nil {
		t.Fatal("unknown Catalog product was accepted")
	}

	tooWide := BoundTaskRequest{Objective: "unpublished width", DataProducts: []string{"expense_detail"},
		Columns: map[string][]string{"expense_detail": {"receipt_no", "not_published"}},
		Scopes:  map[string][]string{"department": {"销售部"}}, VisibleRelation: "reporting.expense_detail",
		CompanionRelation: "taskgate_ordinal.expense_detail_v1"}
	if err := validateTaskCatalogCapacity(parsed, tooWide, []BoundQueryExpectation{
		testQuery("SELECT receipt_no,not_published FROM expense_detail", 1, 1, 2, "wide")}); err == nil {
		t.Fatal("column outside source-controlled Catalog was accepted")
	}
	unrelatedScope := prov.Task
	unrelatedScope.Scopes = map[string][]string{"partition_key": {"1"}, "department": {"销售部"}}
	if err := validateTaskCatalogCapacity(parsed, unrelatedScope, []BoundQueryExpectation{one}); err == nil {
		t.Fatal("known but unrelated Catalog scope was accepted for the ProvSQL products")
	}
}
