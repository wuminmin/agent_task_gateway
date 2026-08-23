#!/bin/bash
# Shared setup for the offline server run scripts. Sourced, never executed.
#
# Expected layout on the server (override the root with TASKGATE_OFFLINE_ROOT):
#   /opt/taskgate/bundle/              the uploaded bundle (this file lives in
#                                      bundle/server/)
#   /opt/taskgate/agent_task_gateway/  repo clone made by 01-preflight.sh
#   /opt/taskgate/results/             logs and collected outputs
set -euo pipefail

ROOT="${TASKGATE_OFFLINE_ROOT:-/opt/taskgate}"
BUNDLE="$ROOT/bundle"
REPO="$ROOT/agent_task_gateway"
RESULTS="$ROOT/results"
TOOLBOX_IMAGE="${TASKGATE_TOOLBOX_IMAGE:-taskgate-toolbox:offline}"

# The deterministic local-only Compose deployment this runbook uses.
PILOT_CAMPAIGN=pilot-local-only
DEPLOYMENT_ID=deployment-01

# reexec_in_toolbox: scripts call this first. Outside the toolbox it re-runs
# the same script inside the toolbox container with the docker socket and the
# offline root mounted at its host path (Compose bind mounts resolve on the
# host, so path identity is load-bearing). Inside, it returns.
reexec_in_toolbox() {
  if [ -n "${TASKGATE_IN_TOOLBOX:-}" ]; then
    return 0
  fi
  local self
  self="$(realpath "${BASH_SOURCE[1]}")"
  case "$self" in
    "$ROOT"/*) ;;
    *) echo "run scripts from inside $ROOT (found $self)" >&2; exit 2 ;;
  esac
  exec docker run --rm --network host --security-opt label=disable \
    -e TASKGATE_IN_TOOLBOX=1 \
    -e "TASKGATE_OFFLINE_ROOT=$ROOT" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$ROOT:$ROOT" \
    -w "$ROOT" \
    "$TOOLBOX_IMAGE" bash "$self" "$@"
}

# log_to NAME: tee all further output of the calling script into a timestamped
# log under results/logs.
log_to() {
  mkdir -p "$RESULTS/logs"
  local log="$RESULTS/logs/$1-$(date -u +%Y%m%dT%H%M%SZ).log"
  echo "logging to $log"
  exec > >(tee -a "$log") 2>&1
}

project_name() {
  bash "$REPO/evaluation/final-v5-wsl2/scripts/deployment-project-name.sh" "$1" "$2"
}
