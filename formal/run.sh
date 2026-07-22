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
tmp_log=$(mktemp /tmp/taskgate-tlc.XXXXXX)
cleanup() {
  case "$tmp_log" in /tmp/taskgate-tlc.*) rm -f "$tmp_log" ;; esac
}
trap cleanup EXIT INT TERM

set +e
docker run --rm \
  --volume "$SCRIPT_DIR:/model:ro" \
  --tmpfs /tmp:size=512m,mode=1777 \
  "$IMAGE" -workers auto -deadlock -metadir /tmp/tlc -config TaskGate.cfg TaskGate.tla >"$tmp_log" 2>&1
exit_code=$?
set -e

cp "$tmp_log" "$RESULT_DIR/tlc.log"
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
model_sha=$(sha256sum "$SCRIPT_DIR/TaskGate.tla" | cut -d ' ' -f 1)
config_sha=$(sha256sum "$SCRIPT_DIR/TaskGate.cfg" | cut -d ' ' -f 1)
log_sha=$(sha256sum "$RESULT_DIR/tlc.log" | cut -d ' ' -f 1)
cat >"$RESULT_DIR/tlc.json" <<EOF
{
  "schema_version": 1,
  "status": "$status",
  "checked_at": "$checked_at",
  "tool": "TLC",
  "tool_version": "1.7.1",
  "model": "formal/TaskGate.tla",
  "config": "formal/TaskGate.cfg",
  "model_sha256": "$model_sha",
  "config_sha256": "$config_sha",
  "raw_log": "formal/results/tlc.log",
  "log_sha256": "$log_sha",
  "states_generated": ${states_generated:-null},
  "distinct_states": ${distinct_states:-null},
  "search_depth": ${search_depth:-null},
  "exit_code": $exit_code
}
EOF

if [ "$exit_code" -ne 0 ]; then
  fail "TLC returned exit code $exit_code; complete log: formal/results/tlc.log"
fi
echo "ok - TLC invariants passed; result: formal/results/tlc.json"
