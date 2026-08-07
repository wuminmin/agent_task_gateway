-- Let an execution binding record that its operation read no exposure ledger.
--
-- Until now every stored binding carried a pre-state, because only exposure v5
-- operations produced one at all. The receipt contract now requires the
-- opposite: every COMPLETED query states which physical statements produced its
-- rows, including a query on a task with no exposure grant. Such an operation
-- reads no ledger -- there is none to read -- and derives its visible row limit
-- from the row budget alone.
--
-- The two pre-state columns therefore become nullable together. NULL is not a
-- relaxation of the old NOT NULL; it is the only honest encoding of "no ledger
-- was read". The alternative, storing a zero-valued ledger document, would be a
-- row asserting that limits and used counts WERE read and were zero, and that
-- assertion would be canonicalized, digested and signed into the receipt as
-- though it had happened.
--
-- The pairing is what is actually constrained: the document and its digest are
-- both present or both absent, so no row can carry a digest naming a document it
-- does not have, or a document nothing names.
--
-- The original inline CHECKs on those two columns are unnamed, so they are found
-- by their definition rather than by a name this migration would have to guess.
DO $$
DECLARE
    doomed TEXT;
BEGIN
    FOR doomed IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'query_execution_bindings'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%exposure_ledger_before%'
    LOOP
        EXECUTE format('ALTER TABLE query_execution_bindings DROP CONSTRAINT %I', doomed);
    END LOOP;
END $$;

ALTER TABLE query_execution_bindings
    ALTER COLUMN exposure_ledger_before_json DROP NOT NULL;

ALTER TABLE query_execution_bindings
    ALTER COLUMN exposure_ledger_before_sha256 DROP NOT NULL;

ALTER TABLE query_execution_bindings
    ADD CONSTRAINT query_execution_bindings_exposure_pre_state_pairing
    CHECK (
        (exposure_ledger_before_json IS NULL) = (exposure_ledger_before_sha256 IS NULL)
        AND (exposure_ledger_before_json IS NULL OR octet_length(exposure_ledger_before_json) > 0)
        AND (exposure_ledger_before_sha256 IS NULL OR length(exposure_ledger_before_sha256) = 64)
    );
