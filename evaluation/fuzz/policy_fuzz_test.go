package fuzz

import (
	"encoding/json"
	"errors"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

var fuzzGrant = sqlpolicy.Grant{Products: []sqlpolicy.ProductGrant{
	{
		LogicalName:     "tpch_orders",
		PhysicalSchema:  "reporting",
		PhysicalView:    "tpch_orders",
		ApprovedColumns: []string{"o_orderkey", "o_orderstatus", "o_totalprice", "o_orderdate", "eval_scope"},
		MandatoryScope: []sqlpolicy.ScopePredicate{
			{Column: "eval_scope", Operator: sqlpolicy.ScopeEqual, Values: []string{"all"}},
		},
	},
}}

func FuzzAuthorizeNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"SELECT o_orderkey FROM tpch_orders",
		"SELECT * FROM tpch_orders",
		"SELECT pg_sleep(1) FROM tpch_orders",
		"SELECT o_orderkey FROM tpch_orders; DROP TABLE orders",
		"WITH x AS (SELECT o_orderkey FROM tpch_orders) SELECT o_orderkey FROM x",
		"\x00\xffSELECT",
	} {
		f.Add(seed)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	f.Fuzz(func(t *testing.T, input string) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Authorize panicked for %q: %v", input, recovered)
			}
		}()
		_, _ = engine.Authorize(sqlpolicy.Request{SQL: input, Grant: fuzzGrant, RowLimit: 100})
	})
}

func FuzzFormattingMetamorphic(f *testing.F) {
	for _, seed := range []string{
		"SELECT o_orderkey FROM tpch_orders ORDER BY o_orderkey",
		"SELECT count(o_orderkey) FROM tpch_orders",
		"SELECT o_orderstatus, sum(o_totalprice) FROM tpch_orders GROUP BY o_orderstatus",
		"DELETE FROM tpch_orders",
	} {
		f.Add(seed)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	f.Fuzz(func(t *testing.T, input string) {
		original, originalErr := engine.Authorize(sqlpolicy.Request{SQL: input, Grant: fuzzGrant, RowLimit: 100})
		formatted, formattedErr := engine.Authorize(sqlpolicy.Request{SQL: "\n/* taskgate-fuzz */\n" + input + "\n", Grant: fuzzGrant, RowLimit: 100})
		if policyCode(originalErr) != policyCode(formattedErr) {
			t.Fatalf("formatting changed decision: original=%q formatted=%q", policyCode(originalErr), policyCode(formattedErr))
		}
		if originalErr == nil && (original.Fingerprint != formatted.Fingerprint || original.SQL != formatted.SQL) {
			t.Fatalf("formatting changed an allowed query's canonical decision")
		}
	})
}

func FuzzQueryPlanCompileNeverPanics(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"product":"expense_summary","columns":["month"],"limit":10}`),
		[]byte(`{"product":"expense_summary","columns":["month"],"aggregates":[{"function":"sum","column":"total_amount","alias":"amount"}],"group_by":["month"]}`),
		[]byte(`{"product":"expense_summary","columns":["month"],"filters":[{"column":"department","op":"=","value":"sales' OR true --"}]}`),
		[]byte(`{"product":"other","columns":["*"]}`),
		[]byte(`{"from":{"join":{"left":{"product":"expense_summary","role":"expense_summary"},"right":{"product":"other","role":"other"},"on":[{"left":"expense_summary.month","right":"other.month"}]}},"columns":["expense_summary.month"]}`),
	} {
		f.Add(seed)
	}
	product := queryplan.Product{
		Name: "expense_summary",
		Columns: map[string]struct{}{
			"month": {}, "department": {}, "total_amount": {},
		},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
	}
	f.Fuzz(func(t *testing.T, encoded []byte) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("QueryPlan compile panicked for %q: %v", encoded, recovered)
			}
		}()
		var plan queryplan.QueryPlan
		if err := json.Unmarshal(encoded, &plan); err != nil {
			return
		}
		if plan.From != nil {
			_, _ = queryplan.CompileRelational(plan, map[string]queryplan.Product{"expense_summary": product})
			return
		}
		_, _ = queryplan.Compile(plan, product)
	})
}

func policyCode(err error) string {
	if err == nil {
		return "ALLOW"
	}
	var policyErr *sqlpolicy.PolicyError
	if errors.As(err, &policyErr) {
		return string(policyErr.Code)
	}
	return "OTHER_ERROR"
}
