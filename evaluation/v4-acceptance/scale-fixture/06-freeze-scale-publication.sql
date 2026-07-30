BEGIN;

-- Reuse the deterministic Orders/Lineitem seed created by
-- evaluation/exposure-scale/05-scale-data.sql, but publish only the 45,000
-- order source used by the prior scale campaign.  The measured query still
-- filters at 45,000 orders, preserving the old 33.9 ms baseline's data and
-- planner-statistics context.  These are populated materialized views rather
-- than mutable projections; the ordinal compiler refuses weaker relations.
DROP VIEW reporting.scale_lineitem;
DROP VIEW reporting.scale_orders;

CREATE MATERIALIZED VIEW reporting.scale_orders AS
SELECT dataset_partition, o_orderkey, o_orderstatus
FROM legacy.scale_orders
WITH DATA;

CREATE MATERIALIZED VIEW reporting.scale_lineitem AS
SELECT dataset_partition, l_orderkey, l_linenumber, l_extendedprice
FROM legacy.scale_lineitem
WITH DATA;

CREATE UNIQUE INDEX scale_orders_publication_key
    ON reporting.scale_orders (o_orderkey);
CREATE UNIQUE INDEX scale_lineitem_publication_key
    ON reporting.scale_lineitem (l_orderkey, l_linenumber);
CREATE INDEX scale_lineitem_publication_join_key
    ON reporting.scale_lineitem (l_orderkey);

ANALYZE reporting.scale_orders;
ANALYZE reporting.scale_lineitem;

-- Ownership is the publication boundary.  Neither the Gateway nor the
-- offline snapshot scanner can SET ROLE to this principal or refresh the
-- materialized views.  PostgreSQL superusers remain an explicit
-- infrastructure trust boundary.
CREATE ROLE taskgate_snapshot_owner
    NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

CREATE SCHEMA taskgate_ordinal AUTHORIZATION taskgate_snapshot_owner;

CREATE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $taskgate_reject_mutation$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = format(
            'TaskGate frozen publication relation %I.%I rejects %s',
            TG_TABLE_SCHEMA,
            TG_TABLE_NAME,
            TG_OP
        );
END
$taskgate_reject_mutation$;

REVOKE ALL ON FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() FROM PUBLIC;

CREATE TRIGGER reject_frozen_scale_orders_seed_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON legacy.scale_orders
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_scale_lineitem_seed_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON legacy.scale_lineitem
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_scale_attestation_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON reporting.datasource_attestation
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

COMMENT ON MATERIALIZED VIEW reporting.scale_orders IS
    'Immutable TaskGate V4 narrow publication exposure-scale-2026-v4-narrow-1 (50,000 orders).';
COMMENT ON MATERIALIZED VIEW reporting.scale_lineitem IS
    'Immutable TaskGate V4 narrow publication exposure-scale-2026-v4-narrow-1 (250,000 lineitems).';
COMMENT ON ROLE taskgate_snapshot_owner IS
    'NOLOGIN owner of immutable TaskGate business snapshots and ordinal sidecars.';

REVOKE ALL ON SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON SCHEMA taskgate_ordinal FROM PUBLIC;

ALTER TABLE legacy.scale_orders OWNER TO taskgate_snapshot_owner;
ALTER TABLE legacy.scale_lineitem OWNER TO taskgate_snapshot_owner;
ALTER TABLE reporting.datasource_attestation OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.scale_orders OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.scale_lineitem OWNER TO taskgate_snapshot_owner;
ALTER FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA legacy OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA reporting OWNER TO taskgate_snapshot_owner;

COMMIT;
