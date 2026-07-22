#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_ARTIFACT_IMAGE:-taskgate-evaluation-artifacts:local}
ALLOW_EMPTY=0

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [--allow-empty]" >&2
  exit 2
fi
if [ "$#" -eq 1 ]; then
  [ "$1" = "--allow-empty" ] || { echo "usage: $0 [--allow-empty]" >&2; exit 2; }
  ALLOW_EMPTY=1
fi

command -v docker >/dev/null 2>&1 || { echo "artifact generation requires Docker" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable or not permitted" >&2; exit 1; }

docker build --file "$SCRIPT_DIR/Dockerfile" --target artifacts --tag "$IMAGE" "$ROOT_DIR"

set -- docker run --rm \
  --user "$(id -u):$(id -g)" \
  --volume "$ROOT_DIR:/workspace:ro" \
  --volume "$SCRIPT_DIR/generated:/workspace/evaluation/generated" \
  "$IMAGE" --root /workspace --raw-root /workspace/evaluation/raw \
  --output /workspace/evaluation/generated

if [ "$ALLOW_EMPTY" = 1 ]; then
  set -- "$@" --allow-empty
fi

if [ -n "${EVAL_RAW_RUNS:-}" ]; then
  old_ifs=$IFS
  IFS=:
  for run_dir in $EVAL_RAW_RUNS; do
    case "$run_dir" in
      /workspace/*) container_path=$run_dir ;;
      /*) echo "EVAL_RAW_RUNS paths must be under the repository: $run_dir" >&2; exit 2 ;;
      *) container_path="/workspace/$run_dir" ;;
    esac
    set -- "$@" --run-dir "$container_path"
  done
  IFS=$old_ifs
fi

"$@"
