BEGIN;

-- The Final-V5 RLS baseline is a real base table governed by PostgreSQL row
-- security.  It contains the same frozen ten-row snapshot as the two TaskGate
-- publications; it is not a reporting-view predicate masquerading as RLS.
CREATE SCHEMA final_v5_rls;

CREATE ROLE final_v5_rls_reader
  NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

CREATE TABLE final_v5_rls.expense_detail (
    receipt_no text PRIMARY KEY,
    employee_name text NOT NULL,
    department text NOT NULL,
    amount numeric(12,2) NOT NULL CHECK (amount >= 0)
);

INSERT INTO final_v5_rls.expense_detail(receipt_no, employee_name, department, amount)
SELECT receipt_no, employee_name, department, amount
FROM reporting.expense_detail
ORDER BY receipt_no;

ALTER TABLE final_v5_rls.expense_detail ENABLE ROW LEVEL SECURITY;
ALTER TABLE final_v5_rls.expense_detail FORCE ROW LEVEL SECURITY;
CREATE POLICY final_v5_sales_scope
ON final_v5_rls.expense_detail
AS PERMISSIVE
FOR SELECT
TO final_v5_rls_reader
USING (department = '销售部');

-- These are physically independent immutable TaskGate publications.  The
-- Catalog injects the same sales-department scope used by the RLS policy.
CREATE MATERIALIZED VIEW reporting.final_v5_rls_unlimited_expense_detail AS
SELECT receipt_no, department, amount
FROM reporting.expense_detail;

CREATE MATERIALIZED VIEW reporting.final_v5_rls_bounded_expense_detail AS
SELECT receipt_no, department, amount
FROM reporting.expense_detail;

DO $taskgate_rls_fixture_check$
BEGIN
    IF (SELECT count(*) FROM final_v5_rls.expense_detail) <> 10 OR
       (SELECT count(*) FROM final_v5_rls.expense_detail WHERE department = '销售部') <> 6 OR
       EXISTS (
           SELECT receipt_no, employee_name, department, amount
           FROM final_v5_rls.expense_detail
           EXCEPT
           SELECT receipt_no, employee_name, department, amount
           FROM reporting.expense_detail
       ) OR EXISTS (
           SELECT receipt_no, employee_name, department, amount
           FROM reporting.expense_detail
           EXCEPT
           SELECT receipt_no, employee_name, department, amount
           FROM final_v5_rls.expense_detail
       ) THEN
        RAISE EXCEPTION 'Final-V5 RLS base table differs from expense-detail-v1';
    END IF;

    IF EXISTS (
        SELECT receipt_no, department, amount FROM reporting.final_v5_rls_unlimited_expense_detail
        EXCEPT
        SELECT receipt_no, department, amount FROM reporting.expense_detail
    ) OR EXISTS (
        SELECT receipt_no, department, amount FROM reporting.expense_detail
        EXCEPT
        SELECT receipt_no, department, amount FROM reporting.final_v5_rls_unlimited_expense_detail
    ) OR EXISTS (
        SELECT receipt_no, department, amount FROM reporting.final_v5_rls_bounded_expense_detail
        EXCEPT
        SELECT receipt_no, department, amount FROM reporting.expense_detail
    ) OR EXISTS (
        SELECT receipt_no, department, amount FROM reporting.expense_detail
        EXCEPT
        SELECT receipt_no, department, amount FROM reporting.final_v5_rls_bounded_expense_detail
    ) THEN
        RAISE EXCEPTION 'Final-V5 RLS TaskGate projections differ from expense-detail-v1';
    END IF;
END
$taskgate_rls_fixture_check$;

REVOKE ALL ON SCHEMA final_v5_rls FROM PUBLIC;
REVOKE ALL ON TABLE final_v5_rls.expense_detail FROM PUBLIC;
REVOKE ALL ON TABLE reporting.final_v5_rls_unlimited_expense_detail FROM PUBLIC;
REVOKE ALL ON TABLE reporting.final_v5_rls_bounded_expense_detail FROM PUBLIC;
GRANT USAGE ON SCHEMA final_v5_rls TO final_v5_rls_reader;
GRANT SELECT (receipt_no, amount) ON final_v5_rls.expense_detail TO final_v5_rls_reader;

CREATE TRIGGER reject_frozen_rls_expense_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_rls.expense_detail
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

COMMENT ON ROLE final_v5_rls_reader IS
    'NOLOGIN non-owner Final-V5 PostgreSQL RLS subject; no BYPASSRLS or memberships.';
COMMENT ON TABLE final_v5_rls.expense_detail IS
    'Immutable ten-row Final-V5 base table with FORCE ROW LEVEL SECURITY.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_rls_unlimited_expense_detail IS
    'Independent immutable TaskGate validity-control publication for the Final-V5 RLS corpus.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_rls_bounded_expense_detail IS
    'Independent immutable TaskGate 70-percent-budget publication for the Final-V5 RLS corpus.';

ALTER TABLE final_v5_rls.expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_rls_unlimited_expense_detail OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_rls_bounded_expense_detail OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA final_v5_rls OWNER TO taskgate_snapshot_owner;

COMMIT;
