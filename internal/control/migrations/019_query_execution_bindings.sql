-- The signed description of what one governed operation actually executed.
--
-- Query Receipt V8 binds the authorization, the budget and the exposure
-- accounting, but nothing in it says which physical statements ran. The
-- evaluation filled that gap by re-deriving them afterwards, which is a second
-- opinion about the execution rather than evidence about it.
--
-- The row is written in the same transaction as the terminal query evidence, so
-- a settled query either has both or neither. It is read back by
-- QueryReceiptEvidence exactly like the exposure charge and the artifact
-- registration, which is what lets a receipt be re-signed from persisted rows
-- after a restart rather than from anything held in memory.
--
-- No SQL text is stored. Every statement appears as a digest: the receipt is
-- retained, replayed and handed to a finalizer that must not learn what was
-- queried.
CREATE TABLE IF NOT EXISTS query_execution_bindings (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    -- The canonical, SQL-free structures. Stored whole rather than shredded
    -- into columns: they are covered by the receipt signature as documents, and
    -- a column-by-column projection would let a reload differ from what was
    -- signed while every individual column still looked right.
    binding_json BYTEA NOT NULL,
    binding_sha256 TEXT NOT NULL,
    exposure_ledger_before_json BYTEA NOT NULL,
    exposure_ledger_before_sha256 TEXT NOT NULL,
    -- Denormalized for queryability and for cheap fail-closed checks. They are
    -- redundant with binding_json on purpose; a disagreement between them and
    -- the document is itself evidence of tampering.
    path_kind TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (length(binding_sha256) = 64),
    CHECK (length(exposure_ledger_before_sha256) = 64),
    CHECK (octet_length(binding_json) > 0),
    CHECK (octet_length(exposure_ledger_before_json) > 0),
    -- idempotent_replay is deliberately absent: that path returns the original
    -- signed receipt unchanged and creates no new binding, so a row claiming it
    -- is a contradiction.
    CHECK (path_kind IN ('paired_novel', 'single_query', 'semantic_replay'))
);

CREATE INDEX IF NOT EXISTS query_execution_bindings_path_kind_idx
    ON query_execution_bindings(path_kind);

-- Immutable once written, like the receipt it is signed into. An execution
-- binding that could be updated after the fact would describe whatever the
-- last writer preferred rather than what ran.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_execution_bindings_no_update'
          AND tgrelid = 'query_execution_bindings'::regclass
    ) THEN
        CREATE TRIGGER query_execution_bindings_no_update
        BEFORE UPDATE ON query_execution_bindings
        FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'query_execution_bindings_no_delete'
          AND tgrelid = 'query_execution_bindings'::regclass
    ) THEN
        CREATE TRIGGER query_execution_bindings_no_delete
        BEFORE DELETE ON query_execution_bindings
        FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
    END IF;
END $$;
