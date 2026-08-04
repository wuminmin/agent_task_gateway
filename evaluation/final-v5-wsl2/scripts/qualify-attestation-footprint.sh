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
"${compose[@]}" up -d --wait --wait-timeout 600 >"$outdir/compose-up.log" 2>&1 || {
  echo "deployment failed; see $outdir/compose-up.log" >&2
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

admin_dsn="postgres://postgres:postgres-04f459a04bd431eba54c2a8271bb1db34c7e5c812e1b83db@127.0.0.1:25434/travel_demo?sslmode=disable"
reader_dsn="postgres://gateway_reader:gateway-reader-66c7f869ec350adeb83e105185c2d72690e6077765e43157@127.0.0.1:25434/travel_demo?sslmode=disable"

echo "== qualifying against ${PROFILE_CATALOG}"
DIAGNOSIS_ID="$diagnosis_id" \
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-attestation-footprint \
  --gateway-reader-dsn "$reader_dsn" \
  --admin-dsn "$admin_dsn" \
  --catalog "$PROFILE_CATALOG" \
  --postgresql-identity "$outdir/postgresql-identity.json" \
  --datasource-id taskgate-demo-travel \
  --database travel_demo \
  --reader-role gateway_reader \
  --repetitions "$REPETITIONS" \
  --out "$outdir/attestation-footprint-v2.json" | tee "$outdir/qualification.log"

echo "== ${QUALIFICATION_ID} complete: $outdir"
