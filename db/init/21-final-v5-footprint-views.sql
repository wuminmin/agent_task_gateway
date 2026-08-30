-- Final-V5 refused-footprint ladder publications. Two physically independent
-- immutable TaskGate publications over the deterministic benchmark relation:
-- the unlimited arm accepts every ladder rung, the bounded arm's Dependency
-- budget equals the smallest rung's derived footprint. The 21 prefix is load
-- bearing: reporting.final_v5_result_heavy exists after
-- 20-final-v5-benchmark-dataset.sh and gateway_reader after 10-reader.sh.
CREATE MATERIALIZED VIEW reporting.final_v5_footprint_unlimited_result_heavy AS
SELECT row_id, category, amount, quantity, unit_price, tax_amount
FROM reporting.final_v5_result_heavy;

CREATE MATERIALIZED VIEW reporting.final_v5_footprint_bounded_result_heavy AS
SELECT row_id, category, amount, quantity, unit_price, tax_amount
FROM reporting.final_v5_result_heavy;

GRANT SELECT ON reporting.final_v5_footprint_unlimited_result_heavy,
  reporting.final_v5_footprint_bounded_result_heavy TO gateway_reader;
