#!/usr/bin/env bash
# Targeted validation of the six frozen artifact/result-heavy cells, through v3
# acceptance, against a fresh isolated full topology.
#
# NOT a Campaign, NOT publication-eligible, NOT an activation smoke. It changes
# no capability, no activation support and no contract state. It runs with
# campaign_class=pilot and pilot_kind=artifact_targeted, which is the reviewed
# run class for exactly this.
#
# # Why this is not a flag on run-deployment.sh
#
# run-deployment.sh is the formal nine-experiment campaign runner. It brings one
# deployment up through start-fresh-deployment.sh and runs baseline, scale,
# artifact, rls, attack, provsql, compiler, concurrency and rq5 against it in
# sequence -- and neither it nor start-fresh-deployment.sh sets
# TASKGATE_PROFILE_CATALOG, so the Gateway serves the master ./config/catalog.yaml.
#
# The Artifact path since the v3 cutover requires the Gateway to be serving the
# Result-heavy PROFILE Catalog, byte-identical to the one the registry pins and
# the qualification was measured against. Those two models do not compose: point
# the campaign at the profile Catalog and the other eight experiments lose the
# Products they read; leave it on the master Catalog and the Artifact arm is
# refused by the profile resolver, correctly but for a reason that reads like a
# different fault.
#
# So the artifact v3 run is a targeted single-profile run, which is what this is.
#
# # What must already be true
#
# The profile registry must have cleared this profile for a targeted run. That
# clearance comes from a live activation smoke -- activate-profile.sh, then
# final-v5-activation-support, then final-v5-profile to regenerate the registry.
# This script refuses up front rather than letting the adapter discover it after
# a deployment has been built, but it deliberately does not perform the
# activation itself: a measured run must never be able to grant the readiness it
# depends on.
#
# # Bring-up
#
# The two-phase bring-up below is the one qualify-attestation-footprint.sh uses,
# for the same reason -- the Gateway activates exactly one Profile closure and
# fails closed on any publication outside it, so it cannot start against the
# shared artifact volume. The duplication is deliberate and temporary: that
# script produces retained qualification evidence and cannot be re-verified from
# a checkout, so it is not being refactored underneath a run that has not
# happened yet. Merge the two once this one has run green.
set -euo pipefail
umask 077

# ---------------------------------------------------------- operator inputs

# The retained qualification and the PostgreSQL identity it was measured
# against. Both are REQUIRED and neither has a default.
#
# Defaulting them to the repository's diagnosis-only qualification would make a
# run that is meant to validate the artifact path quietly cite diagnostic
# evidence, and the resulting Sample would look exactly like one that had cited
# a real qualification. They come from one qualification run and must be that
# run's pair: a footprint is evidence only while it is bound to the server it
# was measured on.
ATTESTATION_QUALIFICATION="${ATTESTATION_QUALIFICATION:?set ATTESTATION_QUALIFICATION to the retained attestation-footprint-v2.json this run is judged against}"
POSTGRESQL_IDENTITY="${POSTGRESQL_IDENTITY:?set POSTGRESQL_IDENTITY to the postgresql-identity.json from the SAME qualification run}"
: "${TASKGATE_DATASET_BINDINGS:?set TASKGATE_DATASET_BINDINGS to the private deployment dataset binding}"
RUN_ID="${RUN_ID:?set RUN_ID, e.g. artifact-targeted-01}"

PROFILE_CATALOG="${PROFILE_CATALOG:-config/profiles/result-heavy.catalog.yaml}"
PROFILE_ID="${PROFILE_ID:-profile-a86cd4df5cad6e26}"
PROFILE_ALIAS="${PROFILE_ALIAS:-result-heavy}"
PROFILE_REGISTRY="${PROFILE_REGISTRY:-config/profiles/registry.json}"
# One sample per cell by default. The question this run answers is whether the
# path completes and is accepted, which one sample settles; a distribution is
# what the publication campaign is for, and a pilot may carry at most three.
SAMPLES="${SAMPLES:-1}"
WARMUPS="${WARMUPS:-0}"
KEEP_UP="${KEEP_UP:-0}"

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

# The evidence is commit-bound, so it is only meaningful from a clean tree: a
# Sample naming a commit whose worktree had uncommitted changes names bytes
# nobody can recover.
[[ -z "$(git status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "a targeted artifact run requires a clean worktree for commit-bound evidence" >&2
  exit 2
}
commit="$(git rev-parse HEAD)"
export TASKGATE_SUBMISSION_COMMIT="$commit"

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
run_name="${RUN_ID}-${stamp}-${commit:0:12}"
project="taskgate-artifact-${RUN_ID}-${commit:0:12}"
outdir="evaluation/final-v5-wsl2/raw/targeted-${run_name}"
[[ -e "$outdir" ]] && { echo "refusing to overwrite $outdir" >&2; exit 2; }

for input in "$ATTESTATION_QUALIFICATION" "$POSTGRESQL_IDENTITY" "$TASKGATE_DATASET_BINDINGS" "$PROFILE_CATALOG" "$PROFILE_REGISTRY"; do
  [[ -f "$input" && ! -L "$input" ]] || { echo "required input is missing or unsafe: $input" >&2; exit 2; }
done

# Structural checks only. The qualification's cryptographic self-check -- that
# its footprint digests to the digest it records, and qualifies this
# ExpectedSchema, environment and server -- belongs to the finalizer's resolver
# and is deliberately not repeated here: a second implementation of it could
# disagree with the one that decides.
jq -e '(.footprint | type == "object") and (.footprint_sha256 | test("^[0-9a-f]{64}$"))' \
  "$ATTESTATION_QUALIFICATION" >/dev/null || {
  echo "$ATTESTATION_QUALIFICATION does not carry a footprint and its digest" >&2; exit 2; }
jq -e 'has("image_reference") and has("repo_digest") and has("local_image_id") and
       has("container_image_id") and has("platform")' "$POSTGRESQL_IDENTITY" >/dev/null || {
  echo "$POSTGRESQL_IDENTITY is not a complete PostgreSQL runtime identity" >&2; exit 2; }

# ------------------------------------------------- the clearance, checked first
#
# Refused here rather than inside the adapter. Both the profile resolver and the
# Adapter's own profile binding apply this gate, so a run that fails it fails
# anyway -- but only after a full topology has been built and six cells
# attempted, which reads as a measurement failure rather than as a missing
# prerequisite.
jq -e --arg alias "$PROFILE_ALIAS" '
  .profiles | map(select(.alias == $alias)) | length == 1 and
  (.[0].targeted_run_eligible == true and .[0].status.activation_supported == true)
' "$PROFILE_REGISTRY" >/dev/null || {
  cat >&2 <<CLEARANCE
profile "$PROFILE_ALIAS" is not cleared for a targeted run in $PROFILE_REGISTRY.

A targeted run may only execute a profile whose live activation smoke has been
recorded; activation support does not carry across a contract release. Run the
activation first, then regenerate the registry:

  evaluation/final-v5-wsl2/scripts/activate-profile.sh \\
      --compose-project <project> --deployment-id <id> \\
      --profile-id $PROFILE_ID --evidence-out <evidence.json>
  go run ./evaluation/cmd/final-v5-activation-support ...
  go run ./evaluation/cmd/final-v5-profile ...

Current status:
$(jq -r --arg alias "$PROFILE_ALIAS" '.profiles[] | select(.alias == $alias) | .status' "$PROFILE_REGISTRY")
CLEARANCE
  exit 2
}

mkdir -m 700 -p "$outdir" "$outdir/raw" "$outdir/environment"
cat >"$outdir/TARGETED-NOT-FOR-PUBLICATION" <<MARKER
publication_eligible=false
capability_changing=false
activation_support_changing=false
formal_campaign=false
pilot_kind=artifact_targeted
run_id=${RUN_ID}
commit=${commit}
compose_project=${project}
profile_id=${PROFILE_ID}
attestation_qualification=${ATTESTATION_QUALIFICATION}
postgresql_identity=${POSTGRESQL_IDENTITY}
MARKER

# ------------------------------------------- the adapter and the observer

# Both are built from this checkout with a sealed build manifest, because the
# adapter verifies the observer's manifest against the submission commit before
# it will run it. The listing is over tracked files so the manifest describes the
# commit rather than the working directory.
source_listing="$(git ls-files | sort | while IFS= read -r file; do
  printf '%s  %s\n' "$(sha256sum "$file" | awk '{print $1}')" "$file"
done)"
source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"

build_sealed() { # target, out-binary, out-manifest, build-command
  local target="$1" binary="$2" manifest="$3" command="$4"
  GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$binary" "$target"
  chmod 700 "$binary"
  local digest
  digest="$(sha256sum "$binary" | awk '{print $1}')"
  jq -n --arg submission_commit "$commit" --arg binary_sha256 "$digest" \
    --arg source_sha256 "$source_sha" --arg go_version "$(go version)" \
    --arg build_command "$command" --arg source_files "$source_listing" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,
      source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,
      source_files:$source_files}' > "$manifest"
  chmod 600 "$manifest"
  printf '%s' "$digest"
}

adapter_binary="$outdir/final-v5-adapter"
observer_binary="$outdir/final-v5-observer"
observer_manifest="$outdir/final-v5-observer.build.json"
adapter_manifest="$outdir/final-v5-adapter.build.json"

build_sealed ./evaluation/cmd/final-v5-adapter "$adapter_binary" "$adapter_manifest" \
  "go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" >/dev/null
observer_digest="$(build_sealed ./evaluation/cmd/final-v5-observer "$observer_binary" "$observer_manifest" \
  "go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer")"

export TASKGATE_FINAL_V5_OBSERVER="$(realpath "$observer_binary")"
export TASKGATE_FINAL_V5_OBSERVER_SHA256="$observer_digest"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST="$(realpath "$observer_manifest")"
export TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST_SHA256="$(sha256sum "$observer_manifest" | awk '{print $1}')"

# The Adapter refuses a dataset binding whose file and section digests differ
# from the ones frozen before the run started, so they are established here from
# its own strict validation rather than asserted.
binding_validation="$("$adapter_binary" --validate-binding)"
jq -e '.schema_version == 1 and .status == "valid" and .artifact_cells == 6' <<< "$binding_validation" >/dev/null || {
  echo "strict dataset binding validation did not report six artifact cells" >&2; exit 1; }
export TASKGATE_FINAL_V5_BINDING_FILE_SHA256="$(jq -er .dataset_binding_sha256 <<< "$binding_validation")"
export TASKGATE_FINAL_V5_BINDING_SECTION_SHA256="$(jq -er .final_v5_adapter_sha256 <<< "$binding_validation")"
# artifact_profile.go binds every sample to the run's Dataset Binding by this
# digest; it is the same value, named for what the profile binding calls it.
export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$TASKGATE_FINAL_V5_BINDING_FILE_SHA256"

# ----------------------------------------------------------------- bring-up

# The Gateway must activate exactly the Profile Catalog this run is judged
# against. compose.yaml defaults TASKGATE_PROFILE_CATALOG to the master
# config/catalog.yaml, which is a different ExpectedSchema.
export TASKGATE_PROFILE_CATALOG="./${PROFILE_CATALOG#./}"

compose=(docker compose --project-name "$project"
  --file compose.yaml
  --file compose.debug.yaml
  --file evaluation/final-v5-wsl2/compose.real-pilot.yaml
  --file evaluation/final-v5-wsl2/compose.observer-v3.yaml)

cleanup() {
  local status=$?
  if [[ "$KEEP_UP" == "1" ]]; then
    echo "leaving $project up (KEEP_UP=1)"
  else
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

retain_failure() {
  "${compose[@]}" ps --all >>"$outdir/compose-up.log" 2>&1 || true
  "${compose[@]}" logs --no-color --tail 200 >"$outdir/compose-logs-failure.log" 2>&1 || true
}

echo "== ${RUN_ID}: fresh deployment ${project}"
"${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true

# Phase 1: everything except the Gateway. The snapshot-index services populate
# one shared artifact volume with every publication in the repository, and the
# Gateway fails closed on any publication its closure does not declare, so it
# cannot be started against that volume. Phase 2 gives it a per-profile
# directory instead.
phase1_services=(business-postgres control-postgres oa-demo
  result-object-store result-object-store-init
  snapshot-index-detail snapshot-index-summary snapshot-index-result-heavy
  snapshot-sidecar-install)
phase1_healthy=(business-postgres control-postgres oa-demo result-object-store)
phase1_jobs=(result-object-store-init snapshot-index-detail snapshot-index-summary
  snapshot-index-result-heavy snapshot-sidecar-install)

"${compose[@]}" up -d "${phase1_services[@]}" >"$outdir/compose-up.log" 2>&1 || {
  echo "phase 1 failed to start; see $outdir/compose-up.log" >&2; retain_failure; exit 1; }

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
      [[ "$code" == 0 ]] || { echo "$service exited $code" >&2; retain_failure; exit 1; }
      break
    fi
    [[ "$attempt" == 180 ]] && { echo "$service never completed" >&2; retain_failure; exit 1; }
    sleep 2
  done
done
echo "phase 1: all services healthy, all jobs completed"

# The per-profile artifact directory, materialized from the verified full one.
# It is what the deployment mounts AND what the finalizer reads its retained
# publication manifests from, so both sides are bound to one immutable artifact.
full_artifacts="$outdir/snapshot-index-artifacts-full"
profile_artifacts_root="$outdir/profile-artifacts"
mkdir -p "$full_artifacts" "$profile_artifacts_root"
artifact_volume="${project}_snapshot-index-artifacts"
docker run --rm \
  --volume "${artifact_volume}:/data:ro" \
  --volume "${repo}/${full_artifacts}:/out" \
  alpine:3.20 sh -c "cp -R /data/. /out/ && chown -R $(id -u):$(id -g) /out" \
  >>"$outdir/compose-up.log" 2>&1
copied="$(find "$full_artifacts" -type f | wc -l)"
[[ "$copied" -gt 0 ]] || { echo "the snapshot artifact volume copied no files" >&2; exit 1; }
echo "copied $copied artifact files from $artifact_volume"

GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-artifacts \
  --profile-id "$PROFILE_ID" \
  --source "$full_artifacts" \
  --destination "$profile_artifacts_root" \
  --manifest-out "$outdir/profile-artifact-manifest.json" | tee "$outdir/profile-artifacts.log"

profile_dir="$(python3 -c "
import json
m = json.load(open('$outdir/profile-artifact-manifest.json'))
p = m['profiles']['$PROFILE_ID']
print(p.get('directory') or p.get('Directory') or '')")"
[[ -n "$profile_dir" ]] || profile_dir="${profile_artifacts_root}/${PROFILE_ID}"
[[ -d "$profile_dir" ]] || { echo "materialized profile directory $profile_dir is absent" >&2; exit 1; }
export TASKGATE_PROFILE_ARTIFACT_DIR="$(cd "$profile_dir" && pwd)"
echo "profile artifact dir: $TASKGATE_PROFILE_ARTIFACT_DIR"

# Phase 2: the Gateway, against exactly its own closure.
"${compose[@]}" up -d --wait --wait-timeout 600 --no-deps gateway >>"$outdir/compose-up.log" 2>&1 || {
  echo "gateway failed to start; see $outdir/compose-up.log" >&2; retain_failure; exit 1; }

gateway_container="$("${compose[@]}" ps -q gateway)"
probe_command="$(docker inspect --format '{{json .Config.Healthcheck.Test}}' "$gateway_container")"
echo "$probe_command" | grep -q '/health/live' || {
  echo "the running Gateway does not carry the /health/live periodic probe: $probe_command" >&2
  echo "a probe that still reaches /health/ready performs a full Attestation on every interval" >&2
  exit 1
}

echo "== proving readiness explicitly (outside every measurement window)"
for attempt in $(seq 1 60); do
  curl --fail --silent --output /dev/null http://127.0.0.1:8082/health/ready && {
    echo "readiness: pass (attempt ${attempt})"; break; }
  [[ "$attempt" == 60 ]] && { echo "gateway never became ready" >&2; retain_failure; exit 1; }
  sleep 2
done

# ------------------------------------------- the server the footprint qualifies
#
# The finalizer reads the retained identity and then requires the observer's own
# in-window Docker Engine inspection to equal it. Comparing here as well is not
# redundant: it turns "the qualification does not apply to this server" from a
# per-sample finalization failure into one message before any cell runs.
pg_container="$("${compose[@]}" ps -q business-postgres)"
running_image_reference="$(docker inspect --format '{{.Config.Image}}' "$pg_container")"
running_identity="$(python3 - "$POSTGRESQL_IDENTITY" <<PYCHECK
import json, subprocess, sys

def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()

running = {
    "container_image_id": docker("inspect", "--format", "{{.Image}}", "$pg_container"),
    "image_reference": "$running_image_reference",
    "local_image_id": docker("image", "inspect", "--format", "{{.Id}}", "$running_image_reference"),
    "repo_digest": docker("image", "inspect", "--format", "{{index .RepoDigests 0}}", "$running_image_reference"),
    "platform": docker("image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", "$running_image_reference"),
}
retained = json.load(open(sys.argv[1]))
differences = [k for k, v in running.items() if retained.get(k) != v]
if differences:
    print("the retained PostgreSQL identity does not describe the running server; "
          "differs on: " + ", ".join(sorted(differences)), file=sys.stderr)
    sys.exit(1)
print(running["image_reference"])
PYCHECK
)" || exit 1
echo "postgresql runtime identity matches the retained qualification: $running_identity"

# ------------------------------------------------------- deployment bindings

compose_json="$("${compose[@]}" config --format json)"
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
control_port="$("${compose[@]}" port control-postgres 5432 | awk -F: 'END{print $NF}')"
business_port="$("${compose[@]}" port business-postgres 5432 | awk -F: 'END{print $NF}')"
object_port="$("${compose[@]}" port result-object-store 9000 | awk -F: 'END{print $NF}')"
for value in "$alice_token" "$carol_token" "$alice_password" "$bob_password" "$control_password" \
  "$control_database" "$business_password" "$business_database" "$business_admin_password" \
  "$object_access_key" "$object_secret_key" "$object_bucket" \
  "$control_port" "$business_port" "$object_port"; do
  [[ -n "$value" ]] || { echo "Compose omitted a required deployment binding" >&2; exit 1; }
done

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
unset alice_token carol_token alice_password bob_password control_password \
  business_password business_admin_password object_access_key object_secret_key

# ------------------------------------------------ the v3 finalizer's material
#
# These are the sources OpenDeploymentFinalizerV3 reads. It takes a context and
# nothing else, so a caller cannot point it at material of its choosing; the
# deployment describes itself here instead.
export TASKGATE_FINAL_V5_CATALOG="$repo/${PROFILE_CATALOG#./}"
export TASKGATE_FINAL_V5_PROFILE_REGISTRY="$repo/${PROFILE_REGISTRY#./}"
export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$(realpath "$ATTESTATION_QUALIFICATION")"
export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$(realpath "$POSTGRESQL_IDENTITY")"
export TASKGATE_FINAL_V5_REPO_ROOT="$repo"
# TASKGATE_PROFILE_ARTIFACT_DIR is exported above, where it is materialized:
# the deployment mounts it and the finalizer reads it, and they must be one path.

# ------------------------------------------------------------------- the run

config="$outdir/config.json"
jq --arg campaign "$RUN_ID" --arg commit "$commit" \
   --argjson samples "$SAMPLES" --argjson warmups "$WARMUPS" '
  .campaign_class = "pilot" | .pilot_kind = "artifact_targeted" |
  .campaign_id = $campaign | .submission_commit = $commit |
  .deployments = 1 | .samples = $samples | .warmups = $warmups
' evaluation/final-v5-wsl2/config/artifact.example.json > "$config"
chmod 600 "$config"

echo "== running the six frozen artifact/result-heavy cells through v3 acceptance"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/v5-artifact \
  -config "$config" \
  -deployment-id deployment-01 \
  -adapter "$(realpath "$adapter_binary")" \
  -output "$outdir/raw/deployment-01.jsonl" | tee "$outdir/run.log"

# Every retained sample must carry the finalizer's own acceptance record. A
# sample without one was never adjudicated, whatever its status says.
jq -e -s '
  length > 0 and
  (map(select(.system == "taskgate")) | length > 0) and
  (map(select(.system == "taskgate")) | all(.taskgate_acceptance_v3 != null))
' "$outdir/raw/deployment-01.jsonl" >/dev/null || {
  echo "a retained TaskGate sample carries no v3 acceptance record" >&2
  exit 1
}
passed="$(jq -s '[.[] | select(.status == "pass")] | length' "$outdir/raw/deployment-01.jsonl")"
total="$(jq -s 'length' "$outdir/raw/deployment-01.jsonl")"
echo "== ${RUN_ID} complete: ${passed}/${total} samples passed"
echo "   evidence: $outdir"
