CREATE TABLE principals (
    id TEXT PRIMARY KEY,
    subject TEXT NOT NULL UNIQUE,
    role TEXT NOT NULL,
    token_hash TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ
);

CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(id),
    objective TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('AWAITING_SUBMISSION','AWAITING_APPROVAL','ACTIVE','ARCHIVED')),
    terminal_reason TEXT NOT NULL DEFAULT '',
    catalog_version TEXT NOT NULL,
    sensitivity TEXT NOT NULL DEFAULT '',
    requested_budget_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    request_context_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    approval_ref TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ
);
CREATE INDEX tasks_principal_state_idx ON tasks(principal_id, state, id);

CREATE TABLE task_grants (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    subject TEXT NOT NULL,
    purpose TEXT NOT NULL,
    approved_products_json JSONB NOT NULL,
    approved_columns_json JSONB NOT NULL,
    mandatory_scope_json JSONB NOT NULL,
    sensitivity_ceiling TEXT NOT NULL,
    max_queries BIGINT NOT NULL CHECK (max_queries >= 0),
    max_rows BIGINT NOT NULL CHECK (max_rows >= 0),
    max_db_ms BIGINT NOT NULL CHECK (max_db_ms >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    catalog_version TEXT NOT NULL,
    approval_receipt TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE VIEW grants AS SELECT * FROM task_grants;

CREATE TABLE approval_events (
    event_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    actor TEXT NOT NULL,
    decision TEXT NOT NULL,
    payload_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX approval_events_task_idx ON approval_events(task_id, created_at, event_id);

CREATE TABLE budget_ledger (
    task_id TEXT PRIMARY KEY REFERENCES tasks(id),
    max_queries BIGINT NOT NULL CHECK (max_queries >= 0),
    max_rows BIGINT NOT NULL CHECK (max_rows >= 0),
    max_db_ms BIGINT NOT NULL CHECK (max_db_ms >= 0),
    used_queries BIGINT NOT NULL DEFAULT 0 CHECK (used_queries >= 0),
    used_rows BIGINT NOT NULL DEFAULT 0 CHECK (used_rows >= 0),
    used_db_ms BIGINT NOT NULL DEFAULT 0 CHECK (used_db_ms >= 0),
    reserved_queries BIGINT NOT NULL DEFAULT 0 CHECK (reserved_queries >= 0),
    reserved_rows BIGINT NOT NULL DEFAULT 0 CHECK (reserved_rows >= 0),
    reserved_db_ms BIGINT NOT NULL DEFAULT 0 CHECK (reserved_db_ms >= 0),
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK (used_queries + reserved_queries <= max_queries),
    CHECK (used_rows + reserved_rows <= max_rows),
    CHECK (used_db_ms + reserved_db_ms <= max_db_ms)
);

CREATE TABLE query_records (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    actor TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    sql_fingerprint TEXT NOT NULL,
    catalog_version TEXT NOT NULL,
    policy_decision TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RESERVED','COMPLETED','RELEASED','INTERRUPTED')),
    reserved_rows BIGINT NOT NULL DEFAULT 0,
    reserved_db_ms BIGINT NOT NULL DEFAULT 0,
    result_rows BIGINT NOT NULL DEFAULT 0,
    result_db_ms BIGINT NOT NULL DEFAULT 0,
    charged_queries BIGINT NOT NULL DEFAULT 0,
    charged_rows BIGINT NOT NULL DEFAULT 0,
    charged_db_ms BIGINT NOT NULL DEFAULT 0,
    budget_before_json JSONB NOT NULL,
    budget_after_json JSONB,
    result_sha256 TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);
CREATE INDEX query_records_task_idx ON query_records(task_id, created_at, id);

CREATE TABLE encrypted_query_results (
    query_id TEXT PRIMARY KEY REFERENCES query_records(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    plaintext_sha256 TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX encrypted_results_task_idx ON encrypted_query_results(task_id, created_at, query_id);
CREATE VIEW encrypted_results AS SELECT * FROM encrypted_query_results;

CREATE TABLE audit_chain_head (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    last_sequence BIGINT NOT NULL,
    last_hash TEXT NOT NULL
);
INSERT INTO audit_chain_head(singleton, last_sequence, last_hash)
VALUES (TRUE, 0, repeat('0', 64));

CREATE TABLE audit_events (
    sequence BIGINT PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    task_id TEXT,
    query_id TEXT,
    actor TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payload_json JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    previous_hash TEXT NOT NULL,
    current_hash TEXT NOT NULL UNIQUE
);
CREATE INDEX audit_events_task_idx ON audit_events(task_id, sequence);
CREATE INDEX audit_events_actor_type_idx ON audit_events(actor, event_type, sequence);

CREATE TABLE callback_idempotency (
    event_id TEXT PRIMARY KEY,
    payload_sha256 TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PROCESSING','COMPLETED','RETRYABLE')),
    response_body BYTEA,
    last_error TEXT NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);

CREATE FUNCTION reject_immutable_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is immutable', TG_TABLE_NAME;
END;
$$;

CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER audit_events_no_delete
BEFORE DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER task_grants_no_update
BEFORE UPDATE ON task_grants
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER task_grants_no_delete
BEFORE DELETE ON task_grants
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
