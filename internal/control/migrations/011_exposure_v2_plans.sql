-- taskgate-exposure-v2 semantic Fact payloads and atomic representation plans.

ALTER TABLE exposure_facts ADD COLUMN canonical_payload BYTEA;

CREATE TABLE representation_plans (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    root_task_id TEXT NOT NULL REFERENCES exposure_ledgers(root_task_id),
    profile_version TEXT NOT NULL CHECK (profile_version = 'taskgate-exposure-v2'),
    planner_version TEXT NOT NULL,
    snapshot_bundle_sha256 TEXT NOT NULL CHECK (length(snapshot_bundle_sha256) = 64),
    candidates_sha256 TEXT NOT NULL CHECK (length(candidates_sha256) = 64),
    candidate_effects_json JSONB NOT NULL,
    selected_json JSONB NOT NULL,
    selected_sha256 TEXT NOT NULL CHECK (length(selected_sha256) = 64),
    union_effect_sha256 TEXT NOT NULL CHECK (length(union_effect_sha256) = 64),
    release_facts BIGINT NOT NULL CHECK (release_facts >= 0),
    influence_facts BIGINT NOT NULL CHECK (influence_facts >= 0),
    utility DOUBLE PRECISION NOT NULL CHECK (utility >= 0),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX representation_plans_root_idx
ON representation_plans(root_task_id, created_at, query_id);

CREATE TRIGGER representation_plans_no_update
BEFORE UPDATE ON representation_plans
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER representation_plans_no_delete
BEFORE DELETE ON representation_plans
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
