\set ON_ERROR_STOP on

BEGIN;
CREATE SCHEMA IF NOT EXISTS reporting;

CREATE OR REPLACE VIEW reporting.tpch_orders AS
SELECT o_orderkey, o_custkey, o_orderstatus, o_totalprice, o_orderdate,
       o_orderpriority::text AS o_orderpriority, 'all'::text AS eval_scope
FROM public.orders;

CREATE OR REPLACE VIEW reporting.tpch_customer AS
SELECT c_custkey, c_name::text AS c_name, c_mktsegment::text AS c_mktsegment, c_acctbal,
       'all'::text AS eval_scope
FROM public.customer;

CREATE OR REPLACE VIEW reporting.tpch_lineitem AS
SELECT l_orderkey, l_linenumber, l_quantity, l_extendedprice, l_shipdate,
       l_shipmode::text AS l_shipmode,
       'all'::text AS eval_scope
FROM public.lineitem;

REVOKE ALL ON SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;
COMMIT;
