#!/usr/bin/env bash

# taskgate_formal_campaign_compose constructs the one permitted formal campaign
# Compose argv. Callers provide the fixed source-controlled list with the
# observer-v3 override last; this function inserts the private image-only
# override immediately before it and returns an array through a nameref.
taskgate_formal_campaign_compose() {
  if (( $# != 8 )); then
    echo "formal campaign Compose needs an output array, project, image override, and source files" >&2
    return 2
  fi
  local output_name="$1" project="$2" image_override="$3"
  shift 3
  local -a source_files=("$@")
  local -a expected_source_files=(
    compose.yaml
    compose.debug.yaml
    evaluation/final-v5-wsl2/compose.real-pilot.yaml
    evaluation/final-v5-wsl2/compose.provsql.yaml
    evaluation/final-v5-wsl2/compose.observer-v3.yaml
  )
  [[ "$output_name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
    echo "formal campaign Compose output name is unsafe" >&2; return 2;
  }
  [[ "$project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || {
    echo "formal campaign Compose project is unsafe" >&2; return 2;
  }
  [[ "$image_override" == /* && -f "$image_override" && ! -L "$image_override" &&
    "$(stat -c '%a' "$image_override")" == 600 ]] || {
    echo "formal Gateway image override is not an absolute regular mode-0600 file" >&2; return 2;
  }
  local index
  for index in "${!expected_source_files[@]}"; do
    [[ "${source_files[$index]}" == "${expected_source_files[$index]}" ]] || {
      echo "formal campaign Compose source list differs at index $index" >&2; return 2;
    }
  done
  local observer_index=$((${#source_files[@]}-1))
  [[ "${source_files[$observer_index]}" == evaluation/final-v5-wsl2/compose.observer-v3.yaml ]] || {
    echo "formal observer Compose override is not last" >&2; return 2;
  }
  local source_file
  for source_file in "${source_files[@]}"; do
    [[ -f "$source_file" && ! -L "$source_file" ]] || {
      echo "formal campaign Compose source is missing or unsafe: $source_file" >&2; return 2;
    }
  done

  local -n output="$output_name"
  output=(docker compose --project-name "$project")
  for ((index=0; index<observer_index; index++)); do
    output+=(--file "${source_files[$index]}")
  done
  output+=(--file "$image_override")
  output+=(--file "${source_files[$observer_index]}")
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "formal-campaign-compose.sh must be sourced" >&2
  exit 2
fi
