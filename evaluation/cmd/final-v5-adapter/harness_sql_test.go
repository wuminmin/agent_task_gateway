package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type harnessSQLChecker struct {
	t        *testing.T
	catalog  *catalog.Catalog
	approved map[string][]string
	accepted int
	rejected int
}

// TestAllFinalV5HarnessQueriesMatchProductionSQLRules is deliberately a pure-Go
// pre-deployment gate. It calls the production Catalog projection, SQL
// lowering, QueryPlan compiler, and SQL policy; it does not connect to a
// database and does not call physicalquery.Prepare or reproduce its rules.
//
// Baseline, Scale, and Artifact contract templates remain under the independent
// 28-artifact/71-rendered-cell SQL executability gate. This test covers every
// other source-controlled TaskGate SQL generator, plus RQ5's structured-plan
// entrypoint, so a harness/profile mismatch fails before a live deployment.
func TestAllFinalV5HarnessQueriesMatchProductionSQLRules(t *testing.T) {
	master, err := catalog.Load("../../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checker := &harnessSQLChecker{t: t, catalog: master, approved: map[string][]string{
		concurrencyfixture.ProductName:   {"receipt_no", "expense_type", "city", "department"},
		"final_v5_attack_expense_detail": {"receipt_no", "amount"},
		"expense_detail":                 {"receipt_no", "amount", "department"},
		finalv5rls.UnlimitedProduct:      {"receipt_no", "amount"},
		finalv5rls.BoundedProduct:        {"receipt_no", "amount"},
		"provsql_orders":                 {"orderkey", "status", "partition_key"},
		"provsql_lineitem":               {"orderkey", "linenumber", "extendedprice", "partition_key"},
		"provsql_nonce":                  {"nonce_id", "partition_key"},
	}}

	concurrencySQL := append(append([]string(nil), concurrencyfixture.PrefixSQL...),
		concurrencyfixture.ContenderSQL, concurrencyfixture.OverflowSQL)
	for index, sqlText := range concurrencySQL {
		checker.checkSQL(fmt.Sprintf("concurrency/%d", index+1), sqlText,
			[]string{concurrencyfixture.ProductName}, "", "")
	}
	checker.checkSQL("baseline/pilot/taskgate", pilotTaskGateSQL, []string{"expense_detail"}, "", "")
	checker.checkSQL("baseline/pilot/normalized-rewrite", pilotRewriteSQL, []string{"expense_detail"}, "", "")

	rlsManifest, err := finalv5rls.Load()
	if err != nil {
		t.Fatal(err)
	}
	rlsSteps, err := rlsManifest.Trace()
	if err != nil {
		t.Fatal(err)
	}
	policyStep, err := rlsManifest.PolicyInvisibleStep()
	if err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{finalv5rls.UnlimitedProduct, finalv5rls.BoundedProduct} {
		for _, step := range rlsSteps {
			checker.checkSQL(fmt.Sprintf("rls/%s/%03d", product, step.Index),
				step.LogicalSQL(product), []string{product}, "", "")
		}
		checker.checkSQL("rls/"+product+"/policy-invisible", policyStep.LogicalSQL(product),
			[]string{product}, "", "")
		checker.checkSQL("rls/"+product+"/authorization-control",
			finalv5rls.PolicyAuthorizationControl().LogicalSQL(product), []string{product},
			sqllowering.CodeColumnNotApproved, "COLUMN_NOT_APPROVED")
	}

	attackManifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, attackCase := range attackManifest.Cases {
		product := expectedAttackCatalogBinding(attackCase.WorkloadID).Product
		for _, step := range attackCase.Steps {
			code, reason := "", ""
			if step.ExpectedErrorCode == sqllowering.CodeNotLowerable {
				code, reason = step.ExpectedErrorCode, step.ExpectedErrorReason
			}
			checker.checkSQL("attack/"+attackCase.WorkloadID+"/"+attackCase.Scale+"/"+step.ID,
				step.LogicalSQL, []string{product}, code, reason)
		}
	}

	provSQLProducts := []string{"provsql_orders", "provsql_lineitem", "provsql_nonce"}
	for _, scale := range []string{"1k", "10k", "45k"} {
		for iteration := 1; iteration <= 35; iteration++ {
			warmup := iteration <= 5
			ordinal := iteration
			if !warmup {
				ordinal -= 5
			}
			nonce, nonceErr := provsqlfixture.Nonce(scale, 1, ordinal, warmup)
			if nonceErr != nil {
				t.Fatal(nonceErr)
			}
			sqlText, sqlErr := provsqlfixture.LogicalSQL(scale, nonce)
			if sqlErr != nil {
				t.Fatal(sqlErr)
			}
			checker.checkSQL(fmt.Sprintf("provsql/%s/%d", scale, nonce), sqlText,
				provSQLProducts, "", "")
		}
	}

	if checker.accepted != 343 || checker.rejected != 5 {
		t.Fatalf("harness SQL decision count = accepted %d, expected rejection %d; want 343/5",
			checker.accepted, checker.rejected)
	}
	checkRQ5StructuredPlan(t)
	t.Logf("harness SQL static audit: accepted=%d expected_rejections=%d total=%d rq5_plans=1 baseline_scale_artifact_contract_cells=71 fixture_sha256=%s plans_sha256=%s",
		checker.accepted, checker.rejected, checker.accepted+checker.rejected,
		concurrencyfixture.FixtureSHA256(), concurrencyfixture.PlansSHA256())
}

func (checker *harnessSQLChecker) checkSQL(source, sqlText string, productNames []string,
	wantCode, wantReason string) {
	checker.t.Helper()
	products, grant := checker.productsAndGrant(productNames)
	lowered, err := sqllowering.Lower(sqlText, products)
	if wantCode != "" {
		var rejected *sqllowering.Error
		if !errors.As(err, &rejected) || rejected.Code != wantCode || rejected.Reason != wantReason {
			checker.t.Fatalf("%s lowering = %v; want %s/%s", source, err, wantCode, wantReason)
		}
		checker.rejected++
		return
	}
	if err != nil {
		checker.t.Fatalf("%s production lowering: %v", source, err)
	}
	visibleSQL := ""
	if lowered.Plan.From == nil {
		product, found := products[lowered.Plan.Product]
		if !found {
			checker.t.Fatalf("%s lowered to unapproved product %q", source, lowered.Plan.Product)
		}
		visibleSQL, err = queryplan.Compile(lowered.Plan, product)
	} else {
		var compiled queryplan.RelationalCompilation
		compiled, err = queryplan.CompileRelational(lowered.Plan, products)
		visibleSQL = compiled.VisibleSQL
	}
	if err != nil {
		checker.t.Fatalf("%s production QueryPlan compile: %v", source, err)
	}
	if _, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: visibleSQL, Grant: grant, RowLimit: 10_000,
	}); err != nil {
		checker.t.Fatalf("%s production SQL policy: %v", source, err)
	}
	checker.accepted++
}

func (checker *harnessSQLChecker) productsAndGrant(names []string) (map[string]queryplan.Product, sqlpolicy.Grant) {
	checker.t.Helper()
	products := make(map[string]queryplan.Product, len(names))
	grant := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(names))}
	for _, name := range names {
		product, found := checker.catalog.LookupProduct(name)
		if !found {
			checker.t.Fatalf("Catalog product %q is absent", name)
		}
		columns, found := checker.approved[name]
		if !found || len(columns) == 0 {
			checker.t.Fatalf("approved harness columns for %q are absent", name)
		}
		approved := make(map[string]struct{}, len(columns))
		for _, column := range columns {
			approved[column] = struct{}{}
		}
		products[name] = physicalquery.QueryProductFromCatalog(product, approved)
		parts := strings.Split(product.ReportingView, ".")
		if len(parts) != 2 {
			checker.t.Fatalf("Catalog product %q reporting view %q is not schema-qualified", name, product.ReportingView)
		}
		grant.Products = append(grant.Products, sqlpolicy.ProductGrant{
			LogicalName: name, PhysicalSchema: parts[0], PhysicalView: parts[1],
			ApprovedColumns: append([]string(nil), columns...), AllowedFunctions: append([]string(nil), product.AllowedFunctions...),
			AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
			AllowedOperators:  append([]string(nil), product.AllowedOperators...),
		})
	}
	return products, grant
}

func checkRQ5StructuredPlan(t *testing.T) {
	day := rq5fixture.Days[0]
	digest := strings.Repeat("a", 64)
	daily, _, err := experiment.BuildRQ5DailyCatalog(experiment.RQ5DailyCatalogInput{
		Day: day, PublicationName: fmt.Sprintf("daily-lineitem-%s-r%d", day, rq5fixture.RowsPerPublication),
		CatalogSource: "daily_reporting", SourceID: "taskgate-eval-daily-publication",
		SourceNamespace: "evaluation.daily_lineitem", SourceRelation: "reporting.daily_lineitem_" + day,
		Snapshot:                  fmt.Sprintf("rq5-daily-lineitem-%s-rows-%d", day, rq5fixture.RowsPerPublication),
		OrdinalSidecar:            fmt.Sprintf("taskgate_ordinal.daily_lineitem_%s_r%d", day, rq5fixture.RowsPerPublication),
		PublicationManifestSHA256: digest, DictionarySHA256: digest, SidecarSHA256: digest, SchemaSHA256: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	checker := &harnessSQLChecker{t: t, catalog: daily, approved: map[string][]string{
		"daily_lineitem": {"dataset_partition", "l_orderkey", "l_linenumber", "l_extendedprice"},
	}}
	products, grant := checker.productsAndGrant([]string{"daily_lineitem"})
	plan := queryplan.QueryPlan{Product: "daily_lineitem",
		Columns: []string{"l_orderkey", "l_linenumber", "l_extendedprice"},
		Filters: []queryplan.Filter{{Column: "l_orderkey", Op: "=", Value: json.Number("1")}},
		OrderBy: []queryplan.Order{{Column: "l_linenumber", Direction: "asc"}}, Limit: 10}
	visibleSQL, err := queryplan.Compile(plan, products["daily_lineitem"])
	if err != nil {
		t.Fatalf("RQ5 production QueryPlan compile: %v", err)
	}
	if _, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: visibleSQL, Grant: grant, RowLimit: 10,
	}); err != nil {
		t.Fatalf("RQ5 production SQL policy: %v", err)
	}
}
