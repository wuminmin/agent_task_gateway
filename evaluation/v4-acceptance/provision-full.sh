#!/bin/sh
set -eu
umask 077

REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
: "${V4_FULL_RUN_DIR:?set V4_FULL_RUN_DIR to a new credential-private evidence directory}"
: "${TASKBOUND_ALICE_TOKEN:?set TASKBOUND_ALICE_TOKEN}"
: "${OA_ALICE_PASSWORD:?set OA_ALICE_PASSWORD}"
: "${OA_BOB_PASSWORD:?set OA_BOB_PASSWORD}"
: "${V4_FULL_ENVIRONMENT_MANIFEST:?set V4_FULL_ENVIRONMENT_MANIFEST to environment.json}"
: "${V4_FULL_ENVIRONMENT_SHA256:?set V4_FULL_ENVIRONMENT_SHA256}"
: "${V4_FULL_BASELINE_ARTIFACT:?set V4_FULL_BASELINE_ARTIFACT to V2 results.json}"
: "${V4_FULL_BASELINE_SHA256:?set V4_FULL_BASELINE_SHA256}"
: "${V4_FULL_CANDIDATE_ARTIFACT:?set V4_FULL_CANDIDATE_ARTIFACT to V4 results.json}"
: "${V4_FULL_CANDIDATE_SHA256:?set V4_FULL_CANDIDATE_SHA256}"

PROVISIONER=${V4_FULL_PROVISIONER:-exposure-bench}
ACCEPTANCE=${V4_FULL_ACCEPTANCE:-v4-acceptance}
TASKS_PATH=$V4_FULL_RUN_DIR/tasks.json
CONFIG_PATH=$V4_FULL_RUN_DIR/config.json
DEFAULT_TEMPLATE=$REPOSITORY_ROOT/evaluation/v4-acceptance/full-matrix.template.json
if [ ! -f "$DEFAULT_TEMPLATE" ]; then
  DEFAULT_TEMPLATE=/usr/local/share/taskgate/full-matrix.template.json
fi
TEMPLATE_PATH=${V4_FULL_TEMPLATE:-$DEFAULT_TEMPLATE}

if [ -e "$TASKS_PATH" ] || [ -e "$CONFIG_PATH" ]; then
  echo "refusing to overwrite an existing full-matrix provisioning artifact in $V4_FULL_RUN_DIR" >&2
  exit 1
fi
mkdir -p "$V4_FULL_RUN_DIR"

"$PROVISIONER" \
  -mode scale-bootstrap \
  -scale-sizes 45000 \
  -scale-trials 140 \
  -tasks "$TASKS_PATH"

"$ACCEPTANCE" \
  -config "$TEMPLATE_PATH" \
  -prepare-full-task-pool "$TASKS_PATH" \
  -full-environment-path "$V4_FULL_ENVIRONMENT_MANIFEST" \
  -full-environment-sha256 "$V4_FULL_ENVIRONMENT_SHA256" \
  -full-baseline-path "$V4_FULL_BASELINE_ARTIFACT" \
  -full-baseline-sha256 "$V4_FULL_BASELINE_SHA256" \
  -full-candidate-path "$V4_FULL_CANDIDATE_ARTIFACT" \
  -full-candidate-sha256 "$V4_FULL_CANDIDATE_SHA256" \
  -output "$CONFIG_PATH"

echo "prepared 140 ACTIVE V4 roots across seven full-matrix cases without running the acceptance campaign"
echo "task pool: $TASKS_PATH"
echo "acceptance config: $CONFIG_PATH"
