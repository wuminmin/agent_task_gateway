CREATE OR REPLACE FUNCTION reject_terminal_query_record_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'query_records rows are immutable';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.task_id IS DISTINCT FROM OLD.task_id
       OR NEW.request_id IS DISTINCT FROM OLD.request_id
       OR NEW.actor IS DISTINCT FROM OLD.actor
       OR NEW.request_digest IS DISTINCT FROM OLD.request_digest
       OR NEW.sql_fingerprint IS DISTINCT FROM OLD.sql_fingerprint
       OR NEW.catalog_version IS DISTINCT FROM OLD.catalog_version
       OR NEW.catalog_digest IS DISTINCT FROM OLD.catalog_digest
       OR NEW.datasource_id IS DISTINCT FROM OLD.datasource_id
       OR NEW.schema_digest IS DISTINCT FROM OLD.schema_digest
       OR NEW.manifest_digest IS DISTINCT FROM OLD.manifest_digest
       OR NEW.grant_digest IS DISTINCT FROM OLD.grant_digest
       OR NEW.policy_decision IS DISTINCT FROM OLD.policy_decision
       OR NEW.reserved_rows IS DISTINCT FROM OLD.reserved_rows
       OR NEW.reserved_db_ms IS DISTINCT FROM OLD.reserved_db_ms
       OR NEW.budget_before_json IS DISTINCT FROM OLD.budget_before_json
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'query_records identity and authorization evidence are immutable';
    END IF;

    IF OLD.status <> 'RESERVED' THEN
        RAISE EXCEPTION 'terminal query_records row is immutable';
    END IF;

    RETURN NEW;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_records_terminal_no_update'
          AND tgrelid = 'query_records'::regclass
    ) THEN
        CREATE TRIGGER query_records_terminal_no_update
        BEFORE UPDATE ON query_records
        FOR EACH ROW EXECUTE FUNCTION reject_terminal_query_record_change();
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_records_no_delete'
          AND tgrelid = 'query_records'::regclass
    ) THEN
        CREATE TRIGGER query_records_no_delete
        BEFORE DELETE ON query_records
        FOR EACH ROW EXECUTE FUNCTION reject_terminal_query_record_change();
    END IF;
END $$;
