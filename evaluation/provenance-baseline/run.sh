#!/bin/sh
set -eu

REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
RUN_ID=${PROVENANCE_BASELINE_RUN_ID:-run-$(date -u +%Y%m%dt%H%M%S%Nz)-$$}
PROJECT_NAME=${PROVENANCE_BASELINE_PROJECT_NAME:-taskgate-provsql-$RUN_ID}
KEEP_STACK=${PROVENANCE_BASELINE_KEEP_STACK:-0}
PROVENANCE_BASELINE_CONFIG=${PROVENANCE_BASELINE_CONFIG:-$REPOSITORY_ROOT/evaluation/provenance-baseline/config.smoke.json}
PROVENANCE_BASELINE_RUN_DIR=${PROVENANCE_BASELINE_RUN_DIR:-$REPOSITORY_ROOT/evaluation/provenance-baseline/raw/$RUN_ID}
export PROVENANCE_BASELINE_CONFIG PROVENANCE_BASELINE_RUN_DIR

mkdir -m 700 -p "$PROVENANCE_BASELINE_RUN_DIR"
STACK_OWNED=0

compose() {
  docker compose --project-name "$PROJECT_NAME" \
    --file "$REPOSITORY_ROOT/evaluation/provenance-baseline/compose.yaml" "$@"
}

cleanup() {
  status=$?
  trap - EXIT
  if [ "$status" -ne 0 ]; then
    echo "ProvSQL baseline failed; retaining the run directory and printing recent logs." >&2
    if [ "$STACK_OWNED" = "1" ]; then
      compose ps --all >&2 || true
      compose logs --no-color --tail 160 >&2 || true
    fi
  fi
  if [ "$STACK_OWNED" = "1" ] && [ "$KEEP_STACK" != "1" ]; then
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  exit "$status"
}
interrupted() {
  code=$1
  trap - INT TERM
  exit "$code"
}
trap cleanup EXIT
trap 'interrupted 130' INT
trap 'interrupted 143' TERM

if [ ! -f "$PROVENANCE_BASELINE_CONFIG" ]; then
  echo "configuration is not a regular file: $PROVENANCE_BASELINE_CONFIG" >&2
  exit 1
fi

existing_resources=$(
  {
    docker ps --all --quiet --filter "label=com.docker.compose.project=$PROJECT_NAME"
    docker volume ls --quiet --filter "label=com.docker.compose.project=$PROJECT_NAME"
    docker network ls --quiet --filter "label=com.docker.compose.project=$PROJECT_NAME"
  } | sed '/^$/d'
)
if [ -n "$existing_resources" ]; then
  echo "refusing to reuse Compose project with existing resources: $PROJECT_NAME" >&2
  exit 1
fi
STACK_OWNED=1

compose up --build --detach --wait direct-postgres provsql-postgres
compose --profile tools build provenance-baseline
compose --profile tools run --rm --user "$(id -u):$(id -g)" provenance-baseline \
  -config /config/config.json -config-evidence /results/config.json \
  -output /results/report.json

DIRECT_ID=$(compose ps --quiet direct-postgres)
PROVSQL_ID=$(compose ps --quiet provsql-postgres)
DIRECT_IMAGE=$(docker inspect --format '{{.Image}}' "$DIRECT_ID")
PROVSQL_IMAGE=$(docker inspect --format '{{.Image}}' "$PROVSQL_ID")
PROVSQL_REVISION=$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$PROVSQL_ID")
DIRECT_MEMORY_PEAK=$(docker exec "$DIRECT_ID" sh -c 'cat /sys/fs/cgroup/memory.peak')
PROVSQL_MEMORY_PEAK=$(docker exec "$PROVSQL_ID" sh -c 'cat /sys/fs/cgroup/memory.peak')

python3 "$REPOSITORY_ROOT/evaluation/provenance-baseline/finalize.py" \
  --config "$PROVENANCE_BASELINE_RUN_DIR/config.json" \
  --report "$PROVENANCE_BASELINE_RUN_DIR/report.json" \
  --output "$PROVENANCE_BASELINE_RUN_DIR/results.json" \
  --direct-image "$DIRECT_IMAGE" \
  --provsql-image "$PROVSQL_IMAGE" \
  --provsql-revision "$PROVSQL_REVISION" \
  --direct-memory-peak "$DIRECT_MEMORY_PEAK" \
  --provsql-memory-peak "$PROVSQL_MEMORY_PEAK"

echo "ProvSQL baseline completed: $PROVENANCE_BASELINE_RUN_DIR/results.json"
