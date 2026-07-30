-- V4 replaces per-fact online accounting with immutable snapshot dictionaries
-- and content-addressed, segmented bitmap sets.  This is deliberately a clean
-- cutover: an installation with any V1--V3 exposure state must be rebuilt or
-- migrated by an explicit offline tool.  Never discard old accounting evidence
-- from an automatic schema migration.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM exposure_ledgers LIMIT 1)
       OR EXISTS (SELECT 1 FROM query_exposure_reservations LIMIT 1)
       OR EXISTS (SELECT 1 FROM exposure_facts LIMIT 1)
       OR EXISTS (SELECT 1 FROM query_exposure_facts LIMIT 1)
       OR EXISTS (
           SELECT 1
           FROM query_records q
           JOIN task_grants g ON g.task_id = q.task_id
           WHERE q.status = 'RESERVED'
             AND g.exposure_profile_version <> 'taskgate-exposure-v4'
           LIMIT 1
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'TaskGate V4 clean cutover refused: legacy exposure state or in-flight query exists; drain, rebuild, or run an explicit offline migration';
    END IF;
END $$;

ALTER TABLE task_grants DROP CONSTRAINT task_grants_exposure_shape;
ALTER TABLE task_grants ADD CONSTRAINT task_grants_exposure_shape CHECK (
    (max_release_facts = 0 AND max_influence_facts = 0 AND max_outcome_facts = 0
        AND exposure_profile_version = '')
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts = 0
        AND exposure_profile_version IN ('taskgate-exposure-v1', 'taskgate-exposure-v2'))
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts > 0
        AND exposure_profile_version IN ('taskgate-exposure-v3', 'taskgate-exposure-v4'))
);

CREATE TABLE v4_cutover_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    -- Production activation is bound to the validated Catalog before the
    -- Gateway accepts traffic.  activated_by_task_id remains as a Control-PG
    -- fallback for direct V4 API users and the legacy semantic oracle tests.
    activated_by_task_id TEXT REFERENCES tasks(id),
    activated_catalog_digest TEXT CHECK (activated_catalog_digest ~ '^[0-9a-f]{64}$'),
    activated_at TIMESTAMPTZ NOT NULL,
    CHECK (num_nonnulls(activated_by_task_id, activated_catalog_digest) = 1)
);

CREATE FUNCTION enforce_v4_clean_cutover() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Serialize activation with every legacy write guarded below.  Without
    -- this transaction advisory lock, READ COMMITTED could permit the marker
    -- and a legacy insert to both observe the other as absent and then commit.
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM exposure_ledgers LIMIT 1)
       OR EXISTS (SELECT 1 FROM query_exposure_reservations LIMIT 1)
       OR EXISTS (SELECT 1 FROM exposure_facts LIMIT 1)
       OR EXISTS (SELECT 1 FROM query_exposure_facts LIMIT 1)
       OR EXISTS (
           SELECT 1
           FROM query_records q
           JOIN task_grants g ON g.task_id = q.task_id
           WHERE q.status = 'RESERVED'
             AND g.exposure_profile_version <> 'taskgate-exposure-v4'
           LIMIT 1
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'TaskGate V4 activation refused: legacy exposure state or in-flight query exists';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER v4_cutover_preflight
BEFORE INSERT ON v4_cutover_state
FOR EACH ROW EXECUTE FUNCTION enforce_v4_clean_cutover();

CREATE TRIGGER v4_cutover_state_no_update
BEFORE UPDATE ON v4_cutover_state
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER v4_cutover_state_no_delete
BEFORE DELETE ON v4_cutover_state
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE FUNCTION reject_legacy_exposure_after_v4() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'legacy exposure accounting is disabled after TaskGate V4 activation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER exposure_ledgers_reject_after_v4
BEFORE INSERT ON exposure_ledgers
FOR EACH ROW EXECUTE FUNCTION reject_legacy_exposure_after_v4();
CREATE TRIGGER query_exposure_reservations_reject_after_v4
BEFORE INSERT ON query_exposure_reservations
FOR EACH ROW EXECUTE FUNCTION reject_legacy_exposure_after_v4();
CREATE TRIGGER exposure_facts_reject_after_v4
BEFORE INSERT ON exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_legacy_exposure_after_v4();
CREATE TRIGGER query_exposure_facts_reject_after_v4
BEFORE INSERT ON query_exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_legacy_exposure_after_v4();

-- A replaced legacy Catalog must not bypass the bitmap boundary by creating a
-- V2/V3 or resource-only grant, nor may a pre-cutover resource-only task start
-- a query after V4 activation.  These guards live in Control PG, so replacing
-- the Gateway binary or Catalog cannot turn them off accidentally.
CREATE FUNCTION reject_non_v4_grant_after_v4() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton)
       AND NEW.exposure_profile_version <> 'taskgate-exposure-v4' THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'non-V4 task grants are disabled after TaskGate V4 activation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER task_grants_reject_non_v4_after_v4
BEFORE INSERT ON task_grants
FOR EACH ROW EXECUTE FUNCTION reject_non_v4_grant_after_v4();

CREATE FUNCTION require_v4_grant_after_v4() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(728194631046);
    IF EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton)
       AND NOT EXISTS (
           SELECT 1 FROM task_grants
           WHERE task_id = NEW.task_id
             AND exposure_profile_version = 'taskgate-exposure-v4'
       ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'queries require a V4 task grant after TaskGate V4 activation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER query_records_require_v4_after_v4
BEFORE INSERT ON query_records
FOR EACH ROW EXECUTE FUNCTION require_v4_grant_after_v4();

-- Immutable, independently verifiable snapshot dictionary artifacts.
CREATE TABLE v4_dictionary_manifests (
    dictionary_digest TEXT PRIMARY KEY CHECK (dictionary_digest ~ '^[0-9a-f]{64}$'),
    manifest_digest TEXT NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    publication_digest TEXT NOT NULL CHECK (publication_digest ~ '^[0-9a-f]{64}$'),
    datasource_id TEXT NOT NULL CHECK (length(btrim(datasource_id)) > 0),
    source_namespace TEXT NOT NULL CHECK (length(btrim(source_namespace)) > 0),
    snapshot_id TEXT NOT NULL CHECK (length(btrim(snapshot_id)) > 0),
    fact_count BIGINT NOT NULL CHECK (fact_count >= 0),
    segment_count INTEGER NOT NULL CHECK (segment_count > 0),
    manifest_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (dictionary_digest, manifest_digest),
    UNIQUE (publication_digest, source_namespace, snapshot_id)
);

CREATE TABLE v4_dictionary_segments (
    dictionary_digest TEXT NOT NULL REFERENCES v4_dictionary_manifests(dictionary_digest),
    segment_id TEXT NOT NULL CHECK (length(btrim(segment_id)) > 0),
    fact_kind TEXT NOT NULL CHECK (fact_kind IN ('BASE_ROW','BASE_CELL')),
    field_name TEXT NOT NULL DEFAULT '',
    ordinal_count BIGINT NOT NULL CHECK (ordinal_count >= 0 AND ordinal_count <= 4294967296),
    segment_digest TEXT NOT NULL CHECK (segment_digest ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (dictionary_digest, segment_id),
    UNIQUE (dictionary_digest, segment_digest),
    CHECK ((fact_kind = 'BASE_ROW' AND field_name = '') OR
           (fact_kind = 'BASE_CELL' AND length(btrim(field_name)) > 0))
);

CREATE TABLE v4_dictionary_chunks (
    chunk_sha256 TEXT PRIMARY KEY CHECK (chunk_sha256 ~ '^[0-9a-f]{64}$'),
    compression TEXT NOT NULL CHECK (compression IN ('NONE','ZSTD')),
    payload BYTEA NOT NULL CHECK (octet_length(payload) > 0),
    uncompressed_bytes BIGINT NOT NULL CHECK (uncompressed_bytes > 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v4_dictionary_segment_chunks (
    dictionary_digest TEXT NOT NULL,
    segment_id TEXT NOT NULL,
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    chunk_sha256 TEXT NOT NULL REFERENCES v4_dictionary_chunks(chunk_sha256),
    first_ordinal BIGINT NOT NULL CHECK (first_ordinal >= 0 AND first_ordinal < 4294967296),
    fact_count BIGINT NOT NULL CHECK (fact_count > 0 AND fact_count <= 4294967296),
    PRIMARY KEY (dictionary_digest, segment_id, chunk_index),
    UNIQUE (dictionary_digest, segment_id, first_ordinal),
    FOREIGN KEY (dictionary_digest, segment_id)
        REFERENCES v4_dictionary_segments(dictionary_digest, segment_id),
    CHECK (first_ordinal + fact_count <= 4294967296)
);

-- Whole verified hot/cold/sidecar artifacts produced by internal/ordinal may
-- span several segments.  Keep their content-addressed chunks separately from
-- optional segment-local audit chunks.
CREATE TABLE v4_dictionary_artifacts (
    dictionary_digest TEXT NOT NULL REFERENCES v4_dictionary_manifests(dictionary_digest),
    artifact_kind TEXT NOT NULL CHECK (artifact_kind IN ('HOT','COLD','SIDECAR')),
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    chunk_sha256 TEXT NOT NULL REFERENCES v4_dictionary_chunks(chunk_sha256),
    manifest_digest TEXT NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (dictionary_digest, artifact_kind, chunk_index),
    FOREIGN KEY (dictionary_digest, manifest_digest)
        REFERENCES v4_dictionary_manifests(dictionary_digest, manifest_digest)
);

CREATE TABLE v4_dictionary_sets (
    dictionary_set_digest TEXT PRIMARY KEY CHECK (dictionary_set_digest ~ '^[0-9a-f]{64}$'),
    catalog_digest TEXT NOT NULL CHECK (catalog_digest ~ '^[0-9a-f]{64}$'),
    manifest_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v4_dictionary_set_members (
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    member_index INTEGER NOT NULL CHECK (member_index >= 0),
    publication_name TEXT NOT NULL CHECK (publication_name = btrim(publication_name) AND publication_name <> ''),
    dictionary_digest TEXT NOT NULL,
    manifest_digest TEXT NOT NULL CHECK (manifest_digest ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (dictionary_set_digest, member_index),
    UNIQUE (dictionary_set_digest, publication_name),
    FOREIGN KEY (dictionary_digest, manifest_digest)
        REFERENCES v4_dictionary_manifests(dictionary_digest, manifest_digest)
);

-- Dynamic FactID hashes are globally canonical. Keep one payload for each
-- hash so even observations from different Catalog snapshots cannot hide a
-- digest/payload collision.
CREATE TABLE v4_dynamic_facts (
    fact_sha256 TEXT PRIMARY KEY CHECK (fact_sha256 ~ '^[0-9a-f]{64}$'),
    fact_kind TEXT NOT NULL CHECK (fact_kind IN ('DERIVED_RELEASE','OUTCOME')),
    canonical_payload BYTEA NOT NULL CHECK (octet_length(canonical_payload) > 0),
    first_dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    first_query_id TEXT NOT NULL REFERENCES query_records(id),
    first_seen_at TIMESTAMPTZ NOT NULL
);

-- Each portable Roaring blob contains exactly one high-16 ordinal container.
-- Its digest is domain separated by the ordinal package and binds the
-- dictionary, segment, high-16 key, cardinality, and canonical payload.
CREATE TABLE v4_bitmap_containers (
    container_sha256 TEXT PRIMARY KEY CHECK (container_sha256 ~ '^[0-9a-f]{64}$'),
    dictionary_digest TEXT NOT NULL REFERENCES v4_dictionary_manifests(dictionary_digest),
    segment_id TEXT NOT NULL,
    high16 INTEGER NOT NULL CHECK (high16 >= 0 AND high16 <= 65535),
    cardinality BIGINT NOT NULL CHECK (cardinality > 0 AND cardinality <= 65536),
    portable_payload BYTEA NOT NULL CHECK (octet_length(portable_payload) > 0),
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (dictionary_digest, segment_id)
        REFERENCES v4_dictionary_segments(dictionary_digest, segment_id)
);

CREATE TABLE v4_bitmap_sets (
    set_sha256 TEXT PRIMARY KEY CHECK (set_sha256 ~ '^[0-9a-f]{64}$'),
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    static_cardinality BIGINT NOT NULL CHECK (static_cardinality >= 0),
    dynamic_cardinality BIGINT NOT NULL CHECK (dynamic_cardinality >= 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v4_bitmap_set_containers (
    set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    dictionary_digest TEXT NOT NULL,
    segment_id TEXT NOT NULL,
    high16 INTEGER NOT NULL CHECK (high16 >= 0 AND high16 <= 65535),
    container_sha256 TEXT NOT NULL REFERENCES v4_bitmap_containers(container_sha256),
    cardinality BIGINT NOT NULL CHECK (cardinality > 0 AND cardinality <= 65536),
    PRIMARY KEY (set_sha256, dictionary_digest, segment_id, high16),
    FOREIGN KEY (dictionary_digest, segment_id)
        REFERENCES v4_dictionary_segments(dictionary_digest, segment_id)
);

CREATE TABLE v4_bitmap_set_dynamic_facts (
    set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    fact_sha256 TEXT NOT NULL REFERENCES v4_dynamic_facts(fact_sha256),
    PRIMARY KEY (set_sha256, fact_sha256)
);

-- A head update publishes all three dimensions and one epoch together.
CREATE TABLE v4_exposure_root_heads (
    root_task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v4'),
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
    outcome_set_sha256 TEXT REFERENCES v4_bitmap_sets(set_sha256),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (used_release_facts <= max_release_facts),
    CHECK (used_influence_facts <= max_influence_facts),
    CHECK (used_outcome_facts <= max_outcome_facts),
    CHECK ((dictionary_set_digest IS NULL AND epoch = 0
            AND used_release_facts = 0 AND used_influence_facts = 0 AND used_outcome_facts = 0
            AND release_set_sha256 IS NULL AND influence_set_sha256 IS NULL AND outcome_set_sha256 IS NULL)
        OR dictionary_set_digest IS NOT NULL)
);

CREATE FUNCTION require_v4_cutover() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM v4_cutover_state WHERE singleton) THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'TaskGate V4 cutover has not been activated';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER v4_root_head_requires_cutover
BEFORE INSERT ON v4_exposure_root_heads
FOR EACH ROW EXECUTE FUNCTION require_v4_cutover();

CREATE TABLE v4_query_exposure_reservations (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES v4_exposure_root_heads(root_task_id),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v4'),
    status TEXT NOT NULL CHECK (status IN ('RESERVED','SETTLED','RELEASED')),
    estimated_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_release_facts >= 0),
    estimated_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_influence_facts >= 0),
    estimated_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_outcome_facts >= 0),
    actual_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_release_facts >= 0),
    actual_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_influence_facts >= 0),
    actual_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_outcome_facts >= 0),
    charged_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_release_facts >= 0),
    charged_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_influence_facts >= 0),
    charged_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_outcome_facts >= 0),
    observation_sha256 TEXT NOT NULL DEFAULT '',
    root_epoch BIGINT NOT NULL DEFAULT 0 CHECK (root_epoch >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    CHECK (
        (status = 'RESERVED' AND settled_at IS NULL AND observation_sha256 = '' AND root_epoch = 0)
        OR (status = 'SETTLED' AND settled_at IS NOT NULL AND observation_sha256 ~ '^[0-9a-f]{64}$')
        OR (status = 'RELEASED' AND settled_at IS NOT NULL AND observation_sha256 = '' AND root_epoch = 0)
    )
);
CREATE INDEX v4_query_exposure_root_idx
    ON v4_query_exposure_reservations(root_task_id, created_at, query_id);

CREATE TABLE v4_observations (
    observation_sha256 TEXT PRIMARY KEY CHECK (observation_sha256 ~ '^[0-9a-f]{64}$'),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v4'),
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    release_set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    influence_set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    outcome_set_sha256 TEXT NOT NULL REFERENCES v4_bitmap_sets(set_sha256),
    actual_release_facts BIGINT NOT NULL CHECK (actual_release_facts >= 0),
    actual_influence_facts BIGINT NOT NULL CHECK (actual_influence_facts >= 0),
    actual_outcome_facts BIGINT NOT NULL CHECK (actual_outcome_facts >= 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE v4_root_observations (
    root_task_id TEXT NOT NULL REFERENCES v4_exposure_root_heads(root_task_id),
    observation_sha256 TEXT NOT NULL REFERENCES v4_observations(observation_sha256),
    first_query_id TEXT NOT NULL REFERENCES query_records(id),
    first_epoch BIGINT NOT NULL CHECK (first_epoch >= 0),
    first_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (root_task_id, observation_sha256)
);

CREATE TABLE v4_query_observations (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    root_task_id TEXT NOT NULL REFERENCES v4_exposure_root_heads(root_task_id),
    observation_sha256 TEXT NOT NULL REFERENCES v4_observations(observation_sha256),
    root_epoch BIGINT NOT NULL CHECK (root_epoch >= 0),
    charged_release_facts BIGINT NOT NULL CHECK (charged_release_facts >= 0),
    charged_influence_facts BIGINT NOT NULL CHECK (charged_influence_facts >= 0),
    charged_outcome_facts BIGINT NOT NULL CHECK (charged_outcome_facts >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (root_task_id, observation_sha256)
        REFERENCES v4_root_observations(root_task_id, observation_sha256)
);
CREATE INDEX v4_query_observations_root_idx
    ON v4_query_observations(root_task_id, observation_sha256, query_id);

-- A materialization is published only by the same transaction that completes
-- its source query and observation.  It is a cache index, not accounting
-- evidence, so explicit eviction is allowed; updates are not.
CREATE TABLE v4_committed_materializations (
    cache_key_sha256 TEXT PRIMARY KEY CHECK (cache_key_sha256 ~ '^[0-9a-f]{64}$'),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES v4_exposure_root_heads(root_task_id),
    source_query_id TEXT NOT NULL REFERENCES query_records(id),
    observation_sha256 TEXT NOT NULL REFERENCES v4_observations(observation_sha256),
    dictionary_set_digest TEXT NOT NULL REFERENCES v4_dictionary_sets(dictionary_set_digest),
    grant_digest TEXT NOT NULL CHECK (grant_digest ~ '^[0-9a-f]{64}$'),
    catalog_digest TEXT NOT NULL CHECK (catalog_digest ~ '^[0-9a-f]{64}$'),
    result_sha256 TEXT NOT NULL CHECK (result_sha256 ~ '^[0-9a-f]{64}$'),
    row_count BIGINT NOT NULL CHECK (row_count >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    UNIQUE (task_id, source_query_id, cache_key_sha256),
    CHECK (expires_at IS NULL OR expires_at > created_at),
    FOREIGN KEY (root_task_id, observation_sha256)
        REFERENCES v4_root_observations(root_task_id, observation_sha256)
);
CREATE INDEX v4_materializations_lookup_idx
    ON v4_committed_materializations(task_id, grant_digest, catalog_digest,
                                      dictionary_set_digest, cache_key_sha256);

CREATE FUNCTION reject_v4_materialization_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'v4_committed_materializations rows cannot be updated';
END;
$$;
CREATE TRIGGER v4_materializations_no_update
BEFORE UPDATE ON v4_committed_materializations
FOR EACH ROW EXECUTE FUNCTION reject_v4_materialization_update();

-- All content-addressed evidence and membership rows are immutable.  Root
-- heads are intentionally excluded because their epoch is the atomic CAS.
DO $$
DECLARE
    relation_name TEXT;
BEGIN
    FOREACH relation_name IN ARRAY ARRAY[
        'v4_dictionary_manifests', 'v4_dictionary_segments',
        'v4_dictionary_chunks', 'v4_dictionary_segment_chunks',
        'v4_dictionary_artifacts',
        'v4_dictionary_sets', 'v4_dictionary_set_members',
        'v4_dynamic_facts', 'v4_bitmap_containers', 'v4_bitmap_sets',
        'v4_bitmap_set_containers', 'v4_bitmap_set_dynamic_facts',
        'v4_observations', 'v4_root_observations', 'v4_query_observations'
    ] LOOP
        EXECUTE format('CREATE TRIGGER %I_no_update BEFORE UPDATE ON %I '
                       'FOR EACH ROW EXECUTE FUNCTION reject_immutable_change()', relation_name, relation_name);
        EXECUTE format('CREATE TRIGGER %I_no_delete BEFORE DELETE ON %I '
                       'FOR EACH ROW EXECUTE FUNCTION reject_immutable_change()', relation_name, relation_name);
    END LOOP;
END $$;
