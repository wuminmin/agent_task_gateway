#!/usr/bin/env bash
set -uo pipefail

experiment_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$experiment_dir/../.." && pwd)
config_path=${DAILY_PUBLICATION_CONFIG:-$experiment_dir/config.json}
rows=${DAILY_PUBLICATION_ROWS:-2000}
run_id=${DAILY_PUBLICATION_RUN_ID:-run-$(date -u +%Y%m%dT%H%M%SZ)}
run_dir=${DAILY_PUBLICATION_RUN_DIR:-$experiment_dir/raw/$run_id}
compose_project=${DAILY_PUBLICATION_PROJECT:-taskgate-daily-${run_id,,}}
compose_project=${compose_project//[^a-z0-9_-]/-}
compose=(docker compose --project-name "$compose_project" --file "$experiment_dir/compose.yaml")

if [[ -e "$run_dir" ]]; then
  echo "refusing to overwrite existing run directory: $run_dir" >&2
  exit 1
fi
mkdir -m 700 -p "$run_dir" "$run_dir/raw" "$run_dir/calibration" "$run_dir/approved-inputs"

cleanup() {
  if [[ ${DAILY_PUBLICATION_KEEP_STACK:-0} != 1 ]]; then
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

python3 "$experiment_dir/harness.py" validate-config --config "$config_path" || exit 1
python3 "$experiment_dir/harness.py" render-inputs \
  --config "$config_path" --rows "$rows" --output-dir "$run_dir/candidate-inputs" || exit 1

export DAILY_PUBLICATION_ROWS=$rows
if ! "${compose[@]}" build phase; then
  echo "failed to build the daily-publication phase image" >&2
  exit 1
fi
if ! "${compose[@]}" up --detach --wait business-postgres; then
  echo "failed to start the isolated daily-publication database" >&2
  exit 1
fi

if ! "${compose[@]}" exec -T business-postgres \
  psql --username postgres --dbname taskgate_daily --no-psqlrc --quiet --tuples-only --no-align \
  --file /evaluation/dataset-manifest.sql >"$run_dir/dataset-manifest.json"; then
  echo "failed to record the generated four-day dataset manifest" >&2
  exit 1
fi

run_phase() {
  local day=$1
  local sample=$2
  local phase=$3
  local input_path=$4
  local artifact_dir=$5
  local receipt_dir=$6
  local report_path=$7
  local receipt_digest=${8:-}
  local -a mounts=(
    --volume "$input_path:/input/input.json:ro"
    --volume "$artifact_dir:/artifacts:ro"
  )
  local -a child=(/usr/local/bin/v4-offline)
  case "$phase" in
    build)
      mounts=(
        --volume "$input_path:/input/input.json:ro"
        --volume "$artifact_dir:/artifacts:rw"
      )
      child+=(build -input /input/input.json -output-dir /artifacts)
      ;;
    strict_verify)
      mounts+=(--volume "$receipt_dir:/receipts:rw")
      child+=(verify -input /input/input.json -artifact-dir /artifacts -receipt /receipts/verification.json)
      ;;
    activation)
      mounts+=(--volume "$receipt_dir:/receipts:ro")
      child+=(activate -input /input/input.json -artifact-dir /artifacts \
        -receipt /receipts/verification.json -receipt-sha256 "$receipt_digest")
      ;;
    *)
      echo "unknown phase $phase" >&2
      return 1
      ;;
  esac
  "${compose[@]}" run --rm --no-deps --quiet-pull --user "$(id -u):$(id -g)" \
    "${mounts[@]}" phase \
    -phase "$phase" -day "$day" -sample "$sample" -- "${child[@]}" >"$report_path"
}

campaign_failed=0
for day in day0 day1 day2 day3; do
  candidate_input="$run_dir/candidate-inputs/$day.json"
  calibration_dir="$run_dir/calibration/$day"
  calibration_artifacts="$calibration_dir/artifacts"
  mkdir -m 750 -p "$calibration_artifacts"
  if ! run_phase "$day" 0 build "$candidate_input" "$calibration_artifacts" \
      "$calibration_dir" "$calibration_dir/build.json"; then
    echo "calibration build failed for $day" >&2
    campaign_failed=1
    break
  fi
  if ! python3 "$experiment_dir/harness.py" approve \
      --input "$candidate_input" \
      --build-report "$calibration_dir/build.json" \
      --output "$run_dir/approved-inputs/$day.json"; then
    campaign_failed=1
    break
  fi
done

if [[ $campaign_failed -eq 0 ]]; then
  for day in day0 day1 day2 day3; do
    approved_input="$run_dir/approved-inputs/$day.json"
    for sample in 1 2 3; do
      sample_dir="$run_dir/raw/$day/sample-$sample"
      artifact_dir="$sample_dir/artifacts"
      receipt_dir="$sample_dir/receipt"
      mkdir -m 750 -p "$artifact_dir" "$receipt_dir"
      if ! run_phase "$day" "$sample" build "$approved_input" "$artifact_dir" \
          "$receipt_dir" "$sample_dir/build.json"; then
        echo "measured build failed for $day sample $sample" >&2
        campaign_failed=1
        break 2
      fi
      if ! run_phase "$day" "$sample" strict_verify "$approved_input" "$artifact_dir" \
          "$receipt_dir" "$sample_dir/strict_verify.json"; then
        echo "strict verification failed for $day sample $sample" >&2
        campaign_failed=1
        break 2
      fi
      receipt_digest=$(python3 "$experiment_dir/harness.py" receipt-sha \
        --report "$sample_dir/strict_verify.json") || {
          campaign_failed=1
          break 2
        }
      if ! run_phase "$day" "$sample" activation "$approved_input" "$artifact_dir" \
          "$receipt_dir" "$sample_dir/activation.json" "$receipt_digest"; then
        echo "receipt-bound activation failed for $day sample $sample" >&2
        campaign_failed=1
        break 2
      fi
    done
  done
fi

summary_args=(
  summarize
  --config "$config_path"
  --rows "$rows"
  --raw-dir "$run_dir/raw"
  --dataset-manifest "$run_dir/dataset-manifest.json"
  --output "$run_dir/results.json"
)
if [[ -n ${DAILY_PUBLICATION_ONLINE_EVIDENCE:-} ]]; then
  online_source=$DAILY_PUBLICATION_ONLINE_EVIDENCE
  online_copy="$run_dir/online-evidence.json"
  if [[ ! -f "$online_source" || -L "$online_source" || -e "$online_copy" ]]; then
    echo "online evidence must be a regular non-symlink file and the run copy must be new" >&2
    exit 1
  fi
  if ! cp -- "$online_source" "$online_copy" || ! chmod 600 "$online_copy"; then
    echo "failed to preserve online evidence inside the offline run directory" >&2
    exit 1
  fi
  summary_args+=(--online-evidence "$online_copy")
fi
python3 "$experiment_dir/harness.py" "${summary_args[@]}"
summary_status=$?

echo "daily-publication evidence: $run_dir/results.json"
if [[ $campaign_failed -ne 0 ]]; then
  exit 1
fi
if [[ $summary_status -eq 2 && ${DAILY_PUBLICATION_ALLOW_INCOMPLETE:-0} == 1 ]]; then
  echo "offline campaign completed; overall RQ5 remains incomplete without online version-routed evidence"
  exit 0
fi
exit "$summary_status"
