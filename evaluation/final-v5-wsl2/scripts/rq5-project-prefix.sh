#!/usr/bin/env bash
set -euo pipefail

(( $# == 2 )) || { echo "usage: rq5-project-prefix.sh CAMPAIGN_ID DEPLOYMENT_ID" >&2; exit 2; }
campaign_id="$1"
deployment_id="$2"
[[ -n "$campaign_id" && "$deployment_id" =~ ^deployment-0[1-3]$ ]] || {
  echo "invalid RQ5 campaign/deployment identity" >&2
  exit 2
}

# Hash the complete, NUL-delimited identity. The fixed 20-hex suffix keeps
# all later Compose project/container derivations below Docker's 63-byte cap
# without the collisions caused by truncating a human-readable slug.
identity_sha256="$(
  printf '%s\0%s' "$campaign_id" "$deployment_id" | sha256sum | awk '{print $1}'
)"
[[ "$identity_sha256" =~ ^[0-9a-f]{64}$ ]] || { echo "failed to derive RQ5 project identity" >&2; exit 1; }
printf 'rq5-%s\n' "${identity_sha256:0:20}"
