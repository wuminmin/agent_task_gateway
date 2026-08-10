package snapshotbundle

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const snapshotScanStatementTimeout = "600000ms"

// ScanPostgresSnapshot replaces caller-supplied rows with every declared
// physical field read from a frozen reporting materialized view. The source
// identity, owner boundary, physical types, and collation semantics are
// attested in the same read-only REPEATABLE READ transaction as the row scan.
// Compile must still be called afterwards: its expected digests are the
// human-reviewed assertion that these database cells are the approved
// publication.
func ScanPostgresSnapshot(ctx context.Context, input CompilerInput, dsn string) (CompilerInput, error) {
	if ctx == nil {
		return CompilerInput{}, errors.New("snapshot scan context is required")
	}
	schema, relation, err := splitSourceRelation(input.SourceRelation)
	if err != nil {
		return CompilerInput{}, err
	}
	if strings.TrimSpace(dsn) == "" {
		return CompilerInput{}, errors.New("SNAPSHOT_POSTGRES_DSN is required when source_relation is configured")
	}
	if !configNamePattern.MatchString(input.Snapshot.SourceID) {
		return CompilerInput{}, errors.New("snapshot source_id is invalid")
	}
	_, physicalFields, fieldTypes, err := prepareSnapshotFields(input.Snapshot.Fields)
	if err != nil {
		return CompilerInput{}, err
	}
	if len(physicalFields) == 0 {
		return CompilerInput{}, errors.New("snapshot scan requires at least one physical field")
	}
	if len(input.EntityKeyFields) == 0 {
		return CompilerInput{}, errors.New("snapshot scan requires at least one entity key field")
	}
	if _, err := prepareSidecarKeyFields(input.EntityKeyFields, fieldTypes); err != nil {
		return CompilerInput{}, err
	}

	connectionConfig, err := pgx.ParseConfig(strings.TrimSpace(dsn))
	if err != nil {
		return CompilerInput{}, errors.New("parse SNAPSHOT_POSTGRES_DSN")
	}
	// Publication scans are offline evidence production, not an execution
	// workload. Keep every statement on PostgreSQL's simple protocol so this
	// pre-run tool cannot create a prepared statement that could be mistaken
	// for a measured Gateway operation.
	connectionConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connectionConfig.ConnectTimeout = 10 * time.Second
	if connectionConfig.RuntimeParams == nil {
		connectionConfig.RuntimeParams = make(map[string]string)
	}
	connectionConfig.RuntimeParams["application_name"] = "taskgate-snapshot-index"
	connection, err := pgx.ConnectConfig(ctx, connectionConfig)
	if err != nil {
		return CompilerInput{}, fmt.Errorf("connect to snapshot PostgreSQL: %w", err)
	}
	defer connection.Close(context.Background())

	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return CompilerInput{}, fmt.Errorf("begin snapshot scan transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	settings := [][2]string{
		{"search_path", "pg_catalog"},
		{"statement_timeout", snapshotScanStatementTimeout},
		{"lock_timeout", "5s"},
		{"idle_in_transaction_session_timeout", "10min"},
		{"TimeZone", "UTC"},
		{"extra_float_digits", "3"},
	}
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config($1, $2, true)`, setting[0], setting[1]); err != nil {
			return CompilerInput{}, fmt.Errorf("set snapshot scan transaction policy: %w", err)
		}
	}
	var isolation, readOnly string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(
		&isolation, &readOnly,
	); err != nil {
		return CompilerInput{}, fmt.Errorf("verify snapshot scan transaction: %w", err)
	}
	if isolation != "repeatable read" || readOnly != "on" {
		return CompilerInput{}, errors.New("snapshot scan transaction is not read-only REPEATABLE READ")
	}
	if err := attestSnapshotDatasource(ctx, tx, input.Snapshot.SourceID); err != nil {
		return CompilerInput{}, err
	}
	if err := attestFrozenSourceRelation(ctx, tx, schema, relation); err != nil {
		return CompilerInput{}, err
	}
	if err := attestPhysicalFields(ctx, tx, schema, relation, physicalFields); err != nil {
		return CompilerInput{}, err
	}
	rows, err := scanPhysicalRows(ctx, tx, schema, relation, physicalFields)
	if err != nil {
		return CompilerInput{}, err
	}
	if len(rows) == 0 {
		return CompilerInput{}, errors.New("frozen source relation contains no rows")
	}
	var preparedStatementCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements`).Scan(&preparedStatementCount); err != nil {
		return CompilerInput{}, fmt.Errorf("verify snapshot scan prepared-statement state: %w", err)
	}
	if preparedStatementCount != 0 {
		return CompilerInput{}, fmt.Errorf("snapshot scan session contains %d prepared statements; expected zero",
			preparedStatementCount)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompilerInput{}, fmt.Errorf("commit snapshot scan transaction: %w", err)
	}
	committed = true
	input.Snapshot.Rows = rows
	return input, nil
}

func splitSourceRelation(source string) (string, string, error) {
	if !sourceRelationPattern.MatchString(source) {
		return "", "", errors.New("source_relation must match reporting.<lowercase_identifier>")
	}
	schema, relation, found := strings.Cut(source, ".")
	if !found || schema != "reporting" || relation == "" {
		return "", "", errors.New("invalid source_relation")
	}
	return schema, relation, nil
}

func attestSnapshotDatasource(ctx context.Context, tx pgx.Tx, expectedSourceID string) error {
	var actualSourceID string
	if err := tx.QueryRow(ctx, `
SELECT datasource_id
FROM reporting.datasource_attestation
WHERE singleton = TRUE`).Scan(&actualSourceID); err != nil {
		return fmt.Errorf("attest snapshot datasource: %w", err)
	}
	if actualSourceID != expectedSourceID {
		return errors.New("snapshot datasource identity mismatch")
	}
	return nil
}

func attestFrozenSourceRelation(ctx context.Context, tx pgx.Tx, schema, relation string) error {
	var state frozenSourceRelationState
	err := tx.QueryRow(ctx, `
SELECT cls.relkind::text,
       cls.relispopulated,
       owner_role.rolname,
       owner_role.rolcanlogin,
       reader_role.rolsuper,
       reader_role.rolcreatedb,
       reader_role.rolcreaterole,
       reader_role.rolreplication,
       reader_role.rolbypassrls,
       pg_has_role(current_user, owner_role.oid, 'MEMBER'),
       has_table_privilege(current_user, cls.oid, 'SELECT'),
       has_table_privilege(current_user, cls.oid, 'INSERT') OR
           has_table_privilege(current_user, cls.oid, 'UPDATE') OR
           has_table_privilege(current_user, cls.oid, 'DELETE') OR
           has_table_privilege(current_user, cls.oid, 'TRUNCATE')
FROM pg_catalog.pg_namespace AS ns
JOIN pg_catalog.pg_class AS cls ON cls.relnamespace = ns.oid
JOIN pg_catalog.pg_roles AS owner_role ON owner_role.oid = cls.relowner
JOIN pg_catalog.pg_roles AS reader_role ON reader_role.rolname = current_user
WHERE ns.nspname = $1 AND cls.relname = $2`, schema, relation).Scan(
		&state.RelationKind, &state.Populated, &state.OwnerName, &state.OwnerCanLogin,
		&state.RoleSuperuser, &state.RoleCreateDB, &state.RoleCreateRole, &state.RoleReplication, &state.RoleBypassRLS,
		&state.OwnerMember, &state.CanSelect, &state.CanWrite,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("source_relation does not exist")
		}
		return fmt.Errorf("attest frozen source relation: %w", err)
	}
	return validateFrozenSourceRelation(state)
}

type frozenSourceRelationState struct {
	RelationKind    string
	OwnerName       string
	Populated       bool
	OwnerCanLogin   bool
	RoleSuperuser   bool
	RoleCreateDB    bool
	RoleCreateRole  bool
	RoleReplication bool
	RoleBypassRLS   bool
	OwnerMember     bool
	CanSelect       bool
	CanWrite        bool
}

func validateFrozenSourceRelation(state frozenSourceRelationState) error {
	if state.RelationKind != "m" || !state.Populated {
		return errors.New("source_relation must be a populated materialized view")
	}
	if state.OwnerCanLogin || state.OwnerMember || state.OwnerName == "" {
		return errors.New("source_relation must have a NOLOGIN owner outside the scanner role hierarchy")
	}
	if state.RoleSuperuser || state.RoleCreateDB || state.RoleCreateRole || state.RoleReplication || state.RoleBypassRLS {
		return errors.New("snapshot scanner role has administrative PostgreSQL attributes")
	}
	if !state.CanSelect || state.CanWrite {
		return errors.New("snapshot scanner role must have read-only access to source_relation")
	}
	return nil
}

type postgresPhysicalField struct {
	Name                   string
	SQLType                string
	Collation              string
	CollationVersion       string
	CollationDeterministic bool
}

func attestPhysicalFields(ctx context.Context, tx pgx.Tx, schema, relation string, expected []physicalSnapshotField) error {
	names := make([]string, len(expected))
	for index := range expected {
		names[index] = expected[index].Name
	}
	rows, err := tx.Query(ctx, `
SELECT attr.attname,
       CASE
           WHEN typ.typtype = 'd' THEN
               CASE
                   WHEN base_typ.typelem <> 0 AND base_typ.typlen = -1 THEN 'ARRAY'
                   WHEN base_typ_ns.nspname = 'pg_catalog' THEN pg_catalog.format_type(typ.typbasetype, NULL)
                   ELSE 'USER-DEFINED'
               END
           ELSE
               CASE
                   WHEN typ.typelem <> 0 AND typ.typlen = -1 THEN 'ARRAY'
                   WHEN typ_ns.nspname = 'pg_catalog' THEN pg_catalog.format_type(attr.atttypid, NULL)
                   ELSE 'USER-DEFINED'
               END
       END,
       CASE WHEN coll.oid IS NULL THEN '' WHEN coll.collname = 'default' THEN db.datcollate ELSE coll.collname END,
       COALESCE(CASE WHEN coll.oid IS NULL THEN '' WHEN coll.collname = 'default' THEN db.datcollversion ELSE pg_catalog.pg_collation_actual_version(coll.oid) END, ''),
       COALESCE(coll.collisdeterministic, TRUE)
FROM pg_catalog.pg_namespace AS ns
JOIN pg_catalog.pg_class AS cls ON cls.relnamespace = ns.oid
JOIN pg_catalog.pg_attribute AS attr ON attr.attrelid = cls.oid AND attr.attnum > 0 AND NOT attr.attisdropped
JOIN pg_catalog.pg_type AS typ ON typ.oid = attr.atttypid
JOIN pg_catalog.pg_namespace AS typ_ns ON typ_ns.oid = typ.typnamespace
LEFT JOIN pg_catalog.pg_type AS base_typ ON typ.typtype = 'd' AND base_typ.oid = typ.typbasetype
LEFT JOIN pg_catalog.pg_namespace AS base_typ_ns ON base_typ_ns.oid = base_typ.typnamespace
LEFT JOIN pg_catalog.pg_collation AS coll ON coll.oid = attr.attcollation
JOIN pg_catalog.pg_database AS db ON db.datname = current_database()
WHERE ns.nspname = $1 AND cls.relname = $2 AND cls.relkind = 'm'
  AND attr.attname = ANY($3::text[])
ORDER BY attr.attnum`, schema, relation, names)
	if err != nil {
		return fmt.Errorf("inspect source_relation fields: %w", err)
	}
	defer rows.Close()
	actual := make(map[string]postgresPhysicalField, len(expected))
	for rows.Next() {
		var field postgresPhysicalField
		if err := rows.Scan(&field.Name, &field.SQLType, &field.Collation, &field.CollationVersion, &field.CollationDeterministic); err != nil {
			return fmt.Errorf("scan source_relation field metadata: %w", err)
		}
		if _, duplicate := actual[field.Name]; duplicate {
			return fmt.Errorf("source_relation contains duplicate physical field %q", field.Name)
		}
		actual[field.Name] = field
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate source_relation field metadata: %w", err)
	}
	for _, declared := range expected {
		field, found := actual[declared.Name]
		if !found {
			return fmt.Errorf("source_relation misses declared physical field %q", declared.Name)
		}
		canonicalType, err := exposure.CanonicalSQLTypeV2(field.SQLType)
		if err != nil || canonicalType != declared.SQLType {
			return fmt.Errorf("source_relation field %q type mismatch", declared.Name)
		}
		if field.Collation != declared.Collation || field.CollationVersion != declared.CollationVersion ||
			(declared.Collation != "" && !field.CollationDeterministic) {
			return fmt.Errorf("source_relation field %q collation mismatch", declared.Name)
		}
	}
	return nil
}

func scanPhysicalRows(ctx context.Context, tx pgx.Tx, schema, relation string, fields []physicalSnapshotField) ([]SnapshotRow, error) {
	relationSQL := pgx.Identifier{schema, relation}.Sanitize()
	aliasSQL := pgx.Identifier{"snapshot_source"}.Sanitize()
	projections := make([]string, len(fields))
	for index, field := range fields {
		valueSQL := aliasSQL + "." + pgx.Identifier{field.Name}.Sanitize()
		if field.SQLType == "bytea" {
			valueSQL = "CASE WHEN " + valueSQL + " IS NULL THEN NULL ELSE 'base64:' || " +
				"pg_catalog.replace(pg_catalog.encode(" + valueSQL + ", 'base64'), pg_catalog.chr(10), '') END"
		}
		projections[index] = "COALESCE(pg_catalog.to_jsonb(" + valueSQL + ")::text, 'null')"
	}
	query := "SELECT " + strings.Join(projections, ", ") + " FROM " + relationSQL + " AS " + aliasSQL
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("scan frozen source_relation: %w", err)
	}
	defer rows.Close()

	result := make([]SnapshotRow, 0)
	for rowIndex := 0; rows.Next(); rowIndex++ {
		encoded := make([]string, len(fields))
		destinations := make([]any, len(fields))
		for index := range encoded {
			destinations[index] = &encoded[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("scan source_relation row %d: %w", rowIndex, err)
		}
		values := make(map[string]any, len(fields))
		rowBytes := uint64(0)
		for index, field := range fields {
			rowBytes += uint64(len(encoded[index]))
			if rowBytes < uint64(len(encoded[index])) || rowBytes > maxSidecarLineBytes {
				return nil, fmt.Errorf("source_relation row %d exceeds the per-row scan limit", rowIndex)
			}
			var value any
			if err := decodeStrictJSON(strings.NewReader(encoded[index]), &value); err != nil {
				return nil, fmt.Errorf("decode source_relation row %d field %q: %w", rowIndex, field.Name, err)
			}
			value, err = normalizeScannedFloat(field.SQLType, value)
			if err != nil {
				return nil, fmt.Errorf("decode source_relation row %d field %q: %w", rowIndex, field.Name, err)
			}
			canonicalInput, err := normalizeJSONValue(field.SQLType, value)
			if err != nil {
				return nil, fmt.Errorf("validate source_relation row %d field %q: %w", rowIndex, field.Name, err)
			}
			if _, err := exposure.CanonicalSQLValue(field.SQLType, canonicalInput); err != nil {
				return nil, fmt.Errorf("validate source_relation row %d field %q: %w", rowIndex, field.Name, err)
			}
			values[field.Name] = value
		}
		result = append(result, SnapshotRow{Values: values})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate frozen source_relation: %w", err)
	}
	return result, nil
}

func normalizeScannedFloat(sqlType string, value any) (any, error) {
	if sqlType != "real" && sqlType != "double precision" {
		return value, nil
	}
	text, isString := value.(string)
	if !isString {
		return value, nil
	}
	switch strings.ToLower(text) {
	case "nan":
		return math.NaN(), nil
	case "infinity", "+infinity":
		return math.Inf(1), nil
	case "-infinity":
		return math.Inf(-1), nil
	default:
		return nil, errors.New("PostgreSQL encoded a finite floating value as a JSON string")
	}
}
