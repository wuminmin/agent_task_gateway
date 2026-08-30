-- Final-V5 refused-footprint ladder publications. Two physically independent
-- immutable TaskGate publications over the deterministic benchmark relation:
-- the unlimited arm accepts every ladder rung, the bounded arm's Dependency
-- budget equals the smallest rung's derived footprint. The 21 prefix is load
-- bearing: reporting.final_v5_result_heavy exists after
-- 20-final-v5-benchmark-dataset.sh and gateway_reader after 10-reader.sh.
-- Harnesses that deliberately exclude the benchmark generator (the
-- SQL-executability gate initializes every db/init member EXCEPT 20, because
-- the generator is its artifact under test) come up without the base
-- relation, so the views are created only when it exists.
DO $taskgate_footprint_views$
BEGIN
    IF to_regclass('reporting.final_v5_result_heavy') IS NULL THEN
        RAISE NOTICE 'reporting.final_v5_result_heavy is absent; skipping footprint ladder views';
        RETURN;
    END IF;
    EXECUTE 'CREATE MATERIALIZED VIEW reporting.final_v5_footprint_unlimited_result_heavy AS
        SELECT row_id, category, amount, quantity, unit_price, tax_amount
        FROM reporting.final_v5_result_heavy';
    EXECUTE 'CREATE MATERIALIZED VIEW reporting.final_v5_footprint_bounded_result_heavy AS
        SELECT row_id, category, amount, quantity, unit_price, tax_amount
        FROM reporting.final_v5_result_heavy';
    EXECUTE 'GRANT SELECT ON reporting.final_v5_footprint_unlimited_result_heavy,
        reporting.final_v5_footprint_bounded_result_heavy TO gateway_reader';
END
$taskgate_footprint_views$;
