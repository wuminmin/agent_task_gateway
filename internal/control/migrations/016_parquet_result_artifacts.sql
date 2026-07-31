-- New query results are immutable encrypted Parquet objects. Control
-- PostgreSQL retains only authorization, lifecycle and integrity metadata;
-- legacy encrypted_query_results rows remain readable during migration.
CREATE TABLE result_artifacts (
    result_id TEXT PRIMARY KEY CHECK (length(btrim(result_id)) > 0 AND result_id = btrim(result_id)),
    query_id TEXT NOT NULL UNIQUE REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    key_id TEXT NOT NULL REFERENCES result_encryption_keys(key_id),
    format TEXT NOT NULL CHECK (format = 'parquet'),
    encryption TEXT NOT NULL CHECK (encryption = 'chunked-aes-gcm-v1'),
    staging_key TEXT NOT NULL UNIQUE CHECK (length(btrim(staging_key)) > 0),
    object_key TEXT NOT NULL UNIQUE CHECK (length(btrim(object_key)) > 0),
    object_etag TEXT NOT NULL CHECK (length(btrim(object_etag)) > 0),
    parquet_sha256 TEXT NOT NULL CHECK (parquet_sha256 ~ '^[0-9a-f]{64}$'),
    object_sha256 TEXT NOT NULL CHECK (object_sha256 ~ '^[0-9a-f]{64}$'),
    parquet_size BIGINT NOT NULL CHECK (parquet_size >= 0),
    object_size BIGINT NOT NULL CHECK (object_size > 0),
    row_count BIGINT NOT NULL CHECK (row_count >= 0),
    column_count INTEGER NOT NULL CHECK (column_count > 0),
    schema_json JSONB NOT NULL,
    result_metadata_json JSONB NOT NULL,
    acl_json JSONB NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING','AVAILABLE','DELETING','DELETED')),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    CHECK (expires_at IS NULL OR expires_at > created_at),
    CHECK (consumed_at IS NULL OR consumed_at >= created_at),
    CHECK (deleted_at IS NULL OR (consumed_at IS NOT NULL AND deleted_at >= consumed_at)),
    CHECK (
        (status = 'PENDING' AND consumed_at IS NULL AND deleted_at IS NULL)
        OR (status = 'AVAILABLE' AND consumed_at IS NOT NULL AND deleted_at IS NULL)
        OR (status = 'DELETING' AND consumed_at IS NOT NULL AND deleted_at IS NULL)
        OR (status = 'DELETED' AND consumed_at IS NOT NULL AND deleted_at IS NOT NULL)
    )
);

CREATE INDEX result_artifacts_task_idx
    ON result_artifacts(task_id, created_at, result_id);
CREATE INDEX result_artifacts_pending_idx
    ON result_artifacts(status, result_id)
    WHERE status = 'PENDING';
CREATE INDEX result_artifacts_retention_idx
    ON result_artifacts(status, expires_at, result_id)
    WHERE status IN ('AVAILABLE','DELETING');
CREATE INDEX result_artifacts_admin_purge_idx
    ON result_artifacts(status, created_at, result_id)
    WHERE status IN ('AVAILABLE','DELETING');
CREATE INDEX result_artifacts_key_status_idx
    ON result_artifacts(key_id, status);

CREATE FUNCTION enforce_result_artifact_immutability()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'result artifact evidence cannot be deleted';
    END IF;
    IF NEW.result_id IS DISTINCT FROM OLD.result_id
       OR NEW.query_id IS DISTINCT FROM OLD.query_id
       OR NEW.task_id IS DISTINCT FROM OLD.task_id
       OR NEW.key_id IS DISTINCT FROM OLD.key_id
       OR NEW.format IS DISTINCT FROM OLD.format
       OR NEW.encryption IS DISTINCT FROM OLD.encryption
       OR NEW.staging_key IS DISTINCT FROM OLD.staging_key
       OR NEW.object_key IS DISTINCT FROM OLD.object_key
       OR NEW.parquet_sha256 IS DISTINCT FROM OLD.parquet_sha256
       OR NEW.object_sha256 IS DISTINCT FROM OLD.object_sha256
       OR NEW.parquet_size IS DISTINCT FROM OLD.parquet_size
       OR NEW.object_size IS DISTINCT FROM OLD.object_size
       OR NEW.row_count IS DISTINCT FROM OLD.row_count
       OR NEW.column_count IS DISTINCT FROM OLD.column_count
       OR NEW.schema_json IS DISTINCT FROM OLD.schema_json
       OR NEW.result_metadata_json IS DISTINCT FROM OLD.result_metadata_json
       OR NEW.acl_json IS DISTINCT FROM OLD.acl_json
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'result artifact identity and integrity evidence is immutable';
    END IF;
    IF OLD.status = 'PENDING' AND NEW.status = 'AVAILABLE' THEN
        IF length(btrim(NEW.object_etag)) = 0 OR NEW.consumed_at IS NULL OR NEW.deleted_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid result artifact availability transition';
        END IF;
    ELSIF OLD.status = 'AVAILABLE' AND NEW.status = 'DELETING' THEN
        IF NEW.object_etag IS DISTINCT FROM OLD.object_etag
           OR NEW.consumed_at IS DISTINCT FROM OLD.consumed_at
           OR NEW.deleted_at IS NOT NULL THEN
            RAISE EXCEPTION 'invalid result artifact deletion claim';
        END IF;
    ELSIF OLD.status = 'DELETING' AND NEW.status = 'DELETED' THEN
        IF NEW.object_etag IS DISTINCT FROM OLD.object_etag
           OR NEW.consumed_at IS DISTINCT FROM OLD.consumed_at
           OR NEW.deleted_at IS NULL THEN
            RAISE EXCEPTION 'invalid result artifact deletion completion';
        END IF;
    ELSE
        RAISE EXCEPTION 'invalid result artifact status transition % -> %', OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER result_artifacts_immutable
BEFORE UPDATE OR DELETE ON result_artifacts
FOR EACH ROW EXECUTE FUNCTION enforce_result_artifact_immutability();

-- Migration 015 coupled the V4 semantic-cache row specifically to the legacy
-- PostgreSQL ciphertext table. Source validity is now checked against either
-- the legacy table or result_artifacts in the Control code.
ALTER TABLE v4_committed_materializations
    DROP CONSTRAINT IF EXISTS v4_materialization_encrypted_source_fkey;
