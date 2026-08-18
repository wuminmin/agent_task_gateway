#!/usr/bin/env bash
set -euo pipefail

container_id="${1:?Gateway container ID is required}"
output="${2:?Gateway image record output is required}"
repo="${3:?repository root is required}"

[[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || {
  echo "Gateway container ID is malformed" >&2
  exit 2
}
[[ "$output" != / && ! -e "$output" && ! -L "$output" ]] || {
  echo "Gateway image record output already exists or is unsafe: $output" >&2
  exit 2
}
[[ -d "$repo/.git" || -f "$repo/.git" ]] || {
  echo "repository root is not a Git worktree: $repo" >&2
  exit 2
}
command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
command -v git >/dev/null || { echo "git is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

container_inspect="$(docker container inspect "$container_id")"
running="$(jq -er '.[0].State.Running' <<< "$container_inspect")"
[[ "$running" == true ]] || {
  echo "Gateway container is not running" >&2
  exit 1
}
container_image_id="$(jq -er '.[0].Image' <<< "$container_inspect")"
image_reference="$(jq -er '.[0].Config.Image' <<< "$container_inspect")"
[[ "$container_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "running Gateway container reports a malformed image ID" >&2
  exit 1
}

# Inspect by immutable ID. The Compose image reference is retained only as an
# informational lookup name because it can move after this capture.
image_inspect="$(docker image inspect "$container_image_id")"
local_image_id="$(jq -er '.[0].Id' <<< "$image_inspect")"
[[ "$local_image_id" == "$container_image_id" ]] || {
  echo "running Gateway image ID differs from its local image inspection" >&2
  exit 1
}

captured_at="$(date -u +%FT%TZ)"
repository_head="$(git -C "$repo" rev-parse HEAD)"
repository_worktree_clean=false
if [[ -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]]; then
  repository_worktree_clean=true
fi

# Ordinary Pilot images are not required to carry the formal build labels.
# Missing labels therefore remain JSON null: the checkout observed at capture
# time is not asserted to be the tree that produced the running image.
record="$(jq -n \
  --arg captured_at "$captured_at" \
  --arg container_id "$container_id" \
  --arg container_image_id "$container_image_id" \
  --arg image_reference "$image_reference" \
  --arg repository_head "$repository_head" \
  --argjson repository_worktree_clean "$repository_worktree_clean" \
  --argjson image "$(jq '.[0]' <<< "$image_inspect")" '
  def image_label($name): $image.Config.Labels[$name] // null;
  {
    schema_version: 1,
    record_kind: "taskgate-pilot-gateway-image-observation-v1",
    experiment_class: "pilot",
    provenance_assertion: "observation_only_not_publication_verification",
    formal_gateway_built: (image_label("taskgate.formal_build") == "v1"),
    formal_build_label: image_label("taskgate.formal_build"),
    captured_at: $captured_at,
    container_id: $container_id,
    image_reference: $image_reference,
    container_image_id: $container_image_id,
    image_id: $image.Id,
    repo_digests: ($image.RepoDigests // []),
    created: ($image.Created // null),
    platform: (
      if (($image.Os // "") != "" and ($image.Architecture // "") != "")
      then ($image.Os + "/" + $image.Architecture)
      else null
      end
    ),
    source_provenance: {
      submission_commit: image_label("org.opencontainers.image.revision"),
      build_context_sha256: image_label("taskgate.build_context_sha256"),
      source_manifest_sha256: image_label("taskgate.source_manifest_sha256"),
      build_target: image_label("taskgate.build_target"),
      builder_base_image: image_label("taskgate.builder_base_image"),
      runtime_base_image: image_label("taskgate.runtime_base_image"),
      clean_tree_at_build: null
    },
    repository_at_capture: {
      head: $repository_head,
      worktree_clean: $repository_worktree_clean,
      image_source_equivalence_asserted: false
    }
  }
')"

mkdir -m 700 -p "$(dirname "$output")"
(set -o noclobber; printf '%s\n' "$record" > "$output") || {
  echo "cannot create Gateway image record exclusively: $output" >&2
  exit 1
}
chmod 600 "$output"
