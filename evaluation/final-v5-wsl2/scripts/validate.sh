#!/usr/bin/env bash
set -euo pipefail
go run ./evaluation/cmd/final-v5 validate --root evaluation/final-v5-wsl2
# The per-profile Catalogs and the profile registry are generated, so prove
# they still regenerate byte-identically from the contracts and the Catalog.
go run ./evaluation/cmd/final-v5-profile -verify
echo "final V5 workload-closure profile regeneration: pass"

# activation_supported is per profile and comes only from recorded live evidence.
# This re-derives every registry state from the committed manifest, so a
# hand-edited readiness state fails the build.
go run ./evaluation/cmd/final-v5-activation-support -verify
echo "final V5 per-profile activation support: pass"

# Every SQL and plan artifact the Contract Index names must actually parse,
# execute or compile. Digest and structure checks alone let three releases ship
# a dataset probe PostgreSQL could not parse; see contracts/AMENDMENT-v1.3.md.
# Without a disposable PostgreSQL this reports SKIPPED and claims nothing.
go run ./evaluation/cmd/final-v5-contract-sql-check
evaluation/final-v5-wsl2/scripts/validate-pilot-harness.sh
