-- 009_result_observed_db_ms.sql
-- Preserve the raw, untruncated database time the connector reported for a
-- query (before clamping to the reservation), alongside the charged value. The
-- budget ledger invariant still holds via charged_db_ms; this column keeps the
-- observed measurement so accounting quota, observed time, and the physical
-- upper bound stay distinguishable. See TDSC review item #7.
ALTER TABLE query_records
    ADD COLUMN result_db_ms_observed BIGINT NOT NULL DEFAULT 0 CHECK (result_db_ms_observed >= 0);
