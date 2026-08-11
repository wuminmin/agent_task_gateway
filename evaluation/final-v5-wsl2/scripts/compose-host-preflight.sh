#!/usr/bin/env bash
# Fail-fast, read-only host checks shared by the live Artifact and publication
# campaign runners. The exact Compose file set remains the source of truth for
# published ports; this helper never starts, stops, or removes Docker resources.
set -euo pipefail
umask 077

project="${1:-}"
shift || true
[[ "$project" =~ ^[a-z0-9][a-z0-9_-]{0,127}$ ]] || {
  echo "usage: $0 <compose-project> <compose-file> [compose-file ...]" >&2
  exit 2
}
(($# > 0)) || {
  echo "usage: $0 <compose-project> <compose-file> [compose-file ...]" >&2
  exit 2
}

for required_command in docker jq ss; do
  command -v "$required_command" >/dev/null 2>&1 || {
    echo "runner environment preflight cannot inspect the host: missing $required_command" >&2
    exit 1
  }
done

compose=(docker compose --project-name "$project")
for compose_file in "$@"; do
  [[ -f "$compose_file" && ! -L "$compose_file" ]] || {
    echo "runner environment preflight received a missing or unsafe Compose file: $compose_file" >&2
    exit 2
  }
  compose+=(--file "$compose_file")
done

# Compose interpolation consumes private .env values. Keep the resolved JSON in
# memory only and extract just the non-secret port tuples before diagnostics.
if ! compose_json="$("${compose[@]}" config --format json)"; then
  echo "runner environment preflight could not resolve the Compose topology" >&2
  exit 1
fi
if ! required_ports="$(jq -er '
  [.services | to_entries[] |
    .key as $service |
    (.value.ports // [])[] |
    select(.published != null) |
    {service:$service, host_ip:(.host_ip // "0.0.0.0"),
     published:(.published | tostring), target:(.target | tostring),
     protocol:(.protocol // "tcp")}]
  | if length == 0 then error("Compose topology publishes no host ports") else . end
  | sort_by((.published | tonumber), .protocol, .service)
  | .[] | [.service,.host_ip,.published,.target,.protocol] | @tsv
' <<< "$compose_json")"; then
  unset compose_json
  echo "runner environment preflight could not derive numeric published ports from Compose" >&2
  exit 1
fi
unset compose_json

failed=0
failure_header() {
  if ((failed == 0)); then
    echo "runner environment preflight failed for Compose project $project:" >&2
  fi
  failed=1
}

containers="$(docker ps --all --filter "label=com.docker.compose.project=$project" --format '{{.Names}}')" || {
  echo "runner environment preflight could not inspect Docker containers" >&2
  exit 1
}
volumes="$(docker volume ls --filter "label=com.docker.compose.project=$project" --format '{{.Name}}')" || {
  echo "runner environment preflight could not inspect Docker volumes" >&2
  exit 1
}
networks="$(docker network ls --filter "label=com.docker.compose.project=$project" --format '{{.Name}}')" || {
  echo "runner environment preflight could not inspect Docker networks" >&2
  exit 1
}
if [[ -n "$containers" || -n "$volumes" || -n "$networks" ]]; then
  failure_header
  echo "- an earlier Compose project with this exact identity still has resources" >&2
  [[ -z "$containers" ]] || printf '  containers: %s\n' "$(tr '\n' ' ' <<< "$containers" | sed 's/[[:space:]]*$//')" >&2
  [[ -z "$volumes" ]] || printf '  volumes: %s\n' "$(tr '\n' ' ' <<< "$volumes" | sed 's/[[:space:]]*$//')" >&2
  [[ -z "$networks" ]] || printf '  networks: %s\n' "$(tr '\n' ' ' <<< "$networks" | sed 's/[[:space:]]*$//')" >&2
  echo "  Action: confirm these belong to the interrupted run, then clean that exact project with:" >&2
  printf '    ' >&2
  printf '%q ' "${compose[@]}" down --volumes --remove-orphans >&2
  printf '\n' >&2
fi

declare -A required_by=()
port_count=0
while IFS=$'\t' read -r service host_ip published target protocol; do
  [[ "$published" =~ ^[0-9]+$ ]] && ((published >= 1 && published <= 65535)) || {
    echo "runner environment preflight resolved an invalid host port for $service: $published" >&2
    exit 1
  }
  [[ "$target" =~ ^[0-9]+$ ]] && ((target >= 1 && target <= 65535)) || {
    echo "runner environment preflight resolved an invalid container port for $service: $target" >&2
    exit 1
  }
  [[ "$protocol" == tcp || "$protocol" == udp ]] || {
    echo "runner environment preflight does not support Compose protocol $protocol" >&2
    exit 1
  }
  port_count=$((port_count + 1))
  port_key="$protocol:$published"
  if [[ -n "${required_by[$port_key]+present}" ]]; then
    failure_header
    echo "- Compose itself assigns $published/$protocol to both ${required_by[$port_key]} and $service" >&2
    continue
  fi
  required_by["$port_key"]="$service"

  owners="$(docker ps --filter "publish=$published/$protocol" \
    --format '{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}|{{.Ports}}')" || {
    echo "runner environment preflight could not attribute Docker port $published/$protocol" >&2
    exit 1
  }
  listener=""
  if [[ "$protocol" == tcp ]]; then
    listener="$(ss -H -ltn "sport = :$published" 2>/dev/null || true)"
  else
    listener="$(ss -H -lun "sport = :$published" 2>/dev/null || true)"
  fi
  [[ -n "$owners" || -n "$listener" ]] || continue

  failure_header
  echo "- required host port $host_ip:$published/$protocol for $service -> $target is already occupied" >&2
  dbtest_owner=0
  if [[ -n "$owners" ]]; then
    while IFS='|' read -r owner_name owner_project owner_service owner_ports; do
      [[ -n "$owner_name" ]] || continue
      [[ -n "$owner_project" ]] || owner_project="<not-compose-managed>"
      [[ -n "$owner_service" ]] || owner_service="<unknown>"
      echo "  occupying container: $owner_name" >&2
      echo "  compose project: $owner_project (service: $owner_service)" >&2
      echo "  published ports: $owner_ports" >&2
      if [[ "$owner_project" == taskgate-dbtest ]]; then
        dbtest_owner=1
      else
        echo "  Action: stop this owner before retrying (for example: docker stop $owner_name)." >&2
      fi
    done <<< "$owners"
  else
    echo "  host listener: $listener" >&2
    if command -v lsof >/dev/null 2>&1; then
      if [[ "$protocol" == tcp ]]; then
        process_owner="$(lsof -nP -iTCP:"$published" -sTCP:LISTEN 2>/dev/null | sed -n '2,$p' || true)"
      else
        process_owner="$(lsof -nP -iUDP:"$published" 2>/dev/null | sed -n '2,$p' || true)"
      fi
      [[ -z "$process_owner" ]] || echo "  process owner: $process_owner" >&2
    fi
    echo "  Action: stop the listed listener, verify the port is free, then rerun." >&2
  fi
  if ((dbtest_owner == 1)); then
    echo "  Action: run ./scripts/db-test-env.sh down, verify port $published is free, then rerun." >&2
  fi
done <<< "$required_ports"

if ((failed != 0)); then
  echo "Preflight made no changes: no containers, volumes, or networks were stopped or removed." >&2
  exit 2
fi

echo "runner environment preflight passed: project=$project; $port_count host ports free; no Compose residue"
