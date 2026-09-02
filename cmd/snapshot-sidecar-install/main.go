// snapshot-sidecar-install is an offline publication step. It verifies the
// immutable compiler bundle, imports its ordinal companion with COPY, proves
// that the companion and reporting snapshot have the same entity-key set,
// and only then transfers the tables to the NOLOGIN snapshot owner.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	ownerRole  = "taskgate_snapshot_owner"
	readerRole = "gateway_reader"
	maxLine    = 64 << 20
)

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type inputPaths []string

func (p *inputPaths) String() string { return strings.Join(*p, ",") }
func (p *inputPaths) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("input path is empty")
	}
	*p = append(*p, value)
	return nil
}

type publication struct {
	input          snapshotbundle.CompilerInput
	bundle         snapshotbundle.BundleManifest
	header         snapshotbundle.SidecarHeader
	sidecarPath    string
	sourceSchema   string
	sourceRelation string
	sidecarSchema  string
	sidecarTable   string
}

func main() {
	artifactDirectory := flag.String("artifact-dir", "", "directory containing immutable publication bundles")
	var inputs inputPaths
	flag.Var(&inputs, "input", "snapshot compiler input used for one bundle (repeatable)")
	flag.Parse()
	if strings.TrimSpace(*artifactDirectory) == "" || len(inputs) == 0 || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	dsn := strings.TrimSpace(os.Getenv("SNAPSHOT_INSTALL_POSTGRES_DSN"))
	if dsn == "" {
		fatal(errors.New("SNAPSHOT_INSTALL_POSTGRES_DSN is required"))
	}

	publications := make([]publication, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, inputPath := range inputs {
		candidate, err := loadPublication(*artifactDirectory, inputPath)
		if err != nil {
			fatal(err)
		}
		if _, duplicate := seen[candidate.bundle.PublicationName]; duplicate {
			fatal(fmt.Errorf("publication %q was supplied twice", candidate.bundle.PublicationName))
		}
		seen[candidate.bundle.PublicationName] = struct{}{}
		publications = append(publications, candidate)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := install(ctx, dsn, publications); err != nil {
		fatal(err)
	}
	for _, item := range publications {
		fmt.Printf("installed_publication=%s row_count=%d manifest_digest=%s sidecar=%s\n",
			item.bundle.PublicationName, item.bundle.RowCount, item.bundle.ManifestDigest, item.bundle.OrdinalSidecar)
	}
}

func loadPublication(artifactDirectory, inputPath string) (publication, error) {
	inputFile, err := os.Open(inputPath)
	if err != nil {
		return publication{}, fmt.Errorf("open compiler input: %w", err)
	}
	input, decodeErr := snapshotbundle.DecodeCompilerInput(inputFile)
	closeErr := inputFile.Close()
	if decodeErr != nil || closeErr != nil {
		return publication{}, fmt.Errorf("decode compiler input: %w", errors.Join(decodeErr, closeErr))
	}
	sourceSchema, sourceRelation, ok := splitRelation(input.SourceRelation, "reporting")
	if !ok {
		return publication{}, errors.New("compiler source_relation is not a safe reporting relation")
	}
	sidecarSchema, sidecarTable, ok := splitRelation(input.OrdinalSidecar, "taskgate_ordinal")
	if !ok {
		return publication{}, errors.New("compiler ordinal_sidecar is not a safe TaskGate relation")
	}

	directory := filepath.Join(artifactDirectory, input.PublicationName)
	manifestPath := filepath.Join(directory, input.PublicationName+".bundle.json")
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return publication{}, fmt.Errorf("open bundle manifest: %w", err)
	}
	bundle, decodeErr := snapshotbundle.DecodeBundleManifest(manifestFile)
	closeErr = manifestFile.Close()
	if decodeErr != nil || closeErr != nil {
		return publication{}, fmt.Errorf("decode bundle manifest: %w", errors.Join(decodeErr, closeErr))
	}
	manifest := bundle.DictionaryManifest
	if bundle.PublicationName != input.PublicationName || bundle.CatalogSource != input.CatalogSource ||
		bundle.OrdinalSidecar != input.OrdinalSidecar || manifest.SourceID != input.Snapshot.SourceID ||
		manifest.SourceNamespace != input.Snapshot.SourceNamespace || manifest.Snapshot != input.Snapshot.Snapshot ||
		manifest.SchemaDigest != input.Snapshot.SchemaDigest {
		return publication{}, errors.New("bundle identity differs from its compiler input")
	}

	hotPath := filepath.Join(directory, bundle.Hot.Name)
	hot, err := readVerifiedFile(hotPath, bundle.Hot)
	if err != nil {
		return publication{}, fmt.Errorf("read HOT artifact: %w", err)
	}
	index, err := ordinal.ParseHotDictionary(hot, bundle.ManifestDigest)
	if err != nil {
		return publication{}, fmt.Errorf("verify HOT artifact: %w", err)
	}
	if index.RowCount() != bundle.RowCount || index.DictionaryDigest() != manifest.DictionaryDigest {
		return publication{}, errors.New("HOT artifact does not match bundle envelope")
	}

	sidecarPath := filepath.Join(directory, bundle.Sidecar.Name)
	sidecarFile, err := os.Open(sidecarPath)
	if err != nil {
		return publication{}, fmt.Errorf("open sidecar artifact: %w", err)
	}
	info, err := sidecarFile.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != bundle.Sidecar.Bytes {
		_ = sidecarFile.Close()
		return publication{}, errors.New("sidecar artifact size or file type differs from bundle")
	}
	hash := sha256.New()
	expected := snapshotbundle.SidecarExpectation{PublicationName: bundle.PublicationName,
		OrdinalSidecar: bundle.OrdinalSidecar, SourceNamespace: manifest.SourceNamespace,
		ManifestDigest: bundle.ManifestDigest, SidecarDigest: manifest.SidecarDigest}
	verifyErr := snapshotbundle.VerifySidecarNDJSON(io.TeeReader(sidecarFile, hash), index, expected)
	closeErr = sidecarFile.Close()
	if verifyErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != bundle.Sidecar.SHA256 {
		return publication{}, fmt.Errorf("verify sidecar artifact: %w", errors.Join(verifyErr, closeErr))
	}
	header, err := readSidecarHeader(sidecarPath)
	if err != nil {
		return publication{}, err
	}
	if len(header.EntityKeyFields) != len(input.EntityKeyFields) {
		return publication{}, errors.New("sidecar entity key differs from compiler input")
	}
	for index, name := range input.EntityKeyFields {
		if header.EntityKeyFields[index].Name != name {
			return publication{}, errors.New("sidecar entity key order differs from compiler input")
		}
	}
	return publication{input: input, bundle: bundle, header: header, sidecarPath: sidecarPath,
		sourceSchema: sourceSchema, sourceRelation: sourceRelation,
		sidecarSchema: sidecarSchema, sidecarTable: sidecarTable}, nil
}

func readVerifiedFile(path string, descriptor snapshotbundle.FileDescriptor) ([]byte, error) {
	if descriptor.Bytes <= 0 || descriptor.Bytes > snapshotbundle.DefaultPublicationLimits().MaxHotBytes {
		return nil, errors.New("artifact descriptor is outside the installer limit")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) != descriptor.Bytes {
		return nil, errors.New("artifact size differs from descriptor")
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != descriptor.SHA256 {
		return nil, errors.New("artifact SHA-256 differs from descriptor")
	}
	return payload, nil
}

func readSidecarHeader(path string) (snapshotbundle.SidecarHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return snapshotbundle.SidecarHeader{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	if !scanner.Scan() {
		return snapshotbundle.SidecarHeader{}, errors.New("sidecar has no header")
	}
	var header snapshotbundle.SidecarHeader
	if err := decodeJSONLine(scanner.Bytes(), &header); err != nil || header.Type != "header" {
		return snapshotbundle.SidecarHeader{}, errors.New("sidecar header is invalid after verification")
	}
	return header, nil
}

func install(ctx context.Context, dsn string, publications []publication) error {
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to publication PostgreSQL: %w", err)
	}
	defer connection.Close(context.Background())
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(6072350824599964721)`); err != nil {
		return err
	}
	if err := verifyRoles(ctx, tx); err != nil {
		return err
	}
	var metadataExists bool
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass('taskgate_ordinal.publications') IS NOT NULL`).Scan(&metadataExists); err != nil {
		return err
	}
	if !metadataExists {
		if err := createPublicationMetadata(ctx, tx); err != nil {
			return err
		}
		for index := range publications {
			if err := importPublication(ctx, tx, index, publications[index]); err != nil {
				return fmt.Errorf("import %s: %w", publications[index].bundle.PublicationName, err)
			}
		}
		if err := sealPublicationMetadata(ctx, tx); err != nil {
			return err
		}
	}
	if err := verifyPublicationSet(ctx, tx, publications); err != nil {
		return err
	}
	for _, item := range publications {
		if err := verifyPublishedSidecar(ctx, tx, item); err != nil {
			return fmt.Errorf("verify %s: %w", item.bundle.PublicationName, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func verifyRoles(ctx context.Context, tx pgx.Tx) error {
	var currentSuper, ownerLogin, ownerAdmin, readerAdmin bool
	err := tx.QueryRow(ctx, `
SELECT current_principal.rolsuper,
       owner.rolcanlogin,
       owner.rolsuper OR owner.rolcreatedb OR owner.rolcreaterole OR owner.rolreplication OR owner.rolbypassrls,
       reader.rolsuper OR reader.rolcreatedb OR reader.rolcreaterole OR reader.rolreplication OR reader.rolbypassrls
FROM pg_catalog.pg_roles AS current_principal
CROSS JOIN pg_catalog.pg_roles AS owner
CROSS JOIN pg_catalog.pg_roles AS reader
WHERE current_principal.rolname = current_user AND owner.rolname = $1 AND reader.rolname = $2`, ownerRole, readerRole).Scan(
		&currentSuper, &ownerLogin, &ownerAdmin, &readerAdmin,
	)
	if err != nil {
		return fmt.Errorf("inspect publication roles: %w", err)
	}
	if !currentSuper || ownerLogin || ownerAdmin || readerAdmin {
		return errors.New("publication installer role boundary is unsafe")
	}
	return nil
}

func createPublicationMetadata(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
CREATE TABLE taskgate_ordinal.publications (
    publication_name text PRIMARY KEY,
    manifest_digest text NOT NULL UNIQUE CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    dictionary_digest text NOT NULL UNIQUE CHECK (dictionary_digest ~ '^[0-9a-f]{64}$'),
    sidecar_digest text NOT NULL UNIQUE CHECK (sidecar_digest ~ '^[0-9a-f]{64}$'),
    source_relation text NOT NULL UNIQUE CHECK (source_relation ~ '^reporting[.][a-z_][a-z0-9_]*$'),
    ordinal_sidecar text NOT NULL UNIQUE CHECK (ordinal_sidecar ~ '^taskgate_ordinal[.][a-z_][a-z0-9_]*$'),
    row_count bigint NOT NULL CHECK (row_count > 0)
)`)
	return err
}

func importPublication(ctx context.Context, tx pgx.Tx, position int, item publication) error {
	stage := fmt.Sprintf("tg_sidecar_stage_%d", position)
	stageSQL := pgx.Identifier{stage}.Sanitize()
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE `+stageSQL+` (
row_handle bigint NOT NULL, entity_key text NOT NULL, key_values text NOT NULL
) ON COMMIT DROP`); err != nil {
		return err
	}
	file, err := os.Open(item.sidecarPath)
	if err != nil {
		return err
	}
	source := newSidecarCopySource(file, item.bundle.RowCount)
	count, copyErr := tx.CopyFrom(ctx, pgx.Identifier{"pg_temp", stage},
		[]string{"row_handle", "entity_key", "key_values"}, source)
	closeErr := file.Close()
	if copyErr != nil || source.Err() != nil || closeErr != nil {
		return errors.Join(copyErr, source.Err(), closeErr)
	}
	if uint64(count) != item.bundle.RowCount {
		return fmt.Errorf("copied %d rows, want %d", count, item.bundle.RowCount)
	}

	tableSQL := qualified(item.sidecarSchema, item.sidecarTable)
	columns := []string{
		`row_handle bigint PRIMARY KEY CHECK (row_handle BETWEEN 1 AND ` + fmt.Sprint(item.bundle.RowCount) + `)`,
		`entity_key text NOT NULL UNIQUE CHECK (entity_key ~ '^[0-9a-f]{64}$')`,
	}
	keyColumns := make([]string, 0, len(item.header.EntityKeyFields))
	selects := []string{"row_handle", "entity_key"}
	for index, field := range item.header.EntityKeyFields {
		if !identifier.MatchString(field.Name) || !safeSQLType(field.SQLType) {
			return errors.New("sidecar entity key has an unsafe SQL identity")
		}
		column := quoteIdentifier(field.Name)
		columns = append(columns, column+" "+field.SQLType+" NOT NULL")
		keyColumns = append(keyColumns, column)
		selects = append(selects, sidecarCastExpression(index, field.SQLType))
	}
	columns = append(columns, "UNIQUE ("+strings.Join(keyColumns, ", ")+")")
	if _, err := tx.Exec(ctx, "CREATE TABLE "+tableSQL+" ("+strings.Join(columns, ", ")+")"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+tableSQL+" SELECT "+strings.Join(selects, ", ")+" FROM "+stageSQL+" ORDER BY row_handle"); err != nil {
		return fmt.Errorf("materialize typed sidecar: %w", err)
	}
	manifest := item.bundle.DictionaryManifest
	if _, err := tx.Exec(ctx, `
INSERT INTO taskgate_ordinal.publications
    (publication_name, manifest_digest, dictionary_digest, sidecar_digest,
     source_relation, ordinal_sidecar, row_count)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, item.bundle.PublicationName,
		item.bundle.ManifestDigest, manifest.DictionaryDigest, manifest.SidecarDigest,
		item.input.SourceRelation, item.input.OrdinalSidecar, int64(item.bundle.RowCount)); err != nil {
		return err
	}
	if err := verifyEntitySet(ctx, tx, item); err != nil {
		return err
	}
	statements := []string{
		"REVOKE ALL ON TABLE " + tableSQL + " FROM PUBLIC, " + quoteIdentifier(readerRole),
		"GRANT SELECT ON TABLE " + tableSQL + " TO " + quoteIdentifier(readerRole),
		"CREATE TRIGGER reject_frozen_sidecar_mutation BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON " + tableSQL +
			" FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()",
		"ALTER TABLE " + tableSQL + " OWNER TO " + quoteIdentifier(ownerRole),
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func sealPublicationMetadata(ctx context.Context, tx pgx.Tx) error {
	statements := []string{
		`REVOKE ALL ON TABLE taskgate_ordinal.publications FROM PUBLIC, gateway_reader`,
		`CREATE TRIGGER reject_frozen_publication_metadata_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON taskgate_ordinal.publications
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()`,
		`ALTER TABLE taskgate_ordinal.publications OWNER TO taskgate_snapshot_owner`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func verifyPublicationSet(ctx context.Context, tx pgx.Tx, publications []publication) error {
	rows, err := tx.Query(ctx, `
SELECT publication_name, manifest_digest, dictionary_digest, sidecar_digest,
       source_relation, ordinal_sidecar, row_count
FROM taskgate_ordinal.publications
ORDER BY publication_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := make(map[string]publication, len(publications))
	for _, item := range publications {
		expected[item.bundle.PublicationName] = item
	}
	seen := 0
	for rows.Next() {
		var name, manifestDigest, dictionaryDigest, sidecarDigest, sourceRelation, sidecar string
		var rowCount int64
		if err := rows.Scan(&name, &manifestDigest, &dictionaryDigest, &sidecarDigest, &sourceRelation, &sidecar, &rowCount); err != nil {
			return err
		}
		item, found := expected[name]
		if !found || manifestDigest != item.bundle.ManifestDigest ||
			dictionaryDigest != item.bundle.DictionaryManifest.DictionaryDigest ||
			sidecarDigest != item.bundle.DictionaryManifest.SidecarDigest ||
			sourceRelation != item.input.SourceRelation || sidecar != item.input.OrdinalSidecar ||
			rowCount != int64(item.bundle.RowCount) {
			return errors.New("installed publication metadata differs from immutable bundle set")
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seen != len(expected) {
		return errors.New("installed publication set is incomplete")
	}
	return nil
}

func verifyPublishedSidecar(ctx context.Context, tx pgx.Tx, item publication) error {
	if err := verifyEntitySet(ctx, tx, item); err != nil {
		return err
	}
	var kind, owner string
	var canRead, canWrite, triggerEnabled bool
	err := tx.QueryRow(ctx, `
SELECT cls.relkind::text,
       pg_catalog.pg_get_userbyid(cls.relowner),
       pg_catalog.has_table_privilege($1, cls.oid, 'SELECT'),
       pg_catalog.has_table_privilege($1, cls.oid, 'INSERT') OR
           pg_catalog.has_table_privilege($1, cls.oid, 'UPDATE') OR
           pg_catalog.has_table_privilege($1, cls.oid, 'DELETE') OR
           pg_catalog.has_table_privilege($1, cls.oid, 'TRUNCATE'),
       EXISTS (
           SELECT 1 FROM pg_catalog.pg_trigger
           WHERE tgrelid = cls.oid AND tgname = 'reject_frozen_sidecar_mutation'
             AND tgenabled = 'O' AND NOT tgisinternal
       )
FROM pg_catalog.pg_class AS cls
JOIN pg_catalog.pg_namespace AS ns ON ns.oid = cls.relnamespace
WHERE ns.nspname = $2 AND cls.relname = $3`, readerRole, item.sidecarSchema, item.sidecarTable).Scan(
		&kind, &owner, &canRead, &canWrite, &triggerEnabled,
	)
	if err != nil {
		return err
	}
	if kind != "r" || owner != ownerRole || !canRead || canWrite || !triggerEnabled {
		return errors.New("installed sidecar is not sealed behind the read-only owner boundary")
	}
	return nil
}

func verifyEntitySet(ctx context.Context, tx pgx.Tx, item publication) error {
	source := qualified(item.sourceSchema, item.sourceRelation)
	sidecar := qualified(item.sidecarSchema, item.sidecarTable)
	keys := make([]string, len(item.header.EntityKeyFields))
	for index, field := range item.header.EntityKeyFields {
		keys[index] = quoteIdentifier(field.Name)
	}
	projection := strings.Join(keys, ", ")
	query := `
SELECT (SELECT count(*) FROM ` + source + `),
       (SELECT count(*) FROM ` + sidecar + `),
       NOT EXISTS (
           (SELECT ` + projection + ` FROM ` + source + `
            EXCEPT
            SELECT ` + projection + ` FROM ` + sidecar + `)
           UNION ALL
           (SELECT ` + projection + ` FROM ` + sidecar + `
            EXCEPT
            SELECT ` + projection + ` FROM ` + source + `)
       )`
	var sourceRows, sidecarRows int64
	var equal bool
	if err := tx.QueryRow(ctx, query).Scan(&sourceRows, &sidecarRows, &equal); err != nil {
		return err
	}
	if sourceRows != int64(item.bundle.RowCount) || sidecarRows != sourceRows || !equal {
		return fmt.Errorf("reporting/sidecar entity sets differ: source=%d sidecar=%d", sourceRows, sidecarRows)
	}
	return nil
}

type sidecarCopySource struct {
	scanner      *bufio.Scanner
	expectedRows uint64
	rows         uint64
	values       []any
	err          error
	done         bool
}

func newSidecarCopySource(file *os.File, expectedRows uint64) *sidecarCopySource {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	return &sidecarCopySource{scanner: scanner, expectedRows: expectedRows}
}

func (s *sidecarCopySource) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	for s.scanner.Scan() {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(s.scanner.Bytes(), &envelope); err != nil {
			s.err = err
			return false
		}
		switch envelope.Type {
		case "header":
			if s.rows != 0 {
				s.err = errors.New("sidecar header is out of order")
				return false
			}
			continue
		case "row":
			var row snapshotbundle.SidecarRow
			if err := decodeJSONLine(s.scanner.Bytes(), &row); err != nil {
				s.err = err
				return false
			}
			s.rows++
			if row.RowHandle != s.rows || row.RowHandle > math.MaxInt64 {
				s.err = errors.New("sidecar row handles are not dense int64 values")
				return false
			}
			encoded, err := json.Marshal(row.KeyValues)
			if err != nil {
				s.err = err
				return false
			}
			s.values = []any{int64(row.RowHandle), row.EntityKey, string(encoded)}
			return true
		case "footer":
			var footer snapshotbundle.SidecarFooter
			if err := decodeJSONLine(s.scanner.Bytes(), &footer); err != nil ||
				footer.RowCount != s.rows || footer.RowCount != s.expectedRows {
				s.err = errors.New("sidecar footer row count differs")
				return false
			}
			s.done = true
			return false
		default:
			s.err = errors.New("sidecar contains an unknown record")
			return false
		}
	}
	if err := s.scanner.Err(); err != nil {
		s.err = err
	} else if !s.done {
		s.err = errors.New("sidecar ended before its footer")
	}
	return false
}

func (s *sidecarCopySource) Values() ([]any, error) { return s.values, nil }
func (s *sidecarCopySource) Err() error             { return s.err }

func decodeJSONLine(line []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("sidecar line has trailing JSON")
	}
	return nil
}

func splitRelation(value, requiredSchema string) (string, string, bool) {
	schema, relation, found := strings.Cut(value, ".")
	return schema, relation, found && schema == requiredSchema && identifier.MatchString(relation)
}

func qualified(schema, relation string) string {
	return pgx.Identifier{schema, relation}.Sanitize()
}

func quoteIdentifier(value string) string { return pgx.Identifier{value}.Sanitize() }

func safeSQLType(value string) bool {
	switch value {
	case "smallint", "integer", "bigint", "numeric", "real", "double precision", "boolean", "bytea",
		"date", "time without time zone", "timestamp with time zone", "timestamp without time zone",
		"jsonb", "uuid", "text", "character", "character varying":
		return true
	default:
		return false
	}
}

func sidecarCastExpression(position int, sqlType string) string {
	jsonText := fmt.Sprintf("(key_values::jsonb ->> %d)", position)
	switch sqlType {
	case "jsonb":
		return fmt.Sprintf("(key_values::jsonb -> %d)", position)
	case "bytea":
		return "pg_catalog.decode(pg_catalog.substring(" + jsonText + " from 8), 'base64')"
	default:
		return "(" + jsonText + ")::" + sqlType
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "snapshot-sidecar-install: %v\n", err)
	os.Exit(1)
}
