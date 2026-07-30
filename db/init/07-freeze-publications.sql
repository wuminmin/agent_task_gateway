BEGIN;

-- Publication is all-or-nothing: do not expose a Catalog snapshot unless the
-- materialized reporting relations and their ordinal companions describe the
-- same entity-key sets and the manifest-declared row counts are exact.
DO $taskgate_publication_check$
DECLARE
    detail_rows bigint;
    summary_rows bigint;
BEGIN
    SELECT row_count INTO detail_rows
    FROM taskgate_ordinal.publications
    WHERE publication_name = 'expense-detail-v1';

    SELECT row_count INTO summary_rows
    FROM taskgate_ordinal.publications
    WHERE publication_name = 'expense-summary-v1';

    IF detail_rows IS NULL OR summary_rows IS NULL THEN
        RAISE EXCEPTION 'a required TaskGate snapshot publication is missing';
    END IF;

    IF (SELECT count(*) FROM reporting.expense_detail) <> detail_rows OR
       (SELECT count(*) FROM taskgate_ordinal.expense_detail_v1) <> detail_rows THEN
        RAISE EXCEPTION 'expense-detail-v1 row count does not match its publication';
    END IF;

    IF EXISTS (
        SELECT receipt_no FROM reporting.expense_detail
        EXCEPT
        SELECT receipt_no FROM taskgate_ordinal.expense_detail_v1
    ) OR EXISTS (
        SELECT receipt_no FROM taskgate_ordinal.expense_detail_v1
        EXCEPT
        SELECT receipt_no FROM reporting.expense_detail
    ) THEN
        RAISE EXCEPTION 'expense-detail-v1 entity keys do not match its ordinal sidecar';
    END IF;

    IF EXISTS (
        SELECT handle FROM generate_series(1::bigint, detail_rows) AS expected(handle)
        EXCEPT
        SELECT row_handle FROM taskgate_ordinal.expense_detail_v1
    ) THEN
        RAISE EXCEPTION 'expense-detail-v1 row handles are not contiguous';
    END IF;

    IF (SELECT count(*) FROM reporting.expense_summary) <> summary_rows OR
       (SELECT count(*) FROM taskgate_ordinal.expense_summary_v1) <> summary_rows THEN
        RAISE EXCEPTION 'expense-summary-v1 row count does not match its publication';
    END IF;

    IF EXISTS (
        SELECT month, department, expense_type FROM reporting.expense_summary
        EXCEPT
        SELECT month, department, expense_type FROM taskgate_ordinal.expense_summary_v1
    ) OR EXISTS (
        SELECT month, department, expense_type FROM taskgate_ordinal.expense_summary_v1
        EXCEPT
        SELECT month, department, expense_type FROM reporting.expense_summary
    ) THEN
        RAISE EXCEPTION 'expense-summary-v1 entity keys do not match its ordinal sidecar';
    END IF;

    IF EXISTS (
        SELECT handle FROM generate_series(1::bigint, summary_rows) AS expected(handle)
        EXCEPT
        SELECT row_handle FROM taskgate_ordinal.expense_summary_v1
    ) THEN
        RAISE EXCEPTION 'expense-summary-v1 row handles are not contiguous';
    END IF;
END
$taskgate_publication_check$;

-- A NOLOGIN owner makes REFRESH/DDL unavailable to every application role.
-- PostgreSQL superusers remain an infrastructure trust boundary, as they do
-- for every database-enforced control.
CREATE ROLE taskgate_snapshot_owner
    NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

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

CREATE TRIGGER reject_frozen_employees_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON legacy.employees
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_expenses_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON legacy.expenses
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_attestation_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON reporting.datasource_attestation
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_publications_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON taskgate_ordinal.publications
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_detail_sidecar_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON taskgate_ordinal.expense_detail_v1
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_summary_sidecar_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON taskgate_ordinal.expense_summary_v1
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

COMMENT ON MATERIALIZED VIEW reporting.expense_detail IS
    'Immutable TaskGate publication expense-detail-v1; refresh requires an explicit new Catalog publication.';
COMMENT ON MATERIALIZED VIEW reporting.expense_summary IS
    'Immutable TaskGate publication expense-summary-v1; refresh requires an explicit new Catalog publication.';
COMMENT ON ROLE taskgate_snapshot_owner IS
    'NOLOGIN owner of the immutable TaskGate business snapshot and ordinal sidecars.';

-- Ownership, not a mutable session setting, enforces the publication boundary.
-- The gateway role is created later and receives only explicitly enumerated
-- SELECT grants.
ALTER TABLE legacy.employees OWNER TO taskgate_snapshot_owner;
ALTER TABLE legacy.expenses OWNER TO taskgate_snapshot_owner;
ALTER TABLE reporting.datasource_attestation OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.expense_summary OWNER TO taskgate_snapshot_owner;
ALTER TABLE taskgate_ordinal.publications OWNER TO taskgate_snapshot_owner;
ALTER TABLE taskgate_ordinal.expense_detail_v1 OWNER TO taskgate_snapshot_owner;
ALTER TABLE taskgate_ordinal.expense_summary_v1 OWNER TO taskgate_snapshot_owner;
ALTER FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA legacy OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA reporting OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA taskgate_ordinal OWNER TO taskgate_snapshot_owner;

COMMIT;
