#!/bin/sh
set -eu

OUTPUT=${1:-evaluation/workflow-study/raw/ground-truth.json}
PROJECT_NAME=${WORKFLOW_STUDY_PROJECT_NAME:-taskgate-workflow-study}
REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
: "${POSTGRES_USER:=postgres}"
: "${POSTGRES_DB:=travel_demo}"

mkdir -p "$(dirname "$REPOSITORY_ROOT/$OUTPUT")"
docker compose --project-name "$PROJECT_NAME" \
  --file "$REPOSITORY_ROOT/compose.yaml" \
  --file "$REPOSITORY_ROOT/evaluation/workflow-study/compose.yaml" \
  exec -T business-postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
    --tuples-only --no-align --command \
    "SELECT jsonb_object_agg(task_id, answer ORDER BY task_id) FROM study_hidden.task_ground_truth" \
  >"$REPOSITORY_ROOT/$OUTPUT"

python3 "$REPOSITORY_ROOT/evaluation/workflow-study/validate.py" --truth "$REPOSITORY_ROOT/$OUTPUT"
echo "Exported hidden workflow-study truth to $OUTPUT"
