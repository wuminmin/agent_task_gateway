-- Task-scoped canonical view bindings. Empty digest values preserve the
-- historical V1 grant/query representation; every Phase-B binding is a
-- lowercase SHA-256 digest with immutable content-addressed evidence.

ALTER TABLE task_grants
    ADD COLUMN view_binding_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE task_grants
    ADD CONSTRAINT task_grants_view_binding_digest_shape CHECK (
        view_binding_digest = '' OR view_binding_digest ~ '^[0-9a-f]{64}$'
    );

ALTER TABLE query_records
    ADD COLUMN view_binding_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE query_records
    ADD CONSTRAINT query_records_view_binding_digest_shape CHECK (
        view_binding_digest = '' OR view_binding_digest ~ '^[0-9a-f]{64}$'
    );

-- CREATE VIEW expanded SELECT * at migration 001 time, so refresh the
-- compatibility view explicitly after extending task_grants.
CREATE OR REPLACE VIEW grants AS SELECT * FROM task_grants;

CREATE TABLE view_binding_sets (
    digest TEXT PRIMARY KEY CHECK (digest ~ '^[0-9a-f]{64}$'),
    profile_version TEXT NOT NULL CHECK (length(btrim(profile_version)) > 0),
    -- BYTEA preserves the exact canonical bytes covered by the domain-separated
    -- digest; JSONB would reorder/normalize them on write.
    canonical_json BYTEA NOT NULL CHECK (octet_length(canonical_json) > 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE task_view_dependencies (
    task_id TEXT NOT NULL REFERENCES task_grants(task_id),
    binding_digest TEXT NOT NULL REFERENCES view_binding_sets(digest),
    product TEXT NOT NULL CHECK (length(btrim(product)) > 0),
    dependency_key TEXT NOT NULL CHECK (length(btrim(dependency_key)) > 0),
    PRIMARY KEY (task_id, product, dependency_key)
);
CREATE INDEX task_view_dependencies_reverse_idx
    ON task_view_dependencies(dependency_key, task_id, product);

CREATE TABLE task_view_binding_status (
    task_id TEXT PRIMARY KEY REFERENCES task_grants(task_id),
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'REQUIRE_REBIND')),
    bound_digest TEXT NOT NULL REFERENCES view_binding_sets(digest),
    observed_digest TEXT NOT NULL DEFAULT '',
    detected_at TIMESTAMPTZ,
    CHECK (
        (status = 'ACTIVE' AND observed_digest = '' AND detected_at IS NULL)
        OR
        (status = 'REQUIRE_REBIND'
            AND observed_digest ~ '^[0-9a-f]{64}$'
            AND observed_digest <> bound_digest
            AND detected_at IS NOT NULL)
    )
);

CREATE TRIGGER view_binding_sets_no_update
BEFORE UPDATE ON view_binding_sets
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER view_binding_sets_no_delete
BEFORE DELETE ON view_binding_sets
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER task_view_dependencies_no_update
BEFORE UPDATE ON task_view_dependencies
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER task_view_dependencies_no_delete
BEFORE DELETE ON task_view_dependencies
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

-- Dependency rows are assembled before the ACTIVE status row in the same
-- grant transaction. Once that status exists, the task-scoped dependency set
-- is closed to append as well as update/delete.
CREATE FUNCTION enforce_task_view_dependency_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    grant_digest TEXT;
BEGIN
    SELECT view_binding_digest INTO grant_digest
    FROM task_grants WHERE task_id = NEW.task_id;

    IF grant_digest IS NULL OR grant_digest = '' OR grant_digest <> NEW.binding_digest THEN
        RAISE EXCEPTION 'task view dependency disagrees with grant binding';
    END IF;
    IF EXISTS (SELECT 1 FROM task_view_binding_status WHERE task_id = NEW.task_id) THEN
        RAISE EXCEPTION 'task view dependency set is already sealed';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER task_view_dependencies_validate_insert
BEFORE INSERT ON task_view_dependencies
FOR EACH ROW EXECUTE FUNCTION enforce_task_view_dependency_insert();

CREATE FUNCTION enforce_task_view_binding_status_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    grant_digest TEXT;
BEGIN
    SELECT view_binding_digest INTO grant_digest
    FROM task_grants WHERE task_id = NEW.task_id;

    IF NEW.status <> 'ACTIVE' OR NEW.observed_digest <> '' OR NEW.detected_at IS NOT NULL
       OR grant_digest IS NULL OR grant_digest = '' OR grant_digest <> NEW.bound_digest THEN
        RAISE EXCEPTION 'initial task view binding status disagrees with grant binding';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM task_view_dependencies
        WHERE task_id = NEW.task_id AND binding_digest = NEW.bound_digest
    ) THEN
        RAISE EXCEPTION 'task view binding requires dependency evidence';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER task_view_binding_status_validate_insert
BEFORE INSERT ON task_view_binding_status
FOR EACH ROW EXECUTE FUNCTION enforce_task_view_binding_status_insert();

CREATE FUNCTION enforce_task_view_binding_status_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'task_view_binding_status rows are retained as semantic-drift evidence';
    END IF;

    IF NEW.task_id IS DISTINCT FROM OLD.task_id
       OR NEW.bound_digest IS DISTINCT FROM OLD.bound_digest THEN
        RAISE EXCEPTION 'task view binding identity is immutable';
    END IF;

    IF OLD.status = 'ACTIVE'
       AND NEW.status = 'REQUIRE_REBIND'
       AND OLD.observed_digest = ''
       AND OLD.detected_at IS NULL
       AND NEW.observed_digest ~ '^[0-9a-f]{64}$'
       AND NEW.observed_digest <> OLD.bound_digest
       AND NEW.detected_at IS NOT NULL THEN
        RETURN NEW;
    END IF;

    RAISE EXCEPTION 'task view binding status transition is immutable or invalid';
END;
$$;

CREATE TRIGGER task_view_binding_status_transition
BEFORE UPDATE OR DELETE ON task_view_binding_status
FOR EACH ROW EXECUTE FUNCTION enforce_task_view_binding_status_transition();

-- The original trigger enumerates immutable reservation identity fields while
-- allowing the one RESERVED -> terminal settlement update. Extend that list
-- so a live reservation can never be rebound to another view semantic digest.
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
       OR NEW.view_binding_digest IS DISTINCT FROM OLD.view_binding_digest
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
