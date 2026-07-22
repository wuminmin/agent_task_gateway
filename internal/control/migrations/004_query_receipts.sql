CREATE TABLE IF NOT EXISTS query_receipts (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    receipt_version TEXT NOT NULL,
    gateway_key_id TEXT NOT NULL,
    signature TEXT NOT NULL,
    signed_at TIMESTAMPTZ NOT NULL,
    terminal_audit_sequence BIGINT NOT NULL REFERENCES audit_events(sequence),
    terminal_audit_hash TEXT NOT NULL,
    receipt_json BYTEA NOT NULL,
    receipt_sha256 TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (terminal_audit_sequence > 0),
    CHECK (length(terminal_audit_hash) = 64),
    CHECK (length(receipt_sha256) = 64)
);

CREATE INDEX IF NOT EXISTS query_receipts_terminal_audit_idx
    ON query_receipts(terminal_audit_sequence);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_receipts_no_update'
          AND tgrelid = 'query_receipts'::regclass
    ) THEN
        CREATE TRIGGER query_receipts_no_update
        BEFORE UPDATE ON query_receipts
        FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_receipts_no_delete'
          AND tgrelid = 'query_receipts'::regclass
    ) THEN
        CREATE TRIGGER query_receipts_no_delete
        BEFORE DELETE ON query_receipts
        FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
    END IF;
END $$;
