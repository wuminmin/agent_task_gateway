package sqllowering

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func TestLowerPreservesNullInCallerPredicates(t *testing.T) {
	result, err := Lower(`SELECT o.order_id FROM orders o WHERE o.order_id IN (1, NULL, 1)`, loweringTestProducts())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Filters) != 1 || result.Plan.Filters[0].Op != "IN" {
		t.Fatalf("lowered filter = %#v", result.Plan.Filters)
	}
	values, ok := result.Plan.Filters[0].Value.([]any)
	if !ok || len(values) != 3 || values[1] != nil {
		t.Fatalf("NULL literal was not preserved: %#v", result.Plan.Filters[0].Value)
	}
}

func TestLowerTypedNumericLiteralSpellingsShareV4NormalForm(t *testing.T) {
	products := loweringTestProducts()
	base, err := Lower(`SELECT l.order_id, l.extended_price FROM lineitem AS l WHERE l.extended_price = 320 ORDER BY l.order_id ASC`, products)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := Lower(`SELECT l.order_id, l.extended_price FROM lineitem AS l WHERE l.extended_price = 320.00 ORDER BY l.order_id ASC`, products)
	if err != nil {
		t.Fatal(err)
	}
	baseNormal, err := queryplan.NormalizeV4(base.Plan, products["lineitem"])
	if err != nil {
		t.Fatal(err)
	}
	candidateNormal, err := queryplan.NormalizeV4(candidate.Plan, products["lineitem"])
	if err != nil {
		t.Fatal(err)
	}
	baseDigest, err := baseNormal.Digest()
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest, err := candidateNormal.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baseNormal, candidateNormal) || baseDigest != candidateDigest {
		t.Fatalf("typed-equivalent numeric SQL diverged: base=%#v/%s candidate=%#v/%s",
			baseNormal, baseDigest, candidateNormal, candidateDigest)
	}
}

func TestLowerTenTableJoinWithMultiplePredicatesPerEdge(t *testing.T) {
	products := graphTestProducts(10)
	var sql strings.Builder
	sql.WriteString("SELECT a00.value FROM product_00 AS a00")
	for index := 1; index < 10; index++ {
		previousAlias := fmt.Sprintf("a%02d", index-1)
		alias := fmt.Sprintf("a%02d", index)
		product := fmt.Sprintf("product_%02d", index)
		fmt.Fprintf(&sql, " INNER JOIN %s AS %s ON %s.id = %s.id AND %s.tenant_id = %s.tenant_id", product, alias, previousAlias, alias, previousAlias, alias)
	}
	result, err := Lower(sql.String(), products)
	if err != nil {
		t.Fatal(err)
	}
	join := result.Plan.From.JoinMany
	if join == nil || len(join.Sources) != 10 || len(join.On) != 18 {
		t.Fatalf("ten-source join_many = %#v", join)
	}
	for index, source := range join.Sources {
		if source.Role != fmt.Sprintf("relation_%02d", index) {
			t.Fatalf("source %d role = %q", index, source.Role)
		}
	}
}

func TestLowerRejectsJoinBeyondOperationalSourceGuard(t *testing.T) {
	chainSQL := func(count int) string {
		var sql strings.Builder
		sql.WriteString("SELECT a00.value FROM product_00 AS a00")
		for index := 1; index < count; index++ {
			previousAlias := fmt.Sprintf("a%02d", index-1)
			alias := fmt.Sprintf("a%02d", index)
			product := fmt.Sprintf("product_%02d", index)
			fmt.Fprintf(&sql, " INNER JOIN %s AS %s ON %s.id = %s.id", product, alias, previousAlias, alias)
		}
		return sql.String()
	}
	if _, err := Lower(chainSQL(queryplan.MaxJoinSources), graphTestProducts(queryplan.MaxJoinSources)); err != nil {
		t.Fatalf("join at operational source guard: %v", err)
	}
	count := queryplan.MaxJoinSources + 1
	_, err := Lower(chainSQL(count), graphTestProducts(count))
	if err == nil {
		t.Fatal("join beyond operational source guard was accepted")
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeNotLowerable || typed.Reason != "JOIN_SOURCE_LIMIT_EXCEEDED" {
		t.Fatalf("source guard error = %#v", err)
	}
}

func TestLowerCanonicalGraphIgnoresEqualityPlacementWithinInnerJoinTree(t *testing.T) {
	products := graphTestProducts(3)
	first, err := Lower(`
		SELECT a.value
		FROM product_00 a
		JOIN product_01 b ON a.id = b.id AND a.tenant_id = b.tenant_id
		JOIN product_02 c ON b.id = c.id`, products)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Lower(`
		SELECT left_node.value
		FROM product_00 left_node
		JOIN product_01 middle_node ON left_node.id = middle_node.id
		JOIN product_02 right_node ON middle_node.id = right_node.id
			AND middle_node.tenant_id = left_node.tenant_id`, products)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Plan, second.Plan) {
		t.Fatalf("equivalent equality placement changed canonical plan:\nfirst=%#v\nsecond=%#v", first.Plan, second.Plan)
	}
}

func TestLowerAcceptsStarAndCyclicConnectedEquiJoinGraphs(t *testing.T) {
	result, err := Lower(`
		SELECT a.value
		FROM product_00 a
		JOIN product_01 b ON a.id = b.id
		JOIN product_02 c ON b.id = c.id AND c.tenant_id = a.tenant_id`, graphTestProducts(3))
	if err != nil {
		t.Fatal(err)
	}
	join := result.Plan.From.JoinMany
	if join == nil || len(join.Sources) != 3 || len(join.On) != 3 {
		t.Fatalf("cyclic join graph = %#v", join)
	}
	star, err := Lower(`
		SELECT hub.value
		FROM product_00 hub
		JOIN product_01 spoke_1 ON hub.id = spoke_1.id
		JOIN product_02 spoke_2 ON hub.id = spoke_2.id
		JOIN product_03 spoke_3 ON hub.id = spoke_3.id
		JOIN product_04 spoke_4 ON hub.id = spoke_4.id`, graphTestProducts(5))
	if err != nil {
		t.Fatal(err)
	}
	starJoin := star.Plan.From.JoinMany
	if starJoin == nil || len(starJoin.Sources) != 5 || len(starJoin.On) != 4 {
		t.Fatalf("star join graph = %#v", starJoin)
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

func TestLowerFiveTableJoinTreeShapesCanonicalizeToSamePlan(t *testing.T) {
	products := graphTestProducts(5)
	leftDeep, err := Lower(`
		SELECT a.value
		FROM product_00 AS a
		JOIN product_01 AS b ON a.id = b.id
		JOIN product_02 AS c ON b.id = c.id
		JOIN product_03 AS d ON c.id = d.id
		JOIN product_04 AS e ON d.id = e.id`, products)
	if err != nil {
		t.Fatal(err)
	}
	rightDeep, err := Lower(`
		SELECT root_node.value
		FROM product_00 AS root_node
		JOIN (
			product_01 AS first_node
			JOIN (
				product_02 AS second_node
				JOIN (
					product_03 AS third_node
					JOIN product_04 AS fourth_node
						ON fourth_node.id = third_node.id
				) ON third_node.id = second_node.id
			) ON second_node.id = first_node.id
		) ON first_node.id = root_node.id`, products)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leftDeep.Plan, rightDeep.Plan) {
		t.Fatalf("left- and right-deep join trees lowered differently:\nleft=%#v\nright=%#v", leftDeep.Plan, rightDeep.Plan)
	}
	join := leftDeep.Plan.From.JoinMany
	if join == nil || len(join.Sources) != 5 || len(join.On) != 4 {
		t.Fatalf("five-table canonical join = %#v", join)
	}
}

func TestLowerCompositeJoinKeyCanonicalizesAliasesConjunctionAndEqualityDirection(t *testing.T) {
	products := graphTestProducts(2)
	first, err := Lower(`
		SELECT left_input.value
		FROM product_00 AS left_input
		JOIN product_01 AS right_input
			ON left_input.id = right_input.id
			AND left_input.tenant_id = right_input.tenant_id`, products)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Lower(`
		SELECT "Fact Side".value
		FROM product_01 AS "Dimension Side"
		JOIN product_00 AS "Fact Side"
			ON "Dimension Side".tenant_id = "Fact Side".tenant_id
			AND "Dimension Side".id = "Fact Side".id`, products)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Plan, second.Plan) {
		t.Fatalf("composite join key spelling changed canonical plan:\nfirst=%#v\nsecond=%#v", first.Plan, second.Plan)
	}
	want := []queryplan.JoinPredicate{
		{Left: "relation_00.id", Right: "relation_01.id"},
		{Left: "relation_00.tenant_id", Right: "relation_01.tenant_id"},
	}
	if got := first.Plan.From.JoinMany.On; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical composite predicates = %#v, want %#v", got, want)
	}
}

func TestLowerRejectsJoinsOutsideCanonicalEquiJoinGraph(t *testing.T) {
	products := loweringTestProducts()
	tests := []struct {
		name, sql, code, reason string
	}{
		{name: "right join", sql: `SELECT o.status FROM orders o RIGHT JOIN lineitem l ON o.order_id = l.order_id`, code: CodeJoinTypeUnsupported, reason: "RIGHT_JOIN_UNSUPPORTED"},
		{name: "full join", sql: `SELECT o.status FROM orders o FULL JOIN lineitem l ON o.order_id = l.order_id`, code: CodeJoinTypeUnsupported, reason: "FULL_JOIN_UNSUPPORTED"},
		{name: "natural join", sql: `SELECT o.status FROM orders o NATURAL JOIN lineitem l`, code: CodeJoinTypeUnsupported, reason: "NATURAL_JOIN_UNSUPPORTED"},
		{name: "using join", sql: `SELECT o.status FROM orders o JOIN lineitem l USING (order_id)`, code: CodeJoinTypeUnsupported, reason: "JOIN_USING_UNSUPPORTED"},
		{name: "non equality", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id < l.order_id`, code: CodeNotLowerable, reason: "JOIN_PREDICATE_UNSUPPORTED"},
		{name: "column literal", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = 1`, code: CodeNotLowerable, reason: "JOIN_PREDICATE_UNSUPPORTED"},
		{name: "same relation", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = o.order_id`, code: CodeNotLowerable, reason: "JOIN_PREDICATE_SCOPE_INVALID"},
		{name: "or predicate", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = l.order_id OR o.order_id = l.line_id`, code: CodeNotLowerable, reason: "BOOLEAN_OPERATOR_UNSUPPORTED"},
		{name: "duplicate equality", sql: `SELECT o.status FROM orders o JOIN lineitem l ON o.order_id = l.order_id AND l.order_id = o.order_id`, code: CodeNotLowerable},
		{name: "self join", sql: `SELECT o.status FROM orders o JOIN orders other ON o.order_id = other.order_id`, code: CodeNotLowerable, reason: "SELF_JOIN_UNSUPPORTED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Lower(test.sql, products)
			if err == nil {
				t.Fatal("join outside the canonical equijoin graph was accepted")
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("error type = %T (%v)", err, err)
			}
			if typed.Code != test.code || test.reason != "" && typed.Reason != test.reason {
				t.Fatalf("error = %q/%q, want %q/%q", typed.Code, typed.Reason, test.code, test.reason)
			}
		})
	}
}
