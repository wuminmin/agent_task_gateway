#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${TASKGATE_EXPERIMENT_CLASS:?TASKGATE_EXPERIMENT_CLASS is required}"
: "${TASKGATE_SUBMISSION_COMMIT:?TASKGATE_SUBMISSION_COMMIT is required}"
: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_DEPLOYMENT_ID:?TASKGATE_DEPLOYMENT_ID is required}"
: "${TASKGATE_PRIVATE_CONFIG_DIR:?TASKGATE_PRIVATE_CONFIG_DIR is required}"
: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"
: "${TASKGATE_FRESH_DEPLOYMENT:?TASKGATE_FRESH_DEPLOYMENT is required}"
: "${TASKGATE_WINDOWS_ENVIRONMENT_SHA256:?TASKGATE_WINDOWS_ENVIRONMENT_SHA256 is required}"
: "${TASKGATE_WINDOWS_ENVIRONMENT_BASE64:?TASKGATE_WINDOWS_ENVIRONMENT_BASE64 is required}"

[[ "$TASKGATE_EXPERIMENT_CLASS" == publication ]] || { echo "run-deployment requires publication class" >&2; exit 2; }
[[ "$TASKGATE_FRESH_DEPLOYMENT" == 1 ]] || { echo "PowerShell must attest a fresh WSL deployment" >&2; exit 2; }
[[ "$TASKGATE_SUBMISSION_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "submission commit must be a full SHA" >&2; exit 2; }
[[ "$TASKGATE_CAMPAIGN_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || { echo "campaign ID must be a path-safe identifier" >&2; exit 2; }
[[ "$TASKGATE_WINDOWS_ENVIRONMENT_SHA256" =~ ^[0-9a-f]{64}$ ]] || { echo "Windows environment digest must be a SHA-256" >&2; exit 2; }
[[ "$TASKGATE_DEPLOYMENT_ID" =~ ^deployment-0[1-3]$ ]] || { echo "deployment ID must be deployment-01..03" >&2; exit 2; }
[[ -d "$TASKGATE_PRIVATE_CONFIG_DIR" && -f "$TASKGATE_DATASET_BINDINGS" ]] || { echo "private config or dataset binding path is missing" >&2; exit 2; }
[[ "$(stat -c '%a' "$TASKGATE_PRIVATE_CONFIG_DIR")" == 700 ]] || { echo "private config directory must have mode 0700" >&2; exit 2; }
[[ "$(stat -c '%a' "$(dirname "$TASKGATE_DATASET_BINDINGS")")" == 700 ]] || { echo "private dataset binding directory must have mode 0700" >&2; exit 2; }
[[ "$(stat -c '%a' "$TASKGATE_DATASET_BINDINGS")" == 600 ]] || { echo "private dataset binding must have mode 0600" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ "$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT" ]] || { echo "checkout does not match frozen commit" >&2; exit 1; }
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode publication

# Validate every private config before creating a campaign marker, building a
# Compose image, or touching a deployment volume. A malformed late config must
# never turn a fresh host into a partially consumed deployment.
campaign_root="evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID"
run_names=(baseline scale artifact rls attack provsql compiler concurrency rq5)
experiment_ids=(baseline scale artifact rls attack provsql compiler concurrency rq5)
commands=(v5-full v5-scale v5-artifact rls-adaptive adaptive-attacks taskgate-provsql-pair view-scale v5-concurrency v5-rq5)
configs=(baseline.json scale.json artifact.json rls.json attack.json provsql.json compiler.json concurrency.json rq5.json)
if [[ "${TASKGATE_ENABLE_SCALE_EXTREME:-0}" == 1 ]]; then
  run_names+=(scale-extreme); experiment_ids+=(scale); commands+=(v5-scale); configs+=(scale-extreme.json)
fi
for index in "${!run_names[@]}"; do
  config_source="$TASKGATE_PRIVATE_CONFIG_DIR/${configs[$index]}"
  [[ -f "$config_source" && ! -L "$config_source" ]] || { echo "missing or unsafe config for ${run_names[$index]}" >&2; exit 2; }
  config_mode="$(stat -c '%a' "$config_source")"
  [[ "$config_mode" == 600 ]] || { echo "private config must have mode 0600: ${run_names[$index]}" >&2; exit 2; }
  preflight_experiment_root="$campaign_root/${run_names[$index]}"
  mkdir -m 700 -p "$preflight_experiment_root"
  frozen_config="$preflight_experiment_root/config.json"
  if [[ -e "$frozen_config" ]]; then
    [[ -f "$frozen_config" && ! -L "$frozen_config" && "$(stat -c '%a' "$frozen_config")" == 600 ]] &&
      cmp --silent "$config_source" "$frozen_config" || {
      echo "config changed across deployments: ${run_names[$index]}" >&2; exit 1;
    }
  else
    install -m 600 "$config_source" "$frozen_config"
  fi
  jq -e --arg campaign "$TASKGATE_CAMPAIGN_ID" --arg commit "$TASKGATE_SUBMISSION_COMMIT" \
    --arg experiment "${experiment_ids[$index]}" '
      .campaign_class == "publication" and .campaign_id == $campaign and
      .submission_commit == $commit and .experiment_id == $experiment
    ' "$frozen_config" >/dev/null || { echo "private config identity mismatch: ${run_names[$index]}" >&2; exit 2; }
  go run "./evaluation/cmd/${commands[$index]}" -config "$frozen_config" -validate-only >/dev/null
done

adapter_dir="$campaign_root/source-adapter"
adapter_binary="$adapter_dir/final-v5-adapter"
adapter_build_manifest="$adapter_dir/build-manifest.json"
observer_binary="$adapter_dir/final-v5-observer"
observer_build_manifest="$adapter_dir/observer-build-manifest.json"
rq5_driver_binary="$adapter_dir/rq5-sequential-driver"
rq5_driver_build_manifest="$adapter_dir/rq5-driver-build-manifest.json"
mkdir -m 700 -p "$adapter_dir"
adapter_tmp="$(mktemp /tmp/taskgate-final-v5-adapter.XXXXXX)"
trap 'rm -f "$adapter_tmp"' EXIT
go build -buildvcs=false -trimpath -o "$adapter_tmp" ./evaluation/cmd/final-v5-adapter
adapter_digest="$(sha256sum "$adapter_tmp" | awk '{print $1}')"
if [[ -e "$adapter_binary" ]]; then
  [[ -f "$adapter_binary" && ! -L "$adapter_binary" && "$(stat -c '%a' "$adapter_binary")" == 700 &&
    "$(sha256sum "$adapter_binary" | awk '{print $1}')" == "$adapter_digest" ]] || { echo "source-controlled adapter build changed across deployments" >&2; exit 1; }
else
  install -m 700 "$adapter_tmp" "$adapter_binary"
  source_listing="$(git ls-files | sort | while IFS= read -r source_file; do printf '%s  %s\n' "$(sha256sum "$source_file" | awk '{print $1}')" "$source_file"; done)"
  source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"
  jq -n --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" --arg binary_sha256 "$adapter_digest" --arg source_sha256 "$source_sha" --arg go_version "$(go version)" --arg build_command "go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" --arg source_files "$source_listing" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:$source_files}' > "$adapter_build_manifest"
  chmod 600 "$adapter_build_manifest"
fi
[[ -f "$adapter_build_manifest" && ! -L "$adapter_build_manifest" && "$(stat -c '%a' "$adapter_build_manifest")" == 600 ]] || { echo "adapter build manifest is missing or unsafe" >&2; exit 1; }
rm -f "$adapter_tmp"
trap - EXIT

# Build the only accepted out-of-process observer from the same frozen commit.
# Private bindings cannot select an executable; the adapter consumes only this
# source-built binary and its sealed source/build manifest.
observer_tmp="$(mktemp /tmp/taskgate-final-v5-observer.XXXXXX)"
trap 'rm -f "$observer_tmp"' EXIT
go build -buildvcs=false -trimpath -o "$observer_tmp" ./evaluation/cmd/final-v5-observer
observer_digest="$(sha256sum "$observer_tmp" | awk '{print $1}')"
if [[ -e "$observer_binary" ]]; then
  [[ -f "$observer_binary" && ! -L "$observer_binary" && "$(stat -c '%a' "$observer_binary")" == 700 ]] || { echo "stored observer is unsafe" >&2; exit 1; }
  [[ "$(sha256sum "$observer_binary" | awk '{print $1}')" == "$observer_digest" ]] || { echo "source-controlled observer build changed across deployments" >&2; exit 1; }
else
  install -m 700 "$observer_tmp" "$observer_binary"
  observer_source_listing="$(git ls-files | sort | while IFS= read -r source_file; do printf '%s  %s\n' "$(sha256sum "$source_file" | awk '{print $1}')" "$source_file"; done)"
  observer_source_sha="$(printf '%s' "$observer_source_listing" | sha256sum | awk '{print $1}')"
  jq -n --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" --arg binary_sha256 "$observer_digest" --arg source_sha256 "$observer_source_sha" --arg go_version "$(go version)" --arg build_command "go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer" --arg source_files "$observer_source_listing" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:$source_files}' > "$observer_build_manifest"
  chmod 600 "$observer_build_manifest"
fi
rm -f "$observer_tmp"
trap - EXIT
[[ -f "$observer_build_manifest" && ! -L "$observer_build_manifest" && "$(stat -c '%a' "$observer_build_manifest")" == 600 ]] || { echo "observer build manifest is missing or unsafe" >&2; exit 1; }
observer_build_manifest_sha="$(sha256sum "$observer_build_manifest" | awk '{print $1}')"
[[ "$observer_build_manifest_sha" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid observer build-manifest digest" >&2; exit 1; }
export TASKGATE_FINAL_V5_OBSERVER="$(realpath "$observer_binary")"
export TASKGATE_FINAL_V5_OBSERVER_SHA256="$observer_digest"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST="$(realpath "$observer_build_manifest")"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST_SHA256="$observer_build_manifest_sha"

# The formal RQ5 path is a separate source-built host orchestrator. The
# adapter executes this exact binary over stdin/stdout and verifies its
# deployment-bound SHA-256 before accepting a cycle.
rq5_driver_tmp="$(mktemp /tmp/taskgate-final-v5-rq5-driver.XXXXXX)"
trap 'rm -f "$rq5_driver_tmp"' EXIT
go build -buildvcs=false -trimpath -o "$rq5_driver_tmp" ./evaluation/cmd/rq5-sequential-driver
rq5_driver_digest="$(sha256sum "$rq5_driver_tmp" | awk '{print $1}')"
if [[ -e "$rq5_driver_binary" ]]; then
  [[ -f "$rq5_driver_binary" && ! -L "$rq5_driver_binary" && "$(stat -c '%a' "$rq5_driver_binary")" == 700 &&
    "$(sha256sum "$rq5_driver_binary" | awk '{print $1}')" == "$rq5_driver_digest" ]] || { echo "source-controlled RQ5 driver build changed across deployments" >&2; exit 1; }
else
  install -m 700 "$rq5_driver_tmp" "$rq5_driver_binary"
  # Seal the complete tracked source inventory. The RQ5 driver materializes
  # these exact bytes into a private read-only build context before Compose
  # can execute COPY, so Go embed/test fixtures and non-Go runtime inputs are
  # covered by the same manifest instead of a hand-maintained narrow list.
  rq5_source_listing="$(git ls-files | sort | while IFS= read -r source_file; do printf '%s  %s\n' "$(sha256sum "$source_file" | awk '{print $1}')" "$source_file"; done)"
  rq5_source_sha="$(printf '%s' "$rq5_source_listing" | sha256sum | awk '{print $1}')"
  jq -n --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" --arg binary_sha256 "$rq5_driver_digest" --arg source_sha256 "$rq5_source_sha" --arg go_version "$(go version)" --arg build_command "go build -buildvcs=false -trimpath -o rq5-sequential-driver ./evaluation/cmd/rq5-sequential-driver" --arg source_files "$rq5_source_listing" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:$source_files}' > "$rq5_driver_build_manifest"
  chmod 600 "$rq5_driver_build_manifest"
fi
[[ -f "$rq5_driver_build_manifest" && ! -L "$rq5_driver_build_manifest" && "$(stat -c '%a' "$rq5_driver_build_manifest")" == 600 ]] || { echo "RQ5 build manifest is missing or unsafe" >&2; exit 1; }
rm -f "$rq5_driver_tmp"
trap - EXIT
rq5_driver_build_manifest_sha="$(sha256sum "$rq5_driver_build_manifest" | awk '{print $1}')"
[[ "$rq5_driver_build_manifest_sha" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid RQ5 build-manifest digest" >&2; exit 1; }
# Read the two runtime-input identities back from the sealed build manifest,
# not from transient shell variables. Every later cycle receives these exact
# manifest entries and independently re-hashes the files before binding copies.
rq5_manifest_source_digest() {
  jq -er --arg source "$1" '
    .source_files | split("\n")
    | map(select(endswith("  " + $source)))
    | if length == 1 then .[0] | capture("^(?<sha>[0-9a-f]{64})  ").sha
      else error("sealed RQ5 source entry missing or duplicated") end
  ' "$rq5_driver_build_manifest"
}
rq5_generator_sha="$(rq5_manifest_source_digest evaluation/daily-publication/sql/05-generate-daily-data.sh)"
rq5_config_sha="$(rq5_manifest_source_digest evaluation/daily-publication/config.json)"
[[ "$(sha256sum evaluation/daily-publication/sql/05-generate-daily-data.sh | awk '{print $1}')" == "$rq5_generator_sha" ]] || {
  echo "RQ5 generator differs from sealed driver build manifest" >&2; exit 1;
}
[[ "$(sha256sum evaluation/daily-publication/config.json | awk '{print $1}')" == "$rq5_config_sha" ]] || {
  echo "RQ5 config differs from sealed driver build manifest" >&2; exit 1;
}
$adapter_binary --validate-observer-runtime >/dev/null
binding_validation="$($adapter_binary --validate-binding)"
jq -e '
  .schema_version == 1 and .status == "valid" and
  (.dataset_sha256 | test("^[0-9a-f]{64}$")) and
  (.catalog_sha256 | test("^[0-9a-f]{64}$")) and
  (.dataset_binding_sha256 | test("^[0-9a-f]{64}$")) and
  (.final_v5_adapter_sha256 | test("^[0-9a-f]{64}$")) and
  (.dataset_probe_sql_sha256 | test("^[0-9a-f]{64}$")) and
  .scale_cells == 12 and .artifact_cells == 6 and .provsql_cells == 105
' <<< "$binding_validation" >/dev/null || { echo "strict dataset binding validation report is invalid" >&2; exit 1; }
binding_file_sha="$(jq -er .dataset_binding_sha256 <<< "$binding_validation")"
binding_section_sha="$(jq -er .final_v5_adapter_sha256 <<< "$binding_validation")"
[[ "$(sha256sum "$TASKGATE_DATASET_BINDINGS" | awk '{print $1}')" == "$binding_file_sha" ]] || {
  echo "dataset binding changed during strict validation" >&2; exit 1;
}
export TASKGATE_FINAL_V5_BINDING_FILE_SHA256="$binding_file_sha"
export TASKGATE_FINAL_V5_BINDING_SECTION_SHA256="$binding_section_sha"
for identity in "dataset-binding:$binding_file_sha" "final-v5-adapter-binding:$binding_section_sha"; do
  identity_name="${identity%%:*}"
  identity_sha="${identity#*:}"
  identity_path="$adapter_dir/$identity_name.sha256"
  if [[ -e "$identity_path" ]]; then
    [[ -f "$identity_path" && ! -L "$identity_path" && "$(stat -c '%a' "$identity_path")" == 600 &&
      "$(tr -d '[:space:]' < "$identity_path")" == "$identity_sha" ]] || {
      echo "$identity_name changed across deployments" >&2; exit 1;
    }
  else
    (set -o noclobber; printf '%s\n' "$identity_sha" > "$identity_path") || { echo "cannot freeze $identity_name identity" >&2; exit 1; }
    chmod 600 "$identity_path"
  fi
done
capabilities="$($adapter_binary --capabilities)"
jq -n -e --argjson capabilities "$capabilities" '$capabilities.schema_version == 1 and $capabilities.adapter == "final-v5-adapter" and (["baseline","scale","artifact","rls","attack","provsql","compiler","concurrency","rq5"] | all(. as $name | $capabilities.experiments[$name] == true))' >/dev/null || {
  echo "formal campaign blocked: unified source-controlled adapter capabilities are incomplete" >&2; exit 1;
}

environment_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.json"
fresh_proof_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json"
fresh_volume_inspect_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json"
fresh_compose_config_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml"
fresh_dataset_fingerprint_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt"
fresh_catalog_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.catalog.yaml"
windows_host_path="$campaign_root/environment/windows-host.json"
vmstat_before_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-before.txt"
vmstat_after_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-after.txt"
marker="$campaign_root/deployment-markers/$TASKGATE_DEPLOYMENT_ID.STARTED"
mkdir -m 700 -p "$campaign_root/environment" "$campaign_root/deployment-markers"
(set -o noclobber; printf '%s\n' "$(date -u +%FT%TZ)" > "$marker") || { echo "deployment directory already used" >&2; exit 1; }
windows_host_tmp="$(mktemp /tmp/taskgate-windows-host.XXXXXX)"
trap 'rm -f "$windows_host_tmp"' EXIT
printf '%s' "$TASKGATE_WINDOWS_ENVIRONMENT_BASE64" | base64 --decode > "$windows_host_tmp"
[[ "$(sha256sum "$windows_host_tmp" | awk '{print $1}')" == "$TASKGATE_WINDOWS_ENVIRONMENT_SHA256" ]] || { echo "Windows host manifest bytes do not match digest" >&2; exit 1; }
if [[ -e "$windows_host_path" ]]; then
  cmp --silent "$windows_host_tmp" "$windows_host_path" || { echo "Windows host manifest changed across deployments" >&2; exit 1; }
else
  install -m 600 "$windows_host_tmp" "$windows_host_path"
fi
rm -f "$windows_host_tmp"
trap - EXIT

export COMPOSE_PROJECT_NAME="$(
  bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh \
    "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"
)"
[[ "$COMPOSE_PROJECT_NAME" =~ ^taskgate-final-v5-deployment-0[1-3]-[0-9a-f]{20}$ ]] || {
  echo "derived Compose project name violates the exact deployment contract" >&2; exit 1;
}
formal_compose_files=compose.yaml:compose.debug.yaml:evaluation/final-v5-wsl2/compose.real-pilot.yaml:evaluation/final-v5-wsl2/compose.provsql.yaml
if [[ -n "${TASKGATE_COMPOSE_FILES:-}" && "$TASKGATE_COMPOSE_FILES" != "$formal_compose_files" ]]; then
  echo "publication Compose files are source-controlled and cannot be overridden" >&2
  exit 2
fi
export TASKGATE_COMPOSE_FILES="$formal_compose_files"
export TASKGATE_FINAL_V5_DIRECT_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25534/final_v5_provsql?sslmode=disable'
export TASKGATE_FINAL_V5_PROVSQL_DSN='postgres://postgres:final-v5-provsql-local-only@127.0.0.1:25535/final_v5_provsql?sslmode=disable'
export TASKGATE_FRESH_PROOF_OUTPUT="$fresh_proof_path"
export TASKGATE_FINAL_V5_RQ5_DRIVER="$(realpath "$rq5_driver_binary")"
export TASKGATE_FINAL_V5_RQ5_DRIVER_SHA256="$rq5_driver_digest"
export TASKGATE_FINAL_V5_RQ5_GENERATOR_SHA256="$rq5_generator_sha"
export TASKGATE_FINAL_V5_RQ5_CONFIG_SHA256="$rq5_config_sha"
export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST="$(realpath "$rq5_driver_build_manifest")"
export TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256="$rq5_driver_build_manifest_sha"
export TASKGATE_FINAL_V5_RQ5_REPO_ROOT="$repo"
export TASKGATE_FINAL_V5_RQ5_RUN_ROOT="$repo/$campaign_root/rq5-live/$TASKGATE_DEPLOYMENT_ID"
export TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID="$TASKGATE_CAMPAIGN_ID"
export TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID="$TASKGATE_DEPLOYMENT_ID"
export TASKGATE_FINAL_V5_RQ5_PROJECT="$(
  bash evaluation/final-v5-wsl2/scripts/rq5-project-prefix.sh \
    "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"
)"
[[ "$TASKGATE_FINAL_V5_RQ5_PROJECT" =~ ^[a-z0-9][a-z0-9_-]{0,30}$ ]] || {
  echo "derived RQ5 project prefix exceeds the driver contract" >&2; exit 1;
}
mkdir -m 700 -p "$TASKGATE_FINAL_V5_RQ5_RUN_ROOT"
export TASKGATE_FINAL_V5_RQ5_SECRET_ROOT="$(mktemp -d "/tmp/taskgate-rq5-secrets.${TASKGATE_DEPLOYMENT_ID}.XXXXXXXX")"
[[ "$TASKGATE_FINAL_V5_RQ5_SECRET_ROOT" =~ ^/tmp/taskgate-rq5-secrets\.deployment-0[1-3]\.[A-Za-z0-9]+$ ]] || {
  echo "mktemp returned an unsafe RQ5 secret root" >&2; exit 1;
}
[[ "$(stat -c '%a' "$TASKGATE_FINAL_V5_RQ5_SECRET_ROOT")" == 700 ]] || {
  echo "RQ5 secret root is not mode 0700" >&2; exit 1;
}
cleanup_rq5_secret_setup_exit() {
  local status=$?
  trap - EXIT
  bash evaluation/final-v5-wsl2/scripts/rq5-secret-root-cleanup.sh \
    "$TASKGATE_FINAL_V5_RQ5_SECRET_ROOT" || status=1
  exit "$status"
}
trap cleanup_rq5_secret_setup_exit EXIT
rq5_fixture_project="$TASKGATE_FINAL_V5_RQ5_PROJECT-fixture"
rq5_business_network="$TASKGATE_FINAL_V5_RQ5_PROJECT-business"
rq5_owner_sha256="$(printf '%s' "$TASKGATE_FINAL_V5_RQ5_RUN_ROOT" | sha256sum | awk '{print $1}')"
rq5_compose_file=evaluation/daily-publication-online/compose.yaml
rq5_cleanup_env=(env
  "DAILY_RQ5_BUSINESS_NETWORK=$rq5_business_network"
  DAILY_RQ5_OA_SERVICE_TOKEN=cleanup
  DAILY_RQ5_OA_CALLBACK_SECRET=cleanup
  DAILY_RQ5_OA_RECEIPT_KEY_ID=cleanup
  DAILY_RQ5_OA_RECEIPT_PRIVATE_KEY=cleanup
  DAILY_RQ5_OA_SESSION_SECRET=cleanup
  DAILY_RQ5_OA_ALICE_PASSWORD=cleanup
  DAILY_RQ5_OA_BOB_PASSWORD=cleanup
  DAILY_RQ5_GATEWAY_CALLBACK_URL=http://rq5-cleanup.invalid/api/v1/oa/callback)

rq5_cycle_project_is_owned() {
  local project="$1"
  [[ "$project" =~ ^${TASKGATE_FINAL_V5_RQ5_PROJECT}-c[1-4]-[0-9a-f]{12}$ ]]
}

rq5_recorded_cycle_projects() {
  local workspace project
  local -a workspaces=()
  shopt -s nullglob
  workspaces=("$TASKGATE_FINAL_V5_RQ5_RUN_ROOT"/cycles/*/cycle-workspace.json)
  shopt -u nullglob
  for workspace in "${workspaces[@]}"; do
    [[ -f "$workspace" && ! -L "$workspace" ]] || {
      echo "invalid recorded RQ5 cycle workspace: $workspace" >&2
      return 1
    }
    project="$(jq -er --arg network "$rq5_business_network" '
      .project as $project
      | select(type == "object" and .schema_version == 1)
      | select(.business_network == $network)
      | select(.gateway_container == ($project + "-gateway-slot"))
      | $project
    ' "$workspace")" || {
      echo "malformed recorded RQ5 cycle workspace: $workspace" >&2
      return 1
    }
    rq5_cycle_project_is_owned "$project" || {
      echo "recorded RQ5 project is outside this deployment: $project" >&2
      return 1
    }
    printf '%s\n' "$project"
  done
}

rq5_project_resource_ids() {
  local project="$1" resource="$2"
  case "$resource" in
    container) docker ps --all --quiet --filter "label=com.docker.compose.project=$project" ;;
    volume) docker volume ls --quiet --filter "label=com.docker.compose.project=$project" ;;
    network) docker network ls --quiet --filter "label=com.docker.compose.project=$project" ;;
    *) return 2 ;;
  esac
}

cleanup_rq5_project() {
  local project="$1" status=0 resource output
  local -a ids=()
  if [[ "$project" != "$rq5_fixture_project" ]] && ! rq5_cycle_project_is_owned "$project"; then
    echo "refusing to clean an RQ5 project outside this deployment: $project" >&2
    return 1
  fi
  for resource in container volume network; do
    output="$(rq5_project_resource_ids "$project" "$resource")" || return 1
    [[ -z "$output" ]] || break
  done
  [[ -n "${output:-}" ]] || return 0

  "${rq5_cleanup_env[@]}" docker compose --project-name "$project" --file "$rq5_compose_file" \
    down --volumes --remove-orphans >/dev/null 2>&1 || status=1

  output="$(rq5_project_resource_ids "$project" container)" || { status=1; output=""; }
  ids=(); [[ -z "$output" ]] || mapfile -t ids <<< "$output"
  ((${#ids[@]} == 0)) || docker container rm --force --volumes "${ids[@]}" >/dev/null 2>&1 || status=1
  output="$(rq5_project_resource_ids "$project" volume)" || { status=1; output=""; }
  ids=(); [[ -z "$output" ]] || mapfile -t ids <<< "$output"
  ((${#ids[@]} == 0)) || docker volume rm "${ids[@]}" >/dev/null 2>&1 || status=1
  output="$(rq5_project_resource_ids "$project" network)" || { status=1; output=""; }
  ids=(); [[ -z "$output" ]] || mapfile -t ids <<< "$output"
  ((${#ids[@]} == 0)) || docker network rm "${ids[@]}" >/dev/null 2>&1 || status=1

  for resource in container volume network; do
    output="$(rq5_project_resource_ids "$project" "$resource")" || status=1
    [[ -z "$output" ]] || status=1
  done
  return "$status"
}

cleanup_rq5_business_network() {
  local status=0 owner remaining
  remaining="$(docker network ls --quiet --filter "name=^${rq5_business_network}$")" || return 1
  [[ -n "$remaining" ]] || return 0
  owner="$(docker network inspect "$rq5_business_network" \
    --format '{{ index .Labels "taskgate.rq5.owner" }}')" || return 1
  [[ "$owner" == "$rq5_owner_sha256" ]] || {
    echo "refusing to remove RQ5 Business network owned by another deployment" >&2
    return 1
  }
  docker network rm "$rq5_business_network" >/dev/null 2>&1 || status=1
  remaining="$(docker network ls --quiet --filter "name=^${rq5_business_network}$")" || status=1
  [[ -z "$remaining" ]] || status=1
  return "$status"
}

cleanup_rq5_deployment() {
  local status=0 projects_output project
  local -a projects=()
  projects_output="$(rq5_recorded_cycle_projects)" || status=1
  if [[ -n "$projects_output" ]]; then
    mapfile -t projects <<< "$projects_output"
  fi
  for project in "${projects[@]}"; do
    cleanup_rq5_project "$project" || status=1
  done
  cleanup_rq5_project "$rq5_fixture_project" || status=1
  cleanup_rq5_business_network || status=1
  return "$status"
}

record_rq5_cleanup_proof() {
  local cleanup_status="$1" proof="$TASKGATE_FINAL_V5_RQ5_RUN_ROOT/deployment-cleanup.json"
  (set -o noclobber; jq -n \
    --arg fixture_project "$rq5_fixture_project" \
    --arg business_network "$rq5_business_network" \
    --arg owner_sha256 "$rq5_owner_sha256" \
    --arg status "$cleanup_status" \
    '{schema_version:1,fixture_project:$fixture_project,business_network:$business_network,
      owner_sha256:$owner_sha256,status:$status}' > "$proof") || return 1
  chmod 600 "$proof"
}

finalize_rq5_cleanup() {
  local cleanup_status=pass status=0
  cleanup_rq5_deployment || { cleanup_status=fail; status=1; }
  record_rq5_cleanup_proof "$cleanup_status" || status=1
  return "$status"
}

cleanup_rq5_secret_root() {
  bash evaluation/final-v5-wsl2/scripts/rq5-secret-root-cleanup.sh \
    "$TASKGATE_FINAL_V5_RQ5_SECRET_ROOT"
}

compose_build=(docker compose --project-name "$COMPOSE_PROJECT_NAME")
IFS=: read -r -a build_files <<< "$TASKGATE_COMPOSE_FILES"
for build_file in "${build_files[@]}"; do compose_build+=(--file "$build_file"); done
cleanup_early_deployment() {
  status=$?
  trap - EXIT
  finalize_rq5_cleanup || status=1
  cleanup_rq5_secret_root || status=1
  "${compose_build[@]}" down >/dev/null 2>&1 || status=1
  exit "$status"
}
trap - EXIT
trap cleanup_early_deployment EXIT
"${compose_build[@]}" build
evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh

# Resolve the exact values already consumed by Compose without sourcing .env
# as shell code and without persisting the interpolated JSON. The unified
# adapter runs on the host, so it needs these live bindings explicitly.
compose_json="$("${compose_build[@]}" config --format json)"
service_env() { jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<< "$compose_json"; }
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
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
concurrency_token="$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_TOKEN)"
connector_capacity="$(service_env gateway GATEWAY_CONNECTOR_MAX_CONNECTIONS)"
control_capacity="$(service_env gateway GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS)"
control_port="$("${compose_build[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
business_port="$("${compose_build[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
object_port="$("${compose_build[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
for required_value in "$alice_token" "$carol_token" "$alice_password" "$bob_password" \
  "$control_password" "$control_database" "$business_password" "$business_database" \
  "$business_admin_password" "$object_access_key" "$object_secret_key" "$object_bucket" \
  "$concurrency_token" "$control_port" "$business_port" "$object_port"; do
  [[ -n "$required_value" ]] || { echo "Compose omitted a required formal adapter binding" >&2; exit 1; }
done
[[ "$connector_capacity" =~ ^[0-9]+$ && "$control_capacity" =~ ^[0-9]+$ ]] || {
  echo "formal concurrency capacities are not integers" >&2; exit 1;
}
(( connector_capacity >= 32 && control_capacity >= 32 )) || {
  echo "formal width-500 run requires explicit connector and Control pool capacities of at least 32" >&2; exit 1;
}
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
export TASKGATE_FINAL_V5_CONCURRENCY_TOKEN="$concurrency_token"
fresh_proof_sha="$(sha256sum "$fresh_proof_path" | awk '{print $1}')"

# Exercise the exact source-built observer once against the live fresh Compose
# project before any measured runner starts.  Use a minimal environment so
# adapter credentials cannot be inherited by the observer, and retain no
# bootstrap snapshot (the measured samples carry their own before/after pairs).
observer_bootstrap_env=(env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin "COMPOSE_PROJECT_NAME=$COMPOSE_PROJECT_NAME")
if [[ -n "${DOCKER_HOST:-}" ]]; then observer_bootstrap_env+=("DOCKER_HOST=$DOCKER_HOST"); fi
if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then observer_bootstrap_env+=("XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR"); fi
observer_bootstrap_before="$("${observer_bootstrap_env[@]}" "$TASKGATE_FINAL_V5_OBSERVER" --phase before)"
observer_bootstrap_after="$("${observer_bootstrap_env[@]}" "$TASKGATE_FINAL_V5_OBSERVER" --phase after)"
printf '%s\n%s\n' "$observer_bootstrap_before" "$observer_bootstrap_after" | jq -s -e '
  def nonnegative_integer: type == "number" and . >= 0 and floor == .;
  def positive_integer: type == "number" and . > 0 and floor == .;
  def valid_snapshot($phase):
  (keys | sort) == ([
    "business_sql_queries", "business_wal_bytes", "container_restarts", "control_wal_bytes",
    "gateway_cpu_usec", "gateway_memory_peak_bytes", "gateway_network_rx_bytes",
    "gateway_network_tx_bytes", "memory_scope", "oom_events", "phase", "runtime_identity_sha256",
    "schema_version"
  ] | sort) and
  .schema_version == 1 and .memory_scope == "cgroup_v2_memory_peak_including_mmap" and .phase == $phase and
  (.runtime_identity_sha256 | test("^[0-9a-f]{64}$")) and
  (.gateway_memory_peak_bytes | positive_integer) and (.control_wal_bytes | positive_integer) and
  (.business_wal_bytes | positive_integer) and (.gateway_cpu_usec | nonnegative_integer) and
  (.gateway_network_rx_bytes | nonnegative_integer) and (.gateway_network_tx_bytes | nonnegative_integer) and
  (.business_sql_queries | nonnegative_integer) and (.oom_events | nonnegative_integer) and
  (.container_restarts | nonnegative_integer);
  length == 2 and (.[0] | valid_snapshot("before")) and (.[1] | valid_snapshot("after")) and
  .[0].runtime_identity_sha256 == .[1].runtime_identity_sha256 and
  .[0].oom_events == 0 and .[1].oom_events == 0 and
  .[0].container_restarts == 0 and .[1].container_restarts == 0 and
  .[1].gateway_memory_peak_bytes >= .[0].gateway_memory_peak_bytes and
  .[1].gateway_cpu_usec >= .[0].gateway_cpu_usec and
  .[1].gateway_network_rx_bytes >= .[0].gateway_network_rx_bytes and
  .[1].gateway_network_tx_bytes >= .[0].gateway_network_tx_bytes and
  .[1].business_sql_queries >= .[0].business_sql_queries and
  .[1].control_wal_bytes >= .[0].control_wal_bytes and
  .[1].business_wal_bytes >= .[0].business_wal_bytes and
  .[1].oom_events >= .[0].oom_events and .[1].container_restarts >= .[0].container_restarts
' >/dev/null || { echo "source-built observer bootstrap transition is invalid" >&2; exit 1; }
unset observer_bootstrap_before observer_bootstrap_after

experiment_roots=()
for index in "${!run_names[@]}"; do
  run_name="${run_names[$index]}"
  experiment_root="$campaign_root/$run_name"
  mkdir -m 700 -p "$experiment_root/raw" "$experiment_root/environment" "$experiment_root/deployments"
  [[ -f "$experiment_root/config.json" && ! -L "$experiment_root/config.json" && "$(stat -c '%a' "$experiment_root/config.json")" == 600 ]] || { echo "frozen config is missing or unsafe: $run_name" >&2; exit 1; }
  if [[ -e "$experiment_root/adapter.sha256" ]]; then
    [[ -f "$experiment_root/adapter.sha256" && ! -L "$experiment_root/adapter.sha256" && "$(stat -c '%a' "$experiment_root/adapter.sha256")" == 600 &&
      "$(tr -d '[:space:]' < "$experiment_root/adapter.sha256")" == "$adapter_digest" ]] || { echo "adapter changed across deployments: $run_name" >&2; exit 1; }
  else
    printf '%s\n' "$adapter_digest" > "$experiment_root/adapter.sha256"
    chmod 600 "$experiment_root/adapter.sha256"
  fi
  if [[ -e "$experiment_root/adapter-build.json" ]]; then
    [[ -f "$experiment_root/adapter-build.json" && ! -L "$experiment_root/adapter-build.json" && "$(stat -c '%a' "$experiment_root/adapter-build.json")" == 600 ]] &&
      cmp --silent "$adapter_build_manifest" "$experiment_root/adapter-build.json" || { echo "adapter build manifest changed across deployments: $run_name" >&2; exit 1; }
  else
    install -m 600 "$adapter_build_manifest" "$experiment_root/adapter-build.json"
  fi
  if [[ -e "$experiment_root/observer.sha256" ]]; then
    [[ -f "$experiment_root/observer.sha256" && ! -L "$experiment_root/observer.sha256" && "$(stat -c '%a' "$experiment_root/observer.sha256")" == 600 &&
      "$(tr -d '[:space:]' < "$experiment_root/observer.sha256")" == "$observer_digest" ]] || { echo "observer changed across deployments: $run_name" >&2; exit 1; }
  else
    printf '%s\n' "$observer_digest" > "$experiment_root/observer.sha256"
    chmod 600 "$experiment_root/observer.sha256"
  fi
  if [[ -e "$experiment_root/observer-build.json" ]]; then
    [[ -f "$experiment_root/observer-build.json" && ! -L "$experiment_root/observer-build.json" && "$(stat -c '%a' "$experiment_root/observer-build.json")" == 600 ]] &&
      cmp --silent "$observer_build_manifest" "$experiment_root/observer-build.json" || { echo "observer build manifest changed across deployments: $run_name" >&2; exit 1; }
  else
    install -m 600 "$observer_build_manifest" "$experiment_root/observer-build.json"
  fi
  if [[ "$run_name" == rq5 ]]; then
    if [[ -e "$experiment_root/rq5-driver-build.json" ]]; then
      [[ -f "$experiment_root/rq5-driver-build.json" && ! -L "$experiment_root/rq5-driver-build.json" && "$(stat -c '%a' "$experiment_root/rq5-driver-build.json")" == 600 ]] &&
        cmp --silent "$rq5_driver_build_manifest" "$experiment_root/rq5-driver-build.json" || { echo "RQ5 driver build manifest changed across deployments" >&2; exit 1; }
    else
      install -m 600 "$rq5_driver_build_manifest" "$experiment_root/rq5-driver-build.json"
    fi
    if [[ -e "$experiment_root/rq5-driver.sha256" ]]; then
      [[ -f "$experiment_root/rq5-driver.sha256" && ! -L "$experiment_root/rq5-driver.sha256" && "$(stat -c '%a' "$experiment_root/rq5-driver.sha256")" == 600 &&
        "$(tr -d '[:space:]' < "$experiment_root/rq5-driver.sha256")" == "$rq5_driver_digest" ]] || { echo "RQ5 driver changed across deployments" >&2; exit 1; }
    else
      printf '%s\n' "$rq5_driver_digest" > "$experiment_root/rq5-driver.sha256"
      chmod 600 "$experiment_root/rq5-driver.sha256"
    fi
  fi
  go run "./evaluation/cmd/${commands[$index]}" -config "$experiment_root/config.json" -validate-only >/dev/null
  experiment_roots+=("$experiment_root")
done

started_at="$(date -u +%FT%TZ)"
started_epoch="$(date +%s)"
swap_in_before="$(awk '$1=="pswpin"{print $2}' /proc/vmstat)"
swap_out_before="$(awk '$1=="pswpout"{print $2}' /proc/vmstat)"
install -m 600 /proc/vmstat "$vmstat_before_path"
daemon_id_before="$(docker info --format '{{.ID}}')"

finish_deployment() {
  status=$?
  trap - EXIT
  set +e
  finished_at="$(date -u +%FT%TZ)"
  finished_epoch="$(date +%s)"
  swap_in_after="$(awk '$1=="pswpin"{print $2}' /proc/vmstat)"
  swap_out_after="$(awk '$1=="pswpout"{print $2}' /proc/vmstat)"
  install -m 600 /proc/vmstat "$vmstat_after_path" || status=1
  swap_in_delta=$((swap_in_after-swap_in_before)); swap_out_delta=$((swap_out_after-swap_out_before))
  ((swap_in_delta >= 0)) || swap_in_delta=0
  ((swap_out_delta >= 0)) || swap_out_delta=0
  restarts="$(docker events --since "$started_epoch" --until "$finished_epoch" --filter event=restart --format '{{.ID}}' 2>/dev/null | wc -l)"
  oom_events="$(docker events --since "$started_epoch" --until "$finished_epoch" --filter event=oom --format '{{.ID}}' 2>/dev/null | wc -l)"
  daemon_id_after="$(docker info --format '{{.ID}}' 2>/dev/null)"
  [[ -n "$daemon_id_after" && "$daemon_id_after" == "$daemon_id_before" ]] || restarts=$((restarts+1))
  environment_sha="$(sha256sum "$environment_path" 2>/dev/null | awk '{print $1}')"
  vmstat_before_sha="$(sha256sum "$vmstat_before_path" 2>/dev/null | awk '{print $1}')"
  vmstat_after_sha="$(sha256sum "$vmstat_after_path" 2>/dev/null | awk '{print $1}')"
  oom_flag=(); ((oom_events == 0)) || oom_flag=(--oom)

  # Cleanup is part of the deployment result, not an afterthought. Perform it
  # before sealing record-deployment so any RQ5/main Compose cleanup failure is
  # carried by exit_status and cannot be finalized as a passing deployment.
  finalize_rq5_cleanup || status=1
  cleanup_rq5_secret_root || status=1
  compose_cleanup=(docker compose --project-name "$COMPOSE_PROJECT_NAME")
  IFS=: read -r -a cleanup_files <<< "$TASKGATE_COMPOSE_FILES"
  for cleanup_file in "${cleanup_files[@]}"; do compose_cleanup+=(--file "$cleanup_file"); done
  "${compose_cleanup[@]}" down >/dev/null 2>&1 || status=1

  for experiment_root in "${experiment_roots[@]}"; do
    mkdir -m 700 -p "$experiment_root/environment" "$experiment_root/deployments" || status=1
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.json" ]]; then
      install -m 600 "$environment_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.json" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-before.txt" ]]; then
      install -m 600 "$vmstat_before_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-before.txt" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-after.txt" ]]; then
      install -m 600 "$vmstat_after_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-after.txt" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json" ]]; then
      install -m 600 "$fresh_proof_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json" ]]; then
      install -m 600 "$fresh_volume_inspect_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml" ]]; then
      install -m 600 "$fresh_compose_config_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt" ]]; then
      install -m 600 "$fresh_dataset_fingerprint_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.catalog.yaml" ]]; then
      install -m 600 "$fresh_catalog_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.catalog.yaml" || status=1
    fi
    if [[ ! -e "$experiment_root/environment/windows-host.json" ]]; then
      install -m 600 "$windows_host_path" "$experiment_root/environment/windows-host.json" || status=1
    fi
    go run ./evaluation/cmd/final-v5 record-deployment \
      --output "$experiment_root/deployments/$TASKGATE_DEPLOYMENT_ID.json" \
      --campaign-id "$TASKGATE_CAMPAIGN_ID" --deployment-id "$TASKGATE_DEPLOYMENT_ID" \
      --environment-sha256 "$environment_sha" --started-at "$started_at" --finished-at "$finished_at" \
      --fresh-deployment-proof-sha256 "$fresh_proof_sha" \
      --windows-environment-sha256 "$TASKGATE_WINDOWS_ENVIRONMENT_SHA256" \
      --vmstat-before-sha256 "$vmstat_before_sha" --vmstat-after-sha256 "$vmstat_after_sha" \
      --exit-status "$status" --swap-in-delta "$swap_in_delta" --swap-out-delta "$swap_out_delta" \
      --unexpected-container-restarts "$restarts" "${oom_flag[@]}" || status=1
  done
  exit "$status"
}
trap - EXIT
trap finish_deployment EXIT

export TASKGATE_ENVIRONMENT_OUTPUT="$environment_path"
evaluation/final-v5-wsl2/scripts/record-environment.sh
for index in "${!run_names[@]}"; do
  experiment_root="${experiment_roots[$index]}"
  go run "./evaluation/cmd/${commands[$index]}" \
    -config "$experiment_root/config.json" \
    -deployment-id "$TASKGATE_DEPLOYMENT_ID" \
    -adapter "$adapter_binary" \
    -output "$experiment_root/raw/$TASKGATE_DEPLOYMENT_ID.jsonl"
done
