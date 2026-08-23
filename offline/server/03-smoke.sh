#!/bin/bash
# Synthetic scheduler/schema smoke (always publication_eligible=false).
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"
log_to 03-smoke

cd "$REPO"
make eval-v5-final-smoke
echo "03-smoke: pass"
