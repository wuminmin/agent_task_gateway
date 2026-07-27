package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

func TestRelationalAlgebraNormalFormBuildsForJoinUnionAndGrouping(t *testing.T) {
	loaded, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	join := queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
		Left: queryplan.Scan{Product: "expense_detail", Role: "expense_detail"}, Right: queryplan.Scan{Product: "expense_summary", Role: "expense_summary"},
		On: []queryplan.JoinPredicate{{Left: "expense_detail.department", Right: "expense_summary.department"}},
	}}, Columns: []string{"expense_detail.receipt_no", "expense_summary.total_amount"}}
	grouped := join
	grouped.Columns = []string{"expense_detail.department"}
	grouped.Aggregates = []queryplan.Aggregate{{Function: "sum", Column: "expense_detail.amount", Alias: "total"}}
	grouped.GroupBy = []string{"expense_detail.department"}
	union := queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{Role: "expense_summary", Columns: []string{"department", "month"}, Left: queryplan.Scan{Product: "expense_summary", Role: "left_branch", Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}}, Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch", Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}}}}, Columns: []string{"expense_summary.department"}}
	for _, test := range []struct {
		name string
		plan queryplan.QueryPlan
	}{{"join", join}, {"grouped_join", grouped}, {"union", union}} {
		t.Run(test.name, func(t *testing.T) {
			names, err := queryplan.RelationalProductNames(test.plan)
			if err != nil {
				t.Fatal(err)
			}
			queryProducts := make(map[string]queryplan.Product)
			catalogProducts := make(map[string]catalog.Product)
			approved := make(map[string][]string)
			for _, name := range names {
				product, _ := loaded.LookupProduct(name)
				columns := product.FieldNames()
				approved[name] = columns
				queryProducts[name] = relationalQueryProduct(product, stringSetFromSlice(columns))
				catalogProducts[name] = product
			}
			compiled, err := queryplan.CompileRelational(test.plan, queryProducts)
			if err != nil {
				t.Fatal(err)
			}
			context, err := buildRelationalExposureContext(test.plan, compiled, catalogProducts, approved)
			if err != nil {
				t.Fatal(err)
			}
			if context.planDigest == "" || context.algebraNormalForm == nil {
				t.Fatal("relational plan lacks algebra identity")
			}
		})
	}
}

// TestRelationalOnlinePathAgainstPostgreSQL executes the public Join and
// UNION DISTINCT plans plus their provenance companions against the real
// reporting views. The Compose verification job supplies this DSN; ordinary
// unit runs skip the database-dependent evidence.
func TestRelationalOnlinePathAgainstPostgreSQL(t *testing.T) {
	dsn := os.Getenv("BUSINESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for relational online tests")
	}
	loaded, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: dsn, StatementTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 200, MaxConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	join := queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
		Left: queryplan.Scan{Product: "expense_detail", Role: "expense_detail"}, Right: queryplan.Scan{Product: "expense_summary", Role: "expense_summary"},
		On: []queryplan.JoinPredicate{{Left: "expense_detail.department", Right: "expense_summary.department"}},
	}}, Columns: []string{"expense_detail.receipt_no", "expense_summary.total_amount"}}
	swappedJoin := queryplan.QueryPlan{From: &queryplan.From{Join: &queryplan.Join{
		Left: queryplan.Scan{Product: "expense_summary", Role: "expense_summary"}, Right: queryplan.Scan{Product: "expense_detail", Role: "expense_detail"},
		On: []queryplan.JoinPredicate{{Left: "expense_summary.department", Right: "expense_detail.department"}},
	}}, Columns: append([]string(nil), join.Columns...)}
	first := executeRelationalPostgresObservation(t, ctx, connector, loaded, join)
	second := executeRelationalPostgresObservation(t, ctx, connector, loaded, swappedJoin)
	firstDigest, err := exposure.ObservationDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := exposure.ObservationDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("PostgreSQL join operand swap changed exposure: %s != %s", firstDigest, secondDigest)
	}
	if len(first.Release) == 0 || len(first.Influence) <= len(first.Release) {
		t.Fatalf("join observation lacks fanout dependencies: release=%d dependency=%d", len(first.Release), len(first.Influence))
	}
	groupedJoin := join
	groupedJoin.Columns = nil
	groupedJoin.Aggregates = []queryplan.Aggregate{{Function: "sum", Column: "expense_detail.amount", Alias: "total"}}
	groupedJoin.GroupBy = []string{"expense_detail.department"}
	groupedJoinObservation := executeRelationalPostgresObservation(t, ctx, connector, loaded, groupedJoin)
	if len(groupedJoinObservation.Release) != 1 || len(groupedJoinObservation.Influence) <= len(groupedJoinObservation.Release) {
		t.Fatalf("grouped join did not compose online annotations: release=%d dependency=%d", len(groupedJoinObservation.Release), len(groupedJoinObservation.Influence))
	}

	union := queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
		Role: "expense_summary", Columns: []string{"department", "month"},
		Left:  queryplan.Scan{Product: "expense_summary", Role: "left_branch", Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}},
		Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch", Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}},
	}}, Columns: []string{"expense_summary.department"}}
	swappedUnion := union
	unionNode := *union.From.UnionDistinct
	unionNode.Left, unionNode.Right = unionNode.Right, unionNode.Left
	from := *union.From
	from.UnionDistinct = &unionNode
	swappedUnion.From = &from
	unionFirst := executeRelationalPostgresObservation(t, ctx, connector, loaded, union)
	unionSecond := executeRelationalPostgresObservation(t, ctx, connector, loaded, swappedUnion)
	unionFirstDigest, err := exposure.ObservationDigest(unionFirst)
	if err != nil {
		t.Fatal(err)
	}
	unionSecondDigest, err := exposure.ObservationDigest(unionSecond)
	if err != nil {
		t.Fatal(err)
	}
	if unionFirstDigest != unionSecondDigest {
		t.Fatalf("PostgreSQL UNION operand exchange changed exposure: %s != %s", unionFirstDigest, unionSecondDigest)
	}
	if len(unionFirst.Release) == 0 || len(unionFirst.Influence) <= len(unionFirst.Release) {
		t.Fatalf("UNION observation omitted member/hidden-field dependencies: release=%d dependency=%d", len(unionFirst.Release), len(unionFirst.Influence))
	}
	groupedUnion := union
	groupedUnion.Columns = []string{"expense_summary.department"}
	groupedUnion.Aggregates = []queryplan.Aggregate{{Function: "count", Column: "*", Alias: "members"}}
	groupedUnion.GroupBy = []string{"expense_summary.department"}
	groupedUnionObservation := executeRelationalPostgresObservation(t, ctx, connector, loaded, groupedUnion)
	if len(groupedUnionObservation.Release) != 2 || len(groupedUnionObservation.Influence) <= len(groupedUnionObservation.Release) {
		t.Fatalf("grouped UNION did not compose online annotations: release=%d dependency=%d", len(groupedUnionObservation.Release), len(groupedUnionObservation.Influence))
	}
}

// TestRelationalGatewayEndToEndAgainstPostgreSQL crosses both real databases:
// the public execute_plan path runs the paired statements in Business
// PostgreSQL, settles their facts in Control PostgreSQL, and only then returns
// the stripped result. It is optional because it requires both test DSNs.
func TestRelationalGatewayEndToEndAgainstPostgreSQL(t *testing.T) {
	dsn := os.Getenv("BUSINESS_TEST_POSTGRES_DSN")
	if dsn == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN and CONTROL_TEST_POSTGRES_DSN are required")
	}
	harness := newGatewayHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(harness.catalog.Sources) != 1 {
		t.Fatalf("gateway E2E requires one Catalog source, got %d", len(harness.catalog.Sources))
	}
	source := harness.catalog.Sources[0]
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2,
		ExpectedSchema:       relationalExpectedSchema(t, harness.catalog),
		ExpectedSchemaDigest: source.SchemaDigest,
		ExpectedAttestation: dataconnector.ExpectedAttestation{
			DatasourceID: source.DatasourceID, Database: source.Database, User: source.User,
			PostgreSQLMajorVersion: source.PostgreSQLMajorVersion,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	attestation, err := connector.Attestation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	harness.connector.attestation = attestation
	harness.service.connector = connector

	harness.createTaskWithGrantAndExposureProfile(t, "task-v2-pg-join", nil,
		control.ExposureLimits{ReleaseFacts: 500, InfluenceFacts: 500}, exposure.ProfileV2,
		[]string{"expense_detail", "expense_summary"}, map[string][]string{
			"expense_detail":  {"amount", "department", "receipt_no"},
			"expense_summary": {"department", "month", "total_amount"},
		}, domain.SensitivityHigh)
	join := map[string]any{
		"from": map[string]any{"join": map[string]any{
			"left":  map[string]any{"product": "expense_detail", "role": "expense_detail"},
			"right": map[string]any{"product": "expense_summary", "role": "expense_summary"},
			"on":    []map[string]any{{"left": "expense_detail.department", "right": "expense_summary.department"}},
		}},
		"columns": []string{"expense_detail.receipt_no", "expense_summary.total_amount"},
	}
	joinResult := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-pg-join", "request_id": "pg-join", "plan": join,
	})
	joinCharge := joinResult["exposure"].(control.ExposureCharge)
	if joinCharge.ChargedReleaseFacts == 0 || joinCharge.ChargedInfluenceFacts <= joinCharge.ChargedReleaseFacts {
		t.Fatalf("real gateway join did not settle dependency facts: %+v", joinCharge)
	}
	if columns := joinResult["columns"].([]dataconnector.Column); len(columns) != 2 {
		t.Fatalf("real gateway join leaked internal fields: %+v", columns)
	}

	harness.createTaskWithGrantAndExposureProfile(t, "task-v2-pg-union", nil,
		control.ExposureLimits{ReleaseFacts: 500, InfluenceFacts: 500}, exposure.ProfileV2,
		[]string{"expense_summary"}, map[string][]string{
			"expense_summary": {"department", "expense_type", "month"},
		}, domain.SensitivityLow)
	union := map[string]any{
		"from": map[string]any{"union_distinct": map[string]any{
			"role": "expense_summary", "columns": []string{"department", "month"},
			"left":  map[string]any{"product": "expense_summary", "role": "left_branch", "filters": []map[string]any{{"column": "expense_type", "op": "=", "value": "机票"}}},
			"right": map[string]any{"product": "expense_summary", "role": "right_branch", "filters": []map[string]any{{"column": "expense_type", "op": "=", "value": "酒店"}}},
		}},
		"columns": []string{"expense_summary.department"},
	}
	unionResult := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-pg-union", "request_id": "pg-union", "plan": union,
	})
	unionCharge := unionResult["exposure"].(control.ExposureCharge)
	if unionCharge.ChargedReleaseFacts == 0 || unionCharge.ChargedInfluenceFacts <= unionCharge.ChargedReleaseFacts {
		t.Fatalf("real gateway UNION did not settle member dependencies: %+v", unionCharge)
	}
	if columns := unionResult["columns"].([]dataconnector.Column); len(columns) != 1 || columns[0].Name != "expense_summary.department" {
		t.Fatalf("real gateway UNION leaked its hidden distinct field: %+v", columns)
	}
}

func executeRelationalPostgresObservation(t *testing.T, ctx context.Context, connector *dataconnector.Connector, loaded *catalog.Catalog, plan queryplan.QueryPlan) exposure.Observation {
	t.Helper()
	names, err := queryplan.RelationalProductNames(plan)
	if err != nil {
		t.Fatal(err)
	}
	queryProducts := make(map[string]queryplan.Product, len(names))
	catalogProducts := make(map[string]catalog.Product, len(names))
	approved := make(map[string][]string, len(names))
	grant := sqlpolicy.Grant{}
	for _, name := range names {
		product, ok := loaded.LookupProduct(name)
		if !ok {
			t.Fatalf("missing product %s", name)
		}
		columns := product.FieldNames()
		approved[name] = columns
		queryProducts[name] = relationalQueryProduct(product, stringSetFromSlice(columns))
		catalogProducts[name] = product
		parts := splitReportingView(t, product.ReportingView)
		grant.Products = append(grant.Products, sqlpolicy.ProductGrant{LogicalName: name, PhysicalSchema: parts[0], PhysicalView: parts[1], ApprovedColumns: columns, AllowedFunctions: product.AllowedFunctions, AllowedAggregates: product.AllowedAggregates, AllowedOperators: product.AllowedOperators, MandatoryScope: []sqlpolicy.ScopePredicate{{Column: "department", Operator: sqlpolicy.ScopeEqual, Values: []string{"销售部"}}}})
	}
	compiled, err := queryplan.CompileRelational(plan, queryProducts)
	if err != nil {
		t.Fatal(err)
	}
	exposureContext, err := buildRelationalExposureContext(plan, compiled, catalogProducts, approved)
	if err != nil {
		t.Fatal(err)
	}
	grant, err = exposureContext.extendGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	visible, err := engine.Authorize(sqlpolicy.Request{SQL: compiled.VisibleSQL, Grant: grant, RowLimit: 100})
	if err != nil {
		t.Fatalf("authorize relational visible SQL: %v\n%s", err, compiled.VisibleSQL)
	}
	provenance, err := engine.Authorize(sqlpolicy.Request{SQL: compiled.ProvenanceSQL, Grant: grant, RowLimit: 101})
	if err != nil {
		t.Fatalf("authorize relational provenance SQL: %v\n%s", err, compiled.ProvenanceSQL)
	}
	pair, err := connector.QueryPair(ctx, dataconnector.QueryPairRequest{Visible: dataconnector.QueryRequest{SQL: visible.SQL, StatementTimeout: 5 * time.Second, MaxRows: 100}, Provenance: dataconnector.QueryRequest{SQL: provenance.SQL, StatementTimeout: 5 * time.Second, MaxRows: 100}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := exposureContext.deriveObservation(pair.Visible, pair.Provenance, exposure.ProfileV2)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

func splitReportingView(t *testing.T, value string) []string {
	t.Helper()
	for index := range value {
		if value[index] == '.' {
			return []string{value[:index], value[index+1:]}
		}
	}
	t.Fatalf("invalid reporting view %q", value)
	return nil
}

func relationalExpectedSchema(t *testing.T, loaded *catalog.Catalog) []dataconnector.ViewSchema {
	t.Helper()
	result := make([]dataconnector.ViewSchema, 0, len(loaded.Products))
	for _, product := range loaded.Products {
		parts := splitReportingView(t, product.ReportingView)
		columns := make([]dataconnector.SchemaColumn, 0, len(product.Fields))
		for _, field := range product.Fields {
			columns = append(columns, dataconnector.SchemaColumn{
				Name: field.Name, PostgreSQLType: field.Type, Collation: field.Collation,
				CollationVersion: field.CollationVersion, CollationDeterministic: field.Collation != "",
			})
		}
		result = append(result, dataconnector.ViewSchema{Schema: parts[0], View: parts[1], Columns: columns})
	}
	return result
}
