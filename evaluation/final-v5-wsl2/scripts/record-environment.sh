#!/usr/bin/env bash
set -euo pipefail
: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_DEPLOYMENT_ID:?TASKGATE_DEPLOYMENT_ID is required}"
: "${TASKGATE_ENVIRONMENT_OUTPUT:?TASKGATE_ENVIRONMENT_OUTPUT is required}"
repo="$(git rev-parse --show-toplevel)"
eligible=false
[[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]] && eligible=true
args=(record-environment --repo "$repo" --campaign-id "$TASKGATE_CAMPAIGN_ID" --deployment-id "$TASKGATE_DEPLOYMENT_ID" --output "$TASKGATE_ENVIRONMENT_OUTPUT")
[[ "$eligible" == true ]] && args+=(--publication-eligible --fresh-deployment-proof "$TASKGATE_FRESH_PROOF_OUTPUT")
[[ -n "${TASKGATE_DATASET_BINDINGS:-}" ]] && args+=(--dataset-bindings "$TASKGATE_DATASET_BINDINGS")
go run ./evaluation/cmd/final-v5 "${args[@]}"
