#!/bin/bash
# Optional: legacy evaluation smoke track. Needs the bundle built with
# --with-eval (taskgate-evaluation:local preloaded). Experimental on offline
# hosts; the primary runbook is 02-06.
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"
log_to 07-optional-eval

cd "$REPO"
docker image inspect taskgate-evaluation:local >/dev/null 2>&1 \
  || { echo "taskgate-evaluation:local not loaded; rebuild the bundle with --with-eval" >&2; exit 1; }
TASKGATE_EVAL_SKIP_BUILD=1 ./evaluation/run.sh smoke
echo "07-optional-eval: pass"
