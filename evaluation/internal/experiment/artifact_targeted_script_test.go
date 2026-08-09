package experiment

import (
	"encoding/json"
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

func TestArtifactTargetedLauncherWiresTheFormalRuntimeContract(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	for _, required := range []string{
		`set -euo pipefail`,
		`export TASKGATE_EXPERIMENT_CLASS=pilot`,
		`export TASKGATE_CAMPAIGN_ID="$RUN_ID"`,
		`printf '%s\0%s' "$TASKGATE_CAMPAIGN_ID" "$commit"`,
		`deployment-project-name.sh`,
		`export COMPOSE_PROJECT_NAME="$project"`,
		`--file evaluation/final-v5-wsl2/compose.provsql.yaml`,
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
		`.schema_version == 1`,
		`.status == "valid"`,
		`.artifact_cells == 6`,
		`.selected_cells == $selected_cells`,
		`(.dataset_probe_sha256 | test("^[0-9a-f]{64}$"))`,
		`.binding_file_sha256 == $binding_file_sha256`,
	} {
		if !strings.Contains(bindingBlock, required) {
			t.Fatalf("Artifact-targeted binding validation omits %q", required)
		}
	}
	finalAdjudication := strings.Index(body[runnerStart:], `# A process-level zero exit retains failed measured samples`)
	if finalAdjudication < 0 ||
		!strings.Contains(body[runnerStart+finalAdjudication:],
			".taskgate_acceptance_v3 != null and\n    .publication_eligible == false") {
		t.Fatal("the post-run adjudicator does not require v3 acceptance")
	}
}

func TestArtifactTargetedScaleSelectionIsAValidatedFrozenSubset(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "ARTIFACT_SCALE_SELECTION")

	defaultOutput, err := runLauncherShellBlock(block, `artifact_default_scales`)
	if err != nil {
		t.Fatalf("default scale selection: %v\n%s", err, defaultOutput)
	}
	const allScales = "100x4,10k-x4,100k-x4,100x16,10k-x16,100k-x16"
	if got := strings.TrimSpace(string(defaultOutput)); got != allScales {
		t.Fatalf("default scale selection = %q, want %q", got, allScales)
	}

	for _, test := range []struct {
		name      string
		selection string
		want      []string
	}{
		{name: "single canary cell", selection: "100x4", want: []string{"100x4"}},
		{name: "two cells retain frozen order", selection: "100k-x16,100x4", want: []string{"100x4", "100k-x16"}},
		{name: "all cells", selection: allScales, want: strings.Split(allScales, ",")},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runLauncherShellBlock(block, `resolve_artifact_scales "$1"`, test.selection)
			if err != nil {
				t.Fatalf("resolve %q: %v\n%s", test.selection, err, output)
			}
			var got []string
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatalf("decode scale selection %q: %v\n%s", test.selection, err, output)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("resolve %q = %v, want %v", test.selection, got, test.want)
			}
		})
	}

	for _, selection := range []string{
		"",
		"unknown",
		"100x4,100x4",
		"100x4,",
		",100x4",
		"100x4, 10k-x4",
	} {
		t.Run("reject "+selection, func(t *testing.T) {
			if output, err := runLauncherShellBlock(block, `resolve_artifact_scales "$1"`, selection); err == nil {
				t.Fatalf("resolve %q unexpectedly succeeded: %s", selection, output)
			}
		})
	}
}

func TestArtifactTargetedFormalWindowGateRequiresThreeExactPasses(t *testing.T) {
	body := artifactTargetedLauncherBody(t)
	block := launcherShellBlock(t, body, "FORMAL_WINDOW_GATE_ADJUDICATION")
	tests := []string{
		"TestFormalDeploymentRunsTheApprovedHealthcheckLive",
		"TestPeriodicLivenessProbesAddNoBusinessStatements",
		"TestExplicitReadinessOutsideTheWindowStillAttests",
	}
	expected, err := json.Marshal(tests)
	if err != nil {
		t.Fatal(err)
	}
	event := func(name, action string) string {
		payload, err := json.Marshal(map[string]string{"Action": action, "Test": name})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	packageEvent := func(action string) string {
		payload, err := json.Marshal(map[string]string{"Action": action})
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	passes := []string{event(tests[0], "pass"), event(tests[1], "pass"), event(tests[2], "pass")}

	for _, test := range []struct {
		name    string
		events  []string
		wantErr bool
	}{
		{name: "three exact passes", events: passes},
		{name: "skip", events: []string{passes[0], event(tests[1], "skip"), passes[2]}, wantErr: true},
		{name: "failure", events: []string{passes[0], event(tests[1], "fail"), passes[2]}, wantErr: true},
		{name: "missing", events: passes[:2], wantErr: true},
		{name: "duplicate terminal", events: append(append([]string{}, passes...), passes[2]), wantErr: true},
		{name: "unexpected terminal", events: []string{passes[0], passes[1], event("TestUnexpected", "pass")}, wantErr: true},
		{name: "package skip", events: append(append([]string{}, passes...), packageEvent("skip")), wantErr: true},
		{name: "package failure", events: append(append([]string{}, passes...), packageEvent("fail")), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "formal-window.jsonl")
			if err := os.WriteFile(report, []byte(strings.Join(test.events, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := runLauncherShellBlock(block,
				`require_formal_window_gate_passes "$1" "$2"`, report, string(expected))
			if test.wantErr && err == nil {
				t.Fatalf("gate adjudication unexpectedly succeeded: %s", output)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("gate adjudication failed: %v\n%s", err, output)
			}
		})
	}
}
