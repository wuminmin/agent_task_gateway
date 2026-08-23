#!/bin/bash
# Real-system pilot against a fresh Compose deployment, using the preloaded
# images. Deliberately calls the pilot script directly instead of the make
# target: the make target forces TASKGATE_REAL_PILOT_BUILD=1, and an offline
# host must never rebuild (`up --no-build` uses the retagged bundle images).
#
# The run directory lands in evaluation/final-v5-wsl2/raw/real-pilot-<ts>/
# (gitignored) and its PASS re-establishes pilot evidence for this commit.
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"
log_to 04-real-pilot

cd "$REPO"
export TASKGATE_PILOT_HOST_CLASS=offline-linux
evaluation/final-v5-wsl2/scripts/run-real-pilot.sh
echo "04-real-pilot: pass (run directory printed above)"
