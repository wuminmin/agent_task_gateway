-- TaskGate V5 keeps V4 publication ordinal/Roaring storage for release and
-- influence, while replacing sparse per-fact outcome membership with an exact
-- content-addressed SHA-256 radix set. Existing V4 roots are never rewritten.

ALTER TABLE task_grants
    ADD COLUMN predicate_footprint_version TEXT NOT NULL DEFAULT '',
    ADD COLUMN predicate_max_raw_literals INTEGER NOT NULL DEFAULT 0 CHECK (predicate_max_raw_literals >= 0),
    ADD COLUMN predicate_max_unique_atoms INTEGER NOT NULL DEFAULT 0 CHECK (predicate_max_unique_atoms BETWEEN 0 AND 65536),
    ADD COLUMN predicate_max_atom_payload_bytes INTEGER NOT NULL DEFAULT 0 CHECK (predicate_max_atom_payload_bytes >= 0),
    ADD COLUMN predicate_max_total_payload_bytes INTEGER NOT NULL DEFAULT 0 CHECK (predicate_max_total_payload_bytes >= 0);

CREATE OR REPLACE VIEW grants AS SELECT * FROM task_grants;

ALTER TABLE task_grants DROP CONSTRAINT task_grants_exposure_shape;
ALTER TABLE task_grants ADD CONSTRAINT task_grants_exposure_shape CHECK (
    (max_release_facts = 0 AND max_influence_facts = 0 AND max_outcome_facts = 0
        AND exposure_profile_version = '' AND predicate_footprint_version = ''
        AND predicate_max_raw_literals = 0 AND predicate_max_unique_atoms = 0
        AND predicate_max_atom_payload_bytes = 0 AND predicate_max_total_payload_bytes = 0)
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts = 0
        AND exposure_profile_version IN ('taskgate-exposure-v1', 'taskgate-exposure-v2')
        AND predicate_footprint_version = '' AND predicate_max_raw_literals = 0
        AND predicate_max_unique_atoms = 0 AND predicate_max_atom_payload_bytes = 0
        AND predicate_max_total_payload_bytes = 0)
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts > 0
        AND exposure_profile_version IN ('taskgate-exposure-v3', 'taskgate-exposure-v4')
        AND predicate_footprint_version = '' AND predicate_max_raw_literals = 0
        AND predicate_max_unique_atoms = 0 AND predicate_max_atom_payload_bytes = 0
        AND predicate_max_total_payload_bytes = 0)
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts > 0
        AND exposure_profile_version = 'taskgate-exposure-v5'
        AND predicate_footprint_version = 'taskgate-predicate-footprint-v1'
        AND predicate_max_raw_literals >= predicate_max_unique_atoms
        AND predicate_max_unique_atoms BETWEEN 1 AND 65536
        AND predicate_max_atom_payload_bytes > 0
        AND predicate_max_total_payload_bytes >= predicate_max_atom_payload_bytes)
);

-- Once the ordinal cutover is active, only ordinal-backed V4/V5 grants and
-- queries may be created. Replacing these functions preserves the deployment
-- marker while admitting the clean-cut V5 profile.
CREATE OR REPLACE FUNCTION reject_non_v4_grant_after_v4() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton)
       AND NEW.exposure_profile_version NOT IN ('taskgate-exposure-v4', 'taskgate-exposure-v5') THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'non-V4 task grants are disabled after TaskGate V4 activation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION require_v4_grant_after_v4() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton)
       AND NOT EXISTS (
           SELECT 1 FROM task_grants
           WHERE task_id = NEW.task_id
             AND exposure_profile_version IN ('taskgate-exposure-v4', 'taskgate-exposure-v5')
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'queries require a V4 task grant after cutover';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE v5_outcome_facts (
    fact_sha256 TEXT PRIMARY KEY CHECK (fact_sha256 ~ '^[0-9a-f]{64}$'),
    fact_kind TEXT NOT NULL CHECK (fact_kind IN ('PREDICATE_ATOM', 'COMPOSITE_OUTCOME')),
    canonical_payload BYTEA NOT NULL,
    predicate_context_sha256 TEXT CHECK (predicate_context_sha256 IS NULL OR predicate_context_sha256 ~ '^[0-9a-f]{64}$'),
    first_catalog_digest TEXT NOT NULL CHECK (first_catalog_digest ~ '^[0-9a-f]{64}$'),
    first_query_id TEXT NOT NULL REFERENCES query_records(id),
    first_seen_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v5_outcome_hash_leaves (
    leaf_sha256 TEXT PRIMARY KEY CHECK (leaf_sha256 ~ '^[0-9a-f]{64}$'),
    prefix16 INTEGER NOT NULL CHECK (prefix16 BETWEEN 0 AND 65535),
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    cardinality INTEGER NOT NULL CHECK (cardinality BETWEEN 1 AND 4096),
    codec TEXT NOT NULL CHECK (codec IN ('RAW', 'ZSTD')),
    payload BYTEA NOT NULL,
    uncompressed_size INTEGER NOT NULL CHECK (uncompressed_size > 0),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (prefix16, chunk_index, leaf_sha256)
);

CREATE TABLE v5_outcome_hash_blocks (
    block_sha256 TEXT PRIMARY KEY CHECK (block_sha256 ~ '^[0-9a-f]{64}$'),
    prefix8 INTEGER NOT NULL CHECK (prefix8 BETWEEN 0 AND 255),
    cardinality BIGINT NOT NULL CHECK (cardinality > 0),
    manifest BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v5_outcome_hash_sets (
    set_sha256 TEXT PRIMARY KEY CHECK (set_sha256 ~ '^[0-9a-f]{64}$'),
    cardinality BIGINT NOT NULL CHECK (cardinality >= 0),
    block_count INTEGER NOT NULL CHECK (block_count BETWEEN 0 AND 256),
    root_manifest BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v5_exposure_root_heads (
    root_task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v5'),
    dictionary_set_digest TEXT REFERENCES v4_dictionary_sets(dictionary_set_digest),
    epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    max_release_facts BIGINT NOT NULL CHECK (max_release_facts > 0),
    max_influence_facts BIGINT NOT NULL CHECK (max_influence_facts > 0),
    max_outcome_facts BIGINT NOT NULL CHECK (max_outcome_facts > 0),
    used_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_release_facts >= 0),
    used_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_influence_facts >= 0),
    used_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_outcome_facts >= 0),
    release_set_sha256 TEXT REFERENCES v4_bitmap_sets(set_sha256),
    influence_set_sha256 TEXT REFERENCES v4_bitmap_sets(set_sha256),
    outcome_set_sha256 TEXT REFERENCES v5_outcome_hash_sets(set_sha256),
    predicate_profile_version TEXT NOT NULL DEFAULT 'taskgate-predicate-footprint-v1'
        CHECK (predicate_profile_version = 'taskgate-predicate-footprint-v1'),
    max_raw_literals_per_query INTEGER NOT NULL DEFAULT 20000 CHECK (max_raw_literals_per_query BETWEEN 1 AND 1000000),
    max_unique_atoms_per_query INTEGER NOT NULL DEFAULT 10000 CHECK (max_unique_atoms_per_query BETWEEN 1 AND 65536),
    max_atom_payload_bytes INTEGER NOT NULL DEFAULT 4096 CHECK (max_atom_payload_bytes BETWEEN 1 AND 1048576),
    max_total_atom_payload_bytes INTEGER NOT NULL DEFAULT 8388608 CHECK (max_total_atom_payload_bytes BETWEEN 1 AND 1073741824),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (used_release_facts <= max_release_facts),
    CHECK (used_influence_facts <= max_influence_facts),
    CHECK (used_outcome_facts <= max_outcome_facts),
    CHECK ((dictionary_set_digest IS NULL AND epoch = 0
            AND used_release_facts = 0 AND used_influence_facts = 0 AND used_outcome_facts = 0
            AND release_set_sha256 IS NULL AND influence_set_sha256 IS NULL AND outcome_set_sha256 IS NULL)
        OR dictionary_set_digest IS NOT NULL)
);

CREATE TRIGGER v5_root_head_requires_cutover
BEFORE INSERT ON v5_exposure_root_heads
FOR EACH ROW EXECUTE FUNCTION require_v4_cutover();

CREATE TABLE v5_query_exposure_reservations (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES v5_exposure_root_heads(root_task_id),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v5'),
    status TEXT NOT NULL CHECK (status IN ('RESERVED','SETTLED','RELEASED')),
    estimated_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_release_facts >= 0),
    estimated_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_influence_facts >= 0),
    estimated_outcome_facts BIGINT NOT NULL CHECK (estimated_outcome_facts >= 1),
    actual_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_release_facts >= 0),
    actual_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_influence_facts >= 0),
    actual_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_outcome_facts >= 0),
    actual_predicate_atom_count BIGINT NOT NULL DEFAULT 0 CHECK (actual_predicate_atom_count >= 0),
    charged_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_release_facts >= 0),
    charged_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_influence_facts >= 0),
    charged_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_outcome_facts >= 0),
    charged_predicate_atom_count BIGINT NOT NULL DEFAULT 0 CHECK (charged_predicate_atom_count >= 0),
    observation_sha256 TEXT NOT NULL DEFAULT '',
    predicate_context_sha256 TEXT NOT NULL DEFAULT '',
    predicate_set_sha256 TEXT NOT NULL DEFAULT '',
    composite_outcome_sha256 TEXT NOT NULL DEFAULT '',
    root_epoch BIGINT NOT NULL DEFAULT 0 CHECK (root_epoch >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    CHECK (
        (status = 'RESERVED' AND settled_at IS NULL AND observation_sha256 = '' AND root_epoch = 0)
        OR (status = 'SETTLED' AND settled_at IS NOT NULL
            AND observation_sha256 ~ '^[0-9a-f]{64}$'
            AND predicate_context_sha256 ~ '^[0-9a-f]{64}$'
            AND predicate_set_sha256 ~ '^[0-9a-f]{64}$'
            AND composite_outcome_sha256 ~ '^[0-9a-f]{64}$'
            AND actual_outcome_facts = actual_predicate_atom_count + 1
            AND charged_predicate_atom_count <= charged_outcome_facts)
        OR (status = 'RELEASED' AND settled_at IS NOT NULL AND observation_sha256 = '' AND root_epoch = 0)
    )
);
CREATE INDEX v5_query_exposure_root_idx
    ON v5_query_exposure_reservations(root_task_id, created_at, query_id);

CREATE TABLE v5_observations (
    observation_sha256 TEXT PRIMARY KEY CHECK (observation_sha256 ~ '^[0-9a-f]{64}$'),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v5'),
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    release_set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    influence_set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    outcome_set_sha256 TEXT NOT NULL REFERENCES v5_outcome_hash_sets(set_sha256),
    predicate_context_sha256 TEXT NOT NULL CHECK (predicate_context_sha256 ~ '^[0-9a-f]{64}$'),
    predicate_set_sha256 TEXT NOT NULL CHECK (predicate_set_sha256 ~ '^[0-9a-f]{64}$'),
    predicate_atom_count BIGINT NOT NULL CHECK (predicate_atom_count >= 0),
    composite_outcome_sha256 TEXT NOT NULL CHECK (composite_outcome_sha256 ~ '^[0-9a-f]{64}$'),
    actual_release_facts BIGINT NOT NULL CHECK (actual_release_facts >= 0),
    actual_influence_facts BIGINT NOT NULL CHECK (actual_influence_facts >= 0),
    actual_outcome_facts BIGINT NOT NULL CHECK (actual_outcome_facts = predicate_atom_count + 1),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v5_root_observations (
    root_task_id TEXT NOT NULL REFERENCES v5_exposure_root_heads(root_task_id),
    observation_sha256 TEXT NOT NULL REFERENCES v5_observations(observation_sha256),
    first_query_id TEXT NOT NULL REFERENCES query_records(id),
    first_epoch BIGINT NOT NULL CHECK (first_epoch >= 0),
    first_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (root_task_id, observation_sha256)
);

CREATE TABLE v5_query_observations (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    root_task_id TEXT NOT NULL REFERENCES v5_exposure_root_heads(root_task_id),
    observation_sha256 TEXT NOT NULL REFERENCES v5_observations(observation_sha256),
    root_epoch BIGINT NOT NULL CHECK (root_epoch >= 0),
    charged_release_facts BIGINT NOT NULL CHECK (charged_release_facts >= 0),
    charged_influence_facts BIGINT NOT NULL CHECK (charged_influence_facts >= 0),
    charged_predicate_atom_count BIGINT NOT NULL CHECK (charged_predicate_atom_count >= 0),
    charged_composite_count BIGINT NOT NULL CHECK (charged_composite_count BETWEEN 0 AND 1),
    charged_outcome_facts BIGINT NOT NULL CHECK (charged_outcome_facts = charged_predicate_atom_count + charged_composite_count),
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (root_task_id, observation_sha256)
        REFERENCES v5_root_observations(root_task_id, observation_sha256)
);

-- V5 replay is physically separate so V4/V5 observations can never cross
-- profile through a cache row even if every other digest were equal.
CREATE TABLE v5_committed_materializations (
    cache_key_sha256 TEXT PRIMARY KEY CHECK (cache_key_sha256 ~ '^[0-9a-f]{64}$'),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES v5_exposure_root_heads(root_task_id),
    source_query_id TEXT NOT NULL REFERENCES query_records(id),
    observation_sha256 TEXT NOT NULL REFERENCES v5_observations(observation_sha256),
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    grant_digest TEXT NOT NULL CHECK (grant_digest ~ '^[0-9a-f]{64}$'),
    catalog_digest TEXT NOT NULL CHECK (catalog_digest ~ '^[0-9a-f]{64}$'),
    result_sha256 TEXT NOT NULL CHECK (result_sha256 ~ '^[0-9a-f]{64}$'),
    row_count BIGINT NOT NULL CHECK (row_count >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    UNIQUE (task_id,source_query_id,cache_key_sha256),
    CHECK (expires_at IS NULL OR expires_at > created_at),
    FOREIGN KEY (root_task_id,observation_sha256)
        REFERENCES v5_root_observations(root_task_id,observation_sha256)
);
CREATE INDEX v5_materializations_lookup_idx ON v5_committed_materializations(
    task_id,grant_digest,catalog_digest,dictionary_set_digest,cache_key_sha256);
CREATE TRIGGER v5_materializations_no_update BEFORE UPDATE ON v5_committed_materializations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER v5_observations_no_update BEFORE UPDATE ON v5_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_observations_no_delete BEFORE DELETE ON v5_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_root_observations_no_update BEFORE UPDATE ON v5_root_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_root_observations_no_delete BEFORE DELETE ON v5_root_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_query_observations_no_update BEFORE UPDATE ON v5_query_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_query_observations_no_delete BEFORE DELETE ON v5_query_observations
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

-- Content-addressed objects and dictionary facts are immutable. Root heads and
-- reservations are the only mutable V5 rows and move under one transaction.
CREATE TRIGGER v5_outcome_facts_no_update BEFORE UPDATE ON v5_outcome_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_facts_no_delete BEFORE DELETE ON v5_outcome_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_leaves_no_update BEFORE UPDATE ON v5_outcome_hash_leaves
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_leaves_no_delete BEFORE DELETE ON v5_outcome_hash_leaves
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_blocks_no_update BEFORE UPDATE ON v5_outcome_hash_blocks
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_blocks_no_delete BEFORE DELETE ON v5_outcome_hash_blocks
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_sets_no_update BEFORE UPDATE ON v5_outcome_hash_sets
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
CREATE TRIGGER v5_outcome_sets_no_delete BEFORE DELETE ON v5_outcome_hash_sets
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
