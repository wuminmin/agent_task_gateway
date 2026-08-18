package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileCampaignLauncherKeepsCommitProfileAndEvidenceBoundaries(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-profile-campaign.sh")
	payload, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	script := string(payload)
	for _, required := range []string{
		`repetitions="${TASKGATE_CAMPAIGN_REPETITIONS:-1}"`,
		`final-v5-campaign-plan -require-ready`,
		`-profile-alias "$alias"`,
		`export TASKGATE_FINAL_V5_PROFILE_ALIAS="$alias"`,
		`-selected-cells "$selected"`,
		`final-v5-launcher-gate`,
		`-campaign-class pilot -samples-per-cell 1`,
		`concurrency-preregistration-v1.json`,
		`-deployment-repetition "$repetition"`,
		`-preregistration-sha256 "$preregistration_sha256"`,
		`final-v5-profile-artifacts`,
		`record-pilot-gateway-image.sh`,
		`down --volumes --remove-orphans`,
		`final-v5-campaign-evidence`,
		`deployment-overrides-v1.json`,
		`final-v5-profile-deployment-config`,
		`apply_profile_deployment_environment "$deployment_configuration"`,
		`check-profile-deployment-compose.sh`,
		`GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE`,
		`GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE`,
		`export TASKGATE_FINAL_V5_CATALOG=`,
		`export TASKGATE_PROFILE_ARTIFACT_DIR=`,
		`export TASKGATE_FINAL_V5_REPO_ROOT=`,
		`selected Artifact/ProvSQL profile requires ATTESTATION_QUALIFICATION before deployment`,
		`selected Artifact/ProvSQL profile requires POSTGRESQL_IDENTITY before deployment`,
		`selected Scale profile requires SCALE_ATTESTATION_QUALIFICATION before deployment`,
		`selected Scale profile requires SCALE_POSTGRESQL_IDENTITY before deployment`,
		`any(. == "artifact" or . == "provsql")`,
		`any(. == "scale")`,
		`export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$finalizer_qualification"`,
		`export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$finalizer_postgresql_identity"`,
		`export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$scale_finalizer_qualification"`,
		`export TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY="$scale_finalizer_postgresql_identity"`,
		`finalizer_material_dispatch`,
		`attestation_qualification_path:$finalizer_qualification`,
		`attestation_qualification_sha256:$finalizer_qualification_sha256`,
		`postgresql_identity_path:$finalizer_postgresql_identity`,
		`postgresql_identity_sha256:$finalizer_postgresql_identity_sha256`,
		`attestation_qualification_path:$scale_finalizer_qualification`,
		`attestation_qualification_sha256:$scale_finalizer_qualification_sha256`,
		`postgresql_identity_path:$scale_finalizer_postgresql_identity`,
		`postgresql_identity_sha256:$scale_finalizer_postgresql_identity_sha256`,
		`-adapter-stderr-output "$adapter_stderr"`,
		`final-v5-adapter-stderr-scan`,
		`-sensitive-json-file "$current_rq5_secret/deployment-secrets.json"`,
		`Compose profile capacity binding drift`,
		`add_ref adapter_stderr "$experiment"`,
		`add_ref adapter_stderr_credential_scan "$experiment"`,
		`add_ref launcher_gate "$experiment"`,
		`add_ref deployment_configuration ""`,
		`publication_eligible:false`,
		`formal_campaign:false`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("profile campaign launcher lacks %q", required)
		}
	}
	if strings.Contains(script, `.taskgate_acceptance_v3 != null and .taskgate_rejection_v1 == null`) {
		t.Fatal("profile campaign launcher still applies one finalizer shape to every experiment")
	}
	if strings.Count(script, `git rev-parse HEAD`) != 1 {
		t.Fatal("submission commit must be read only for the launch-time fixed-input assertion")
	}
	scan := strings.Index(script, `final-v5-adapter-stderr-scan`)
	retain := strings.Index(script, `add_ref adapter_stderr "$experiment"`)
	if scan < 0 || retain < 0 || scan > retain {
		t.Fatal("Adapter stderr is not credential-gated before campaign retention")
	}
	if !strings.Contains(script, `-retained-source-path "$deployment_overrides_retained_rel"`) ||
		!strings.Contains(script, `-overrides "$deployment_overrides_retained"`) {
		t.Fatal("profile deployment configuration is not bound to its retained source-controlled override")
	}
	composeCheck := strings.Index(script, `check-profile-deployment-compose.sh`)
	composeUp := strings.Index(script, `"${current_compose[@]}" up`)
	if composeCheck < 0 || composeUp < 0 || composeCheck > composeUp {
		t.Fatal("profile deployment Compose capacities are not rendered and checked before service startup")
	}
	preregistrationInstall := strings.Index(script, `install -m 600 "$preregistration_source" "$preregistration_retained"`)
	if preregistrationInstall < 0 || preregistrationInstall > composeUp {
		t.Fatal("concurrency preregistration is not retained and digest-checked before service startup")
	}
	qualificationGate := strings.Index(script, `selected Artifact/ProvSQL profile requires ATTESTATION_QUALIFICATION before deployment`)
	scaleQualificationGate := strings.Index(script, `selected Scale profile requires SCALE_ATTESTATION_QUALIFICATION before deployment`)
	qualificationExport := strings.Index(script, `export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$finalizer_qualification"`)
	scaleQualificationExport := strings.Index(script, `export TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION="$scale_finalizer_qualification"`)
	materialUnset := strings.LastIndex(script, `unset TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY`)
	runnerDispatch := strings.Index(script, `go run "./evaluation/cmd/$runner"`)
	if qualificationGate < 0 || qualificationGate > composeUp {
		t.Fatal("Artifact/ProvSQL qualification material is not required before deployment startup")
	}
	if scaleQualificationGate < 0 || scaleQualificationGate > composeUp {
		t.Fatal("Scale qualification material is not required before deployment startup")
	}
	if qualificationExport < 0 || runnerDispatch < 0 || qualificationExport > runnerDispatch {
		t.Fatal("Artifact/ProvSQL qualification material is not exported before experiment runner dispatch")
	}
	if scaleQualificationExport < 0 || scaleQualificationExport > runnerDispatch {
		t.Fatal("Scale qualification material is not exported before experiment runner dispatch")
	}
	if materialUnset < 0 || materialUnset > qualificationExport || materialUnset > scaleQualificationExport {
		t.Fatal("experiment dispatch can inherit finalizer material from a preceding experiment")
	}
	if !strings.Contains(script, `"$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT"`) {
		t.Fatal("launcher does not assert the checkout against its fixed submission-commit input")
	}
	if !strings.Contains(script, `rq5-project-prefix.sh "$TASKGATE_CAMPAIGN_ID" deployment-01`) ||
		strings.Contains(script, `rq5-project-prefix.sh "$project_identity" deployment-01`) {
		t.Fatal("RQ5 driver project prefix is not derived from the campaign identity bound in driver state")
	}
	for _, required := range []string{
		`fixture="$current_rq5_project-fixture"`,
		`network="$current_rq5_project-business"`,
		`"$rq5_driver" --cleanup-deployment`,
		`DAILY_RQ5_INSTALL_DSN=postgres://cleanup:cleanup@rq5-cleanup.invalid/cleanup?sslmode=disable`,
		`docker container rm --force --volumes`,
		`docker volume rm`,
		`docker network rm`,
		`label=com.docker.compose.project=$project`,
		`taskgate.rq5.owner`,
		`external_networks:$external_networks`,
		`deployment-cleanup-driver.json`,
		`deployment-cleanup.json`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("profile campaign RQ5 cleanup lacks %q", required)
		}
	}
}

func TestProfileCampaignArtifactRootMatchesMaterializerLayout(t *testing.T) {
	launcherPath := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-profile-campaign.sh")
	launcherPayload, err := os.ReadFile(launcherPath)
	if err != nil {
		t.Fatal(err)
	}
	materializerPath := filepath.Join("..", "finalv5profile", "artifacts.go")
	materializerPayload, err := os.ReadFile(materializerPath)
	if err != nil {
		t.Fatal(err)
	}
	materializer := string(materializerPayload)
	if !strings.Contains(materializer, `final := filepath.Join(destinationRoot, profile.ID)`) {
		t.Fatal("profile artifact materializer no longer writes beneath destination/<profile-id>")
	}

	launcher := string(launcherPayload)
	materialize := strings.Index(launcher, `--destination "$profile_artifacts"`)
	deriveProfileRoot := strings.Index(launcher, `profile_artifact_dir="$profile_artifacts/$profile_id"`)
	exportProfileRoot := strings.Index(launcher,
		`export TASKGATE_PROFILE_ARTIFACT_DIR="$(cd "$profile_artifact_dir" && pwd)"`)
	activationParentRoot := strings.Index(launcher, `-profile-artifact-root "$profile_artifacts"`)
	if materialize < 0 || deriveProfileRoot < materialize || exportProfileRoot < deriveProfileRoot {
		t.Fatal("finalizer artifact root is not derived from the materializer's destination/<profile-id> layout")
	}
	if activationParentRoot < exportProfileRoot {
		t.Fatal("activation no longer receives the parent root from which it derives its own profile-id directory")
	}
	if strings.Contains(launcher, `export TASKGATE_PROFILE_ARTIFACT_DIR="$profile_artifacts"`) {
		t.Fatal("finalizer artifact root still names the parent that contains multiple profile directories")
	}
}

func TestProfileCampaignUsesOneFormalGatewayImageForEveryDeployment(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-profile-campaign.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(payload)
	build := strings.Index(script, `go run ./evaluation/cmd/final-v5-gateway-build build`)
	deploymentLoop := strings.Index(script, "deployment_count=0\nfor alias in \"${selected_profiles[@]}\"; do")
	if build < 0 || deploymentLoop < 0 || build > deploymentLoop {
		t.Fatal("formal Gateway is not built once before the deployment loop")
	}
	if strings.Count(script, `go run ./evaluation/cmd/final-v5-gateway-build build`) != 1 {
		t.Fatal("profile campaign has more than one formal Gateway build path")
	}
	for _, required := range []string{
		`-manifest-out "$formal_gateway_build_manifest"`,
		`-compose-override-out "$formal_gateway_compose_override"`,
		`-dataset-binding "$TASKGATE_DATASET_BINDINGS"`,
		`-profile-registry "$TASKGATE_FINAL_V5_PROFILE_REGISTRY"`,
		`taskgate_formal_campaign_compose current_compose`,
		`.services.gateway.image == $image and .services.gateway.pull_policy == "never" and
      (.services.gateway | has("build") | not)`,
		`go run ./evaluation/cmd/final-v5-gateway-build verify`,
		`.formal_gateway_built == true and .formal_build_label == "v1"`,
		`formal_gateway_built:true`,
		`build_context_sha256:$formal_gateway_manifest[0].build_context_sha256`,
		`dataset_binding_sha256:$formal_gateway_manifest[0].dataset_binding_sha256`,
		`add_ref formal_gateway_runtime "" "$formal_gateway_runtime"`,
		`add_ref formal_gateway_build_manifest "" "$formal_gateway_build_manifest"`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("profile campaign formal Gateway wiring lacks %q", required)
		}
	}
	if strings.Contains(script, `if [[ "$requires_finalizer_material" == true ]]`) {
		t.Fatal("formal Gateway image selection is conditional on the current profile")
	}

	observation, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "record-pilot-gateway-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	record := string(observation)
	for _, required := range []string{
		`formal_gateway_built: (image_label("taskgate.formal_build") == "v1")`,
		`formal_build_label: image_label("taskgate.formal_build")`,
		`build_target: image_label("taskgate.build_target")`,
		`builder_base_image: image_label("taskgate.builder_base_image")`,
		`runtime_base_image: image_label("taskgate.runtime_base_image")`,
		`image_source_equivalence_asserted: false`,
	} {
		if !strings.Contains(record, required) {
			t.Fatalf("P20 Gateway observation lacks %q", required)
		}
	}
}

func TestRunDeploymentRoutesOnlyPilotToProfileCampaign(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-deployment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(payload)
	dispatch := `if [[ "${TASKGATE_EXPERIMENT_CLASS:-}" == pilot ]]; then`
	publicationGuard := `[[ "$TASKGATE_EXPERIMENT_CLASS" == publication ]]`
	if strings.Index(script, dispatch) < 0 || strings.Index(script, dispatch) > strings.Index(script, publicationGuard) {
		t.Fatal("pilot dispatch is absent or occurs after the publication-only guard")
	}
	if !strings.Contains(script,
		`DAILY_RQ5_INSTALL_DSN=postgres://cleanup:cleanup@rq5-cleanup.invalid/cleanup?sslmode=disable`) {
		t.Fatal("formal campaign RQ5 cleanup cannot interpolate the installer service")
	}
}
