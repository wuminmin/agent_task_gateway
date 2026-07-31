package viewcompiler

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestComposeTransparentJoinMapsEveryOuterReference(t *testing.T) {
	artifact := transparentCompositionArtifact()
	outer := queryplan.QueryPlan{
		Product: "semantic_report",
		Columns: []string{"customer"},
		Filters: []queryplan.Filter{{Column: "amount", Op: "IN", Value: []any{
			json.Number("2"), json.Number("3"),
		}}},
		GroupBy: []string{"customer"},
		Aggregates: []queryplan.Aggregate{
			{Function: "SUM", Column: "amount", Alias: "revenue"},
			{Function: "COUNT", Column: "*", Alias: "rows"},
		},
	}

	composition, err := ComposeQueryPlan("semantic_report", outer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	wantVisible := []string{"orders.tenant_id", "revenue", "rows"}
	if !reflect.DeepEqual(composition.VisibleFields, wantVisible) {
		t.Fatalf("visible fields = %v, want %v", composition.VisibleFields, wantVisible)
	}
	plan := composition.Plan
	if plan.Product != "" || !reflect.DeepEqual(plan.Columns, []string{"orders.tenant_id"}) ||
		!reflect.DeepEqual(plan.GroupBy, []string{"orders.tenant_id"}) {
		t.Fatalf("mapped projection/group = %#v", plan)
	}
	if len(plan.Filters) != 2 || plan.Filters[0].Column != "orders.value" || plan.Filters[1].Column != "lines.value" {
		t.Fatalf("composed filters = %#v", plan.Filters)
	}
	wantAggregates := []queryplan.Aggregate{
		{Function: "SUM", Column: "lines.value", Alias: "revenue"},
		{Function: "COUNT", Column: "*", Alias: "rows"},
	}
	if !reflect.DeepEqual(plan.Aggregates, wantAggregates) {
		t.Fatalf("mapped aggregates = %#v, want %#v", plan.Aggregates, wantAggregates)
	}
	if plan.From == nil || plan.From.JoinMany == nil || len(plan.From.JoinMany.Sources) != 2 {
		t.Fatalf("artifact from was not retained: %#v", plan.From)
	}
	if _, _, err := queryplan.CompileSemantic(plan, compositionProducts()); err != nil {
		t.Fatalf("composed plan does not enter CompileRelational: %v", err)
	}
}

func TestComposeTransparentPlanDeepCopiesInputs(t *testing.T) {
	artifact := transparentCompositionArtifact()
	outerValues := []any{json.Number("2"), json.Number("3")}
	outer := queryplan.QueryPlan{
		Product: "semantic_report", Columns: []string{"amount"},
		Filters: []queryplan.Filter{{Column: "amount", Op: "IN", Value: outerValues}},
	}

	composition, err := ComposeQueryPlan("semantic_report", outer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	outer.Columns[0] = "mutated"
	outerValues[0] = json.Number("99")
	artifact.Plan.Columns[0] = "mutated.field"
	artifact.Plan.Filters[0].Value.([]any)[0] = json.Number("88")
	artifact.Plan.From.JoinMany.Sources[0].Filters[0].Value.([]any)[0] = json.Number("77")
	artifact.Plan.From.JoinMany.On[0].Left = "mutated.field"

	if !reflect.DeepEqual(composition.Plan.Columns, []string{"lines.value"}) {
		t.Fatalf("result aliased input projection: %v", composition.Plan.Columns)
	}
	if got := composition.Plan.Filters[1].Value.([]any)[0]; got != json.Number("2") {
		t.Fatalf("result aliased outer filter values: %v", got)
	}
	if got := composition.Plan.Filters[0].Value.([]any)[0]; got != json.Number("1") {
		t.Fatalf("result aliased artifact filter values: %v", got)
	}
	if got := composition.Plan.From.JoinMany.Sources[0].Filters[0].Value.([]any)[0]; got != json.Number("10") {
		t.Fatalf("result aliased artifact scan filter values: %v", got)
	}
	if got := composition.Plan.From.JoinMany.On[0].Left; got != "orders.id" {
		t.Fatalf("result aliased artifact join predicates: %q", got)
	}
	if !reflect.DeepEqual(composition.VisibleFields, []string{"lines.value"}) {
		t.Fatalf("visible fields = %v", composition.VisibleFields)
	}
}

func TestComposeAggregateBarrierPreservesPublicSubsetAndReorder(t *testing.T) {
	artifact := aggregateCompositionArtifact()
	tests := []struct {
		name           string
		columns        []string
		wantFields     []string
		wantAggregates []string
		wantVisible    []string
	}{
		{
			name: "mixed reorder", columns: []string{"rows", "customer", "revenue"},
			wantFields: []string{"orders.tenant_id"}, wantAggregates: []string{"rows", "revenue"},
			wantVisible: []string{"rows", "orders.tenant_id", "revenue"},
		},
		{
			name: "aggregate subset", columns: []string{"revenue"},
			wantAggregates: []string{"revenue"}, wantVisible: []string{"revenue"},
		},
		{
			name: "field subset", columns: []string{"customer"},
			wantFields: []string{"orders.tenant_id"}, wantVisible: []string{"orders.tenant_id"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			composition, err := ComposeQueryPlan("customer_totals", queryplan.QueryPlan{
				Product: "customer_totals", Columns: append([]string(nil), test.columns...),
			}, artifact)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(composition.Plan.Columns, test.wantFields) {
				t.Fatalf("fields = %v, want %v", composition.Plan.Columns, test.wantFields)
			}
			var aliases []string
			for _, aggregate := range composition.Plan.Aggregates {
				aliases = append(aliases, aggregate.Alias)
			}
			if !reflect.DeepEqual(aliases, test.wantAggregates) {
				t.Fatalf("aggregates = %v, want %v", aliases, test.wantAggregates)
			}
			if !reflect.DeepEqual(composition.VisibleFields, test.wantVisible) {
				t.Fatalf("visible fields = %v, want %v", composition.VisibleFields, test.wantVisible)
			}
			if !reflect.DeepEqual(composition.Plan.GroupBy, artifact.Plan.GroupBy) ||
				!reflect.DeepEqual(composition.Plan.Filters, artifact.Plan.Filters) || composition.Plan.From == nil {
				t.Fatalf("aggregation input was not retained: %#v", composition.Plan)
			}
			if _, _, err := queryplan.CompileSemantic(composition.Plan, compositionProducts()); err != nil {
				t.Fatalf("barrier projection does not compile: %v", err)
			}
		})
	}
}

func TestComposeAggregateBarrierDeepCopiesRetainedPlan(t *testing.T) {
	artifact := aggregateCompositionArtifact()
	composition, err := ComposeQueryPlan("customer_totals", queryplan.QueryPlan{
		Product: "customer_totals", Columns: []string{"revenue", "customer"},
	}, artifact)
	if err != nil {
		t.Fatal(err)
	}
	composition.Plan.GroupBy[0] = "mutated.field"
	composition.Plan.Filters[0].Value.([]any)[0] = json.Number("99")
	composition.Plan.From.JoinMany.Sources[0].Filters[0].Value.([]any)[0] = json.Number("99")
	composition.Plan.Aggregates[0].Alias = "mutated"
	composition.VisibleFields[0] = "mutated"

	if artifact.Plan.GroupBy[0] != "orders.tenant_id" ||
		artifact.Plan.Filters[0].Value.([]any)[0] != json.Number("1") ||
		artifact.Plan.From.JoinMany.Sources[0].Filters[0].Value.([]any)[0] != json.Number("10") ||
		artifact.Plan.Aggregates[0].Alias != "revenue" {
		t.Fatalf("composition mutation changed artifact: %#v", artifact.Plan)
	}
}

func TestComposeRejectsOperationsAcrossAggregateBarrier(t *testing.T) {
	artifact := aggregateCompositionArtifact()
	tests := []struct {
		name   string
		mutate func(*queryplan.QueryPlan)
	}{
		{name: "filter", mutate: func(plan *queryplan.QueryPlan) {
			plan.Filters = []queryplan.Filter{{Column: "revenue", Op: ">", Value: json.Number("0")}}
		}},
		{name: "group", mutate: func(plan *queryplan.QueryPlan) { plan.GroupBy = []string{"customer"} }},
		{name: "reaggregate", mutate: func(plan *queryplan.QueryPlan) {
			plan.Aggregates = []queryplan.Aggregate{{Function: "sum", Column: "revenue", Alias: "total"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outer := queryplan.QueryPlan{Product: "customer_totals", Columns: []string{"customer"}}
			test.mutate(&outer)
			_, err := ComposeQueryPlan("customer_totals", outer, artifact)
			assertCompositionCode(t, err)
		})
	}
}

func TestComposeRejectsOrderAndPaginationForEveryExpansion(t *testing.T) {
	artifacts := []struct {
		name     string
		product  string
		artifact Artifact
		column   string
	}{
		{name: "join", product: "semantic_report", artifact: transparentCompositionArtifact(), column: "customer"},
		{name: "single scan", product: "single_view", artifact: singleScanCompositionArtifact(), column: "id"},
	}
	mutations := []struct {
		name   string
		mutate func(*queryplan.QueryPlan)
	}{
		{name: "order", mutate: func(plan *queryplan.QueryPlan) {
			plan.OrderBy = []queryplan.Order{{Column: plan.Columns[0], Direction: "asc"}}
		}},
		{name: "limit", mutate: func(plan *queryplan.QueryPlan) { plan.Limit = 10 }},
		{name: "offset", mutate: func(plan *queryplan.QueryPlan) { plan.Offset = 10 }},
	}
	for _, fixture := range artifacts {
		for _, mutation := range mutations {
			t.Run(fixture.name+"/"+mutation.name, func(t *testing.T) {
				outer := queryplan.QueryPlan{Product: fixture.product, Columns: []string{fixture.column}}
				mutation.mutate(&outer)
				_, err := ComposeQueryPlan(fixture.product, outer, fixture.artifact)
				assertCompositionCode(t, err)
			})
		}
	}
}

func TestComposeRejectsIncompleteOuterMappings(t *testing.T) {
	artifact := transparentCompositionArtifact()
	tests := []struct {
		name  string
		outer queryplan.QueryPlan
	}{
		{name: "wrong root", outer: queryplan.QueryPlan{Product: "other", Columns: []string{"customer"}}},
		{name: "nested from", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"customer"},
			From: &queryplan.From{Scan: &queryplan.Scan{Product: "orders", Role: "orders"}}}},
		{name: "empty select", outer: queryplan.QueryPlan{Product: "semantic_report"}},
		{name: "unknown projection", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"missing"}}},
		{name: "duplicate projection", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"customer", "customer"}}},
		{name: "unknown filter", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"customer"},
			Filters: []queryplan.Filter{{Column: "missing", Op: "=", Value: json.Number("1")}}}},
		{name: "duplicate group", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"customer"},
			GroupBy: []string{"customer", "customer"}}},
		{name: "selected field not grouped", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"amount"},
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "amount", Alias: "total"}}}},
		{name: "unknown aggregate", outer: queryplan.QueryPlan{Product: "semantic_report",
			Aggregates: []queryplan.Aggregate{{Function: "avg", Column: "amount", Alias: "average"}}}},
		{name: "sum star", outer: queryplan.QueryPlan{Product: "semantic_report",
			Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "*", Alias: "total"}}}},
		{name: "alias collision", outer: queryplan.QueryPlan{Product: "semantic_report", Columns: []string{"customer"},
			GroupBy: []string{"customer"}, Aggregates: []queryplan.Aggregate{{Function: "count", Column: "*", Alias: "customer"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ComposeQueryPlan("semantic_report", test.outer, artifact)
			assertCompositionCode(t, err)
		})
	}
}

func TestComposeRejectsMalformedArtifactBindings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{name: "legacy product", mutate: func(artifact *Artifact) { artifact.Plan.Product = "semantic_report" }},
		{name: "missing from", mutate: func(artifact *Artifact) { artifact.Plan.From = nil }},
		{name: "multiple from", mutate: func(artifact *Artifact) {
			artifact.Plan.From.Scan = &queryplan.Scan{Product: "orders", Role: "orders"}
		}},
		{name: "unsupported binary join", mutate: func(artifact *Artifact) {
			artifact.Plan.From = &queryplan.From{Join: &queryplan.Join{}}
		}},
		{name: "output count", mutate: func(artifact *Artifact) { artifact.Outputs = artifact.Outputs[:2] }},
		{name: "duplicate public name", mutate: func(artifact *Artifact) { artifact.Outputs[1].Name = artifact.Outputs[0].Name }},
		{name: "field mismatch", mutate: func(artifact *Artifact) { artifact.Outputs[0].FieldID = "orders.id" }},
		{name: "duplicate plan field", mutate: func(artifact *Artifact) { artifact.Plan.Columns[1] = artifact.Plan.Columns[0] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := transparentCompositionArtifact()
			test.mutate(&artifact)
			_, err := ComposeQueryPlan("semantic_report", queryplan.QueryPlan{
				Product: "semantic_report", Columns: []string{"customer"},
			}, artifact)
			assertCompositionCode(t, err)
		})
	}

	t.Run("aggregate metadata mismatch", func(t *testing.T) {
		artifact := aggregateCompositionArtifact()
		artifact.Outputs[0].Argument = "orders.value"
		_, err := ComposeQueryPlan("customer_totals", queryplan.QueryPlan{
			Product: "customer_totals", Columns: []string{"revenue"},
		}, artifact)
		assertCompositionCode(t, err)
	})
}

func transparentCompositionArtifact() Artifact {
	return Artifact{
		Root: RelationName{Schema: "semantic", Name: "report"},
		Plan: queryplan.QueryPlan{
			From: &queryplan.From{JoinMany: &queryplan.JoinMany{
				Sources: []queryplan.Scan{
					{Product: "orders", Role: "orders", Filters: []queryplan.Filter{{
						Column: "tenant_id", Op: "IN", Value: []any{json.Number("10"), json.Number("20")},
					}}},
					{Product: "lines", Role: "lines"},
				},
				On: []queryplan.JoinPredicate{{Left: "orders.id", Right: "lines.parent_id"}},
			}},
			Columns: []string{"orders.tenant_id", "lines.value", "orders.id"},
			Filters: []queryplan.Filter{{Column: "orders.value", Op: "IN", Value: []any{
				json.Number("1"), json.Number("4"),
			}}},
		},
		Outputs: []Output{
			{Name: "customer", Kind: OutputField, FieldID: "orders.tenant_id", SQLType: "integer"},
			{Name: "amount", Kind: OutputField, FieldID: "lines.value", SQLType: "integer"},
			{Name: "order_id", Kind: OutputField, FieldID: "orders.id", SQLType: "integer"},
		},
	}
}

func aggregateCompositionArtifact() Artifact {
	artifact := transparentCompositionArtifact()
	artifact.Root = RelationName{Schema: "semantic", Name: "customer_totals"}
	artifact.Plan.Columns = []string{"orders.tenant_id"}
	artifact.Plan.GroupBy = []string{"orders.tenant_id"}
	artifact.Plan.Aggregates = []queryplan.Aggregate{
		{Function: "sum", Column: "lines.value", Alias: "revenue"},
		{Function: "count", Column: "*", Alias: "rows"},
	}
	// Deliberately interleave aggregate and field outputs. QueryPlan stores
	// those kinds separately, while Composition.VisibleFields must retain this
	// public order and any outer reorder/subset.
	artifact.Outputs = []Output{
		{Name: "revenue", Kind: OutputAggregate, Function: "sum", Argument: "lines.value", SQLType: "bigint"},
		{Name: "customer", Kind: OutputField, FieldID: "orders.tenant_id", SQLType: "integer"},
		{Name: "rows", Kind: OutputAggregate, Function: "count", Argument: "*", SQLType: "bigint"},
	}
	return artifact
}

func singleScanCompositionArtifact() Artifact {
	return Artifact{
		Root: RelationName{Schema: "semantic", Name: "single_view"},
		Plan: queryplan.QueryPlan{
			From:    &queryplan.From{Scan: &queryplan.Scan{Product: "orders", Role: "orders"}},
			Columns: []string{"orders.id"},
		},
		Outputs: []Output{{Name: "id", Kind: OutputField, FieldID: "orders.id", SQLType: "integer"}},
	}
}

func compositionProducts() map[string]queryplan.Product {
	return map[string]queryplan.Product{"orders": testProduct("orders"), "lines": testProduct("lines")}
}

func assertCompositionCode(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("composition unexpectedly succeeded")
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeCompositionUnsupported {
		t.Fatalf("error = %#v, want %s", err, CodeCompositionUnsupported)
	}
	if !strings.Contains(typed.Error(), string(CodeCompositionUnsupported)) {
		t.Fatalf("structured error lost code: %v", typed)
	}
}
