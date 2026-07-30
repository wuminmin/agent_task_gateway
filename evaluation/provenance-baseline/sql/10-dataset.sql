\set ON_ERROR_STOP on

BEGIN;

CREATE SCHEMA benchmark;

CREATE TABLE benchmark.orders (
    o_orderkey bigint PRIMARY KEY,
    o_orderstatus smallint NOT NULL CHECK (o_orderstatus BETWEEN 0 AND 2)
);

CREATE TABLE benchmark.lineitem (
    l_orderkey bigint NOT NULL REFERENCES benchmark.orders(o_orderkey),
    l_linenumber integer NOT NULL CHECK (l_linenumber BETWEEN 1 AND 5),
    l_extendedprice numeric(12,2) NOT NULL CHECK (l_extendedprice >= 0),
    PRIMARY KEY (l_orderkey, l_linenumber)
);

-- One real tracked dependency per execution gives each measured query a novel
-- circuit while leaving its visible aggregates unchanged.
CREATE TABLE benchmark.provenance_nonce (
    nonce_id bigint PRIMARY KEY
);

INSERT INTO benchmark.orders (o_orderkey, o_orderstatus)
SELECT order_key, (order_key % 3)::smallint
FROM generate_series(1, 50000) AS generated(order_key);

INSERT INTO benchmark.lineitem (l_orderkey, l_linenumber, l_extendedprice)
SELECT order_key,
       line_number,
       ((((order_key * 13) + (line_number * 7)) % 100000) + 100)::numeric / 100
FROM generate_series(1, 50000) AS orders(order_key)
CROSS JOIN generate_series(1, 5) AS lines(line_number);

INSERT INTO benchmark.provenance_nonce (nonce_id)
SELECT nonce_id
FROM generate_series(1, 1000) AS generated(nonce_id);

CREATE INDEX lineitem_orderkey_idx ON benchmark.lineitem(l_orderkey);

ANALYZE benchmark.orders;
ANALYZE benchmark.lineitem;
ANALYZE benchmark.provenance_nonce;

COMMIT;
