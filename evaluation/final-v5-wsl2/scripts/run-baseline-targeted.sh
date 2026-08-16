#!/usr/bin/env bash
# Targeted execution of Baseline S1 and S2's twenty frozen cells against a
# fresh isolated full topology.
#
# NOT a Campaign and NOT publication-eligible. It runs with
# campaign_class=pilot and pilot_kind=baseline_targeted, changes no capability
# and no contract state, and Baseline stays 20 of 58 with its capability false.
#
# # Why this is a separate launcher
#
# run-real-pilot.sh drives the retained non-publication S1/tiny Pilot and
# asserts that Pilot's exact five-sample shape, including its pending_recovery
# diagnostic. Those assertions are its evidence and must not be relaxed to let
# a different selection through, so the bring-up is shared by copy and the
# acceptance is written for the cells actually being run.
#
# Unlike run-artifact-targeted.sh this does not activate a profile Catalog. The
# Gateway serves the master ./config/catalog.yaml, where S1's [provsql_orders]
# and S2's [provsql_orders, provsql_lineitem] closures resolve through the
# default low route. The Task grant is still exactly the cell's Products; the
# deployment simply publishes more Products than the cell's registered profile
# closure, which is why this is a targeted run and not a profile activation.
#
# TASKGATE_BASELINE_WARMUPS and TASKGATE_BASELINE_SAMPLES override the frozen
# config's counts so a smoke can run one sample before a longer measurement.
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot
[[ -f .env ]] || { echo "targeted Baseline run requires the local Compose .env" >&2; exit 2; }

run_dir="${1:-evaluation/final-v5-wsl2/raw/baseline-targeted-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ ! -e "$run_dir" ]] || { echo "targeted Baseline output already exists" >&2; exit 1; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "targeted Baseline run requires a clean worktree for commit-bound evidence" >&2
  exit 1
}
submission_commit="$(git rev-parse HEAD)"
mkdir -m 700 "$run_dir"
mkdir -m 700 "$run_dir/raw" "$run_dir/environment"
jq --arg submission_commit "$submission_commit" \
   --argjson warmups "${TASKGATE_BASELINE_WARMUPS:-0}" \
   --argjson samples "${TASKGATE_BASELINE_SAMPLES:-1}" \
   '.submission_commit = $submission_commit | .warmups = $warmups | .samples = $samples' \
  evaluation/final-v5-wsl2/config/baseline-targeted.example.json > "$run_dir/config.json"
chmod 600 "$run_dir/config.json"
printf '%s\n' 'publication_eligible=false' 'real_system=true' 'scope=baseline-S1-S2' > "$run_dir/PILOT-NOT-FOR-PUBLICATION"

export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_CAMPAIGN_ID=pilot-local-only
export TASKGATE_DEPLOYMENT_ID=deployment-01
export COMPOSE_PROJECT_NAME="$(
  bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh \
    "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"
)"
export TASKGATE_COMPOSE_FILES=compose.yaml:compose.debug.yaml:evaluation/final-v5-wsl2/compose.real-pilot.yaml
export TASKGATE_FRESH_PROOF_OUTPUT="$run_dir/environment/deployment-01.fresh.json"

compose=(docker compose --project-name "$COMPOSE_PROJECT_NAME" --file compose.yaml --file compose.debug.yaml --file evaluation/final-v5-wsl2/compose.real-pilot.yaml)
compose_json="$("${compose[@]}" config --format json)"
service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<< "$compose_json"; }
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
export GATEWAY_OBJECT_STORE_BUCKET="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
[[ -n "$GATEWAY_OBJECT_STORE_BUCKET" ]] || { echo "Compose omitted the result bucket" >&2; exit 1; }
adapter_bin="$(mktemp /tmp/taskgate-final-v5-real-adapter.XXXXXX)"
cleanup() {
  status=$?
  rm -f "$adapter_bin"
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

if [[ "${TASKGATE_REAL_PILOT_BUILD:-0}" == 1 ]]; then
  "${compose[@]}" build
fi
evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh

# Resolve the exact runtime values Compose already consumed without sourcing
# .env as shell code. The JSON remains in-process and is never written to the
# evidence directory.
alice_token="$(service_env gateway TASKBOUND_ALICE_TOKEN)"
carol_token="$(service_env gateway TASKBOUND_CAROL_TOKEN)"
alice_password="$(service_env oa-demo OA_ALICE_PASSWORD)"
bob_password="$(service_env oa-demo OA_BOB_PASSWORD)"
control_password="$(service_env control-postgres POSTGRES_PASSWORD)"
control_database="$(service_env control-postgres POSTGRES_DB)"
business_password="$(service_env gateway GATEWAY_DB_PASSWORD)"
business_database="$(service_env business-postgres POSTGRES_DB)"
business_admin_password="$(service_env business-postgres POSTGRES_PASSWORD)"
object_access_key="$(service_env gateway GATEWAY_OBJECT_STORE_ACCESS_KEY)"
object_secret_key="$(service_env gateway GATEWAY_OBJECT_STORE_SECRET_KEY)"
object_bucket="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
control_port="$("${compose[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
business_port="$("${compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
object_port="$("${compose[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
for value in "$alice_token" "$carol_token" "$alice_password" "$bob_password" "$control_password" "$control_database" "$business_password" "$business_database" "$business_admin_password" "$object_access_key" "$object_secret_key" "$object_bucket" "$control_port" "$business_port" "$object_port"; do
  [[ -n "$value" ]] || { echo "Compose omitted a required real-Pilot binding" >&2; exit 1; }
done
export TASKBOUND_ALICE_TOKEN="$alice_token" TASKBOUND_CAROL_TOKEN="$carol_token"
export OA_ALICE_PASSWORD="$alice_password" OA_BOB_PASSWORD="$bob_password"
export TASKGATE_FINAL_V5_CONTROL_DSN="postgres://postgres:$(urlencode "$control_password")@127.0.0.1:$control_port/$(urlencode "$control_database")?sslmode=disable"
export TASKGATE_FINAL_V5_BUSINESS_DSN="postgres://gateway_reader:$(urlencode "$business_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
export TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN="postgres://postgres:$(urlencode "$business_admin_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
export TASKGATE_FINAL_V5_GATEWAY_URL=http://127.0.0.1:8082
export TASKGATE_FINAL_V5_OA_URL=http://127.0.0.1:8092
export TASKGATE_FINAL_V5_OBJECT_STORE_URL="http://127.0.0.1:$object_port"
export TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY="$object_access_key"
export TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY="$object_secret_key"
export TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET="$object_bucket"

go build -trimpath -buildvcs=false -o "$adapter_bin" ./evaluation/cmd/final-v5-adapter
sha256sum "$adapter_bin" | awk '{print $1}' > "$run_dir/adapter.sha256"
go run ./evaluation/cmd/v5-full -config "$run_dir/config.json" -deployment-id deployment-01 \
  -adapter "$adapter_bin" -output "$run_dir/raw/deployment-01.jsonl"
go run ./evaluation/cmd/final-v5 finalize --run-dir "$run_dir" >/dev/null
jq -e --arg submission_commit "$submission_commit" \
  '.status == "pass" and .publication_eligible == false and
   .submission_commit == $submission_commit and (.reasons | length) == 0' \
  "$run_dir/generated/summary.json" >/dev/null

# Acceptance for the cells this launcher runs. Every sample must pass, the
# Direct arm must be plain PostgreSQL and the four governed modes must be
# TaskGate, and the replay modes must leave the database untouched -- which is
# the result the governed-overhead comparison rests on.
samples="${TASKGATE_BASELINE_SAMPLES:-1}"
jq -s -e --argjson samples "$samples" '
  def stable_business($before; $after):
    ($before | type == "object") and ($after | type == "object") and
    $before.stats_reset_unix_micro > 0 and
    $before.stats_reset_unix_micro == $after.stats_reset_unix_micro and
    $before.dealloc == $after.dealloc and
    $before.visible_calls == $after.visible_calls and
    $before.companion_calls == $after.companion_calls;
  ([.[] | select(.warmup | not)]) as $measured |
  ($measured | length) == 20 * $samples and
  all($measured[]; .status == "pass" and .publication_eligible == false) and
  ([$measured[] | .cell_id] | unique | length) == 20 and
  all($measured[] | select(.mode == "direct"); .system == "postgresql") and
  all($measured[] | select(.mode != "direct"); .system == "taskgate" and .receipt_verified) and
  all($measured[] | select(.mode == "novel");
    (.business_sql_delta > 0) and (.semantic_replay | not) and (.idempotent_replay | not)) and
  all($measured[] | select(.mode == "semantic_replay" or .mode == "normalized_rewrite_replay");
    (.replay_verification | type == "object") and
    stable_business(.replay_verification.business_before; .replay_verification.business_after) and
    .replay_verification.root_before == .replay_verification.root_after and
    .business_sql_delta == 0) and
  all($measured[] | select(.mode == "idempotent_replay");
    (.idempotent_verification | type == "object") and
    .idempotent_verification.before == .idempotent_verification.after and
    .business_sql_delta == 0)' \
  "$run_dir/raw/deployment-01.jsonl" >/dev/null

# The governance overhead this run exists to measure: per cell, the Direct
# median against each governed mode's median, from the measured samples only.
jq -s -r '
  [.[] | select(.warmup | not)]
  | group_by(.cell_id)
  | map({cell: .[0].cell_id, mode: .[0].mode, system: .[0].system,
         n: length,
         median: (map(.client_full_drain_ms) | sort | .[(length / 2 | floor)])})
  | sort_by(.cell)[]
  | "\(.cell)\t\(.system)\tn=\(.n)\tmedian_ms=\(.median)"' \
  "$run_dir/raw/deployment-01.jsonl" | tee "$run_dir/cell-medians.tsv"
chmod 600 "$run_dir/cell-medians.tsv"
echo "$run_dir"
