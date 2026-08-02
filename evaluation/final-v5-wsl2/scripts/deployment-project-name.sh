#!/usr/bin/env bash
set -euo pipefail

(( $# == 2 )) || { echo "usage: deployment-project-name.sh CAMPAIGN_ID DEPLOYMENT_ID" >&2; exit 2; }
campaign_id="$1"
deployment_id="$2"
[[ "$campaign_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ && "$deployment_id" =~ ^deployment-0[1-3]$ ]] || {
  echo "invalid final-V5 campaign/deployment identity" >&2
  exit 2
}

# Hash the complete, length-unambiguous identity. Human-readable truncation or
# case folding can alias two campaigns and make exact `down --volumes` delete
# another run, whereas this fixed form stays well below Docker's name limit.
identity_sha256="$(
  printf '%s\0%s' "$campaign_id" "$deployment_id" | sha256sum | awk '{print $1}'
)"
[[ "$identity_sha256" =~ ^[0-9a-f]{64}$ ]] || { echo "failed to derive deployment project identity" >&2; exit 1; }
printf 'taskgate-final-v5-%s-%s\n' "$deployment_id" "${identity_sha256:0:20}"
