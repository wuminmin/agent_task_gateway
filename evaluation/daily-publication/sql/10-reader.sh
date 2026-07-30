#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=ON_ERROR_STOP=1 --set=reader_password="$DAILY_SNAPSHOT_PASSWORD" <<'SQL'
CREATE ROLE daily_snapshot_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'reader_password';
ALTER ROLE daily_snapshot_reader SET default_transaction_read_only = on;
ALTER ROLE daily_snapshot_reader SET statement_timeout = '10min';
ALTER ROLE daily_snapshot_reader SET search_path = pg_catalog;
GRANT CONNECT ON DATABASE :DBNAME TO daily_snapshot_reader;
GRANT USAGE ON SCHEMA reporting TO daily_snapshot_reader;
GRANT SELECT ON reporting.datasource_attestation,
                reporting.daily_lineitem_day0,
                reporting.daily_lineitem_day1,
                reporting.daily_lineitem_day2,
                reporting.daily_lineitem_day3
TO daily_snapshot_reader;
SQL
