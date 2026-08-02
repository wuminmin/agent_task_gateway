package experiment

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSourceBuildBindingVerifiesBinarySourceCommitAndCommand(t *testing.T) {
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "adapter")
	if err := os.WriteFile(binaryPath, []byte("bound source-built executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := FileSHA256(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	sources := strings.Repeat("b", 64) + "  evaluation/cmd/final-v5-adapter/main.go"
	manifest := sourceBuildBinding{SchemaVersion: 1, SubmissionCommit: commit, BinarySHA256: digest,
		SourceSHA256: sha256Hex([]byte(sources)), GoVersion: "go-test", BuildCommand: sourceAdapterBuildCommand,
		SourceFiles: sources}
	manifestBytes, _ := json.Marshal(manifest)
	digestPath, manifestPath := filepath.Join(directory, "adapter.sha256"), filepath.Join(directory, "adapter-build.json")
	if err := os.WriteFile(digestPath, []byte(digest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := verifySourceBuildBinding(digestPath, manifestPath, binaryPath, commit, sourceAdapterBuildCommand, nil); err != nil || got != digest {
		t.Fatalf("valid binding digest=%q err=%v", got, err)
	}
	manifest.BuildCommand = "go build ./placeholder"
	manifestBytes, _ = json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySourceBuildBinding(digestPath, manifestPath, binaryPath, commit, sourceAdapterBuildCommand, nil); err == nil {
		t.Fatal("mutated build command was accepted")
	}
}

func TestRunDeploymentBuildCommandsMatchCampaignFinalizer(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-deployment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(value)
	for _, command := range []string{sourceAdapterBuildCommand, rq5DriverBuildCommand} {
		if !strings.Contains(script, `--arg build_command "`+command+`"`) {
			t.Fatalf("run-deployment build manifest does not record %q", command)
		}
	}
	const fullInventory = `git ls-files | sort | while IFS= read -r source_file`
	if strings.Count(script, fullInventory) != 2 {
		t.Fatal("adapter and RQ5 build manifests must each bind the complete tracked-file inventory")
	}
	repoRoot := filepath.Join("..", "..", "..")
	for _, path := range rq5RequiredRuntimeSources {
		if output, err := exec.Command("git", "-C", repoRoot, "ls-files", "--error-unmatch", path).CombinedOutput(); err != nil {
			t.Fatalf("required RQ5 runtime input %q is outside the complete tracked inventory: %v: %s", path, err, output)
		}
	}
}

func TestFinalRouteInitDoesNotPlacePasswordsInChildArgv(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")
	initScripts := []string{
		"db/init/10-reader.sh",
		"db/control-init/10-control-role.sh",
		"evaluation/daily-publication-online/sql/10-online-runtime.sh",
		"evaluation/daily-publication/sql/10-reader.sh",
	}
	passwordArgument := regexp.MustCompile(`--set=[^[:space:]]*password`)
	for _, relative := range initScripts {
		value, err := os.ReadFile(filepath.Join(repoRoot, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		script := string(value)
		if passwordArgument.MatchString(script) {
			t.Fatalf("%s places a password in the psql process argv", relative)
		}
		if !strings.Contains(script, `\getenv `) {
			t.Fatalf("%s does not import its role password from the process environment", relative)
		}
	}

	for _, relative := range []string{
		"evaluation/final-v5-wsl2/scripts/run-deployment.sh",
		"evaluation/final-v5-wsl2/scripts/run-real-pilot.sh",
	} {
		value, err := os.ReadFile(filepath.Join(repoRoot, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		script := string(value)
		if strings.Contains(script, `--arg value "$1"`) {
			t.Fatalf("%s passes a password to jq through argv", relative)
		}
		if !strings.Contains(script, `printf '%s' "$1" | jq -sRr '@uri'`) {
			t.Fatalf("%s does not URL-encode credentials through stdin", relative)
		}
	}
}

func TestFinishDeploymentPropagatesEveryEvidenceWriteFailure(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-deployment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(value)
	start := strings.Index(script, "finish_deployment() {")
	if start < 0 {
		t.Fatal("finish_deployment source boundary is absent")
	}
	end := strings.Index(script[start:], "\n}\ntrap - EXIT")
	if end < 0 {
		t.Fatal("finish_deployment source boundary is absent")
	}
	body := script[start : start+end]
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if (strings.HasPrefix(trimmed, "install -m 600 ") || strings.HasPrefix(trimmed, "mkdir -m 700 ")) &&
			!strings.HasSuffix(trimmed, "|| status=1") {
			t.Fatalf("finish_deployment can ignore an evidence write failure: %q", trimmed)
		}
	}
	if !strings.Contains(body, `--unexpected-container-restarts "$restarts" "${oom_flag[@]}" || status=1`) {
		t.Fatal("record-deployment failure does not propagate to deployment exit status")
	}
}

func TestFinalV5EvidenceNeverEntersDockerBuildContext(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "..", "..", ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	ignored := make(map[string]bool)
	for _, line := range strings.Split(string(value), "\n") {
		ignored[strings.TrimSpace(line)] = true
	}
	for _, path := range []string{"**/raw", "**/evidence", "**/generated", "evaluation/final-v5-wsl2/raw", "evaluation/final-v5-wsl2/generated"} {
		if !ignored[path] {
			t.Fatalf("Docker build context does not exclude %s", path)
		}
	}
}

func TestDeploymentProjectNameHashesCompleteIdentityWithoutCollision(t *testing.T) {
	script := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "deployment-project-name.sh")
	derive := func(campaignID, deploymentID string) string {
		t.Helper()
		output, err := exec.Command("bash", script, campaignID, deploymentID).CombinedOutput()
		if err != nil {
			t.Fatalf("derive project for %q/%q: %v: %s", campaignID, deploymentID, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	identities := [][2]string{
		{"CaseSensitive", "deployment-01"},
		{"casesensitive", "deployment-01"},
		{"campaign.with.dots", "deployment-01"},
		{"campaign_with_dots", "deployment-01"},
		{strings.Repeat("a", 127) + "x", "deployment-01"},
		{strings.Repeat("a", 127) + "y", "deployment-01"},
		{"CaseSensitive", "deployment-02"},
	}
	pattern := regexp.MustCompile(`^taskgate-final-v5-deployment-0[1-3]-[0-9a-f]{20}$`)
	seen := make(map[string][2]string)
	for _, identity := range identities {
		project := derive(identity[0], identity[1])
		if !pattern.MatchString(project) || len(project) > 63 {
			t.Fatalf("unsafe or oversized project name %q", project)
		}
		if prior, exists := seen[project]; exists {
			t.Fatalf("project collision: %v and %v both produced %q", prior, identity, project)
		}
		seen[project] = identity
		if project != derive(identity[0], identity[1]) {
			t.Fatal("deployment project derivation is not deterministic")
		}
	}
	launcher, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "start-fresh-deployment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launcher), "deployment-project-name.sh") ||
		!strings.Contains(string(launcher), `"$COMPOSE_PROJECT_NAME" == "$expected_compose_project"`) {
		t.Fatal("destructive fresh-deployment launcher does not rederive the exact project owner identity")
	}
}
