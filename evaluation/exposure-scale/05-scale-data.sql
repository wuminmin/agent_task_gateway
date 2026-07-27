BEGIN;

UPDATE reporting.datasource_attestation
SET datasource_id = 'taskgate-eval-exposure-scale'
WHERE singleton;

CREATE TABLE legacy.scale_orders (
    dataset_partition smallint NOT NULL DEFAULT 1 CHECK (dataset_partition = 1),
    o_orderkey bigint PRIMARY KEY,
    o_orderstatus smallint NOT NULL CHECK (o_orderstatus BETWEEN 0 AND 2)
);

CREATE TABLE legacy.scale_lineitem (
    dataset_partition smallint NOT NULL DEFAULT 1 CHECK (dataset_partition = 1),
    l_orderkey bigint NOT NULL REFERENCES legacy.scale_orders(o_orderkey),
    l_linenumber integer NOT NULL CHECK (l_linenumber BETWEEN 1 AND 5),
    l_extendedprice numeric(12,2) NOT NULL CHECK (l_extendedprice >= 0),
    PRIMARY KEY (l_orderkey, l_linenumber)
);

INSERT INTO legacy.scale_orders (o_orderkey, o_orderstatus)
SELECT order_key, (order_key % 3)::smallint
FROM generate_series(1, 50000) AS generated(order_key);

INSERT INTO legacy.scale_lineitem (l_orderkey, l_linenumber, l_extendedprice)
SELECT order_key,
       line_number,
       ((((order_key * 13) + (line_number * 7)) % 100000) + 100)::numeric / 100
FROM generate_series(1, 50000) AS orders(order_key)
CROSS JOIN generate_series(1, 5) AS lines(line_number);

CREATE INDEX scale_lineitem_orderkey_idx ON legacy.scale_lineitem(l_orderkey);

CREATE VIEW reporting.scale_orders AS
SELECT dataset_partition, o_orderkey, o_orderstatus
FROM legacy.scale_orders;

CREATE VIEW reporting.scale_lineitem AS
SELECT dataset_partition, l_orderkey, l_linenumber, l_extendedprice
FROM legacy.scale_lineitem;

ANALYZE legacy.scale_orders;
ANALYZE legacy.scale_lineitem;

COMMIT;
