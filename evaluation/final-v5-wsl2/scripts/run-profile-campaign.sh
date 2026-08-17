#!/usr/bin/env bash
# Pilot-only launcher for the P30 per-profile deployment mechanism. A selected
# profile/repetition receives one fresh Compose project, only its planned cells
# are dispatched, and the credential-free records are merged after cleanup.
set -euo pipefail
umask 077

: "${TASKGATE_EXPERIMENT_CLASS:?TASKGATE_EXPERIMENT_CLASS is required}"
: "${TASKGATE_SUBMISSION_COMMIT:?TASKGATE_SUBMISSION_COMMIT is required}"
: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"
[[ "$TASKGATE_EXPERIMENT_CLASS" == pilot ]] || { echo "profile campaign runner is pilot-only" >&2; exit 2; }
[[ "$TASKGATE_SUBMISSION_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "submission commit must be a full SHA" >&2; exit 2; }
[[ "$TASKGATE_CAMPAIGN_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || { echo "campaign ID must be path-safe" >&2; exit 2; }

repetitions="${TASKGATE_CAMPAIGN_REPETITIONS:-1}"
profiles_csv="${TASKGATE_CAMPAIGN_PROFILES:-}"
[[ "$repetitions" =~ ^[1-9][0-9]*$ ]] || { echo "TASKGATE_CAMPAIGN_REPETITIONS must be positive" >&2; exit 2; }

for command in docker git go jq sha256sum curl install; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 2; }
done

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ "$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT" ]] || { echo "checkout differs from fixed submission commit" >&2; exit 2; }
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "profile campaign requires a clean worktree" >&2; exit 2; }

profile_registry="$repo/config/profiles/registry.json"
rq5_cell_map_source="$repo/config/profiles/rq5-workload-cell-map-v1.json"
rq5_catalog_family_source="$repo/config/profiles/rq5-daily-catalog-family-v1.json"
dataset_binding="$(realpath "$TASKGATE_DATASET_BINDINGS")"
for input in "$profile_registry" "$rq5_cell_map_source" "$rq5_catalog_family_source" "$dataset_binding"; do
  [[ -f "$input" && ! -L "$input" ]] || { echo "required input is missing or unsafe: $input" >&2; exit 2; }
done

campaign_root="$repo/evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID"
[[ ! -e "$campaign_root" ]] || { echo "refusing to overwrite $campaign_root" >&2; exit 2; }
mkdir -m 700 -p "$campaign_root/source" "$campaign_root/deployments"
plan="$campaign_root/campaign-plan.json"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-campaign-plan -require-ready >"$plan"
chmod 600 "$plan"
rq5_cell_map_retained_rel="source/rq5-workload-cell-map-v1.json"
rq5_cell_map_retained="$campaign_root/$rq5_cell_map_retained_rel"
install -m 600 "$rq5_cell_map_source" "$rq5_cell_map_retained"
rq5_cell_map_sha="$(sha256sum "$rq5_cell_map_retained" | awk '{print $1}')"
[[ "$rq5_cell_map_sha" == "$(sha256sum "$rq5_cell_map_source" | awk '{print $1}')" ]] || {
  echo "retained RQ5 cell map differs from its source" >&2; exit 1; }
rq5_catalog_family_retained_rel="source/rq5-daily-catalog-family-v1.json"
rq5_catalog_family_retained="$campaign_root/$rq5_catalog_family_retained_rel"
install -m 600 "$rq5_catalog_family_source" "$rq5_catalog_family_retained"
[[ "$(sha256sum "$rq5_catalog_family_retained" | awk '{print $1}')" == \
   "$(sha256sum "$rq5_catalog_family_source" | awk '{print $1}')" ]] || {
  echo "retained RQ5 Catalog family differs from its source" >&2; exit 1; }

if [[ -n "$profiles_csv" ]]; then
  IFS=, read -r -a selected_profiles <<< "$profiles_csv"
else
  mapfile -t selected_profiles < <(jq -er '.deployments[].alias' "$plan")
fi
declare -A selected_seen=()
for alias in "${selected_profiles[@]}"; do
  [[ -n "$alias" && -z "${selected_seen[$alias]+present}" ]] || { echo "profiles contain an empty or duplicate alias" >&2; exit 2; }
  jq -e --arg alias "$alias" '[.deployments[] | select(.alias == $alias and .ready == true)] | length == 1' "$plan" >/dev/null || {
    echo "profile is absent or not ready: $alias" >&2; exit 2; }
  selected_seen["$alias"]=1
done
echo "P30-STAGE: plan=pass ready=$(jq '.deployments | length' "$plan") selected=${#selected_profiles[@]} repetitions=$repetitions"

# Build the host-side adapter, observer, activator, and optional RQ5 driver from
# one clean fixed checkout. The manifest uses the complete tracked-file set.
source_listing="$(git ls-files | sort | while IFS= read -r file; do
  printf '%s  %s\n' "$(sha256sum "$file" | awk '{print $1}')" "$file"
done)"
source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"
build_sealed() { # package, binary, manifest, command
  local package="$1" binary="$2" manifest="$3" build_command="$4" digest
  GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$binary" "$package"
  chmod 700 "$binary"
  digest="$(sha256sum "$binary" | awk '{print $1}')"
  printf '%s' "$source_listing" | jq -Rs --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
    --arg binary_sha256 "$digest" --arg source_sha256 "$source_sha" --arg go_version "$(go version)" \
    --arg build_command "$build_command" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,
      source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:.}' >"$manifest"
  chmod 600 "$manifest"
  printf '%s' "$digest"
}

adapter="$campaign_root/source/final-v5-adapter"
adapter_manifest="$campaign_root/source/final-v5-adapter.build.json"
observer="$campaign_root/source/final-v5-observer"
observer_manifest="$campaign_root/source/final-v5-observer.build.json"
activator="$campaign_root/source/final-v5-profile-activate"
rq5_driver="$campaign_root/source/rq5-sequential-driver"
rq5_manifest="$campaign_root/source/rq5-sequential-driver.build.json"
adapter_sha="$(build_sealed ./evaluation/cmd/final-v5-adapter "$adapter" "$adapter_manifest" \
  'go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter')"
observer_sha="$(build_sealed ./evaluation/cmd/final-v5-observer "$observer" "$observer_manifest" \
  'go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer')"
GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$activator" ./evaluation/cmd/final-v5-profile-activate
chmod 700 "$activator"
rq5_sha="$(build_sealed ./evaluation/cmd/rq5-sequential-driver "$rq5_driver" "$rq5_manifest" \
  'go build -buildvcs=false -trimpath -o rq5-sequential-driver ./evaluation/cmd/rq5-sequential-driver')"

export TASKGATE_DATASET_BINDINGS="$dataset_binding"
binding_file_sha="$(sha256sum "$dataset_binding" | awk '{print $1}')"
binding_validation=""
binding_strict_valid=0
if binding_validation="$($adapter --validate-binding 2>"$campaign_root/source/dataset-binding.validation.stderr")" &&
  jq -e '.schema_version == 2 and .status == "valid"' <<<"$binding_validation" >/dev/null; then
	  binding_strict_valid=1
  [[ "$(jq -er .dataset_binding_sha256 <<<"$binding_validation")" == "$binding_file_sha" ]] || { echo "dataset binding digest drift" >&2; exit 1; }
  export TASKGATE_FINAL_V5_BINDING_FILE_SHA256="$binding_file_sha"
  export TASKGATE_FINAL_V5_BINDING_SECTION_SHA256="$(jq -er .final_v5_adapter_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_DATASET_SHA256="$(jq -er .dataset_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_DATASET_PROBE_SQL_SHA256="$(jq -er .dataset_probe_sql_sha256 <<<"$binding_validation")"
  export TASKGATE_FINAL_V5_DATASET_PROBE_SHA256="$(jq -er .dataset_probe_sha256 <<<"$binding_validation")"
  rm "$campaign_root/source/dataset-binding.validation.stderr"
else
  # RLS/Baseline/Attack/Concurrency/RQ5 do not consume the reviewed private
  # Scale/Artifact/ProvSQL sections. Their profile and activation records still
  # bind the exact supplied file digest. Refuse sensitive cells before any
  # deployment rather than burning a partial run with an unusable binding.
  if jq -e --argjson aliases "$(printf '%s\n' "${selected_profiles[@]}" | jq -Rsc 'split("\n")[:-1]')" '
      [.deployments[] | select(.alias as $a | $aliases | index($a)) | .experiments[]] |
      any(. == "scale" or . == "artifact" or . == "provsql")' "$plan" >/dev/null; then
    echo "selected Scale/Artifact/ProvSQL cells require a currently valid private Dataset Binding" >&2
    exit 2
  fi
fi
export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$binding_file_sha"
export TASKGATE_FINAL_V5_PROFILE_REGISTRY="$profile_registry"
export TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256="$(sha256sum "$profile_registry" | awk '{print $1}')"
export TASKGATE_FINAL_V5_OBSERVER="$observer"
export TASKGATE_FINAL_V5_OBSERVER_SHA256="$observer_sha"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST="$observer_manifest"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST_SHA256="$(sha256sum "$observer_manifest" | awk '{print $1}')"

rq5_manifest_source_digest() {
  jq -er --arg source "$1" '.source_files | split("\n") | map(select(endswith("  " + $source))) |
    if length == 1 then .[0] | capture("^(?<sha>[0-9a-f]{64})  ").sha else error("RQ5 source missing") end' "$rq5_manifest"
}
rq5_generator_sha="$(rq5_manifest_source_digest evaluation/daily-publication/sql/05-generate-daily-data.sh)"
rq5_config_sha="$(rq5_manifest_source_digest evaluation/daily-publication/config.json)"

compose_files=(compose.yaml compose.debug.yaml evaluation/final-v5-wsl2/compose.real-pilot.yaml
  evaluation/final-v5-wsl2/compose.provsql.yaml evaluation/final-v5-wsl2/compose.observer-v3.yaml)
compose_files_colon="$(IFS=:; printf '%s' "${compose_files[*]}")"
# The activation diagnostic is deliberately disabled by an empty token. Give
# every fresh pilot deployment one ephemeral process-only token before Compose
# resolves its environment; it is never written to retained evidence.
if [[ -z "${GATEWAY_ADMIN_TOKEN:-}" ]]; then
  GATEWAY_ADMIN_TOKEN="$(sha256sum /proc/sys/kernel/random/uuid | awk '{print $1}')"
  export GATEWAY_ADMIN_TOKEN
fi
phase1_services=(business-postgres control-postgres oa-demo result-object-store result-object-store-init
  snapshot-index-detail snapshot-index-summary snapshot-index-result-heavy snapshot-sidecar-install
  final-v5-direct-postgres final-v5-provsql-postgres)
phase1_healthy=(business-postgres control-postgres oa-demo result-object-store
  final-v5-direct-postgres final-v5-provsql-postgres)
phase1_jobs=(result-object-store-init snapshot-index-detail snapshot-index-summary
  snapshot-index-result-heavy snapshot-sidecar-install)

current_project=""
current_dir=""
current_compose=()
current_rq5_secret=""
current_rq5_project=""
current_rq5_run_root=""
current_stage="preflight"
cleanup_rq5() {
  local status=0 fixture network owner expected_owner project
  local -a projects=()
  [[ -n "$current_rq5_project" ]] || return 0
  fixture="$current_rq5_project-fixture"
  network="$current_rq5_project-business"
  if [[ -d "$current_rq5_run_root/cycles" ]]; then
    mapfile -t projects < <(find "$current_rq5_run_root/cycles" -name cycle-workspace.json -type f -print0 |
      sort -z | xargs -0 -r jq -er '.project')
  fi
  projects+=("$fixture")
  for project in "${projects[@]}"; do
    if [[ "$project" != "$fixture" && ! "$project" =~ ^${current_rq5_project}-c[1-4]-[0-9a-f]{12}$ ]]; then
      echo "refusing RQ5 cleanup outside deployment: $project" >&2
      status=1
      continue
    fi
    env "DAILY_RQ5_BUSINESS_NETWORK=$network" DAILY_RQ5_OA_SERVICE_TOKEN=cleanup \
      DAILY_RQ5_OA_CALLBACK_SECRET=cleanup DAILY_RQ5_OA_RECEIPT_KEY_ID=cleanup \
      DAILY_RQ5_OA_RECEIPT_PRIVATE_KEY=cleanup DAILY_RQ5_OA_SESSION_SECRET=cleanup \
      DAILY_RQ5_OA_ALICE_PASSWORD=cleanup DAILY_RQ5_OA_BOB_PASSWORD=cleanup \
      DAILY_RQ5_GATEWAY_CALLBACK_URL=http://rq5-cleanup.invalid/api/v1/oa/callback \
      docker compose --project-name "$project" --file evaluation/daily-publication-online/compose.yaml \
      down --volumes --remove-orphans >/dev/null 2>&1 || status=1
    [[ -z "$(docker ps --all --quiet --filter "label=com.docker.compose.project=$project")" ]] || status=1
    [[ -z "$(docker volume ls --quiet --filter "label=com.docker.compose.project=$project")" ]] || status=1
    [[ -z "$(docker network ls --quiet --filter "label=com.docker.compose.project=$project")" ]] || status=1
  done
  if docker network inspect "$network" >/dev/null 2>&1; then
    owner="$(docker network inspect "$network" --format '{{ index .Labels "taskgate.rq5.owner" }}')"
    expected_owner="$(printf '%s' "$current_rq5_run_root" | sha256sum | awk '{print $1}')"
    if [[ "$owner" == "$expected_owner" ]]; then
      docker network rm "$network" >/dev/null 2>&1 || status=1
    else
      echo "refusing RQ5 network owned by another deployment" >&2
      status=1
    fi
  fi
  if [[ -n "$current_rq5_secret" ]]; then
    bash evaluation/final-v5-wsl2/scripts/rq5-secret-root-cleanup.sh "$current_rq5_secret" || status=1
  fi
  current_rq5_secret=""
  current_rq5_project=""
  current_rq5_run_root=""
  return "$status"
}
cleanup_current() {
  local status="${1:-0}"
  set +e
  if [[ -n "$current_dir" && "$status" -ne 0 ]]; then
    if [[ ! -e "$current_dir/deployment-failure.json" ]]; then
      jq -n --arg status "fail" --arg failure_stage "$current_stage" \
        --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
        --arg compose_project "$current_project" \
        '{schema_version:1,status:$status,failure_stage:$failure_stage,campaign_class:"pilot",
          publication_eligible:false,campaign_id:$campaign_id,submission_commit:$submission_commit,
          compose_project:$compose_project}' >"$current_dir/deployment-failure.json"
      chmod 600 "$current_dir/deployment-failure.json"
    fi
    "${current_compose[@]}" ps --all >>"$current_dir/compose-up.log" 2>&1
    "${current_compose[@]}" logs --no-color --tail 200 >"$current_dir/compose-logs-failure.log" 2>&1
  fi
  if [[ -n "$current_project" ]]; then
    "${current_compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1
  fi
  cleanup_rq5 || status=1
  current_project=""
  current_dir=""
  current_compose=()
  set -e
  return "$status"
}
on_exit() { local status=$?; trap - EXIT; cleanup_current "$status" || true; exit "$status"; }
trap on_exit EXIT

service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<<"$compose_json"; }
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
config_source() {
  case "$1" in
    baseline) printf '%s' evaluation/final-v5-wsl2/config/publication.example.json ;;
    scale) printf '%s' evaluation/final-v5-wsl2/config/scale.example.json ;;
    artifact) printf '%s' evaluation/final-v5-wsl2/config/artifact.example.json ;;
    rls) printf '%s' evaluation/final-v5-wsl2/config/rls.example.json ;;
    attack) printf '%s' evaluation/final-v5-wsl2/config/attacks.example.json ;;
    provsql) printf '%s' evaluation/final-v5-wsl2/config/provsql-paired.example.json ;;
    compiler) printf '%s' evaluation/final-v5-wsl2/config/compiler-scale.example.json ;;
    concurrency) printf '%s' evaluation/final-v5-wsl2/config/concurrency.example.json ;;
    rq5) printf '%s' evaluation/final-v5-wsl2/config/daily-publication.example.json ;;
    *) echo "unknown experiment $1" >&2; return 2 ;;
  esac
}
experiment_command() {
  case "$1" in
    baseline) printf '%s' v5-full ;; scale) printf '%s' v5-scale ;; artifact) printf '%s' v5-artifact ;;
    rls) printf '%s' rls-adaptive ;; attack) printf '%s' adaptive-attacks ;;
    provsql) printf '%s' taskgate-provsql-pair ;; compiler) printf '%s' view-scale ;;
    concurrency) printf '%s' v5-concurrency ;; rq5) printf '%s' v5-rq5 ;;
    *) echo "unknown experiment $1" >&2; return 2 ;;
  esac
}

deployment_count=0
for alias in "${selected_profiles[@]}"; do
  for repetition in $(seq 1 "$repetitions"); do
    export TASKGATE_FINAL_V5_PROFILE_ALIAS="$alias"
    deployment_count=$((deployment_count + 1))
    profile_id="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .profile_id' "$plan")"
    catalog_path="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .catalog_path' "$plan")"
    catalog_sha="$(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .catalog_sha256' "$plan")"
    cells_json="$(jq -c --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .cells' "$plan")"
    deployment_key="${alias}/$(printf '%03d' "$repetition")"
    current_dir="$campaign_root/deployments/${alias}/$(printf '%03d' "$repetition")"
    mkdir -m 700 -p "$current_dir/raw" "$current_dir/config" "$current_dir/selected-cells" "$current_dir/activation"
    project_identity="$(printf '%s\0%s\0%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_SUBMISSION_COMMIT" "$alias" "$repetition" | sha256sum | awk '{print $1}')"
    current_project="$(bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh "$project_identity" deployment-01)"
    export COMPOSE_PROJECT_NAME="$current_project"
    current_compose=(docker compose --project-name "$current_project")
    for compose_file in "${compose_files[@]}"; do current_compose+=(--file "$compose_file"); done
    bash evaluation/final-v5-wsl2/scripts/compose-host-preflight.sh "$current_project" "${compose_files[@]}"
    [[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || { echo "worktree changed before $deployment_key" >&2; exit 1; }
    echo "P30-STAGE: deployment_start=$deployment_key compose_project=$current_project cells=$(jq 'length' <<<"$cells_json")"

    current_stage=phase1_bringup
    "${current_compose[@]}" up -d "${phase1_services[@]}" >"$current_dir/compose-up.log" 2>&1
    for service in "${phase1_healthy[@]}"; do
      for attempt in $(seq 1 120); do
        container="$("${current_compose[@]}" ps -q "$service")"
        state="$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || echo unknown)"
        [[ "$state" == healthy ]] && break
        [[ "$attempt" == 120 ]] && { echo "$service never became healthy" >&2; exit 1; }
        sleep 2
      done
    done
    for service in "${phase1_jobs[@]}"; do
      for attempt in $(seq 1 180); do
        container="$("${current_compose[@]}" ps -aq "$service")"
        running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null || echo true)"
        if [[ "$running" == false ]]; then
          [[ "$(docker inspect --format '{{.State.ExitCode}}' "$container")" == 0 ]] || { echo "$service failed" >&2; exit 1; }
          break
        fi
        [[ "$attempt" == 180 ]] && { echo "$service never completed" >&2; exit 1; }
        sleep 2
      done
    done

    current_stage=deployment_binding
    compose_json="$("${current_compose[@]}" config --format json)"
    [[ "$(service_env gateway GATEWAY_ADMIN_TOKEN)" == "$GATEWAY_ADMIN_TOKEN" ]] || { echo "Compose admin-token binding drift" >&2; exit 1; }
    export TASKBOUND_ALICE_TOKEN="$(service_env gateway TASKBOUND_ALICE_TOKEN)"
    export TASKBOUND_CAROL_TOKEN="$(service_env gateway TASKBOUND_CAROL_TOKEN)"
    export OA_ALICE_PASSWORD="$(service_env oa-demo OA_ALICE_PASSWORD)"
    export OA_BOB_PASSWORD="$(service_env oa-demo OA_BOB_PASSWORD)"
    control_password="$(service_env control-postgres POSTGRES_PASSWORD)"
    control_database="$(service_env control-postgres POSTGRES_DB)"
    business_password="$(service_env gateway GATEWAY_DB_PASSWORD)"
    business_database="$(service_env business-postgres POSTGRES_DB)"
    business_admin_password="$(service_env business-postgres POSTGRES_PASSWORD)"
    control_port="$("${current_compose[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
    business_port="$("${current_compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
    object_port="$("${current_compose[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
    export TASKGATE_FINAL_V5_CONTROL_DSN="postgres://postgres:$(urlencode "$control_password")@127.0.0.1:$control_port/$(urlencode "$control_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_BUSINESS_DSN="postgres://gateway_reader:$(urlencode "$business_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN="postgres://postgres:$(urlencode "$business_admin_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
    export TASKGATE_FINAL_V5_GATEWAY_URL=http://127.0.0.1:8082
    export TASKGATE_FINAL_V5_OA_URL=http://127.0.0.1:8092
    export TASKGATE_FINAL_V5_OBJECT_STORE_URL="http://127.0.0.1:$object_port"
    export TASKGATE_FINAL_V5_OBJECT_STORE_ACCESS_KEY="$(service_env gateway GATEWAY_OBJECT_STORE_ACCESS_KEY)"
    export TASKGATE_FINAL_V5_OBJECT_STORE_SECRET_KEY="$(service_env gateway GATEWAY_OBJECT_STORE_SECRET_KEY)"
    export TASKGATE_FINAL_V5_OBJECT_STORE_BUCKET="$(service_env gateway GATEWAY_OBJECT_STORE_BUCKET)"
    export TASKGATE_FINAL_V5_CONCURRENCY_TOKEN="$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_TOKEN)"
    export TASKGATE_FINAL_V5_DIRECT_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25534/final_v5_provsql?sslmode=disable'
    export TASKGATE_FINAL_V5_PROVSQL_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25535/final_v5_provsql?sslmode=disable'

    current_stage=profile_artifacts
    full_artifacts="$current_dir/snapshot-index-artifacts-full"
    profile_artifacts="$current_dir/profile-artifacts"
    mkdir -m 700 -p "$full_artifacts" "$profile_artifacts"
    docker run --rm --volume "${current_project}_snapshot-index-artifacts:/data:ro" \
      --volume "$full_artifacts:/out" alpine:3.20 \
      sh -c "cp -R /data/. /out/ && chown -R $(id -u):$(id -g) /out" >>"$current_dir/compose-up.log" 2>&1
    [[ "$(find "$full_artifacts" -type f | wc -l)" -gt 0 ]] || { echo "artifact volume was empty" >&2; exit 1; }
    artifact_manifest="$current_dir/profile-artifacts.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-artifacts --profile-id "$profile_id" \
      --source "$full_artifacts" --destination "$profile_artifacts" --manifest-out "$artifact_manifest" \
      >"$current_dir/profile-artifacts.log"

    current_stage=profile_activation
    profile_binding="$current_dir/profile-binding.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-binding --registry "$profile_registry" \
      --alias "$alias" --dataset-binding-sha256 "$binding_file_sha" --out "$profile_binding"
    activation_evidence="$current_dir/activation/$alias.json"
    GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-route-matrix -mode live -profile-alias "$alias" \
      -root "$repo" -registry "$profile_registry" -activation-evidence-dir "$current_dir/activation" \
      -activator-binary "$activator" -compose-project "$current_project" -compose-files "$compose_files_colon" \
      -deployment-id deployment-01 -dataset-binding "$dataset_binding" -profile-artifact-root "$profile_artifacts" \
      -profile-artifact-manifest "$artifact_manifest" -ready-timeout 10m
    jq -e '.status == "pass" and .activation_smoke_passed == true and .publication_eligible == false' "$activation_evidence" >/dev/null

    gateway_image="$current_dir/gateway-image.json"
    gateway_container="$("${current_compose[@]}" ps -q gateway)"
    bash evaluation/final-v5-wsl2/scripts/record-pilot-gateway-image.sh "$gateway_container" "$gateway_image" "$repo"
    environment="$current_dir/environment.json"
    export TASKGATE_DEPLOYMENT_ID=deployment-01 TASKGATE_ENVIRONMENT_OUTPUT="$environment"
    if [[ "$binding_strict_valid" == 1 ]]; then
      bash evaluation/final-v5-wsl2/scripts/record-environment.sh
    else
      # record-environment's Dataset section validates the complete reviewed
      # Scale/Artifact/ProvSQL binding. A profile that consumes none of those
      # sections is already bound by ProfileBinding to the supplied file SHA;
      # do not ask the environment recorder to make a broader claim.
      env -u TASKGATE_DATASET_BINDINGS bash evaluation/final-v5-wsl2/scripts/record-environment.sh
    fi
    fresh_proof="$current_dir/fresh-proof.json"
    jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" \
      --arg compose_project "$current_project" --arg profile_alias "$alias" --arg profile_id "$profile_id" \
      --arg catalog_sha256 "$catalog_sha" --argjson repetition "$repetition" \
      '{schema_version:1,record:"taskgate-p30-fresh-profile-deployment-v1",status:"pass",
        campaign_class:"pilot",publication_eligible:false,campaign_id:$campaign_id,
        submission_commit:$submission_commit,compose_project:$compose_project,profile_alias:$profile_alias,
        profile_id:$profile_id,catalog_sha256:$catalog_sha256,repetition:$repetition}' >"$fresh_proof"

    mapfile -t experiments < <(jq -er --arg alias "$alias" '.deployments[] | select(.alias == $alias) | .experiments[]' "$plan")
    if printf '%s\n' "${experiments[@]}" | grep -qx rq5; then
      current_rq5_run_root="$current_dir/rq5-live"
      current_rq5_project="$(bash evaluation/final-v5-wsl2/scripts/rq5-project-prefix.sh "$project_identity" deployment-01)"
      current_rq5_secret="$(mktemp -d /tmp/taskgate-rq5-secrets.deployment-01.XXXXXXXX)"
      mkdir -m 700 -p "$current_rq5_run_root"
      export TASKGATE_FINAL_V5_RQ5_DRIVER="$rq5_driver"
      export TASKGATE_FINAL_V5_RQ5_DRIVER_SHA256="$rq5_sha"
      export TASKGATE_FINAL_V5_RQ5_GENERATOR_SHA256="$rq5_generator_sha"
      export TASKGATE_FINAL_V5_RQ5_CONFIG_SHA256="$rq5_config_sha"
      export TASKGATE_FINAL_V5_RQ5_CATALOG_FAMILY="$rq5_catalog_family_retained"
      export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST="$rq5_manifest"
      export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256="$(sha256sum "$rq5_manifest" | awk '{print $1}')"
      export TASKGATE_FINAL_V5_RQ5_REPO_ROOT="$repo"
      export TASKGATE_FINAL_V5_RQ5_RUN_ROOT="$current_rq5_run_root"
      export TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID="$TASKGATE_CAMPAIGN_ID"
      export TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID=deployment-01
      export TASKGATE_FINAL_V5_RQ5_PROJECT="$current_rq5_project"
      export TASKGATE_FINAL_V5_RQ5_SECRET_ROOT="$current_rq5_secret"
      rq5_profile_binding="$current_dir/rq5-profile-binding.json"
      GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-binding \
        -registry "$profile_registry" -alias "$alias" -dataset-binding-sha256 "$binding_file_sha" \
        -rq5-catalog-family "$rq5_catalog_family_retained" -rq5-build-manifest "$rq5_manifest" \
        -rq5-build-manifest-sha256 "$(sha256sum "$rq5_manifest" | awk '{print $1}')" \
        -submission-commit "$TASKGATE_SUBMISSION_COMMIT" -rq5-generator-sha256 "$rq5_generator_sha" \
        -rq5-config-sha256 "$rq5_config_sha" -out "$rq5_profile_binding"
    fi
    for experiment in "${experiments[@]}"; do
      current_stage="cells_$experiment"
      campaign_selected="$current_dir/selected-cells/$experiment.json"
      jq --arg experiment "$experiment" '[.[] | select(startswith($experiment + "/"))]' <<<"$cells_json" >"$campaign_selected"
      selected="$campaign_selected"
      gate_mapping_args=()
      if [[ "$experiment" == rq5 ]]; then
        selected="$current_dir/selected-cells/rq5.runner.json"
        translation="$current_dir/selected-cells/rq5.translation.json"
        GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-rq5-cell-map \
          -map config/profiles/rq5-workload-cell-map-v1.json \
          -campaign-selected "$campaign_selected" -experiment-selected-out "$selected" \
          -evidence-out "$translation" -retained-map-path "$rq5_cell_map_retained_rel"
        gate_mapping_args=(-campaign-selected-cells "$campaign_selected" -rq5-cell-map "$rq5_cell_map_retained")
      fi
      config="$current_dir/config/$experiment.json"
      jq --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
        '.campaign_class="pilot" | .pilot_kind="real_system" | .campaign_id=$campaign |
         .submission_commit=$commit | .deployments=1 | .process_replicates=1 | .warmups=0 | .samples=1 |
         .fresh_root_per_sample=true' "$(config_source "$experiment")" >"$config"
      runner="$(experiment_command "$experiment")"
      raw="$current_dir/raw/$experiment.jsonl"
      operation_profile_binding="$profile_binding"
      [[ "$experiment" != rq5 ]] || operation_profile_binding="$rq5_profile_binding"
      GOFLAGS=-buildvcs=false go run "./evaluation/cmd/$runner" -config "$config" -deployment-id deployment-01 \
        -adapter "$adapter" -profile-binding "$operation_profile_binding" -selected-cells "$selected" -output "$raw" \
        >"$current_dir/$experiment.log" 2>&1
      GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-launcher-gate \
        -experiment "$experiment" -selected-cells "$selected" -input "$raw" \
        -campaign-class pilot -samples-per-cell 1 "${gate_mapping_args[@]}" >/dev/null || {
        echo "$deployment_key/$experiment retained a failed or misrouted cell" >&2; exit 1; }
      echo "P30-STAGE: cells=pass deployment=$deployment_key experiment=$experiment count=$(jq -s 'length' "$raw")"
    done

    current_stage=cleanup
    cleanup_rq5
    "${current_compose[@]}" down --volumes --remove-orphans >/dev/null
    containers="$(docker ps --all --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    volumes="$(docker volume ls --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    networks="$(docker network ls --quiet --filter "label=com.docker.compose.project=$current_project" | wc -l)"
    cleanup="$current_dir/cleanup.json"
    cleanup_status=pass
    [[ "$containers" == 0 && "$volumes" == 0 && "$networks" == 0 ]] || cleanup_status=fail
    jq -n --arg status "$cleanup_status" --argjson containers "$containers" --argjson volumes "$volumes" \
      --argjson networks "$networks" '{schema_version:1,status:$status,containers:$containers,volumes:$volumes,networks:$networks}' >"$cleanup"
    [[ "$cleanup_status" == pass ]] || { echo "$deployment_key cleanup left Compose resources" >&2; exit 1; }
    current_project=""
    current_dir=""
    current_compose=()
    current_stage=evidence_record

    refs="$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/.file-refs.jsonl"
    : >"$refs"
    add_ref() {
      local kind="$1" experiment="$2" path="$3" relative
      relative="${path#"$campaign_root/"}"
      jq -cn --arg kind "$kind" --arg experiment "$experiment" --arg path "$relative" \
        --arg sha256 "$(sha256sum "$path" | awk '{print $1}')" --argjson bytes "$(stat -c %s "$path")" \
        '{kind:$kind,path:$path,sha256:$sha256,bytes:$bytes} +
         (if $experiment == "" then {} else {experiment:$experiment} end)' >>"$refs"
    }
    add_ref profile_binding "" "$profile_binding"
    add_ref activation_evidence "" "$activation_evidence"
    add_ref environment "" "$environment"
    add_ref fresh_proof "" "$fresh_proof"
    add_ref gateway_image "" "$gateway_image"
    add_ref cleanup "" "$cleanup"
    for experiment in "${experiments[@]}"; do
      add_ref config "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/config/$experiment.json"
      add_ref selected_cells "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/$experiment.json"
      if [[ "$experiment" == rq5 ]]; then
        add_ref runner_selected_cells rq5 "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/rq5.runner.json"
        add_ref rq5_cell_translation rq5 "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/selected-cells/rq5.translation.json"
        add_ref rq5_cell_map rq5 "$rq5_cell_map_retained"
        add_ref rq5_profile_binding rq5 "$rq5_profile_binding"
        add_ref rq5_catalog_family rq5 "$rq5_catalog_family_retained"
        add_ref rq5_build_manifest rq5 "$rq5_manifest"
      fi
      add_ref raw_jsonl "$experiment" "$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/raw/$experiment.jsonl"
    done
    record="$campaign_root/deployments/$alias/$(printf '%03d' "$repetition")/deployment-record.json"
    jq -n --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
      --arg compose_project "$COMPOSE_PROJECT_NAME" --arg profile_id "$profile_id" --arg profile_alias "$alias" \
      --arg catalog_path "$catalog_path" --arg catalog_sha256 "$catalog_sha" --argjson repetition "$repetition" \
      --argjson cells "$cells_json" --slurpfile files "$refs" \
      '{schema_version:1,campaign_id:$campaign_id,campaign_class:"pilot",publication_eligible:false,
        formal_campaign:false,submission_commit:$commit,compose_project:$compose_project,profile_id:$profile_id,
        profile_alias:$profile_alias,catalog_path:$catalog_path,catalog_sha256:$catalog_sha256,
        repetition:$repetition,cells:$cells,files:$files}' >"$record"
    rm "$refs"
    echo "P30-STAGE: deployment_cleanup=pass deployment=$deployment_key containers=0 volumes=0 networks=0"
  done
done

manifest="$campaign_root/campaign-evidence.json"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-campaign-evidence -root "$campaign_root" -plan "$plan" \
  -campaign-id "$TASKGATE_CAMPAIGN_ID" -submission-commit "$TASKGATE_SUBMISSION_COMMIT" \
  -repetitions "$repetitions" -profiles "$profiles_csv" -out "$manifest"
jq -e '.status == "pass" and .campaign_class == "pilot" and .publication_eligible == false and .formal_campaign == false' "$manifest" >/dev/null
trap - EXIT
echo "P30-STAGE: mechanism=pass deployments=$deployment_count publication_eligible=false evidence=$manifest"
