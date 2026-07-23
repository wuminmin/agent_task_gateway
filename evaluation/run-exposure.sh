#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EXPOSURE_EVAL_IMAGE:-taskgate-exposure-evaluation:local}

command -v docker >/dev/null 2>&1 || {
  echo "exposure evaluation failed: Docker is required" >&2
  exit 1
}
docker info >/dev/null 2>&1 || {
  echo "exposure evaluation failed: Docker daemon is unavailable" >&2
  exit 1
}

docker build --file "$SCRIPT_DIR/Dockerfile" --target exposure --tag "$IMAGE" "$ROOT_DIR"
tmp=$(mktemp /tmp/taskgate-exposure-eval.XXXXXX)
trap 'rm -f "$tmp"' EXIT HUP INT TERM
docker run --rm "$IMAGE" >"$tmp"
cat "$tmp"
cp "$tmp" "$SCRIPT_DIR/exposure/results.json"
