-- Canonical policy excerpt installed by db/init/09-final-v5-rls-fixture.sql.
-- The target is a real base table over the frozen travel fixture, never the
-- historical/nonexistent reporting.orders placeholder.
ALTER TABLE final_v5_rls.expense_detail ENABLE ROW LEVEL SECURITY;
ALTER TABLE final_v5_rls.expense_detail FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS final_v5_sales_scope ON final_v5_rls.expense_detail;
CREATE POLICY final_v5_sales_scope
ON final_v5_rls.expense_detail
AS PERMISSIVE
FOR SELECT
TO final_v5_rls_reader
USING (department = '销售部');
REVOKE ALL ON TABLE final_v5_rls.expense_detail FROM final_v5_rls_reader;
GRANT SELECT (receipt_no, amount) ON final_v5_rls.expense_detail TO final_v5_rls_reader;
