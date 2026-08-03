#!/usr/bin/env bash
set -euo pipefail
repo="$(git rev-parse --show-toplevel)"
cd "$repo"
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode smoke
run_dir="${1:-evaluation/final-v5-wsl2/raw/pilot-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ ! -e "$run_dir" ]] || { echo "pilot output already exists" >&2; exit 1; }
mkdir -m 700 -p "$run_dir/raw"
install -m 600 evaluation/final-v5-wsl2/config/smoke.example.json "$run_dir/config.json"
printf '%s\n' 'publication_eligible=false' > "$run_dir/PILOT-NOT-FOR-PUBLICATION"
adapter_bin="$(mktemp /tmp/taskgate-final-v5-smoke-adapter.XXXXXX)"
trap 'rm -f "$adapter_bin"' EXIT
go build -buildvcs=false -o "$adapter_bin" ./evaluation/cmd/v5-smoke-adapter
TASKGATE_EXPERIMENT_CLASS=pilot TASKGATE_CAMPAIGN_ID=smoke-local-only \
  go run ./evaluation/cmd/v5-full -config "$run_dir/config.json" \
  -deployment-id deployment-01 -adapter "$adapter_bin" -output "$run_dir/raw/deployment-01.jsonl"
go run ./evaluation/cmd/final-v5 finalize --run-dir "$run_dir" --allow-incomplete-pilot >/dev/null
echo "$run_dir"
