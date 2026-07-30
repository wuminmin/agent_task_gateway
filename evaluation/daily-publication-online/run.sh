#!/usr/bin/env bash
set -euo pipefail
umask 077

experiment_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(CDPATH= cd -- "$experiment_dir/../.." && pwd)
offline_dir="$repository_root/evaluation/daily-publication"
rows=${DAILY_PUBLICATION_ROWS:-2000}
run_id=${DAILY_PUBLICATION_ONLINE_RUN_ID:-run-$(date -u +%Y%m%dT%H%M%SZ)}
run_dir=${DAILY_PUBLICATION_ONLINE_RUN_DIR:-$experiment_dir/raw/$run_id}
compose_project=${DAILY_PUBLICATION_ONLINE_PROJECT:-taskgate-daily-online-${run_id,,}}
compose_project=${compose_project//[^a-z0-9_-]/-}
export DAILY_PUBLICATION_ONLINE_IMAGE="${compose_project}-tool"
compose=(docker compose --project-name "$compose_project" --file "$experiment_dir/compose.yaml")

if [[ -e "$run_dir" ]]; then
  echo "refusing to overwrite existing run directory: $run_dir" >&2
  exit 1
fi
mkdir -m 700 -p "$run_dir"

cleanup() {
  if [[ ${DAILY_PUBLICATION_ONLINE_KEEP_STACK:-0} != 1 ]]; then
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

python3 "$offline_dir/harness.py" validate-config --config "$offline_dir/config.json"
python3 "$offline_dir/harness.py" render-inputs \
  --config "$offline_dir/config.json" --rows "$rows" --output-dir "$run_dir/candidate-inputs"

export DAILY_PUBLICATION_ROWS=$rows
"${compose[@]}" build online
"${compose[@]}" up --detach --wait business-postgres control-postgres

"${compose[@]}" exec -T business-postgres \
  psql --username postgres --dbname taskgate_daily --no-psqlrc --quiet --tuples-only --no-align \
  --file /evaluation/dataset-manifest.sql >"$run_dir/dataset-manifest.json"

"${compose[@]}" run --rm --no-deps --user "$(id -u):$(id -g)" \
  --volume "$run_dir:/evidence:rw" online prepare \
  -input-dir /evidence/candidate-inputs \
  -approved-dir /evidence/approved-inputs \
  -artifact-dir /evidence/artifacts \
  -calibration-dir /evidence/calibration \
  -manifest /evidence/preparation.json

for day in day0 day1 day2 day3; do
  install_dsn="postgres://postgres:${DAILY_POSTGRES_PASSWORD:-daily-postgres-local}@business-postgres:5432/taskgate_daily_${day}?sslmode=disable"
  "${compose[@]}" run --rm --no-deps --user "$(id -u):$(id -g)" \
    --env "SNAPSHOT_INSTALL_POSTGRES_DSN=$install_dsn" \
    --volume "$run_dir:/evidence:ro" installer \
    -artifact-dir /evidence/artifacts \
    -input "/evidence/approved-inputs/${day}.json"
done

"${compose[@]}" run --rm --no-deps --user "$(id -u):$(id -g)" \
  --volume "$run_dir:/evidence:rw" \
  --volume "$offline_dir/sql/05-generate-daily-data.sh:/bound/generator:ro" \
  --volume "$offline_dir/config.json:/bound/config.json:ro" \
  online run \
  -input-dir /evidence/approved-inputs \
  -artifact-dir /evidence/artifacts \
  -catalog-dir /evidence/catalogs \
  -dataset-manifest /evidence/dataset-manifest.json \
  -generator /bound/generator \
  -config /bound/config.json \
  -output /evidence/online-evidence.json

PYTHONDONTWRITEBYTECODE=1 python3 "$offline_dir/harness.py" validate-online \
  --evidence "$run_dir/online-evidence.json"

echo "daily-publication online evidence: $run_dir/online-evidence.json"
