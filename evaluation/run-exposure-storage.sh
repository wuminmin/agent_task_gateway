#!/bin/sh
set -eu

REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CONTAINER_NAME=${EXPOSURE_STORAGE_CONTAINER:-taskgate-exposure-storage-postgres}
POSTGRES_PORT=${EXPOSURE_STORAGE_PORT:-28435}
POSTGRES_PASSWORD=${EXPOSURE_STORAGE_PASSWORD:-storage-eval-change-me}
OUTPUT=${EXPOSURE_STORAGE_OUTPUT:-evaluation/exposure-storage/results.json}

cleanup() {
  docker rm --force "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

cleanup
docker build --file "$REPOSITORY_ROOT/evaluation/Dockerfile" \
  --target build --tag taskgate-exposure-evaluation-build:local \
  "$REPOSITORY_ROOT" >/dev/null
docker run --detach --rm --name "$CONTAINER_NAME" \
  --publish "127.0.0.1:${POSTGRES_PORT}:5432" \
  --env POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
  --env POSTGRES_DB=exposure_storage \
  postgres:16-bookworm >/dev/null

attempt=0
until docker exec "$CONTAINER_NAME" pg_isready --username postgres --dbname exposure_storage >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    echo "PostgreSQL did not become ready" >&2
    exit 1
  fi
  sleep 1
done

mkdir -p "$REPOSITORY_ROOT/evaluation/exposure-storage"
docker run --rm --network host \
  --volume "$REPOSITORY_ROOT:/workspace" --workdir /workspace \
  --env "EXPOSURE_STORAGE_POSTGRES_DSN=postgres://postgres:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/exposure_storage?sslmode=disable" \
  taskgate-exposure-evaluation-build:local \
  go run ./evaluation/cmd/exposure-storage -output "$OUTPUT"
