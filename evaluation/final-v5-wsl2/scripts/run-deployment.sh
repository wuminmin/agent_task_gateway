#!/usr/bin/env bash
set -euo pipefail

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
[[ "$TASKGATE_WINDOWS_ENVIRONMENT_SHA256" =~ ^[0-9a-f]{64}$ ]] || { echo "Windows environment digest must be a SHA-256" >&2; exit 2; }
[[ "$TASKGATE_DEPLOYMENT_ID" =~ ^deployment-0[1-3]$ ]] || { echo "deployment ID must be deployment-01..03" >&2; exit 2; }
[[ -d "$TASKGATE_PRIVATE_CONFIG_DIR" && -f "$TASKGATE_DATASET_BINDINGS" ]] || { echo "private config or dataset binding path is missing" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
[[ "$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT" ]] || { echo "checkout does not match frozen commit" >&2; exit 1; }
evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode publication

campaign_root="evaluation/final-v5-wsl2/raw/$TASKGATE_CAMPAIGN_ID"
adapter_dir="$campaign_root/source-adapter"
adapter_binary="$adapter_dir/final-v5-adapter"
adapter_build_manifest="$adapter_dir/build-manifest.json"
mkdir -m 700 -p "$adapter_dir"
adapter_tmp="$(mktemp /tmp/taskgate-final-v5-adapter.XXXXXX)"
trap 'rm -f "$adapter_tmp"' EXIT
go build -trimpath -o "$adapter_tmp" ./evaluation/cmd/final-v5-adapter
adapter_digest="$(sha256sum "$adapter_tmp" | awk '{print $1}')"
if [[ -e "$adapter_binary" ]]; then
  [[ "$(sha256sum "$adapter_binary" | awk '{print $1}')" == "$adapter_digest" ]] || { echo "source-controlled adapter build changed across deployments" >&2; exit 1; }
else
  install -m 700 "$adapter_tmp" "$adapter_binary"
  source_listing="$(git ls-files -- '*.go' go.mod go.sum | sort | while IFS= read -r source_file; do printf '%s  %s\n' "$(sha256sum "$source_file" | awk '{print $1}')" "$source_file"; done)"
  source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"
  jq -n --arg submission_commit "$TASKGATE_SUBMISSION_COMMIT" --arg binary_sha256 "$adapter_digest" --arg source_sha256 "$source_sha" --arg go_version "$(go version)" --arg build_command "go build -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" --arg source_files "$source_listing" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,source_files:$source_files}' > "$adapter_build_manifest"
  chmod 600 "$adapter_build_manifest"
fi
rm -f "$adapter_tmp"
trap - EXIT
capabilities="$($adapter_binary --capabilities)"
jq -n -e --argjson capabilities "$capabilities" '$capabilities.schema_version == 1 and $capabilities.adapter == "final-v5-adapter" and (["baseline","scale","artifact","rls","attack","provsql","compiler","concurrency","rq5"] | all(. as $name | $capabilities.experiments[$name] == true))' >/dev/null || {
  echo "formal campaign blocked: unified source-controlled adapter capabilities are incomplete" >&2; exit 1;
}

environment_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.json"
fresh_proof_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json"
fresh_volume_inspect_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json"
fresh_compose_config_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml"
fresh_dataset_fingerprint_path="$campaign_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt"
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

project_slug="$(printf '%s' "$TASKGATE_CAMPAIGN_ID" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-' | cut -c1-36)"
[[ -n "$project_slug" ]] || { echo "campaign ID cannot form a safe Compose project" >&2; exit 2; }
export COMPOSE_PROJECT_NAME="taskgate-final-v5-$project_slug-$TASKGATE_DEPLOYMENT_ID"
export TASKGATE_COMPOSE_FILES="${TASKGATE_COMPOSE_FILES:-compose.yaml:compose.debug.yaml}"
export TASKGATE_FRESH_PROOF_OUTPUT="$fresh_proof_path"
evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh
fresh_proof_sha="$(sha256sum "$fresh_proof_path" | awk '{print $1}')"

run_names=(baseline scale artifact rls attack provsql compiler concurrency rq5)
commands=(v5-full v5-scale v5-artifact rls-adaptive adaptive-attacks taskgate-provsql-pair view-scale v5-concurrency v5-rq5)
configs=(baseline.json scale.json artifact.json rls.json attack.json provsql.json compiler.json concurrency.json rq5.json)
if [[ "${TASKGATE_ENABLE_SCALE_EXTREME:-0}" == 1 ]]; then
  run_names+=(scale-extreme); commands+=(v5-scale); configs+=(scale-extreme.json)
fi

experiment_roots=()
for index in "${!run_names[@]}"; do
  run_name="${run_names[$index]}"
  config_source="$TASKGATE_PRIVATE_CONFIG_DIR/${configs[$index]}"
  [[ -f "$config_source" ]] || { echo "missing config for $run_name" >&2; exit 2; }
  experiment_root="$campaign_root/$run_name"
  mkdir -m 700 -p "$experiment_root/raw" "$experiment_root/environment" "$experiment_root/deployments"
  if [[ -e "$experiment_root/config.json" ]]; then
    cmp --silent "$config_source" "$experiment_root/config.json" || { echo "config changed across deployments: $run_name" >&2; exit 1; }
  else
    install -m 600 "$config_source" "$experiment_root/config.json"
  fi
  if [[ -e "$experiment_root/adapter.sha256" ]]; then
    [[ "$(tr -d '[:space:]' < "$experiment_root/adapter.sha256")" == "$adapter_digest" ]] || { echo "adapter changed across deployments: $run_name" >&2; exit 1; }
  else
    printf '%s\n' "$adapter_digest" > "$experiment_root/adapter.sha256"
  fi
  [[ -e "$experiment_root/adapter-build.json" ]] || install -m 600 "$adapter_build_manifest" "$experiment_root/adapter-build.json"
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
  install -m 600 /proc/vmstat "$vmstat_after_path"
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
  for experiment_root in "${experiment_roots[@]}"; do
    mkdir -m 700 -p "$experiment_root/environment" "$experiment_root/deployments"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.json" ]] || install -m 600 "$environment_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.json"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-before.txt" ]] || install -m 600 "$vmstat_before_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-before.txt"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-after.txt" ]] || install -m 600 "$vmstat_after_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.vmstat-after.txt"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json" ]] || install -m 600 "$fresh_proof_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.json"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json" ]] || install -m 600 "$fresh_volume_inspect_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.volume-inspect.json"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml" ]] || install -m 600 "$fresh_compose_config_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.compose-config.yaml"
    [[ -e "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt" ]] || install -m 600 "$fresh_dataset_fingerprint_path" "$experiment_root/environment/$TASKGATE_DEPLOYMENT_ID.fresh.dataset-fingerprint.txt"
    [[ -e "$experiment_root/environment/windows-host.json" ]] || install -m 600 "$windows_host_path" "$experiment_root/environment/windows-host.json"
    go run ./evaluation/cmd/final-v5 record-deployment \
      --output "$experiment_root/deployments/$TASKGATE_DEPLOYMENT_ID.json" \
      --campaign-id "$TASKGATE_CAMPAIGN_ID" --deployment-id "$TASKGATE_DEPLOYMENT_ID" \
      --environment-sha256 "$environment_sha" --started-at "$started_at" --finished-at "$finished_at" \
      --fresh-deployment-proof-sha256 "$fresh_proof_sha" \
      --windows-environment-sha256 "$TASKGATE_WINDOWS_ENVIRONMENT_SHA256" \
      --vmstat-before-sha256 "$vmstat_before_sha" --vmstat-after-sha256 "$vmstat_after_sha" \
      --exit-status "$status" --swap-in-delta "$swap_in_delta" --swap-out-delta "$swap_out_delta" \
      --unexpected-container-restarts "$restarts" "${oom_flag[@]}"
  done
  compose_cleanup=(docker compose --project-name "$COMPOSE_PROJECT_NAME")
  IFS=: read -r -a cleanup_files <<< "$TASKGATE_COMPOSE_FILES"
  for cleanup_file in "${cleanup_files[@]}"; do compose_cleanup+=(--file "$cleanup_file"); done
  "${compose_cleanup[@]}" down >/dev/null 2>&1 || status=1
  exit "$status"
}
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
