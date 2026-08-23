#!/bin/bash
# Verify the bundle's image tarballs against SHA256SUMS and load them into the
# local Docker engine. Runs directly on the host (no toolbox yet: this is the
# step that loads it).
set -euo pipefail
cd "$(dirname "$0")/.."

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }

echo "== verifying bundle digests"
( cd images && sha256sum --check --strict SHA256SUMS )

echo "== loading images"
for archive in images/*.tar.gz; do
  echo "-- $archive"
  gunzip -c "$archive" | docker load
done

echo "== loaded taskgate images"
docker image ls --format '{{.Repository}}:{{.Tag}}  {{.Size}}' \
  | grep -E 'taskgate|postgres:16-bookworm|minio' || true
echo "done; continue with 01-preflight.sh"
