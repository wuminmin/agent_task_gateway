#!/usr/bin/env bash
# Targeted validation of an explicit subset of the six frozen
# artifact/result-heavy cells, through v3 acceptance, against a fresh isolated
# full topology. SCALES defaults to all six so this targeted launcher's existing
# six-cell behavior is unchanged; a canary must name its narrower selection
# explicitly.
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

# ARTIFACT_SCALE_SELECTION_BEGIN
readonly -a frozen_artifact_scales=(
  100x4 10k-x4 100k-x4 100x16 10k-x16 100k-x16
)

artifact_default_scales() {
  local IFS=,
  printf '%s' "${frozen_artifact_scales[*]}"
}

resolve_artifact_scales() {
  local selection="$1"
  local scale_alternation scale_list_pattern scale
  local -a requested_scales=() selected_scales=()
  local -A requested_scale_set=()

  scale_alternation="$(IFS='|'; printf '%s' "${frozen_artifact_scales[*]}")"
  scale_list_pattern="^(${scale_alternation})(,(${scale_alternation}))*$"
  if ! [[ "$selection" =~ $scale_list_pattern ]]; then
    echo "SCALES must be a comma-separated subset of: $(artifact_default_scales)" >&2
    return 2
  fi
  IFS=, read -r -a requested_scales <<< "$selection"
  for scale in "${requested_scales[@]}"; do
    if [[ -n "${requested_scale_set[$scale]+present}" ]]; then
      echo "SCALES repeats $scale" >&2
      return 2
    fi
    requested_scale_set["$scale"]=1
  done
  for scale in "${frozen_artifact_scales[@]}"; do
    [[ -n "${requested_scale_set[$scale]+present}" ]] && selected_scales+=("$scale")
  done
  jq -cn --args '$ARGS.positional' "${selected_scales[@]}"
}
# ARTIFACT_SCALE_SELECTION_END

# FORMAL_WINDOW_GATE_ADJUDICATION_BEGIN
require_formal_window_gate_passes() {
  local report="$1" expected_tests_json="$2"
  jq -e -s --argjson expected "$expected_tests_json" '
    . as $events |
    def terminal: .Action == "pass" or .Action == "fail" or .Action == "skip";
    def terminals($name):
      [$events[] | select(.Test == $name and terminal) | .Action];
    [$events[] | select(.Test != null and terminal)] as $test_terminals |
    ($expected | length) == 3 and
    ($expected | unique | length) == 3 and
    all($events[]; .Action != "fail" and .Action != "skip") and
    ($test_terminals | length) == 3 and
    all($test_terminals[]; .Test as $name | ($expected | index($name)) != null) and
    all($expected[]; . as $name | terminals($name) == ["pass"])
  ' "$report" >/dev/null
}
# FORMAL_WINDOW_GATE_ADJUDICATION_END

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
[[ "$RUN_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || {
  echo "RUN_ID must be a path-safe campaign identity" >&2; exit 2; }
[[ "$SAMPLES" =~ ^[1-3]$ ]] || {
  echo "SAMPLES must be an integer from 1 through 3 for a pilot" >&2; exit 2; }
[[ "$WARMUPS" =~ ^[0-9]+$ ]] || {
  echo "WARMUPS must be a non-negative integer" >&2; exit 2; }
[[ "$KEEP_UP" == 0 || "$KEEP_UP" == 1 ]] || {
  echo "KEEP_UP must be 0 or 1" >&2; exit 2; }

repo="$(git rev-parse --show-toplevel)"
cd "$repo"

artifact_config="evaluation/final-v5-wsl2/config/artifact.example.json"
if [[ -z "${SCALES+x}" ]]; then
  SCALES="$(artifact_default_scales)"
fi
if ! selected_scales_json="$(resolve_artifact_scales "$SCALES")"; then
  exit 2
fi
selected_scale_count="$(jq -er 'length' <<< "$selected_scales_json")"
selected_scales_csv="$(jq -er 'join(",")' <<< "$selected_scales_json")"
frozen_scales_json="$(jq -cn --args '$ARGS.positional' "${frozen_artifact_scales[@]}")"

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
outdir="evaluation/final-v5-wsl2/raw/targeted-${run_name}"
[[ -e "$outdir" ]] && { echo "refusing to overwrite $outdir" >&2; exit 2; }

for input in "$ATTESTATION_QUALIFICATION" "$POSTGRESQL_IDENTITY" "$PROFILE_CATALOG" "$PROFILE_REGISTRY"; do
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

# The operator-facing alias, the profile ID used to materialize artifacts and
# the Catalog bytes mounted into the Gateway must name one registry entry. Do
# this before the clearance gate and before creating the run directory: a stale
# hard-coded ID or a changed Catalog is a bad invocation, not run evidence.
profile_catalog_sha256="$(sha256sum "$PROFILE_CATALOG" | awk '{print $1}')"
jq -e --arg alias "$PROFILE_ALIAS" --arg profile_id "$PROFILE_ID" \
  --arg catalog_sha256 "$profile_catalog_sha256" '
  [.profiles[] | select(.alias == $alias)] as $matches |
  ($matches | length) == 1 and
  $matches[0].profile_id == $profile_id and
  $matches[0].catalog_sha256 == $catalog_sha256
' "$PROFILE_REGISTRY" >/dev/null || {
  echo "profile alias, ID, registry Catalog digest and $PROFILE_CATALOG bytes do not agree" >&2
  exit 2
}

# ------------------------------------------------- the clearance, checked first
#
# Refused here rather than inside the adapter. Both the profile resolver and the
# Adapter's own profile binding apply this gate, so a run that fails it fails
# anyway -- but only after a full topology has been built and every selected
# cell attempted, which reads as a measurement failure rather than as a missing
# prerequisite.
jq -e --arg alias "$PROFILE_ALIAS" '
  .profiles | map(select(.alias == $alias)) | length == 1 and
  (.[0].targeted_run_eligible == true and
   .[0].status.activation_supported == true and
   .[0].status.activation_smoke_passed == true)
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

# Runner identity and observer ownership are deployment inputs. Export them
# before Compose resolves the Gateway environment and retain exactly the same
# values for the runner. First bind the campaign to this commit, then let the
# shared helper bind that identity to the deployment in the observer's formal
# project namespace. A retry at another commit can never alias this project.
deployment_id=deployment-01
export TASKGATE_EXPERIMENT_CLASS=pilot
export TASKGATE_CAMPAIGN_ID="$RUN_ID"
project_campaign_identity="$(printf '%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$commit" | sha256sum | awk '{print $1}')"
project="$(bash evaluation/final-v5-wsl2/scripts/deployment-project-name.sh \
  "$project_campaign_identity" "$deployment_id")"
export COMPOSE_PROJECT_NAME="$project"

mkdir -m 700 -p "$outdir" "$outdir/raw" "$outdir/environment"

# ------------------------------------------- the adapter and the observer

# Both are built from this checkout with a sealed build manifest, because the
# adapter verifies the observer's manifest against the submission commit before
# it will run it. The listing is over tracked files so the manifest describes the
# commit rather than the working directory.
source_listing="$(git ls-files | sort | while IFS= read -r file; do
  printf '%s  %s\n' "$(sha256sum "$file" | awk '{print $1}')" "$file"
done)"
source_sha="$(printf '%s' "$source_listing" | sha256sum | awk '{print $1}')"

# SOURCE_BUILD_MANIFEST_BEGIN
build_sealed() { # target, out-binary, out-manifest, build-command
  local target="$1" binary="$2" manifest="$3" command="$4"
  GOFLAGS=-buildvcs=false go build -buildvcs=false -trimpath -o "$binary" "$target"
  chmod 700 "$binary"
  local digest
  digest="$(sha256sum "$binary" | awk '{print $1}')"
  printf '%s' "$source_listing" | jq -Rs \
    --arg submission_commit "$commit" --arg binary_sha256 "$digest" \
    --arg source_sha256 "$source_sha" --arg go_version "$(go version)" \
    --arg build_command "$command" \
    '{schema_version:1,submission_commit:$submission_commit,binary_sha256:$binary_sha256,
      source_sha256:$source_sha256,go_version:$go_version,build_command:$build_command,
      source_files:.}' > "$manifest"
  chmod 600 "$manifest"
  printf '%s' "$digest"
}
# SOURCE_BUILD_MANIFEST_END

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

# The observer accepts only a Gateway built by the tracked-file-only formal
# build path. Select that verified local image through a last-wins Compose
# override; --no-build at Gateway startup prevents Compose from replacing it
# with the ordinary Dockerfile build declared by compose.yaml. This expensive
# build is source-bound to the clean submission commit; the live targeted
# binding is created later, only after fresh Business PostgreSQL is ready.
formal_gateway_tag="taskgate-final-v5-gateway:${commit}"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-gateway-build build \
  -root "$repo" \
  -tag "$formal_gateway_tag" | tee "$outdir/formal-gateway-build.log"
formal_gateway_override="$outdir/compose.formal-gateway.yaml"
cat >"$formal_gateway_override" <<COMPOSE_OVERRIDE
services:
  gateway:
    image: "${formal_gateway_tag}"
    pull_policy: never
COMPOSE_OVERRIDE
chmod 600 "$formal_gateway_override"

# ----------------------------------------------------------------- bring-up

# The Gateway must activate exactly the Profile Catalog this run is judged
# against. compose.yaml defaults TASKGATE_PROFILE_CATALOG to the master
# config/catalog.yaml, which is a different ExpectedSchema.
export TASKGATE_PROFILE_CATALOG="./${PROFILE_CATALOG#./}"

compose=(docker compose --project-name "$project"
  --file compose.yaml
  --file compose.debug.yaml
  --file evaluation/final-v5-wsl2/compose.real-pilot.yaml
  --file evaluation/final-v5-wsl2/compose.provsql.yaml
  --file evaluation/final-v5-wsl2/compose.observer-v3.yaml
  --file "$formal_gateway_override")

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
  snapshot-sidecar-install final-v5-direct-postgres final-v5-provsql-postgres)
phase1_healthy=(business-postgres control-postgres oa-demo result-object-store
  final-v5-direct-postgres final-v5-provsql-postgres)
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

# ------------------------------------------------------- deployment bindings
#
# Read the fresh deployment's credentials only after its backing services are
# healthy. The Artifact-targeted binding command receives the Business DSN
# through its environment, probes that live database once, and writes no
# credential into the retained binding.
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

# ------------------------------------ Artifact-targeted deployment binding
#
# This credential-free record binds only the six frozen Artifact cells and the
# selected subset to the fresh deployment. It is not the publication-wide
# private Scale/ProvSQL binding and is never exposed through those private
# binding environment variables.
artifact_targeted_binding="$outdir/artifact-targeted-deployment-binding.json"
artifact_targeted_binding_report="$outdir/artifact-targeted-deployment-binding.validation.json"
artifact_targeted_binding_validation="$(
  GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-artifact-targeted-binding \
    --registry "$PROFILE_REGISTRY" \
    --profile-alias "$PROFILE_ALIAS" \
    --catalog "$PROFILE_CATALOG" \
    --selected-scales "$selected_scales_csv" \
    --attestation-qualification "$ATTESTATION_QUALIFICATION" \
    --postgresql-identity "$POSTGRESQL_IDENTITY" \
    --out "$artifact_targeted_binding"
)"
printf '%s\n' "$artifact_targeted_binding_validation" >"$artifact_targeted_binding_report"
chmod 600 "$artifact_targeted_binding_report"

[[ -f "$artifact_targeted_binding" && ! -L "$artifact_targeted_binding" ]] || {
  echo "Artifact-targeted binding generator did not create a safe regular file" >&2; exit 1; }
[[ "$(stat -c '%a' "$artifact_targeted_binding")" == "600" ]] || {
  echo "Artifact-targeted binding generator did not create a mode-0600 file" >&2; exit 1; }
artifact_targeted_binding_sha256="$(sha256sum "$artifact_targeted_binding" | awk '{print $1}')"
jq -e --arg binding_file_sha256 "$artifact_targeted_binding_sha256" \
  --argjson selected_cells "$selected_scale_count" '
  .schema_version == 1 and
  .status == "valid" and
  .artifact_cells == 6 and
  .selected_cells == $selected_cells and
  (.dataset_probe_sha256 | test("^[0-9a-f]{64}$")) and
  .binding_file_sha256 == $binding_file_sha256
' <<< "$artifact_targeted_binding_validation" >/dev/null || {
  echo "Artifact-targeted binding validation report is incomplete or disagrees with the retained file" >&2
  exit 1
}
export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$artifact_targeted_binding_sha256"

# Resolve the orchestrator-owned profile binding through the same Go
# implementation the Adapter uses, keyed by the exact targeted binding bytes.
profile_binding="$outdir/profile-binding.json"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/final-v5-profile-binding \
  --registry "$PROFILE_REGISTRY" \
  --alias "$PROFILE_ALIAS" \
  --dataset-binding-sha256 "$TASKGATE_FINAL_V5_DATASET_BINDING_SHA256" \
  --out "$profile_binding"
[[ -f "$profile_binding" && ! -L "$profile_binding" ]] || {
  echo "profile binding resolver did not create a safe regular file" >&2; exit 1; }
chmod 600 "$profile_binding"
jq -e --arg profile_id "$PROFILE_ID" --arg catalog_sha256 "$profile_catalog_sha256" \
  --arg dataset_binding_sha256 "$TASKGATE_FINAL_V5_DATASET_BINDING_SHA256" '
  .version == "taskgate-final-v5-profile-binding-v1" and
  .profile_id == $profile_id and
  .catalog_sha256 == $catalog_sha256 and
  .dataset_binding_sha256 == $dataset_binding_sha256
' "$profile_binding" >/dev/null || {
  echo "resolved ProfileBinding differs from the selected profile, Catalog or targeted binding" >&2
  exit 1
}

artifact_targeted_binding_path="$(realpath "$artifact_targeted_binding")"
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
scales=${selected_scales_csv}
attestation_qualification=${ATTESTATION_QUALIFICATION}
postgresql_identity=${POSTGRESQL_IDENTITY}
artifact_targeted_binding_path=${artifact_targeted_binding_path}
artifact_targeted_binding_sha256=${artifact_targeted_binding_sha256}
claim_scope=artifact_path_and_v3_observer_acceptance_only
publication_factset_oracle_ready=false
MARKER
chmod 600 "$outdir/TARGETED-NOT-FOR-PUBLICATION"

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
"${compose[@]}" up -d --wait --wait-timeout 600 --no-build --no-deps gateway >>"$outdir/compose-up.log" 2>&1 || {
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

# FORMAL_WINDOW_LIVE_GATE_RUN_BEGIN
# A green go test process is insufficient here because Go reports an all-SKIP
# selection as success. Retain the JSON event stream, then require exactly one
# PASS terminal for each due live gate and no other test terminal before the
# first measured operation is allowed to start.
formal_window_gate_tests_json='[
  "TestFormalDeploymentRunsTheApprovedHealthcheckLive",
  "TestPeriodicLivenessProbesAddNoBusinessStatements",
  "TestExplicitReadinessOutsideTheWindowStillAttests"
]'
formal_window_gate_regex="$(jq -er '"^(" + join("|") + ")$"' <<< "$formal_window_gate_tests_json")"
formal_window_gate_log="$outdir/formal-window-live-gates.jsonl"
export TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT="$project"
export TASKGATE_FINAL_V5_FORMAL_WINDOW_GATEWAY="http://127.0.0.1:8082"
echo "== proving all formal-window live gates before any measurement"
if ! GOFLAGS=-buildvcs=false go test -count=1 -json \
    -run "$formal_window_gate_regex" ./evaluation/internal/experiment \
    | tee "$formal_window_gate_log"; then
  echo "formal-window live gates failed; no measurement was started" >&2
  retain_failure
  exit 1
fi
if ! require_formal_window_gate_passes \
    "$formal_window_gate_log" "$formal_window_gate_tests_json"; then
  echo "formal-window gate report did not record exactly three PASS terminals (FAIL/SKIP/missing refused)" >&2
  retain_failure
  exit 1
fi
unset TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT TASKGATE_FINAL_V5_FORMAL_WINDOW_GATEWAY
echo "formal-window live gates: 3/3 pass, 0 skip"
# FORMAL_WINDOW_LIVE_GATE_RUN_END

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
   --argjson samples "$SAMPLES" --argjson warmups "$WARMUPS" \
   --argjson scales "$selected_scales_json" \
   --argjson frozen_scales "$frozen_scales_json" '
  if ((.workloads | length) != 1 or
      .workloads[0].id != "result-heavy" or
      .workloads[0].modes != ["novel"] or
      .workloads[0].scales != $frozen_scales)
  then error("artifact example no longer carries the exact six frozen result-heavy/novel cells")
  else
    .campaign_class = "pilot" | .pilot_kind = "artifact_targeted" |
    .campaign_id = $campaign | .submission_commit = $commit |
    .deployments = 1 | .samples = $samples | .warmups = $warmups |
    .workloads[0].scales = $scales
  end
' "$artifact_config" > "$config"
chmod 600 "$config"

echo "== running ${selected_scale_count}/6 frozen artifact/result-heavy cells (${selected_scales_csv}) through v3 acceptance"
GOFLAGS=-buildvcs=false go run ./evaluation/cmd/v5-artifact \
  -config "$config" \
  -deployment-id "$deployment_id" \
  -adapter "$(realpath "$adapter_binary")" \
  -profile-binding "$(realpath "$profile_binding")" \
  -output "$outdir/raw/deployment-01.jsonl" | tee "$outdir/run.log"

# A process-level zero exit retains failed measured samples by design, so the
# targeted launcher must adjudicate the complete selected result set itself.
# Every selected scale is requested once per sample replicate, and every
# retained record must be a non-publication TaskGate PASS carrying the
# finalizer's acceptance.
expected=$((selected_scale_count * SAMPLES))
passed="$(jq -s '[.[] | select(.status == "pass")] | length' "$outdir/raw/deployment-01.jsonl")"
total="$(jq -s 'length' "$outdir/raw/deployment-01.jsonl")"
[[ "$total" -eq "$expected" ]] || {
  echo "retained $total measured samples, expected $expected" >&2
  exit 1
}
[[ "$passed" -eq "$expected" ]] || {
  echo "artifact targeted run failed: only $passed/$expected samples passed" >&2
  exit 1
}
jq -e -s --argjson expected "$expected" --argjson samples "$SAMPLES" \
  --argjson scales "$selected_scales_json" '
  . as $records |
  ($records | length) == $expected and
  all($records[];
    .experiment_id == "artifact" and
    .workload_id == "result-heavy" and
    .mode == "novel" and
    (.scale as $scale | ($scales | index($scale)) != null) and
    .status == "pass" and
    .system == "taskgate" and
    .taskgate_acceptance_v3 != null and
    .publication_eligible == false) and
  all($scales[]; . as $scale |
    ([$records[] | select(.scale == $scale)] | length) == $samples)
' "$outdir/raw/deployment-01.jsonl" >/dev/null || {
  echo "a retained sample is not an accepted, non-publication TaskGate PASS" >&2
  exit 1
}
echo "== ${RUN_ID} complete: ${passed}/${total} samples passed"
echo "   evidence: $outdir"
