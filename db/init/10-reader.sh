#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=gateway_password="$GATEWAY_DB_PASSWORD" <<'SQL'
CREATE ROLE gateway_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'gateway_password';
ALTER ROLE gateway_reader SET default_transaction_read_only = on;
ALTER ROLE gateway_reader SET statement_timeout = '5s';
ALTER ROLE gateway_reader SET search_path = pg_catalog;
GRANT CONNECT ON DATABASE :DBNAME TO gateway_reader;
GRANT USAGE ON SCHEMA reporting TO gateway_reader;
GRANT SELECT ON reporting.expense_summary, reporting.expense_detail TO gateway_reader;
SQL
