-- Retain the per-query V3 observation membership needed by audit-only policy
-- comparisons. exposure_facts remains the root-level novelty ledger; this
-- table records both novel and repeated facts for each settled query.

CREATE TABLE query_exposure_facts (
    root_task_id TEXT NOT NULL,
    query_id TEXT NOT NULL REFERENCES query_records(id),
    ledger_kind TEXT NOT NULL CHECK (ledger_kind IN ('RELEASE', 'INFLUENCE', 'OUTCOME')),
    fact_sha256 TEXT NOT NULL CHECK (length(fact_sha256) = 64),
    PRIMARY KEY (query_id, ledger_kind, fact_sha256),
    FOREIGN KEY (root_task_id, ledger_kind, fact_sha256)
        REFERENCES exposure_facts(root_task_id, ledger_kind, fact_sha256)
);

CREATE INDEX query_exposure_facts_root_idx
    ON query_exposure_facts(root_task_id, ledger_kind, fact_sha256);

CREATE TRIGGER query_exposure_facts_no_update
BEFORE UPDATE ON query_exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();

CREATE TRIGGER query_exposure_facts_no_delete
BEFORE DELETE ON query_exposure_facts
FOR EACH ROW EXECUTE FUNCTION reject_immutable_change();
