#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
FUZZ_ROOT="$ROOT_DIR/evaluation/fuzz/results"
ATTACK_ROOT="$SCRIPT_DIR/raw"
IMAGE=${TASKGATE_ARTIFACT_IMAGE:-taskgate-evaluation-artifacts:local}
ALLOW_PARTIAL=${SECURITY_ALLOW_PARTIAL:-0}

case "$ALLOW_PARTIAL" in 0|1) ;; *) echo "SECURITY_ALLOW_PARTIAL must be 0 or 1" >&2; exit 2 ;; esac

command -v docker >/dev/null 2>&1 || { echo "full security campaign requires Docker" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable or not permitted" >&2; exit 1; }

campaign_id=${SECURITY_FULL_RUN_ID:-full-$(date -u +%Y%m%dT%H%M%SZ)-$$}
fuzz_id=${FUZZ_RUN_ID:-$campaign_id-fuzz}
attack_id=${SECURITY_RUN_ID:-$campaign_id-attack}
for identifier in "$campaign_id" "$fuzz_id" "$attack_id"; do
  case "$identifier" in
    ''|.|..|*[!A-Za-z0-9._-]*) echo "security run IDs may contain only letters, digits, dot, underscore, and hyphen" >&2; exit 2 ;;
  esac
done
result_path="$SCRIPT_DIR/results.json"
if [ -f "$result_path" ]; then
  mv "$result_path" "$SCRIPT_DIR/results.previous-$campaign_id.json"
fi

FUZZ_RESULT_ROOT="$FUZZ_ROOT" FUZZ_RUN_ID="$fuzz_id" "$ROOT_DIR/evaluation/fuzz/campaign.sh"
SECURITY_RESULT_ROOT="$ATTACK_ROOT" SECURITY_RUN_ID="$attack_id" "$SCRIPT_DIR/run-corpus.sh"

docker build --file "$ROOT_DIR/evaluation/Dockerfile" --target artifacts --tag "$IMAGE" "$ROOT_DIR"
docker run --rm \
  --user "$(id -u):$(id -g)" \
  --entrypoint python3 \
  --volume "$ROOT_DIR:/workspace:ro" \
  --volume "$SCRIPT_DIR:/workspace/evaluation/security" \
  "$IMAGE" /workspace/evaluation/security/verify.py \
    --root /workspace \
    --campaign "/workspace/evaluation/fuzz/results/$fuzz_id/campaign.json" \
    --attack-run "/workspace/evaluation/security/raw/$attack_id/run.json" \
    --output /workspace/evaluation/security/results.json

docker run --rm \
  --entrypoint python3 \
  --volume "$ROOT_DIR:/workspace:ro" \
  "$IMAGE" /workspace/evaluation/security/verify.py \
    --root /workspace \
    --verify-result /workspace/evaluation/security/results.json

if ! grep -q '"fuzz_cpu_requirement_met": true' "$result_path"; then
  if [ "$ALLOW_PARTIAL" != 1 ]; then
    mv "$result_path" "$SCRIPT_DIR/results.rejected-$campaign_id.json"
    echo "security campaign failed: verified fuzz CPU time is below the fixed 24-hour publication bar" >&2
    echo "use SECURITY_ALLOW_PARTIAL=1 only for explicitly non-publication pipeline validation" >&2
    exit 1
  fi
  echo "warning: retaining explicitly partial security evidence below the 24-hour publication bar" >&2
fi

echo "security evidence: evaluation/security/results.json"
