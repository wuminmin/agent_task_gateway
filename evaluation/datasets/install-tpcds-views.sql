\set ON_ERROR_STOP on

BEGIN;
CREATE SCHEMA IF NOT EXISTS reporting;

CREATE OR REPLACE VIEW reporting.tpcds_store_sales AS
SELECT ss_sold_date_sk, ss_ticket_number, ss_item_sk, ss_store_sk, ss_quantity, ss_net_paid,
       'all'::text AS eval_scope
FROM public.store_sales;

CREATE OR REPLACE VIEW reporting.tpcds_item AS
SELECT i_item_sk, i_item_id::text AS i_item_id, i_current_price,
       i_category::text AS i_category,
       'all'::text AS eval_scope
FROM public.item;

CREATE OR REPLACE VIEW reporting.tpcds_customer_demographics AS
SELECT cd_demo_sk, cd_gender::text AS cd_gender,
       cd_marital_status::text AS cd_marital_status,
       'all'::text AS eval_scope
FROM public.customer_demographics;

REVOKE ALL ON SCHEMA reporting FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA reporting FROM PUBLIC;
COMMIT;
