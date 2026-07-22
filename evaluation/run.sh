#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EVAL_IMAGE:-taskgate-evaluation:local}
ENV_FILE=${EVAL_ENV_FILE:-$SCRIPT_DIR/.env}

fail() {
  echo "evaluation failed: $*" >&2
  exit 1
}

usage() {
  echo "usage: $0 smoke|sf1|sf10|full|validate" >&2
  exit 2
}

[ "$#" -eq 1 ] || usage
mode=$1
case "$mode" in smoke|sf1|sf10|full|validate) ;; *) usage ;; esac

command -v docker >/dev/null 2>&1 || fail "Docker is required; install Docker Engine and retry"
docker info >/dev/null 2>&1 || fail "the Docker daemon is unavailable or not permitted"
if [ "$mode" = "full" ]; then
  command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required to seal the full campaign manifest"
fi

docker build --file "$SCRIPT_DIR/Dockerfile" --target runner --tag "$IMAGE" "$ROOT_DIR"

git_revision=$(git -C "$ROOT_DIR" rev-parse --verify HEAD 2>/dev/null || echo unknown)
if [ -n "$(git -C "$ROOT_DIR" status --short 2>/dev/null || true)" ]; then
  git_dirty=1
else
  git_dirty=0
fi
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
campaign_id="${mode}-${timestamp}"

write_full_campaign_manifest() {
  manifest_status=$1
  manifest_path="$SCRIPT_DIR/raw/campaign-$campaign_id.json"
  manifest_tmp="$manifest_path.tmp.$$"
  if [ "$git_dirty" -eq 1 ]; then
    manifest_dirty=true
  else
    manifest_dirty=false
  fi
  if [ "$manifest_status" = "complete" ]; then
    sf1_run_json="$SCRIPT_DIR/raw/full-sf1-$timestamp/run.json"
    sf10_run_json="$SCRIPT_DIR/raw/full-sf10-$timestamp/run.json"
    [ -f "$sf1_run_json" ] || fail "missing completed SF1 run metadata: $sf1_run_json"
    [ -f "$sf10_run_json" ] || fail "missing completed SF10 run metadata: $sf10_run_json"
    sf1_run_sha=$(sha256sum "$sf1_run_json" | cut -d ' ' -f 1)
    sf10_run_sha=$(sha256sum "$sf10_run_json" | cut -d ' ' -f 1)
  else
    sf1_run_sha=null
    sf10_run_sha=null
  fi
  mkdir -p "$SCRIPT_DIR/raw"
  printf '%s\n' \
    '{' \
    '  "schema_version": 1,' \
    "  \"campaign_id\": \"$campaign_id\"," \
    '  "mode": "full",' \
    "  \"status\": \"$manifest_status\"," \
    "  \"git_revision\": \"$git_revision\"," \
    "  \"git_dirty\": $manifest_dirty," \
    '  "runs": [' \
    "    {\"run_id\": \"full-sf1-$timestamp\", \"suite\": \"taskgate-sf1-four-baseline\", \"run_json_sha256\": \"$sf1_run_sha\"}," \
    "    {\"run_id\": \"full-sf10-$timestamp\", \"suite\": \"taskgate-sf10-four-baseline\", \"run_json_sha256\": \"$sf10_run_sha\"}" \
    '  ]' \
    '}' >"$manifest_tmp"
  mv "$manifest_tmp" "$manifest_path"
}

run_preflight() {
  primary=$1
  shift
  additional="$*"
  set -- docker run --rm \
    --user "$(id -u):$(id -g)" \
    --network "${EVAL_DOCKER_NETWORK:-host}" \
    --add-host host.docker.internal:host-gateway \
    --volume "$ROOT_DIR:/workspace:ro" \
    --env HOME=/tmp \
    --env "EVAL_GIT_REVISION=$git_revision" \
    --env "EVAL_GIT_DIRTY=$git_dirty" \
    --env "EVAL_CAMPAIGN_ID=$campaign_id"
  if [ -f "$ENV_FILE" ]; then
    set -- "$@" --env-file "$ENV_FILE"
  fi
  for env_name in $(env | sed -n 's/^\(EVAL_[A-Za-z0-9_]*\)=.*/\1/p' | LC_ALL=C sort); do
    case "$env_name" in EVAL_GIT_REVISION|EVAL_GIT_DIRTY|EVAL_CAMPAIGN_ID) continue ;; esac
    set -- "$@" --env "$env_name"
  done
  set -- "$@" "$IMAGE" \
    -config "/workspace/evaluation/config/$primary.json" -preflight-only
  for config_name in $additional; do
    set -- "$@" -additional-config "/workspace/evaluation/config/$config_name.json"
  done
  "$@"
}

run_config() {
  config_name=$1
  if [ "$mode" = "validate" ]; then
    run_name=validate
  else
    run_name=$mode
  fi
  run_id="${run_name}-${config_name}-${timestamp}"
  output="$SCRIPT_DIR/raw/$run_id"

  set -- docker run --rm \
    --user "$(id -u):$(id -g)" \
    --network "${EVAL_DOCKER_NETWORK:-host}" \
    --add-host host.docker.internal:host-gateway \
    --volume "$ROOT_DIR:/workspace:ro" \
    --volume "$SCRIPT_DIR/raw:/workspace/evaluation/raw" \
    --env HOME=/tmp \
    --env "EVAL_GIT_REVISION=$git_revision" \
    --env "EVAL_GIT_DIRTY=$git_dirty" \
    --env "EVAL_CAMPAIGN_ID=$campaign_id"

  if [ -f "$ENV_FILE" ]; then
    set -- "$@" --env-file "$ENV_FILE"
  fi
  for env_name in $(env | sed -n 's/^\(EVAL_[A-Za-z0-9_]*\)=.*/\1/p' | LC_ALL=C sort); do
    case "$env_name" in EVAL_GIT_REVISION|EVAL_GIT_DIRTY|EVAL_CAMPAIGN_ID) continue ;; esac
    set -- "$@" --env "$env_name"
  done

  if [ "$mode" = "validate" ]; then
    set -- "$@" "$IMAGE" \
      -config "/workspace/evaluation/config/$config_name.json" -validate-only
  else
    mkdir -p "$output"
    set -- "$@" "$IMAGE" \
      -config "/workspace/evaluation/config/$config_name.json" \
      -output "/workspace/evaluation/raw/$run_id" \
      -run-id "$run_id"
  fi
  "$@"
}

case "$mode" in
  smoke)
    run_preflight smoke
    run_config smoke
    ;;
  sf1)
    run_preflight sf1
    run_config sf1
    ;;
  sf10)
    run_preflight sf10
    run_config sf10
    ;;
  full)
    run_preflight sf1 sf10
    write_full_campaign_manifest running
    run_config sf1
    run_config sf10
    write_full_campaign_manifest complete
    ;;
  validate)
    run_config smoke
    run_config sf1
    run_config sf10
    ;;
esac
