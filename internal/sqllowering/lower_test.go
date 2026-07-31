package sqllowering

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestLowerThreeTableJoinErasesAliasesAndJoinOrder(t *testing.T) {
	products := loweringTestProducts()
	first, err := Lower(`
		SELECT o.status, SUM(l.extended_price) AS revenue
		FROM orders AS o
		INNER JOIN lineitem AS l ON o.order_id = l.order_id
		INNER JOIN supplier AS s ON l.supplier_id = s.supplier_id
		WHERE o.region = 'PH'
		GROUP BY o.status`, products)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Lower(`
		SELECT ord.status, SUM(item.extended_price) AS revenue
		FROM supplier AS vendor
		JOIN lineitem AS item ON vendor.supplier_id = item.supplier_id
		JOIN orders AS ord ON item.order_id = ord.order_id
		WHERE ord.region = 'PH'
		GROUP BY ord.status`, products)
	if err != nil {
		t.Fatal(err)
	}
	if first.Profile != Profile || second.Profile != Profile {
		t.Fatalf("profiles = %q, %q", first.Profile, second.Profile)
	}
	if !reflect.DeepEqual(first.Plan, second.Plan) {
		t.Fatalf("equivalent SQL lowered differently:\nfirst=%#v\nsecond=%#v", first.Plan, second.Plan)
	}
	join := first.Plan.From.JoinMany
	if join == nil || len(join.Sources) != 3 || len(join.On) != 2 {
		t.Fatalf("join_many = %#v", join)
	}
	if got := []string{join.Sources[0].Role, join.Sources[1].Role, join.Sources[2].Role}; !reflect.DeepEqual(got, []string{"lineitem", "orders", "supplier"}) {
		t.Fatalf("canonical roles = %v", got)
	}
	if !reflect.DeepEqual(first.DisplayColumns, []string{"status", "revenue"}) ||
		!reflect.DeepEqual(second.DisplayColumns, []string{"status", "revenue"}) {
		t.Fatalf("display columns = %v, %v", first.DisplayColumns, second.DisplayColumns)
	}
}

func TestLowerPreservesDirectProjectionAliasesOutsideCanonicalPlan(t *testing.T) {
	result, err := Lower(`SELECT o.status AS order_state FROM orders o`, loweringTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Plan.Columns, []string{"status"}) || !reflect.DeepEqual(result.DisplayColumns, []string{"order_state"}) {
		t.Fatalf("lowered projection = plan %v display %v", result.Plan.Columns, result.DisplayColumns)
	}
}

func TestLowerRecordsPublicOrderForInterleavedAggregateProjection(t *testing.T) {
	result, err := Lower(`
		SELECT SUM(l.extended_price) AS revenue, o.status
		FROM orders o JOIN lineitem l ON o.order_id = l.order_id
		GROUP BY o.status`, loweringTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.DisplayColumns, []string{"revenue", "status"}) ||
		!reflect.DeepEqual(result.ResultOrder, []int{1, 0}) {
		t.Fatalf("public projection = columns %v order %v", result.DisplayColumns, result.ResultOrder)
	}
}

func TestLowerSingleProductPreservesSupportedPagination(t *testing.T) {
	result, err := Lower(`
		SELECT o.status
		FROM orders o
		WHERE o.region = 'PH' AND o.order_id >= 10
		ORDER BY o.status DESC
		LIMIT 5 OFFSET 2`, loweringTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	plan := result.Plan
	if plan.Product != "orders" || plan.From != nil {
		t.Fatalf("single-product input = %#v", plan)
	}
	if plan.Limit != 5 || plan.Offset != 2 || len(plan.OrderBy) != 1 || plan.OrderBy[0] != (queryplan.Order{Column: "status", Direction: "DESC"}) {
		t.Fatalf("pagination = limit %d offset %d order %#v", plan.Limit, plan.Offset, plan.OrderBy)
	}
	if len(plan.Filters) != 2 || plan.Filters[0].Column != "order_id" || plan.Filters[1].Column != "region" {
		t.Fatalf("canonical filters = %#v", plan.Filters)
	}
}

func TestLowerComposesNestedUnaryNumericSignsWithoutChangingMeaning(t *testing.T) {
	result, err := Lower(`SELECT o.status FROM orders o WHERE o.order_id = - - 1`, loweringTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Filters) != 1 || result.Plan.Filters[0].Value != json.Number("1") {
		t.Fatalf("nested unary literal = %#v", result.Plan.Filters)
	}
}

func TestLowerReturnsStableRepairableErrors(t *testing.T) {
	products := loweringTestProducts()
	tests := []struct {
		name   string
		sql    string
		code   string
		reason string
	}{
		{name: "syntax", sql: `SELEC status FROM orders`, code: CodeSyntaxError, reason: CodeSyntaxError},
		{name: "product", sql: `SELECT m.status FROM missing m`, code: CodeProductNotApproved, reason: CodeProductNotApproved},
		{name: "column", sql: `SELECT o.secret FROM orders o`, code: CodeColumnNotApproved, reason: CodeColumnNotApproved},
		{name: "left join", sql: `SELECT o.status FROM orders o LEFT JOIN lineitem l ON o.order_id = l.order_id`, code: CodeJoinTypeUnsupported, reason: "LEFT_JOIN_UNSUPPORTED"},
		{name: "disconnected", sql: `SELECT o.status FROM orders o CROSS JOIN lineitem l`, code: CodeJoinGraphDisconnected, reason: "JOIN_PREDICATE_REQUIRED"},
		{name: "subquery", sql: `SELECT q.status FROM (SELECT status FROM orders) q`, code: CodeSubqueryUnsupported, reason: CodeSubqueryUnsupported},
		{name: "join type", sql: `SELECT o.status FROM orders o JOIN supplier s ON o.order_id = s.name`, code: CodeJoinKeyTypeMismatch, reason: CodeJoinKeyTypeMismatch},
		{name: "collation", sql: `SELECT o.status FROM orders o JOIN supplier s ON o.status = s.name`, code: CodeCollationMismatch, reason: CodeCollationMismatch},
		{name: "future join alias", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = s.supplier_id JOIN supplier s ON l.supplier_id = s.supplier_id`, code: CodeNotLowerable, reason: "JOIN_PREDICATE_SCOPE_INVALID"},
		{name: "literal type", sql: `SELECT o.status FROM orders o WHERE o.order_id = 'bad'`, code: CodeNotLowerable, reason: "FILTER_LITERAL_TYPE_MISMATCH"},
		{name: "inexact aggregate", sql: `SELECT SUM(l.ratio) AS total FROM lineitem l`, code: CodeNotLowerable, reason: "AGGREGATE_TYPE_UNSUPPORTED"},
		{name: "or", sql: `SELECT o.status FROM orders o WHERE o.region = 'PH' OR o.region = 'US'`, code: CodeNotLowerable, reason: "BOOLEAN_OPERATOR_UNSUPPORTED"},
		{name: "multi pagination", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = l.order_id LIMIT 2`, code: CodeNotLowerable, reason: "PAGINATION_UNSUPPORTED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Lower(test.sql, products)
			if err == nil {
				t.Fatal("expected lowering error")
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("error type = %T (%v)", err, err)
			}
			if typed.Code != test.code || typed.Reason != test.reason {
				t.Fatalf("error = code %q reason %q, want %q/%q", typed.Code, typed.Reason, test.code, test.reason)
			}
			if !typed.Retryable || typed.Message == "" || typed.Alternative == "" || typed.Location.Clause == "" {
				t.Fatalf("error is not repairable: %#v", typed)
			}
		})
	}
}

func loweringTestProducts() map[string]queryplan.Product {
	return map[string]queryplan.Product{
		"orders": {
			Name: "orders", StableRole: "orders", SourceNamespace: "sales.orders", Snapshot: "snapshot-1",
			StableEntityKey: []string{"order_id"},
			Columns:         map[string]struct{}{"order_id": {}, "status": {}, "region": {}},
			ColumnTypes:     map[string]string{"order_id": "integer", "status": "text", "region": "text"},
			ColumnCollations: map[string]string{
				"status": "C", "region": "C",
			},
			CollationVersions: map[string]string{"status": "builtin", "region": "builtin"},
			AllowedAggregates: map[string]struct{}{"count": {}, "min": {}, "max": {}},
		},
		"lineitem": {
			Name: "lineitem", StableRole: "lineitem", SourceNamespace: "sales.lineitem", Snapshot: "snapshot-1",
			StableEntityKey:  []string{"line_id"},
			Columns:          map[string]struct{}{"line_id": {}, "order_id": {}, "supplier_id": {}, "extended_price": {}, "ratio": {}},
			ColumnTypes:      map[string]string{"line_id": "integer", "order_id": "integer", "supplier_id": "integer", "extended_price": "numeric", "ratio": "double precision"},
			ColumnCollations: map[string]string{}, CollationVersions: map[string]string{},
			AllowedAggregates: map[string]struct{}{"count": {}, "sum": {}, "min": {}, "max": {}},
		},
		"supplier": {
			Name: "supplier", StableRole: "supplier", SourceNamespace: "sales.supplier", Snapshot: "snapshot-1",
			StableEntityKey:  []string{"supplier_id"},
			Columns:          map[string]struct{}{"supplier_id": {}, "name": {}},
			ColumnTypes:      map[string]string{"supplier_id": "integer", "name": "text"},
			ColumnCollations: map[string]string{"name": "en_US"}, CollationVersions: map[string]string{"name": "v1"},
			AllowedAggregates: map[string]struct{}{"count": {}, "min": {}, "max": {}},
		},
	}
}
