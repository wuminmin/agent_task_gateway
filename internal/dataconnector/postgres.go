// Package dataconnector executes policy-produced SQL against PostgreSQL using
// defense-in-depth read-only transactions, server-side timeouts, and row caps.
package dataconnector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	CodeSchemaDrift   ErrorCode = "DATA_CONNECTOR_SCHEMA_DRIFT"
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
	case CodeSchemaDrift:
		return string(e.Code) + ": reporting schema does not match the approved catalog"
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
	DSN                  string
	StatementTimeout     time.Duration
	ConnectTimeout       time.Duration
	MaxRows              int64
	MaxConnections       int32
	ApplicationName      string
	ExpectedSchema       []ViewSchema
	ExpectedSchemaDigest string
	ExpectedAttestation  ExpectedAttestation
}

// ExpectedAttestation pins the live PostgreSQL endpoint to the signed catalog
// source instead of accepting any DSN that exposes a compatible schema.
type ExpectedAttestation struct {
	DatasourceID           string
	Database               string
	User                   string
	PostgreSQLMajorVersion int
}

// Attestation is the datasource evidence carried into grants and receipts.
type Attestation struct {
	DatasourceID           string `json:"datasource_id"`
	Database               string `json:"database"`
	User                   string `json:"user"`
	PostgreSQLMajorVersion int    `json:"postgres_major_version"`
	SchemaDigest           string `json:"schema_digest"`
}

// ViewSchema is the catalog-pinned shape of one PostgreSQL reporting view.
// Column order is significant because SELECT * is forbidden and result
// metadata must remain stable across an approved task's lifetime.
type ViewSchema struct {
	Schema     string
	View       string
	Definition string
	Columns    []SchemaColumn
}

type SchemaColumn struct {
	Name           string
	PostgreSQLType string
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

// QueryPairRequest binds a visible query and its provenance companion to one
// repeatable-read database snapshot.
type QueryPairRequest struct {
	Visible    QueryRequest
	Provenance QueryRequest
}

type QueryPairResult struct {
	Visible    Result
	Provenance Result
}

// Connector owns a pgx connection pool. Its fields are immutable after New.
type Connector struct {
	pool                 *pgxpool.Pool
	statementTimeout     time.Duration
	maxRows              int64
	expectedSchema       []ViewSchema
	expectedSchemaDigest string
	expectedAttestation  ExpectedAttestation
	attestationMu        sync.RWMutex
	attestation          Attestation
}

type attestationQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
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
	connector := &Connector{
		pool:                 pool,
		statementTimeout:     normalized.StatementTimeout,
		maxRows:              normalized.MaxRows,
		expectedSchema:       cloneViewSchemas(normalized.ExpectedSchema),
		expectedSchemaDigest: normalized.ExpectedSchemaDigest,
		expectedAttestation:  normalized.ExpectedAttestation,
	}
	if _, err := connector.Attestation(connectContext); err != nil {
		pool.Close()
		return nil, err
	}
	return connector, nil
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
	_, err := c.Attestation(ctx)
	return err
}

// AttestSchema preserves the earlier readiness API while delegating to the full
// datasource attestation used by query execution.
func (c *Connector) AttestSchema(ctx context.Context) error {
	_, err := c.Attestation(ctx)
	return err
}

// Attestation verifies the configured datasource identity and reporting schema,
// then returns the evidence that higher layers bind into signed grants and
// gateway query receipts.
func (c *Connector) Attestation(ctx context.Context) (Attestation, error) {
	if c == nil || c.pool == nil {
		return Attestation{}, connectorError(CodeConnection, errors.New("connector is closed"))
	}
	attestation, err := c.attestDatasource(ctx, c.pool)
	if err != nil {
		return Attestation{}, err
	}
	c.rememberAttestation(attestation)
	return attestation, nil
}

func (c *Connector) attestDatasource(ctx context.Context, querier attestationQuerier) (Attestation, error) {
	attestation, err := c.liveIdentity(ctx, querier)
	if err != nil {
		return Attestation{}, err
	}
	schemaDigest, err := c.attestSchemaDigest(ctx, querier)
	if err != nil {
		return Attestation{}, err
	}
	attestation.SchemaDigest = schemaDigest
	if err := c.compareAttestation(attestation); err != nil {
		return Attestation{}, err
	}
	return attestation, nil
}

func (c *Connector) liveIdentity(ctx context.Context, querier attestationQuerier) (Attestation, error) {
	expected := c.expectedAttestation
	if expected == (ExpectedAttestation{}) {
		return Attestation{}, nil
	}
	var attestation Attestation
	var serverVersionNum string
	err := querier.QueryRow(ctx, `
SELECT COALESCE((SELECT datasource_id FROM reporting.datasource_attestation WHERE singleton = TRUE), ''),
       current_database(), current_user, current_setting('server_version_num')`).Scan(
		&attestation.DatasourceID, &attestation.Database, &attestation.User, &serverVersionNum,
	)
	if err != nil {
		return Attestation{}, connectorError(CodeSchemaDrift, err)
	}
	major, err := postgresMajorVersion(serverVersionNum)
	if err != nil {
		return Attestation{}, connectorError(CodeSchemaDrift, err)
	}
	attestation.PostgreSQLMajorVersion = major
	return attestation, nil
}

func (c *Connector) attestSchemaDigest(ctx context.Context, querier attestationQuerier) (string, error) {
	if len(c.expectedSchema) == 0 {
		return "", nil
	}
	actualSchemas := make([]ViewSchema, 0, len(c.expectedSchema))
	for _, expected := range c.expectedSchema {
		rows, err := querier.Query(ctx, `
SELECT column_name, data_type
FROM information_schema.columns
WHERE table_schema=$1 AND table_name=$2
ORDER BY ordinal_position`, expected.Schema, expected.View)
		if err != nil {
			return "", connectorError(CodeConnection, err)
		}
		var actual []SchemaColumn
		for rows.Next() {
			var column SchemaColumn
			if err := rows.Scan(&column.Name, &column.PostgreSQLType); err != nil {
				rows.Close()
				return "", connectorError(CodeConnection, err)
			}
			actual = append(actual, column)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", connectorError(CodeConnection, err)
		}
		rows.Close()
		if !sameSchemaColumns(expected.Columns, actual) {
			return "", connectorError(CodeSchemaDrift, errors.New("catalog/view mismatch"))
		}
		definition, err := c.viewDefinition(ctx, querier, expected)
		if err != nil {
			return "", err
		}
		actualSchemas = append(actualSchemas, ViewSchema{Schema: expected.Schema, View: expected.View, Definition: definition, Columns: actual})
	}
	digest, err := SchemaDigest(actualSchemas)
	if err != nil {
		return "", connectorError(CodeSchemaDrift, err)
	}
	return digest, nil
}

func (c *Connector) viewDefinition(ctx context.Context, querier attestationQuerier, expected ViewSchema) (string, error) {
	var definition string
	err := querier.QueryRow(ctx, `
WITH taskgate_schema_digest_path AS (
	SELECT set_config('search_path', 'pg_catalog', true)
)
SELECT pg_get_viewdef(format('%I.%I', $1::text, $2::text)::regclass, true)
FROM taskgate_schema_digest_path`, expected.Schema, expected.View).Scan(&definition)
	if err != nil {
		return "", connectorError(CodeSchemaDrift, err)
	}
	return definition, nil
}

func (c *Connector) compareAttestation(actual Attestation) error {
	expected := c.expectedAttestation
	if expected != (ExpectedAttestation{}) {
		if actual.DatasourceID != expected.DatasourceID ||
			actual.Database != expected.Database ||
			actual.User != expected.User ||
			actual.PostgreSQLMajorVersion != expected.PostgreSQLMajorVersion {
			return connectorError(CodeSchemaDrift, errors.New("datasource identity mismatch"))
		}
	}
	if c.expectedSchemaDigest != "" && actual.SchemaDigest != c.expectedSchemaDigest {
		return connectorError(CodeSchemaDrift, errors.New("schema digest mismatch"))
	}
	return nil
}

func (c *Connector) rememberAttestation(attestation Attestation) {
	c.attestationMu.Lock()
	c.attestation = attestation
	c.attestationMu.Unlock()
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
	attestation, err := c.attestDatasource(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	c.rememberAttestation(attestation)

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

// QueryPair executes a visible query and its provenance companion in one
// read-only repeatable-read transaction. Neither result is returned unless
// both statements and the transaction commit succeed.
func (c *Connector) QueryPair(ctx context.Context, request QueryPairRequest) (result QueryPairResult, err error) {
	if c == nil || c.pool == nil {
		return QueryPairResult{}, connectorError(CodeConnection, errors.New("connector is closed"))
	}
	for _, query := range []QueryRequest{request.Visible, request.Provenance} {
		if strings.TrimSpace(query.SQL) == "" || query.MaxRows < 0 || query.StatementTimeout < 0 {
			return QueryPairResult{}, connectorError(CodeInvalidQuery, errors.New("empty query or negative limit"))
		}
	}
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return QueryPairResult{}, connectorError(CodeConnection, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('search_path', 'pg_catalog', true)`); err != nil {
		return QueryPairResult{}, classifyQueryError(err)
	}
	attestation, err := c.attestDatasource(ctx, tx)
	if err != nil {
		return QueryPairResult{}, err
	}
	c.rememberAttestation(attestation)
	result.Visible, err = c.queryInTx(ctx, tx, request.Visible)
	if err != nil {
		return QueryPairResult{}, err
	}
	result.Provenance, err = c.queryInTx(ctx, tx, request.Provenance)
	if err != nil {
		return QueryPairResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return QueryPairResult{}, classifyQueryError(err)
	}
	committed = true
	return result, nil
}

func (c *Connector) queryInTx(ctx context.Context, tx pgx.Tx, request QueryRequest) (Result, error) {
	maxRows := clampRows(request.MaxRows, c.maxRows)
	if maxRows <= 0 {
		return Result{}, connectorError(CodeInvalidQuery, errors.New("row limit is zero"))
	}
	timeout := clampTimeout(request.StatementTimeout, c.statementTimeout)
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('statement_timeout', $1, true)`, timeoutSetting(timeout)); err != nil {
		return Result{}, classifyQueryError(err)
	}
	startedAt := time.Now()
	rows, err := tx.Query(ctx, request.SQL)
	if err != nil {
		return Result{}, classifyQueryError(err)
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	result := Result{Columns: make([]Column, 0, len(fields))}
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
	for _, view := range config.ExpectedSchema {
		if strings.TrimSpace(view.Schema) == "" || strings.TrimSpace(view.View) == "" || len(view.Columns) == 0 {
			return Config{}, connectorError(CodeInvalidConfig, errors.New("invalid expected reporting schema"))
		}
		for _, column := range view.Columns {
			if strings.TrimSpace(column.Name) == "" || strings.TrimSpace(column.PostgreSQLType) == "" {
				return Config{}, connectorError(CodeInvalidConfig, errors.New("invalid expected reporting column"))
			}
		}
	}
	if config.ExpectedSchemaDigest != "" && !isSHA256Hex(config.ExpectedSchemaDigest) {
		return Config{}, connectorError(CodeInvalidConfig, errors.New("invalid expected schema digest"))
	}
	expected := config.ExpectedAttestation
	if expected != (ExpectedAttestation{}) {
		if strings.TrimSpace(expected.DatasourceID) == "" || strings.TrimSpace(expected.Database) == "" ||
			strings.TrimSpace(expected.User) == "" || expected.PostgreSQLMajorVersion <= 0 {
			return Config{}, connectorError(CodeInvalidConfig, errors.New("invalid expected datasource attestation"))
		}
	}
	return config, nil
}

func SchemaDigest(schemas []ViewSchema) (string, error) {
	if len(schemas) == 0 {
		return "", errors.New("schema digest requires at least one reporting view")
	}
	views := cloneViewSchemas(schemas)
	sort.Slice(views, func(i, j int) bool {
		left := views[i].Schema + "." + views[i].View
		right := views[j].Schema + "." + views[j].View
		return left < right
	})
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-REPORTING-SCHEMA-V2\x00"))
	for _, view := range views {
		if strings.TrimSpace(view.Schema) == "" || strings.TrimSpace(view.View) == "" || len(view.Columns) == 0 {
			return "", errors.New("invalid reporting view shape")
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%d\x00", view.Schema, view.View, normalizeViewDefinition(view.Definition), len(view.Columns))
		for _, column := range view.Columns {
			if strings.TrimSpace(column.Name) == "" || strings.TrimSpace(column.PostgreSQLType) == "" {
				return "", errors.New("invalid reporting column shape")
			}
			_, _ = fmt.Fprintf(hash, "%s\x00%s\x00", column.Name, strings.ToLower(strings.TrimSpace(column.PostgreSQLType)))
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func normalizeViewDefinition(definition string) string {
	return strings.Join(strings.Fields(definition), " ")
}

func postgresMajorVersion(serverVersionNum string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(serverVersionNum))
	if err != nil || value <= 0 {
		return 0, errors.New("invalid PostgreSQL server_version_num")
	}
	return value / 10000, nil
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sameSchemaColumns(expected, actual []SchemaColumn) bool {
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		if expected[index].Name != actual[index].Name ||
			strings.ToLower(strings.TrimSpace(expected[index].PostgreSQLType)) != strings.ToLower(strings.TrimSpace(actual[index].PostgreSQLType)) {
			return false
		}
	}
	return true
}

func cloneViewSchemas(source []ViewSchema) []ViewSchema {
	result := make([]ViewSchema, len(source))
	for index, view := range source {
		result[index] = view
		result[index].Definition = view.Definition
		result[index].Columns = append([]SchemaColumn(nil), view.Columns...)
	}
	return result
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
