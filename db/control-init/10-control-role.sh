#!/bin/sh
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv control_password CONTROL_DB_PASSWORD
CREATE ROLE gateway_control LOGIN PASSWORD :'control_password';
GRANT CONNECT ON DATABASE :DBNAME TO gateway_control;
GRANT USAGE, CREATE ON SCHEMA public TO gateway_control;
SQL
