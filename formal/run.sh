#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_TLA_IMAGE:-taskgate-tla:1.7.1}
RESULT_DIR=$SCRIPT_DIR/results

fail() {
  echo "formal verification failed: $*" >&2
  exit 1
}

command -v docker >/dev/null 2>&1 || fail "Docker is required; install Docker Engine and retry 'make formal'"
docker info >/dev/null 2>&1 || fail "the Docker daemon is unavailable or not permitted"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required to record model provenance"

docker build --file "$SCRIPT_DIR/Dockerfile" --tag "$IMAGE" "$ROOT_DIR"
mkdir -p "$RESULT_DIR"

run_model() {
  model_file=$1
  config_file=$2
  result_json=$3
  result_log=$4
  tmp_log=$(mktemp /tmp/taskgate-tlc.XXXXXX)
  cleanup_tmp() {
    case "$tmp_log" in /tmp/taskgate-tlc.*) rm -f "$tmp_log" ;; esac
  }

  set +e
  docker run --rm \
    --volume "$SCRIPT_DIR:/model:ro" \
    --tmpfs /tmp:size=512m,mode=1777 \
    "$IMAGE" -workers auto -deadlock -metadir /tmp/tlc -config "$config_file" "$model_file" >"$tmp_log" 2>&1
  exit_code=$?
  set -e

  cp "$tmp_log" "$RESULT_DIR/$result_log"
  cat "$tmp_log"

  if [ "$exit_code" -eq 0 ]; then
    status=passed
  else
    status=failed
  fi
  stats_line=$(sed -n '/ states generated, .* distinct states found, .* states left on queue\./p' "$tmp_log" | tail -n 1)
  states_generated=$(printf '%s\n' "$stats_line" | sed -n 's/^\([0-9][0-9,]*\) states generated,.*/\1/p' | tr -d ',')
  distinct_states=$(printf '%s\n' "$stats_line" | sed -n 's/^[0-9][0-9,]* states generated, \([0-9][0-9,]*\) distinct states found,.*/\1/p' | tr -d ',')
  search_depth=$(sed -n 's/^The depth of the complete state graph search is \([0-9][0-9,]*\)\..*/\1/p' "$tmp_log" | tail -n 1 | tr -d ',')
  if [ "$status" = passed ] && { [ -z "$states_generated" ] || [ -z "$distinct_states" ] || [ -z "$search_depth" ]; }; then
    status=failed
    exit_code=65
    echo "formal verification failed: TLC exited successfully but its final state/depth statistics were not recognizable" >&2
  fi
  checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  model_sha=$(sha256sum "$SCRIPT_DIR/$model_file" | cut -d ' ' -f 1)
  config_sha=$(sha256sum "$SCRIPT_DIR/$config_file" | cut -d ' ' -f 1)
  log_sha=$(sha256sum "$RESULT_DIR/$result_log" | cut -d ' ' -f 1)
  cat >"$RESULT_DIR/$result_json" <<EOF
{
  "schema_version": 1,
  "status": "$status",
  "checked_at": "$checked_at",
  "tool": "TLC",
  "tool_version": "1.7.1",
  "model": "formal/$model_file",
  "config": "formal/$config_file",
  "model_sha256": "$model_sha",
  "config_sha256": "$config_sha",
  "raw_log": "formal/results/$result_log",
  "log_sha256": "$log_sha",
  "states_generated": ${states_generated:-null},
  "distinct_states": ${distinct_states:-null},
  "search_depth": ${search_depth:-null},
  "exit_code": $exit_code
}
EOF

  cleanup_tmp
  if [ "$exit_code" -ne 0 ]; then
    fail "TLC returned exit code $exit_code; complete log: formal/results/$result_log"
  fi
  echo "ok - TLC invariants passed; result: formal/results/$result_json"
}

run_model TaskGate.tla TaskGate.cfg tlc.json tlc.log
run_model VectorBudget.tla VectorBudget.cfg vector_budget.json vector_budget.log
run_model SQLAuthorization.tla SQLAuthorization.cfg sql_authorization.json sql_authorization.log
run_model MultiTaskAudit.tla MultiTaskAudit.cfg multi_task_audit.json multi_task_audit.log
run_model ReceiptAudit.tla ReceiptAudit.cfg receipt_audit.json receipt_audit.log
run_model RecoveryLiveness.tla RecoveryLiveness.cfg recovery_liveness.json recovery_liveness.log
