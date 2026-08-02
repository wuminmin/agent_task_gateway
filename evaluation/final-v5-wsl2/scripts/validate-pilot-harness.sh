#!/usr/bin/env bash
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

fresh_script=evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh
smoke_script=evaluation/final-v5-wsl2/scripts/run-pilot.sh
real_script=evaluation/final-v5-wsl2/scripts/run-real-pilot.sh

grep -Fq '"${compose[@]}" config --no-interpolate > "$compose_config_output"' "$fresh_script"
! grep -Fq '"${compose[@]}" config > "$compose_config_output"' "$fresh_script"
grep -Fq 'go build -buildvcs=false -o "$adapter_bin"' "$smoke_script"
grep -Fq 'go build -trimpath -buildvcs=false -o "$adapter_bin"' "$real_script"

tmp_dir="$(mktemp -d /tmp/taskgate-final-v5-harness.XXXXXX)"
trap 'rm -rf "$tmp_dir"' EXIT
failing_runner="$tmp_dir/failing-smoke-runner.sh"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -eu' \
  'run_dir="$1"' \
  'mkdir -p "$run_dir/generated/latex"' \
  "printf '%s\\n' 'publication_eligible=false' > \"\$run_dir/PILOT-NOT-FOR-PUBLICATION\"" \
  ': > "$run_dir/generated/latex/evidence.tex"' \
  'exit 23' > "$failing_runner"
chmod 700 "$failing_runner"

set +e
make --no-print-directory eval-v5-final-smoke \
  FINAL_V5_SMOKE_RUNNER="$failing_runner" >/dev/null 2>&1
smoke_rc=$?
set -e
(( smoke_rc != 0 )) || {
  echo "final V5 smoke target swallowed its runner failure" >&2
  exit 1
}

echo "final V5 Pilot harness validation: pass"
