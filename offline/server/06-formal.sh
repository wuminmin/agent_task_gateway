#!/bin/bash
# Formal TLA+/TLC campaign against the preloaded taskgate-tla:1.7.1 image.
# Run this AFTER 04/05: it rewrites the tracked formal/results/ files, which
# dirties the worktree that the real pilot requires clean.
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"
log_to 06-formal

cd "$REPO"
TASKGATE_TLA_SKIP_BUILD=1 make formal
echo "06-formal: pass (results in formal/results/)"
