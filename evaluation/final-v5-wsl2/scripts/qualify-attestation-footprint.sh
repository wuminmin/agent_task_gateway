#!/usr/bin/env bash
# Non-formal Attestation-footprint qualification against a fresh, isolated full
# topology.
#
# NOT a Campaign, NOT an activation smoke, NOT publication-eligible. It changes
# no capability, no activation support and no contract state.
#
# It brings up its own Compose project with its own volumes, waits for the
# Gateway to become ready, captures the complete immutable PostgreSQL runtime
# identity from the running container, runs the qualification probe against the
# activated Profile Catalog, and tears the deployment down again.
#
# Two runs of this script with different QUALIFICATION_ID values are the two
# independent qualifications the footprint must reproduce. They are deliberately
# sequential: business-postgres publishes a fixed host port, and sharing it would
# make the two runs anything but isolated.
set -euo pipefail

QUALIFICATION_ID="${QUALIFICATION_ID:?set QUALIFICATION_ID, e.g. qualification-01}"
PROFILE_CATALOG="${PROFILE_CATALOG:-config/profiles/result-heavy.catalog.yaml}"
PROFILE_ID="${PROFILE_ID:-profile-a86cd4df5cad6e26}"
REPETITIONS="${REPETITIONS:-3}"
KEEP_UP="${KEEP_UP:-0}"

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

commit="$(git rev-parse HEAD)"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
diagnosis_id="attestation-footprint-${QUALIFICATION_ID}-${stamp}-${commit:0:12}"
project="taskgate-n4-${QUALIFICATION_ID}-${commit:0:12}"
outdir="evaluation/final-v5-wsl2/raw/diagnosis-${diagnosis_id}"

# Never overwrite a retained run directory.
[[ -e "$outdir" ]] && { echo "refusing to overwrite $outdir" >&2; exit 2; }

# The Gateway must activate exactly the Profile Catalog being qualified.
# compose.yaml defaults TASKGATE_PROFILE_CATALOG to the full config/catalog.yaml,
# whose closure exceeds the 160 MiB hot-artifact activation boundary and which is
# in any case a different ExpectedSchema from the one the footprint qualifies.
export TASKGATE_PROFILE_CATALOG="./${PROFILE_CATALOG#./}"

compose=(docker compose --project-name "$project"
  --file compose.yaml
  --file compose.debug.yaml
  --file evaluation/final-v5-wsl2/compose.real-pilot.yaml
  --file evaluation/final-v5-wsl2/compose.provsql.yaml
  # The observer-v3 override is last: it carries the /health/live periodic probe
  # and the immutable PostgreSQL image pin.
  --file evaluation/final-v5-wsl2/compose.observer-v3.yaml)

cleanup() {
  if [[ "$KEEP_UP" == "1" ]]; then
    echo "leaving $project up (KEEP_UP=1)"
    return
  fi
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$outdir"
cat >"$outdir/DIAGNOSIS-NOT-FOR-PUBLICATION" <<MARKER
publication_eligible=false
capability_changing=false
activation_support_changing=false
formal_campaign=false
diagnosis_id=${diagnosis_id}
qualification_id=${QUALIFICATION_ID}
commit=${commit}
compose_project=${project}
MARKER

echo "== ${QUALIFICATION_ID}: fresh deployment ${project}"
"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true

# Phase 1: everything except the Gateway.
#
# The three snapshot-index services populate one shared artifact volume with
# every publication in the repository. The Gateway activates exactly one Profile
# closure and fails closed on any publication that closure does not declare, so
# it cannot be started against the shared volume. Phase 2 gives it a per-profile
# directory instead.
#
# --wait is not used here. It treats any container exit as a stop condition and
# returns non-zero even for a one-shot service that exited 0, which is exactly
# what the snapshot-index and object-store-init jobs do on success. The
# long-running services are polled for health and the one-shot jobs for a zero
# exit code instead, so a real failure is still caught.
phase1_services=(business-postgres control-postgres oa-demo
  result-object-store result-object-store-init
  snapshot-index-detail snapshot-index-summary snapshot-index-result-heavy snapshot-index-scale-e7
  snapshot-sidecar-install final-v5-direct-postgres final-v5-provsql-postgres)
phase1_healthy=(business-postgres control-postgres oa-demo
  result-object-store final-v5-direct-postgres final-v5-provsql-postgres)
phase1_jobs=(result-object-store-init snapshot-index-detail snapshot-index-summary
  snapshot-index-result-heavy snapshot-index-scale-e7 snapshot-sidecar-install)

retain_failure() {
  "${compose[@]}" ps --all >>"$outdir/compose-up.log" 2>&1 || true
  "${compose[@]}" logs --no-color --tail 200 >"$outdir/compose-logs-failure.log" 2>&1 || true
}

"${compose[@]}" up -d "${phase1_services[@]}" >"$outdir/compose-up.log" 2>&1 || {
  echo "phase 1 failed to start; see $outdir/compose-up.log" >&2
  retain_failure
  exit 1
}

for service in "${phase1_healthy[@]}"; do
  for attempt in $(seq 1 120); do
    container="$("${compose[@]}" ps -q "$service")"
    state="$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || echo unknown)"
    [[ "$state" == healthy ]] && break
    [[ "$attempt" == 120 ]] && {
      echo "$service never became healthy (last state: $state)" >&2; retain_failure; exit 1; }
    sleep 2
  done
done

for service in "${phase1_jobs[@]}"; do
  for attempt in $(seq 1 180); do
    container="$("${compose[@]}" ps -aq "$service")"
    running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null || echo true)"
    if [[ "$running" == false ]]; then
      code="$(docker inspect --format '{{.State.ExitCode}}' "$container")"
      [[ "$code" == 0 ]] || {
        echo "$service exited $code" >&2; retain_failure; exit 1; }
      break
    fi
    [[ "$attempt" == 180 ]] && { echo "$service never completed" >&2; retain_failure; exit 1; }
    sleep 2
  done
done
echo "phase 1: all services healthy, all jobs completed"

# Materialize the per-profile artifact directory from the verified full one.
full_artifacts="$outdir/snapshot-index-artifacts-full"
profile_artifacts_root="$outdir/profile-artifacts"
mkdir -p "$full_artifacts" "$profile_artifacts_root"
artifact_volume="${project}_snapshot-index-artifacts"
# The copy runs as root because the volume's files are root-owned and mode 0700;
# a non-root copier silently produces empty directories. Ownership is handed to
# the invoking user inside the same container so the Go materializer can read it.
docker run --rm \
  --volume "${artifact_volume}:/data:ro" \
  --volume "${repo}/${full_artifacts}:/out" \
  alpine:3.20 sh -c "cp -R /data/. /out/ && chown -R $(id -u):$(id -g) /out" \
  >>"$outdir/compose-up.log" 2>&1

# A copy that produced no regular file at all means the volume was unreadable.
copied="$(find "$full_artifacts" -type f | wc -l)"
[[ "$copied" -gt 0 ]] || { echo "the snapshot artifact volume copied no files" >&2; exit 1; }
echo "copied $copied artifact files from $artifact_volume"

echo "== materializing artifacts for ${PROFILE_ID}"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-artifacts \
  --profile-id "$PROFILE_ID" \
  --source "$full_artifacts" \
  --destination "$profile_artifacts_root" \
  --manifest-out "$outdir/profile-artifact-manifest.json" | tee "$outdir/profile-artifacts.log"

profile_dir="$(python3 -c "
import json,sys
m=json.load(open('$outdir/profile-artifact-manifest.json'))
p=m['profiles']['$PROFILE_ID']
print(p.get('directory') or p.get('Directory') or '')")"
[[ -n "$profile_dir" ]] || profile_dir="${profile_artifacts_root}/${PROFILE_ID}"
[[ -d "$profile_dir" ]] || { echo "materialized profile directory $profile_dir is absent" >&2; exit 1; }
export TASKGATE_PROFILE_ARTIFACT_DIR="$(cd "$profile_dir" && pwd)"
echo "profile artifact dir: $TASKGATE_PROFILE_ARTIFACT_DIR"

# Phase 2: the Gateway, against exactly its own closure.
"${compose[@]}" up -d --wait --wait-timeout 600 --no-deps gateway >>"$outdir/compose-up.log" 2>&1 || {
  echo "gateway failed to start; see $outdir/compose-up.log" >&2
  "${compose[@]}" ps --all >>"$outdir/compose-up.log" 2>&1 || true
  # Retain the failure evidence: without per-service logs the run directory
  # records that something failed but not what.
  "${compose[@]}" logs --no-color --tail 200 >"$outdir/compose-logs-failure.log" 2>&1 || true
  exit 1
}

# The periodic probe must be /health/live: /health/ready performs a full
# Attestation and would contaminate every interval.
health="$("${compose[@]}" ps --format json gateway | python3 -c 'import sys,json
raw=sys.stdin.read().strip()
rows=[json.loads(l) for l in raw.splitlines() if l.strip()] if not raw.startswith("[") else json.loads(raw)
print(rows[0].get("Health",""))')"
echo "gateway health: ${health}"

gateway_container="$("${compose[@]}" ps -q gateway)"
probe_command="$(docker inspect --format '{{json .Config.Healthcheck.Test}}' "$gateway_container")"
echo "$probe_command" | grep -q '/health/live' || {
  echo "the running Gateway does not carry the /health/live periodic probe: $probe_command" >&2
  exit 1
}

# Readiness is proven explicitly, outside any measurement window: the probe
# resets pg_stat_statements itself after this point.
echo "== proving readiness explicitly (outside the measurement window)"
for attempt in $(seq 1 60); do
  if curl --fail --silent --output /dev/null http://127.0.0.1:8082/health/ready; then
    echo "readiness: pass (attempt ${attempt})"
    break
  fi
  [[ "$attempt" == 60 ]] && { echo "gateway never became ready" >&2; exit 1; }
  sleep 2
done

# Complete PostgreSQL runtime identity, from the running container.
pg_container="$("${compose[@]}" ps -q business-postgres)"
container_image_id="$(docker inspect --format '{{.Image}}' "$pg_container")"
image_reference="$(docker inspect --format '{{.Config.Image}}' "$pg_container")"
local_image_id="$(docker image inspect --format '{{.Id}}' "$image_reference")"
repo_digest="$(docker image inspect --format '{{index .RepoDigests 0}}' "$image_reference")"
platform="$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image_reference")"

python3 - "$outdir/postgresql-identity.json" <<PYIDENT
import json, sys
json.dump({
    "image_reference": "${image_reference}",
    "repo_digest": "${repo_digest}",
    "local_image_id": "${local_image_id}",
    "container_image_id": "${container_image_id}",
    "platform": "${platform}",
}, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PYIDENT
echo "postgresql image: ${image_reference} on ${platform}"

# Credentials are read from the deployment rather than written here, and are
# passed to the probe through the environment rather than as flags: a flag lands
# in the process table, in shell history and in any log that echoes the command.
# Nothing below echoes them, and the probe never writes them to its report.
read_service_env() { # service, variable
  docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \
    "$("${compose[@]}" ps -q "$1")" | sed -n "s/^$2=//p" | head -1
}
postgres_password="$(read_service_env business-postgres POSTGRES_PASSWORD)"
reader_password="$(read_service_env business-postgres GATEWAY_DB_PASSWORD)"
[[ -n "$postgres_password" && -n "$reader_password" ]] || {
  echo "could not read deployment credentials from the running container" >&2; exit 1; }

export TASKGATE_QUALIFICATION_ADMIN_DSN="postgres://postgres:${postgres_password}@127.0.0.1:25434/travel_demo?sslmode=disable"
export TASKGATE_QUALIFICATION_READER_DSN="postgres://gateway_reader:${reader_password}@127.0.0.1:25434/travel_demo?sslmode=disable"
unset postgres_password reader_password

echo "== qualifying against ${PROFILE_CATALOG}"
DIAGNOSIS_ID="$diagnosis_id" \
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-attestation-footprint \
  --root "$repo" \
  --catalog "$PROFILE_CATALOG" \
  --profile-id "$PROFILE_ID" \
  --profile-artifact-manifest "$outdir/profile-artifact-manifest.json" \
  --postgresql-identity "$outdir/postgresql-identity.json" \
  --datasource-id taskgate-demo-travel \
  --database travel_demo \
  --reader-role gateway_reader \
  --repetitions "$REPETITIONS" \
  --out "$outdir/attestation-footprint-v2.json" | tee "$outdir/qualification.log"

echo "== ${QUALIFICATION_ID} complete: $outdir"
