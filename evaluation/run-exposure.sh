#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EXPOSURE_EVAL_IMAGE:-taskgate-exposure-evaluation:local}
BUILD_IMAGE=${TASKGATE_EXPOSURE_BUILD_IMAGE:-taskgate-exposure-evaluation-build:local}
POSTGRES_IMAGE=${TASKGATE_EXPOSURE_POSTGRES_IMAGE:-postgres:16-bookworm}
RUN_SUFFIX="$$"
POSTGRES_CONTAINER="taskgate-exposure-oracle-${RUN_SUFFIX}"
POSTGRES_NETWORK="taskgate-exposure-oracle-${RUN_SUFFIX}"
POSTGRES_PASSWORD="taskgate-exposure-oracle-local"
POSTGRES_DATABASE="taskgate_exposure_oracle"

command -v docker >/dev/null 2>&1 || {
  echo "exposure evaluation failed: Docker is required" >&2
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
  "$POSTGRES_IMAGE" >/dev/null

attempt=0
until docker exec "$POSTGRES_CONTAINER" pg_isready --username postgres --dbname "$POSTGRES_DATABASE" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "exposure evaluation failed: PostgreSQL oracle did not become ready" >&2
    exit 1
  fi
  sleep 1
done

integration_command="go test -race -json -count=1 -run ^(TestDelegatedTasksShareRootExposureKnowledge|TestConcurrentTaskFamilySettlementCannotOverspend)$ ./internal/control"
set +e
docker run --rm --network "$POSTGRES_NETWORK" \
  --env "CONTROL_TEST_POSTGRES_DSN=postgres://postgres:${POSTGRES_PASSWORD}@${POSTGRES_CONTAINER}:5432/${POSTGRES_DATABASE}?sslmode=disable" \
  "$BUILD_IMAGE" go test -race -json -count=1 \
  -run '^(TestDelegatedTasksShareRootExposureKnowledge|TestConcurrentTaskFamilySettlementCannotOverspend)$' \
  ./internal/control >"$integration_tmp" 2>&1
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
