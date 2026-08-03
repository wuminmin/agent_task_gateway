BEGIN;

-- Assert the source-side frozen corpus before any live snapshot compiler is
-- allowed to scan it. The offline sidecar installer independently proves the
-- row count and entity-key equality for all five compiler bundles.
DO $taskgate_publication_check$
BEGIN
    IF (SELECT count(*) FROM reporting.expense_detail) <> 10 THEN
        RAISE EXCEPTION 'expense-detail-v1 source row count is not frozen at 10';
    END IF;

    IF (SELECT count(*) FROM reporting.final_v5_attack_expense_detail) <> 10 OR
       EXISTS (
           SELECT 1
           FROM reporting.final_v5_attack_expense_detail
           WHERE receipt_no IS NULL OR department IS NULL OR amount IS NULL
       ) OR
       EXISTS (
           SELECT receipt_no, department, amount FROM reporting.final_v5_attack_expense_detail
           EXCEPT
           SELECT receipt_no, department, amount FROM reporting.expense_detail
       ) OR EXISTS (
           SELECT receipt_no, department, amount FROM reporting.expense_detail
           EXCEPT
           SELECT receipt_no, department, amount FROM reporting.final_v5_attack_expense_detail
       ) THEN
        RAISE EXCEPTION 'Final-V5 attack projection does not match expense-detail-v1';
    END IF;

    IF (SELECT count(*) FROM reporting.final_v5_concurrency_expense_detail) <> 10 OR
       EXISTS (
           SELECT 1
           FROM reporting.final_v5_concurrency_expense_detail
           WHERE receipt_no IS NULL OR department IS NULL OR expense_type IS NULL OR city IS NULL
       ) OR
       EXISTS (
           SELECT receipt_no, department, expense_type, city
           FROM reporting.final_v5_concurrency_expense_detail
           EXCEPT
           SELECT receipt_no, department, expense_type, city FROM reporting.expense_detail
       ) OR EXISTS (
           SELECT receipt_no, department, expense_type, city FROM reporting.expense_detail
           EXCEPT
           SELECT receipt_no, department, expense_type, city
           FROM reporting.final_v5_concurrency_expense_detail
       ) THEN
        RAISE EXCEPTION 'Final-V5 concurrency projection does not match expense-detail-v1';
    END IF;

    IF (SELECT count(*) FROM reporting.expense_summary) <> 10 THEN
        RAISE EXCEPTION 'expense-summary-v1 source row count is not frozen at 10';
    END IF;

    IF (SELECT count(*) FROM reporting.provsql_orders) <> 50000 OR
       (SELECT count(*) FROM reporting.provsql_lineitem) <> 250000 OR
       (SELECT count(*) FROM reporting.provsql_nonce) <> 1000 THEN
        RAISE EXCEPTION 'Final-V5 ProvSQL source row counts differ from the frozen corpus';
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

CREATE TRIGGER reject_frozen_provsql_orders_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_provsql.orders
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_provsql_lineitem_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_provsql.lineitem
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_provsql_nonce_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_provsql.nonce
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_attestation_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON reporting.datasource_attestation
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

COMMENT ON MATERIALIZED VIEW reporting.expense_detail IS
    'Immutable TaskGate publication expense-detail-v1; refresh requires an explicit new Catalog publication.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_attack_expense_detail IS
    'Immutable physical projection of expense-detail-v1 for the frozen Final-V5 A--D corpus.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_concurrency_expense_detail IS
    'Immutable physical projection of expense-detail-v1 for the frozen Final-V5 same-task/same-root matrix.';
COMMENT ON MATERIALIZED VIEW reporting.expense_summary IS
    'Immutable TaskGate publication expense-summary-v1; refresh requires an explicit new Catalog publication.';
COMMENT ON MATERIALIZED VIEW reporting.provsql_orders IS
    'Immutable Final-V5 ProvSQL orders publication; refresh requires a new Catalog publication.';
COMMENT ON MATERIALIZED VIEW reporting.provsql_lineitem IS
    'Immutable Final-V5 ProvSQL lineitem publication; refresh requires a new Catalog publication.';
COMMENT ON MATERIALIZED VIEW reporting.provsql_nonce IS
    'Immutable Final-V5 ProvSQL nonce publication; refresh requires a new Catalog publication.';
COMMENT ON ROLE taskgate_snapshot_owner IS
    'NOLOGIN owner of the immutable TaskGate business snapshot and ordinal sidecars.';

-- Ownership, not a mutable session setting, enforces the publication boundary.
-- The gateway role is created later and receives only explicitly enumerated
-- SELECT grants.
ALTER TABLE legacy.employees OWNER TO taskgate_snapshot_owner;
ALTER TABLE legacy.expenses OWNER TO taskgate_snapshot_owner;
ALTER TABLE reporting.datasource_attestation OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_attack_expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_concurrency_expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.expense_summary OWNER TO taskgate_snapshot_owner;
ALTER TABLE final_v5_provsql.orders OWNER TO taskgate_snapshot_owner;
ALTER TABLE final_v5_provsql.lineitem OWNER TO taskgate_snapshot_owner;
ALTER TABLE final_v5_provsql.nonce OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.provsql_orders OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.provsql_lineitem OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.provsql_nonce OWNER TO taskgate_snapshot_owner;
ALTER FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA legacy OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA final_v5_provsql OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA reporting OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA taskgate_ordinal OWNER TO taskgate_snapshot_owner;

COMMIT;
