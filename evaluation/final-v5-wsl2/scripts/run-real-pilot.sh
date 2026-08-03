#!/usr/bin/env bash
set -euo pipefail

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot
[[ -f .env ]] || { echo "real Pilot requires the local Compose .env" >&2; exit 2; }

run_dir="${1:-evaluation/final-v5-wsl2/raw/real-pilot-$(date -u +%Y%m%dT%H%M%SZ)}"
[[ ! -e "$run_dir" ]] || { echo "real Pilot output already exists" >&2; exit 1; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "real Pilot requires a clean worktree for commit-bound evidence" >&2
  exit 1
}
submission_commit="$(git rev-parse HEAD)"
mkdir -m 700 "$run_dir"
mkdir -m 700 "$run_dir/raw" "$run_dir/environment"
jq --arg submission_commit "$submission_commit" '.submission_commit = $submission_commit' \
  evaluation/final-v5-wsl2/config/pilot.example.json > "$run_dir/config.json"
chmod 600 "$run_dir/config.json"
printf '%s\n' 'publication_eligible=false' 'real_system=true' 'scope=baseline-tiny' > "$run_dir/PILOT-NOT-FOR-PUBLICATION"

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
jq -s -e '
  def sha: type == "string" and test("^[0-9a-f]{64}$");
  def stable_business($before; $after):
    ($before | type == "object") and ($after | type == "object") and
    $before.stats_reset_unix_micro > 0 and
    $before.stats_reset_unix_micro == $after.stats_reset_unix_micro and
    $before.dealloc == $after.dealloc and
    $before.visible_calls == $after.visible_calls and
    $before.companion_calls == $after.companion_calls;
  def manifest_ok($manifest):
    ($manifest | type == "object") and
    $manifest.verifier_version == "taskgate-final-v5-composite-verifier-v1" and
    $manifest.verification_result == "pass" and
    all([$manifest.query_id_hash, $manifest.result_id_hash, $manifest.root_task_id_hash,
         $manifest.receipt_sha256, $manifest.observation_sha256, $manifest.release_set_sha256,
         $manifest.dependency_set_sha256, $manifest.outcome_set_sha256,
         $manifest.artifact_intent_sha256, $manifest.object_key_sha256,
         $manifest.canonical_ciphertext_sha256, $manifest.released_parquet_sha256,
         $manifest.schema_sha256][]; sha) and
    $manifest.canonical_ciphertext_size > 0 and $manifest.released_parquet_size > 0 and
    $manifest.terminal_audit_sequence > 0 and $manifest.registration_audit_sequence > 0 and
    $manifest.availability_audit_sequence > $manifest.registration_audit_sequence;
  length == 5 and
  all(.[]; .status == "pass" and .publication_eligible == false) and
  ([.[] | select(.system == "postgresql" and .mode == "direct")] | length == 1) and
  ([.[] | select(.system == "taskgate") | .mode] | sort == ["idempotent_replay", "novel", "pending_recovery", "semantic_replay"]) and
  all(.[] | select(.system == "taskgate");
    .receipt_verified and .artifact_available and .parquet_bytes > 0 and
    manifest_ok(.baseline_verification.verifier_manifest)) and
  ([.[] | select(.mode == "novel")] | length == 1 and (.[0] as $sample |
    ($sample.replay_verification | type == "object") and $sample.business_sql_delta == 2 and
    $sample.replay_verification.business_before.stats_reset_unix_micro == $sample.replay_verification.business_after.stats_reset_unix_micro and
    $sample.replay_verification.business_before.dealloc == $sample.replay_verification.business_after.dealloc and
    ($sample.replay_verification.business_after.visible_calls - $sample.replay_verification.business_before.visible_calls) == 1 and
    ($sample.replay_verification.business_after.companion_calls - $sample.replay_verification.business_before.companion_calls) == 1 and
    $sample.replay_verification.root_before.epoch == 0 and
    $sample.replay_verification.root_before.release_cardinality == 0 and
    $sample.replay_verification.root_before.dependency_cardinality == 0 and
    $sample.replay_verification.root_before.outcome_cardinality == 0 and
    $sample.replay_verification.root_before.root_observation_count == 0 and
    $sample.replay_verification.root_before.dictionary_set_sha256 == "" and
    $sample.replay_verification.root_before.release_set_sha256 == "" and
    $sample.replay_verification.root_before.dependency_set_sha256 == "" and
    $sample.replay_verification.root_before.outcome_set_sha256 == "" and
    $sample.replay_verification.root_after.epoch > 0 and
    ($sample.semantic_replay | not) and ($sample.idempotent_replay | not))) and
  ([.[] | select(.mode == "semantic_replay")] | length == 1 and (.[0] as $sample |
    ($sample.replay_verification | type == "object") and
    stable_business($sample.replay_verification.business_before; $sample.replay_verification.business_after) and
    $sample.replay_verification.root_before == $sample.replay_verification.root_after and
    ($sample.replay_verification.root_after.epoch > 0) and
    ($sample.replay_verification.root_after.root_observation_count > 0) and
    ($sample.replay_verification.source_observation_sha256 | sha) and
    $sample.replay_verification.source_observation_sha256 == $sample.replay_verification.replay_observation_sha256 and
    ($sample.cross_binding_verification as $cross |
      ($cross | type == "object") and
      $cross.first_task_id_hash != $cross.second_task_id_hash and
      $cross.first_root_task_id_hash != $cross.second_root_task_id_hash and
      $cross.first_query_id_hash != $cross.second_query_id_hash and
      $cross.first_grant_sha256 != $cross.second_grant_sha256 and
      $cross.first_cache_key_sha256 != $cross.second_cache_key_sha256 and
      $cross.first_observation_binding_sha256 != $cross.second_observation_binding_sha256 and
      $cross.first_sql_fingerprint_sha256 == $cross.second_sql_fingerprint_sha256 and
      $cross.first_catalog_sha256 == $cross.second_catalog_sha256 and
      $cross.first_schema_sha256 == $cross.second_schema_sha256 and
      $cross.first_datasource_id_hash == $cross.second_datasource_id_hash and
      $cross.first_source_query_id_hash == $cross.first_query_id_hash and
      $cross.second_source_query_id_hash == $cross.second_query_id_hash and
      $cross.second_root_first_query_id_hash == $cross.second_query_id_hash and
      $cross.business_before.stats_reset_unix_micro == $cross.business_after.stats_reset_unix_micro and
      $cross.business_before.dealloc == $cross.business_after.dealloc and
      ($cross.business_after.visible_calls - $cross.business_before.visible_calls) == 1 and
      ($cross.business_after.companion_calls - $cross.business_before.companion_calls) == 1 and
      $cross.semantic_replay_audits == 0 and $cross.settlement_audits == 1 and
      ($cross.semantic_replay | not) and ($cross.idempotent_replay | not) and
      manifest_ok($cross.verifier_manifest)))) and
  ([.[] | select(.mode == "idempotent_replay")] | length == 1 and (.[0] as $sample |
    ($sample.idempotent_verification | type == "object") and
    $sample.idempotent_verification.before == $sample.idempotent_verification.after and
    $sample.idempotent_verification.returned == $sample.idempotent_verification.before.target and
    $sample.idempotent_verification.before.query_records > 0 and
    $sample.idempotent_verification.before.exposure_charges > 0 and
    $sample.idempotent_verification.before.observations > 0 and
    $sample.idempotent_verification.before.receipts > 0 and
    $sample.idempotent_verification.before.available_artifacts > 0 and
    $sample.idempotent_verification.before.terminal_audits > 0 and
    $sample.idempotent_verification.before.registration_audits > 0 and
    $sample.idempotent_verification.before.availability_audits > 0 and
    $sample.idempotent_verification.before.canonical_objects > 0 and
    $sample.idempotent_verification.returned.found and
    $sample.idempotent_verification.returned.artifact_status == "AVAILABLE")) and
  ([.[] | select(.mode == "pending_recovery")] | length == 1 and (.[0] as $sample |
    ($sample.recovery_verification as $recovery |
      ($recovery | type == "object") and $recovery.failure_observed and
      $recovery.canonical_object_observed and $recovery.artifact_status_before == "PENDING" and
      $recovery.artifact_status_after == "AVAILABLE" and $sample.business_sql_delta == 2 and
      $recovery.business_before_snapshot.stats_reset_unix_micro == $recovery.business_at_failure_snapshot.stats_reset_unix_micro and
      $recovery.business_at_failure_snapshot.stats_reset_unix_micro == $recovery.business_after_snapshot.stats_reset_unix_micro and
      $recovery.business_before_snapshot.dealloc == $recovery.business_at_failure_snapshot.dealloc and
      $recovery.business_at_failure_snapshot.dealloc == $recovery.business_after_snapshot.dealloc and
      ($recovery.business_at_failure_snapshot.visible_calls - $recovery.business_before_snapshot.visible_calls) == 1 and
      ($recovery.business_at_failure_snapshot.companion_calls - $recovery.business_before_snapshot.companion_calls) == 1 and
      stable_business($recovery.business_at_failure_snapshot; $recovery.business_after_snapshot) and
      $recovery.root_at_failure == $recovery.root_after and
      $recovery.exposure_at_failure == $recovery.exposure_after and
      $recovery.object_at_failure == $recovery.object_after and $recovery.object_at_failure.exists and
      $recovery.settlement_audit_sequences_at_failure == $recovery.settlement_audit_sequences_after and
      ($recovery.settlement_audit_sequences_at_failure | length) == 1 and
      $recovery.terminal_audits_at_failure == 1 and $recovery.terminal_audits_after == 1 and
      $recovery.registration_audits_at_failure == 1 and $recovery.registration_audits_after == 1 and
      $recovery.availability_audits_at_failure == 0 and $recovery.availability_audits_after == 1 and
      $recovery.terminal_audit_sequence_at_failure == $recovery.terminal_audit_sequence_after and
      $recovery.registration_audit_sequence_at_failure == $recovery.registration_audit_sequence_after and
      $recovery.availability_audit_sequence_at_failure == 0 and
      $recovery.availability_audit_sequence_after > $recovery.registration_audit_sequence_after)))' \
  "$run_dir/raw/deployment-01.jsonl" >/dev/null
echo "$run_dir"
