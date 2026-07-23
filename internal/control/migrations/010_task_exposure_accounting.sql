-- Root-task exposure accounting for the TKDE data-semantics model.
-- Existing tasks become singleton families and remain in legacy resource-only
-- mode until a grant explicitly enables both exposure dimensions.

ALTER TABLE tasks ADD COLUMN root_task_id TEXT;
ALTER TABLE tasks ADD COLUMN parent_task_id TEXT;
UPDATE tasks SET root_task_id = id WHERE root_task_id IS NULL;
ALTER TABLE tasks ALTER COLUMN root_task_id SET NOT NULL;
ALTER TABLE tasks ADD CONSTRAINT tasks_root_task_fkey
    FOREIGN KEY (root_task_id) REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE tasks ADD CONSTRAINT tasks_parent_task_fkey
    FOREIGN KEY (parent_task_id) REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE tasks ADD CONSTRAINT tasks_family_shape
    CHECK ((parent_task_id IS NULL AND root_task_id = id) OR parent_task_id IS NOT NULL);
CREATE INDEX tasks_root_task_idx ON tasks(root_task_id, id);
CREATE INDEX tasks_parent_task_idx ON tasks(parent_task_id, id) WHERE parent_task_id IS NOT NULL;

ALTER TABLE task_grants ADD COLUMN max_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (max_release_facts >= 0);
ALTER TABLE task_grants ADD COLUMN max_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (max_influence_facts >= 0);
ALTER TABLE task_grants ADD COLUMN exposure_profile_version TEXT NOT NULL DEFAULT '';
ALTER TABLE task_grants ADD CONSTRAINT task_grants_exposure_shape CHECK (
    (max_release_facts = 0 AND max_influence_facts = 0 AND exposure_profile_version = '')
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND length(btrim(exposure_profile_version)) > 0)
);

CREATE TABLE exposure_ledgers (
    root_task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    profile_version TEXT NOT NULL CHECK (length(btrim(profile_version)) > 0),
    max_release_facts BIGINT NOT NULL CHECK (max_release_facts > 0),
    max_influence_facts BIGINT NOT NULL CHECK (max_influence_facts > 0),
    used_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_release_facts >= 0),
    used_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_influence_facts >= 0),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (used_release_facts <= max_release_facts),
    CHECK (used_influence_facts <= max_influence_facts)
);

CREATE TABLE query_exposure_reservations (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES exposure_ledgers(root_task_id),
    profile_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RESERVED','SETTLED','RELEASED')),
    estimated_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_release_facts >= 0),
    estimated_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_influence_facts >= 0),
    actual_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_release_facts >= 0),
    actual_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_influence_facts >= 0),
    charged_release_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_release_facts >= 0),
    charged_influence_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_influence_facts >= 0),
    observation_sha256 TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ,
    CHECK (
        (status = 'RESERVED' AND settled_at IS NULL AND observation_sha256 = '')
        OR
        (status = 'SETTLED' AND settled_at IS NOT NULL AND length(observation_sha256) = 64)
        OR
        (status = 'RELEASED' AND settled_at IS NOT NULL AND observation_sha256 = '')
    )
);
CREATE INDEX query_exposure_root_idx ON query_exposure_reservations(root_task_id, created_at, query_id);

CREATE TABLE exposure_facts (
    root_task_id TEXT NOT NULL REFERENCES exposure_ledgers(root_task_id),
    ledger_kind TEXT NOT NULL CHECK (ledger_kind IN ('RELEASE','INFLUENCE')),
    fact_sha256 TEXT NOT NULL CHECK (length(fact_sha256) = 64),
    identity_json JSONB NOT NULL,
    first_query_id TEXT NOT NULL REFERENCES query_records(id),
    first_seen_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (root_task_id, ledger_kind, fact_sha256)
);
CREATE INDEX exposure_facts_first_query_idx ON exposure_facts(first_query_id, ledger_kind, fact_sha256);

CREATE TRIGGER exposure_facts_no_update
BEFORE UPDATE ON exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER exposure_facts_no_delete
BEFORE DELETE ON exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
