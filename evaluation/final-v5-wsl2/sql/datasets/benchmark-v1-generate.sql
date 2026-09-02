\set ON_ERROR_STOP on

-- Contract stage: execute as PostgreSQL superuser only after the standard
-- Final-V5 init scripts have created the frozen ProvSQL publications,
-- taskgate_snapshot_owner, and the mutation-rejection trigger function.
BEGIN;

DO $preconditions$
DECLARE
    actual_version text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'taskgate_snapshot_owner') THEN
        RAISE EXCEPTION 'taskgate_snapshot_owner is required';
    END IF;
    IF to_regprocedure('taskgate_ordinal.reject_frozen_publication_mutation()') IS NULL THEN
        RAISE EXCEPTION 'frozen-publication mutation guard is required';
    END IF;
    SELECT pg_collation_actual_version(oid)
      INTO actual_version
      FROM pg_collation
     WHERE collname = 'en_US.utf8'
     ORDER BY oid
     LIMIT 1;
    IF actual_version IS DISTINCT FROM '2.36' THEN
        RAISE EXCEPTION 'en_US.utf8 actual collation version is %, expected 2.36', actual_version;
    END IF;
END
$preconditions$;

CREATE SCHEMA final_v5_benchmark;

CREATE TABLE final_v5_benchmark.exposure_scale (
    member_rank bigint PRIMARY KEY CHECK (member_rank BETWEEN 1 AND 414000),
    metric numeric(12,2) NOT NULL CHECK (metric >= 0 AND metric <= 1001.00),
    family_id integer NOT NULL CHECK (family_id = 1),
    partition_key integer NOT NULL CHECK (partition_key = 1)
);

INSERT INTO final_v5_benchmark.exposure_scale(member_rank, metric, family_id, partition_key)
SELECT member_rank,
       ((((member_rank * 13) % 100000) + 100)::numeric / 100),
       1,
       1
FROM generate_series(1, 414000) AS generated(member_rank);

CREATE TABLE final_v5_benchmark.result_heavy (
    row_id bigint PRIMARY KEY CHECK (row_id BETWEEN 1 AND 100000),
    category text COLLATE "en_US.utf8" NOT NULL,
    amount numeric NOT NULL,
    event_date date NOT NULL,
    sequence_no integer NOT NULL,
    approved boolean NOT NULL,
    event_timestamp timestamp without time zone NOT NULL,
    description text COLLATE "en_US.utf8" NOT NULL,
    quantity bigint NOT NULL,
    unit_price numeric NOT NULL,
    tax_amount numeric NOT NULL,
    settled_date date NOT NULL,
    processed_at timestamp without time zone NOT NULL,
    region text COLLATE "en_US.utf8" NOT NULL,
    revision integer NOT NULL,
    active boolean NOT NULL
);

INSERT INTO final_v5_benchmark.result_heavy(
    row_id, category, amount, event_date, sequence_no, approved,
    event_timestamp, description, quantity, unit_price, tax_amount,
    settled_date, processed_at, region, revision, active
)
SELECT row_id,
       (ARRAY['alpha','beta','gamma','delta'])[((row_id - 1) % 4) + 1],
       ((row_id * 7919) % 100000000)::numeric / 100,
       date '2020-01-01' + ((row_id - 1) % 3653)::integer,
       (row_id % 1000000)::integer,
       (row_id % 3) <> 0,
       timestamp '2020-01-01 00:00:00'
           + ((row_id - 1) * interval '1 second')
           + (((row_id - 1) % 1000) * interval '1 microsecond'),
       'artifact-row-' || row_id::text,
       1 + ((row_id - 1) % 10000),
       ((row_id::bigint * 104729) % 10000000)::numeric / 10000,
       CASE WHEN (row_id % 11) = 0 THEN -1 ELSE 1 END
           * (((row_id * 37) % 1000000)::numeric / 100),
       date '2020-01-01' + ((row_id - 1 + 31) % 3653)::integer,
       timestamp '2020-01-01 12:00:00' + ((row_id - 1) * interval '1 minute'),
       (ARRAY['north','south','east','west','central'])[((row_id - 1) % 5) + 1],
       ((row_id - 1) % 97)::integer,
       (row_id % 7) <> 0
FROM generate_series(1, 100000) AS generated(row_id);

-- P9.E scale point: the 1,250,000-row sixteen-field relation lets one
-- admitted SUM-ladder query settle a Dependency footprint above 10^7
-- declared facts (nine facts per surviving row); same formulas as
-- result_heavy with only the row-count bound changed.
CREATE TABLE final_v5_benchmark.scale_e7 (
    row_id bigint PRIMARY KEY CHECK (row_id BETWEEN 1 AND 1250000),
    category text COLLATE "en_US.utf8" NOT NULL,
    amount numeric NOT NULL,
    event_date date NOT NULL,
    sequence_no integer NOT NULL,
    approved boolean NOT NULL,
    event_timestamp timestamp without time zone NOT NULL,
    description text COLLATE "en_US.utf8" NOT NULL,
    quantity bigint NOT NULL,
    unit_price numeric NOT NULL,
    tax_amount numeric NOT NULL,
    settled_date date NOT NULL,
    processed_at timestamp without time zone NOT NULL,
    region text COLLATE "en_US.utf8" NOT NULL,
    revision integer NOT NULL,
    active boolean NOT NULL
);

INSERT INTO final_v5_benchmark.scale_e7(
    row_id, category, amount, event_date, sequence_no, approved,
    event_timestamp, description, quantity, unit_price, tax_amount,
    settled_date, processed_at, region, revision, active
)
SELECT row_id,
       (ARRAY['alpha','beta','gamma','delta'])[((row_id - 1) % 4) + 1],
       ((row_id::bigint * 7919) % 100000000)::numeric / 100,
       date '2020-01-01' + ((row_id - 1) % 3653)::integer,
       (row_id % 1000000)::integer,
       (row_id % 3) <> 0,
       timestamp '2020-01-01 00:00:00'
           + ((row_id - 1) * interval '1 second')
           + (((row_id - 1) % 1000) * interval '1 microsecond'),
       'artifact-row-' || row_id::text,
       1 + ((row_id - 1) % 10000),
       ((row_id::bigint * 104729) % 10000000)::numeric / 10000,
       CASE WHEN (row_id % 11) = 0 THEN -1 ELSE 1 END
           * (((row_id::bigint * 37) % 1000000)::numeric / 100),
       date '2020-01-01' + ((row_id - 1 + 31) % 3653)::integer,
       timestamp '2020-01-01 12:00:00' + ((row_id - 1) * interval '1 minute'),
       (ARRAY['north','south','east','west','central'])[((row_id - 1) % 5) + 1],
       ((row_id - 1) % 97)::integer,
       (row_id % 7) <> 0
FROM generate_series(1, 1250000) AS generated(row_id);

ANALYZE final_v5_benchmark.scale_e7;

ANALYZE final_v5_benchmark.exposure_scale;
ANALYZE final_v5_benchmark.result_heavy;

CREATE MATERIALIZED VIEW reporting.final_v5_exposure_scale AS
SELECT member_rank, metric, family_id, partition_key
FROM final_v5_benchmark.exposure_scale;

CREATE UNIQUE INDEX final_v5_exposure_scale_member_rank_idx
    ON reporting.final_v5_exposure_scale(member_rank);

CREATE MATERIALIZED VIEW reporting.final_v5_result_heavy AS
SELECT row_id, category, amount, event_date, sequence_no, approved,
       event_timestamp, description, quantity, unit_price, tax_amount,
       settled_date, processed_at, region, revision, active
FROM final_v5_benchmark.result_heavy;

CREATE UNIQUE INDEX final_v5_result_heavy_row_id_idx
    ON reporting.final_v5_result_heavy(row_id);

CREATE MATERIALIZED VIEW reporting.final_v5_scale_e7 AS
SELECT row_id, category, amount, event_date, sequence_no, approved,
       event_timestamp, description, quantity, unit_price, tax_amount,
       settled_date, processed_at, region, revision, active
FROM final_v5_benchmark.scale_e7;

CREATE UNIQUE INDEX final_v5_scale_e7_row_id_idx
    ON reporting.final_v5_scale_e7(row_id);

-- Four semantic layers: fixed filter/projection -> connected join ->
-- aggregate -> root projection. No filter is permitted above layer 3.
CREATE VIEW reporting.final_v5_analytics_depth4_l1 AS
SELECT orderkey, linenumber, extendedprice, partition_key
FROM reporting.provsql_lineitem
WHERE partition_key = 1
  AND orderkey <= 5000;

CREATE VIEW reporting.final_v5_analytics_depth4_l2 AS
SELECT o.status, l.extendedprice,
       o.partition_key AS orders_partition_key,
       l.partition_key AS lineitem_partition_key
FROM reporting.final_v5_analytics_depth4_l1 AS l
INNER JOIN reporting.provsql_orders AS o
        ON o.orderkey = l.orderkey
       AND o.partition_key = l.partition_key
WHERE o.partition_key = 1;

CREATE VIEW reporting.final_v5_analytics_depth4_l3 AS
SELECT status,
       sum(extendedprice) AS total_extendedprice,
       count(*) AS line_count,
       orders_partition_key,
       lineitem_partition_key
FROM reporting.final_v5_analytics_depth4_l2
GROUP BY status, orders_partition_key, lineitem_partition_key;

CREATE VIEW reporting.final_v5_analytics_depth4 AS
SELECT status, total_extendedprice, line_count,
       orders_partition_key, lineitem_partition_key
FROM reporting.final_v5_analytics_depth4_l3;

DO $cardinality_checks$
BEGIN
    IF (SELECT count(*) FROM final_v5_benchmark.exposure_scale) <> 414000 THEN
        RAISE EXCEPTION 'exposure-scale row count is not 414000';
    END IF;
    IF (SELECT count(*) FROM final_v5_benchmark.result_heavy) <> 100000 THEN
        RAISE EXCEPTION 'result-heavy row count is not 100000';
    END IF;
    IF (SELECT count(*) FROM final_v5_benchmark.scale_e7) <> 1250000 THEN
        RAISE EXCEPTION 'scale-e7 row count is not 1250000';
    END IF;
    IF (SELECT count(*) FROM reporting.final_v5_analytics_depth4_l1) <> 25000 THEN
        RAISE EXCEPTION 'depth-4 layer-1 row count is not 25000';
    END IF;
    IF (SELECT count(*) FROM reporting.final_v5_analytics_depth4) <> 3 THEN
        RAISE EXCEPTION 'depth-4 root row count is not 3';
    END IF;
END
$cardinality_checks$;

CREATE TRIGGER reject_frozen_exposure_scale_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_benchmark.exposure_scale
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_result_heavy_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_benchmark.result_heavy
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

CREATE TRIGGER reject_frozen_scale_e7_mutation
BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON final_v5_benchmark.scale_e7
FOR EACH STATEMENT EXECUTE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation();

COMMENT ON MATERIALIZED VIEW reporting.final_v5_exposure_scale IS
    'Immutable source relation for Final-V5 publication final-v5-exposure-scale-v1.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_result_heavy IS
    'Immutable source relation for Final-V5 publication final-v5-result-heavy-v1.';
COMMENT ON MATERIALIZED VIEW reporting.final_v5_scale_e7 IS
    'Immutable source relation for Final-V5 publication final-v5-scale-e7-v1.';
COMMENT ON VIEW reporting.final_v5_analytics_depth4 IS
    'Final-V5 four-layer semantic Product root; live View-contract digests must be generated before freeze.';

REVOKE ALL ON SCHEMA final_v5_benchmark FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA final_v5_benchmark FROM PUBLIC;
REVOKE ALL ON reporting.final_v5_exposure_scale, reporting.final_v5_result_heavy,
    reporting.final_v5_analytics_depth4_l1, reporting.final_v5_analytics_depth4_l2,
    reporting.final_v5_analytics_depth4_l3, reporting.final_v5_analytics_depth4 FROM PUBLIC;

ALTER TABLE final_v5_benchmark.exposure_scale OWNER TO taskgate_snapshot_owner;
ALTER TABLE final_v5_benchmark.result_heavy OWNER TO taskgate_snapshot_owner;
ALTER TABLE final_v5_benchmark.scale_e7 OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_exposure_scale OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_result_heavy OWNER TO taskgate_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.final_v5_scale_e7 OWNER TO taskgate_snapshot_owner;
ALTER VIEW reporting.final_v5_analytics_depth4_l1 OWNER TO taskgate_snapshot_owner;
ALTER VIEW reporting.final_v5_analytics_depth4_l2 OWNER TO taskgate_snapshot_owner;
ALTER VIEW reporting.final_v5_analytics_depth4_l3 OWNER TO taskgate_snapshot_owner;
ALTER VIEW reporting.final_v5_analytics_depth4 OWNER TO taskgate_snapshot_owner;
ALTER SCHEMA final_v5_benchmark OWNER TO taskgate_snapshot_owner;

DO $reader_grants$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_reader') THEN
        EXECUTE 'GRANT SELECT ON reporting.final_v5_exposure_scale, reporting.final_v5_result_heavy, reporting.final_v5_scale_e7, reporting.final_v5_analytics_depth4_l1, reporting.final_v5_analytics_depth4_l2, reporting.final_v5_analytics_depth4_l3, reporting.final_v5_analytics_depth4 TO gateway_reader';
    END IF;
END
$reader_grants$;

COMMIT;
