package gateway

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestCatalogDiscoveryToolsExposeSQLContract(t *testing.T) {
	harness := newGatewayHarness(t)

	listed := mustCallGatewayTool(t, harness.service, harness.alice, "list_data_products", map[string]any{})
	if listed["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("list sql_profile = %v, want %s", listed["sql_profile"], catalogReportingSQLProfile)
	}
	products, ok := listed["products"].([]map[string]any)
	if !ok {
		t.Fatalf("products has type %T, want []map[string]any", listed["products"])
	}
	summary := productNamed(t, products, "expense_summary")
	if summary["stable_relation_role"] != "expense_summary" || summary["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("list product omitted stable SQL identity: %#v", summary)
	}
	if got, ok := summary["allowed_aggregates"].([]string); !ok || !equalStrings(got, []string{"sum", "count", "min", "max"}) {
		t.Fatalf("list allowed_aggregates = %#v", summary["allowed_aggregates"])
	}
	month := fieldNamed(t, summary["fields"], "month")
	if month["type"] != "text" || month["collation"] != "en_US.utf8" || month["collation_version"] != "2.36" {
		t.Fatalf("list month field omitted type/collation: %#v", month)
	}

	described := mustCallGatewayTool(t, harness.service, harness.alice, "describe_data_product", map[string]any{"name": "expense_summary"})
	if described["catalog_version"] != harness.catalog.CatalogVersion || described["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("describe omitted catalog/profile identity: %#v", described)
	}
	if described["stable_relation_role"] != "expense_summary" {
		t.Fatalf("describe stable_relation_role = %#v", described["stable_relation_role"])
	}
	if got, ok := described["allowed_functions"].([]string); !ok || !equalStrings(got, []string{"date_trunc", "to_char"}) {
		t.Fatalf("describe allowed_functions = %#v", described["allowed_functions"])
	}
	if got, ok := described["allowed_operators"].([]string); !ok || len(got) == 0 || got[0] != "=" {
		t.Fatalf("describe allowed_operators = %#v", described["allowed_operators"])
	}
	for _, privateKey := range []string{"reporting_view", "source", "fact_namespace", "snapshot_publication"} {
		if _, exposed := described[privateKey]; exposed {
			t.Fatalf("describe exposed private Catalog field %q: %#v", privateKey, described)
		}
	}

	capabilities := mustCallGatewayTool(t, harness.service, harness.alice, "get_sql_capabilities", map[string]any{})
	if capabilities["catalog_version"] != harness.catalog.CatalogVersion || capabilities["sql_profile"] != catalogReportingSQLProfile || capabilities["single_statement_only"] != true || capabilities["lossless_lowering_required"] != true {
		t.Fatalf("unexpected SQL profile envelope: %#v", capabilities)
	}
	join, ok := capabilities["join"].(map[string]any)
	if !ok || join["predicate"] != "equality" || join["graph"] != "connected" || join["max_sources"] != queryplan.MaxJoinSources || join["limit_kind"] != "operational_complexity_guard" || join["arbitrary_topology"] != true || join["self_join"] != false {
		t.Fatalf("unexpected join capabilities: %#v", capabilities["join"])
	}
	joinTypes, ok := join["types"].([]string)
	if !ok || !equalStrings(joinTypes, []string{"INNER"}) {
		t.Fatalf("join types = %#v", join["types"])
	}
	semanticViews, ok := capabilities["semantic_views"].(map[string]any)
	if !ok || semanticViews["query_time_join_with_other_product"] != false ||
		semanticViews["order_by"] != false || semanticViews["limit"] != false ||
		semanticViews["offset"] != false || semanticViews["aggregate_barrier_above"] != "projection_only" ||
		semanticViews["exposure_required"] != true || semanticViews["shared_child_self_join"] != false {
		t.Fatalf("unexpected semantic View query capabilities: %#v", capabilities["semantic_views"])
	}
	rewriteCodes, ok := capabilities["rewrite_error_codes"].([]string)
	if !ok || !contains(rewriteCodes, apierr.CodeViewQueryUnsupported) {
		t.Fatalf("rewrite error codes = %#v, want %s", capabilities["rewrite_error_codes"], apierr.CodeViewQueryUnsupported)
	}
	rebindCodes, ok := capabilities["rebind_error_codes"].([]string)
	if !ok || !equalStrings(rebindCodes, []string{apierr.CodeViewSemanticChanged}) {
		t.Fatalf("rebind error codes = %#v, want %s", capabilities["rebind_error_codes"], apierr.CodeViewSemanticChanged)
	}
	legacyCodes, ok := capabilities["repairable_error_codes"].([]string)
	if !ok || !equalStrings(legacyCodes, rewriteCodes) {
		t.Fatalf("legacy repairable error codes = %#v, want rewrite alias %v",
			capabilities["repairable_error_codes"], rewriteCodes)
	}
	features, ok := capabilities["features"].(map[string]any)
	if !ok || features["inner_equijoins"] != true || features["multi_relation_join_graphs"] != true ||
		features["multiple_equality_predicates_per_edge"] != true || features["outer_joins"] != false ||
		features["subqueries"] != false || features["window_functions"] != false {
		t.Fatalf("unexpected restricted features: %#v", capabilities["features"])
	}
}

func TestCatalogDiscoveryValidationAndToolAdvertisement(t *testing.T) {
	harness := newGatewayHarness(t)

	advertised := make(map[string]bool)
	for _, tool := range harness.service.ListTools(harness.alice) {
		advertised[tool.Name] = true
	}
	for _, name := range []string{"list_data_products", "describe_data_product", "get_sql_capabilities", "query_sql"} {
		if !advertised[name] {
			t.Fatalf("query tools omitted %q: %#v", name, advertised)
		}
	}
	if advertised["execute_plan"] {
		t.Fatalf("ordinary query tools advertised advanced execute_plan: %#v", advertised)
	}

	_, err := callGatewayTool(harness.service, harness.alice, "execute_plan", map[string]any{})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	_, err = callGatewayTool(harness.service, harness.alice, "describe_data_product", map[string]any{"name": "missing"})
	requireToolCode(t, err, apierr.CodeNotFound)
	_, err = callGatewayTool(harness.service, harness.alice, "describe_data_product", map[string]any{"name": "  "})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	_, err = callGatewayTool(harness.service, harness.alice, "get_sql_capabilities", map[string]any{"unexpected": true})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}

func productNamed(t *testing.T, products []map[string]any, name string) map[string]any {
	t.Helper()
	for _, product := range products {
		if product["name"] == name {
			return product
		}
	}
	t.Fatalf("product %q not found in %#v", name, products)
	return nil
}

func fieldNamed(t *testing.T, raw any, name string) map[string]any {
	t.Helper()
	fields, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("fields has type %T, want []map[string]any", raw)
	}
	for _, field := range fields {
		if field["name"] == name {
			return field
		}
	}
	t.Fatalf("field %q not found in %#v", name, fields)
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
