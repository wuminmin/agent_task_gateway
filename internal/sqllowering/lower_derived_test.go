package sqllowering

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func derivedLowerProduct() queryplan.Product {
	return queryplan.Product{
		Name: "sales_product", SourceNamespace: "sales", Snapshot: "snap-1",
		Columns: map[string]struct{}{"price": {}, "qty": {}, "region": {}},
		ColumnTypes: map[string]string{"price": "numeric", "qty": "numeric", "region": "text"},
		ColumnCollations: map[string]string{"region": "en_US.utf8"},
		CollationVersions: map[string]string{"region": "2.36"},
		AllowedAggregates: map[string]struct{}{"sum": {}, "count": {}},
	}
}

func TestLowerDerivedProjectionAndSum(t *testing.T) {
	product := derivedLowerProduct()
	result, err := Lower("SELECT region, (price * qty) AS revenue FROM sales_product", map[string]queryplan.Product{product.Name: product})
	if err != nil {
		t.Fatalf("projection arithmetic must lower: %+v", err)
	}
	if len(result.Plan.Derived) != 1 || result.Plan.Derived[0].Alias != "revenue" ||
		result.Plan.Derived[0].SQLType != "numeric" {
		t.Fatalf("derived plan wrong: %+v", result.Plan.Derived)
	}
	if len(result.ResultOrder) != 2 || result.ResultOrder[0] != 0 || result.ResultOrder[1] != 1 {
		t.Fatalf("result order must place the derived column after the plain column: %v", result.ResultOrder)
	}
	sum, err := Lower("SELECT region, sum(price - qty) AS margin FROM sales_product GROUP BY region", map[string]queryplan.Product{product.Name: product})
	if err != nil {
		t.Fatalf("SUM over arithmetic must lower: %+v", err)
	}
	if len(sum.Plan.Aggregates) != 1 || sum.Plan.Aggregates[0].DerivedArg == nil {
		t.Fatalf("derived aggregate argument missing: %+v", sum.Plan.Aggregates)
	}
}

func TestLowerDerivedFailsClosed(t *testing.T) {
	product := derivedLowerProduct()
	cases := map[string]string{
		"no alias":        "SELECT (price * qty) FROM sales_product",
		"text operand":    "SELECT (region * qty) AS x FROM sales_product",
		"float literal":   "SELECT (price * 0.5) AS x FROM sales_product",
		"modulo":          "SELECT (price % qty) AS x FROM sales_product",
		"division":        "SELECT (price / qty) AS x FROM sales_product",
		"count derived":   "SELECT count(price - qty) AS x FROM sales_product",
		"unknown column":  "SELECT (price * secret) AS x FROM sales_product",
	}
	for label, sql := range cases {
		if _, err := Lower(sql, map[string]queryplan.Product{product.Name: product}); err == nil {
			t.Fatalf("%s must fail closed: %s", label, sql)
		}
	}
}

func TestPlainPlansKeepIdentity(t *testing.T) {
	product := derivedLowerProduct()
	result, err := Lower("SELECT region FROM sales_product WHERE region = 'east'", map[string]queryplan.Product{product.Name: product})
	if err != nil {
		t.Fatalf("plain plan must lower: %+v", err)
	}
	if len(result.Plan.Derived) != 0 {
		t.Fatal("plain plan must carry no derived columns")
	}
	form, normErr := queryplan.NormalizeV2(result.Plan, product)
	if normErr != nil {
		t.Fatal(normErr)
	}
	if form.Version != queryplan.NormalFormVersion {
		t.Fatalf("plain plan must keep the frozen identity, got %s", form.Version)
	}
	_ = strings.TrimSpace
}
