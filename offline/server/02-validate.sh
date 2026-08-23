#!/bin/bash
# Offline validation: contract/schema validation plus byte-identical profile
# Catalog regeneration. Pure Go + shell; no Compose deployment involved.
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"
log_to 02-validate

cd "$REPO"
make eval-v5-final-validate
echo "02-validate: pass"
