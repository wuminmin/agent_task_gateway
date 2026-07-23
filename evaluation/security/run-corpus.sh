#!/bin/sh
set -eu

# Run the security test corpus (SQL-policy attacks, prompt boundaries, and the
# four end-to-end experiments: connector crossing, budget fault, concurrency,
# crash recovery) against a live PostgreSQL control store and seal the result.
# The end-to-end experiments require CONTROL_TEST_POSTGRES_DSN; without it they
# skip and verify.py rejects the run, so the DSN is mandatory here.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
RESULT_ROOT=${SECURITY_RESULT_ROOT:-$SCRIPT_DIR/raw}
IMAGE=${TASKGATE_FUZZ_IMAGE:-taskgate-evaluation-fuzz:local}

command -v docker >/dev/null 2>&1 || { echo "security corpus requires Docker" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable or not permitted" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "security corpus requires sha256sum" >&2; exit 1; }
[ -n "${CONTROL_TEST_POSTGRES_DSN:-}" ] || { echo "CONTROL_TEST_POSTGRES_DSN is required (the end-to-end experiments need a live control store)" >&2; exit 1; }

run_id=${SECURITY_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
case "$run_id" in
  ''|.|..|*[!A-Za-z0-9._-]*) echo "SECURITY_RUN_ID must contain only letters, digits, dot, underscore, and hyphen" >&2; exit 2 ;;
esac
result_dir="$RESULT_ROOT/$run_id"
[ ! -e "$result_dir" ] || { echo "security result directory already exists: $result_dir" >&2; exit 1; }
mkdir -p "$result_dir/summaries"
log="$result_dir/attack-test.jsonl"

docker build --file "$ROOT_DIR/evaluation/Dockerfile" --target fuzz --tag "$IMAGE" "$ROOT_DIR"
set +e
docker run --rm --network host --env LC_ALL=C \
  --env CONTROL_TEST_POSTGRES_DSN="$CONTROL_TEST_POSTGRES_DSN" \
  --env SECURITY_SUMMARY_DIR=/security-out/summaries \
  --volume "$ROOT_DIR:/src:ro" --workdir /src \
  --volume "$result_dir:/security-out" \
  "$IMAGE" \
  go test -json ./evaluation/security \
    -run '^(TestAttackCorpus|TestPromptInjectionBoundaryCases|TestConnectorCrossing|TestBudgetFault|TestConcurrency|TestCrashRecovery)$' -count=1 >"$log" 2>&1
exit_code=$?
set -e

if [ "$exit_code" -eq 0 ]; then status=passed; else status=failed; fi
revision=$(git -C "$ROOT_DIR" rev-parse --verify HEAD 2>/dev/null || echo unknown)
log_sha=$(sha256sum "$log" | cut -d ' ' -f 1)
corpus_sha=$(sha256sum "$ROOT_DIR/evaluation/attacks/corpus.json" | cut -d ' ' -f 1)
test_sha=$(sha256sum "$SCRIPT_DIR/security_test.go" | cut -d ' ' -f 1)
go_version=$(docker run --rm "$IMAGE" go version | sed 's/"/\\"/g')

inputs="$result_dir/inputs.tsv"
: >"$inputs"
record_input() {
  relative=${1#"$ROOT_DIR"/}
  digest=$(sha256sum "$1" | cut -d ' ' -f 1)
  printf '%s\t%s\n' "$relative" "$digest" >>"$inputs"
}
for input in \
  "$ROOT_DIR/evaluation/attacks/corpus.json" \
  "$ROOT_DIR/evaluation/attacks/prompt-injection.json" \
  "$ROOT_DIR/evaluation/ast-gateway/tpch.json" \
  "$ROOT_DIR/evaluation/security/security_test.go" \
  "$ROOT_DIR/evaluation/security/store_harness_test.go" \
  "$ROOT_DIR/evaluation/security/run-corpus.sh" \
  "$ROOT_DIR/evaluation/Dockerfile" \
  "$ROOT_DIR/evaluation/security/connector-crossing.json" \
  "$ROOT_DIR/evaluation/security/connector_crossing_test.go" \
  "$ROOT_DIR/evaluation/security/budget-faults.json" \
  "$ROOT_DIR/evaluation/security/budget_fault_test.go" \
  "$ROOT_DIR/evaluation/security/concurrency.json" \
  "$ROOT_DIR/evaluation/security/concurrency_test.go" \
  "$ROOT_DIR/evaluation/security/crash-recovery.json" \
  "$ROOT_DIR/evaluation/security/crash_recovery_test.go" \
  "$ROOT_DIR"/evaluation/attacks/sql/*.sql; do
  record_input "$input"
done

# Experiment summaries written by the tests under SECURITY_SUMMARY_DIR. The
# filename is the experiment spec name (underscore form).
summary_names='connector_crossing budget_fault concurrency crash_recovery'
summaries="$result_dir/summaries.tsv"
: >"$summaries"
for name in $summary_names; do
  summary="$result_dir/summaries/$name.json"
  [ -f "$summary" ] || { echo "experiment summary missing: $summary" >&2; exit 1; }
  printf '%s\t%s\tevaluation/security/raw/%s/summaries/%s.json\t%s\n' \
    "$name" "$(sha256sum "$summary" | cut -d ' ' -f 1)" "$run_id" "$name" "$(sha256sum "$summary" | cut -d ' ' -f 1)" >>"$summaries"
done

{
  echo "{"
  echo "  \"schema_version\": 1,"
  echo "  \"status\": \"$status\","
  echo "  \"run_id\": \"attack-corpus-$run_id\","
  echo "  \"git_revision\": \"$revision\","
  echo "  \"go_version\": \"$go_version\","
  echo "  \"corpus\": \"evaluation/attacks/corpus.json\","
  echo "  \"corpus_sha256\": \"$corpus_sha\","
  echo "  \"test_source\": \"evaluation/security/security_test.go\","
  echo "  \"test_source_sha256\": \"$test_sha\","
  echo "  \"raw_log\": \"evaluation/security/raw/$run_id/attack-test.jsonl\","
  echo "  \"raw_log_sha256\": \"$log_sha\","
  echo "  \"exit_code\": $exit_code,"
  echo "  \"inputs\": ["
  index=0
  tab=$(printf '\t')
  while IFS="$tab" read -r input_path input_sha; do
    [ -n "$input_path" ] || continue
    if [ "$index" -gt 0 ]; then echo "    ,"; fi
    printf '    {"path":"%s","sha256":"%s"}\n' "$input_path" "$input_sha"
    index=$((index + 1))
  done <"$inputs"
  echo "  ],"
  echo "  \"experiment_summaries\": {"
  sindex=0
  while IFS="$tab" read -r name name_sha rel_path rel_sha; do
    [ -n "$name" ] || continue
    if [ "$sindex" -gt 0 ]; then echo "    ,"; fi
    printf '    "%s": {"path": "%s", "sha256": "%s"}\n' "$name" "$rel_path" "$rel_sha"
    sindex=$((sindex + 1))
  done <"$summaries"
  echo "  }"
  echo "}"
} >"$result_dir/run.json"

[ "$exit_code" -eq 0 ] || { echo "security corpus failed: $log" >&2; exit "$exit_code"; }
echo "$result_dir"
