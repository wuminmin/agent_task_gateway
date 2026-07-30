-- Large results are persisted as independently authenticated chunks so the
-- gateway never has to reconstruct a >128 MiB plaintext or ciphertext buffer
-- during the atomic result/ledger/audit finalize transaction. Existing rows
-- remain byte-for-byte compatible with the single AES-GCM representation.

ALTER TABLE encrypted_query_results
    ADD COLUMN storage_format TEXT NOT NULL DEFAULT 'single-aes-gcm-v1',
    ADD COLUMN plaintext_size BIGINT,
    ADD COLUMN chunk_count BIGINT NOT NULL DEFAULT 0;

ALTER TABLE encrypted_query_results
    ADD CONSTRAINT encrypted_query_results_storage_format_check
        CHECK (storage_format IN ('single-aes-gcm-v1', 'chunked-aes-gcm-v1')),
    ADD CONSTRAINT encrypted_query_results_plaintext_size_check
        CHECK (plaintext_size IS NULL OR plaintext_size >= 0),
    ADD CONSTRAINT encrypted_query_results_chunk_count_check
        CHECK (chunk_count >= 0),
    ADD CONSTRAINT encrypted_query_results_storage_shape_check
        CHECK (
            (storage_format = 'single-aes-gcm-v1' AND chunk_count = 0)
            OR
            (storage_format = 'chunked-aes-gcm-v1' AND plaintext_size IS NOT NULL AND chunk_count > 0)
        );

CREATE TABLE encrypted_query_result_chunks (
    query_id TEXT NOT NULL REFERENCES encrypted_query_results(query_id) ON DELETE CASCADE,
    chunk_ordinal BIGINT NOT NULL CHECK (chunk_ordinal >= 0),
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    PRIMARY KEY (query_id, chunk_ordinal)
);

-- A retained semantic-cache entry is unusable without its source ciphertext.
-- Couple retention purge to cache eviction while leaving query, observation,
-- ledger, receipt, and audit evidence intact.
ALTER TABLE v4_committed_materializations
    ADD CONSTRAINT v4_materialization_encrypted_source_fkey
    FOREIGN KEY (source_query_id)
    REFERENCES encrypted_query_results(query_id)
    ON DELETE CASCADE;
