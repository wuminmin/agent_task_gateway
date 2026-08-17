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
		`final-v5-profile-artifacts`,
		`record-pilot-gateway-image.sh`,
		`down --volumes --remove-orphans`,
		`final-v5-campaign-evidence`,
		`publication_eligible:false`,
		`formal_campaign:false`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("profile campaign launcher lacks %q", required)
		}
	}
	if strings.Count(script, `git rev-parse HEAD`) != 1 {
		t.Fatal("submission commit must be read only for the launch-time fixed-input assertion")
	}
	if !strings.Contains(script, `"$(git rev-parse HEAD)" == "$TASKGATE_SUBMISSION_COMMIT"`) {
		t.Fatal("launcher does not assert the checkout against its fixed submission-commit input")
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
}
