#!/bin/bash
# Collect everything the author needs back on the NAS into one tarball:
# final-v5 raw run directories, formal results, run logs, and an environment
# report. The tarball never contains .env or any other secret.
set -euo pipefail
source "$(dirname "$0")/common.sh"
reexec_in_toolbox "$@"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RESULTS"
report="$RESULTS/environment-report-$timestamp.txt"
{
  echo "collected_at=$timestamp"
  echo "commit=$(git -C "$REPO" rev-parse HEAD)"
  echo "worktree_status:"
  git -C "$REPO" status --porcelain=v1 --untracked-files=all || true
  echo "uname=$(uname -a)"
  docker version 2>/dev/null || true
  docker compose version 2>/dev/null || true
  echo "cgroup_fs=$(stat -fc %T /sys/fs/cgroup)"
} > "$report"

archive="$RESULTS/taskgate-results-$timestamp.tar.gz"
tar_args=(--exclude='*.env')
for path in evaluation/final-v5-wsl2/raw formal/results; do
  [ -d "$REPO/$path" ] && tar_args+=(-C "$REPO" "$path")
done
tar_args+=(-C "$ROOT" "results/logs" "results/$(basename "$report")")
tar czf "$archive" "${tar_args[@]}"

sha256sum "$archive"
echo "download $archive back to the NAS"
