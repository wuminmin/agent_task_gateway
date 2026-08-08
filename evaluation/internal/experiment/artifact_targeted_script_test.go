package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactTargetedLauncherWiresTheFormalRuntimeContract(t *testing.T) {
	path := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-artifact-targeted.sh")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	for _, required := range []string{
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
		`go run ./evaluation/cmd/final-v5-profile-binding`,
		`--dataset-binding-sha256 "$TASKGATE_FINAL_V5_BINDING_FILE_SHA256"`,
		`-profile-binding "$(realpath "$profile_binding")"`,
		`expected=$((6 * SAMPLES))`,
		`.status == "pass"`,
		`.system == "taskgate"`,
		`.taskgate_acceptance_v3 != null`,
		`.publication_eligible == false`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("targeted Artifact launcher omits runtime contract %q", required)
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
}
