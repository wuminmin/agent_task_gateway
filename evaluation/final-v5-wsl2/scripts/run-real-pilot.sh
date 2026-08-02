#!/usr/bin/env bash
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot
[[ -f .env ]] || { echo "real Pilot requires the local Compose .env" >&2; exit 2; }

run_dir="${1:-evaluation/final-v5-wsl2/raw/real-pilot-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ ! -e "$run_dir" ]] || { echo "real Pilot output already exists" >&2; exit 1; }
mkdir -m 700 -p "$run_dir/raw" "$run_dir/environment"
install -m 600 evaluation/final-v5-wsl2/config/pilot.example.json "$run_dir/config.json"
printf '%s\n' 'publication_eligible=false' 'real_system=true' 'scope=baseline-tiny' > "$run_dir/PILOT-NOT-FOR-PUBLICATION"

export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_CAMPAIGN_ID=pilot-local-only
export TASKGATE_DEPLOYMENT_ID=deployment-01
export COMPOSE_PROJECT_NAME=taskgate-final-v5-pilot-local-only-deployment-01
export TASKGATE_COMPOSE_FILES=compose.yaml:compose.debug.yaml:evaluation/final-v5-wsl2/compose.real-pilot.yaml
export TASKGATE_FRESH_PROOF_OUTPUT="$run_dir/environment/deployment-01.fresh.json"

compose=(docker compose --project-name "$COMPOSE_PROJECT_NAME" --file compose.yaml --file compose.debug.yaml --file evaluation/final-v5-wsl2/compose.real-pilot.yaml)
compose_json="$("${compose[@]}" config --format json)"
service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<< "$compose_json"; }
urlencode() { jq -rn --arg value "$1" '$value|@uri'; }
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
jq -e '.status == "pass" and .publication_eligible == false and (.reasons | length) == 0' "$run_dir/generated/summary.json" >/dev/null
jq -s -e 'all(.[]; .status == "pass") and all(.[]; .publication_eligible == false) and
  any(.[]; .system == "postgresql") and
  any(.[]; .system == "taskgate" and .receipt_verified and .artifact_available and .parquet_bytes > 0) and
  any(.[]; .mode == "pending_recovery" and .recovery_verification.failure_observed and
    .recovery_verification.canonical_object_observed and
    .recovery_verification.artifact_status_before == "PENDING" and
    .recovery_verification.artifact_status_after == "AVAILABLE")' \
  "$run_dir/raw/deployment-01.jsonl" >/dev/null
echo "$run_dir"
