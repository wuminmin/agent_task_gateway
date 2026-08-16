package main

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func baselineOperation(workload, scale, mode string) experiment.AdapterOperation {
	return experiment.AdapterOperation{ExperimentID: "baseline", WorkloadID: workload, Scale: scale, Mode: mode}
}

// TestBaselineCellsResolveToTheirFrozenBinding checks the properties a measured
// sample depends on: the arms differ only by the reporting schema, the Task
// approves exactly the bound Products under the mandatory partition scope, and
// the frozen threshold reaches both arms.
func TestBaselineCellsResolveToTheirFrozenBinding(t *testing.T) {
	orders, err := resolveBaselineExecutionCell(baselineOperation("S1", "SF10", "novel"))
	if err != nil {
		t.Fatalf("resolve S1/SF10/novel: %v", err)
	}
	if orders.Contract.ExpectedRows != 50_000 || orders.Contract.ExpectedColumns != 2 {
		t.Fatalf("S1/SF10 expects %dx%d, want 50000x2",
			orders.Contract.ExpectedRows, orders.Contract.ExpectedColumns)
	}
	if !strings.Contains(orders.DirectSQL, "reporting.provsql_orders") {
		t.Fatal("the Direct arm must read the reporting view")
	}
	if strings.Contains(orders.BDGSQL, "reporting.") {
		t.Fatal("the BDG arm must name the Product, not the reporting view")
	}
	if !strings.Contains(orders.BDGSQL, "50000") || !strings.Contains(orders.DirectSQL, "50000") {
		t.Fatal("the frozen orderkey threshold did not reach both arms")
	}
	if got := orders.Task.DataProducts; len(got) != 1 || got[0] != "provsql_orders" {
		t.Fatalf("S1 approved products %v, want provsql_orders alone", got)
	}
	if values := orders.Task.Scopes["partition_key"]; len(values) != 1 || values[0] != "1" {
		t.Fatalf("S1 scopes = %v, want the mandatory partition key", orders.Task.Scopes)
	}

	join, err := resolveBaselineExecutionCell(baselineOperation("S2", "SF10", "novel"))
	if err != nil {
		t.Fatalf("resolve S2/SF10/novel: %v", err)
	}
	if len(join.Task.DataProducts) != 2 {
		t.Fatalf("S2 approved %d products, want both frozen relations", len(join.Task.DataProducts))
	}
	for _, product := range join.Task.DataProducts {
		if len(join.Task.Columns[product]) == 0 {
			t.Fatalf("S2 approved product %q without columns", product)
		}
	}
	if join.Contract.ExpectedRows != 3 {
		t.Fatalf("S2 expects %d rows, want the three frozen status groups", join.Contract.ExpectedRows)
	}
	// S1 and S2 share every scale and mode, so a resolver that ignored the
	// workload would return one cell's query for the other's coordinate.
	if join.BDGSQL == orders.BDGSQL {
		t.Fatal("S1 and S2 resolved the same BDG query")
	}
}

// TestBaselineDirectCellsCarryNoGovernedArm records the cost split the schedule
// depends on: a direct cell is plain PostgreSQL with no Task and no approval,
// which is why its samples are nearly free.
func TestBaselineDirectCellsCarryNoGovernedArm(t *testing.T) {
	for _, scale := range []string{"SF1", "SF10"} {
		for _, workload := range []string{"S1", "S2"} {
			cell, err := resolveBaselineExecutionCell(baselineOperation(workload, scale, "direct"))
			if err != nil {
				t.Fatalf("resolve %s/%s/direct: %v", workload, scale, err)
			}
			if cell.Contract.BDGActive || !cell.Contract.DirectActive {
				t.Fatalf("%s/%s/direct activates the governed arm", workload, scale)
			}
		}
	}
}

// TestNormalizedRewriteChangesOnlyLayout is what makes the rewrite cell a real
// test of semantic recognition: if the rewrite altered a token, a match would
// prove nothing.
func TestNormalizedRewriteChangesOnlyLayout(t *testing.T) {
	cell, err := resolveBaselineExecutionCell(baselineOperation("S2", "SF1", "normalized_rewrite_replay"))
	if err != nil {
		t.Fatalf("resolve S2/SF1/normalized_rewrite_replay: %v", err)
	}
	if cell.RewriteSQL == cell.BDGSQL {
		t.Fatal("the normalized rewrite is byte-identical to the frozen query")
	}
	if strings.Join(strings.Fields(cell.RewriteSQL), " ") != strings.Join(strings.Fields(cell.BDGSQL), " ") {
		t.Fatalf("the normalized rewrite changed more than layout:\n%q\n%q", cell.RewriteSQL, cell.BDGSQL)
	}
}

// TestUnimplementedBaselineWorkloadsDoNotResolve keeps the resolver honest
// about its own coverage: S4 is frozen in the contract but binds a Product the
// live Catalog does not publish, so it has no execution path, and a substituted query would be worse than a refusal.
func TestUnimplementedBaselineWorkloadsDoNotResolve(t *testing.T) {
	for _, cell := range []struct{ workload, scale, mode string }{
		{"S4", "depth-4", "novel"},
	} {
		if _, err := resolveBaselineExecutionCell(baselineOperation(cell.workload, cell.scale, cell.mode)); err == nil {
			t.Fatalf("%s/%s/%s resolved without an execution path", cell.workload, cell.scale, cell.mode)
		}
	}
}

// TestBaselineSSixReusesTheArtifactBinding records why S6 was the cheapest
// remaining workload: it is the same Product, relations and templates the
// Artifact cells have already executed, differing only in that Baseline also
// measures the Direct arm.
func TestBaselineSSixReusesTheArtifactBinding(t *testing.T) {
	for _, scale := range []string{"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16"} {
		for _, mode := range []string{"direct", "novel"} {
			cell, err := resolveBaselineExecutionCell(baselineOperation("S6", scale, mode))
			if err != nil {
				t.Fatalf("resolve S6/%s/%s: %v", scale, mode, err)
			}
			if got := cell.Task.DataProducts; len(got) != 1 || got[0] != "final_v5_result_heavy" {
				t.Fatalf("S6/%s approved products %v", scale, got)
			}
			if values := cell.Task.Scopes["category"]; len(values) != 4 {
				t.Fatalf("S6/%s scopes = %v, want the complete frozen category domain", scale, cell.Task.Scopes)
			}
			if !strings.Contains(cell.DirectSQL, "reporting.final_v5_result_heavy") {
				t.Fatalf("S6/%s Direct arm does not read the reporting view", scale)
			}
			if cell.Contract.ExpectedColumns != 4 && cell.Contract.ExpectedColumns != 16 {
				t.Fatalf("S6/%s expects %d columns", scale, cell.Contract.ExpectedColumns)
			}
		}
	}
}

// TestBaselineSThreeSharesTheScaleProduct records the coupling that forced the
// exposure-scale route's row ceiling up: S3 and Scale's dependency-e2e arm bind
// the same Product, and a Product may hold at most one route.
func TestBaselineSThreeSharesTheScaleProduct(t *testing.T) {
	for _, scale := range []string{"1k-5k", "10k-50k", "45k-225k"} {
		cell, err := resolveBaselineExecutionCell(baselineOperation("S3", scale, "novel"))
		if err != nil {
			t.Fatalf("resolve S3/%s/novel: %v", scale, err)
		}
		if got := cell.Task.DataProducts; len(got) != 1 || got[0] != "final_v5_exposure_scale" {
			t.Fatalf("S3/%s approved products %v", scale, got)
		}
		if cell.Contract.RenderParameterName != "member_max" {
			t.Fatalf("S3/%s renders on %q, want member_max", scale, cell.Contract.RenderParameterName)
		}
		if !strings.Contains(cell.DirectSQL, "reporting.final_v5_exposure_scale") ||
			strings.Contains(cell.BDGSQL, "reporting.") {
			t.Fatalf("S3/%s arms do not split on the reporting schema", scale)
		}
	}
}

// TestBaselineSFiveRendersAQueryPlan covers the one Baseline workload whose
// governed arm is declarative. Its two thresholds must both reach the plan, and
// the plan must stay a document rather than being flattened into text.
func TestBaselineSFiveRendersAQueryPlan(t *testing.T) {
	cell, err := resolveBaselineExecutionCell(baselineOperation("S5", "SF10", "novel"))
	if err != nil {
		t.Fatalf("resolve S5/SF10/novel: %v", err)
	}
	if !cell.PlanEntrypoint {
		t.Fatal("S5's governed arm is not marked as the plan entrypoint")
	}
	if cell.RewriteSQL != "" {
		t.Fatal("S5 produced a normalized rewrite; a QueryPlan has no layout to vary")
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(cell.BDGSQL), &plan); err != nil {
		t.Fatalf("S5's governed arm is not a JSON document: %v", err)
	}
	if strings.Contains(cell.BDGSQL, "$parameter") {
		t.Fatal("an unsubstituted $parameter object survived into the rendered plan")
	}
	// Both frozen thresholds must appear: SF10 unions branches at 50,000 and
	// 25,000, and a single-parameter renderer would have dropped the second.
	if !strings.Contains(cell.BDGSQL, "50000") || !strings.Contains(cell.BDGSQL, "25000") {
		t.Fatalf("S5/SF10 plan does not carry both thresholds: %s", cell.BDGSQL)
	}
	if !strings.Contains(cell.DirectSQL, "50000") || !strings.Contains(cell.DirectSQL, "25000") {
		t.Fatalf("S5/SF10 Direct arm does not carry both thresholds: %s", cell.DirectSQL)
	}
}

// TestBaselineClientTimeoutCoversItsApprovedQueryTimeout pins the relationship
// the S5/SF10 failure exposed: the Adapter must not abandon a governed query
// before the authorization the Gateway is working under expires. A client
// budget shorter than the route's query_timeout measures the client's patience
// and leaves a half-executed query behind for the replays to trip over.
func TestBaselineClientTimeoutCoversItsApprovedQueryTimeout(t *testing.T) {
	frozen, err := catalog.Load("../../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("load Catalog: %v", err)
	}
	seen := map[string]bool{}
	for _, cell := range baselineImplementedPublicationCells {
		resolved, err := resolveBaselineExecutionCell(baselineOperation(cell.WorkloadID, cell.Scale, cell.Mode))
		if err != nil {
			t.Fatalf("resolve %+v: %v", cell, err)
		}
		key := strings.Join(resolved.Task.DataProducts, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		policy, err := frozen.ResolveTaskPolicy(resolved.Task.DataProducts)
		if err != nil {
			t.Fatalf("resolve policy for %v: %v", resolved.Task.DataProducts, err)
		}
		if baselineClientTimeout < policy.Budget.PerQueryTimeout {
			t.Fatalf("client timeout %s is shorter than the %s query timeout granted to %v",
				baselineClientTimeout, policy.Budget.PerQueryTimeout, resolved.Task.DataProducts)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no Baseline closure was checked")
	}
}

// TestBaselineSSixCarriesTheContractResultSchema pins the reduction the paired
// comparison depends on. S6 releases dates, timestamps and booleans, and raw
// pgx values and raw Parquet values are different Go representations of those,
// so a cell that compared them directly reported a mismatch on every S6 cell
// while both arms had read identical rows.
func TestBaselineSSixCarriesTheContractResultSchema(t *testing.T) {
	for _, scale := range []string{"100x4", "100k-x16"} {
		cell, err := resolveBaselineExecutionCell(baselineOperation("S6", scale, "novel"))
		if err != nil {
			t.Fatalf("resolve S6/%s/novel: %v", scale, err)
		}
		if len(cell.ResultSchema) != cell.Contract.ExpectedColumns {
			t.Fatalf("S6/%s carries %d schema columns for a %d-column result",
				scale, len(cell.ResultSchema), cell.Contract.ExpectedColumns)
		}
	}
	// The workloads whose columns are simple keep the structural hash, and must
	// not silently acquire an Artifact schema that does not describe them.
	for _, workload := range []string{"S1", "S2", "S3"} {
		cell, err := resolveBaselineExecutionCell(baselineOperation(workload, workloadFirstScale(t, workload), "novel"))
		if err != nil {
			t.Fatalf("resolve %s novel: %v", workload, err)
		}
		if len(cell.ResultSchema) != 0 {
			t.Fatalf("%s acquired a %d-column contract schema it does not declare",
				workload, len(cell.ResultSchema))
		}
	}
}

func workloadFirstScale(t *testing.T, workload string) string {
	t.Helper()
	for _, cell := range baselineImplementedPublicationCells {
		if cell.WorkloadID == workload {
			return cell.Scale
		}
	}
	t.Fatalf("workload %s has no implemented cell", workload)
	return ""
}
