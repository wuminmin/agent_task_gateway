// Package postgresoracle runs the RQ2 rewrite campaign against PostgreSQL.
//
// This package is deliberately independent of the TaskGate exposure,
// queryplan, and SQL-policy packages. Expected rows are computed directly from
// a private fixture by a small relational oracle, while PostgreSQL executes the
// baseline and rewritten SQL.
package postgresoracle

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	OracleID          = "independent-go-fixture-oracle-v2"
	ExpectedAttempts  = 1024
	ExpectedTemplates = 8
	PairNormalization = "collapse-sql-whitespace+ordered-statement-framing+sha256-v1"
)

var departments = []string{"sales", "rnd", "ops", "legal"}
var minimumAmounts = []int{0, 5, 10, 15, 20, 25, 30, 35}
var minimumDates = []string{"2026-01-01", "2026-01-15", "2026-02-01", "2026-03-01"}

type fixtureRow struct {
	EntityKey  string  `json:"entity_key"`
	Department *string `json:"department"`
	Amount     *int    `json:"amount"`
	Date       *string `json:"expense_date"`
}

type scenario struct {
	Department    string
	MinimumAmount int
	MinimumDate   string
}

type rewrite struct {
	Template string
	SQL      []string
}

// Summary separates generated attempts from actually distinct instantiated
// rewrites. Differential checks compare PostgreSQL with the independent Go
// oracle; metamorphic checks compare every rewrite with its PostgreSQL
// baseline.
type Summary struct {
	GeneratedAttempts     int      `json:"generated_attempts"`
	UniqueNormalizedPairs int      `json:"unique_normalized_pairs"`
	ExecutedUniquePairs   int      `json:"executed_unique_pairs"`
	DuplicateAttempts     int      `json:"duplicate_attempts"`
	RewriteTemplates      int      `json:"rewrite_templates"`
	Scenarios             int      `json:"scenarios"`
	FixtureRows           int      `json:"fixture_rows"`
	PairNormalization     string   `json:"pair_normalization"`
	PairSetSHA256         string   `json:"pair_set_sha256"`
	PairSignatures        []string `json:"normalized_pair_signatures"`
	DifferentialChecks    int      `json:"differential_checks"`
	MetamorphicChecks     int      `json:"metamorphic_checks"`
	PostgresStatements    int      `json:"postgres_statements"`
	Mismatches            int      `json:"mismatches"`
	Oracle                string   `json:"oracle"`
	OracleFixtureSHA256   string   `json:"oracle_fixture_sha256"`
	PostgresVersion       string   `json:"postgres_version"`
	PostgresMajor         int      `json:"postgres_major"`
}

// Run provisions a connection-local fixture, executes every rewrite on the
// supplied real PostgreSQL server, and fails if either the differential or
// metamorphic comparison finds a mismatch.
func Run(ctx context.Context, dsn string) (Summary, error) {
	if strings.TrimSpace(dsn) == "" {
		return Summary{}, errors.New("PostgreSQL oracle DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return Summary{}, fmt.Errorf("open PostgreSQL oracle: %w", err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("connect PostgreSQL oracle: %w", err)
	}
	defer conn.Close()

	rows := oracleFixture()
	summary := Summary{Oracle: OracleID, OracleFixtureSHA256: fixtureDigest(rows),
		FixtureRows: len(rows), PairNormalization: PairNormalization}
	if err := loadFixture(ctx, conn, rows); err != nil {
		return summary, err
	}
	if err := readPostgresVersion(ctx, conn, &summary); err != nil {
		return summary, err
	}
	if summary.PostgresMajor != 16 {
		return summary, fmt.Errorf("PostgreSQL oracle major version %d, want 16", summary.PostgresMajor)
	}

	unique := make(map[string]struct{}, ExpectedAttempts)
	templates := make(map[string]struct{}, ExpectedTemplates)
	scenarios := campaignScenarios()
	summary.Scenarios = len(scenarios)
	for _, current := range scenarios {
		expected := evaluateFixture(rows, current)
		baselineSQL := baselineQuery(current)
		baseline, err := queryRows(ctx, conn, baselineSQL)
		summary.PostgresStatements++
		summary.DifferentialChecks++
		if err != nil {
			return summary, fmt.Errorf("execute PostgreSQL baseline: %w\n%s", err, baselineSQL)
		}
		if !sameRows(baseline, expected) {
			summary.Mismatches++
			continue
		}

		for _, candidate := range rewrites(current) {
			summary.GeneratedAttempts++
			templates[candidate.Template] = struct{}{}
			signature := rewriteSignature(baselineSQL, candidate.SQL)
			if _, duplicate := unique[signature]; duplicate {
				summary.DuplicateAttempts++
				continue
			}
			unique[signature] = struct{}{}
			summary.ExecutedUniquePairs++
			actual := make([][]string, 0)
			failed := false
			for _, statement := range candidate.SQL {
				page, queryErr := queryRows(ctx, conn, statement)
				summary.PostgresStatements++
				if queryErr != nil {
					return summary, fmt.Errorf("execute PostgreSQL rewrite %s: %w\n%s", candidate.Template, queryErr, statement)
				}
				actual = append(actual, page...)
			}
			summary.DifferentialChecks++
			summary.MetamorphicChecks++
			if !sameRows(actual, expected) || !sameRows(actual, baseline) {
				failed = true
			}
			if failed {
				summary.Mismatches++
			}
		}
	}
	summary.UniqueNormalizedPairs = len(unique)
	summary.RewriteTemplates = len(templates)
	summary.PairSetSHA256 = signatureSetDigest(unique)
	summary.PairSignatures = sortedSignatures(unique)
	if summary.GeneratedAttempts != ExpectedAttempts || summary.UniqueNormalizedPairs != ExpectedAttempts ||
		summary.ExecutedUniquePairs != ExpectedAttempts || summary.DuplicateAttempts != 0 || summary.RewriteTemplates != ExpectedTemplates {
		return summary, fmt.Errorf("rewrite coverage attempts=%d unique=%d executed=%d duplicates=%d templates=%d, want %d/%d/%d/0/%d",
			summary.GeneratedAttempts, summary.UniqueNormalizedPairs, summary.ExecutedUniquePairs,
			summary.DuplicateAttempts, summary.RewriteTemplates,
			ExpectedAttempts, ExpectedAttempts, ExpectedAttempts, ExpectedTemplates)
	}
	if summary.Mismatches != 0 {
		return summary, fmt.Errorf("PostgreSQL oracle campaign found %d mismatches", summary.Mismatches)
	}
	return summary, nil
}

// CoverageSummary computes campaign cardinalities without contacting a
// database. It exists so accidental duplicate rewrites fail ordinary unit
// tests before a PostgreSQL campaign is run.
func CoverageSummary() Summary {
	unique := make(map[string]struct{}, ExpectedAttempts)
	templates := make(map[string]struct{}, ExpectedTemplates)
	result := Summary{Oracle: OracleID, PairNormalization: PairNormalization, FixtureRows: len(oracleFixture())}
	scenarios := campaignScenarios()
	result.Scenarios = len(scenarios)
	for _, current := range scenarios {
		baseline := baselineQuery(current)
		for _, candidate := range rewrites(current) {
			result.GeneratedAttempts++
			templates[candidate.Template] = struct{}{}
			signature := rewriteSignature(baseline, candidate.SQL)
			if _, duplicate := unique[signature]; duplicate {
				result.DuplicateAttempts++
				continue
			}
			unique[signature] = struct{}{}
			result.ExecutedUniquePairs++
		}
	}
	result.UniqueNormalizedPairs = len(unique)
	result.RewriteTemplates = len(templates)
	result.PairSetSHA256 = signatureSetDigest(unique)
	result.PairSignatures = sortedSignatures(unique)
	return result
}

func campaignScenarios() []scenario {
	result := make([]scenario, 0, len(departments)*len(minimumAmounts)*len(minimumDates))
	for _, department := range departments {
		for _, amount := range minimumAmounts {
			for _, date := range minimumDates {
				result = append(result, scenario{Department: department, MinimumAmount: amount, MinimumDate: date})
			}
		}
	}
	return result
}

func rewrites(current scenario) []rewrite {
	department := quoteLiteral(current.Department)
	amount := strconv.Itoa(current.MinimumAmount)
	date := quoteLiteral(current.MinimumDate)
	selectList := `department, amount::text, expense_date::text`
	ordered := ` ORDER BY entity_key`
	basePredicate := `department = ` + department + ` AND amount >= ` + amount + ` AND expense_date >= DATE ` + date
	result := []rewrite{
		{Template: "predicate_order", SQL: []string{`SELECT ` + selectList + ` FROM oracle_expenses WHERE expense_date >= DATE ` + date + ` AND department = ` + department + ` AND amount >= ` + amount + ordered}},
		{Template: "derived_projection", SQL: []string{`SELECT ` + selectList + ` FROM (SELECT entity_key, department, amount, expense_date FROM oracle_expenses WHERE department = ` + department + `) AS projected WHERE amount >= ` + amount + ` AND expense_date >= DATE ` + date + ordered}},
		{Template: "cte_pushdown", SQL: []string{`WITH filtered AS NOT MATERIALIZED (SELECT entity_key, department, amount, expense_date FROM oracle_expenses WHERE amount >= ` + amount + `) SELECT ` + selectList + ` FROM filtered WHERE department = ` + department + ` AND expense_date >= DATE ` + date + ordered}},
		{Template: "de_morgan", SQL: []string{`SELECT ` + selectList + ` FROM oracle_expenses WHERE NOT (department <> ` + department + ` OR amount < ` + amount + ` OR expense_date < DATE ` + date + `)` + ordered}},
		{Template: "correlated_exists", SQL: []string{`SELECT e.department, e.amount::text, e.expense_date::text FROM oracle_expenses AS e WHERE EXISTS (SELECT 1 FROM oracle_expenses AS witness WHERE witness.entity_key = e.entity_key AND witness.department = ` + department + ` AND witness.amount >= ` + amount + ` AND witness.expense_date >= DATE ` + date + `) ORDER BY e.entity_key`}},
		{Template: "values_join", SQL: []string{`SELECT e.department, e.amount::text, e.expense_date::text FROM oracle_expenses AS e JOIN (VALUES (` + department + `::text, ` + amount + `::numeric, DATE ` + date + `)) AS predicate(department, minimum_amount, minimum_date) ON e.department = predicate.department AND e.amount >= predicate.minimum_amount AND e.expense_date >= predicate.minimum_date ORDER BY e.entity_key`}},
	}
	for _, pageSize := range []int{2, 3} {
		statements := make([]string, 0, (len(oracleFixture())+pageSize-1)/pageSize)
		for offset := 0; offset < len(oracleFixture()); offset += pageSize {
			statements = append(statements, `SELECT `+selectList+` FROM oracle_expenses WHERE `+basePredicate+ordered+` LIMIT `+strconv.Itoa(pageSize)+` OFFSET `+strconv.Itoa(offset))
		}
		result = append(result, rewrite{Template: "offset_pages_" + strconv.Itoa(pageSize), SQL: statements})
	}
	return result
}

func baselineQuery(current scenario) string {
	return `SELECT department, amount::text, expense_date::text FROM oracle_expenses WHERE department = ` +
		quoteLiteral(current.Department) + ` AND amount >= ` + strconv.Itoa(current.MinimumAmount) +
		` AND expense_date >= DATE ` + quoteLiteral(current.MinimumDate) + ` ORDER BY entity_key`
}

func rewriteSignature(baseline string, candidate []string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(normalizeSQL(baseline)))
	for _, statement := range candidate {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(normalizeSQL(statement)))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func normalizeSQL(statement string) string { return strings.Join(strings.Fields(statement), " ") }

func signatureSetDigest(signatures map[string]struct{}) string {
	values := sortedSignatures(signatures)
	digest := sha256.New()
	_, _ = digest.Write([]byte("TASKGATE-POSTGRES-REWRITE-PAIR-SET-V1\x00"))
	for _, signature := range values {
		_, _ = digest.Write([]byte(signature))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func sortedSignatures(signatures map[string]struct{}) []string {
	values := make([]string, 0, len(signatures))
	for signature := range signatures {
		values = append(values, signature)
	}
	sort.Strings(values)
	return values
}

func quoteLiteral(value string) string { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }

func oracleFixture() []fixtureRow {
	text := func(value string) *string { return &value }
	number := func(value int) *int { return &value }
	return []fixtureRow{
		{EntityKey: "r01", Department: text("sales"), Amount: number(10), Date: text("2026-01-02")},
		{EntityKey: "r02", Department: text("sales"), Amount: number(20), Date: text("2026-01-20")},
		{EntityKey: "r03", Department: text("rnd"), Amount: number(30), Date: text("2026-02-03")},
		{EntityKey: "r04", Department: text("sales"), Amount: number(10), Date: text("2026-01-02")},
		{EntityKey: "r05", Department: text("ops"), Amount: number(5), Date: text("2026-01-15")},
		{EntityKey: "r06", Department: text("ops"), Amount: number(35), Date: text("2026-03-10")},
		{EntityKey: "r07", Department: text("rnd"), Amount: number(15), Date: text("2026-01-01")},
		{EntityKey: "r08", Department: text("legal"), Amount: number(25), Date: text("2026-02-01")},
		{EntityKey: "r09", Department: text("legal"), Amount: number(0), Date: text("2026-04-01")},
		{EntityKey: "r10", Department: nil, Amount: number(40), Date: text("2026-04-02")},
		{EntityKey: "r11", Department: text("sales"), Amount: nil, Date: text("2026-04-03")},
		{EntityKey: "r12", Department: text("sales"), Amount: number(40), Date: nil},
	}
}

func evaluateFixture(rows []fixtureRow, current scenario) [][]string {
	result := make([][]string, 0)
	for _, row := range rows {
		if row.Department == nil || row.Amount == nil || row.Date == nil ||
			*row.Department != current.Department || *row.Amount < current.MinimumAmount || *row.Date < current.MinimumDate {
			continue
		}
		result = append(result, []string{row.EntityKey, *row.Department, strconv.Itoa(*row.Amount), *row.Date})
	}
	sort.Slice(result, func(i, j int) bool { return result[i][0] < result[j][0] })
	for index := range result {
		result[index] = result[index][1:]
	}
	return result
}

func loadFixture(ctx context.Context, conn *sql.Conn, rows []fixtureRow) error {
	if _, err := conn.ExecContext(ctx, `SET TIME ZONE 'UTC'`); err != nil {
		return fmt.Errorf("set PostgreSQL oracle timezone: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE oracle_expenses (
entity_key text COLLATE "C" PRIMARY KEY,
department text COLLATE "C",
amount numeric,
expense_date date
)`); err != nil {
		return fmt.Errorf("create PostgreSQL oracle fixture: %w", err)
	}
	for _, row := range rows {
		if _, err := conn.ExecContext(ctx, `INSERT INTO oracle_expenses(entity_key, department, amount, expense_date) VALUES ($1, $2, $3, $4)`,
			row.EntityKey, row.Department, row.Amount, row.Date); err != nil {
			return fmt.Errorf("insert PostgreSQL oracle fixture row %s: %w", row.EntityKey, err)
		}
	}
	return nil
}

func readPostgresVersion(ctx context.Context, conn *sql.Conn, summary *Summary) error {
	var versionNumber int
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::integer`).Scan(
		&summary.PostgresVersion, &versionNumber); err != nil {
		return fmt.Errorf("read PostgreSQL oracle version: %w", err)
	}
	summary.PostgresMajor = versionNumber / 10000
	return nil
}

func queryRows(ctx context.Context, conn *sql.Conn, statement string) ([][]string, error) {
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([][]string, 0)
	for rows.Next() {
		var department, amount, date string
		if err := rows.Scan(&department, &amount, &date); err != nil {
			return nil, err
		}
		result = append(result, []string{department, amount, date})
	}
	return result, rows.Err()
}

func sameRows(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if len(left[index]) != len(right[index]) {
			return false
		}
		for column := range left[index] {
			if left[index][column] != right[index][column] {
				return false
			}
		}
	}
	return true
}

func fixtureDigest(rows []fixtureRow) string {
	encoded, _ := json.Marshal(rows)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
