#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EXPOSURE_EVAL_IMAGE:-taskgate-exposure-evaluation:local}
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

docker build --file "$SCRIPT_DIR/Dockerfile" --target exposure --tag "$IMAGE" "$ROOT_DIR"
tmp=$(mktemp /tmp/taskgate-exposure-eval.XXXXXX)
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  docker rm --force "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$POSTGRES_NETWORK" >/dev/null 2>&1 || true
  rm -f "$tmp"
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

docker run --rm --network "$POSTGRES_NETWORK" \
  --env "EXPOSURE_TEST_POSTGRES_DSN=postgres://postgres:${POSTGRES_PASSWORD}@${POSTGRES_CONTAINER}:5432/${POSTGRES_DATABASE}?sslmode=disable" \
  "$IMAGE" >"$tmp"
cat "$tmp"
cp "$tmp" "$SCRIPT_DIR/exposure/results.json"
