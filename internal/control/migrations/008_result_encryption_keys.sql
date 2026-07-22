CREATE TABLE IF NOT EXISTS result_encryption_keys (
    key_id TEXT PRIMARY KEY CHECK (length(btrim(key_id)) > 0 AND key_id = btrim(key_id)),
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','ERASED')),
    created_at TIMESTAMPTZ NOT NULL,
    erased_at TIMESTAMPTZ,
    erased_by TEXT NOT NULL DEFAULT '',
    CHECK (
        (status = 'ACTIVE' AND erased_at IS NULL AND erased_by = '')
        OR (status = 'ERASED' AND erased_at IS NOT NULL AND erased_by <> '')
    )
);

INSERT INTO result_encryption_keys(key_id, status, created_at, erased_by, erased_at)
VALUES (
    'local-aes256-gcm-v1',
    'ACTIVE',
    COALESCE((SELECT min(created_at) FROM encrypted_query_results), CURRENT_TIMESTAMP),
    '',
    NULL
)
ON CONFLICT (key_id) DO NOTHING;

ALTER TABLE encrypted_query_results ADD COLUMN IF NOT EXISTS key_id TEXT;
UPDATE encrypted_query_results
SET key_id = 'local-aes256-gcm-v1'
WHERE key_id IS NULL OR btrim(key_id) = '';
ALTER TABLE encrypted_query_results ALTER COLUMN key_id SET NOT NULL;
ALTER TABLE encrypted_query_results ALTER COLUMN key_id SET DEFAULT 'local-aes256-gcm-v1';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'encrypted_query_results_key_id_fkey'
          AND conrelid = 'encrypted_query_results'::regclass
    ) THEN
        ALTER TABLE encrypted_query_results
        ADD CONSTRAINT encrypted_query_results_key_id_fkey
        FOREIGN KEY (key_id) REFERENCES result_encryption_keys(key_id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS encrypted_results_key_idx
    ON encrypted_query_results(key_id, created_at, query_id);

CREATE OR REPLACE FUNCTION reject_result_encryption_key_reactivation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'result_encryption_keys rows are retained as erasure evidence';
    END IF;

    IF NEW.key_id IS DISTINCT FROM OLD.key_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'result_encryption_keys identity is immutable';
    END IF;

    IF OLD.status = 'ERASED'
       AND (
           NEW.status IS DISTINCT FROM OLD.status
           OR NEW.erased_at IS DISTINCT FROM OLD.erased_at
           OR NEW.erased_by IS DISTINCT FROM OLD.erased_by
       ) THEN
        RAISE EXCEPTION 'erased result_encryption_keys rows are immutable';
    END IF;

    RETURN NEW;
END;
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'result_encryption_keys_no_reactivation'
          AND tgrelid = 'result_encryption_keys'::regclass
    ) THEN
        CREATE TRIGGER result_encryption_keys_no_reactivation
        BEFORE UPDATE OR DELETE ON result_encryption_keys
        FOR EACH ROW EXECUTE FUNCTION reject_result_encryption_key_reactivation();
    END IF;
END $$;
