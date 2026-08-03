#!/usr/bin/env bash
# Thin wrapper around evaluation/cmd/final-v5-profile-activate.
#
# All JSON generation, digest verification and acceptance logic lives in the Go
# activator. This script only forwards arguments so an operator can activate one
# profile without remembering the flag set.
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

usage() {
  cat >&2 <<'USAGE'
usage: activate-profile.sh --compose-project P --deployment-id D --profile-id ID \
                           --evidence-out FILE [--previous-profile-id ID] \
                           [--outside-products A,B] [--activation-sequence N] \
                           [--dataset-binding FILE] [--compose-files a:b]
USAGE
  exit 2
}

[[ $# -gt 0 ]] || usage

exec go run ./evaluation/cmd/final-v5-profile-activate --root "$repo" "$@"
