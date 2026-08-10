package finalv5contracts

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

// Decision 10 fixes S2/S4/S5 at canonical typed-row lexical ordering. These
// digests are the author-approved implementation inputs introduced by 1702e65;
// they are intentionally historical constants, not values generated from the
// checkout under test. Pinning both the bytes and every cell's template/order
// binding prevents a coordinated query/Index/contract rewrite from silently
// changing the comparison while still passing self-consistency checks.
func TestBenchmarkS2S4S5QueriesAndCanonicalNormalizerRemainFrozen(t *testing.T) {
	const canonicalOrder = "canonical_typed_row_lexicographic_v1"
	root := filepath.Join(runtimeCompatibilityRoot(t), "evaluation", "final-v5-wsl2")
	historicalSHA256 := map[string]string{
		"contracts/baseline-v1.json":             "67aff3dcb464bd9ebadd893a7b2cf98fbcab5148af3f906b9904151ca5661bf2",
		"sql/contracts/S2-bdg.sql":               "78c29c7cc060923df4c25b3290a87d15a49a918f2b9718a3dc3d03a48df81480",
		"sql/contracts/S2-direct.sql":            "53e6875c7797e6d2bd5500623f63a62e7e52ab5cda1a79141266254519e32f81",
		"sql/contracts/S4-bdg.sql":               "fafc7435240891dd33bdb5da679fc3b5dded2ae441166c5d8a1bfceb3325ce5a",
		"sql/contracts/S4-direct.sql":            "53e6875c7797e6d2bd5500623f63a62e7e52ab5cda1a79141266254519e32f81",
		"sql/contracts/S5-bdg-plan.json":         "c2f1ab18977ce4cad1791cc77241f13458eebb590714140aa3bc5598bf81830a",
		"sql/contracts/S5-direct.sql":            "b5778471af9350130b3363ac6086090215e767f218c7a42bf42b8c792e08cad1",
		"contracts/result-normalization-v1.json": "2eb64f136ffc164f078517e9604db907543ff02ae86a1249b0cb90bb44cf0f64",
	}
	for relative, want := range historicalSHA256 {
		value, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read frozen %s: %v", relative, err)
		}
		got := fmt.Sprintf("%x", sha256.Sum256(value))
		if got != want {
			t.Errorf("frozen %s SHA-256 = %s, historical author-approved value = %s", relative, got, want)
		}
	}
	normalizerImplementations := map[string]string{
		"canonical.go": "b12743ea60de2c7f02ae66a3f55623a1b2159eeac55ba9c603e5d02287ae7069",
		"result.go":    "fba875ed8372c39a92b1999be8e6668c9250de5429e5dd7c82c93cd3afafeedf",
	}
	for file, want := range normalizerImplementations {
		path := filepath.Join(runtimeCompatibilityRoot(t), "evaluation", "finalv5oracle", file)
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read frozen normalizer implementation %s: %v", file, err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(value)); got != want {
			t.Errorf("frozen normalizer implementation %s SHA-256 = %s, historical author-approved value = %s", file, got, want)
		}
	}

	normalizationBytes, err := os.ReadFile(filepath.Join(root, "contracts", "result-normalization-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	decodedNormalization, err := decodeAny(normalizationBytes)
	if err != nil {
		t.Fatal(err)
	}
	normalization, ok := decodedNormalization.(map[string]any)
	if !ok {
		t.Fatal("result normalization contract is not an object")
	}
	rowStream, ok := objectValue(normalization, "row_stream_encoding")
	if !ok {
		t.Fatal("result normalization contract omits row_stream_encoding")
	}
	orderingModes, ok := objectValue(rowStream, "ordering_modes")
	if !ok || stringValue(orderingModes, canonicalOrder) == "" {
		t.Fatalf("result normalization contract does not register %s", canonicalOrder)
	}
	// Keep the published v1.4 metadata literal, including its historical
	// grammar rationale. Decision 10's authoritative prose now rests on the
	// frozen queries not relying on total result order; only a future contract
	// release may regenerate this byte string.
	const totalOrderRule = "S1, S3, S6, Artifact, and single-Product Scale results use query_order_v1; " +
		"S2, S4, and S5 use canonical_typed_row_lexicographic_v1 because production Join/semantic-View/UNION grammar forbids ORDER BY"
	if got := stringValue(normalization, "total_order_rule"); got != totalOrderRule {
		t.Fatalf("result normalization total-order rule changed: %q", got)
	}

	templates := map[string]struct {
		direct, bdg string
		cells       int
	}{
		"S2": {"sql/contracts/S2-direct.sql", "sql/contracts/S2-bdg.sql", 10},
		"S4": {"sql/contracts/S4-direct.sql", "sql/contracts/S4-bdg.sql", 3},
		"S5": {"sql/contracts/S5-direct.sql", "sql/contracts/S5-bdg-plan.json", 8},
	}
	seen := map[string]int{}
	for _, current := range loadBaselineForTest(t).Cells {
		frozen, guarded := templates[current.Workload]
		if !guarded {
			continue
		}
		query, err := decodeRawObject(current.Query)
		if err != nil {
			t.Fatalf("decode %s/%s/%s query: %v", current.Workload, current.Scale, current.Mode, err)
		}
		wantTemplate := frozen.bdg
		if current.Mode == "direct" {
			wantTemplate = frozen.direct
		}
		if got := stringValue(query, "template"); got != wantTemplate {
			t.Errorf("%s/%s/%s template = %q, want frozen %q",
				current.Workload, current.Scale, current.Mode, got, wantTemplate)
		}
		if got := stringValue(query, "result_ordering"); got != canonicalOrder {
			t.Errorf("%s/%s/%s result ordering = %q, want %q",
				current.Workload, current.Scale, current.Mode, got, canonicalOrder)
		}
		if total, present := query["total_order_required"].(bool); !present || total {
			t.Errorf("%s/%s/%s no longer disables query total order",
				current.Workload, current.Scale, current.Mode)
		}
		seen[current.Workload]++
	}
	for workload, frozen := range templates {
		if seen[workload] != frozen.cells {
			t.Errorf("guarded %s cells = %d, want frozen %d", workload, seen[workload], frozen.cells)
		}
	}
}

func TestBenchmarkS2SQLLowersAndCompilesWithProductionPipeline(t *testing.T) {
	candidate, products := loadRuntimeCompatibilityProducts(t)
	_ = candidate
	sql := readRenderedSQLTemplate(t, "S2-bdg.sql", map[string]string{"$1": "5000"})

	lowered, err := sqllowering.Lower(sql, products)
	if err != nil {
		t.Fatalf("lower S2-bdg.sql: %v", err)
	}
	if lowered.Plan.From == nil || lowered.Plan.From.JoinMany == nil || len(lowered.Plan.From.JoinMany.Sources) != 2 {
		t.Fatalf("S2 did not lower to a two-product join_many plan: %#v", lowered.Plan.From)
	}
	if len(lowered.Plan.OrderBy) != 0 {
		t.Fatalf("S2 multi-product plan retained unsupported order_by: %#v", lowered.Plan.OrderBy)
	}
	compiled, err := queryplan.CompileRelational(lowered.Plan, products)
	if err != nil {
		t.Fatalf("compile lowered S2 plan: %v", err)
	}
	if compiled.Kind != "join" || compiled.VisibleSQL == "" || compiled.ProvenanceSQL == "" {
		t.Fatalf("S2 relational compilation is incomplete: kind=%q visible=%q provenance=%q", compiled.Kind, compiled.VisibleSQL, compiled.ProvenanceSQL)
	}
}

func TestBenchmarkS5StructuredPlanRendersAndCompiles(t *testing.T) {
	_, products := loadRuntimeCompatibilityProducts(t)
	path := filepath.Join(runtimeCompatibilityRoot(t), "evaluation", "final-v5-wsl2", "sql", "contracts", "S5-bdg-plan.json")
	var document struct {
		TemplateSchemaVersion int                        `json:"template_schema_version"`
		Entrypoint            string                     `json:"entrypoint"`
		RenderRule            string                     `json:"render_rule"`
		Parameters            map[string]json.RawMessage `json:"parameters"`
		Plan                  json.RawMessage            `json:"plan"`
	}
	if _, err := decodeStrictJSONFile(path, &document); err != nil {
		t.Fatal(err)
	}
	if document.TemplateSchemaVersion != 1 || document.Entrypoint != "execute_plan" {
		t.Fatalf("unexpected S5 template header: schema=%d entrypoint=%q", document.TemplateSchemaVersion, document.Entrypoint)
	}
	if !strings.Contains(document.RenderRule, "$parameter") {
		t.Fatalf("S5 render rule does not bind parameter objects: %q", document.RenderRule)
	}

	rawPlan, err := decodeAny(document.Plan)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPlanParameters(rawPlan, map[string]json.Number{
		"orderkey_max":       "5000",
		"overlap_branch_max": "2500",
	})
	if err != nil {
		t.Fatal(err)
	}
	renderedJSON, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	var plan queryplan.QueryPlan
	if err := decodeStrictJSON(renderedJSON, &plan); err != nil {
		t.Fatalf("decode rendered S5 plan: %v", err)
	}
	if len(plan.OrderBy) != 0 {
		t.Fatalf("S5 plan retained unsupported order_by: %#v", plan.OrderBy)
	}
	compiled, err := queryplan.CompileRelational(plan, products)
	if err != nil {
		t.Fatalf("compile rendered S5 plan: %v", err)
	}
	if compiled.Kind != "union_distinct" || !compiled.ExpandedEvidence ||
		!strings.Contains(compiled.VisibleSQL, " UNION ") || !strings.Contains(compiled.ProvenanceSQL, " UNION ALL ") {
		t.Fatalf("S5 union compilation lost its visible/provenance semantics: kind=%q expanded=%v visible=%q provenance=%q",
			compiled.Kind, compiled.ExpandedEvidence, compiled.VisibleSQL, compiled.ProvenanceSQL)
	}
}

func TestBenchmarkS6INMatchesCatalogAndLowers(t *testing.T) {
	candidate, products := loadRuntimeCompatibilityProducts(t)
	product := runtimeCatalogProduct(t, candidate, "final_v5_result_heavy")
	if !runtimeContains(product.AllowedOperators, "=") {
		t.Fatalf("result-heavy Catalog allowlist lacks PostgreSQL AEXPR_IN operator '=': %#v", product.AllowedOperators)
	}

	sql := readRenderedSQLTemplate(t, "S6-x4-bdg.sql", map[string]string{"$1": "100000"})
	lowered, err := sqllowering.Lower(sql, products)
	if err != nil {
		t.Fatalf("lower S6-x4-bdg.sql: %v", err)
	}
	foundIN := false
	for _, filter := range lowered.Plan.Filters {
		if filter.Column == "category" && strings.EqualFold(filter.Op, "IN") {
			foundIN = true
			break
		}
	}
	if !foundIN {
		t.Fatalf("S6 PostgreSQL IN predicate did not lower to the closed QueryPlan IN operator: %#v", lowered.Plan.Filters)
	}
	compiled, err := queryplan.Compile(lowered.Plan, products[product.Name])
	if err != nil {
		t.Fatalf("compile lowered S6 plan: %v", err)
	}
	if !strings.Contains(compiled, `"category" IN (`) {
		t.Fatalf("compiled S6 SQL lost the IN predicate: %s", compiled)
	}
}

func TestBenchmarkS4RootPublishesBothTerminalScopes(t *testing.T) {
	candidate, _ := loadRuntimeCompatibilityProducts(t)
	product := runtimeCatalogProduct(t, candidate, "final_v5_analytics_depth4")
	for _, field := range []string{"orders_partition_key", "lineitem_partition_key"} {
		if !runtimeContains(product.Scopes, field) {
			t.Errorf("S4 root scopes omit terminal scope %q: %#v", field, product.Scopes)
		}
		if !runtimeContains(product.EntityKey, field) {
			t.Errorf("S4 root entity key omits terminal scope %q: %#v", field, product.EntityKey)
		}
		if !runtimeContains(product.FieldNames(), field) {
			t.Errorf("S4 root fields omit terminal scope %q: %#v", field, product.FieldNames())
		}
	}
}

func loadRuntimeCompatibilityProducts(t *testing.T) (*catalog.Catalog, map[string]queryplan.Product) {
	t.Helper()
	path := filepath.Join(runtimeCompatibilityRoot(t), "evaluation", "final-v5-wsl2", "catalog", "benchmark-contract-v1.yaml")
	candidate, err := catalog.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	products := make(map[string]queryplan.Product, len(candidate.Products))
	for _, product := range candidate.Products {
		columns := make(map[string]struct{}, len(product.Fields))
		types := make(map[string]string, len(product.Fields))
		collations := make(map[string]string, len(product.Fields))
		versions := make(map[string]string, len(product.Fields))
		for _, field := range product.Fields {
			columns[field.Name] = struct{}{}
			types[field.Name] = field.Type
			collations[field.Name] = field.Collation
			versions[field.Name] = field.CollationVersion
		}
		aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
		for _, aggregate := range product.AllowedAggregates {
			aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
		}
		products[product.Name] = queryplan.Product{
			Name:              product.Name,
			Columns:           columns,
			AllowedAggregates: aggregates,
			ColumnTypes:       types,
			ColumnCollations:  collations,
			CollationVersions: versions,
			SourceNamespace:   product.FactNamespace,
			Snapshot:          product.Snapshot,
			StableRole:        product.StableRelationRole,
			StableEntityKey:   append([]string(nil), product.EntityKey...),
			LineageDigest:     product.LineageManifestDigest,
			RequiredEvidence:  append([]string(nil), product.Scopes...),
		}
	}
	return candidate, products
}

func runtimeCatalogProduct(t *testing.T, candidate *catalog.Catalog, name string) catalog.Product {
	t.Helper()
	for _, product := range candidate.Products {
		if product.Name == name {
			return product
		}
	}
	t.Fatalf("Catalog product %q not found", name)
	return catalog.Product{}
}

func readRenderedSQLTemplate(t *testing.T, name string, replacements map[string]string) string {
	t.Helper()
	path := filepath.Join(runtimeCompatibilityRoot(t), "evaluation", "final-v5-wsl2", "sql", "contracts", name)
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(value)
	for parameter, replacement := range replacements {
		rendered = strings.ReplaceAll(rendered, parameter, replacement)
	}
	if strings.Contains(rendered, "$") {
		t.Fatalf("SQL template %s retains an unrendered parameter: %s", name, rendered)
	}
	return rendered
}

func renderPlanParameters(value any, replacements map[string]json.Number) (any, error) {
	switch typed := value.(type) {
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			rendered, err := renderPlanParameters(item, replacements)
			if err != nil {
				return nil, err
			}
			result[index] = rendered
		}
		return result, nil
	case map[string]any:
		if parameter, present := typed["$parameter"]; present {
			if len(typed) != 1 {
				return nil, fmt.Errorf("parameter object has sibling members")
			}
			name, ok := parameter.(string)
			if !ok {
				return nil, fmt.Errorf("parameter name is not a string")
			}
			replacement, present := replacements[name]
			if !present {
				return nil, fmt.Errorf("parameter %q has no replacement", name)
			}
			return replacement, nil
		}
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			rendered, err := renderPlanParameters(item, replacements)
			if err != nil {
				return nil, err
			}
			result[key] = rendered
		}
		return result, nil
	default:
		return value, nil
	}
}
func runtimeContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func runtimeCompatibilityRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
