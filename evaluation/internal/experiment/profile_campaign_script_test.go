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
		`selected Artifact profile requires ATTESTATION_QUALIFICATION before deployment`,
		`selected Artifact profile requires POSTGRESQL_IDENTITY before deployment`,
		`-adapter-stderr-output "$adapter_stderr"`,
		`final-v5-adapter-stderr-scan`,
		`-sensitive-json-file "$current_rq5_secret/deployment-secrets.json"`,
		`Compose profile capacity binding drift`,
		`add_ref adapter_stderr "$experiment"`,
		`add_ref adapter_stderr_credential_scan "$experiment"`,
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
