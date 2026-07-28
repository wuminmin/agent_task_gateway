-- V3 adds a proposition/outcome ledger while preserving all V1/V2 rows.
-- Release and influence facts keep their V2 semantic identity; only outcome
-- facts use the new profile and ledger kind.

ALTER TABLE task_grants
    ADD COLUMN max_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (max_outcome_facts >= 0);
ALTER TABLE task_grants DROP CONSTRAINT task_grants_exposure_shape;
ALTER TABLE task_grants ADD CONSTRAINT task_grants_exposure_shape CHECK (
    (max_release_facts = 0 AND max_influence_facts = 0 AND max_outcome_facts = 0 AND exposure_profile_version = '')
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts = 0
        AND exposure_profile_version IN ('taskgate-exposure-v1', 'taskgate-exposure-v2'))
    OR
    (max_release_facts > 0 AND max_influence_facts > 0 AND max_outcome_facts > 0
        AND exposure_profile_version = 'taskgate-exposure-v3')
);

ALTER TABLE exposure_ledgers
    ADD COLUMN max_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (max_outcome_facts >= 0),
    ADD COLUMN used_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (used_outcome_facts >= 0),
    ADD CHECK (used_outcome_facts <= max_outcome_facts);

ALTER TABLE query_exposure_reservations
    ADD COLUMN estimated_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (estimated_outcome_facts >= 0),
    ADD COLUMN actual_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (actual_outcome_facts >= 0),
    ADD COLUMN charged_outcome_facts BIGINT NOT NULL DEFAULT 0 CHECK (charged_outcome_facts >= 0);

ALTER TABLE exposure_facts DROP CONSTRAINT exposure_facts_ledger_kind_check;
ALTER TABLE exposure_facts ADD CONSTRAINT exposure_facts_ledger_kind_check
    CHECK (ledger_kind IN ('RELEASE', 'INFLUENCE', 'OUTCOME'));
