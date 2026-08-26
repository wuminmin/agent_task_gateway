package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const artifactTargetedLauncherPath = "../../final-v5-wsl2/scripts/run-artifact-targeted.sh"

func artifactTargetedLauncherBody(t *testing.T) string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Clean(artifactTargetedLauncherPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func launcherShellBlock(t *testing.T, body, name string) string {
	t.Helper()
	begin := "# " + name + "_BEGIN"
	end := "# " + name + "_END"
	start := strings.Index(body, begin)
	finish := strings.Index(body, end)
	if start < 0 || finish < 0 || finish <= start {
		t.Fatalf("targeted Artifact launcher has no complete %s block", name)
	}
	return body[start : finish+len(end)]
}

func runLauncherShellBlock(block, invocation string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-c", block + "\n" + invocation, "launcher-contract-test"}, args...)
	return exec.Command("bash", commandArgs...).CombinedOutput()
}

func artifactTargetedBuildSealedFunction(t *testing.T, body string) string {
	t.Helper()
	return launcherShellBlock(t, body, "SOURCE_BUILD_MANIFEST")
}

func TestArtifactTargetedLauncherWiresTheFormalRuntimeContract(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	for _, required := range []string{
		`set -euo pipefail`,
		`export TASKGATE_EXPERIMENT_CLASS=pilot`,
		`export TASKGATE_CAMPAIGN_ID="$RUN_ID"`,
		`printf '%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$commit"`,
		`deployment-project-name.sh`,
		`export COMPOSE_PROJECT_NAME="$project"`,
		`evaluation/final-v5-wsl2/compose.provsql.yaml`,
		`snapshot-sidecar-install final-v5-direct-postgres final-v5-provsql-postgres`,
		`final-v5-direct-postgres final-v5-provsql-postgres)`,
		`go run ./evaluation/cmd/final-v5-gateway-build build`,
		`formal_gateway_tag="taskgate-final-v5-gateway:${commit}"`,
		`image: "${formal_gateway_tag}"`,
		`--no-build --no-deps gateway`,
		`go run ./evaluation/cmd/final-v5-artifact-targeted-binding`,
		`--registry "$PROFILE_REGISTRY"`,
		`--profile-alias "$PROFILE_ALIAS"`,
		`--catalog "$PROFILE_CATALOG"`,
		`--selected-scales "$selected_scales_csv"`,
		`--attestation-qualification "$ATTESTATION_QUALIFICATION"`,
		`--postgresql-identity "$POSTGRESQL_IDENTITY"`,
		`--out "$artifact_targeted_binding"`,
		`artifact_targeted_binding_sha256="$(sha256sum "$artifact_targeted_binding" | awk '{print $1}')"`,
		`export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$artifact_targeted_binding_sha256"`,
		`go run ./evaluation/cmd/final-v5-profile-binding`,
		`--dataset-binding-sha256 "$TASKGATE_FINAL_V5_DATASET_BINDING_SHA256"`,
		`-profile-binding "$(realpath "$profile_binding")"`,
		`artifact_targeted_binding_path=${artifact_targeted_binding_path}`,
		`artifact_targeted_binding_sha256=${artifact_targeted_binding_sha256}`,
		`claim_scope=artifact_path_and_v3_observer_acceptance_only`,
		`publication_factset_oracle_ready=false`,
		`if [[ -z "${SCALES+x}" ]]`,
		`selected_scales_json="$(resolve_artifact_scales "$SCALES")"`,
		`--argjson scales "$selected_scales_json"`,
		`.workloads[0].id != "result-heavy"`,
		`.workloads[0].modes != ["novel"]`,
		`.workloads[0].scales != $frozen_scales`,
		`.workloads[0].scales = $scales`,
		`.artifact_cells == 6 and`,
		`.selected_cells == $selected_cells and`,
		`.binding_file_sha256 == $binding_file_sha256`,
		`expected=$((selected_scale_count * SAMPLES))`,
		`export TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT="$project"`,
		`export TASKGATE_FINAL_V5_FORMAL_WINDOW_GATEWAY="http://127.0.0.1:8082"`,
		`go test -count=1 -json`,
		`if ! require_formal_window_gate_passes`,
		`TestFormalDeploymentRunsTheApprovedHealthcheckLive`,
		`TestPeriodicLivenessProbesAddNoBusinessStatements`,
		`TestExplicitReadinessOutsideTheWindowStillAttests`,
		`.experiment_id == "artifact"`,
		`.workload_id == "result-heavy"`,
		`(.scale as $scale | ($scales | index($scale)) != null)`,
		`([$records[] | select(.scale == $scale)] | length) == $samples`,
		`.status == "pass"`,
		`.system == "taskgate"`,
		`.taskgate_acceptance_v3 != null`,
		`taskgate_acceptance_v3_present:(.taskgate_acceptance_v3 != null)`,
		`capture_artifact_runner_status "$outdir/run.log"`,
		`-adapter-stderr-output "$outdir/adapter-stderr.log"`,
		`.publication_eligible == false`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("targeted Artifact launcher omits runtime contract %q", required)
		}
	}
	if strings.Contains(body, `SCALES="${SCALES:-`) {
		t.Fatal("targeted Artifact launcher silently defaults an explicitly empty SCALES selection")
	}
	for _, forbidden := range []string{
		`--validate-binding`,
		`TASKGATE_DATASET_BINDINGS`,
		`TASKGATE_FINAL_V5_BINDING_FILE_SHA256`,
		`TASKGATE_FINAL_V5_BINDING_SECTION_SHA256`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("targeted Artifact launcher still reads publication-wide private binding material %q", forbidden)
		}
	}

	clearance := strings.Index(body, `# ------------------------------------------------- the clearance, checked first`)
	if clearance < 0 {
		t.Fatal("targeted Artifact launcher has no explicit early-clearance boundary")
	}
	for _, sideEffect := range []string{
		`mkdir -m 700 -p "$outdir"`,
		`go run ./evaluation/cmd/final-v5-gateway-build build`,
		`build_sealed ./evaluation/cmd/final-v5-adapter`,
		`"${compose[@]}" up`,
	} {
		position := strings.Index(body, sideEffect)
		if position < 0 {
			t.Fatalf("targeted Artifact launcher omits %q", sideEffect)
		}
		if position < clearance {
			t.Fatalf("targeted Artifact launcher performs %q before profile clearance", sideEffect)
		}
	}

	gatewayStart := strings.Index(body, `--no-build --no-deps gateway`)
	runnerStart := strings.Index(body, `go run ./evaluation/cmd/v5-artifact`)
	for name, position := range map[string]int{
		"experiment class": strings.Index(body, `export TASKGATE_EXPERIMENT_CLASS=pilot`),
		"campaign ID":      strings.Index(body, `export TASKGATE_CAMPAIGN_ID="$RUN_ID"`),
		"Compose project":  strings.Index(body, `export COMPOSE_PROJECT_NAME="$project"`),
		"profile binding":  strings.Index(body, `go run ./evaluation/cmd/final-v5-profile-binding`),
		"formal image":     strings.Index(body, `go run ./evaluation/cmd/final-v5-gateway-build build`),
	} {
		if position < clearance || position > gatewayStart || position > runnerStart {
			t.Fatalf("%s is not bound after clearance and before Gateway/runner startup", name)
		}
	}

	phaseOneReady := strings.Index(body, `echo "phase 1: all services healthy, all jobs completed"`)
	businessDSN := strings.Index(body, `export TASKGATE_FINAL_V5_BUSINESS_DSN=`)
	targetedBinding := strings.Index(body, `go run ./evaluation/cmd/final-v5-artifact-targeted-binding`)
	targetedDigest := strings.Index(body, `artifact_targeted_binding_sha256="$(sha256sum`)
	datasetDigestExport := strings.Index(body,
		`export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$artifact_targeted_binding_sha256"`)
	profileBinding := strings.Index(body, `go run ./evaluation/cmd/final-v5-profile-binding`)
	marker := strings.Index(body, `artifact_targeted_binding_path=${artifact_targeted_binding_path}`)
	if phaseOneReady < 0 || businessDSN <= phaseOneReady || targetedBinding <= businessDSN ||
		targetedDigest <= targetedBinding || datasetDigestExport <= targetedDigest ||
		profileBinding <= datasetDigestExport || marker <= profileBinding ||
		gatewayStart <= marker || runnerStart <= gatewayStart {
		t.Fatal("fresh DB, targeted binding, exact digest, profile binding and marker are not ordered before Gateway/measurement startup")
	}

	scaleResolution := strings.Index(body, `selected_scales_json="$(resolve_artifact_scales "$SCALES")"`)
	firstSideEffect := strings.Index(body, `mkdir -m 700 -p "$outdir"`)
	if scaleResolution < 0 || firstSideEffect < 0 || scaleResolution > firstSideEffect {
		t.Fatal("scale selection is not rejected before the launcher creates run state")
	}
	for _, sideEffect := range []string{
		`mkdir -m 700 -p "$outdir"`,
		`go run ./evaluation/cmd/final-v5-gateway-build build`,
		`build_sealed ./evaluation/cmd/final-v5-adapter`,
		`"${compose[@]}" up`,
	} {
		if position := strings.Index(body, sideEffect); position < scaleResolution {
			t.Fatalf("targeted Artifact launcher performs %q before scale selection", sideEffect)
		}
	}
	readiness := strings.Index(body, `echo "== proving readiness explicitly (outside every measurement window)"`)
	formalGate := strings.Index(body, `# FORMAL_WINDOW_LIVE_GATE_RUN_BEGIN`)
	formalGateEnd := strings.Index(body, `# FORMAL_WINDOW_LIVE_GATE_RUN_END`)
	readinessLoopEnd := -1
	if readiness >= 0 {
		if relative := strings.Index(body[readiness:], "\ndone\n"); relative >= 0 {
			readinessLoopEnd = readiness + relative + len("\ndone\n")
		}
	}
	if readiness < 0 || readinessLoopEnd < 0 || formalGate < readinessLoopEnd ||
		formalGateEnd < formalGate || formalGateEnd > runnerStart {
		t.Fatal("formal-window live gates are not between explicit readiness and the measurement runner")
	}
	gateBlock := body[formalGate:formalGateEnd]
	if !strings.Contains(gateBlock, `unset TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT`) {
		t.Fatal("formal-window live gate environment is not cleared after adjudication")
	}
	if strings.Contains(gateBlock, `|| true`) {
		t.Fatal("formal-window live gate block contains a fail-open command")
	}
	goTest := strings.Index(gateBlock, `go test -count=1 -json`)
	adjudication := strings.Index(gateBlock, `if ! require_formal_window_gate_passes`)
	projectExport := strings.Index(gateBlock, `export TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT="$project"`)
	gatewayExport := strings.Index(gateBlock, `export TASKGATE_FINAL_V5_FORMAL_WINDOW_GATEWAY="http://127.0.0.1:8082"`)
	measurementRefusal := -1
	if adjudication >= 0 {
		measurementRefusal = strings.Index(gateBlock[adjudication:], `exit 1`)
	}
	if goTest < 0 || projectExport < 0 || projectExport > goTest ||
		gatewayExport < 0 || gatewayExport > goTest ||
		adjudication < goTest || measurementRefusal < 0 {
		t.Fatal("formal-window live gate report is not fail-closed after the live go test")
	}

	bindingValidation := strings.Index(body, `artifact_targeted_binding_validation="$(`)
	bindingExport := strings.Index(body,
		`export TASKGATE_FINAL_V5_DATASET_BINDING_SHA256="$artifact_targeted_binding_sha256"`)
	if bindingValidation < 0 || bindingExport < bindingValidation {
		t.Fatal("targeted Artifact launcher has no bounded Artifact-targeted binding validation stage")
	}
	bindingBlock := body[bindingValidation:bindingExport]
	for _, required := range []string{
		`.schema_version == 2`,
		`.status == "valid"`,
		`.artifact_cells == 6`,
		`.selected_cells == $selected_cells`,
		`(.dataset_sha256 | test("^[0-9a-f]{64}$"))`,
		`(.dataset_probe_sql_sha256 | test("^[0-9a-f]{64}$"))`,
		`(.dataset_probe_sha256 | test("^[0-9a-f]{64}$"))`,
		`.binding_file_sha256 == $binding_file_sha256`,
	} {
		if !strings.Contains(bindingBlock, required) {
			t.Fatalf("Artifact-targeted binding validation omits %q", required)
		}
	}
	finalAdjudication := strings.Index(body[runnerStart:], `# A process-level zero exit retains failed measured samples`)
	if finalAdjudication < 0 {
		t.Fatal("the post-run adjudicator is absent")
	}
	adjudicator := body[runnerStart+finalAdjudication:]
	for _, required := range []string{
		".taskgate_acceptance_v3 != null and\n    .taskgate_rejection_v1 == null and",
		`report_retained_artifact_rejections "$outdir/raw/deployment-01.jsonl"`,
	} {
		if !strings.Contains(adjudicator, required) {
			t.Fatalf("the post-run adjudicator omits %q", required)
		}
	}
}

func TestArtifactTargetedRunnerFailureStillReachesAdjudication(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "ARTIFACT_RUNNER_STATUS")
	runLog := filepath.Join(t.TempDir(), "run.log")
	output, err := runLauncherShellBlock(block, `
set -euo pipefail
artifact_run_log="$1"
capture_artifact_runner_status "$artifact_run_log" bash -c 'printf "retained runner output\n"; printf "retained runner error\n" >&2; exit 23'
printf 'runner=%s tee=%s\n' "$artifact_runner_status" "$artifact_tee_status"
`, runLog)
	if err != nil {
		t.Fatalf("captured nonzero runner pipeline exited before adjudication: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("runner=23 tee=0")) {
		t.Fatalf("captured pipeline statuses = %q", output)
	}
	retained, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained) != "retained runner output\nretained runner error\n" {
		t.Fatalf("tee retained %q", retained)
	}
}

func TestArtifactTargetedBuildManifestStreamsSourceListingInsteadOfUsingArgv(t *testing.T) {
	block := artifactTargetedBuildSealedFunction(t, artifactTargetedLauncherBody(t))
	for _, forbidden := range []string{
		`--arg source_files "$source_listing"`,
		`--arg source_files`,
		`source_files:$source_files`,
	} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("build_sealed still puts source_listing in jq argv via %q", forbidden)
		}
	}
	for _, required := range []string{
		`printf '%s' "$source_listing"`,
		`jq -Rs`,
		`source_files:.`,
	} {
		if !strings.Contains(block, required) {
			t.Fatalf("streaming build manifest helper omits %q", required)
		}
	}
	printfPosition := strings.Index(block, `printf '%s' "$source_listing"`)
	jqPosition := strings.Index(block, `jq -Rs`)
	sourceFilesPosition := strings.Index(block, `source_files:.`)
	if printfPosition < 0 || jqPosition <= printfPosition || sourceFilesPosition <= jqPosition ||
		!strings.Contains(block[printfPosition:jqPosition], "|") {
		t.Fatal("source listing is not piped through jq raw-input mode into source_files")
	}
}

func artifactTargetedScriptSHA256(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
