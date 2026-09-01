-- Final-V5 comparator-arms publication (docs/p8 a): one physically
-- independent immutable projection of the frozen ten-row expense fixture for
-- the counter experiment's four budget arms. The 22 prefix is load bearing:
-- reporting.expense_detail exists after 00-schema.sql and gateway_reader
-- after 10-reader.sh.
BEGIN;

CREATE MATERIALIZED VIEW reporting.final_v5_counter_expense_detail AS
SELECT receipt_no, department, amount
FROM reporting.expense_detail;

GRANT SELECT ON reporting.final_v5_counter_expense_detail TO gateway_reader;

COMMIT;
