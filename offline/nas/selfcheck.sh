#!/bin/sh
# Pre-ship verification of a built bundle, run on the NAS. Confirms digest
# integrity, git-bundle validity, Compose interpolation against the generated
# .env, and that the toolbox resolves the entire build offline (GOPROXY=off).
#
# usage: selfcheck.sh BUNDLE_DIR
set -eu

repo="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"
bundle="${1:?usage: selfcheck.sh BUNDLE_DIR}"
bundle="$(CDPATH= cd -- "$bundle" && pwd)"

fail() { echo "selfcheck failure: $*" >&2; exit 1; }

echo "== bundle digests"
( cd "$bundle/images" && sha256sum --check --strict SHA256SUMS ) || fail "digest mismatch"

echo "== git bundle"
git bundle verify "$bundle/repo/repo.bundle" || fail "repo.bundle is not usable"
. "$bundle/BUNDLE-INFO"
[ "$(git -C "$repo" rev-parse HEAD)" = "$bundle_commit" ] \
  || fail "bundle commit $bundle_commit differs from the checked-out HEAD"

echo "== required images present locally"
for image in "taskgate-offline/gateway:$app_image_tag" \
             "taskgate-offline/oa-demo:$app_image_tag" \
             "taskgate-offline/snapshot-index:$app_image_tag" \
             "taskgate-offline/snapshot-sidecar-install:$app_image_tag" \
             taskgate-tla:1.7.1 taskgate-toolbox:offline \
             postgres:16-bookworm; do
  docker image inspect "$image" >/dev/null 2>&1 || fail "image not built/loaded: $image"
done

echo "== compose interpolation with the bundle .env"
tmp_env="$(mktemp /tmp/taskgate-selfcheck-env.XXXXXX)"
trap 'rm -f "$tmp_env"' EXIT INT TERM
cp "$bundle/secrets/.env" "$tmp_env"
docker compose --project-name taskgate-offline-selfcheck \
  --env-file "$tmp_env" \
  --file "$repo/compose.yaml" --file "$repo/compose.debug.yaml" \
  --file "$repo/evaluation/final-v5-wsl2/compose.real-pilot.yaml" \
  config --quiet || fail "compose config rejected the generated .env"

echo "== toolbox offline resolution (GOPROXY=off)"
docker run --rm -v "$repo:/selfcheck/src:ro" -w /selfcheck/src \
  taskgate-toolbox:offline \
  sh -ec 'cp -r /selfcheck/src /selfcheck/build && cd /selfcheck/build && go build ./...' \
  || fail "toolbox could not build the repo offline"

echo "selfcheck passed: $bundle is ready to upload"
