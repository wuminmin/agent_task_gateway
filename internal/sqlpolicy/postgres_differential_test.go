package sqlpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgreSQLDifferentialParseDeparseExecution sends libpg_query-authorized
// and rewritten statements to a real PostgreSQL 16 server, then compares their
// ordered results with independently written physical-view queries. The
// Compose acceptance suite supplies BUSINESS_TEST_POSTGRES_DSN; ordinary unit
// runs skip this database-dependent check.
func TestPostgreSQLDifferentialParseDeparseExecution(t *testing.T) {
	dsn := os.Getenv("BUSINESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for PostgreSQL differential tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open business PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping business PostgreSQL: %v", err)
	}

	grant := Grant{Products: []ProductGrant{{
		LogicalName:     "expense_detail",
		PhysicalSchema:  "reporting",
		PhysicalView:    "expense_detail",
		ApprovedColumns: []string{"receipt_no", "department", "expense_date", "amount"},
		MandatoryScope: []ScopePredicate{{
			Column: "department", Operator: ScopeEqual, Values: []string{"销售部"},
		}},
	}}}
	tests := []struct {
		name      string
		agentSQL  string
		directSQL string
	}{
		{
			name:      "ordered projection",
			agentSQL:  `SELECT receipt_no, amount FROM expense_detail ORDER BY receipt_no`,
			directSQL: `SELECT receipt_no, amount FROM reporting.expense_detail WHERE department = '销售部' ORDER BY receipt_no LIMIT 25`,
		},
		{
			name: "cte and date literal",
			agentSQL: `WITH recent AS (
  SELECT receipt_no, amount FROM expense_detail WHERE expense_date >= DATE '2026-02-01'
)
SELECT receipt_no, amount FROM recent ORDER BY receipt_no`,
			directSQL: `SELECT receipt_no, amount FROM reporting.expense_detail WHERE department = '销售部' AND expense_date >= DATE '2026-02-01' ORDER BY receipt_no LIMIT 25`,
		},
		{
			name:      "derived table alias",
			agentSQL:  `SELECT d.receipt_no, d.amount FROM (SELECT receipt_no, amount FROM expense_detail) AS d ORDER BY d.receipt_no`,
			directSQL: `SELECT receipt_no, amount FROM reporting.expense_detail WHERE department = '销售部' ORDER BY receipt_no LIMIT 25`,
		},
	}

	engine := New(Config{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := engine.Authorize(Request{SQL: test.agentSQL, Grant: grant, RowLimit: 25})
			if err != nil {
				t.Fatalf("authorize with libpg_query: %v", err)
			}
			rewritten := queryRows(t, ctx, db, decision.SQL)
			direct := queryRows(t, ctx, db, test.directSQL)
			if !reflect.DeepEqual(rewritten, direct) {
				t.Fatalf("rewritten and direct PostgreSQL results differ\nrewritten=%v\ndirect=%v\nSQL=%s", rewritten, direct, decision.SQL)
			}
		})
	}
}

func queryRows(t *testing.T, ctx context.Context, db *sql.DB, statement string) [][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		t.Fatalf("PostgreSQL rejected executable SQL: %v\n%s", err, statement)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("read result columns: %v", err)
	}
	result := make([][]string, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			t.Fatalf("scan PostgreSQL result: %v", err)
		}
		encoded := make([]string, len(values))
		for index, value := range values {
			switch typed := value.(type) {
			case []byte:
				encoded[index] = string(typed)
			default:
				encoded[index] = fmt.Sprint(typed)
			}
		}
		result = append(result, encoded)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate PostgreSQL result: %v", err)
	}
	return result
}
