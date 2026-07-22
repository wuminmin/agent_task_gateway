#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
IMAGE=${TASKGATE_EVAL_IMAGE:-taskgate-evaluation:local}
ENV_FILE=${EVAL_ENV_FILE:-$SCRIPT_DIR/.env}

if [ "$#" -ne 1 ]; then
  echo "usage: $0 tpch|tpcds" >&2
  exit 2
fi
case "$1" in tpch|tpcds) family=$1 ;; *) echo "family must be tpch or tpcds" >&2; exit 2 ;; esac
[ -f "$ENV_FILE" ] || { echo "AST gateway requires EVAL_ENV_FILE (default evaluation/.env)" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "Docker is required" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is unavailable or not permitted" >&2; exit 1; }

docker build --file "$SCRIPT_DIR/Dockerfile" --target runner --tag "$IMAGE" "$ROOT_DIR"
exec docker run --rm \
  --network "${EVAL_AST_DOCKER_NETWORK:-bridge}" \
  --add-host host.docker.internal:host-gateway \
  --env-file "$ENV_FILE" \
  --publish "127.0.0.1:${EVAL_AST_PORT:-8088}:8088" \
  --volume "$ROOT_DIR:/workspace:ro" \
  --entrypoint /usr/local/bin/ast-gateway \
  "$IMAGE" -config "/workspace/evaluation/ast-gateway/$family.json"
