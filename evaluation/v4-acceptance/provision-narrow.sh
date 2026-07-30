#!/bin/sh
set -eu
umask 077

REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
: "${V4_NARROW_RUN_DIR:?set V4_NARROW_RUN_DIR to a new credential-private evidence directory}"
: "${TASKBOUND_ALICE_TOKEN:?set TASKBOUND_ALICE_TOKEN}"
: "${OA_ALICE_PASSWORD:?set OA_ALICE_PASSWORD}"
: "${OA_BOB_PASSWORD:?set OA_BOB_PASSWORD}"

PROVISIONER=${V4_NARROW_PROVISIONER:-exposure-bench}
ACCEPTANCE=${V4_NARROW_ACCEPTANCE:-v4-acceptance}
TASKS_PATH=$V4_NARROW_RUN_DIR/tasks.json
CONFIG_PATH=$V4_NARROW_RUN_DIR/config.json
DEFAULT_TEMPLATE=$REPOSITORY_ROOT/evaluation/v4-acceptance/narrow-max-point.template.json
if [ ! -f "$DEFAULT_TEMPLATE" ]; then
  DEFAULT_TEMPLATE=/usr/local/share/taskgate/narrow-max-point.template.json
fi
TEMPLATE_PATH=${V4_NARROW_TEMPLATE:-$DEFAULT_TEMPLATE}

if [ -e "$TASKS_PATH" ] || [ -e "$CONFIG_PATH" ]; then
  echo "refusing to overwrite an existing narrow provisioning artifact in $V4_NARROW_RUN_DIR" >&2
  exit 1
fi
mkdir -p "$V4_NARROW_RUN_DIR"

"$PROVISIONER" \
  -mode scale-bootstrap \
  -scale-sizes 45000 \
  -scale-trials 20 \
  -tasks "$TASKS_PATH"

"$ACCEPTANCE" \
  -config "$TEMPLATE_PATH" \
  -prepare-narrow-task-pool "$TASKS_PATH" \
  -output "$CONFIG_PATH"

echo "prepared 20 ACTIVE V4 roots without running the acceptance campaign"
echo "task pool: $TASKS_PATH"
echo "acceptance config: $CONFIG_PATH"
