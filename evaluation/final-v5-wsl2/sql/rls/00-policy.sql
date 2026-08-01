DO $$ BEGIN CREATE ROLE taskgate_rls_subject NOLOGIN NOBYPASSRLS; EXCEPTION WHEN duplicate_object THEN NULL; END $$;
ALTER TABLE reporting.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE reporting.orders FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS final_v5_tenant_scope ON reporting.orders;
CREATE POLICY final_v5_tenant_scope ON reporting.orders FOR SELECT TO taskgate_rls_subject USING (tenant_id = current_setting('taskgate.tenant_id', true)::bigint);
GRANT SELECT ON reporting.orders TO taskgate_rls_subject;
