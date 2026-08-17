package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

func validDriverRequest(t *testing.T) driverRequest {
	t.Helper()
	cycle, err := rq5fixture.LookupCycle(1)
	if err != nil {
		t.Fatal(err)
	}
	return driverRequest{
		SchemaVersion: 1, DriverVersion: driverVersion, FixtureSHA256: rq5fixture.FixtureSHA256(),
		BuildManifestSHA256: fmt.Sprintf("%064x", 3),
		Operation: experiment.AdapterOperation{ExperimentID: "rq5", WorkloadID: rq5fixture.WorkloadID,
			Scale: rq5fixture.Scale, Mode: rq5fixture.BuildMode, Iteration: cycle.Index},
		CycleIndex: cycle.Index, FromDay: cycle.From, ToDay: cycle.To,
		GeneratorSHA256: fmt.Sprintf("%064x", 1), ConfigSHA256: fmt.Sprintf("%064x", 2),
	}
}

func TestPhaseReportWireAcceptsCompleteProducerSchemaAndRejectsUnknownFields(t *testing.T) {
	complete := []byte(`{
		"schema_version":"taskgate-daily-publication-phase-v1",
		"status":"pass",
		"phase":"build",
		"day":"day0",
		"sample":1,
		"executable":"v4-offline",
		"executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"argv_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"wall_ms":123.5,
		"peak_rss_bytes":4096,
		"peak_rss_scope":"root_process_vm_hwm_linux_procfs",
		"exit_code":0,
		"stdout_bytes":64,
		"stdout_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"stderr_bytes":0,
		"stderr_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"command_report":{"schema_version":1,"mode":"build"},
		"measurement_boundary":"child process wall clock"
	}`)
	path := filepath.Join(t.TempDir(), "complete-phase.json")
	if err := os.WriteFile(path, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	var report phaseReport
	if err := decodeJSONFile(path, &report); err != nil {
		t.Fatalf("complete daily-publication phase wire was rejected: %v", err)
	}
	if report.SchemaVersion != "taskgate-daily-publication-phase-v1" || report.Status != "pass" ||
		report.Phase != "build" || report.Day != "day0" || report.Sample != 1 || report.Executable != "v4-offline" ||
		report.PeakRSSBytes == nil || *report.PeakRSSBytes != 4096 || report.ExitCode != 0 || len(report.CommandReport) == 0 {
		t.Fatalf("complete phase report decoded incorrectly: %#v", report)
	}

	var object map[string]any
	if err := json.Unmarshal(complete, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected_wire_field"] = true
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(t.TempDir(), "unknown-phase.json")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := decodeJSONFile(unknownPath, &phaseReport{}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("genuine unknown phase wire field was not rejected: %v", err)
	}
}

func TestRuntimeSourcesAreCopiedFromExactBuildBindings(t *testing.T) {
	repo := t.TempDir()
	generator := filepath.Join(repo, "evaluation", "daily-publication", "sql", "05-generate-daily-data.sh")
	config := filepath.Join(repo, "evaluation", "daily-publication", "config.json")
	if err := os.MkdirAll(filepath.Dir(generator), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generator, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := validDriverRequest(t)
	var err error
	request.GeneratorSHA256, err = experiment.FileSHA256(generator)
	if err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256, err = experiment.FileSHA256(config)
	if err != nil {
		t.Fatal(err)
	}
	state := driverState{repoRoot: repo}
	cycle := filepath.Join(t.TempDir(), "cycle-1")
	if err := os.Mkdir(cycle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.bindRuntimeSources(request, cycle); err != nil {
		t.Fatal(err)
	}
	boundGenerator := filepath.Join(cycle, "bound-sources", "05-generate-daily-data.sh")
	if digest, err := experiment.FileSHA256(boundGenerator); err != nil || digest != request.GeneratorSHA256 {
		t.Fatalf("bound generator digest = %q, %v", digest, err)
	}

	if err := os.WriteFile(generator, []byte("#!/bin/sh\nexit 23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacedCycle := filepath.Join(t.TempDir(), "cycle-2")
	if err := os.Mkdir(replacedCycle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := state.bindRuntimeSources(request, replacedCycle); err == nil {
		t.Fatal("replacement generator passed the build-manifest digest")
	}
}

func TestCompleteSealedSourceInventoryIsMaterializedAndMutationRejected(t *testing.T) {
	checkout := t.TempDir()
	sources := map[string][]byte{
		"evaluation/daily-publication-online/compose.yaml": []byte("services: {}\n"),
		"evaluation/daily-publication/config.json":         []byte("{}\n"),
		"internal/control/migrations/001_test.sql":         []byte("SELECT 1;\n"),
	}
	paths := []string{
		"evaluation/daily-publication-online/compose.yaml",
		"evaluation/daily-publication/config.json",
		"internal/control/migrations/001_test.sql",
	}
	var listing strings.Builder
	for index, path := range paths {
		fullPath := filepath.Join(checkout, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, sources[path], 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(sources[path])
		if index != 0 {
			listing.WriteByte('\n')
		}
		fmt.Fprintf(&listing, "%s  %s", hex.EncodeToString(digest[:]), path)
	}
	sourceDigest := sha256.Sum256([]byte(listing.String()))
	manifest := sourceBuildManifest{
		SchemaVersion: 1, SubmissionCommit: strings.Repeat("a", 40),
		BinarySHA256: strings.Repeat("b", 64), SourceSHA256: hex.EncodeToString(sourceDigest[:]),
		GoVersion: "go-test", BuildCommand: rq5DriverBuildCommand, SourceFiles: listing.String(),
	}
	manifestPath := filepath.Join(t.TempDir(), "build-manifest.json")
	if err := writeJSONExclusive(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := experiment.FileSHA256(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	secretRoot := t.TempDir()
	if err := os.Chmod(secretRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	state := driverState{repoRoot: checkout, checkoutRoot: checkout, secretRoot: secretRoot,
		buildManifestPath: manifestPath}
	if err := state.ensureSourceSnapshot(manifestSHA); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(secretRoot, "source-snapshot")
	if state.repoRoot != snapshot || state.composeFile != filepath.Join(snapshot,
		"evaluation", "daily-publication-online", "compose.yaml") {
		t.Fatalf("driver did not switch to its sealed source snapshot: %#v", state)
	}
	for _, path := range paths {
		fullPath := filepath.Join(snapshot, filepath.FromSlash(path))
		if digest, err := experiment.FileSHA256(fullPath); err != nil ||
			digest == "" {
			t.Fatalf("snapshotted source %s = %q, %v", path, digest, err)
		}
		if info, err := os.Stat(fullPath); err != nil || info.ModTime().Unix() != 0 {
			t.Fatalf("snapshotted source %s has nondeterministic mtime: %#v, %v", path, info, err)
		}
	}

	snapshotConfig := filepath.Join(snapshot, "evaluation", "daily-publication", "config.json")
	if err := os.Chmod(snapshotConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotConfig, []byte("{\"mutated\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.ensureSourceSnapshot(manifestSHA); err == nil {
		t.Fatal("mutated private source snapshot passed re-attestation")
	}

	secondSecretRoot := t.TempDir()
	if err := os.Chmod(secondSecretRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "evaluation", "daily-publication", "config.json"),
		[]byte("{\"replaced\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := driverState{repoRoot: checkout, checkoutRoot: checkout, secretRoot: secondSecretRoot,
		buildManifestPath: manifestPath}
	if err := second.ensureSourceSnapshot(manifestSHA); err == nil {
		t.Fatal("checkout source replacement passed sealed snapshot materialization")
	}
}

func TestRQ5ImagesAreDigestPinnedAndBuildsAvoidMutablePackageRepositories(t *testing.T) {
	dailyDockerfile, err := os.ReadFile(filepath.Join("..", "..", "daily-publication", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	onlineDockerfile, err := os.ReadFile(filepath.Join("..", "..", "daily-publication-online", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join("..", "..", "daily-publication-online", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"daily golang": string(dailyDockerfile), "daily debian": string(dailyDockerfile),
		"online golang": string(onlineDockerfile), "online debian": string(onlineDockerfile),
	} {
		if !strings.Contains(value, "@sha256:") {
			t.Fatalf("%s base image is not digest pinned", name)
		}
		if strings.Contains(value, "apt-get") {
			t.Fatalf("%s build still consumes a mutable apt repository", name)
		}
		if !strings.Contains(value, "ARG SOURCE_DATE_EPOCH") ||
			!strings.Contains(value, `touch --date="@${SOURCE_DATE_EPOCH}"`) {
			t.Fatalf("%s build does not normalize image and binary timestamps", name)
		}
	}
	for _, reference := range []string{
		"minio/minio:RELEASE.2025-04-22T22-12-26Z@sha256:a1ea29fa28355559ef137d71fc570e508a214ec84ff8083e39bc5428980b015e",
		"minio/mc:RELEASE.2025-04-16T18-13-26Z@sha256:aead63c77f9db9107f1696fb08ecb0faeda23729cde94b0f663edf4fe09728e3",
	} {
		if !strings.Contains(string(compose), reference) {
			t.Fatalf("Compose omits pinned image %s", reference)
		}
	}
	if !strings.Contains(string(compose), "image: ${DAILY_RQ5_OA_IMAGE:-taskgate-daily-publication-online-tool}") {
		t.Fatal("OA Compose service has no independently attested runtime-image binding")
	}
}

func TestRuntimeImageBindingPinsPhaseOnlineAndOAByContentID(t *testing.T) {
	state := driverState{composeEnv: []string{
		"DAILY_PUBLICATION_PHASE_IMAGE=phase-tag",
		"DAILY_PUBLICATION_ONLINE_IMAGE=online-tag",
		"DAILY_RQ5_OA_IMAGE=oa-tag",
	}}
	attestation := runtimeImageAttestation{
		SchemaVersion: 1, BuildManifestSHA256: strings.Repeat("a", 64),
		PhaseImageID:  "sha256:" + strings.Repeat("b", 64),
		OnlineImageID: "sha256:" + strings.Repeat("c", 64),
		OAImageID:     "sha256:" + strings.Repeat("c", 64),
	}
	state.bindRuntimeImageIDs(attestation)
	for _, want := range []string{
		"DAILY_PUBLICATION_PHASE_IMAGE=" + attestation.PhaseImageID,
		"DAILY_PUBLICATION_ONLINE_IMAGE=" + attestation.OnlineImageID,
		"DAILY_RQ5_OA_IMAGE=" + attestation.OAImageID,
	} {
		if !containsEnvironment(state.composeEnv, want) {
			t.Fatalf("runtime image binding omitted %q: %#v", want, state.composeEnv)
		}
	}
	for _, old := range []string{"DAILY_PUBLICATION_PHASE_IMAGE=phase-tag",
		"DAILY_PUBLICATION_ONLINE_IMAGE=online-tag", "DAILY_RQ5_OA_IMAGE=oa-tag"} {
		if containsEnvironment(state.composeEnv, old) {
			t.Fatalf("mutable image tag survived binding: %q", old)
		}
	}
}

func TestCycleStackPullsDigestPinnedInfrastructureOnFreshHost(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "docker-argv.log")
	dockerPath := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RQ5_TEST_DOCKER_LOG\"\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	environment := replaceEnvironment(os.Environ(), "RQ5_TEST_DOCKER_LOG="+logPath)
	state := driverState{repoRoot: directory, composeFile: filepath.Join(directory, "compose.yaml")}
	workspace := cycleWorkspace{Project: "rq5-fresh-host", GatewayContainer: "rq5-fresh-host-gateway"}
	if err := state.ensureCycleStack(t.Context(), workspace, environment); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.Split(strings.TrimSpace(string(value)), "\n")
	if len(commands) != 2 ||
		!strings.Contains(commands[0], "up --detach --wait --no-build --pull missing control-postgres result-object-store oa-demo") ||
		!strings.Contains(commands[1], "up --no-deps --no-build --pull missing result-object-store-init") {
		t.Fatalf("fresh-host Compose pull contract = %#v", commands)
	}
}

func TestRuntimeImageBuildUsesStableProjectAndSourceEpoch(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "docker-build-argv.log")
	dockerPath := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nif [ \"$1\" = network ] && [ \"$2\" = inspect ]; then exit 1; fi\nprintf '%s\\n' \"$*\" >> \"$RQ5_TEST_DOCKER_LOG\"\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RQ5_TEST_DOCKER_LOG", logPath)
	environment := os.Environ()
	state := driverState{repoRoot: directory, composeFile: filepath.Join(directory, "compose.yaml"),
		runRoot: directory, fixtureProject: "rq5-deployment-fixture",
		businessNetwork: "rq5-deployment-business", composeEnv: environment}
	if err := state.ensureFixtureStack(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.Split(strings.TrimSpace(string(value)), "\n")
	want := "compose --project-name " + rq5RuntimeImageBuildProject + " --file " + state.composeFile +
		" build --provenance=false --build-arg SOURCE_DATE_EPOCH=" + rq5SourceDateEpoch + " phase online"
	found := false
	for _, command := range commands {
		found = found || command == want
	}
	if !found {
		t.Fatalf("stable image build command %q absent from %#v", want, commands)
	}
}

func TestRuntimeBinaryAttestationRequiresDigestAndSourceEpochMTime(t *testing.T) {
	path := "/usr/local/bin/rq5-online-transition"
	digest := strings.Repeat("a", 64)
	if gotDigest, gotMTime, err := parseRuntimeBinaryAttestation(
		[]byte(digest+"  "+path+"\n0\n"), path); err != nil || gotDigest != digest || gotMTime != 0 {
		t.Fatalf("valid runtime binary attestation = %q, %d, %v", gotDigest, gotMTime, err)
	}
	for name, value := range map[string][]byte{
		"invalid digest": []byte("not-a-digest  " + path + "\n0\n"),
		"wrong binary":   []byte(digest + "  /usr/local/bin/oa-demo\n0\n"),
		"mtime drift":    []byte(digest + "  " + path + "\n1\n"),
		"extra output":   []byte(digest + "  " + path + "\n0\nuntrusted\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseRuntimeBinaryAttestation(value, path); err == nil {
				t.Fatal("invalid runtime binary attestation was accepted")
			}
		})
	}
}

func encodeRequest(t *testing.T, request driverRequest) []byte {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDriverWritesUnderlyingCauseToStderrWithoutChangingResponseCode(t *testing.T) {
	request := validDriverRequest(t)
	for _, name := range []string{
		"TASKGATE_FINAL_V5_RQ5_REPO_ROOT",
		"TASKGATE_FINAL_V5_RQ5_RUN_ROOT",
		"TASKGATE_FINAL_V5_RQ5_SECRET_ROOT",
		"TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST",
		"TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256",
		"TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID",
		"TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID",
		"TASKGATE_FINAL_V5_RQ5_PROJECT",
	} {
		t.Setenv(name, "")
	}
	var stdout, stderr bytes.Buffer
	runDriver(bytes.NewReader(encodeRequest(t, request)), &stdout, &stderr)
	var response driverResponse
	if err := experiment.StrictJSON(bytes.TrimSpace(stdout.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "invalid" || response.ErrorCode != "rq5_driver_environment_invalid" ||
		response.SchemaVersion != 1 || response.DriverVersion != driverVersion {
		t.Fatalf("driver response changed while retaining cause: %#v", response)
	}
	if got := stderr.String(); !strings.Contains(got, "absolute RQ5 roots") {
		t.Fatalf("driver stderr omitted its underlying cause: %q", got)
	}
}

func TestDecodeRequestAcceptsOnlyFrozenCycle(t *testing.T) {
	request := validDriverRequest(t)
	decoded, err := decodeRequest(bytes.NewReader(encodeRequest(t, request)))
	if err != nil || decoded.CycleIndex != 1 || decoded.FromDay != "day3" || decoded.ToDay != "day0" {
		t.Fatalf("decodeRequest() = %#v, %v", decoded, err)
	}

	mutations := map[string]func(*driverRequest){
		"schema version":      func(value *driverRequest) { value.SchemaVersion = 2 },
		"driver version":      func(value *driverRequest) { value.DriverVersion = "wrong" },
		"fixture digest":      func(value *driverRequest) { value.FixtureSHA256 = "wrong" },
		"experiment":          func(value *driverRequest) { value.Operation.ExperimentID = "baseline" },
		"operation mode":      func(value *driverRequest) { value.Operation.Mode = rq5fixture.RetainedMode },
		"operation workload":  func(value *driverRequest) { value.Operation.WorkloadID = "other" },
		"operation scale":     func(value *driverRequest) { value.Operation.Scale = "2000" },
		"operation iteration": func(value *driverRequest) { value.Operation.Iteration = 0 },
		"cycle index":         func(value *driverRequest) { value.CycleIndex = 2 },
		"from day":            func(value *driverRequest) { value.FromDay = "day2" },
		"to day":              func(value *driverRequest) { value.ToDay = "day1" },
		"phase image":         func(value *driverRequest) { value.PhaseImageID = "sha256:" + strings.Repeat("a", 64) },
		"online image":        func(value *driverRequest) { value.OnlineImageID = "sha256:" + strings.Repeat("b", 64) },
		"OA image":            func(value *driverRequest) { value.OAImageID = "sha256:" + strings.Repeat("c", 64) },
		"phase binary":        func(value *driverRequest) { value.PhaseBinarySHA256 = strings.Repeat("d", 64) },
		"online binary":       func(value *driverRequest) { value.OnlineBinarySHA256 = strings.Repeat("e", 64) },
		"OA binary":           func(value *driverRequest) { value.OABinarySHA256 = strings.Repeat("f", 64) },
		"phase mtime":         func(value *driverRequest) { mtime := int64(0); value.PhaseBinaryMTime = &mtime },
		"online mtime":        func(value *driverRequest) { mtime := int64(0); value.OnlineBinaryMTime = &mtime },
		"OA mtime":            func(value *driverRequest) { mtime := int64(0); value.OABinaryMTime = &mtime },
		"manifest digest":     func(value *driverRequest) { value.BuildManifestSHA256 = strings.Repeat("A", 64) },
		"generator digest":    func(value *driverRequest) { value.GeneratorSHA256 = "wrong" },
		"config digest":       func(value *driverRequest) { value.ConfigSHA256 = "wrong" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value := request
			mutate(&value)
			if _, err := decodeRequest(bytes.NewReader(encodeRequest(t, value))); err == nil {
				t.Fatal("mutated request was accepted")
			}
		})
	}
	if _, err := decodeRequest(bytes.NewReader(append(encodeRequest(t, request), []byte(" {}")...))); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
	var object map[string]any
	if err := json.Unmarshal(encodeRequest(t, request), &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	unknown, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRequest(bytes.NewReader(unknown)); err == nil {
		t.Fatal("unknown driver request field was accepted")
	}
}

func TestLoadDriverStateRequiresBoundAbsoluteRoots(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "evaluation", "daily-publication-online", "compose.yaml")
	if err := os.MkdirAll(filepath.Dir(compose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(t.TempDir(), "deployment")
	secretRoot, err := os.MkdirTemp("/tmp", "taskgate-rq5-secrets.deployment-01.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secretRoot) })
	manifestPath := filepath.Join(t.TempDir(), "build-manifest.json")
	if err := os.WriteFile(manifestPath, []byte("manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	campaignID, deploymentID := "campaign", "deployment-01"
	projectPrefix := rq5DeploymentProjectPrefix(campaignID, deploymentID)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_REPO_ROOT", repo)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_RUN_ROOT", runRoot)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_SECRET_ROOT", secretRoot)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST", manifestPath)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256", fmt.Sprintf("%064x", 1))
	t.Setenv("TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID", campaignID)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID", deploymentID)
	t.Setenv("TASKGATE_FINAL_V5_RQ5_PROJECT", projectPrefix)
	state, err := loadDriverState()
	if err != nil {
		t.Fatal(err)
	}
	if state.repoRoot != repo || state.checkoutRoot != repo || state.runRoot != runRoot ||
		state.secretRoot != secretRoot || state.projectPrefix != projectPrefix ||
		state.fixtureProject != projectPrefix+"-fixture" || state.businessNetwork != projectPrefix+"-business" {
		t.Fatalf("unexpected state: %#v", state)
	}

	t.Setenv("TASKGATE_FINAL_V5_RQ5_PROJECT", rq5DeploymentProjectPrefix("internal-project-identity", deploymentID))
	if _, err := loadDriverState(); err == nil || !strings.Contains(err.Error(), "complete campaign/deployment identity") {
		t.Fatalf("project prefix derived from a non-campaign identity was accepted: %v", err)
	}
	t.Setenv("TASKGATE_FINAL_V5_RQ5_PROJECT", "unsafe/project")
	if _, err := loadDriverState(); err == nil {
		t.Fatal("unsafe Compose project was accepted")
	}
}

func TestFixtureCompletionIsStrictAndSourceBound(t *testing.T) {
	fixture := t.TempDir()
	state := driverState{}
	write := func(relative string, value any) {
		t.Helper()
		path := filepath.Join(fixture, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONExclusive(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("preparation.json", map[string]any{"status": "pass"})
	write("dataset-manifest.json", map[string]any{"rows": rq5fixture.RowsPerPublication})
	for _, day := range rq5fixture.Days {
		write(filepath.Join("approved-inputs", day+".json"), map[string]any{"day": day})
	}
	if state.fixtureIsComplete(fixture) {
		t.Fatal("fixture without final marker was accepted")
	}
	datasetSHA256, err := experiment.FileSHA256(filepath.Join(fixture, "dataset-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	write("fixture-complete.json", fixtureCompletion{SchemaVersion: 1, DriverVersion: driverVersion,
		FixtureSHA256: rq5fixture.FixtureSHA256(), DatasetManifestSHA256: datasetSHA256})
	if !state.fixtureIsComplete(fixture) {
		t.Fatal("complete fixture was rejected")
	}
	if err := os.Remove(filepath.Join(fixture, "approved-inputs", "day2.json")); err != nil {
		t.Fatal(err)
	}
	if state.fixtureIsComplete(fixture) {
		t.Fatal("fixture with missing approved input was accepted")
	}
}

func TestCycleWorkspaceParentCanBeCreatedOnce(t *testing.T) {
	runRoot := t.TempDir()
	cyclesRoot := filepath.Join(runRoot, "cycles")
	if err := os.MkdirAll(cyclesRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cycle := filepath.Join(cyclesRoot, "cycle-1")
	if err := os.Mkdir(cycle, 0o700); err != nil {
		t.Fatalf("first cycle workspace create: %v", err)
	}
	if err := os.Mkdir(cycle, 0o700); err == nil {
		t.Fatal("reused cycle workspace was accepted")
	}
}

func TestCycleProjectsAreCryptographicallyUniqueAndRequestScoped(t *testing.T) {
	state := driverState{projectPrefix: "rq5-unit-01", businessNetwork: "rq5-unit-01-business"}
	request := validDriverRequest(t)
	first, err := state.newCycleWorkspace(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.newCycleWorkspace(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Project == second.Project || first.GatewayContainer == second.GatewayContainer {
		t.Fatal("two requests reused a Compose project or callback container")
	}
	for _, workspace := range []cycleWorkspace{first, second} {
		if !safeProject.MatchString(workspace.Project) || !safeProject.MatchString(workspace.GatewayContainer) ||
			workspace.BusinessNetwork != state.businessNetwork {
			t.Fatalf("unsafe derived workspace: %#v", workspace)
		}
		environment := state.cycleEnvironment(workspace)
		want := "DAILY_RQ5_GATEWAY_CALLBACK_URL=http://" + workspace.GatewayContainer + ":8083/api/v1/oa/callback"
		if !containsEnvironment(environment, want) {
			t.Fatalf("cycle environment omitted %q", want)
		}
	}
}

func TestDeploymentProjectPrefixHashesCompleteIdentityWithoutTruncationCollision(t *testing.T) {
	helper := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "rq5-project-prefix.sh")
	derive := func(campaign, deployment string) string {
		t.Helper()
		output, err := exec.Command("bash", helper, campaign, deployment).CombinedOutput()
		if err != nil {
			t.Fatalf("derive prefix: %v (%s)", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	// These IDs collided under the former first-12-character slug scheme.
	first := derive("same-prefix-campaign-alpha", "deployment-01")
	second := derive("same-prefix-campaign-bravo", "deployment-01")
	if first == second {
		t.Fatalf("complete campaign identities collided at %q", first)
	}
	if first != derive("same-prefix-campaign-alpha", "deployment-01") {
		t.Fatal("project prefix derivation is not deterministic")
	}
	if first == derive("same-prefix-campaign-alpha", "deployment-02") {
		t.Fatal("deployment identity was omitted from the project prefix")
	}
	for _, prefix := range []string{first, second} {
		project := prefix + "-c4-0123456789ab"
		container := project + "-gateway-slot"
		if !safeProjectPrefix.MatchString(prefix) || !safeProject.MatchString(project) ||
			!safeProject.MatchString(container) || len(container) > 63 {
			t.Fatalf("derived identity exceeds Docker/driver bounds: %q / %q", project, container)
		}
	}
}

func TestDeploymentCleanupOwnsCycleFixtureAndExternalNetworkFamilies(t *testing.T) {
	directory := t.TempDir()
	runRoot := filepath.Join(directory, "run")
	cycleRoot := filepath.Join(runRoot, "cycles", "cycle-1")
	if err := os.MkdirAll(cycleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	prefix := "rq5-cleanup-unit"
	network := prefix + "-business"
	project := prefix + "-c1-0123456789ab"
	workspace := cycleWorkspace{SchemaVersion: 1, Project: project,
		GatewayContainer: project + "-gateway-slot", BusinessNetwork: network}
	if err := writeJSONExclusive(filepath.Join(cycleRoot, "cycle-workspace.json"), workspace, 0o600); err != nil {
		t.Fatal(err)
	}
	ownerBytes := sha256.Sum256([]byte(filepath.Clean(runRoot)))
	owner := hex.EncodeToString(ownerBytes[:])
	dockerLog := filepath.Join(directory, "docker.log")
	dockerPath := filepath.Join(directory, "docker")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> "$RQ5_TEST_DOCKER_LOG"
if [ "$1" = compose ]; then
  [ "$DAILY_RQ5_INSTALL_DSN" = "postgres://cleanup:cleanup@rq5-cleanup.invalid/cleanup?sslmode=disable" ] || exit 90
  exit 41
fi
if [ "$1" = ps ]; then
  [ -e "$RQ5_TEST_STATE/container.removed" ] || printf 'container-id\n'
  exit 0
fi
if [ "$1" = container ] && [ "$2" = rm ]; then
  : > "$RQ5_TEST_STATE/container.removed"
  exit 0
fi
if [ "$1" = volume ] && [ "$2" = ls ]; then
  [ -e "$RQ5_TEST_STATE/volume.removed" ] || printf 'volume-id\n'
  exit 0
fi
if [ "$1" = volume ] && [ "$2" = rm ]; then
  : > "$RQ5_TEST_STATE/volume.removed"
  exit 0
fi
if [ "$1" = network ] && [ "$2" = ls ]; then
  case "$*" in
    *name=*) [ -e "$RQ5_TEST_STATE/external-network.removed" ] || printf 'external-network-id\n' ;;
    *) [ -e "$RQ5_TEST_STATE/project-network.removed" ] || printf 'project-network-id\n' ;;
  esac
  exit 0
fi
if [ "$1" = network ] && [ "$2" = inspect ]; then
  [ -e "$RQ5_TEST_STATE/external-network.removed" ] && exit 1
  printf '%s\n'
  exit 0
fi
if [ "$1" = network ] && [ "$2" = rm ]; then
  if [ "$3" = "project-network-id" ]; then
    : > "$RQ5_TEST_STATE/project-network.removed"
  else
    : > "$RQ5_TEST_STATE/external-network.removed"
  fi
  exit 0
fi
exit 93
`, owner)
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RQ5_TEST_DOCKER_LOG", dockerLog)
	t.Setenv("RQ5_TEST_STATE", directory)
	state := driverState{repoRoot: directory, runRoot: runRoot, projectPrefix: prefix,
		fixtureProject: prefix + "-fixture", businessNetwork: network,
		composeFile: filepath.Join(directory, "compose.yaml"), composeEnv: os.Environ()}
	report, err := state.cleanupDeploymentResources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || report.Projects != 2 || report.FallbackProjects != 1 ||
		report.Before != (dockerResourceCounts{Containers: 1, Volumes: 1, Networks: 1}) ||
		report.After.total() != 0 || report.ExternalNetworksBefore != 1 || report.ExternalNetworksAfter != 0 {
		t.Fatalf("cleanup report = %#v", report)
	}
	logBytes, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	for _, required := range []string{
		"compose --project-name " + project,
		"container rm --force --volumes container-id",
		"volume rm volume-id",
		"network rm project-network-id",
		"network rm " + network,
	} {
		if !strings.Contains(log, required) {
			t.Fatalf("cleanup omitted %q:\n%s", required, log)
		}
	}
	if !state.ownsProject(state.fixtureProject) || state.ownsProject(prefix) ||
		state.ownsProject(prefix+"-c5-0123456789ab") || state.ownsProject(prefix+"-c1-short") {
		t.Fatal("cleanup project ownership accepted an incomplete resource-family name")
	}
}

func TestSecretRootCleanupFailsClosedAndRemovesOnlyExactTemporaryRoot(t *testing.T) {
	helper := filepath.Join("..", "..", "final-v5-wsl2", "scripts", "rq5-secret-root-cleanup.sh")
	root, err := os.MkdirTemp("/tmp", "taskgate-rq5-secrets.deployment-01.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	secret := filepath.Join(root, "deployment-secrets.json")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "rm-argv.log")
	fakeRM := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$RQ5_TEST_RM_LOG\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "rm"), []byte(fakeRM), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", helper, root)
	command.Env = replaceEnvironment(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RQ5_TEST_RM_LOG="+logPath)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("injected secret cleanup failure passed (%s)", output)
	}
	if !strings.Contains(string(output), "RQ5 secret root survived cleanup") {
		t.Fatalf("cleanup failed before checking its postcondition: %v (%s)", err, output)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("failed cleanup did not retain its target for diagnosis: %v", err)
	}
	if value, err := os.ReadFile(secret); err != nil || string(value) != "secret" {
		t.Fatalf("failed cleanup did not retain its secret: %q, %v", value, err)
	}
	argv, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := strings.Join([]string{"--recursive", "--force", "--one-file-system", "--", root, ""}, "\n")
	if string(argv) != wantArgv {
		t.Fatalf("secret cleanup rm argv = %q, want %q", argv, wantArgv)
	}
	if output, err := exec.Command("bash", helper, root).CombinedOutput(); err != nil {
		t.Fatalf("exact secret cleanup failed: %v (%s)", err, output)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret root survived successful cleanup: %v", err)
	}
	if output, err := exec.Command("bash", helper, t.TempDir()).CombinedOutput(); err == nil {
		t.Fatalf("unsafe cleanup target was accepted (%s)", output)
	}
}

func TestDeploymentSecretsAreRandomAndFullyBound(t *testing.T) {
	first := driverState{composeEnv: []string{"DAILY_CONTROL_PASSWORD=fixed"}}
	second := driverState{}
	if err := first.generateDeploymentSecrets(); err != nil {
		t.Fatal(err)
	}
	if err := second.generateDeploymentSecrets(); err != nil {
		t.Fatal(err)
	}
	if first.secrets.OACallbackSecret == second.secrets.OACallbackSecret ||
		first.secrets.OAReceiptPrivateKey == second.secrets.OAReceiptPrivateKey ||
		first.secrets.DataKey == second.secrets.DataKey {
		t.Fatal("independent deployments reused a cryptographic secret")
	}
	if err := first.bindSecrets(); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"DAILY_POSTGRES_PASSWORD=", "DAILY_GATEWAY_DB_PASSWORD=",
		"DAILY_RQ5_INSTALL_DSN=", "DAILY_CONTROL_PASSWORD=", "DAILY_RQ5_MINIO_ROOT_PASSWORD=",
		"DAILY_RQ5_OA_SERVICE_TOKEN=",
		"DAILY_RQ5_RECEIPT_PRIVATE_KEY=", "DAILY_RQ5_RESULT_DATA_KEY=",
		"DAILY_RQ5_OA_CALLBACK_SECRET=", "DAILY_RQ5_OA_SESSION_SECRET=",
		"DAILY_RQ5_OA_RECEIPT_PRIVATE_KEY=", "DAILY_RQ5_OA_RECEIPT_PUBLIC_KEY=",
		"DAILY_RQ5_OA_ALICE_PASSWORD=", "DAILY_RQ5_OA_BOB_PASSWORD="} {
		if !hasEnvironmentPrefix(first.composeEnv, prefix) {
			t.Fatalf("deployment environment omitted %s", prefix)
		}
	}
	if containsEnvironment(first.composeEnv, "DAILY_CONTROL_PASSWORD=fixed") {
		t.Fatal("fixed inherited Control password survived binding")
	}
}

func TestOnlineCycleSecretsUseComposeEnvironmentAndNeverDockerArgv(t *testing.T) {
	directory := t.TempDir()
	dockerLog := filepath.Join(directory, "docker-argv.log")
	dockerPath := filepath.Join(directory, "docker")
	script := `#!/bin/sh
[ "$DAILY_RQ5_RECEIPT_PRIVATE_KEY" = "$RQ5_TEST_WANT_RECEIPT" ] || exit 91
[ "$DAILY_RQ5_RESULT_DATA_KEY" = "$RQ5_TEST_WANT_DATA_KEY" ] || exit 92
printf '%s\n' "$*" > "$RQ5_TEST_DOCKER_LOG"
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	runRoot := filepath.Join(directory, "run")
	cycleDirectory := filepath.Join(runRoot, "cycles", "cycle-1")
	secretRoot := filepath.Join(directory, "secret")
	for _, path := range []string{cycleDirectory, secretRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	state := driverState{repoRoot: directory, runRoot: runRoot, secretRoot: secretRoot,
		composeFile: filepath.Join(directory, "compose.yaml")}
	if err := state.generateDeploymentSecrets(); err != nil {
		t.Fatal(err)
	}
	wantReceipt, wantDataKey := state.secrets.ReceiptPrivateKey, state.secrets.DataKey
	if err := writeJSONExclusive(filepath.Join(secretRoot, "deployment-secrets.json"), state.secrets, 0o600); err != nil {
		t.Fatal(err)
	}
	state.composeEnv = replaceEnvironment(os.Environ(), "RQ5_TEST_DOCKER_LOG="+dockerLog,
		"RQ5_TEST_WANT_RECEIPT="+wantReceipt, "RQ5_TEST_WANT_DATA_KEY="+wantDataKey)
	workspace := cycleWorkspace{Project: "rq5-secret-argv-c1-0123456789ab",
		GatewayContainer: "rq5-secret-argv-c1-0123456789ab-gateway-slot"}
	output := filepath.Join(cycleDirectory, "response.json")
	if err := state.runOnlineCycle(t.Context(), validDriverRequest(t), cycleDirectory, output,
		workspace, state.composeEnv); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{wantReceipt, wantDataKey,
		"RQ5_RECEIPT_PRIVATE_KEY=", "RQ5_RESULT_DATA_KEY="} {
		if strings.Contains(string(argv), forbidden) {
			t.Fatal("deployment secret or NAME=value binding reached Docker argv")
		}
	}
	for _, want := range []string{"DAILY_RQ5_RECEIPT_PRIVATE_KEY=" + wantReceipt,
		"DAILY_RQ5_RESULT_DATA_KEY=" + wantDataKey} {
		if !containsEnvironment(state.composeEnv, want) {
			t.Fatal("deployment secret was not bound through the Compose process environment")
		}
	}
	if !strings.Contains(string(argv), " online final-v5-cycle ") {
		t.Fatal("test did not exercise the online-cycle Compose invocation")
	}
}

func TestComposeMapsOnlineAndInstallerSecretsFromProcessEnvironment(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join("..", "..", "daily-publication-online", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []string{
		"RQ5_RECEIPT_PRIVATE_KEY: ${DAILY_RQ5_RECEIPT_PRIVATE_KEY:-}",
		"RQ5_RESULT_DATA_KEY: ${DAILY_RQ5_RESULT_DATA_KEY:-}",
		"SNAPSHOT_INSTALL_POSTGRES_DSN: ${DAILY_RQ5_INSTALL_DSN:?set deployment-bound installer DSN}",
	} {
		if !strings.Contains(string(compose), binding) {
			t.Fatalf("Compose omits process-environment binding %q", binding)
		}
	}
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		`"RQ5_RECEIPT_PRIVATE_KEY="+state.secrets.ReceiptPrivateKey`,
		`"RQ5_RESULT_DATA_KEY="+state.secrets.DataKey`,
		`"SNAPSHOT_INSTALL_POSTGRES_DSN="+installDSN`,
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("driver embeds a deployment secret in Docker argv through %s", forbidden)
		}
	}
}

func TestCycleCleanupFailureIsFailClosedAndRetainsEvidence(t *testing.T) {
	evidence := &experiment.RQ5VerificationEvidence{}
	response := driverResponse{
		SchemaVersion: 1,
		DriverVersion: driverVersion,
		Status:        "pass",
		Evidence:      evidence,
	}
	var returnErr error
	injected := errors.New("injected cleanup failure")
	applyFailClosedCycleCleanup(&response, &returnErr, injected)
	if response.Status != "fail" || response.ErrorCode != "rq5_cycle_cleanup_failed" {
		t.Fatalf("cleanup failure response = %#v", response)
	}
	if response.Evidence != evidence {
		t.Fatal("cleanup failure discarded the completed cycle evidence")
	}
	if !errors.Is(returnErr, injected) {
		t.Fatalf("cleanup failure was not returned: %v", returnErr)
	}

	unchanged := driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "pass"}
	returnErr = nil
	applyFailClosedCycleCleanup(&unchanged, &returnErr, nil)
	if unchanged.Status != "pass" || returnErr != nil {
		t.Fatalf("successful cleanup changed response: %#v, %v", unchanged, returnErr)
	}
}

func containsEnvironment(environment []string, want string) bool {
	for _, entry := range environment {
		if entry == want {
			return true
		}
	}
	return false
}

func hasEnvironmentPrefix(environment []string, prefix string) bool {
	for _, entry := range environment {
		if len(entry) > len(prefix) && entry[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
