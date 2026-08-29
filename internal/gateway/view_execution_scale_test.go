//go:build taskgate_scale

// These cases prepare an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so they belong on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"context"
	"reflect"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

func TestExecutePlanSemanticViewCarriesRegistryExpectationToPairedQueries(t *testing.T) {
	harness := newGatewayHarness(t)
	fixture := newSemanticRuntimeFixture(t, false)
	harness.catalog.Products = append(harness.catalog.Products, fixture.root)
	for _, scope := range fixture.service.catalog.Scopes {
		if scope.Name == "business_unit" {
			harness.catalog.Scopes = append(harness.catalog.Scopes, scope)
			break
		}
	}
	registryConnector := &registryFakeConnector{fakeConnector: harness.connector, snapshot: fixture.registry}
	harness.service.connector = registryConnector
	indexes := harness.installCatalogV4SnapshotRegistry(t)
	taskID := requestAndApproveSemanticRuntimeTask(t, harness)

	// Build the exact terminal ordinal row contract used by the public path,
	// then feed one immutable published row through the streaming fake.
	fixture.service = harness.service
	fixture.grant.Exposure.ProfileVersion = exposure.ProfileV4
	amountOuter := queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, amountOuter, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.artifact, composition, fixture.binding, amountOuter,
	)
	if err != nil {
		t.Fatalf("prepare expected V4 row contract: %v", err)
	}
	bound := prepared.Exposure.ordinal
	if bound == nil {
		t.Fatal("expected V4 ordinal binding")
	}
	row := map[string]any{
		"receipt_no": "TR-2026-0001", "department": "销售部", "amount": "1680.00",
	}
	harness.connector.result = scanVisibleResult(t, bound.Program, []map[string]any{row})
	harness.connector.result.DatabaseTime = 2 * time.Millisecond
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRow := make([]any, len(provenanceColumns))
	for _, source := range bound.Program.Sources {
		entityKey := ordinalFixtureEntityKey(t, source, row)
		handle, present := indexes["expense-detail-v1"].LookupRowHandle(entityKey)
		if !present {
			t.Fatalf("published terminal index misses entity %q", entityKey)
		}
		provenanceRow[positions[source.HandleAlias]] = uint64(handle)
		for _, field := range source.EvidenceFields {
			provenanceRow[positions[field.ProvenanceAlias]] = row[field.Column]
		}
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1,
		DatabaseTime: time.Millisecond,
	}

	mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": "semantic-view-execution-e2e",
		"plan": queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}},
	})
	record, err := harness.store.GetQueryByRequestID(context.Background(), taskID, "semantic-view-execution-e2e")
	if err != nil || record.ViewBindingDigest == "" {
		t.Fatalf("query record omitted View binding evidence: record=%#v err=%v", record, err)
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("connector requests = %d, want visible/provenance pair", len(harness.connector.requests))
	}
	for index, request := range harness.connector.requests {
		if request.ViewRegistry == nil {
			t.Fatalf("paired request %d omitted ViewRegistry", index)
		}
		if request.ViewRegistry.ExpectedRevisionDigest != fixture.registry.RevisionDigest ||
			len(request.ViewRegistry.Roots) != 1 || request.ViewRegistry.Roots[0] != fixture.artifact.Root {
			t.Fatalf("paired request %d ViewRegistry = %#v", index, request.ViewRegistry)
		}
		if !reflect.DeepEqual(request.ViewRegistry.BaseProducts,
			map[string]string{"reporting.expense_detail": "expense_detail"}) {
			t.Fatalf("paired request %d terminal closure = %#v", index, request.ViewRegistry.BaseProducts)
		}
	}
}

func TestPrepareSemanticViewPlanV4BindsOnlyTerminalOrdinalSources(t *testing.T) {
	fixture := newSemanticRuntimeFixture(t, false)
	installSemanticRuntimeSnapshotRegistry(t, fixture.service)
	fixture.grant.Exposure.ProfileVersion = exposure.ProfileV4
	if fixture.root.SnapshotPublication != "" {
		t.Fatalf("fixture root unexpectedly owns snapshot publication %q", fixture.root.SnapshotPublication)
	}
	amountOuter := queryplan.QueryPlan{Product: fixture.root.Name, Columns: []string{"amount"}}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, amountOuter, fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := fixture.service.prepareSemanticViewPlan(
		fixture.grant, fixture.artifact, composition, fixture.binding, amountOuter,
	)
	if err != nil {
		t.Fatalf("prepare V4 semantic View: %v", err)
	}
	if prepared.Exposure.ordinal == nil {
		t.Fatal("V4 semantic View did not bind an ordinal execution")
	}
	sources := prepared.Exposure.ordinal.Program.Sources
	if len(sources) != 1 || sources[0].Product != fixture.terminal.Name ||
		sources[0].SourceAlias != fixture.terminal.StableRelationRole {
		t.Fatalf("ordinal sources = %#v, want terminal only", sources)
	}
	if _, present := prepared.Exposure.ordinal.Indexes[fixture.terminal.StableRelationRole]; !present {
		t.Fatalf("ordinal indexes = %#v, terminal role missing", prepared.Exposure.ordinal.Indexes)
	}
	if _, present := prepared.Exposure.ordinal.Indexes[fixture.root.StableRelationRole]; present {
		t.Fatalf("semantic root incorrectly became an ordinal source: %#v", prepared.Exposure.ordinal.Indexes)
	}
	if len(prepared.Exposure.ordinal.SidecarGrants) != 1 {
		t.Fatalf("sidecar grants = %#v, want terminal publication only", prepared.Exposure.ordinal.SidecarGrants)
	}
}
