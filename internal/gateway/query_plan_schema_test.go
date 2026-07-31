package gateway

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestQueryPlanSchemaJoinManyUsesOperationalSourceGuard(t *testing.T) {
	asMap := func(label string, value any) map[string]any {
		result, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s schema has type %T, want map[string]any", label, value)
		}
		return result
	}

	root := queryPlanSchema()
	rootProperties := asMap("query plan properties", root["properties"])
	from := asMap("from", rootProperties["from"])
	fromProperties := asMap("from properties", from["properties"])
	joinMany := asMap("join_many", fromProperties["join_many"])
	joinProperties := asMap("join_many properties", joinMany["properties"])
	sources := asMap("join_many sources", joinProperties["sources"])
	if sources["minItems"] != 2 {
		t.Fatalf("join_many sources minItems = %#v, want 2", sources["minItems"])
	}
	if maximum := sources["maxItems"]; maximum != queryplan.MaxJoinSources {
		t.Fatalf("join_many sources maxItems = %#v, want operational guard %d", maximum, queryplan.MaxJoinSources)
	}
}
