#!/bin/bash
# Offline-server preflight: host capability checks, repo clone from the git
# bundle, Compose .env installation, deterministic image retags, and an
# offline Go sanity build. Idempotent; run again after fixing any failure.
set -euo pipefail
source "$(dirname "$0")/common.sh"

fail() { echo "preflight failure: $*" >&2; exit 1; }
warn() { echo "preflight warning: $*" >&2; }

[ -d "$BUNDLE" ] || fail "bundle not found at $BUNDLE (upload it there or set TASKGATE_OFFLINE_ROOT)"
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "the Docker daemon is unavailable"
docker image inspect "$TOOLBOX_IMAGE" >/dev/null 2>&1 || fail "toolbox image missing; run 00-load-images.sh first"

echo "== host checks"
[ "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs ] \
  || fail "cgroup v2 is required (found $(stat -fc %T /sys/fs/cgroup)); boot with systemd.unified_cgroup_hierarchy=1"
if command -v getenforce >/dev/null 2>&1 && [ "$(getenforce)" = Enforcing ]; then
  warn "SELinux is Enforcing; if containers cannot read bind mounts run: chcon -R -t container_file_t $ROOT"
fi
free_kib="$(df -Pk "$ROOT" | awk 'NR==2{print $4}')"
[ "$free_kib" -ge $((40 * 1024 * 1024)) ] || fail "less than 40 GiB free under $ROOT"
if command -v ss >/dev/null 2>&1; then
  for port in 8082 8092 25433; do
    ss -ltn "( sport = :$port )" | grep -q ":$port" && fail "loopback port $port is already in use" || true
  done
else
  warn "ss unavailable; skipped the free-port check for 8082/8092/25433"
fi

echo "== repo clone"
source "$BUNDLE/BUNDLE-INFO"
toolbox() {
  docker run --rm --network host --security-opt label=disable \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$ROOT:$ROOT" -w "$ROOT" "$TOOLBOX_IMAGE" bash -ec "$1"
}
if [ ! -d "$REPO/.git" ]; then
  toolbox "git clone --branch '$bundle_branch' '$BUNDLE/repo/repo.bundle' '$REPO'"
fi
actual_commit="$(toolbox "git -C '$REPO' rev-parse HEAD")"
[ "$actual_commit" = "$bundle_commit" ] || fail "repo is at $actual_commit, bundle expects $bundle_commit"
install -m 600 "$BUNDLE/secrets/.env" "$REPO/.env"
mkdir -p "$RESULTS/logs"

echo "== deterministic image retags"
pilot_project="$(project_name "$PILOT_CAMPAIGN" "$DEPLOYMENT_ID")"
for project in "$pilot_project"; do
  docker tag "taskgate-offline/gateway:$app_image_tag" "$project-gateway"
  docker tag "taskgate-offline/oa-demo:$app_image_tag" "$project-oa-demo"
  docker tag "taskgate-offline/snapshot-index:$app_image_tag" "$project-snapshot-index-detail"
  docker tag "taskgate-offline/snapshot-index:$app_image_tag" "$project-snapshot-index-summary"
  docker tag "taskgate-offline/snapshot-index:$app_image_tag" "$project-snapshot-index-result-heavy"
  docker tag "taskgate-offline/snapshot-index:$app_image_tag" "$project-snapshot-index-exposure-scale"
  docker tag "taskgate-offline/snapshot-sidecar-install:$app_image_tag" "$project-snapshot-sidecar-install"
  echo "tagged services for Compose project $project"
done

echo "== offline Go sanity (GOPROXY=off resolve from the baked caches)"
toolbox "cd '$REPO' && go build ./... && go version"

echo "== harness preflight (pilot mode, offline host class)"
toolbox "cd '$REPO' && TASKGATE_PILOT_HOST_CLASS=offline-linux \
  evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot"

echo "preflight passed; continue with 02-validate.sh"
