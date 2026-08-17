#!/usr/bin/env bash
# Render the exact profile-campaign Compose topology without starting services
# and reconcile every source-controlled concurrency capacity with the Gateway.
set -euo pipefail

[[ $# -ge 2 ]] || {
  echo "usage: $0 DEPLOYMENT_CONFIGURATION COMPOSE_FILE..." >&2
  exit 2
}
configuration="$1"
shift
[[ -f "$configuration" && ! -L "$configuration" ]] || {
  echo "deployment configuration is missing or unsafe" >&2
  exit 2
}
for command in docker jq; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 2; }
done

compose=(docker compose)
for compose_file in "$@"; do
  [[ -f "$compose_file" && ! -L "$compose_file" ]] || {
    echo "Compose file is missing or unsafe: $compose_file" >&2
    exit 2
  }
  compose+=(--file "$compose_file")
done

profile="$(jq -er '.profile_alias' "$configuration")"
active="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE // empty' "$configuration")"
queue="$(jq -r '.environment.GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE // empty' "$configuration")"
connector="$(jq -r '.environment.GATEWAY_CONNECTOR_MAX_CONNECTIONS // empty' "$configuration")"
control="$(jq -r '.environment.GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS // empty' "$configuration")"
if [[ -z "$active$queue$connector$control" ]]; then
  printf 'profile deployment Compose capacity: profile=%s overrides=0 compose_files=%d\n' "$profile" "$#"
  exit 0
fi
for value in "$active" "$queue" "$connector" "$control"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || {
    echo "profile deployment capacity set is incomplete or invalid" >&2
    exit 1
  }
done

export GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE="$active"
export GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE="$queue"
export GATEWAY_CONNECTOR_MAX_CONNECTIONS="$connector"
export GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS="$control"
compose_json="$("${compose[@]}" config --format json)"
service_env() {
  jq -r --arg service "$1" --arg name "$2" '.services[$service].environment[$name] // empty' <<<"$compose_json"
}
[[ "$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE)" == "$active" &&
   "$(service_env gateway GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE)" == "$queue" &&
   "$(service_env gateway GATEWAY_CONNECTOR_MAX_CONNECTIONS)" == "$connector" &&
   "$(service_env gateway GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS)" == "$control" ]] || {
  echo "Compose profile capacity binding drift" >&2
  exit 1
}
printf 'profile deployment Compose capacity: profile=%s active=%s queue=%s connector=%s control=%s compose_files=%d\n' \
  "$profile" "$active" "$queue" "$connector" "$control" "$#"
