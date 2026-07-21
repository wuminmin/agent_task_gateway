#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --set=control_password="$CONTROL_DB_PASSWORD" <<'SQL'
CREATE ROLE gateway_control LOGIN PASSWORD :'control_password';
GRANT CONNECT ON DATABASE :DBNAME TO gateway_control;
GRANT USAGE, CREATE ON SCHEMA public TO gateway_control;
SQL
