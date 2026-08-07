-- Which QueryExecutionBinding version a stored row carries.
--
-- # Why the 019 row schema did not need replacing
--
-- Every column 019 defines means the same thing for a V2 document as for a V1
-- one. binding_json is the whole canonical document either way; binding_sha256
-- is the document's own sealed digest; path_kind is the same closed set; the
-- exposure ledger pre-state is byte-identical in shape. None of them is
-- V1-specific, so none of them is being reinterpreted here and no V2 table is
-- needed.
--
-- What 019 has no way to say is WHICH document a row holds. A reader could peek
-- at the "version" member inside binding_json and dispatch on that, and that is
-- in fact what the decoder does -- the document is the fact. But then the
-- version would be the one load-bearing property of a row with no column, no
-- constraint and no index behind it, and "how many V2 rows exist" would be a
-- question only a full scan with a JSON parse could answer. That question is the
-- cutover.
--
-- So the column is added for the same reason binding_sha256 and path_kind are
-- already denormalized: queryability, and a cheap fail-closed check. It is
-- redundant with the document on purpose, and a disagreement between them is
-- itself evidence that a value changed outside what the receipt signature
-- covers.
--
-- Existing rows are V1 by construction: V2 did not exist when they were written,
-- and the table is immutable by trigger so none of them can have become
-- something else. The default backfills them and is then dropped, so a new write
-- has to state which version it is storing rather than inherit an answer.
ALTER TABLE query_execution_bindings
    ADD COLUMN IF NOT EXISTS binding_version TEXT NOT NULL
        DEFAULT 'taskgate-query-execution-binding-v1';

ALTER TABLE query_execution_bindings
    ALTER COLUMN binding_version DROP DEFAULT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'query_execution_bindings_binding_version_check'
          AND conrelid = 'query_execution_bindings'::regclass
    ) THEN
        ALTER TABLE query_execution_bindings
            ADD CONSTRAINT query_execution_bindings_binding_version_check
            CHECK (binding_version IN (
                'taskgate-query-execution-binding-v1',
                'taskgate-query-execution-binding-v2'
            ));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS query_execution_bindings_binding_version_idx
    ON query_execution_bindings(binding_version);
