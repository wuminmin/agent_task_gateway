\set ON_ERROR_STOP on

BEGIN;

CREATE SCHEMA IF NOT EXISTS reporting;
CREATE SCHEMA final_v5_provsql;

CREATE TABLE final_v5_provsql.orders (
    orderkey bigint PRIMARY KEY,
    status bigint NOT NULL CHECK (status BETWEEN 0 AND 2),
    partition_key integer NOT NULL CHECK (partition_key = 1)
);

CREATE TABLE final_v5_provsql.lineitem (
    orderkey bigint NOT NULL REFERENCES final_v5_provsql.orders(orderkey),
    linenumber integer NOT NULL CHECK (linenumber BETWEEN 1 AND 5),
    extendedprice numeric(12,2) NOT NULL CHECK (extendedprice >= 0),
    partition_key integer NOT NULL CHECK (partition_key = 1),
    PRIMARY KEY (orderkey, linenumber)
);

CREATE TABLE final_v5_provsql.nonce (
    nonce_id bigint PRIMARY KEY,
    partition_key integer NOT NULL CHECK (partition_key = 1)
);

INSERT INTO final_v5_provsql.orders(orderkey,status,partition_key)
SELECT order_key,(order_key % 3)::bigint,1
FROM generate_series(1,50000) AS generated(order_key);

INSERT INTO final_v5_provsql.lineitem(orderkey,linenumber,extendedprice,partition_key)
SELECT order_key,line_number,
       ((((order_key * 13) + (line_number * 7)) % 100000) + 100)::numeric / 100,1
FROM generate_series(1,50000) AS orders(order_key)
CROSS JOIN generate_series(1,5) AS lines(line_number);

INSERT INTO final_v5_provsql.nonce(nonce_id,partition_key)
SELECT nonce_id,1
FROM generate_series(1,1000) AS generated(nonce_id);

CREATE INDEX provsql_lineitem_orderkey_idx ON final_v5_provsql.lineitem(orderkey);

ANALYZE final_v5_provsql.orders;
ANALYZE final_v5_provsql.lineitem;
ANALYZE final_v5_provsql.nonce;

-- Independent, frozen physical publications. TaskGate and the offline
-- snapshot compiler see only these read-only relations; neither can mutate or
-- refresh the underlying benchmark corpus.
CREATE MATERIALIZED VIEW reporting.provsql_orders AS
SELECT orderkey,status,partition_key
FROM final_v5_provsql.orders;

CREATE MATERIALIZED VIEW reporting.provsql_lineitem AS
SELECT orderkey,linenumber,extendedprice,partition_key
FROM final_v5_provsql.lineitem;

CREATE MATERIALIZED VIEW reporting.provsql_nonce AS
SELECT nonce_id,partition_key
FROM final_v5_provsql.nonce;

REVOKE ALL ON SCHEMA final_v5_provsql FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA final_v5_provsql FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;

COMMIT;
