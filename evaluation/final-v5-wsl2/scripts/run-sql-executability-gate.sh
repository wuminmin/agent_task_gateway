#!/usr/bin/env bash
# Run the contract SQL-executability gate against a disposable PostgreSQL, with
# a skip treated as a failure.
#
# validate.sh reports SKIPPED when TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is unset,
# and a skip must never count as a pass at freeze time. This script supplies the
# DSN and passes -require-live, so the only outcomes are pass and fail.
#
# The database has to be initialized the way the deployment initializes it, and
# has to be otherwise EMPTY:
#
#   - the generator refuses without taskgate_snapshot_owner and without the
#     frozen-publication mutation guard, both created by db/init;
#   - but it creates schema final_v5_benchmark itself, so running it against a
#     deployment that already ran db/init/20-final-v5-benchmark-dataset.sh fails
#     with "schema already exists".
#
# So every db/init member is applied EXCEPT the benchmark dataset generator,
# which is the artifact under test.
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

CONTAINER="${SQLCHECK_CONTAINER:-taskgate-final-v5-sqlcheck}"
PORT="${SQLCHECK_PORT:-25999}"
# The immutable image the formal measurement contract pins.
IMAGE="postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55"

initdir="$(mktemp -d)"
cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$initdir"
}
trap cleanup EXIT

for member in db/init/*; do
  [[ "$(basename "$member")" == "20-final-v5-benchmark-dataset.sh" ]] && continue
  cp "$member" "$initdir/"
done
# mktemp -d gives mode 0700, which the in-container postgres user cannot
# traverse; the entrypoint then finds no init member and the database comes up
# without the roles and the mutation guard the generator requires.
chmod 755 "$initdir"
chmod 644 "$initdir"/*
chmod +x "$initdir"/*.sh 2>/dev/null || true

# Generated, never written down, never echoed.
password="$(openssl rand -hex 24)"

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$CONTAINER" \
  -e POSTGRES_PASSWORD="$password" \
  -e POSTGRES_DB=travel_demo \
  -e GATEWAY_DB_PASSWORD="$(openssl rand -hex 24)" \
  -v "$initdir:/docker-entrypoint-initdb.d:ro" \
  -p "127.0.0.1:${PORT}:5432" \
  "$IMAGE" \
  postgres -c shared_preload_libraries=pg_stat_statements \
           -c compute_query_id=on \
           -c 'pg_stat_statements.track=all' >/dev/null

for attempt in $(seq 1 60); do
  if docker exec "$CONTAINER" pg_isready -U postgres -d travel_demo >/dev/null 2>&1; then
    sleep 3
    break
  fi
  [[ "$attempt" == 60 ]] && { echo "disposable PostgreSQL never became ready" >&2; exit 1; }
  sleep 2
done

version="$(docker exec "$CONTAINER" psql -U postgres -d travel_demo -tAc 'select version()')"
echo "disposable PostgreSQL: ${version%% on *}"

TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN="postgres://postgres:${password}@127.0.0.1:${PORT}/travel_demo?sslmode=disable" \
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-contract-sql-check -require-live "$@"
