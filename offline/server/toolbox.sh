#!/bin/bash
# Interactive toolbox shell for debugging: Go toolchain, make, jq, git,
# docker CLI + compose, warmed offline Go caches, repo mounted at its host
# path. Usage: toolbox.sh [command...]
set -euo pipefail
source "$(dirname "$0")/common.sh"

exec docker run --rm -it --network host --security-opt label=disable \
  -e TASKGATE_IN_TOOLBOX=1 \
  -e "TASKGATE_OFFLINE_ROOT=$ROOT" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$ROOT:$ROOT" \
  -w "$REPO" \
  "$TOOLBOX_IMAGE" "${@:-bash}"
