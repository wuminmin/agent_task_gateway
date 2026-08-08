#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EXPOSURE_EVAL_IMAGE:-taskgate-exposure-evaluation:local}
BUILD_IMAGE=${TASKGATE_EXPOSURE_BUILD_IMAGE:-taskgate-exposure-evaluation-build:local}
CERTIFIED_POSTGRES_IMAGE=postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55
POSTGRES_IMAGE=${TASKGATE_EXPOSURE_POSTGRES_IMAGE:-$CERTIFIED_POSTGRES_IMAGE}
RUN_SUFFIX="$$"
POSTGRES_CONTAINER="taskgate-exposure-oracle-${RUN_SUFFIX}"
POSTGRES_NETWORK="taskgate-exposure-oracle-${RUN_SUFFIX}"
POSTGRES_PASSWORD="taskgate-exposure-oracle-local"
POSTGRES_DATABASE="travel_demo"

command -v docker >/dev/null 2>&1 || {
  echo "exposure evaluation failed: Docker is required" >&2
  exit 1
}
[ "$POSTGRES_IMAGE" = "$CERTIFIED_POSTGRES_IMAGE" ] || {
  echo "exposure evaluation failed: PostgreSQL image is not the certified 16.14 digest" >&2
  exit 1
}
docker info >/dev/null 2>&1 || {
  echo "exposure evaluation failed: Docker daemon is unavailable" >&2
  exit 1
}

docker build --file "$SCRIPT_DIR/Dockerfile" --target build --tag "$BUILD_IMAGE" "$ROOT_DIR"
docker build --file "$SCRIPT_DIR/Dockerfile" --target exposure --tag "$IMAGE" "$ROOT_DIR"
tmp=$(mktemp /tmp/taskgate-exposure-eval.XXXXXX)
integration_tmp=$(mktemp /tmp/taskgate-exposure-integration.XXXXXX)
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$status" -ne 0 ]; then
    if [ -s "$integration_tmp" ]; then
      echo "exposure evaluation failed: retained RQ3 integration output follows" >&2
      cat "$integration_tmp" >&2
    fi
    docker inspect --format \
      'PostgreSQL oracle state: running={{.State.Running}} exit_code={{.State.ExitCode}} oom_killed={{.State.OOMKilled}} error={{json .State.Error}}' \
      "$POSTGRES_CONTAINER" >&2 2>/dev/null || true
    docker logs "$POSTGRES_CONTAINER" >&2 2>/dev/null || true
  fi
  docker rm --force "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$POSTGRES_NETWORK" >/dev/null 2>&1 || true
  rm -f "$tmp"
  rm -f "$integration_tmp"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

docker network create "$POSTGRES_NETWORK" >/dev/null
docker run --detach --name "$POSTGRES_CONTAINER" --network "$POSTGRES_NETWORK" \
  --env "POSTGRES_PASSWORD=$POSTGRES_PASSWORD" --env "POSTGRES_DB=$POSTGRES_DATABASE" \
  --env "GATEWAY_DB_PASSWORD=$POSTGRES_PASSWORD" \
  --volume "$ROOT_DIR/db/init:/docker-entrypoint-initdb.d:ro" \
  --volume "$ROOT_DIR/evaluation/final-v5-wsl2/sql/datasets:/opt/taskgate/final-v5-sql:ro" \
  "$POSTGRES_IMAGE" >/dev/null

attempt=0
until docker run --rm --network "$POSTGRES_NETWORK" --entrypoint pg_isready \
  "$POSTGRES_IMAGE" --host "$POSTGRES_CONTAINER" --port 5432 \
  --username postgres --dbname "$POSTGRES_DATABASE" >/dev/null 2>&1; do
  if [ "$(docker inspect --format '{{.State.Running}}' "$POSTGRES_CONTAINER" 2>/dev/null || true)" != true ]; then
    exit_code=$(docker inspect --format '{{.State.ExitCode}}' "$POSTGRES_CONTAINER" 2>/dev/null || echo unknown)
    echo "exposure evaluation failed: PostgreSQL oracle exited before final-server readiness (exit_code=$exit_code)" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    echo "exposure evaluation failed: PostgreSQL oracle did not reach final-server readiness" >&2
    exit 1
  fi
  sleep 3
done

server_version_num=$(docker exec "$POSTGRES_CONTAINER" psql --username postgres \
  --dbname "$POSTGRES_DATABASE" --tuples-only --no-align \
  --command 'SHOW server_version_num')
[ "$server_version_num" = 160014 ] || {
  echo "exposure evaluation failed: PostgreSQL server_version_num=$server_version_num, want 160014" >&2
  exit 1
}

integration_command="go test -race -json -count=1 -run ^(TestDelegatedTasksShareRootAccountingState|TestConcurrentTaskFamilySettlementCannotOverspend|TestRelationalOnlinePathAgainstPostgreSQL|TestRelationalGatewayEndToEndAgainstPostgreSQL|TestExposureV3ChargesDistinctZeroResultPredicates)$ ./internal/control ./internal/gateway"
set +e
docker run --rm --network "$POSTGRES_NETWORK" \
  --env "CONTROL_TEST_POSTGRES_DSN=postgres://postgres:${POSTGRES_PASSWORD}@${POSTGRES_CONTAINER}:5432/${POSTGRES_DATABASE}?sslmode=disable" \
  --env "BUSINESS_TEST_POSTGRES_DSN=postgres://gateway_reader:${POSTGRES_PASSWORD}@${POSTGRES_CONTAINER}:5432/${POSTGRES_DATABASE}?sslmode=disable" \
  "$BUILD_IMAGE" go test -race -json -count=1 \
  -run '^(TestDelegatedTasksShareRootAccountingState|TestConcurrentTaskFamilySettlementCannotOverspend|TestRelationalOnlinePathAgainstPostgreSQL|TestRelationalGatewayEndToEndAgainstPostgreSQL|TestExposureV3ChargesDistinctZeroResultPredicates)$' \
  ./internal/control ./internal/gateway >"$integration_tmp" 2>&1
integration_status=$?
set -e

docker run --rm --network "$POSTGRES_NETWORK" \
  --env "EXPOSURE_TEST_POSTGRES_DSN=postgres://postgres:${POSTGRES_PASSWORD}@${POSTGRES_CONTAINER}:5432/${POSTGRES_DATABASE}?sslmode=disable" \
  "$IMAGE" >"$tmp"

raw_log="$SCRIPT_DIR/exposure/raw/rq3-postgres-go-test.jsonl"
artifact="$SCRIPT_DIR/exposure/rq3-integration.json"
mkdir -p "$SCRIPT_DIR/exposure/raw"
cp "$integration_tmp" "$raw_log"
go_version=$(docker run --rm "$BUILD_IMAGE" go version)
python3 "$SCRIPT_DIR/exposure/record_integration.py" \
  --report "$tmp" --log "$raw_log" --artifact "$artifact" \
  --output "$SCRIPT_DIR/exposure/results.json" --exit-code "$integration_status" \
  --command "$integration_command" --go-version "$go_version"
cat "$SCRIPT_DIR/exposure/results.json"
