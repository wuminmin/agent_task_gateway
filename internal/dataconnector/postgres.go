// Package dataconnector executes policy-produced SQL against PostgreSQL using
// defense-in-depth read-only transactions, server-side timeouts, and row caps.
package dataconnector

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultStatementTimeout = 5 * time.Second
	DefaultConnectTimeout   = 10 * time.Second
	DefaultMaxRows          = int64(1000)
	DefaultMaxConnections   = int32(4)
)

// ErrorCode is stable and contains no DSN, SQL, or physical object name.
type ErrorCode string

const (
	CodeInvalidConfig ErrorCode = "DATA_CONNECTOR_INVALID_CONFIG"
	CodeConnection    ErrorCode = "DATA_CONNECTOR_CONNECTION_FAILED"
	CodeInvalidQuery  ErrorCode = "DATA_CONNECTOR_INVALID_QUERY"
	CodeQueryFailed   ErrorCode = "DATA_CONNECTOR_QUERY_FAILED"
	CodeQueryTimeout  ErrorCode = "DATA_CONNECTOR_QUERY_TIMEOUT"
)

// Error wraps an internal cause for logs while keeping Error() safe for an API
// response. Callers must not serialize Unwrap() to an untrusted client.
type Error struct {
	Code  ErrorCode
	cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	switch e.Code {
	case CodeInvalidConfig:
		return string(e.Code) + ": connector configuration is invalid"
	case CodeConnection:
		return string(e.Code) + ": the data source is unavailable"
	case CodeInvalidQuery:
		return string(e.Code) + ": the executable query is invalid"
	case CodeQueryTimeout:
		return string(e.Code) + ": the database statement timed out"
	default:
		return string(e.Code) + ": the read-only query failed"
	}
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// IsCode reports whether err has the supplied stable connector error code.
func IsCode(err error, code ErrorCode) bool {
	var connectorErr *Error
	return errors.As(err, &connectorErr) && connectorErr.Code == code
}

func connectorError(code ErrorCode, cause error) error {
	return &Error{Code: code, cause: cause}
}

// Config defines hard connector ceilings. A Query may request a shorter
// timeout or smaller row count, but never enlarge these values.
type Config struct {
	DSN              string
	StatementTimeout time.Duration
	ConnectTimeout   time.Duration
	MaxRows          int64
	MaxConnections   int32
	ApplicationName  string
}

// QueryRequest accepts only already-authorized executable SQL. Client
// parameters are intentionally absent from this boundary.
type QueryRequest struct {
	SQL              string
	StatementTimeout time.Duration
	MaxRows          int64
}

// Column contains result metadata that does not reveal a physical table OID.
type Column struct {
	Name        string `json:"name"`
	DataTypeOID uint32 `json:"data_type_oid"`
}

// Result is a bounded, in-memory query result. Higher layers encrypt it before
// persistence.
type Result struct {
	Columns      []Column      `json:"columns"`
	Rows         [][]any       `json:"rows"`
	RowCount     int64         `json:"row_count"`
	DatabaseTime time.Duration `json:"database_time"`
	Truncated    bool          `json:"truncated"`
}

// Connector owns a pgx connection pool. Its fields are immutable after New.
type Connector struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
	maxRows          int64
}

// New validates config, builds a pgx/v5 pool, and verifies connectivity. The
// reporting role should also be read-only at the PostgreSQL privilege layer.
func New(ctx context.Context, config Config) (*Connector, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(normalized.DSN)
	if err != nil {
		return nil, connectorError(CodeInvalidConfig, err)
	}
	poolConfig.MaxConns = normalized.MaxConnections
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolConfig.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	poolConfig.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = timeoutSetting(normalized.StatementTimeout)
	poolConfig.ConnConfig.RuntimeParams["application_name"] = normalized.ApplicationName

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, connectorError(CodeConnection, err)
	}
	connectContext, cancel := context.WithTimeout(ctx, normalized.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(connectContext); err != nil {
		pool.Close()
		return nil, connectorError(CodeConnection, err)
	}
	return &Connector{
		pool:             pool,
		statementTimeout: normalized.StatementTimeout,
		maxRows:          normalized.MaxRows,
	}, nil
}

// Close releases all PostgreSQL connections.
func (c *Connector) Close() {
	if c != nil && c.pool != nil {
		c.pool.Close()
	}
}

// Ping is suitable for a readiness check.
func (c *Connector) Ping(ctx context.Context) error {
	if c == nil || c.pool == nil {
		return connectorError(CodeConnection, errors.New("connector is closed"))
	}
	if err := c.pool.Ping(ctx); err != nil {
		return connectorError(CodeConnection, err)
	}
	return nil
}

// Query runs one bounded query in an explicit PostgreSQL read-only transaction.
func (c *Connector) Query(ctx context.Context, request QueryRequest) (result Result, err error) {
	if c == nil || c.pool == nil {
		return Result{}, connectorError(CodeConnection, errors.New("connector is closed"))
	}
	if strings.TrimSpace(request.SQL) == "" || request.MaxRows < 0 || request.StatementTimeout < 0 {
		return Result{}, connectorError(CodeInvalidQuery, errors.New("empty query or negative limit"))
	}
	maxRows := clampRows(request.MaxRows, c.maxRows)
	if maxRows <= 0 {
		return Result{}, connectorError(CodeInvalidQuery, errors.New("row limit is zero"))
	}
	timeout := clampTimeout(request.StatementTimeout, c.statementTimeout)

	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return Result{}, connectorError(CodeConnection, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	// set_config with is_local=true is transaction-scoped and parameterized;
	// no request text is interpolated into these settings.
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`, timeoutSetting(timeout)); err != nil {
		return Result{}, classifyQueryError(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('search_path', 'pg_catalog', true)`); err != nil {
		return Result{}, classifyQueryError(err)
	}

	startedAt := time.Now()
	rows, err := tx.Query(ctx, request.SQL)
	if err != nil {
		return Result{}, classifyQueryError(err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	result.Columns = make([]Column, 0, len(fields))
	for _, field := range fields {
		result.Columns = append(result.Columns, Column{Name: field.Name, DataTypeOID: field.DataTypeOID})
	}
	capacity := maxRows
	if capacity > 1024 {
		capacity = 1024
	}
	result.Rows = make([][]any, 0, int(capacity))
	for rows.Next() {
		if int64(len(result.Rows)) == maxRows {
			result.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return Result{}, classifyQueryError(err)
		}
		result.Rows = append(result.Rows, append([]any(nil), values...))
	}
	rows.Close()
	result.DatabaseTime = time.Since(startedAt)
	if err := rows.Err(); err != nil {
		return Result{}, classifyQueryError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, classifyQueryError(err)
	}
	committed = true
	result.RowCount = int64(len(result.Rows))
	return result, nil
}

func normalizeConfig(config Config) (Config, error) {
	if strings.TrimSpace(config.DSN) == "" || config.StatementTimeout < 0 || config.ConnectTimeout < 0 || config.MaxRows < 0 || config.MaxConnections < 0 {
		return Config{}, connectorError(CodeInvalidConfig, errors.New("missing DSN or negative ceiling"))
	}
	if config.StatementTimeout == 0 {
		config.StatementTimeout = DefaultStatementTimeout
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = DefaultConnectTimeout
	}
	if config.MaxRows == 0 {
		config.MaxRows = DefaultMaxRows
	}
	if config.MaxConnections == 0 {
		config.MaxConnections = DefaultMaxConnections
	}
	if config.ApplicationName == "" {
		config.ApplicationName = "taskbound-agent-data-gateway"
	}
	return config, nil
}

func clampRows(requested, maximum int64) int64 {
	if requested == 0 || requested > maximum {
		return maximum
	}
	return requested
}

func clampTimeout(requested, maximum time.Duration) time.Duration {
	if requested == 0 || requested > maximum {
		return maximum
	}
	return requested
}

func timeoutSetting(timeout time.Duration) string {
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	return strconv.FormatInt(milliseconds, 10) + "ms"
}

func classifyQueryError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return connectorError(CodeQueryTimeout, err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return connectorError(CodeQueryTimeout, err)
	}
	return connectorError(CodeQueryFailed, err)
}
