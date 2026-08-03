#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv gateway_password GATEWAY_DB_PASSWORD
CREATE ROLE gateway_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'gateway_password';
ALTER ROLE gateway_reader SET default_transaction_read_only = on;
ALTER ROLE gateway_reader SET statement_timeout = '5s';
ALTER ROLE gateway_reader SET search_path = pg_catalog;
GRANT CONNECT ON DATABASE :DBNAME TO gateway_reader;
GRANT USAGE ON SCHEMA reporting TO gateway_reader;
GRANT SELECT ON reporting.datasource_attestation, reporting.expense_summary, reporting.expense_detail,
  reporting.final_v5_attack_expense_detail, reporting.final_v5_concurrency_expense_detail, reporting.provsql_orders,
  reporting.provsql_lineitem, reporting.provsql_nonce,
  reporting.final_v5_rls_unlimited_expense_detail, reporting.final_v5_rls_bounded_expense_detail TO gateway_reader;
GRANT USAGE ON SCHEMA taskgate_ordinal TO gateway_reader;
GRANT USAGE ON SCHEMA final_v5_compiler TO gateway_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA final_v5_compiler TO gateway_reader;
SQL
