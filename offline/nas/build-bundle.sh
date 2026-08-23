#!/bin/sh
# Build the complete offline execution bundle on an internet-connected host.
#
# Produces a directory the operator uploads to the air-gapped server: docker
# image tarballs (with a sha256 manifest), a git bundle of the exact commit,
# a generated Compose .env, the server run scripts, and tla2tools.jar as
# rebuild insurance. Everything the server needs; nothing it must download.
#
# usage: build-bundle.sh [--with-eval] [OUTPUT_DIR]
set -eu

repo="$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)"
cd "$repo"

with_eval=0
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --with-eval) with_eval=1; shift ;;
    -*) echo "unknown option: $1" >&2; exit 2 ;;
    *) out="$1"; shift ;;
  esac
done
[ -n "$out" ] || out="$repo/../taskgate-offline-bundle-$(date -u +%Y%m%d)"

fail() { echo "build-bundle: $*" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker info >/dev/null 2>&1 || fail "the Docker daemon is unavailable"
command -v git >/dev/null 2>&1 || fail "git is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"

[ -z "$(git status --porcelain=v1 --untracked-files=all)" ] \
  || fail "the worktree must be clean; the bundle is bound to one exact commit"
commit="$(git rev-parse HEAD)"
branch="$(git rev-parse --abbrev-ref HEAD)"
tag="${commit%"${commit#????????????}"}"

mkdir -p "$out/images" "$out/repo" "$out/secrets" "$out/third-party" "$out/server"

echo "== repo bundle ($branch @ $commit)"
git bundle create "$out/repo/repo.bundle" HEAD "$branch"

echo "== secrets"
[ -f "$out/secrets/.env" ] || sh offline/nas/make-env.sh "$out/secrets/.env"

echo "== application images (tag $tag)"
for target in gateway oa-demo snapshot-index snapshot-sidecar-install; do
  docker build --build-arg "TARGET=$target" -t "taskgate-offline/$target:$tag" .
done

echo "== formal TLC image"
docker build --file formal/Dockerfile --tag taskgate-tla:1.7.1 .

echo "== toolbox image"
docker build --file offline/toolbox.Dockerfile --tag taskgate-toolbox:offline .

echo "== base images"
for image in postgres:16-bookworm \
             minio/minio:RELEASE.2025-04-22T22-12-26Z \
             minio/mc:RELEASE.2025-04-16T18-13-26Z; do
  docker pull "$image"
done

if [ "$with_eval" = 1 ]; then
  echo "== evaluation runner image"
  docker build --file evaluation/Dockerfile --target runner --tag taskgate-evaluation:local .
fi

echo "== tla2tools.jar (rebuild insurance)"
container="$(docker create taskgate-tla:1.7.1)"
docker cp "$container:/opt/tla2tools.jar" "$out/third-party/tla2tools.jar"
docker rm "$container" >/dev/null

echo "== docker save"
save_images() {
  archive="$1"; shift
  if [ ! -f "$out/images/$archive" ]; then
    docker save "$@" | gzip > "$out/images/$archive.partial"
    mv "$out/images/$archive.partial" "$out/images/$archive"
  fi
  echo "saved $archive"
}
save_images "taskgate-app-$tag.tar.gz" \
  "taskgate-offline/gateway:$tag" "taskgate-offline/oa-demo:$tag" \
  "taskgate-offline/snapshot-index:$tag" "taskgate-offline/snapshot-sidecar-install:$tag"
save_images "taskgate-tla-1.7.1.tar.gz" taskgate-tla:1.7.1
save_images "taskgate-toolbox-offline.tar.gz" taskgate-toolbox:offline
save_images "postgres-16-bookworm.tar.gz" postgres:16-bookworm
save_images "minio.tar.gz" minio/minio:RELEASE.2025-04-22T22-12-26Z minio/mc:RELEASE.2025-04-16T18-13-26Z
[ "$with_eval" = 0 ] || save_images "taskgate-evaluation-local.tar.gz" taskgate-evaluation:local

echo "== manifest"
{
  echo "bundle_commit=$commit"
  echo "bundle_branch=$branch"
  echo "app_image_tag=$tag"
  echo "created_at=$(date -u +%FT%TZ)"
} > "$out/BUNDLE-INFO"
( cd "$out/images" && sha256sum ./*.tar.gz > SHA256SUMS )
( cd "$out" && sha256sum repo/repo.bundle third-party/tla2tools.jar >> images/SHA256SUMS )

echo "== server scripts"
cp offline/server/*.sh offline/server/README.md "$out/server/"
chmod 755 "$out/server/"*.sh

echo "bundle complete: $out"
du -sh "$out"
