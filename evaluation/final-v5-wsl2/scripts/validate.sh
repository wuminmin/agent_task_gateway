#!/usr/bin/env bash
set -euo pipefail
go run ./evaluation/cmd/final-v5 validate --root evaluation/final-v5-wsl2
# The per-profile Catalogs and the profile registry are generated, so prove
# they still regenerate byte-identically from the contracts and the Catalog.
go run ./evaluation/cmd/final-v5-profile -verify
echo "final V5 workload-closure profile regeneration: pass"
evaluation/final-v5-wsl2/scripts/validate-pilot-harness.sh
