package dataconnector

// Attestation statements, as source-controlled constants.
//
// These are the statements that make a governed read attributable: the
// datasource identity, and per ExpectedSchema entry the column attestation and
// the view-definition attestation. They are exported for the same reason as the
// session pins -- the observer's classifier manifest must be generated from the
// exact bytes the Connector executes, never from a retyped copy that could
// drift away from production without any test noticing.
//
// The view-definition attestation is also the statement that provokes the
// nested pg_rewrite lookup PostgreSQL performs inside pg_get_viewdef, which is
// counted separately under pg_stat_statements.track=all.
const (
	// DatasourceIdentitySQL reads the pinned datasource identity.
	DatasourceIdentitySQL = `
SELECT COALESCE((SELECT datasource_id FROM reporting.datasource_attestation WHERE singleton = TRUE), ''),
       current_database(), current_user, current_setting('server_version_num')`

	// ViewColumnAttestationSQL reads one reporting view's ordered column
	// projection, once per ExpectedSchema entry.
	ViewColumnAttestationSQL = `
SELECT attr.attname,
       CASE
           WHEN typ.typtype = 'd' THEN
               CASE
                   WHEN base_typ.typelem <> 0 AND base_typ.typlen = -1 THEN 'ARRAY'
                   WHEN base_typ_ns.nspname = 'pg_catalog' THEN format_type(typ.typbasetype, NULL)
                   ELSE 'USER-DEFINED'
               END
           ELSE
               CASE
                   WHEN typ.typelem <> 0 AND typ.typlen = -1 THEN 'ARRAY'
                   WHEN typ_ns.nspname = 'pg_catalog' THEN format_type(attr.atttypid, NULL)
                   ELSE 'USER-DEFINED'
               END
       END,
       CASE WHEN coll.oid IS NULL THEN '' WHEN coll.collname = 'default' THEN db.datcollate ELSE coll.collname END,
       COALESCE(CASE WHEN coll.oid IS NULL THEN '' WHEN coll.collname = 'default' THEN db.datcollversion ELSE pg_collation_actual_version(coll.oid) END, ''),
       COALESCE(coll.collisdeterministic, TRUE)
FROM pg_namespace AS ns
JOIN pg_class AS cls ON cls.relnamespace = ns.oid
JOIN pg_attribute AS attr ON attr.attrelid = cls.oid AND attr.attnum > 0 AND NOT attr.attisdropped
JOIN pg_type AS typ ON typ.oid = attr.atttypid
JOIN pg_namespace AS typ_ns ON typ_ns.oid = typ.typnamespace
LEFT JOIN pg_type AS base_typ ON typ.typtype = 'd' AND base_typ.oid = typ.typbasetype
LEFT JOIN pg_namespace AS base_typ_ns ON base_typ_ns.oid = base_typ.typnamespace
LEFT JOIN pg_collation AS coll ON coll.oid = attr.attcollation
JOIN pg_database AS db ON db.datname = current_database()
WHERE ns.nspname=$1 AND cls.relname=$2
  AND cls.relkind IN ('r', 'v', 'm', 'f', 'p')
  AND (pg_has_role(cls.relowner, 'USAGE') OR has_column_privilege(cls.oid, attr.attnum, 'SELECT, INSERT, UPDATE, REFERENCES'))
ORDER BY attr.attnum`

	// ViewDefinitionAttestationSQL reads one reporting view's definition, once
	// per ExpectedSchema entry.
	ViewDefinitionAttestationSQL = `
WITH taskgate_schema_digest_path AS (
	SELECT set_config('search_path', 'pg_catalog', true)
)
SELECT pg_get_viewdef(format('%I.%I', $1::text, $2::text)::regclass, true)
FROM taskgate_schema_digest_path`
)
