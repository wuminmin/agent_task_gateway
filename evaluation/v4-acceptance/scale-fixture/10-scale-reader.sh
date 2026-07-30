#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=gateway_password="$GATEWAY_DB_PASSWORD" <<'SQL'
CREATE ROLE gateway_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'gateway_password';
ALTER ROLE gateway_reader SET default_transaction_read_only = on;
ALTER ROLE gateway_reader SET statement_timeout = '15min';
ALTER ROLE gateway_reader SET search_path = pg_catalog;
GRANT CONNECT ON DATABASE :DBNAME TO gateway_reader;
GRANT USAGE ON SCHEMA reporting TO gateway_reader;
GRANT SELECT ON reporting.datasource_attestation,
                reporting.scale_orders,
                reporting.scale_lineitem
TO gateway_reader;
-- Sidecar tables are installed only after the read-only compiler finishes.
-- The installer grants their enumerated SELECT privileges atomically.
GRANT USAGE ON SCHEMA taskgate_ordinal TO gateway_reader;
SQL
