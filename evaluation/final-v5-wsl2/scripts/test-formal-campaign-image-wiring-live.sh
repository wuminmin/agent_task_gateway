#!/usr/bin/env bash
set -euo pipefail
umask 077

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
command -v git >/dev/null || { echo "git is required" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
command -v sha256sum >/dev/null || { echo "sha256sum is required" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
formal_source_root="${TASKGATE_FORMAL_SOURCE_ROOT:-$repo}"
[[ "$formal_source_root" == /* && -d "$formal_source_root" ]] || {
  echo "TASKGATE_FORMAL_SOURCE_ROOT must be an absolute repository directory" >&2; exit 2;
}
formal_source_root="$(git -C "$formal_source_root" rev-parse --show-toplevel)"
cd "$repo"
commit="$(git -C "$formal_source_root" rev-parse HEAD)"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "HEAD is not a full commit" >&2; exit 2; }
if [[ -n "${TASKGATE_FORMAL_GATEWAY_BUILD_BIN:-}" ]]; then
  [[ "$TASKGATE_FORMAL_GATEWAY_BUILD_BIN" == /* && -f "$TASKGATE_FORMAL_GATEWAY_BUILD_BIN" &&
    ! -L "$TASKGATE_FORMAL_GATEWAY_BUILD_BIN" && -x "$TASKGATE_FORMAL_GATEWAY_BUILD_BIN" ]] || {
    echo "TASKGATE_FORMAL_GATEWAY_BUILD_BIN must be an absolute executable regular file" >&2; exit 2;
  }
  gateway_build=("$TASKGATE_FORMAL_GATEWAY_BUILD_BIN")
  gateway_build_tool_kind=binary_sha256
  gateway_build_tool_identity="$(sha256sum "$TASKGATE_FORMAL_GATEWAY_BUILD_BIN" | awk '{print $1}')"
else
  gateway_build=(go run ./evaluation/cmd/final-v5-gateway-build)
  gateway_build_tool_kind=source_commit
  gateway_build_tool_identity="$(git rev-parse HEAD)"
fi

source_compose_files=(
  compose.yaml
  compose.debug.yaml
  evaluation/final-v5-wsl2/compose.real-pilot.yaml
  evaluation/final-v5-wsl2/compose.provsql.yaml
  evaluation/final-v5-wsl2/compose.observer-v3.yaml
)
project_nonce="$(printf '%s\0%s\0%s' "$commit" "$$" "$(date -u +%s%N)" | sha256sum | cut -c1-12)"
project="taskgate-p41-wiring-${commit:0:8}-${project_nonce}"
container_name="$project-gateway-proof"
existing_container_ids="$(docker container ls --all --format '{{.Names}} {{.ID}}' | awk -v name="$container_name" '$1 == name {print $2}')"
[[ -z "$existing_container_ids" ]] || {
  echo "live-test container name is already owned by another container: $container_name" >&2; exit 2;
}

# Match the unattended runner boundary: report conflicts without attempting to
# clean any resource that predates this uniquely named test project.
bash evaluation/final-v5-wsl2/scripts/compose-host-preflight.sh \
  "$project" "${source_compose_files[@]}"

workspace="$(mktemp -d /tmp/taskgate-p41-formal-wiring.XXXXXXXX)"
[[ "$workspace" =~ ^/tmp/taskgate-p41-formal-wiring\.[A-Za-z0-9]+$ ]] || {
  echo "mktemp returned an unsafe live-test workspace" >&2; exit 2;
}
[[ "$(stat -c '%a' "$workspace")" == 700 ]] || { echo "live-test workspace is not mode 0700" >&2; exit 2; }

manifest="$workspace/formal-gateway-build.json"
override="$workspace/compose.formal-gateway.yaml"
identity="$workspace/gateway-runtime-identity.json"
tag="taskgate-final-v5-gateway:p41-live-${commit}"

GOFLAGS=-buildvcs=false "${gateway_build[@]}" build \
  -root "$formal_source_root" \
  -tag "$tag" \
  -manifest-out "$manifest" \
  -compose-override-out "$override" \
  -profile-registry "$formal_source_root/config/profiles/registry.json" \
  | tee "$workspace/formal-gateway-build.log"
chmod 600 "$workspace/formal-gateway-build.log"

GOFLAGS=-buildvcs=false "${gateway_build[@]}" verify-build \
  -root "$formal_source_root" \
  -manifest "$manifest" \
  -compose-override "$override" \
  -profile-registry "$formal_source_root/config/profiles/registry.json" \
  > "$workspace/formal-gateway-verify.log"
chmod 600 "$workspace/formal-gateway-verify.log"

manifest_image_id="$(jq -er '.image_id' "$manifest")"
manifest_sha256="$(sha256sum "$manifest" | awk '{print $1}')"

# Negative 1: a syntactically valid manifest/override pair names an image ID
# that does not exist locally. Verification must fail before Compose is called.
missing_image_id="sha256:0000000000000000000000000000000000000000000000000000000000000000"
missing_manifest="$workspace/missing-image-manifest.json"
missing_override="$workspace/missing-image-override.yaml"
jq --arg image "$missing_image_id" '.image_id = $image' "$manifest" > "$missing_manifest"
sed "s/$manifest_image_id/$missing_image_id/" "$override" > "$missing_override"
chmod 600 "$missing_manifest" "$missing_override"
if GOFLAGS=-buildvcs=false "${gateway_build[@]}" verify-build \
  -root "$formal_source_root" -manifest "$missing_manifest" -compose-override "$missing_override" \
  -profile-registry "$formal_source_root/config/profiles/registry.json" \
  > "$workspace/missing-image.stdout" 2> "$workspace/missing-image.stderr"; then
  echo "missing formal image was accepted" >&2
  exit 1
fi
chmod 600 "$workspace/missing-image.stdout" "$workspace/missing-image.stderr"
grep -F 'formal Gateway manifest image is unavailable' "$workspace/missing-image.stderr" >/dev/null || {
  echo "missing-image negative failed for an unexpected reason" >&2; exit 1;
}

# Negative 2: the image exists, but the manifest source identity is corrupt.
# The immutable override alone must not be enough to admit it.
invalid_manifest="$workspace/invalid-source-manifest.json"
jq '.source_manifest_sha256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
  "$manifest" > "$invalid_manifest"
chmod 600 "$invalid_manifest"
if GOFLAGS=-buildvcs=false "${gateway_build[@]}" verify-build \
  -root "$formal_source_root" -manifest "$invalid_manifest" -compose-override "$override" \
  -profile-registry "$formal_source_root/config/profiles/registry.json" \
  > "$workspace/invalid-manifest.stdout" 2> "$workspace/invalid-manifest.stderr"; then
  echo "invalid formal build manifest was accepted" >&2
  exit 1
fi
chmod 600 "$workspace/invalid-manifest.stdout" "$workspace/invalid-manifest.stderr"
grep -F 'source-manifest digest' "$workspace/invalid-manifest.stderr" >/dev/null || {
  echo "invalid-manifest negative failed for an unexpected reason" >&2; exit 1;
}

# Use the exact same argv constructor as both production launch scripts, so the
# live proof cannot silently drift in image/observer override ordering.
# shellcheck source=formal-campaign-compose.sh
source evaluation/final-v5-wsl2/scripts/formal-campaign-compose.sh
taskgate_formal_campaign_compose compose "$project" "$override" "${source_compose_files[@]}"

created_container_id=""
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$created_container_id" ]]; then
    cleanup_project="$(docker container inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$created_container_id" 2>/dev/null)" || cleanup_project=""
    cleanup_service="$(docker container inspect --format '{{ index .Config.Labels "com.docker.compose.service" }}' "$created_container_id" 2>/dev/null)" || cleanup_service=""
    if [[ "$cleanup_project" == "$project" && "$cleanup_service" == gateway ]]; then
      docker container rm --force --volumes "$created_container_id" >/dev/null 2>&1 || status=1
    elif docker container inspect "$created_container_id" >/dev/null 2>&1; then
      echo "refusing to remove a live-test container whose ownership labels changed" >&2
      status=1
    fi
  fi
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || status=1
  exit "$status"
}
trap cleanup EXIT

compose_json="$("${compose[@]}" config --format json)"
jq -e --arg image "$manifest_image_id" '
  .services.gateway.image == $image and .services.gateway.pull_policy == "never"
' <<< "$compose_json" >/dev/null || { echo "live topology does not select the manifest image ID" >&2; exit 1; }
unset compose_json

# `compose run` builds only when --build is supplied; --pull never additionally
# makes an unavailable exact image ID a hard error rather than a registry path.
"${compose[@]}" run --detach --no-deps --pull never --name "$container_name" \
  --entrypoint /bin/sleep gateway 300 > "$workspace/compose-run.stdout"
chmod 600 "$workspace/compose-run.stdout"
container_id="$(docker container inspect --format '{{.Id}}' "$container_name")"
container_project="$(docker container inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$container_id")"
container_service="$(docker container inspect --format '{{ index .Config.Labels "com.docker.compose.service" }}' "$container_id")"
[[ "$container_project" == "$project" && "$container_service" == gateway ]] || {
  echo "live Gateway proof container does not carry this test's ownership labels" >&2; exit 1;
}
created_container_id="$container_id"
running_image_id="$(docker container inspect --format '{{.Image}}' "$container_id")"
[[ "$running_image_id" == "$manifest_image_id" ]] || {
  echo "live Gateway container does not run the manifest image ID" >&2; exit 1;
}

GOFLAGS=-buildvcs=false "${gateway_build[@]}" verify \
  -root "$formal_source_root" -container "$container_id" -out "$identity" \
  > "$workspace/formal-gateway-runtime-verify.log"
chmod 600 "$workspace/formal-gateway-runtime-verify.log"
jq -e --arg image "$manifest_image_id" '
  .local_image_id == $image and .container_image_id == $image and .build_target == "gateway"
' "$identity" >/dev/null || { echo "runtime identity does not close to the build manifest image ID" >&2; exit 1; }

profile_registry_sha256="$(sha256sum "$formal_source_root/config/profiles/registry.json" | awk '{print $1}')"
jq -e --arg registry "$profile_registry_sha256" '.profile_registry_sha256 == $registry' "$manifest" >/dev/null || {
  echo "build manifest does not bind the profile registry used by the live test" >&2; exit 1;
}

jq -n \
  --arg status pass \
  --arg submission_commit "$commit" \
  --arg image_id "$manifest_image_id" \
  --arg running_image_id "$running_image_id" \
  --arg build_manifest_sha256 "$manifest_sha256" \
  --arg runtime_identity_sha256 "$(jq -er '.aggregate_sha256' "$identity")" \
  --arg profile_registry_sha256 "$profile_registry_sha256" \
  --arg gateway_build_tool_kind "$gateway_build_tool_kind" \
  --arg gateway_build_tool_identity "$gateway_build_tool_identity" \
  --arg formal_source_root "$formal_source_root" \
  --arg compose_project "$project" \
  --arg missing_image_negative fail_closed \
  --arg invalid_manifest_negative fail_closed \
  --arg workspace "$workspace" \
  '{status:$status,submission_commit:$submission_commit,image_id:$image_id,
    running_image_id:$running_image_id,build_manifest_sha256:$build_manifest_sha256,
    runtime_identity_sha256:$runtime_identity_sha256,profile_registry_sha256:$profile_registry_sha256,
    gateway_build_tool:{kind:$gateway_build_tool_kind,identity:$gateway_build_tool_identity},
    formal_source_root:$formal_source_root,
    compose_project:$compose_project,
    missing_image_negative:$missing_image_negative,invalid_manifest_negative:$invalid_manifest_negative,
    formal_campaign_started:false,workspace:$workspace}' \
  | tee "$workspace/summary.json"
chmod 600 "$workspace/summary.json"
