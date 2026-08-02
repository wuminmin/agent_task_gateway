#!/usr/bin/env bash
set -euo pipefail

(( $# == 1 )) || { echo "usage: rq5-secret-root-cleanup.sh SECRET_ROOT" >&2; exit 2; }
secret_root="$1"
[[ "$secret_root" =~ ^/tmp/taskgate-rq5-secrets\.deployment-0[1-3]\.[A-Za-z0-9]+$ ]] || {
  echo "refusing unsafe RQ5 secret-root cleanup target" >&2
  exit 2
}
if [[ ! -e "$secret_root" && ! -L "$secret_root" ]]; then
  exit 0
fi
[[ -d "$secret_root" && ! -L "$secret_root" ]] || {
  echo "RQ5 secret root is not a non-symlink directory" >&2
  exit 1
}

# The target is an exact mktemp-created deployment directory under /tmp. It
# contains only ephemeral source copies and secret state, never evidence.
rm --recursive --force --one-file-system -- "$secret_root"
[[ ! -e "$secret_root" && ! -L "$secret_root" ]] || {
  echo "RQ5 secret root survived cleanup" >&2
  exit 1
}
