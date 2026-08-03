package finalv5contracts

import (
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
