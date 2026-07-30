#!/bin/sh
set -eu

rows=${DAILY_PUBLICATION_ROWS:-2000}
case "$rows" in
  ''|*[!0-9]*)
    echo "DAILY_PUBLICATION_ROWS must be an integer" >&2
    exit 1
    ;;
esac
if [ "$rows" -lt 500 ] || [ "$rows" -gt 345000 ] || [ $((rows % 500)) -ne 0 ]; then
  echo "DAILY_PUBLICATION_ROWS must be a multiple of 500 between 500 and 345000" >&2
  exit 1
fi

base_orders=$((rows / 5))
changed_day1=$((rows / 100))
changed_day2=$((rows * 5 / 100))
changed_day3=$((rows * 10 / 100))
churn_rows=$((rows / 100))
churn_orders=$((churn_rows / 5))
source_orders=$((base_orders + churn_orders))

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=ON_ERROR_STOP=1 \
  --set=base_orders="$base_orders" \
  --set=source_orders="$source_orders" \
  --set=changed_day1="$changed_day1" \
  --set=changed_day2="$changed_day2" \
  --set=changed_day3="$changed_day3" \
  --set=churn_orders="$churn_orders" <<'SQL'
BEGIN;

UPDATE reporting.datasource_attestation
SET datasource_id = 'taskgate-eval-daily-publication'
WHERE singleton;

CREATE TABLE legacy.daily_orders (
    o_orderkey bigint PRIMARY KEY,
    o_orderstatus smallint NOT NULL CHECK (o_orderstatus BETWEEN 0 AND 2)
);

CREATE TABLE legacy.daily_lineitem (
    dataset_partition smallint NOT NULL DEFAULT 1 CHECK (dataset_partition = 1),
    l_orderkey bigint NOT NULL REFERENCES legacy.daily_orders(o_orderkey),
    l_linenumber integer NOT NULL CHECK (l_linenumber BETWEEN 1 AND 5),
    l_extendedprice numeric(12,2) NOT NULL CHECK (l_extendedprice >= 0),
    PRIMARY KEY (l_orderkey, l_linenumber)
);

INSERT INTO legacy.daily_orders (o_orderkey, o_orderstatus)
SELECT order_key, (order_key % 3)::smallint
FROM generate_series(1, :source_orders) AS generated(order_key);

INSERT INTO legacy.daily_lineitem (l_orderkey, l_linenumber, l_extendedprice)
SELECT order_key,
       line_number,
       ((((order_key * 13) + (line_number * 7)) % 100000) + 100)::numeric / 100
FROM generate_series(1, :source_orders) AS orders(order_key)
CROSS JOIN generate_series(1, 5) AS lines(line_number);

CREATE MATERIALIZED VIEW reporting.daily_lineitem_day0 AS
SELECT dataset_partition, l_orderkey, l_linenumber, l_extendedprice
FROM legacy.daily_lineitem
WHERE l_orderkey <= :base_orders
WITH DATA;

CREATE MATERIALIZED VIEW reporting.daily_lineitem_day1 AS
SELECT dataset_partition,
       l_orderkey,
       l_linenumber,
       CASE WHEN ((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day1
            THEN l_extendedprice + 1.00
            ELSE l_extendedprice
       END::numeric(12,2) AS l_extendedprice
FROM legacy.daily_lineitem
WHERE l_orderkey <= :base_orders
WITH DATA;

CREATE MATERIALIZED VIEW reporting.daily_lineitem_day2 AS
SELECT dataset_partition,
       l_orderkey,
       l_linenumber,
       CASE WHEN ((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day2
            THEN l_extendedprice + 2.00
            ELSE l_extendedprice
       END::numeric(12,2) AS l_extendedprice
FROM legacy.daily_lineitem
WHERE l_orderkey <= :base_orders
WITH DATA;

CREATE MATERIALIZED VIEW reporting.daily_lineitem_day3 AS
SELECT dataset_partition,
       l_orderkey,
       l_linenumber,
       CASE WHEN l_orderkey <= :base_orders
                 AND ((l_orderkey - 1) * 5 + l_linenumber) <= :changed_day3
            THEN l_extendedprice + 3.00
            ELSE l_extendedprice
       END::numeric(12,2) AS l_extendedprice
FROM legacy.daily_lineitem
WHERE l_orderkey <= (:base_orders - :churn_orders)
   OR l_orderkey > :base_orders
WITH DATA;

CREATE UNIQUE INDEX daily_lineitem_day0_key
    ON reporting.daily_lineitem_day0 (l_orderkey, l_linenumber);
CREATE UNIQUE INDEX daily_lineitem_day1_key
    ON reporting.daily_lineitem_day1 (l_orderkey, l_linenumber);
CREATE UNIQUE INDEX daily_lineitem_day2_key
    ON reporting.daily_lineitem_day2 (l_orderkey, l_linenumber);
CREATE UNIQUE INDEX daily_lineitem_day3_key
    ON reporting.daily_lineitem_day3 (l_orderkey, l_linenumber);

ANALYZE reporting.daily_lineitem_day0;
ANALYZE reporting.daily_lineitem_day1;
ANALYZE reporting.daily_lineitem_day2;
ANALYZE reporting.daily_lineitem_day3;

CREATE ROLE taskgate_daily_snapshot_owner
    NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

ALTER MATERIALIZED VIEW reporting.daily_lineitem_day0 OWNER TO taskgate_daily_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.daily_lineitem_day1 OWNER TO taskgate_daily_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.daily_lineitem_day2 OWNER TO taskgate_daily_snapshot_owner;
ALTER MATERIALIZED VIEW reporting.daily_lineitem_day3 OWNER TO taskgate_daily_snapshot_owner;

REVOKE ALL ON SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA legacy FROM PUBLIC;
REVOKE ALL ON SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;

COMMENT ON MATERIALIZED VIEW reporting.daily_lineitem_day0 IS
    'Immutable RQ5 Day0 reporting publication.';
COMMENT ON MATERIALIZED VIEW reporting.daily_lineitem_day1 IS
    'Immutable RQ5 Day1 publication with exactly 1 percent field updates.';
COMMENT ON MATERIALIZED VIEW reporting.daily_lineitem_day2 IS
    'Immutable RQ5 Day2 publication with exactly 5 percent field updates from Day1.';
COMMENT ON MATERIALIZED VIEW reporting.daily_lineitem_day3 IS
    'Immutable RQ5 Day3 publication with exactly 10 percent field updates, 1 percent inserts, and 1 percent deletes.';

COMMIT;
SQL
