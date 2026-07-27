#!/bin/sh
set -eu

PROJECT_NAME=${EXPOSURE_SCALE_PROJECT_NAME:-taskgate-exposure-scale}
KEEP_STACK=${EXPOSURE_SCALE_KEEP_STACK:-0}
PROMOTE=${EXPOSURE_SCALE_PROMOTE:-0}
ALLOW_SMOKE=${EXPOSURE_SCALE_ALLOW_SMOKE:-0}
RUN_ID=${EXPOSURE_SCALE_RUN_ID:-run-$(date -u +%Y%m%dT%H%M%SZ)}
REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
EXPOSURE_SCALE_RUN_DIR=$REPOSITORY_ROOT/evaluation/exposure-scale/raw/$RUN_ID

: "${TASKBOUND_ALICE_TOKEN:=alice-demo-token-change-me}"
: "${TASKBOUND_CAROL_TOKEN:=carol-demo-token-change-me}"
: "${POSTGRES_DB:=travel_demo}"
: "${POSTGRES_USER:=postgres}"
: "${POSTGRES_PASSWORD:=postgres-demo-change-me}"
: "${GATEWAY_DB_PASSWORD:=gateway-reader-demo-change-me}"
: "${GATEWAY_DATA_KEY:=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=}"
: "${GATEWAY_RECEIPT_KEY_ID:=gateway-exposure-scale-ed25519-v1}"
: "${GATEWAY_RECEIPT_PRIVATE_KEY:=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=}"
: "${CONTROL_POSTGRES_DB:=taskbound_gateway}"
: "${CONTROL_POSTGRES_ADMIN_PASSWORD:=control-admin-demo-change-me}"
: "${CONTROL_DB_PASSWORD:=control-app-demo-change-me}"
: "${OA_SERVICE_TOKEN:=oa-service-token-change-me}"
: "${OA_CALLBACK_SECRET:=oa-callback-secret-change-me}"
: "${OA_RECEIPT_KEY_ID:=oa-exposure-scale-ed25519-v1}"
: "${OA_RECEIPT_PRIVATE_KEY:=nWGxne/9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A=}"
: "${OA_RECEIPT_PUBLIC_KEY:=11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=}"
: "${OA_SESSION_SECRET:=oa-session-secret-change-me}"
: "${OA_ALICE_PASSWORD:=alice-demo-change-me}"
: "${OA_BOB_PASSWORD:=bob-demo-change-me}"

export EXPOSURE_SCALE_RUN_DIR TASKBOUND_ALICE_TOKEN TASKBOUND_CAROL_TOKEN
export POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD GATEWAY_DB_PASSWORD
export GATEWAY_DATA_KEY GATEWAY_RECEIPT_KEY_ID GATEWAY_RECEIPT_PRIVATE_KEY
export CONTROL_POSTGRES_DB CONTROL_POSTGRES_ADMIN_PASSWORD CONTROL_DB_PASSWORD
export OA_SERVICE_TOKEN OA_CALLBACK_SECRET OA_RECEIPT_KEY_ID OA_RECEIPT_PRIVATE_KEY OA_RECEIPT_PUBLIC_KEY
export OA_SESSION_SECRET OA_ALICE_PASSWORD OA_BOB_PASSWORD

mkdir -p "$EXPOSURE_SCALE_RUN_DIR"

compose() {
  docker compose --project-name "$PROJECT_NAME" \
    --file "$REPOSITORY_ROOT/compose.yaml" \
    --file "$REPOSITORY_ROOT/evaluation/exposure-scale/compose.yaml" "$@"
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ "${STATS_PID:-}" ]; then
    kill "$STATS_PID" >/dev/null 2>&1 || true
    wait "$STATS_PID" >/dev/null 2>&1 || true
  fi
  if [ "$status" -ne 0 ]; then
    echo "Exposure scale run failed; Compose state and recent logs follow." >&2
    compose ps --all >&2 || true
    compose logs --no-color --tail 160 >&2 || true
  fi
  if [ "$KEEP_STACK" != "1" ]; then
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

compose down --volumes --remove-orphans >/dev/null 2>&1 || true
compose up --build --detach --wait
compose --profile exposure-tools build exposure-bench
compose --profile exposure-tools run --rm exposure-bench -mode scale-bootstrap

CONTROL_ID=$(compose ps --quiet control-postgres)
BUSINESS_ID=$(compose ps --quiet business-postgres)
GATEWAY_ID=$(compose ps --quiet gateway)
(
  while :; do
    docker stats --no-stream "$CONTROL_ID" "$BUSINESS_ID" "$GATEWAY_ID" --format '{{json .}}' || break
    sleep 1
  done
) >"$EXPOSURE_SCALE_RUN_DIR/docker-stats.jsonl" &
STATS_PID=$!

compose --profile exposure-tools run --rm exposure-bench -mode scale-run
kill "$STATS_PID" >/dev/null 2>&1 || true
wait "$STATS_PID" >/dev/null 2>&1 || true
STATS_PID=

python3 "$REPOSITORY_ROOT/evaluation/exposure-performance/merge_memory.py" \
  --report "$EXPOSURE_SCALE_RUN_DIR/report.json" \
  --stats "$EXPOSURE_SCALE_RUN_DIR/docker-stats.jsonl" \
  --output "$EXPOSURE_SCALE_RUN_DIR/report-with-memory.json"

relative=${EXPOSURE_SCALE_RUN_DIR#"$REPOSITORY_ROOT"/}/report-with-memory.json
output="$EXPOSURE_SCALE_RUN_DIR/results.json"
if [ "$PROMOTE" = "1" ]; then
  output="$REPOSITORY_ROOT/evaluation/exposure-scale/results.json"
fi
if [ "$ALLOW_SMOKE" = "1" ]; then
  python3 "$REPOSITORY_ROOT/evaluation/exposure-scale/finalize.py" \
    --allow-smoke --report "$EXPOSURE_SCALE_RUN_DIR/report-with-memory.json" \
    --raw-relative "$relative" --output "$output"
else
  python3 "$REPOSITORY_ROOT/evaluation/exposure-scale/finalize.py" \
    --report "$EXPOSURE_SCALE_RUN_DIR/report-with-memory.json" \
    --raw-relative "$relative" --output "$output"
fi

echo "Exposure scale campaign completed: $output"
