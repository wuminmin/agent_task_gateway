// Package finalv5sqlcheck executes the SQL and plan artifacts the Final-V5
// Contract Index names, against a real PostgreSQL and the production compiler.
//
// It exists because contract verification used to check only that an indexed
// artifact had the reviewed digest and the reviewed structure. It never checked
// that the SQL parses. Three releases shipped a dataset probe that used the
// reserved keyword COLLATION as a bare CTE identifier and could not run at all;
// see contracts/AMENDMENT-v1.3.md.
package finalv5sqlcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdminDSNEnv names the environment variable holding a PostgreSQL superuser DSN
// on a disposable deployment. The gate creates and drops its own databases, so
// it must never be pointed at a deployment that matters.
const AdminDSNEnv = "TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN"

var databaseSequence atomic.Int64

// BenchmarkDatabase is a throwaway database holding exactly the relations the
// contract-indexed generator creates.
type BenchmarkDatabase struct {
	Name      string
	DSN       string
	adminDSN  string
	ServerNum string
	Version   string
}

// AdminDSN returns the configured disposable-deployment DSN.
func AdminDSN() (string, error) {
	dsn := strings.TrimSpace(os.Getenv(AdminDSNEnv))
	if dsn == "" {
		return "", fmt.Errorf("%s is required: the SQL executability gate needs a disposable PostgreSQL 16", AdminDSNEnv)
	}
	return dsn, nil
}

// Provision creates a fresh database and runs the contract-indexed dataset
// generator into it. The generator runs statement by statement with no error
// tolerated, so a partially generated database can never be presented as a
// successful one.
//
// The new database is cloned from the database adminDSN names, which must be a
// disposable deployment whose standard Final-V5 init scripts have already
// created taskgate_snapshot_owner, the frozen-publication mutation guard and
// the ProvSQL fixtures. The generator asserts those preconditions itself and
// refuses to run without them, so cloning is how the gate satisfies the
// contract's own stated stage rather than inventing a substitute environment.
func Provision(ctx context.Context, adminDSN, generatorSQL string) (*BenchmarkDatabase, error) {
	name := fmt.Sprintf("taskgate_sqlcheck_%d_%d", time.Now().UnixNano(), databaseSequence.Add(1))
	template, err := databaseName(adminDSN)
	if err != nil {
		return nil, err
	}
	maintenanceDSN, err := replaceDatabase(adminDSN, "postgres")
	if err != nil {
		return nil, err
	}
	admin, err := pgx.Connect(ctx, maintenanceDSN)
	if err != nil {
		return nil, fmt.Errorf("connect to the disposable deployment: %w", err)
	}
	defer admin.Close(context.Background())

	var serverNum, version string
	if err := admin.QueryRow(ctx,
		`SELECT current_setting('server_version_num'), current_setting('server_version')`).
		Scan(&serverNum, &version); err != nil {
		return nil, err
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`,
		pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize())); err != nil {
		return nil, fmt.Errorf("create benchmark database: %w", err)
	}

	dsn, err := replaceDatabase(adminDSN, name)
	if err != nil {
		return nil, err
	}
	database := &BenchmarkDatabase{Name: name, DSN: dsn, adminDSN: maintenanceDSN,
		ServerNum: serverNum, Version: version}
	if err := database.runScript(ctx, generatorSQL); err != nil {
		_ = database.Drop(context.Background())
		return nil, fmt.Errorf("dataset generator: %w", err)
	}
	return database, nil
}

// Drop removes the throwaway database.
func (database *BenchmarkDatabase) Drop(ctx context.Context) error {
	if database == nil {
		return nil
	}
	admin, err := pgx.Connect(ctx, database.adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	_, err = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{database.Name}.Sanitize()+` WITH (FORCE)`)
	return err
}

// runScript executes a contract SQL script exactly as written, with the
// psql meta-commands the contract uses stripped only because pgx speaks the
// wire protocol rather than psql. No statement is skipped and the first error
// aborts, which is the ON_ERROR_STOP behaviour the contract declares.
func (database *BenchmarkDatabase) runScript(ctx context.Context, script string) error {
	connection, err := pgx.Connect(ctx, database.DSN)
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	body := StripPsqlMetaCommands(script)
	if strings.TrimSpace(body) == "" {
		return errors.New("script is empty after removing psql meta-commands")
	}
	// A multi-statement Exec stops at the first failing statement and reports
	// it, so a generator that half-applies is a failure, not a partial success.
	if _, err := connection.Exec(ctx, body); err != nil {
		return err
	}
	return nil
}

// ScalarQuery runs a single-row single-column contract query and returns the
// value together with the returned column name.
func (database *BenchmarkDatabase) ScalarQuery(ctx context.Context, sql string) (value, column string, err error) {
	connection, err := pgx.Connect(ctx, database.DSN)
	if err != nil {
		return "", "", err
	}
	defer connection.Close(context.Background())
	rows, err := connection.Query(ctx, StripPsqlMetaCommands(sql))
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	if len(fields) != 1 {
		return "", "", fmt.Errorf("query returned %d columns, expected exactly one", len(fields))
	}
	column = string(fields[0].Name)
	if !rows.Next() {
		return "", "", errors.New("query returned no rows, expected exactly one")
	}
	if err := rows.Scan(&value); err != nil {
		return "", "", err
	}
	if rows.Next() {
		return "", "", errors.New("query returned more than one row")
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return value, column, nil
}

// Explain proves PostgreSQL can parse, analyse and plan a rendered statement
// without executing it. Planning resolves every relation, column, type,
// operator and function, so a template naming something that does not exist
// fails here.
func (database *BenchmarkDatabase) Explain(ctx context.Context, sql string) error {
	connection, err := pgx.Connect(ctx, database.DSN)
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	rows, err := connection.Query(ctx, "EXPLAIN "+StripPsqlMetaCommands(sql))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// StripPsqlMetaCommands removes the backslash meta-commands a contract script
// carries for psql. They are client directives, not SQL, and pgx speaks the
// wire protocol directly. Nothing else is altered: no rewriting, no
// substitution, and no repair of the statements themselves.
func StripPsqlMetaCommands(script string) string {
	lines := strings.Split(script, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Digest is the canonical digest helper the manifest uses.
func Digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func replaceDatabase(dsn, database string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse the disposable deployment DSN: %w", err)
	}
	parsed.Path = "/" + database
	return parsed.String(), nil
}

func databaseName(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse the disposable deployment DSN: %w", err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" || name == "postgres" {
		return "", errors.New("the disposable deployment DSN must name the initialized benchmark database")
	}
	return name, nil
}

// Prepare proves PostgreSQL can parse, analyse and rewrite a statement that
// still carries its positional placeholders. It resolves every relation,
// column, type, operator and function, and infers each parameter's type, so it
// works for a template of any parameter arity without the gate having to guess
// how many parameters the contract froze or what to substitute for them.
func (database *BenchmarkDatabase) Prepare(ctx context.Context, sql string) (int, error) {
	connection, err := pgx.Connect(ctx, database.DSN)
	if err != nil {
		return 0, err
	}
	defer connection.Close(context.Background())
	statement, err := connection.Prepare(ctx, "taskgate_sqlcheck", StripPsqlMetaCommands(sql))
	if err != nil {
		return 0, err
	}
	return len(statement.ParamOIDs), nil
}
