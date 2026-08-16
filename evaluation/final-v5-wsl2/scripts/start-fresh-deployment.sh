#!/usr/bin/env bash
set -euo pipefail

: "${COMPOSE_PROJECT_NAME:?COMPOSE_PROJECT_NAME is required}"
: "${TASKGATE_FRESH_PROOF_OUTPUT:?TASKGATE_FRESH_PROOF_OUTPUT is required}"
: "${TASKGATE_CAMPAIGN_ID:?TASKGATE_CAMPAIGN_ID is required}"
: "${TASKGATE_DEPLOYMENT_ID:?TASKGATE_DEPLOYMENT_ID is required}"

[[ "$COMPOSE_PROJECT_NAME" =~ ^taskgate-final-v5-deployment-0[1-3]-[0-9a-f]{20}$ ]] || {
  echo "refusing unsafe Compose project name" >&2; exit 2;
}
expected_compose_project="$(
  bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh \
    "$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"
)"
[[ "$COMPOSE_PROJECT_NAME" == "$expected_compose_project" ]] || {
  echo "Compose project does not match the complete campaign/deployment identity" >&2; exit 2;
}
[[ "$TASKGATE_FRESH_PROOF_OUTPUT" != / ]] || {
  echo "unsafe proof output" >&2; exit 2;
}
if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
  formal_compose_files=compose.yaml:compose.debug.yaml:evaluation/final-v5-wsl2/compose.real-pilot.yaml:evaluation/final-v5-wsl2/compose.provsql.yaml:evaluation/final-v5-wsl2/compose.observer-v3.yaml
  [[ "${TASKGATE_COMPOSE_FILES:-}" == "$formal_compose_files" ]] || {
    echo "publication Compose files differ from the frozen formal topology" >&2; exit 2;
  }
  : "${TASKGATE_FORMAL_GATEWAY_BUILD_MANIFEST:?publication formal Gateway build manifest is required}"
  : "${TASKGATE_FORMAL_GATEWAY_COMPOSE_OVERRIDE:?publication formal Gateway Compose override is required}"
  : "${TASKGATE_DATASET_BINDINGS:?publication Dataset Binding file is required}"
  : "${TASKGATE_FINAL_V5_BINDING_FILE_SHA256:?publication validated Dataset Binding digest is required}"
  : "${TASKGATE_FINAL_V5_PROFILE_REGISTRY:?publication profile registry is required}"
  : "${TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256:?publication frozen profile registry digest is required}"
fi
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 2; }
command -v git >/dev/null || { echo "git is required" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
cd "$repo"
compose=(docker compose --project-name "$COMPOSE_PROJECT_NAME")
if [[ -n "${TASKGATE_COMPOSE_FILES:-}" ]]; then
  IFS=: read -r -a taskgate_compose_files <<< "$TASKGATE_COMPOSE_FILES"
  if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
    for private_formal_file in "$TASKGATE_FORMAL_GATEWAY_BUILD_MANIFEST" "$TASKGATE_FORMAL_GATEWAY_COMPOSE_OVERRIDE"; do
      [[ -f "$private_formal_file" && ! -L "$private_formal_file" && "$(stat -c '%a' "$private_formal_file")" == 600 ]] || {
        echo "formal Gateway deployment input is missing or unsafe: $private_formal_file" >&2; exit 2;
      }
    done
    # shellcheck source=formal-campaign-compose.sh
    source evaluation/final-v5-wsl2/scripts/formal-campaign-compose.sh
    taskgate_formal_campaign_compose compose "$COMPOSE_PROJECT_NAME" \
      "$TASKGATE_FORMAL_GATEWAY_COMPOSE_OVERRIDE" "${taskgate_compose_files[@]}"
  else
    for taskgate_compose_file in "${taskgate_compose_files[@]}"; do
      [[ -f "$taskgate_compose_file" ]] || { echo "missing Compose file: $taskgate_compose_file" >&2; exit 2; }
      compose+=(--file "$taskgate_compose_file")
    done
  fi
fi
# Recompute source and deployment-input identities and inspect the exact local
# image before the destructive fresh-deployment boundary. Missing bytes,
# malformed bytes, a moved image tag, or an unavailable image all stop here;
# there is no path back to Compose's ordinary Gateway build.
if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build verify-build \
    -root "$repo" \
    -manifest "$TASKGATE_FORMAL_GATEWAY_BUILD_MANIFEST" \
    -compose-override "$TASKGATE_FORMAL_GATEWAY_COMPOSE_OVERRIDE" \
    -dataset-binding "$TASKGATE_DATASET_BINDINGS" \
    -profile-registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY"
  jq -e \
    --arg dataset "$TASKGATE_FINAL_V5_BINDING_FILE_SHA256" \
    --arg registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY_SHA256" '
      .dataset_binding_sha256 == $dataset and .profile_registry_sha256 == $registry
    ' "$TASKGATE_FORMAL_GATEWAY_BUILD_MANIFEST" >/dev/null || {
    echo "formal Gateway build manifest differs from the strictly validated Dataset/Profile identities" >&2; exit 2;
  }
  formal_gateway_image_id="$(jq -er '.image_id' "$TASKGATE_FORMAL_GATEWAY_BUILD_MANIFEST")"
fi
# Refuse the deployment before its exact deletion boundary is touched unless
# Compose proves that the ready Gateway must load the one frozen Catalog path
# from the one source-controlled read-only bind mount. The resolved JSON stays
# in-process because it may also contain Compose-resolved credentials.
compose_runtime_json="$("${compose[@]}" config --format json)"
# A deployment serves either the master Catalog or exactly one profile Catalog.
# The profile case is not a relaxation: the bytes must match the digest the
# source-controlled profile registry pins for that profile, so a hand-edited or
# stale profile Catalog is refused here rather than discovered by an Adapter
# after the topology is up.
catalog_path=config/catalog.yaml
if [[ -n "${TASKGATE_PROFILE_CATALOG:-}" ]]; then
  catalog_path="${TASKGATE_PROFILE_CATALOG#./}"
  [[ -f "$catalog_path" && ! -L "$catalog_path" ]] || {
    echo "profile Catalog is missing or unsafe: $catalog_path" >&2; exit 2;
  }
  profile_registry_path="${TASKGATE_FINAL_V5_PROFILE_REGISTRY:-config/profiles/registry.json}"
  [[ -f "$profile_registry_path" && ! -L "$profile_registry_path" ]] || {
    echo "profile registry is missing or unsafe: $profile_registry_path" >&2; exit 2;
  }
  pinned_catalog_sha="$(jq -r --arg path "$catalog_path" \
    '[.profiles[] | select(.catalog_path == $path) | .catalog_sha256] | unique | if length == 1 then .[0] else empty end' \
    "$profile_registry_path")"
  [[ "$pinned_catalog_sha" =~ ^[0-9a-f]{64}$ ]] || {
    echo "profile registry pins no unique digest for $catalog_path" >&2; exit 2;
  }
  [[ "$(sha256sum "$catalog_path" | awk '{print $1}')" == "$pinned_catalog_sha" ]] || {
    echo "profile Catalog bytes differ from the digest the profile registry pins" >&2; exit 1;
  }
fi
expected_catalog_source="$repo/$catalog_path"
jq -e --arg source "$expected_catalog_source" --arg target /etc/taskbound/catalog.yaml '
  .services.gateway.environment.CATALOG_PATH == $target and
  ([.services.gateway.volumes[] | select(.target == $target)] as $mounts |
    ($mounts | length) == 1 and
    $mounts[0].type == "bind" and $mounts[0].source == $source and $mounts[0].read_only == true)
' <<< "$compose_runtime_json" >/dev/null || {
  echo "Gateway Catalog path/mount differs from the frozen source topology" >&2; exit 2;
}
if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
  jq -e --arg image "$formal_gateway_image_id" '
    .services.gateway.image == $image and .services.gateway.pull_policy == "never"
  ' <<< "$compose_runtime_json" >/dev/null || {
    echo "resolved deployment does not select the verified formal Gateway image ID" >&2; exit 2;
  }
fi
unset compose_runtime_json

# The exact campaign/deployment project is the deletion boundary. A stale
# partial attempt is removed before creating new named volumes.
"${compose[@]}" down --volumes --remove-orphans
"${compose[@]}" up --no-build --detach --wait
if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
  gateway_container_id="$("${compose[@]}" ps -q gateway)"
  [[ "$gateway_container_id" =~ ^[0-9a-f]{64}$ ]] || {
    echo "formal deployment did not resolve exactly one Gateway container" >&2; exit 1;
  }
  running_gateway_image_id="$(docker container inspect --format '{{.Image}}' "$gateway_container_id")"
  [[ "$running_gateway_image_id" == "$formal_gateway_image_id" ]] || {
    echo "running Gateway image ID differs from the verified formal build manifest" >&2; exit 1;
  }
  unset gateway_container_id running_gateway_image_id
else
  gateway_container_id="$("${compose[@]}" ps -q gateway)"
  [[ "$gateway_container_id" =~ ^[0-9a-f]{64}$ ]] || {
    echo "pilot deployment did not resolve exactly one Gateway container" >&2; exit 1;
  }
  gateway_image_output="${TASKGATE_FRESH_PROOF_OUTPUT%.fresh.json}.gateway-image.json"
  evaluation/final-v5-wsl2/scripts/record-pilot-gateway-image.sh \
    "$gateway_container_id" "$gateway_image_output" "$repo"
  unset gateway_container_id gateway_image_output
fi

proof_dir="$(dirname "$TASKGATE_FRESH_PROOF_OUTPUT")"
mkdir -m 700 -p "$proof_dir"
tmp_dir="$(mktemp -d /tmp/taskgate-fresh-proof.XXXXXX)"
trap 'rm -rf "$tmp_dir"' EXIT

compose_config_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.compose-config.yaml"
# Preserve the canonical Compose topology without persisting values resolved
# from the local .env. Runtime bindings are consumed only in-process by the
# launcher and must never become Pilot or publication evidence.
"${compose[@]}" config --no-interpolate > "$compose_config_output"
chmod 600 "$compose_config_output"
compose_sha="$(sha256sum "$compose_config_output" | awk '{print $1}')"
mapfile -t volume_names < <("${compose[@]}" config --volumes | while IFS= read -r logical; do
  docker volume ls --filter "label=com.docker.compose.project=$COMPOSE_PROJECT_NAME" --filter "label=com.docker.compose.volume=$logical" --format '{{.Name}}'
done | sort -u)
(( ${#volume_names[@]} >= 5 )) || { echo "fresh deployment has too few Compose volumes" >&2; exit 1; }
printf '[]' > "$tmp_dir/volumes.json"
for volume_name in "${volume_names[@]}"; do
  inspect_path="$tmp_dir/volume-$(printf '%s' "$volume_name" | sha256sum | cut -c1-16).json"
  docker volume inspect "$volume_name" > "$inspect_path"
  inspect_sha="$(jq -cS '.[0]' "$inspect_path" | tr -d '\n' | sha256sum | awk '{print $1}')"
  created_at="$(jq -r '.[0].CreatedAt // ""' "$inspect_path")"
  driver="$(jq -r '.[0].Driver // ""' "$inspect_path")"
  jq --arg name "$volume_name" --arg created_at "$created_at" --arg driver "$driver" --arg inspect_sha "$inspect_sha" \
    '. + [{name:$name,created_at:$created_at,driver:$driver,inspect_sha256:$inspect_sha}]' \
    "$tmp_dir/volumes.json" > "$tmp_dir/volumes.next.json"
  mv "$tmp_dir/volumes.next.json" "$tmp_dir/volumes.json"
done
volume_inspect_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.volume-inspect.json"
jq -s 'map(.[0])' "$tmp_dir"/volume-*.json > "$volume_inspect_output"
chmod 600 "$volume_inspect_output"
volume_inspect_sha="$(sha256sum "$volume_inspect_output" | awk '{print $1}')"
volume_set_sha="$(jq -r '.[] | [.name,.created_at,.driver,.inspect_sha256] | @tsv' "$tmp_dir/volumes.json" | sort | sha256sum | awk '{print $1}')"

control_system_id="$("${compose[@]}" exec -T control-postgres psql -XAt -U postgres -d "${CONTROL_POSTGRES_DB:-taskbound_gateway}" -c 'SELECT system_identifier FROM pg_control_system()')"
business_system_id="$("${compose[@]}" exec -T business-postgres psql -XAt -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-travel_demo}" -c 'SELECT system_identifier FROM pg_control_system()')"
deployment_volume_id_sha="$(
  printf '%s\0%s\0%s\0%s' \
    'TASKGATE-FINAL-V5-DEPLOYMENT-VOLUME-ID-V1' "$volume_set_sha" "$control_system_id" "$business_system_id" \
    | sha256sum | awk '{print $1}'
)"

# Bind the exact source-controlled Catalog bytes to the bytes consumed by the
# live Gateway. A Compose override that replaces or omits the read-only mount
# therefore fails before any measured operation can start.
[[ -f "$catalog_path" && ! -L "$catalog_path" ]] || { echo "source Catalog is missing or unsafe" >&2; exit 2; }
catalog_sha="$(sha256sum "$catalog_path" | awk '{print $1}')"
catalog_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.catalog.yaml"
"${compose[@]}" exec -T gateway cat /etc/taskbound/catalog.yaml > "$catalog_output"
chmod 600 "$catalog_output"
runtime_catalog_sha="$(sha256sum "$catalog_output" | awk '{print $1}')"
[[ "$runtime_catalog_sha" == "$catalog_sha" ]] || { echo "live Gateway Catalog differs from frozen source bytes" >&2; exit 1; }

control_counts="$("${compose[@]}" exec -T control-postgres psql -XAt -U postgres -d "${CONTROL_POSTGRES_DB:-taskbound_gateway}" -c \
  "SELECT jsonb_build_object('tasks',(SELECT count(*) FROM tasks),'query_records',(SELECT count(*) FROM query_records),'root_heads',(SELECT count(*) FROM v4_exposure_root_heads)+(SELECT count(*) FROM v5_exposure_root_heads),'result_artifacts',(SELECT count(*) FROM result_artifacts))::text")"
jq -e '.tasks == 0 and .query_records == 0 and .root_heads == 0 and .result_artifacts == 0' <<< "$control_counts" >/dev/null || {
  echo "fresh Control PostgreSQL is not empty" >&2; exit 1;
}

default_probe_sql=evaluation/final-v5-wsl2/sql/datasets/benchmark-v1-probe.sql
if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication && -n "${TASKGATE_DATASET_FINGERPRINT_SQL:-}" ]]; then
  echo "publication dataset sanity-probe SQL is source-controlled and cannot be overridden" >&2
  exit 2
fi
probe_sql="${TASKGATE_DATASET_FINGERPRINT_SQL:-$default_probe_sql}"
[[ -f "$probe_sql" && ! -L "$probe_sql" ]] || { echo "dataset sanity-probe SQL is missing or unsafe" >&2; exit 2; }
dataset_probe_sql_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.dataset-probe.sql"
install -m 600 "$probe_sql" "$dataset_probe_sql_output"
dataset_probe_sql_sha="$(sha256sum "$dataset_probe_sql_output" | awk '{print $1}')"
dataset_probe_raw="$("${compose[@]}" exec -T business-postgres psql -XAt -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-travel_demo}" < "$dataset_probe_sql_output")"
dataset_probe_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.dataset-probe.txt"
printf '%s' "$dataset_probe_raw" > "$dataset_probe_output"
chmod 600 "$dataset_probe_output"
dataset_probe_sha="$(sha256sum "$dataset_probe_output" | awk '{print $1}')"

# Dataset identity is computed from a complete typed stream of all five live
# Products. The reviewed generator is regenerated only as the independent side
# of the retained agreement. The SQL query above remains a scalar sanity probe;
# neither its source bytes nor its result bytes may substitute for Dataset.
# Resolve the already-consumed Compose values in-process and discard them as
# soon as the credential-free agreement has been written.
compose_runtime_json="$("${compose[@]}" config --format json)"
business_reader_password="$(jq -er '.services["business-postgres"].environment.GATEWAY_DB_PASSWORD' <<< "$compose_runtime_json")"
business_database="$(jq -er '.services["business-postgres"].environment.POSTGRES_DB' <<< "$compose_runtime_json")"
business_port="$("${compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
[[ -n "$business_reader_password" && -n "$business_database" && "$business_port" =~ ^[0-9]+$ ]] || {
  echo "Compose omitted a live Dataset connection binding" >&2; exit 1;
}
urlencode() { printf '%s' "$1" | jq -sRr '@uri'; }
dataset_live_dsn="postgres://gateway_reader:$(urlencode "$business_reader_password")@127.0.0.1:$business_port/$(urlencode "$business_database")?sslmode=disable"
dataset_identity_output="${TASKGATE_FRESH_PROOF_OUTPUT%.json}.dataset-identity.json"
env -u BUSINESS_TEST_POSTGRES_DSN TASKGATE_FINAL_V5_BUSINESS_DSN="$dataset_live_dsn" GOFLAGS=-buildvcs=false \
  go run ./evaluation/cmd/final-v5-oracle dataset-fingerprint-live > "$dataset_identity_output"
chmod 600 "$dataset_identity_output"
unset compose_runtime_json business_reader_password business_database business_port dataset_live_dsn
dataset_identity="$(<"$dataset_identity_output")"
jq -e '
  .version == "taskgate-final-v5-benchmark-dataset-agreement-v1" and
  .agreed == true and .prepared_statement_count == 0 and (.products | length) == 5 and
  .reference.dataset_spec_id == "taskgate-final-v5-benchmark-dataset-v1" and
  .observed.dataset_spec_id == .reference.dataset_spec_id and
  .reference.generator_version == "taskgate-final-v5-benchmark-generator-v1" and
  .observed.generator_version == .reference.generator_version and
  .reference.seed == 20260803 and .observed.seed == .reference.seed and
  .reference.product_count == 5 and .observed.product_count == 5 and
  .reference.row_count == 815000 and .observed.row_count == 815000 and
  .reference.peak_buffered_rows == 1 and .observed.peak_buffered_rows == 1 and
  (.reference.sha256 | test("^[0-9a-f]{64}$")) and .observed.sha256 == .reference.sha256
' <<< "$dataset_identity" >/dev/null || { echo "typed Dataset identity is invalid" >&2; exit 1; }
dataset_sha="$(jq -er .observed.sha256 <<< "$dataset_identity")"
dataset_identity_evidence_sha="$(sha256sum "$dataset_identity_output" | awk '{print $1}')"

if [[ "${TASKGATE_EXPERIMENT_CLASS:-pilot}" == publication ]]; then
  : "${TASKGATE_FINAL_V5_DATASET_SHA256:?publication typed Dataset identity binding is required}"
  : "${TASKGATE_FINAL_V5_DATASET_PROBE_SQL_SHA256:?publication Dataset probe SQL binding is required}"
  : "${TASKGATE_FINAL_V5_DATASET_PROBE_SHA256:?publication Dataset probe result binding is required}"
fi
[[ -z "${TASKGATE_FINAL_V5_DATASET_SHA256:-}" || "$dataset_sha" == "$TASKGATE_FINAL_V5_DATASET_SHA256" ]] || {
  echo "typed Dataset identity differs from the reviewed binding" >&2; exit 1; }
[[ -z "${TASKGATE_FINAL_V5_DATASET_PROBE_SQL_SHA256:-}" || "$dataset_probe_sql_sha" == "$TASKGATE_FINAL_V5_DATASET_PROBE_SQL_SHA256" ]] || {
  echo "Dataset sanity-probe SQL differs from the reviewed binding" >&2; exit 1; }
[[ -z "${TASKGATE_FINAL_V5_DATASET_PROBE_SHA256:-}" || "$dataset_probe_sha" == "$TASKGATE_FINAL_V5_DATASET_PROBE_SHA256" ]] || {
  echo "live Dataset sanity-probe result differs from the reviewed binding" >&2; exit 1; }

bucket="${GATEWAY_OBJECT_STORE_BUCKET:-taskgate-results}"
minio_object_count="$("${compose[@]}" exec -T result-object-store sh -ec 'mc find "local/'"$bucket"'" 2>/dev/null | wc -l')"
[[ "$minio_object_count" == 0 ]] || { echo "fresh MinIO bucket contains old result objects" >&2; exit 1; }

snapshot_listing="$("${compose[@]}" exec -T gateway sh -ec 'find /var/lib/taskgate/snapshot-index -type f -print0 | sort -z | xargs -0 sha256sum')"
[[ -n "$snapshot_listing" ]] || { echo "snapshot artifact volume is empty" >&2; exit 1; }
snapshot_sha="$(printf '%s' "$snapshot_listing" | sha256sum | awk '{print $1}')"

started_at="$(date -u +%FT%TZ)"
jq -n \
  --arg campaign_id "$TASKGATE_CAMPAIGN_ID" --arg deployment_id "$TASKGATE_DEPLOYMENT_ID" \
  --arg captured_at "$started_at" --arg compose_project_name "$COMPOSE_PROJECT_NAME" \
  --arg compose_config_sha256 "$compose_sha" --arg volume_set_sha256 "$volume_set_sha" \
  --arg volume_inspect_sha256 "$volume_inspect_sha" \
  --arg control_pg_system_identifier "$control_system_id" --arg business_pg_system_identifier "$business_system_id" \
	  --arg deployment_volume_id_sha256 "$deployment_volume_id_sha" --arg catalog_sha256 "$catalog_sha" \
	  --arg dataset_sha256 "$dataset_sha" --arg dataset_identity_evidence_sha256 "$dataset_identity_evidence_sha" \
	  --arg dataset_probe_sql_sha256 "$dataset_probe_sql_sha" \
  --arg dataset_probe_sha256 "$dataset_probe_sha" --arg snapshot_artifact_volume_sha256 "$snapshot_sha" \
  --argjson volumes "$(<"$tmp_dir/volumes.json")" --argjson control_initial_counts "$control_counts" \
  --argjson minio_initial_object_count "$minio_object_count" \
  '{schema_version:2,campaign_id:$campaign_id,deployment_id:$deployment_id,captured_at:$captured_at,
    compose_project_name:$compose_project_name,compose_config_sha256:$compose_config_sha256,
    volumes:$volumes,volume_set_sha256:$volume_set_sha256,volume_inspect_sha256:$volume_inspect_sha256,
    control_pg_system_identifier:$control_pg_system_identifier,business_pg_system_identifier:$business_pg_system_identifier,
	    deployment_volume_id_sha256:$deployment_volume_id_sha256,catalog_sha256:$catalog_sha256,
	    control_initial_counts:$control_initial_counts,dataset_sha256:$dataset_sha256,
	    dataset_identity_evidence_sha256:$dataset_identity_evidence_sha256,
	    dataset_probe_sql_sha256:$dataset_probe_sql_sha256,dataset_probe_sha256:$dataset_probe_sha256,
    minio_initial_object_count:$minio_initial_object_count,snapshot_artifact_volume_sha256:$snapshot_artifact_volume_sha256}' \
  > "$TASKGATE_FRESH_PROOF_OUTPUT"
chmod 600 "$TASKGATE_FRESH_PROOF_OUTPUT"
