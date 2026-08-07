-- Narrow the stored execution-binding version to the one this build writes.
--
-- 020 admitted two versions because two existed. Only one does now: the Gateway
-- prepares through internal/physicalquery and signs a QueryExecutionBindingV2,
-- and no code path in this build produces or reads anything else.
--
-- The constraint is narrowed rather than left permissive because a column whose
-- accepted set is wider than the decoder's is a column that can hold a row the
-- application must then refuse at read time -- which is a failure discovered
-- when a receipt is about to be signed, rather than when the row was written.
-- Keeping the two in step makes the write the point of failure.
--
-- There is no upgrade path for a row naming the older version, and none is
-- wanted: every formal test and experiment starts from fresh volumes, so such a
-- row can only come from a database this build did not write.
ALTER TABLE query_execution_bindings
    DROP CONSTRAINT IF EXISTS query_execution_bindings_binding_version_check;

ALTER TABLE query_execution_bindings
    ADD CONSTRAINT query_execution_bindings_binding_version_check
    CHECK (binding_version = 'taskgate-query-execution-binding-v2');
