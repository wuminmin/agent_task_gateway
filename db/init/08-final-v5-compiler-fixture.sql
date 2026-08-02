BEGIN;

-- This tiny deterministic dataset is an execution oracle for the final V5
-- View compiler experiment. Compiler timing uses pure in-memory registries;
-- PostgreSQL execution is outside that boundary and proves that direct SQL and
-- the compiled QueryPlan return the same multiset.
CREATE SCHEMA final_v5_compiler;

DO $compiler_fixture$
DECLARE
    fixture_index integer;
    fixture_table text;
    first_parent integer;
    second_parent integer;
BEGIN
    FOR fixture_index IN 0..16 LOOP
        fixture_table := format('compiler_p%s', lpad(fixture_index::text, 2, '0'));
        first_parent := CASE WHEN fixture_index = 0 THEN 0 ELSE fixture_index END;
        second_parent := CASE WHEN fixture_index = 0 THEN 0 ELSE 100 + fixture_index END;

        EXECUTE format(
            'CREATE TABLE final_v5_compiler.%I (' ||
            'id integer PRIMARY KEY, parent_id integer NOT NULL, ' ||
            'tenant_id integer NOT NULL, value integer NOT NULL)',
            fixture_table
        );
        EXECUTE format(
            'INSERT INTO final_v5_compiler.%I(id,parent_id,tenant_id,value) ' ||
            'VALUES (%s,%s,7,%s),(%s,%s,8,%s)',
            fixture_table,
            fixture_index + 1,
            first_parent,
            100 + fixture_index,
            101 + fixture_index,
            second_parent,
            200 + fixture_index
        );
        EXECUTE format(
            'ALTER TABLE final_v5_compiler.%I OWNER TO taskgate_snapshot_owner',
            fixture_table
        );
    END LOOP;
END
$compiler_fixture$;

ALTER SCHEMA final_v5_compiler OWNER TO taskgate_snapshot_owner;
REVOKE ALL ON SCHEMA final_v5_compiler FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA final_v5_compiler FROM PUBLIC;

COMMENT ON SCHEMA final_v5_compiler IS
    'Immutable source-controlled correctness fixture for the final V5 View compiler experiment.';

COMMIT;
