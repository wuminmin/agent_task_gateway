#!/usr/bin/env bash
set -euo pipefail
run_dir="${1:?run directory is required}"
go run ./evaluation/cmd/final-v5 finalize --run-dir "$run_dir"
