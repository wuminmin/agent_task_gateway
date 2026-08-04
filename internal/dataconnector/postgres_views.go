package dataconnector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const viewRegistryRevisionDomain = "TASKGATE-VIEW-REGISTRY-REVISION-V1\x00"

var viewRegistryIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

type discoveredPostgreSQLRelation struct {
	OID              uint32
	Name             viewcompiler.RelationName
	Kind             string
	Persistence      string
	Populated        bool
	Partition        bool
	RowSecurity      bool
	ForceRowSecurity bool
	OptionsCount     int
	Definition       string
}

type viewRegistryDiscoverer struct {
	querier      attestationQuerier
	baseProducts map[viewcompiler.RelationName]string
	relations    map[viewcompiler.RelationName]viewcompiler.Relation
	states       map[viewcompiler.RelationName]uint8
	edges        int
	bytes        int
}

// ViewRegistryExpectation is task-scoped semantic evidence supplied only on
// authorized QueryRequests. The connector rediscovers and compares the closure
// inside that request's transaction; unrelated view changes do not affect
// readiness or other tasks. ExpectedRevisionDigest is mandatory at execution.
type ViewRegistryExpectation struct {
	Roots                  []viewcompiler.RelationName `json:"roots"`
	BaseProducts           map[string]string           `json:"base_products"`
	ExpectedRevisionDigest string                      `json:"expected_revision_digest"`
}

// DiscoverViewRegistry resolves and snapshots a closed set of governed
// PostgreSQL views. Discovery and every pg_catalog read use one read-only,
// repeatable-read transaction. Ordinary views are recursively expanded;
// materialized views are opaque terminal publications and must be mapped to a
// Catalog product by baseProducts (keyed by canonical schema.name). The map is
// an authorized candidate set; entries outside the reachable closure are
// ignored and must not be copied into an execution expectation.
//
// OIDs are deliberately confined to this transaction. Registry identities and
// digests use stable schema-qualified names, exact definitions, ordered column
// contracts, and the PostgreSQL major version.
func (c *Connector) DiscoverViewRegistry(ctx context.Context, roots []viewcompiler.RelationName,
	baseProducts map[string]string) (snapshot viewcompiler.RegistrySnapshot, err error) {
	if c == nil || c.pool == nil {
		return snapshot, connectorError(CodeConnection, errors.New("connector is closed"))
	}
	tx, beginErr := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if beginErr != nil {
		return snapshot, connectorError(CodeConnection, beginErr)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if _, execErr := tx.Exec(ctx, SafetySessionPinSQL); execErr != nil {
		return snapshot, connectorError(CodeConnection, execErr)
	}
	if _, execErr := tx.Exec(ctx, StatementTimeoutPinSQL, timeoutSetting(c.statementTimeout)); execErr != nil {
		return snapshot, connectorError(CodeConnection, execErr)
	}
	snapshot, discoverErr := discoverViewRegistry(ctx, tx, roots, baseProducts)
	if discoverErr != nil {
		return viewcompiler.RegistrySnapshot{}, discoverErr
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return viewcompiler.RegistrySnapshot{}, connectorError(CodeConnection, commitErr)
	}
	committed = true
	return snapshot, nil
}

// verifyViewRegistry is deliberately expressed in terms of
// attestationQuerier. Query and QueryPairStream pass their own transaction, so
// the dependency check and subsequent SQL observe the same PostgreSQL snapshot.
func verifyViewRegistry(ctx context.Context, querier attestationQuerier,
	expectation *ViewRegistryExpectation) (viewcompiler.RegistrySnapshot, error) {
	if expectation == nil {
		return viewcompiler.RegistrySnapshot{}, nil
	}
	normalized, err := normalizeViewRegistryExpectation(*expectation, true)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(err)
	}
	snapshot, err := discoverViewRegistry(ctx, querier, normalized.Roots, normalized.BaseProducts)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, err
	}
	if snapshot.RevisionDigest != normalized.ExpectedRevisionDigest {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(errors.New("view registry revision mismatch"))
	}
	return snapshot, nil
}

func discoverViewRegistry(ctx context.Context, querier attestationQuerier,
	roots []viewcompiler.RelationName, baseProducts map[string]string) (viewcompiler.RegistrySnapshot, error) {
	if len(roots) == 0 {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(errors.New("at least one view root is required"))
	}
	if len(roots) > viewcompiler.MaxViewNodes {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(errors.New("view root count exceeds the registry node limit"))
	}
	canonicalBases, err := normalizeBaseProducts(baseProducts)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(err)
	}
	canonicalRoots, err := normalizeViewRoots(roots)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(err)
	}
	var serverVersion string
	if err := querier.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')`).Scan(&serverVersion); err != nil {
		return viewcompiler.RegistrySnapshot{}, connectorError(CodeConnection, err)
	}
	major, err := postgresMajorVersion(serverVersion)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(err)
	}
	discoverer := viewRegistryDiscoverer{
		querier: querier, baseProducts: canonicalBases,
		relations: make(map[viewcompiler.RelationName]viewcompiler.Relation),
		states:    make(map[viewcompiler.RelationName]uint8),
	}
	for _, root := range canonicalRoots {
		if err := discoverer.visit(ctx, root, 0); err != nil {
			return viewcompiler.RegistrySnapshot{}, err
		}
	}
	revision, err := viewRegistryRevision(major, discoverer.relations)
	if err != nil {
		return viewcompiler.RegistrySnapshot{}, viewSemanticError(err)
	}
	return viewcompiler.RegistrySnapshot{
		PostgreSQLMajorVersion: major,
		RevisionDigest:         revision,
		Relations:              discoverer.relations,
	}, nil
}

func normalizeViewRegistryExpectation(expectation ViewRegistryExpectation,
	requireDigest bool) (ViewRegistryExpectation, error) {
	if len(expectation.Roots) == 0 {
		return ViewRegistryExpectation{}, errors.New("view registry roots are required")
	}
	roots, err := normalizeViewRoots(expectation.Roots)
	if err != nil {
		return ViewRegistryExpectation{}, err
	}
	if _, err := normalizeBaseProducts(expectation.BaseProducts); err != nil {
		return ViewRegistryExpectation{}, err
	}
	if requireDigest && !isSHA256Hex(expectation.ExpectedRevisionDigest) {
		return ViewRegistryExpectation{}, errors.New("view registry revision digest must be lowercase SHA-256")
	}
	if !requireDigest && expectation.ExpectedRevisionDigest != "" && !isSHA256Hex(expectation.ExpectedRevisionDigest) {
		return ViewRegistryExpectation{}, errors.New("view registry revision digest must be lowercase SHA-256")
	}
	return ViewRegistryExpectation{
		Roots:                  roots,
		BaseProducts:           cloneStringMap(expectation.BaseProducts),
		ExpectedRevisionDigest: expectation.ExpectedRevisionDigest,
	}, nil
}

func matchingViewRegistryExpectations(left, right *ViewRegistryExpectation) (*ViewRegistryExpectation, error) {
	if left == nil && right == nil {
		return nil, nil
	}
	if left == nil || right == nil {
		return nil, viewSemanticError(errors.New("visible and provenance view registry expectations differ"))
	}
	leftNormalized, err := normalizeViewRegistryExpectation(*left, true)
	if err != nil {
		return nil, viewSemanticError(err)
	}
	rightNormalized, err := normalizeViewRegistryExpectation(*right, true)
	if err != nil {
		return nil, viewSemanticError(err)
	}
	if leftNormalized.ExpectedRevisionDigest != rightNormalized.ExpectedRevisionDigest ||
		!sameRelationNames(leftNormalized.Roots, rightNormalized.Roots) ||
		!sameStringMap(leftNormalized.BaseProducts, rightNormalized.BaseProducts) {
		return nil, viewSemanticError(errors.New("visible and provenance view registry expectations differ"))
	}
	return &leftNormalized, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sameRelationNames(left, right []viewcompiler.RelationName) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func normalizeViewRoots(roots []viewcompiler.RelationName) ([]viewcompiler.RelationName, error) {
	result := append([]viewcompiler.RelationName(nil), roots...)
	seen := make(map[viewcompiler.RelationName]struct{}, len(result))
	for _, root := range result {
		if err := validateViewRelationName(root); err != nil {
			return nil, fmt.Errorf("invalid view root: %w", err)
		}
		if _, duplicate := seen[root]; duplicate {
			return nil, errors.New("duplicate view root")
		}
		seen[root] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return relationNameLess(result[i], result[j]) })
	return result, nil
}

func normalizeBaseProducts(products map[string]string) (map[viewcompiler.RelationName]string, error) {
	result := make(map[viewcompiler.RelationName]string, len(products))
	for qualified, product := range products {
		schema, name, ok := strings.Cut(qualified, ".")
		relation := viewcompiler.RelationName{Schema: schema, Name: name}
		if !ok || strings.Contains(name, ".") || validateViewRelationName(relation) != nil || !viewRegistryIdentifier.MatchString(product) {
			return nil, errors.New("base product map contains an invalid relation or product")
		}
		if _, duplicate := result[relation]; duplicate {
			return nil, errors.New("base product relation is duplicated")
		}
		result[relation] = product
	}
	return result, nil
}

func validateViewRelationName(name viewcompiler.RelationName) error {
	if !viewRegistryIdentifier.MatchString(name.Schema) || !viewRegistryIdentifier.MatchString(name.Name) {
		return errors.New("view relation names must be unquoted lowercase identifiers")
	}
	if isSystemViewSchema(name.Schema) {
		return errors.New("system and temporary schemas are outside the governed view registry")
	}
	return nil
}

func isSystemViewSchema(schema string) bool {
	return schema == "information_schema" || strings.HasPrefix(schema, "pg_")
}

func relationNameLess(left, right viewcompiler.RelationName) bool {
	if left.Schema != right.Schema {
		return left.Schema < right.Schema
	}
	return left.Name < right.Name
}

func (d *viewRegistryDiscoverer) visit(ctx context.Context, name viewcompiler.RelationName, depth int) error {
	if depth > viewcompiler.MaxViewDepth {
		return viewSemanticError(errors.New("view dependency depth exceeds the configured limit"))
	}
	switch d.states[name] {
	case 1:
		return viewSemanticError(errors.New("view dependency cycle detected"))
	case 2:
		return nil
	}
	if len(d.relations) >= viewcompiler.MaxViewNodes {
		return viewSemanticError(errors.New("view dependency node count exceeds the configured limit"))
	}
	metadata, err := d.relationMetadata(ctx, name)
	if err != nil {
		return err
	}
	columns, err := d.relationColumns(ctx, metadata)
	if err != nil {
		return err
	}
	definitionBytes := len(metadata.Definition)
	if definitionBytes > viewcompiler.MaxDefinitionBytes-d.bytes {
		return viewSemanticError(errors.New("view definition bytes exceed the configured limit"))
	}
	d.bytes += definitionBytes

	relation := viewcompiler.Relation{
		Name:             name,
		DefinitionSQL:    metadata.Definition,
		DefinitionDigest: viewcompiler.ExactDefinitionDigest(metadata.Definition),
		Columns:          columns,
	}
	d.states[name] = 1
	switch metadata.Kind {
	case "m":
		product := d.baseProducts[name]
		if product == "" {
			return viewSemanticError(errors.New("materialized view leaf is not mapped to a governed base product"))
		}
		if !metadata.Populated {
			return viewSemanticError(errors.New("materialized view leaf is not populated"))
		}
		relation.Kind = viewcompiler.RelationBase
		relation.ProductName = product
	case "v":
		if _, incorrectlyMapped := d.baseProducts[name]; incorrectlyMapped {
			return viewSemanticError(errors.New("ordinary view cannot be used as an opaque base product"))
		}
		dependencies, dependencyErr := d.relationDependencies(ctx, metadata)
		if dependencyErr != nil {
			return dependencyErr
		}
		if len(dependencies) == 0 {
			return viewSemanticError(errors.New("ordinary view has no governed relation dependency"))
		}
		if len(dependencies) > viewcompiler.MaxDependencyEdges-d.edges {
			return viewSemanticError(errors.New("view dependency edge count exceeds the configured limit"))
		}
		d.edges += len(dependencies)
		relation.Kind = viewcompiler.RelationView
		relation.Dependencies = dependencies
		// Store the GRAY relation before recursion so a self-reference or a
		// longer back-edge is detected deterministically.
		d.relations[name] = relation
		for _, dependency := range dependencies {
			if visitErr := d.visit(ctx, dependency, depth+1); visitErr != nil {
				return visitErr
			}
		}
	default:
		return viewSemanticError(errors.New("raw, partitioned, foreign, sequence, and system relations are outside the view registry"))
	}
	d.relations[name] = relation
	d.states[name] = 2
	return nil
}

func (d *viewRegistryDiscoverer) relationMetadata(ctx context.Context, name viewcompiler.RelationName) (discoveredPostgreSQLRelation, error) {
	var result discoveredPostgreSQLRelation
	var oid int64
	result.Name = name
	err := d.querier.QueryRow(ctx, `
SELECT cls.oid::bigint,
       cls.relkind::text,
       cls.relpersistence::text,
       cls.relispopulated,
       cls.relispartition,
       cls.relrowsecurity,
       cls.relforcerowsecurity,
       COALESCE(pg_catalog.cardinality(cls.reloptions), 0),
       CASE WHEN cls.relkind IN ('v', 'm')
            THEN pg_catalog.pg_get_viewdef(cls.oid, false)
            ELSE ''
       END
FROM pg_catalog.pg_class AS cls
JOIN pg_catalog.pg_namespace AS ns ON ns.oid = cls.relnamespace
WHERE ns.nspname = $1 AND cls.relname = $2`, name.Schema, name.Name).Scan(
		&oid, &result.Kind, &result.Persistence, &result.Populated,
		&result.Partition, &result.RowSecurity, &result.ForceRowSecurity,
		&result.OptionsCount, &result.Definition,
	)
	if err != nil {
		return result, viewSemanticError(err)
	}
	if oid <= 0 || oid > int64(^uint32(0)) {
		return result, viewSemanticError(errors.New("view relation OID is invalid"))
	}
	result.OID = uint32(oid)
	if validateErr := validateViewRelationName(name); validateErr != nil {
		return result, viewSemanticError(validateErr)
	}
	if result.Persistence != "p" || result.Partition || result.RowSecurity || result.ForceRowSecurity || result.OptionsCount != 0 {
		return result, viewSemanticError(errors.New("view relation uses unsupported persistence, partition, row-security, or relation options"))
	}
	if result.Kind != "v" && result.Kind != "m" {
		return result, viewSemanticError(errors.New("view dependency resolved to a non-view relation"))
	}
	if strings.TrimSpace(result.Definition) == "" {
		return result, viewSemanticError(errors.New("view definition is unavailable"))
	}
	return result, nil
}

func (d *viewRegistryDiscoverer) relationColumns(ctx context.Context, relation discoveredPostgreSQLRelation) ([]viewcompiler.Column, error) {
	rows, err := d.querier.Query(ctx, `
SELECT attr.attname,
       CASE
           WHEN typ.typtype = 'd' THEN
               CASE
                   WHEN typ_ns.nspname <> 'pg_catalog' THEN 'USER-DEFINED'
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
         AND (coll.oid IS NULL OR coll_ns.nspname = 'pg_catalog')
FROM pg_catalog.pg_attribute AS attr
JOIN pg_catalog.pg_type AS typ ON typ.oid = attr.atttypid
JOIN pg_catalog.pg_namespace AS typ_ns ON typ_ns.oid = typ.typnamespace
LEFT JOIN pg_catalog.pg_type AS base_typ ON typ.typtype = 'd' AND base_typ.oid = typ.typbasetype
LEFT JOIN pg_catalog.pg_namespace AS base_typ_ns ON base_typ_ns.oid = base_typ.typnamespace
LEFT JOIN pg_catalog.pg_collation AS coll ON coll.oid = attr.attcollation
LEFT JOIN pg_catalog.pg_namespace AS coll_ns ON coll_ns.oid = coll.collnamespace
JOIN pg_catalog.pg_database AS db ON db.datname = pg_catalog.current_database()
WHERE attr.attrelid = $1::oid AND attr.attnum > 0 AND NOT attr.attisdropped
ORDER BY attr.attnum`, relation.OID)
	if err != nil {
		return nil, viewSemanticError(err)
	}
	defer rows.Close()
	var result []viewcompiler.Column
	for rows.Next() {
		var column viewcompiler.Column
		var deterministic bool
		if scanErr := rows.Scan(&column.Name, &column.SQLType, &column.Collation, &column.CollationVersion, &deterministic); scanErr != nil {
			return nil, viewSemanticError(scanErr)
		}
		if !viewRegistryIdentifier.MatchString(column.Name) || strings.TrimSpace(column.SQLType) == "" ||
			column.SQLType == "ARRAY" || column.SQLType == "USER-DEFINED" || !deterministic {
			return nil, viewSemanticError(errors.New("view column contract is unsupported"))
		}
		result = append(result, column)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, viewSemanticError(rowsErr)
	}
	if len(result) == 0 {
		return nil, viewSemanticError(errors.New("view relation has no columns"))
	}
	return result, nil
}

func (d *viewRegistryDiscoverer) relationDependencies(ctx context.Context,
	relation discoveredPostgreSQLRelation) ([]viewcompiler.RelationName, error) {
	if err := d.validateNonRelationDependencies(ctx, relation); err != nil {
		return nil, err
	}
	inspection, err := viewcompiler.InspectDefinition(relation.Name, relation.Definition)
	if err != nil {
		return nil, viewSemanticError(err)
	}
	// PostgreSQL intentionally omits a recursive CTE's self-reference from
	// pg_depend. Make it an explicit graph edge so the ordinary DFS cycle guard
	// remains the single fail-closed cycle decision.
	if inspection.RecursiveSelf {
		return []viewcompiler.RelationName{relation.Name}, nil
	}
	rows, err := d.querier.Query(ctx, `
SELECT dep.refobjid::bigint,
       ns.nspname,
       cls.relname,
       cls.relkind::text,
       dep.refobjsubid,
       dep.deptype::text,
       COALESCE(attr.attname, '')
FROM pg_catalog.pg_rewrite AS rewrite
JOIN pg_catalog.pg_depend AS dep
  ON dep.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
 AND dep.objid = rewrite.oid
 AND dep.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
JOIN pg_catalog.pg_class AS cls ON cls.oid = dep.refobjid
JOIN pg_catalog.pg_namespace AS ns ON ns.oid = cls.relnamespace
LEFT JOIN pg_catalog.pg_attribute AS attr
  ON attr.attrelid = dep.refobjid
 AND attr.attnum = dep.refobjsubid
 AND NOT attr.attisdropped
WHERE rewrite.ev_class = $1::oid
  AND rewrite.rulename = '_RETURN'
  AND NOT (dep.refobjid = rewrite.ev_class AND dep.deptype = 'i')
ORDER BY ns.nspname, cls.relname, cls.relkind, dep.refobjsubid`, relation.OID)
	if err != nil {
		return nil, viewSemanticError(err)
	}
	defer rows.Close()
	dependencies := make(map[viewcompiler.RelationName]struct{})
	for rows.Next() {
		var oid int64
		var name viewcompiler.RelationName
		var kind, dependencyType, referencedColumn string
		var columnNumber int32
		if scanErr := rows.Scan(&oid, &name.Schema, &name.Name, &kind, &columnNumber, &dependencyType, &referencedColumn); scanErr != nil {
			return nil, viewSemanticError(scanErr)
		}
		if oid <= 0 || oid > int64(^uint32(0)) {
			return nil, viewSemanticError(errors.New("view dependency OID is invalid"))
		}
		if dependencyType != "n" || validateViewRelationName(name) != nil {
			return nil, viewSemanticError(errors.New("view has an unsupported relation dependency"))
		}
		if kind != "v" && kind != "m" {
			return nil, viewSemanticError(errors.New("view depends on a raw, partitioned, foreign, sequence, or system relation"))
		}
		if columnNumber > 0 && referencedColumn == "" {
			return nil, viewSemanticError(errors.New("view dependency references a missing or dropped column"))
		}
		dependencies[name] = struct{}{}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, viewSemanticError(rowsErr)
	}
	result := make([]viewcompiler.RelationName, 0, len(dependencies))
	for dependency := range dependencies {
		result = append(result, dependency)
	}
	sort.Slice(result, func(i, j int) bool { return relationNameLess(result[i], result[j]) })
	if !sameRelationNames(result, inspection.References) {
		return nil, viewSemanticError(errors.New("parsed and catalog view dependencies differ"))
	}
	return result, nil
}

func (d *viewRegistryDiscoverer) validateNonRelationDependencies(ctx context.Context,
	relation discoveredPostgreSQLRelation) error {
	var unsafe int
	err := d.querier.QueryRow(ctx, `
SELECT pg_catalog.count(*)
FROM pg_catalog.pg_rewrite AS rewrite
JOIN pg_catalog.pg_depend AS dep
  ON dep.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
 AND dep.objid = rewrite.oid
WHERE rewrite.ev_class = $1::oid
  AND rewrite.rulename = '_RETURN'
  AND NOT (
      dep.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
      AND dep.refobjid = rewrite.ev_class
      AND dep.deptype = 'i'
  )
  AND NOT (
      dep.deptype = 'n'
      AND (
          dep.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
          OR (
              dep.refclassid = 'pg_catalog.pg_proc'::pg_catalog.regclass
              AND EXISTS (
                  SELECT 1
                  FROM pg_catalog.pg_proc AS proc
                  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = proc.pronamespace
                  WHERE proc.oid = dep.refobjid
                    AND ns.nspname = 'pg_catalog'
                    AND proc.provolatile = 'i'
                    AND proc.prokind IN ('f', 'a')
              )
          )
          OR (
              dep.refclassid = 'pg_catalog.pg_operator'::pg_catalog.regclass
              AND EXISTS (
                  SELECT 1
                  FROM pg_catalog.pg_operator AS operator
                  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = operator.oprnamespace
                  JOIN pg_catalog.pg_proc AS implementation ON implementation.oid = operator.oprcode
                  WHERE operator.oid = dep.refobjid
                    AND ns.nspname = 'pg_catalog'
                    AND implementation.provolatile = 'i'
              )
          )
          OR (
              dep.refclassid = 'pg_catalog.pg_type'::pg_catalog.regclass
              AND EXISTS (
                  SELECT 1
                  FROM pg_catalog.pg_type AS typ
                  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = typ.typnamespace
                  WHERE typ.oid = dep.refobjid AND ns.nspname = 'pg_catalog'
              )
          )
          OR (
              dep.refclassid = 'pg_catalog.pg_collation'::pg_catalog.regclass
              AND EXISTS (
                  SELECT 1
                  FROM pg_catalog.pg_collation AS coll
                  JOIN pg_catalog.pg_namespace AS ns ON ns.oid = coll.collnamespace
                  WHERE coll.oid = dep.refobjid
                    AND ns.nspname = 'pg_catalog'
                    AND coll.collisdeterministic
              )
          )
      )
  )`, relation.OID).Scan(&unsafe)
	if err != nil {
		return viewSemanticError(err)
	}
	if unsafe != 0 {
		return viewSemanticError(errors.New("view has a volatile, user-defined, or otherwise unsupported object dependency"))
	}
	return nil
}

func viewRegistryRevision(postgreSQLMajor int,
	relations map[viewcompiler.RelationName]viewcompiler.Relation) (string, error) {
	if postgreSQLMajor <= 0 || len(relations) == 0 {
		return "", errors.New("view registry revision requires a PostgreSQL version and relations")
	}
	names := make([]viewcompiler.RelationName, 0, len(relations))
	for name := range relations {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return relationNameLess(names[i], names[j]) })
	hash := sha256.New()
	_, _ = hash.Write([]byte(viewRegistryRevisionDomain))
	writeViewRegistryHashField(hash, fmt.Sprint(postgreSQLMajor))
	writeViewRegistryHashField(hash, fmt.Sprint(len(names)))
	for _, name := range names {
		relation := relations[name]
		if relation.Name != name || relation.Kind == "" || len(relation.Columns) == 0 {
			return "", errors.New("invalid view registry relation")
		}
		writeViewRegistryHashField(hash, name.Schema)
		writeViewRegistryHashField(hash, name.Name)
		writeViewRegistryHashField(hash, string(relation.Kind))
		writeViewRegistryHashField(hash, relation.ProductName)
		writeViewRegistryHashField(hash, relation.DefinitionDigest)
		writeViewRegistryHashField(hash, fmt.Sprint(len(relation.Columns)))
		for _, column := range relation.Columns {
			writeViewRegistryHashField(hash, column.Name)
			writeViewRegistryHashField(hash, strings.ToLower(strings.TrimSpace(column.SQLType)))
			writeViewRegistryHashField(hash, column.Collation)
			writeViewRegistryHashField(hash, column.CollationVersion)
		}
		dependencies := append([]viewcompiler.RelationName(nil), relation.Dependencies...)
		sort.Slice(dependencies, func(i, j int) bool { return relationNameLess(dependencies[i], dependencies[j]) })
		writeViewRegistryHashField(hash, fmt.Sprint(len(dependencies)))
		for _, dependency := range dependencies {
			writeViewRegistryHashField(hash, dependency.Schema)
			writeViewRegistryHashField(hash, dependency.Name)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type viewRegistryHashWriter interface {
	Write([]byte) (int, error)
}

func writeViewRegistryHashField(hash viewRegistryHashWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(value))
}

func viewSemanticError(cause error) error {
	return connectorError(CodeViewSemanticChanged, cause)
}
