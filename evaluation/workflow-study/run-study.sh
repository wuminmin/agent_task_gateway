#!/bin/sh
set -eu

REPOSITORY_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$REPOSITORY_ROOT"
exec python3 evaluation/workflow-study/run_study.py "$@"
