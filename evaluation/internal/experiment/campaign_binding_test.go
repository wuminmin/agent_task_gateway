package experiment

import (
	"encoding/json"
	"fmt"
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
	for _, command := range []string{sourceAdapterBuildCommand, observerBuildCommand, rq5DriverBuildCommand} {
		if !strings.Contains(script, `--arg build_command "`+command+`"`) {
			t.Fatalf("run-deployment build manifest does not record %q", command)
		}
	}
	const fullInventory = `git ls-files | sort | while IFS= read -r source_file`
	if strings.Count(script, fullInventory) != 3 {
		t.Fatal("adapter, observer, and RQ5 build manifests must each bind the complete tracked-file inventory")
	}
	repoRoot := filepath.Join("..", "..", "..")
	var complete strings.Builder
	for index, path := range rq5RequiredRuntimeSources {
		info, err := os.Lstat(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("required RQ5 runtime input %q is absent or unsafe: %v", path, err)
		}
		fmt.Fprintf(&complete, "%064x  %s\n", index+1, path)
	}
	if err := validateBoundSourceListing(complete.String(), rq5RequiredRuntimeSources); err != nil {
		t.Fatalf("complete RQ5 source inventory was rejected: %v", err)
	}
	for omitted, path := range rq5RequiredRuntimeSources {
		var incomplete strings.Builder
		for index, candidate := range rq5RequiredRuntimeSources {
			if index != omitted {
				fmt.Fprintf(&incomplete, "%064x  %s\n", index+1, candidate)
			}
		}
		err := validateBoundSourceListing(incomplete.String(), rq5RequiredRuntimeSources)
		want := "source listing omits required runtime input " + path
		if err == nil || err.Error() != want {
			t.Fatalf("inventory without %q error = %v, want %q", path, err, want)
		}
	}
}

func TestPublicationPrivateInputsAndObserverFailClosedBeforeMeasurement(t *testing.T) {
	value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-deployment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(value)
	marker := strings.Index(script, `(set -o noclobber; printf '%s\n' "$(date -u +%FT%TZ)" > "$marker")`)
	composeBuild := strings.Index(script, `"${compose_build[@]}" build`)
	validateBinding := strings.Index(script, `binding_validation="$($adapter_binary --validate-binding)"`)
	validateObserver := strings.Index(script, `$adapter_binary --validate-observer-runtime`)
	freezeConfig := strings.Index(script, `install -m 600 "$config_source" "$frozen_config"`)
	exportExpectedBinding := strings.Index(script, `export TASKGATE_FINAL_V5_BINDING_FILE_SHA256=`)
	if marker < 0 || composeBuild < 0 || validateBinding < 0 || validateObserver < 0 || freezeConfig < 0 || exportExpectedBinding < 0 ||
		validateBinding > marker || validateObserver > marker || freezeConfig > marker || exportExpectedBinding > marker || marker > composeBuild {
		t.Fatal("private config/binding/observer identity is not frozen and validated before marker/Compose")
	}
	startFresh := strings.Index(script, `evaluation/final-v5-wsl2/scripts/start-fresh-deployment.sh`)
	bootstrapBefore := strings.Index(script, `"$TASKGATE_FINAL_V5_OBSERVER" --phase before`)
	bootstrapAfter := strings.Index(script, `"$TASKGATE_FINAL_V5_OBSERVER" --phase after`)
	measured := strings.Index(script, `export TASKGATE_ENVIRONMENT_OUTPUT="$environment_path"`)
	if startFresh < 0 || bootstrapBefore < startFresh || bootstrapAfter < bootstrapBefore || measured < bootstrapAfter ||
		!strings.Contains(script, `.[0].runtime_identity_sha256 == .[1].runtime_identity_sha256`) ||
		!strings.Contains(script, `.[0].oom_events == 0 and .[1].oom_events == 0`) ||
		!strings.Contains(script, `.[0].container_restarts == 0 and .[1].container_restarts == 0`) {
		t.Fatal("source-built observer is not exercised as a strict before/after transition before measured runners")
	}
	for _, modeGuard := range []string{
		`private config directory must have mode 0700`, `private config must have mode 0600`,
		`private dataset binding directory must have mode 0700`, `private dataset binding must have mode 0600`,
	} {
		if !strings.Contains(script, modeGuard) {
			t.Fatalf("publication launcher omits private input permission guard %q", modeGuard)
		}
	}

	powerShellBytes, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-three-deployments.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	powerShell := string(powerShellBytes)
	precheck := strings.Index(powerShell, `$bindingCheck =`)
	firstDeployment := strings.Index(powerShell, `for ($i=1; $i -le $Deployments; $i++) {
  wsl.exe --shutdown`)
	if precheck < 0 || firstDeployment < 0 || precheck > firstDeployment ||
		!strings.Contains(powerShell, `deployment binding bytes differ`) ||
		!strings.Contains(powerShell, `"$(stat -c %a "$binding")" == 600`) {
		t.Fatal("PowerShell controller does not verify all three byte-identical 0600 bindings before deployment-01")
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

func TestRealPilotUsesCollisionSafeDeploymentProjectName(t *testing.T) {
	launcher, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", "run-real-pilot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(launcher)
	if !strings.Contains(body, "deployment-project-name.sh") ||
		!strings.Contains(body, `"$TASKGATE_CAMPAIGN_ID" "$TASKGATE_DEPLOYMENT_ID"`) {
		t.Fatal("real Pilot does not derive the same complete owner identity as the destructive fresh-deployment launcher")
	}
	if strings.Contains(body, "taskgate-final-v5-pilot-local-only-deployment-01") {
		t.Fatal("real Pilot still uses the legacy project name rejected by the fresh-deployment safety boundary")
	}
}

func TestPublicationEnvironmentBindsFreshDatasetCatalogAndVolumeEvidence(t *testing.T) {
	readScript := func(name string) string {
		t.Helper()
		value, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(value)
	}
	fresh := readScript("start-fresh-deployment.sh")
	for _, required := range []string{
		"TASKGATE-FINAL-V5-DEPLOYMENT-VOLUME-ID-V1",
		"deployment_volume_id_sha256",
		"catalog_sha256",
		`gateway cat /etc/taskbound/catalog.yaml`,
		`publication dataset fingerprint SQL is source-controlled and cannot be overridden`,
		`publication Compose files differ from the frozen formal topology`,
		`.services.gateway.environment.CATALOG_PATH == $target`,
		`$mounts[0].source == $source`,
	} {
		if !strings.Contains(fresh, required) {
			t.Fatalf("fresh-deployment launcher omits binding contract %q", required)
		}
	}
	record := readScript("record-environment.sh")
	if !strings.Contains(record, `--fresh-deployment-proof "$TASKGATE_FRESH_PROOF_OUTPUT"`) {
		t.Fatal("publication environment recorder does not consume the fresh-deployment proof")
	}
	run := readScript("run-deployment.sh")
	for _, required := range []string{`fresh.catalog.yaml`, `publication Compose files are source-controlled and cannot be overridden`} {
		if !strings.Contains(run, required) {
			t.Fatalf("formal deployment omits binding guard %q", required)
		}
	}
}

func TestCampaignEnvironmentDigestMustMatchAcrossExperiments(t *testing.T) {
	digests := make(map[string]string)
	first := strings.Repeat("a", 64)
	if err := bindCampaignEnvironmentDigest(digests, "deployment-01", first); err != nil {
		t.Fatal(err)
	}
	if err := bindCampaignEnvironmentDigest(digests, "deployment-01", first); err != nil {
		t.Fatalf("identical experiment environment was rejected: %v", err)
	}
	if err := bindCampaignEnvironmentDigest(digests, "deployment-01", strings.Repeat("b", 64)); err == nil {
		t.Fatal("changed experiment environment digest was accepted")
	}
}

func TestFreshDeploymentRejectsPublicationComposeOverrideBeforeStartup(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")
	campaignID, deploymentID := "compose-override-test", "deployment-01"
	derive := filepath.Join(repoRoot, "evaluation", "final-v5-wsl2", "scripts", "deployment-project-name.sh")
	projectBytes, err := exec.Command("bash", derive, campaignID, deploymentID).CombinedOutput()
	if err != nil {
		t.Fatalf("derive project name: %v: %s", err, projectBytes)
	}
	launcher := filepath.Join("evaluation", "final-v5-wsl2", "scripts", "start-fresh-deployment.sh")
	toolPath := t.TempDir()
	for _, name := range []string{"awk", "bash", "sha256sum"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(toolPath, name)); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", launcher)
	command.Dir = repoRoot
	command.Env = append(environmentWithout("PATH"),
		"PATH="+toolPath,
		"TASKGATE_EXPERIMENT_CLASS=publication",
		"TASKGATE_CAMPAIGN_ID="+campaignID,
		"TASKGATE_DEPLOYMENT_ID="+deploymentID,
		"COMPOSE_PROJECT_NAME="+strings.TrimSpace(string(projectBytes)),
		"TASKGATE_FRESH_PROOF_OUTPUT="+filepath.Join(t.TempDir(), "deployment-01.fresh.json"),
		"TASKGATE_COMPOSE_FILES=compose.yaml",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("publication Compose override reached deployment startup")
	}
	if !strings.Contains(string(output), "publication Compose files differ from the frozen formal topology") {
		t.Fatalf("publication Compose override failed for the wrong reason: %s", output)
	}
}

func environmentWithout(names ...string) []string {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, found := strings.Cut(entry, "=")
		if _, skip := excluded[name]; found && skip {
			continue
		}
		result = append(result, entry)
	}
	return result
}
