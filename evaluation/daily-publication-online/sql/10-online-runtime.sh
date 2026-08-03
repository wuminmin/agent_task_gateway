#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  --set=ON_ERROR_STOP=1 <<'SQL'
\getenv gateway_password DAILY_GATEWAY_DB_PASSWORD
BEGIN;

CREATE ROLE taskgate_snapshot_owner
  NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

CREATE ROLE gateway_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'gateway_password';
ALTER ROLE gateway_reader SET default_transaction_read_only = on;
ALTER ROLE gateway_reader SET statement_timeout = '30s';
ALTER ROLE gateway_reader SET search_path = pg_catalog;

CREATE SCHEMA taskgate_ordinal AUTHORIZATION taskgate_snapshot_owner;
CREATE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $taskgate_reject_mutation$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = format(
            'TaskGate frozen publication relation %I.%I rejects %s',
            TG_TABLE_SCHEMA,
            TG_TABLE_NAME,
            TG_OP
        );
END
$taskgate_reject_mutation$;

ALTER FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()
  OWNER TO taskgate_snapshot_owner;
REVOKE ALL ON FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() FROM PUBLIC;

GRANT CONNECT ON DATABASE taskgate_daily TO gateway_reader;
GRANT USAGE ON SCHEMA reporting, taskgate_ordinal TO gateway_reader;
GRANT SELECT ON reporting.datasource_attestation,
                reporting.daily_lineitem_day0,
                reporting.daily_lineitem_day1,
                reporting.daily_lineitem_day2,
                reporting.daily_lineitem_day3
TO gateway_reader;

REVOKE CREATE ON SCHEMA reporting, taskgate_ordinal FROM PUBLIC, gateway_reader;

COMMIT;
SQL
