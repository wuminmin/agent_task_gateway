package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNonProfileSmokeLauncherIsPilotOnlyAndReusesFrozenRunners(t *testing.T) {
	path := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-nonprofile-smoke.sh")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(payload)
	for _, required := range []string{
		`-campaign-class publication -require-ready`,
		`final-v5-split-publication`,
		`.campaign_class="pilot" | .pilot_kind="nonprofile_smoke"`,
		`TASKGATE_EXPERIMENT_CLASS=pilot`,
		`v5-scale`,
		`view-scale`,
		`final-v5 nonprofile-finalize`,
		`group_summary_sha256`,
		`raw_execution_sha256`,
		`for repetition in 1 2 3; do`,
		`postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55`,
		`-u TASKGATE_FINAL_V5_COMPILER_DSN`,
		`db/init/08-final-v5-compiler-fixture.sql`,
		`TASKGATE_FINAL_V5_COMPILER_DSN=$nonprofile_dsn`,
		`if [[ "$nonprofile_id" == scale-outcome-merkle || "$nonprofile_id" == compiler ]]; then`,
		`: "${TASKGATE_DATASET_BINDINGS:?TASKGATE_DATASET_BINDINGS is required}"`,
		`expected_binding_sha=3ae86ce4d2b7a94916dc11e5e0092ec5e5280ec6e27a2964a50bda43bcc13380`,
		`expected_binding_section_sha=b088b75e2c81a39ad5219ea36a4d1c8c8abf3e11e32570ddce3ad0b8bb756d5c`,
		`$adapter" --validate-binding`,
		`current_valid:true`,
		`private_path:$path`,
		`-u TASKGATE_DATASET_BINDINGS`,
		`-u TASKGATE_FINAL_V5_BINDING_FILE_SHA256`,
		`-u TASKGATE_FINAL_V5_BINDING_SECTION_SHA256`,
		`if [[ "$nonprofile_id" == scale-outcome-merkle ]]; then`,
		`TASKGATE_DATASET_BINDINGS=$nonprofile_binding_path`,
		`TASKGATE_FINAL_V5_BINDING_FILE_SHA256=$binding_file_sha`,
		`TASKGATE_FINAL_V5_BINDING_SECTION_SHA256=$binding_section_sha`,
		`binding_section_sha="$(jq -er .final_v5_adapter_sha256 "$binding_validator_raw")"`,
		`final_v5_adapter_sha256:$section_sha256`,
		`binding_consumed=`,
		`campaign_class:"pilot",pilot_kind:"nonprofile_smoke",publication_eligible:false,formal_campaign:false`,
		`profile_binding:"forbidden"`,
		`P63E-CELL: cell=`,
		`cells=47/47`,
		`deployments=0`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("non-profile smoke launcher lacks %q", required)
		}
	}
	for _, forbidden := range []string{
		"docker compose",
		"run-deployment.sh",
		"run-profile-campaign.sh",
		"-profile-binding",
		"final-v5 evidence",
		"/evidence/manifest.json",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("non-profile smoke launcher contains deployment/profile path %q", forbidden)
		}
	}
}

func TestNonProfileBindingInjectionMatchesStaticAdapterConsumers(t *testing.T) {
	adapterPath := filepath.Join("..", "..", "cmd", "final-v5-adapter")
	bindingPayload, err := os.ReadFile(filepath.Join(adapterPath, "adapter_bindings.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`os.Getenv("TASKGATE_DATASET_BINDINGS")`,
		`os.Getenv("TASKGATE_FINAL_V5_BINDING_FILE_SHA256")`,
		`os.Getenv("TASKGATE_FINAL_V5_BINDING_SECTION_SHA256")`,
	} {
		if !strings.Contains(string(bindingPayload), required) {
			t.Fatalf("binding loader no longer requires audited environment %q", required)
		}
	}
	scalePayload, err := os.ReadFile(filepath.Join(adapterPath, "scale.go"))
	if err != nil {
		t.Fatal(err)
	}
	scale := string(scalePayload)
	for _, workload := range []string{`case "outcome-merkle":`, `case "taskgate_scale_extreme":`} {
		start := strings.Index(scale, workload)
		if start < 0 {
			t.Fatalf("Scale adapter lacks %s", workload)
		}
		rest := scale[start:]
		end := strings.Index(rest[len(workload):], "\n\tcase ")
		if end >= 0 {
			rest = rest[:len(workload)+end]
		}
		if !strings.Contains(rest, "loadAdapterDeploymentBinding()") {
			t.Fatalf("%s no longer consumes the private binding", workload)
		}
	}
	compilerPayload, err := os.ReadFile(filepath.Join(adapterPath, "compiler.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compilerPayload), "loadAdapterDeploymentBinding()") {
		t.Fatal("Compiler unexpectedly consumes the private Dataset Binding")
	}
}
