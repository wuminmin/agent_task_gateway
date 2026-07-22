package sqlpolicy

import (
	"strings"
	"testing"
)

func TestAuthorizeBuildsScopedQuery(t *testing.T) {
	engine := New(Config{})
	decision, err := engine.Authorize(Request{
		SQL: `WITH recent AS (
  SELECT department, amount FROM expense_detail WHERE expense_date >= '2026-01-01'
)
SELECT department, sum(amount) AS total
FROM recent
GROUP BY department
ORDER BY total DESC`,
		Grant:    testGrant(),
		RowLimit: 17,
	})
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}

	checks := []string{
		`WITH "expense_detail" AS (`,
		`SELECT "department", "expense_date", "amount", "employee_name"`,
		`FROM "reporting"."expense_detail"`,
		`WHERE "department" = E'Sales'' East'`,
		`) AS "__taskbound_result"`,
		`LIMIT 17`,
	}
	for _, want := range checks {
		if !strings.Contains(decision.SQL, want) {
			t.Errorf("executable SQL does not contain %q:\n%s", want, decision.SQL)
		}
	}
	if got, want := strings.Join(decision.ReferencedProducts, ","), "expense_detail"; got != want {
		t.Errorf("ReferencedProducts = %q, want %q", got, want)
	}
	if len(decision.Fingerprint) != 64 {
		t.Errorf("Fingerprint length = %d, want 64", len(decision.Fingerprint))
	}
	if strings.Contains(decision.CanonicalAgentSQL, "reporting") {
		t.Error("canonical agent SQL unexpectedly contains a physical schema")
	}
}

func TestAuthorizeAllowsConservativeSelectForms(t *testing.T) {
	tests := []string{
		`SELECT department, amount FROM expense_detail WHERE amount >= 10 ORDER BY amount LIMIT 5`,
		`SELECT count(*) AS n FROM expense_detail`,
		`SELECT CAST(amount AS numeric) AS amount_num FROM expense_detail`,
		`SELECT department FROM expense_detail UNION ALL SELECT department FROM expense_detail`,
		`SELECT d.department FROM (SELECT department FROM expense_detail) AS d`,
	}
	engine := New(Config{})
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			if _, err := engine.Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 10}); err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
		})
	}
}

func TestAuthorizeRejectsAttackCorpus(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		code Code
	}{
		{name: "multi statement", sql: `SELECT department FROM expense_detail; DELETE FROM expense_detail`, code: CodeMultipleStatements},
		{name: "comment does not hide second statement", sql: `SELECT department FROM expense_detail; /* harmless */ UPDATE expense_detail SET amount = 1`, code: CodeMultipleStatements},
		{name: "top level write", sql: `DELETE FROM expense_detail`, code: CodeWriteForbidden},
		{name: "write CTE", sql: `WITH changed AS (DELETE FROM expense_detail RETURNING department) SELECT department FROM changed`, code: CodeWriteForbidden},
		{name: "select into", sql: `SELECT department INTO leaked FROM expense_detail`, code: CodeSelectInto},
		{name: "row lock", sql: `SELECT department FROM expense_detail FOR UPDATE`, code: CodeLocking},
		{name: "recursive", sql: `WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r) SELECT n FROM r`, code: CodeRecursiveCTE},
		{name: "catalog", sql: `SELECT relname FROM pg_catalog.pg_class`, code: CodeSystemObject},
		{name: "information schema", sql: `SELECT table_name FROM information_schema.tables`, code: CodeSystemObject},
		{name: "physical view", sql: `SELECT department FROM reporting.expense_detail`, code: CodeSystemObject},
		{name: "unpublished object", sql: `SELECT department FROM secret_table`, code: CodeObjectNotAllowed},
		{name: "select star", sql: `SELECT * FROM expense_detail`, code: CodeWildcard},
		{name: "qualified star", sql: `SELECT expense_detail.* FROM expense_detail`, code: CodeWildcard},
		{name: "dangerous function", sql: `SELECT pg_sleep(1)`, code: CodeFunctionNotAllowed},
		{name: "unknown function", sql: `SELECT md5(employee_name) FROM expense_detail`, code: CodeFunctionNotAllowed},
		{name: "session variable", sql: `SELECT current_setting('application_name')`, code: CodeFunctionNotAllowed},
		{name: "SQL value function", sql: `SELECT current_user`, code: CodeParameter},
		{name: "parameter", sql: `SELECT department FROM expense_detail WHERE amount > $1`, code: CodeParameter},
		{name: "column", sql: `SELECT salary FROM expense_detail`, code: CodeColumnNotAllowed},
		{name: "operator", sql: `SELECT amount ^ 2 FROM expense_detail`, code: CodeOperatorNotAllowed},
		{name: "values", sql: `VALUES (1)`, code: CodeFeatureNotAllowed},
		{name: "set returning function", sql: `SELECT department FROM generate_series(1, 2) AS department`, code: CodeFeatureNotAllowed},
	}

	engine := New(Config{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := engine.Authorize(Request{SQL: test.sql, Grant: testGrant(), RowLimit: 10})
			if !IsCode(err, test.code) {
				t.Fatalf("Authorize() error = %v, want code %s", err, test.code)
			}
			if err != nil && strings.Contains(err.Error(), "reporting") {
				t.Fatalf("error leaks a physical name: %v", err)
			}
		})
	}
}

func TestDangerousFunctionCannotBeEnabled(t *testing.T) {
	engine := New(Config{AllowedFunctions: []string{"pg_sleep", "sum"}})
	_, err := engine.Authorize(Request{SQL: `SELECT pg_sleep(1)`, Grant: testGrant(), RowLimit: 1})
	if !IsCode(err, CodeFunctionNotAllowed) {
		t.Fatalf("Authorize() error = %v, want %s", err, CodeFunctionNotAllowed)
	}
}

func TestIndirectSQLAndMetadataExportFunctionsCannotBeEnabled(t *testing.T) {
	tests := map[string]string{
		"query to XML":                `SELECT query_to_xml('SELECT employee_name FROM reporting.expense_detail', true, false, '')`,
		"query XML schema":            `SELECT query_to_xmlschema('SELECT employee_name FROM reporting.expense_detail', true, false, '')`,
		"query XML and schema":        `SELECT query_to_xml_and_xmlschema('SELECT employee_name FROM reporting.expense_detail', true, false, '')`,
		"table to XML":                `SELECT table_to_xml('reporting.expense_detail', true, false, '')`,
		"table XML schema":            `SELECT table_to_xmlschema('reporting.expense_detail', true, false, '')`,
		"table XML and schema":        `SELECT table_to_xml_and_xmlschema('reporting.expense_detail', true, false, '')`,
		"schema to XML":               `SELECT schema_to_xml('reporting', true, false, '')`,
		"schema XML schema":           `SELECT schema_to_xmlschema('reporting', true, false, '')`,
		"schema XML and schema":       `SELECT schema_to_xml_and_xmlschema('reporting', true, false, '')`,
		"database to XML":             `SELECT database_to_xml(true, false, '')`,
		"database XML schema":         `SELECT database_to_xmlschema(true, false, '')`,
		"database XML and schema":     `SELECT database_to_xml_and_xmlschema(true, false, '')`,
		"cursor to XML":               `SELECT cursor_to_xml('portal', 10, true, false, '')`,
		"cursor XML schema":           `SELECT cursor_to_xmlschema('portal', true, false, '')`,
		"text search SQL passthrough": `SELECT ts_stat('SELECT employee_name::tsvector FROM reporting.expense_detail')`,
	}
	for name, sql := range tests {
		t.Run(name, func(t *testing.T) {
			functionName, _, _ := strings.Cut(strings.TrimPrefix(sql, "SELECT "), "(")
			engine := New(Config{AllowedFunctions: []string{functionName}})
			_, err := engine.Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 1})
			if !IsCode(err, CodeFunctionNotAllowed) {
				t.Fatalf("Authorize() error = %v, want %s", err, CodeFunctionNotAllowed)
			}
		})
	}
}

func TestExplicitEmptyAllowlistsDenyFunctionsAndOperators(t *testing.T) {
	engine := New(Config{AllowedFunctions: []string{}, AllowedOperators: []string{}})
	if _, err := engine.Authorize(Request{SQL: `SELECT sum(amount) FROM expense_detail`, Grant: testGrant(), RowLimit: 1}); !IsCode(err, CodeFunctionNotAllowed) {
		t.Fatalf("empty function allowlist error = %v, want %s", err, CodeFunctionNotAllowed)
	}
	if _, err := engine.Authorize(Request{SQL: `SELECT amount + 1 FROM expense_detail`, Grant: testGrant(), RowLimit: 1}); !IsCode(err, CodeOperatorNotAllowed) {
		t.Fatalf("empty operator allowlist error = %v, want %s", err, CodeOperatorNotAllowed)
	}
}

func TestProductIndexedColumnFunctionAndOperatorPermissions(t *testing.T) {
	engine := New(Config{})
	grant := productIndexedGrant()
	tests := []struct {
		name string
		sql  string
		code Code
	}{
		{name: "alpha column accepted", sql: `SELECT a_only FROM alpha`, code: ""},
		{name: "beta cannot borrow alpha column", sql: `SELECT beta.a_only FROM beta`, code: CodeColumnNotAllowed},
		{name: "unique unqualified column accepted", sql: `SELECT a_only FROM alpha AS a JOIN beta AS b ON a.join_key = b.join_key`, code: ""},
		{name: "ambiguous unqualified column rejected", sql: `SELECT shared FROM alpha AS a JOIN beta AS b ON a.join_key = b.join_key`, code: CodeColumnNotAllowed},
		{name: "qualified column accepted", sql: `SELECT b.b_only FROM beta AS b`, code: ""},
		{name: "alias hides original relation name", sql: `SELECT beta.b_only FROM beta AS b`, code: CodeObjectNotAllowed},
		{name: "alpha scalar function accepted", sql: `SELECT lower(a_only) FROM alpha`, code: ""},
		{name: "beta cannot borrow alpha function", sql: `SELECT lower(b_only) FROM beta`, code: CodeFunctionNotAllowed},
		{name: "alpha operator accepted", sql: `SELECT metric + 1 FROM alpha`, code: ""},
		{name: "beta cannot borrow alpha operator", sql: `SELECT metric + 1 FROM beta`, code: CodeOperatorNotAllowed},
		{name: "join equality uses operator intersection", sql: `SELECT a.a_only, b.b_only FROM alpha AS a JOIN beta AS b ON a.join_key = b.join_key`, code: ""},
		{name: "cross product arithmetic requires operator intersection", sql: `SELECT a.metric + b.metric FROM alpha AS a JOIN beta AS b ON a.join_key = b.join_key`, code: CodeOperatorNotAllowed},
		{name: "multi product function requires function intersection", sql: `SELECT concat(a.a_only, b.b_only) FROM alpha AS a JOIN beta AS b ON a.join_key = b.join_key`, code: CodeFunctionNotAllowed},
		{name: "constant function uses task global safe list", sql: `SELECT lower('ABC') AS c`, code: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := engine.Authorize(Request{SQL: test.sql, Grant: grant, RowLimit: 10})
			if test.code == "" {
				if err != nil {
					t.Fatalf("Authorize() error = %v", err)
				}
				return
			}
			if !IsCode(err, test.code) {
				t.Fatalf("Authorize() error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestLexicalScopesShadowingAndDerivedColumnProvenance(t *testing.T) {
	engine := New(Config{})
	grant := productIndexedGrant()
	tests := []struct {
		name string
		sql  string
		code Code
	}{
		{
			name: "output alias is visible to order by",
			sql:  `SELECT a_only AS sorted_name FROM alpha ORDER BY sorted_name`,
			code: "",
		},
		{
			name: "output alias is not visible to where",
			sql:  `SELECT a_only AS sorted_name FROM alpha WHERE sorted_name <> ''`,
			code: CodeColumnNotAllowed,
		},
		{
			name: "CTE shadows product without inheriting product columns",
			sql:  `WITH alpha AS (SELECT b_only FROM beta) SELECT b_only FROM alpha`,
			code: "",
		},
		{
			name: "CTE shadow does not inherit shadowed product permissions",
			sql:  `WITH alpha AS (SELECT b_only FROM beta) SELECT a_only FROM alpha`,
			code: CodeColumnNotAllowed,
		},
		{
			name: "derived column keeps source product for allowed operator",
			sql:  `SELECT d.x + 1 FROM (SELECT metric AS x FROM alpha) AS d`,
			code: "",
		},
		{
			name: "derived column keeps source product for denied operator",
			sql:  `SELECT d.x - 1 FROM (SELECT metric AS x FROM alpha) AS d`,
			code: CodeOperatorNotAllowed,
		},
		{
			name: "alias column names bind derived relation",
			sql:  `SELECT d.renamed + 1 FROM (SELECT metric FROM alpha) AS d(renamed)`,
			code: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := engine.Authorize(Request{SQL: test.sql, Grant: grant, RowLimit: 10})
			if test.code == "" {
				if err != nil {
					t.Fatalf("Authorize() error = %v", err)
				}
				return
			}
			if !IsCode(err, test.code) {
				t.Fatalf("Authorize() error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestStructuralNodesAndCorrelatedSubqueryUseSourceProducts(t *testing.T) {
	engine := New(Config{})
	grant := productIndexedGrant()
	tests := []string{
		`SELECT CASE WHEN a.a_only IS NULL THEN 'missing' ELSE a.a_only END
FROM alpha AS a
WHERE a.metric > 0 AND (a.a_only IS NULL OR a.a_only <> '')`,
		`SELECT a.a_only
FROM alpha AS a
WHERE EXISTS (
  SELECT 1 FROM beta AS b
  WHERE b.join_key = a.join_key AND b.b_only IS NOT NULL
)`,
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			if _, err := engine.Authorize(Request{SQL: sql, Grant: grant, RowLimit: 10}); err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
		})
	}
}

func TestAuthorizeRejectsInvalidGrantAndExhaustedBudget(t *testing.T) {
	engine := New(Config{})
	if _, err := engine.Authorize(Request{SQL: `SELECT department FROM expense_detail`, Grant: testGrant(), RowLimit: 0}); !IsCode(err, CodeBudgetExhausted) {
		t.Fatalf("zero row budget error = %v", err)
	}

	grant := testGrant()
	grant.Products[0].MandatoryScope[0].Operator = ScopeOperator("raw SQL")
	if _, err := engine.Authorize(Request{SQL: `SELECT department FROM expense_detail`, Grant: grant, RowLimit: 1}); !IsCode(err, CodeInvalidGrant) {
		t.Fatalf("invalid grant error = %v", err)
	}
}

func TestScopeLiteralCannotInjectSQL(t *testing.T) {
	grant := testGrant()
	grant.Products[0].MandatoryScope[0].Values = []string{"Sales\\'; DROP TABLE x; --"}
	decision, err := New(Config{}).Authorize(Request{
		SQL: `SELECT department FROM expense_detail`, Grant: grant, RowLimit: 1,
	})
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !strings.Contains(decision.SQL, `E'Sales\\''; DROP TABLE x; --'`) {
		t.Fatalf("scope value was not escaped as one literal:\n%s", decision.SQL)
	}
}

func testGrant() Grant {
	return Grant{Products: []ProductGrant{{
		LogicalName:     "expense_detail",
		PhysicalSchema:  "reporting",
		PhysicalView:    "expense_detail",
		ApprovedColumns: []string{"department", "expense_date", "amount", "employee_name"},
		MandatoryScope: []ScopePredicate{{
			Column: "department", Operator: ScopeEqual, Values: []string{"Sales' East"},
		}},
	}}}
}

func productIndexedGrant() Grant {
	return Grant{Products: []ProductGrant{
		{
			LogicalName:       "alpha",
			PhysicalSchema:    "reporting",
			PhysicalView:      "alpha",
			ApprovedColumns:   []string{"id", "join_key", "shared", "a_only", "metric"},
			AllowedFunctions:  []string{"lower"},
			AllowedAggregates: []string{"count", "sum"},
			AllowedOperators:  []string{"=", "<>", ">", "+", "is", "is not"},
		},
		{
			LogicalName:       "beta",
			PhysicalSchema:    "reporting",
			PhysicalView:      "beta",
			ApprovedColumns:   []string{"id", "join_key", "shared", "b_only", "metric"},
			AllowedFunctions:  []string{"upper"},
			AllowedAggregates: []string{"count"},
			AllowedOperators:  []string{"=", "<>", "is", "is not"},
		},
	}}
}
