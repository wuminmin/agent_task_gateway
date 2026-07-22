#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
RESULT_ROOT=${FUZZ_RESULT_ROOT:-$SCRIPT_DIR/results}
CPU_HOURS=${FUZZ_CPU_HOURS:-24}
WORKERS=${FUZZ_WORKERS:-4}
IMAGE=${TASKGATE_FUZZ_IMAGE:-taskgate-evaluation-fuzz:local}

case "$CPU_HOURS" in *[!0-9]*|'') echo "FUZZ_CPU_HOURS must be a positive integer" >&2; exit 2 ;; esac
case "$WORKERS" in *[!0-9]*|'') echo "FUZZ_WORKERS must be a positive integer" >&2; exit 2 ;; esac
[ "$CPU_HOURS" -gt 0 ] && [ "$WORKERS" -gt 0 ] || { echo "CPU hours and workers must be positive" >&2; exit 2; }
command -v docker >/dev/null 2>&1 || { echo "fuzz campaign requires Docker" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "fuzz campaign requires sha256sum" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable or not permitted" >&2; exit 1; }

target_names="FuzzAuthorizeNeverPanics FuzzFormattingMetamorphic FuzzQueryPlanCompileNeverPanics"
target_count=3
seconds=$((CPU_HOURS * 3600 / WORKERS / target_count))
if [ -n "${FUZZ_SECONDS_PER_TARGET:-}" ]; then
  case "$FUZZ_SECONDS_PER_TARGET" in *[!0-9]*|'') echo "FUZZ_SECONDS_PER_TARGET must be a positive integer" >&2; exit 2 ;; esac
  [ "$FUZZ_SECONDS_PER_TARGET" -gt 0 ] || { echo "FUZZ_SECONDS_PER_TARGET must be positive" >&2; exit 2; }
  seconds=$FUZZ_SECONDS_PER_TARGET
fi
[ "$seconds" -gt 0 ] || seconds=1
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_id=${FUZZ_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
case "$run_id" in
  ''|.|..|*[!A-Za-z0-9._-]*) echo "FUZZ_RUN_ID must contain only letters, digits, dot, underscore, and hyphen" >&2; exit 2 ;;
esac
result_dir="$RESULT_ROOT/$run_id"
[ ! -e "$result_dir" ] || { echo "fuzz result directory already exists: $result_dir" >&2; exit 1; }
mkdir -p "$result_dir"

inputs="$result_dir/inputs.tsv"
: >"$inputs"
for input in \
  "$ROOT_DIR/evaluation/fuzz/policy_fuzz_test.go" \
  "$ROOT_DIR/evaluation/fuzz/campaign.sh" \
  "$ROOT_DIR/evaluation/Dockerfile" \
  "$ROOT_DIR/go.mod" \
  "$ROOT_DIR/go.sum" \
  "$ROOT_DIR"/internal/queryplan/*.go \
  "$ROOT_DIR"/internal/sqlpolicy/*.go; do
  relative=${input#"$ROOT_DIR"/}
  digest=$(sha256sum "$input" | cut -d ' ' -f 1)
  printf '%s\t%s\n' "$relative" "$digest" >>"$inputs"
done

docker build --file "$ROOT_DIR/evaluation/Dockerfile" --target fuzz --tag "$IMAGE" "$ROOT_DIR"
image_id=$(docker image inspect --format '{{.Id}}' "$IMAGE")

revision=$(git -C "$ROOT_DIR" rev-parse --verify HEAD 2>/dev/null || echo unknown)
{
  echo "schema_version=1"
  echo "status=running"
  echo "run_id=$run_id"
  echo "started_at_utc=$started_at"
  echo "git_revision=$revision"
  echo "requested_cpu_hours=$CPU_HOURS"
  echo "workers=$WORKERS"
  echo "wall_seconds_per_target=$seconds"
  echo "targets=FuzzAuthorizeNeverPanics,FuzzFormattingMetamorphic,FuzzQueryPlanCompileNeverPanics"
} >"$result_dir/campaign.env"

rows="$result_dir/targets.tsv"
: >"$rows"

run_target() {
  target=$1
  log="$result_dir/$target.log"
  if ! docker run --rm \
    --user "$(id -u):$(id -g)" \
    --env LC_ALL=C \
    --env "GOMAXPROCS=$WORKERS" \
    --env HOME=/tmp \
    --volume "$result_dir:/results" \
    --workdir /tmp \
    "$IMAGE" \
    sh -c 'mkdir -p "/results/cache-${1}" &&
      exec /usr/bin/time -v /usr/local/bin/taskgate-fuzz.test -test.run "^$" \
        -test.fuzz "^${1}$" -test.fuzztime "${2}s" -test.parallel "$3" \
        -test.fuzzcachedir "/results/cache-${1}"' \
      sh "$target" "$seconds" "$WORKERS" >"$log" 2>&1; then
    echo "fuzz target $target failed; complete log: $log" >&2
    return 1
  fi
  user_seconds=$(sed -n 's/^[[:space:]]*User time (seconds):[[:space:]]*//p' "$log" | tail -n 1)
  system_seconds=$(sed -n 's/^[[:space:]]*System time (seconds):[[:space:]]*//p' "$log" | tail -n 1)
  if [ -z "$user_seconds" ] || [ -z "$system_seconds" ]; then
    echo "fuzz target $target log omitted GNU time CPU totals: $log" >&2
    return 1
  fi
  cpu_seconds=$(awk -v user="$user_seconds" -v sys="$system_seconds" 'BEGIN { printf "%.6f", user + sys }')
  log_sha=$(sha256sum "$log" | cut -d ' ' -f 1)
  printf '%s\t%s\t%s\t%s\t%s\n' "$target" "$(basename -- "$log")" "$log_sha" "$user_seconds" "$system_seconds" >>"$rows"
}

status=complete
for target in $target_names; do
  if ! run_target "$target"; then
    status=failed
    break
  fi
done

finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
actual_cpu_seconds=$(awk -F '\t' '{ total += $4 + $5 } END { printf "%.6f", total }' "$rows")
required_cpu_seconds=$((CPU_HOURS * 3600))
if awk -v actual="$actual_cpu_seconds" -v required="$required_cpu_seconds" 'BEGIN { exit !(actual >= required) }'; then
  cpu_requirement_met=true
else
  cpu_requirement_met=false
fi
if [ "$status" = complete ] && [ "$cpu_requirement_met" = false ] && [ -z "${FUZZ_SECONDS_PER_TARGET:-}" ]; then
  status=insufficient_cpu
fi
{
  echo "schema_version=1"
  echo "status=$status"
  echo "run_id=$run_id"
  echo "started_at_utc=$started_at"
  echo "finished_at_utc=$finished_at"
  echo "git_revision=$revision"
  echo "requested_cpu_hours=$CPU_HOURS"
  echo "actual_cpu_seconds=$actual_cpu_seconds"
  echo "cpu_requirement_met=$cpu_requirement_met"
  echo "workers=$WORKERS"
  echo "wall_seconds_per_target=$seconds"
  echo "note=Requested CPU-hours are a schedule target; report actual CPU time from the GNU time logs."
} >"$result_dir/campaign.env"
tab=$(printf '\t')
while IFS="$tab" read -r input_path input_sha; do
  [ -n "$input_path" ] || continue
  current_sha=$(sha256sum "$ROOT_DIR/$input_path" | cut -d ' ' -f 1)
  [ "$current_sha" = "$input_sha" ] || {
    echo "fuzz input changed during the campaign: $input_path" >&2
    exit 1
  }
done <"$inputs"
source_sha=$(awk -F '\t' '$1 == "evaluation/fuzz/policy_fuzz_test.go" { print $2 }' "$inputs")
campaign_source_sha=$(awk -F '\t' '$1 == "evaluation/fuzz/campaign.sh" { print $2 }' "$inputs")
dockerfile_sha=$(awk -F '\t' '$1 == "evaluation/Dockerfile" { print $2 }' "$inputs")
go_version=$(docker run --rm "$IMAGE" go version | sed 's/"/\\"/g')
corpus="$result_dir/corpus.tsv"
: >"$corpus"
for target in $target_names; do
  cache_dir="$result_dir/cache-$target"
  [ -d "$cache_dir" ] || { echo "fuzz cache is missing: $cache_dir" >&2; exit 1; }
  find "$cache_dir" -type f -print | LC_ALL=C sort | while IFS= read -r cache_file; do
    cache_relative=${cache_file#"$result_dir"/}
    cache_sha=$(sha256sum "$cache_file" | cut -d ' ' -f 1)
    printf '%s\t%s\n' "$cache_relative" "$cache_sha" >>"$corpus"
  done
done

{
  echo "{"
  echo "  \"schema_version\": 1,"
  echo "  \"status\": \"$status\","
  echo "  \"run_id\": \"$run_id\","
  echo "  \"started_at_utc\": \"$started_at\","
  echo "  \"finished_at_utc\": \"$finished_at\","
  echo "  \"git_revision\": \"$revision\","
  echo "  \"go_version\": \"$go_version\","
  echo "  \"fuzz_image_id\": \"$image_id\","
  echo "  \"requested_cpu_hours\": $CPU_HOURS,"
  echo "  \"actual_cpu_seconds\": $actual_cpu_seconds,"
  echo "  \"cpu_requirement_met\": $cpu_requirement_met,"
  echo "  \"workers\": $WORKERS,"
  echo "  \"wall_seconds_per_target\": $seconds,"
  echo "  \"fuzz_source\": \"evaluation/fuzz/policy_fuzz_test.go\","
  echo "  \"fuzz_source_sha256\": \"$source_sha\","
  echo "  \"campaign_source\": \"evaluation/fuzz/campaign.sh\","
  echo "  \"campaign_source_sha256\": \"$campaign_source_sha\","
  echo "  \"dockerfile\": \"evaluation/Dockerfile\","
  echo "  \"dockerfile_sha256\": \"$dockerfile_sha\","
  echo "  \"targets\": ["
  index=0
  while IFS="$tab" read -r target log_name log_sha user_seconds system_seconds; do
    [ -n "$target" ] || continue
    if [ "$index" -gt 0 ]; then echo "    ,"; fi
    cpu_seconds=$(awk -v user="$user_seconds" -v sys="$system_seconds" 'BEGIN { printf "%.6f", user + sys }')
    printf '    {"name":"%s","log":"%s","log_sha256":"%s","user_seconds":%s,"system_seconds":%s,"actual_cpu_seconds":%s}' \
      "$target" "$log_name" "$log_sha" "$user_seconds" "$system_seconds" "$cpu_seconds"
    echo
    index=$((index + 1))
  done <"$rows"
  echo "  ],"
  echo "  \"corpus\": ["
  index=0
  while IFS="$tab" read -r corpus_path corpus_sha; do
    [ -n "$corpus_path" ] || continue
    if [ "$index" -gt 0 ]; then echo "    ,"; fi
    printf '    {"path":"%s","sha256":"%s"}\n' "$corpus_path" "$corpus_sha"
    index=$((index + 1))
  done <"$corpus"
  echo "  ],"
  echo "  \"inputs\": ["
  index=0
  while IFS="$tab" read -r input_path input_sha; do
    [ -n "$input_path" ] || continue
    if [ "$index" -gt 0 ]; then echo "    ,"; fi
    printf '    {"path":"%s","sha256":"%s"}\n' "$input_path" "$input_sha"
    index=$((index + 1))
  done <"$inputs"
  echo "  ]"
  echo "}"
} >"$result_dir/campaign.json"

[ "$status" = complete ] || exit 1
echo "fuzz campaign logs written to $result_dir"
