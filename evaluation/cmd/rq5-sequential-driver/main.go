// rq5-sequential-driver is the source-built host orchestrator for the formal
// 345000-row Stage-B RQ5 adapter. It owns one isolated Compose project per
// deployment and emits only the strict adapter protocol on stdout.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

const driverVersion = "taskgate-final-v5-rq5-sequential-driver-v1"

const rq5DriverBuildCommand = "go build -buildvcs=false -trimpath -o rq5-sequential-driver ./evaluation/cmd/rq5-sequential-driver"

const (
	rq5RuntimeImageBuildProject = "rq5-source-sealed-build-v1"
	rq5SourceDateEpoch          = "0"
)

var (
	safeProjectPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
	safeProject       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	campaignIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	deploymentPattern = regexp.MustCompile(`^deployment-0[1-3]$`)
	imageIDPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type driverRequest struct {
	SchemaVersion       int                         `json:"schema_version"`
	DriverVersion       string                      `json:"driver_version"`
	FixtureSHA256       string                      `json:"fixture_sha256"`
	BuildManifestSHA256 string                      `json:"build_manifest_sha256"`
	Operation           experiment.AdapterOperation `json:"operation"`
	CycleIndex          int                         `json:"cycle_index"`
	FromDay             string                      `json:"from_day"`
	ToDay               string                      `json:"to_day"`
	GeneratorSHA256     string                      `json:"generator_sha256"`
	ConfigSHA256        string                      `json:"config_sha256"`
	PhaseImageID        string                      `json:"phase_image_id,omitempty"`
	OnlineImageID       string                      `json:"online_image_id,omitempty"`
	OAImageID           string                      `json:"oa_image_id,omitempty"`
	PhaseBinarySHA256   string                      `json:"phase_binary_sha256,omitempty"`
	OnlineBinarySHA256  string                      `json:"online_binary_sha256,omitempty"`
	OABinarySHA256      string                      `json:"oa_binary_sha256,omitempty"`
	PhaseBinaryMTime    *int64                      `json:"phase_binary_mtime_unix,omitempty"`
	OnlineBinaryMTime   *int64                      `json:"online_binary_mtime_unix,omitempty"`
	OABinaryMTime       *int64                      `json:"oa_binary_mtime_unix,omitempty"`
}

type driverResponse struct {
	SchemaVersion int                                 `json:"schema_version"`
	DriverVersion string                              `json:"driver_version"`
	Status        string                              `json:"status"`
	ErrorCode     string                              `json:"error_code,omitempty"`
	Evidence      *experiment.RQ5VerificationEvidence `json:"evidence,omitempty"`
}

type driverState struct {
	repoRoot             string
	checkoutRoot         string
	runRoot              string
	secretRoot           string
	buildManifestPath    string
	buildManifestSHA256  string
	expectedCampaignID   string
	expectedDeploymentID string
	projectPrefix        string
	fixtureProject       string
	businessNetwork      string
	composeFile          string
	composeEnv           []string
	secrets              deploymentSecrets
}

type deploymentSecrets struct {
	SchemaVersion           int    `json:"schema_version"`
	ReceiptPrivateKey       string `json:"receipt_private_key"`
	DataKey                 string `json:"data_key"`
	BusinessPassword        string `json:"business_password"`
	GatewayDatabasePassword string `json:"gateway_database_password"`
	ControlPassword         string `json:"control_password"`
	MinIOPassword           string `json:"minio_password"`
	DeliverySigningKey      string `json:"delivery_signing_key"`
	OAServiceToken          string `json:"oa_service_token"`
	OACallbackSecret        string `json:"oa_callback_secret"`
	OASessionSecret         string `json:"oa_session_secret"`
	OAReceiptPrivateKey     string `json:"oa_receipt_private_key"`
	AlicePassword           string `json:"alice_password"`
	BobPassword             string `json:"bob_password"`
}

type cycleWorkspace struct {
	SchemaVersion    int    `json:"schema_version"`
	Project          string `json:"project"`
	GatewayContainer string `json:"gateway_container"`
	BusinessNetwork  string `json:"business_network"`
}

type cycleCleanup struct {
	SchemaVersion int    `json:"schema_version"`
	Project       string `json:"project"`
	Status        string `json:"status"`
}

type fixtureCompletion struct {
	SchemaVersion         int    `json:"schema_version"`
	DriverVersion         string `json:"driver_version"`
	FixtureSHA256         string `json:"fixture_sha256"`
	DatasetManifestSHA256 string `json:"dataset_manifest_sha256"`
}

type sourceBuildManifest struct {
	SchemaVersion    int    `json:"schema_version"`
	SubmissionCommit string `json:"submission_commit"`
	BinarySHA256     string `json:"binary_sha256"`
	SourceSHA256     string `json:"source_sha256"`
	GoVersion        string `json:"go_version"`
	BuildCommand     string `json:"build_command"`
	SourceFiles      string `json:"source_files"`
}

type sourceManifestEntry struct {
	Path   string
	SHA256 string
}

type sourceSnapshotCompletion struct {
	SchemaVersion       int    `json:"schema_version"`
	BuildManifestSHA256 string `json:"build_manifest_sha256"`
	SourceSHA256        string `json:"source_sha256"`
}

type runtimeImageAttestation struct {
	SchemaVersion       int    `json:"schema_version"`
	BuildManifestSHA256 string `json:"build_manifest_sha256"`
	PhaseImageID        string `json:"phase_image_id"`
	OnlineImageID       string `json:"online_image_id"`
	OAImageID           string `json:"oa_image_id"`
	PhaseBinarySHA256   string `json:"phase_binary_sha256"`
	OnlineBinarySHA256  string `json:"online_binary_sha256"`
	OABinarySHA256      string `json:"oa_binary_sha256"`
	PhaseBinaryMTime    int64  `json:"phase_binary_mtime_unix"`
	OnlineBinaryMTime   int64  `json:"online_binary_mtime_unix"`
	OABinaryMTime       int64  `json:"oa_binary_mtime_unix"`
}

type phaseReport struct {
	SchemaVersion    string          `json:"schema_version"`
	Status           string          `json:"status"`
	Phase            string          `json:"phase"`
	Day              string          `json:"day"`
	Sample           int             `json:"sample"`
	Executable       string          `json:"executable"`
	ExecutableSHA256 string          `json:"executable_sha256"`
	ArgvSHA256       string          `json:"argv_sha256"`
	WallMS           float64         `json:"wall_ms"`
	PeakRSSBytes     *uint64         `json:"peak_rss_bytes"`
	PeakRSSScope     string          `json:"peak_rss_scope"`
	ExitCode         int             `json:"exit_code"`
	StdoutBytes      int             `json:"stdout_bytes"`
	StdoutSHA256     string          `json:"stdout_sha256"`
	StderrBytes      int             `json:"stderr_bytes"`
	StderrSHA256     string          `json:"stderr_sha256"`
	CommandReport    json.RawMessage `json:"command_report,omitempty"`
	Failure          string          `json:"failure,omitempty"`
	Measurement      string          `json:"measurement_boundary"`
}

type commandReport struct {
	VerificationReceiptSHA256 string `json:"verification_receipt_sha256"`
}

func main() {
	response := driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	request, err := decodeRequest(os.Stdin)
	if err == nil {
		response, err = run(ctx, request)
	}
	if err != nil && response.ErrorCode == "" {
		response.Status = "invalid"
		response.ErrorCode = "rq5_driver_environment_invalid"
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(response)
}

func decodeRequest(reader io.Reader) (driverRequest, error) {
	var request driverRequest
	decoder := json.NewDecoder(io.LimitReader(reader, 4<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return request, errors.New("driver request contains a trailing JSON value")
	}
	cycle, err := rq5fixture.LookupCycle(request.Operation.Iteration)
	if err != nil || request.SchemaVersion != 1 || request.DriverVersion != driverVersion ||
		request.FixtureSHA256 != rq5fixture.FixtureSHA256() || request.Operation.ExperimentID != "rq5" ||
		request.Operation.Mode != rq5fixture.BuildMode || request.CycleIndex != cycle.Index ||
		request.FromDay != cycle.From || request.ToDay != cycle.To ||
		request.PhaseImageID != "" || request.OnlineImageID != "" || request.OAImageID != "" ||
		request.PhaseBinarySHA256 != "" || request.OnlineBinarySHA256 != "" || request.OABinarySHA256 != "" ||
		request.PhaseBinaryMTime != nil || request.OnlineBinaryMTime != nil || request.OABinaryMTime != nil ||
		!sha256Pattern.MatchString(request.BuildManifestSHA256) ||
		!sha256Pattern.MatchString(request.GeneratorSHA256) || !sha256Pattern.MatchString(request.ConfigSHA256) ||
		!rq5fixture.IsCell(request.Operation.WorkloadID, request.Operation.Scale,
			request.Operation.Mode, request.Operation.Iteration) {
		return request, errors.New("driver request differs from the frozen RQ5 matrix")
	}
	return request, nil
}

func run(ctx context.Context, request driverRequest) (response driverResponse, returnErr error) {
	state, err := loadDriverState()
	if err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_driver_environment_invalid"}, err
	}
	lock, err := lockDeployment(state.runRoot)
	if err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_driver_lock_failed"}, err
	}
	defer unlockDeployment(lock)
	if request.Operation.CampaignID != state.expectedCampaignID ||
		request.Operation.DeploymentID != state.expectedDeploymentID ||
		request.BuildManifestSHA256 != state.buildManifestSHA256 {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_deployment_identity_mismatch"}, errors.New("RQ5 operation differs from its bound deployment")
	}
	if err := state.ensureSourceSnapshot(request.BuildManifestSHA256); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_runtime_source_binding_changed"}, err
	}
	if err := state.ensureFixture(ctx); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_fixture_preparation_failed"}, err
	}
	images, err := state.reAttestRuntimeImages(ctx, request.BuildManifestSHA256)
	if err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_runtime_image_binding_changed"}, err
	}
	request.PhaseImageID, request.OnlineImageID, request.OAImageID =
		images.PhaseImageID, images.OnlineImageID, images.OAImageID
	request.PhaseBinarySHA256, request.OnlineBinarySHA256, request.OABinarySHA256 =
		images.PhaseBinarySHA256, images.OnlineBinarySHA256, images.OABinarySHA256
	request.PhaseBinaryMTime, request.OnlineBinaryMTime, request.OABinaryMTime =
		&images.PhaseBinaryMTime, &images.OnlineBinaryMTime, &images.OABinaryMTime
	if err := state.ensureNoResidualCycleProjects(ctx); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_residual_cycle_project_detected"}, err
	}
	cyclesRoot := filepath.Join(state.runRoot, "cycles")
	if err := os.MkdirAll(cyclesRoot, 0o700); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_workspace_invalid"}, err
	}
	cycleDirectory := filepath.Join(cyclesRoot, fmt.Sprintf("cycle-%d", request.CycleIndex))
	if err := os.Mkdir(cycleDirectory, 0o700); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_workspace_reused"}, err
	}
	for _, directory := range []string{filepath.Join(cycleDirectory, "artifacts"), filepath.Join(cycleDirectory, "receipt")} {
		if err := os.Mkdir(directory, 0o750); err != nil {
			return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
				ErrorCode: "rq5_cycle_workspace_invalid"}, err
		}
	}
	requestPath := filepath.Join(cycleDirectory, "request.json")
	if err := writeJSONExclusive(requestPath, request, 0o600); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_request_persist_failed"}, err
	}
	if err := state.bindRuntimeSources(request, cycleDirectory); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_runtime_source_binding_changed"}, err
	}
	workspace, err := state.newCycleWorkspace(request)
	if err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_project_invalid"}, err
	}
	if absent, err := state.cycleProjectAbsent(ctx, workspace.Project); err != nil || !absent {
		if err == nil {
			err = errors.New("newly derived RQ5 cycle project already has Docker resources")
		}
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_cycle_project_collision"}, err
	}
	if err := writeJSONExclusive(filepath.Join(cycleDirectory, "cycle-workspace.json"), workspace, 0o600); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_project_persist_failed"}, err
	}
	cycleEnvironment := state.cycleEnvironment(workspace)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cleanupErr := state.cleanupCycleProject(cleanupCtx, workspace, cycleEnvironment)
		status := "pass"
		if cleanupErr != nil {
			status = "fail"
		}
		markerErr := writeJSONExclusive(filepath.Join(cycleDirectory, "cycle-cleanup.json"), cycleCleanup{
			SchemaVersion: 1, Project: workspace.Project, Status: status}, 0o600)
		applyFailClosedCycleCleanup(&response, &returnErr, errors.Join(cleanupErr, markerErr))
	}()
	if err := state.ensureCycleStack(ctx, workspace, cycleEnvironment); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_cycle_stack_failed"}, err
	}
	if _, err := state.reAttestRuntimeImages(ctx, request.BuildManifestSHA256); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_runtime_image_binding_changed"}, err
	}
	if err := state.runMeasuredPhases(ctx, request, cycleDirectory, workspace.Project, cycleEnvironment); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_target_phase_failed"}, err
	}
	if _, err := state.reAttestRuntimeImages(ctx, request.BuildManifestSHA256); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_runtime_image_binding_changed"}, err
	}
	responsePath := filepath.Join(cycleDirectory, "driver-response.json")
	if err := state.runOnlineCycle(ctx, request, cycleDirectory, responsePath, workspace, cycleEnvironment); err != nil {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "fail",
			ErrorCode: "rq5_online_cycle_process_failed"}, err
	}
	response = driverResponse{}
	if err := decodeJSONFile(responsePath, &response); err != nil || response.SchemaVersion != 1 ||
		response.DriverVersion != driverVersion || (response.Status != "pass" && response.Status != "fail" && response.Status != "invalid") {
		return driverResponse{SchemaVersion: 1, DriverVersion: driverVersion, Status: "invalid",
			ErrorCode: "rq5_online_cycle_protocol_invalid"}, errors.New("online cycle response is invalid")
	}
	return response, nil
}

func loadDriverState() (driverState, error) {
	repoRoot := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_REPO_ROOT"))
	runRoot := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_RUN_ROOT"))
	secretRoot := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_SECRET_ROOT"))
	buildManifestPath := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST"))
	buildManifestSHA256 := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256"))
	expectedCampaignID := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_EXPECTED_CAMPAIGN_ID"))
	expectedDeploymentID := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_EXPECTED_DEPLOYMENT_ID"))
	projectPrefix := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_RQ5_PROJECT"))
	if !filepath.IsAbs(repoRoot) || !filepath.IsAbs(runRoot) || !filepath.IsAbs(secretRoot) ||
		!filepath.IsAbs(buildManifestPath) || !sha256Pattern.MatchString(buildManifestSHA256) ||
		!campaignIDPattern.MatchString(expectedCampaignID) || !deploymentPattern.MatchString(expectedDeploymentID) ||
		!safeProjectPrefix.MatchString(projectPrefix) {
		return driverState{}, errors.New("absolute RQ5 roots, manifest/deployment bindings, and safe project prefix are required")
	}
	for _, path := range []string{repoRoot, runRoot, secretRoot} {
		clean := filepath.Clean(path)
		if clean == string(filepath.Separator) || clean == "." {
			return driverState{}, errors.New("RQ5 roots must not be broad filesystem roots")
		}
	}
	secretBasePattern := regexp.MustCompile(`^taskgate-rq5-secrets\.deployment-0[1-3]\.[A-Za-z0-9]+$`)
	if filepath.Clean(filepath.Dir(secretRoot)) != "/tmp" || !secretBasePattern.MatchString(filepath.Base(secretRoot)) ||
		pathsOverlap(runRoot, secretRoot) || pathsOverlap(repoRoot, secretRoot) {
		return driverState{}, errors.New("RQ5 secret root must be an isolated exact /tmp deployment directory")
	}
	wantProjectPrefix := rq5DeploymentProjectPrefix(expectedCampaignID, expectedDeploymentID)
	if projectPrefix != wantProjectPrefix {
		return driverState{}, errors.New("RQ5 project prefix does not bind the complete campaign/deployment identity")
	}
	composeFile := filepath.Join(repoRoot, "evaluation", "daily-publication-online", "compose.yaml")
	if info, err := os.Lstat(composeFile); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return driverState{}, errors.New("source-controlled RQ5 Compose file is absent")
	}
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		return driverState{}, err
	}
	secretInfo, err := os.Lstat(secretRoot)
	if err != nil || !secretInfo.IsDir() || secretInfo.Mode()&os.ModeSymlink != 0 || secretInfo.Mode().Perm() != 0o700 {
		return driverState{}, errors.New("RQ5 secret root must be an existing mode-0700 non-symlink directory")
	}
	if stat, ok := secretInfo.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return driverState{}, errors.New("RQ5 secret root is not owned by the driver user")
	}
	state := driverState{repoRoot: repoRoot, checkoutRoot: repoRoot, runRoot: runRoot, secretRoot: secretRoot,
		buildManifestPath: buildManifestPath, buildManifestSHA256: buildManifestSHA256,
		expectedCampaignID: expectedCampaignID, expectedDeploymentID: expectedDeploymentID,
		projectPrefix:  projectPrefix,
		fixtureProject: projectPrefix + "-fixture", businessNetwork: projectPrefix + "-business",
		composeFile: composeFile}
	state.composeEnv = replaceEnvironment(os.Environ(),
		"DAILY_PUBLICATION_ROWS=345000",
		"DAILY_PUBLICATION_PHASE_IMAGE="+projectPrefix+"-phase",
		"DAILY_PUBLICATION_ONLINE_IMAGE="+projectPrefix+"-tool",
		"DAILY_RQ5_OA_IMAGE="+projectPrefix+"-tool",
		"DAILY_RQ5_BUSINESS_NETWORK="+state.businessNetwork)
	return state, nil
}

func pathsOverlap(first, second string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return contains(first, second) || contains(second, first)
}

func rq5DeploymentProjectPrefix(campaignID, deploymentID string) string {
	digest := sha256.Sum256([]byte(campaignID + "\x00" + deploymentID))
	return "rq5-" + hex.EncodeToString(digest[:])[:20]
}

func (state *driverState) ensureSourceSnapshot(expectedManifestSHA256 string) error {
	manifest, entries, err := loadRQ5SourceBuildManifest(state.buildManifestPath, expectedManifestSHA256)
	if err != nil {
		return err
	}
	snapshot := filepath.Join(state.secretRoot, "source-snapshot")
	marker := sourceSnapshotCompletion{SchemaVersion: 1, BuildManifestSHA256: expectedManifestSHA256,
		SourceSHA256: manifest.SourceSHA256}
	if _, err := os.Lstat(snapshot); errors.Is(err, os.ErrNotExist) {
		temporary, err := os.MkdirTemp(state.secretRoot, ".source-snapshot-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temporary)
		for _, entry := range entries {
			source := filepath.Join(state.checkoutRoot, filepath.FromSlash(entry.Path))
			target := filepath.Join(temporary, filepath.FromSlash(entry.Path))
			if err := copyManifestSource(source, target, entry.SHA256); err != nil {
				return fmt.Errorf("snapshot RQ5 source %s: %w", entry.Path, err)
			}
		}
		if err := writeJSONExclusive(filepath.Join(temporary, ".rq5-source-snapshot.json"), marker, 0o400); err != nil {
			return err
		}
		if err := normalizeSourceSnapshotTimestamps(temporary); err != nil {
			return err
		}
		if err := os.Rename(temporary, snapshot); err != nil {
			return err
		}
	}
	if err := verifyRQ5SourceSnapshot(snapshot, marker, entries); err != nil {
		return err
	}
	state.repoRoot = snapshot
	state.composeFile = filepath.Join(snapshot, "evaluation", "daily-publication-online", "compose.yaml")
	return nil
}

func normalizeSourceSnapshotTimestamps(root string) error {
	epoch := time.Unix(0, 0).UTC()
	return filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chtimes(path, epoch, epoch)
	})
}

func loadRQ5SourceBuildManifest(path, expectedSHA256 string) (sourceBuildManifest, []sourceManifestEntry, error) {
	var manifest sourceBuildManifest
	value, _, err := readBoundRegularFile(path, 8<<20)
	if err != nil {
		return manifest, nil, err
	}
	digest := sha256.Sum256(value)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return manifest, nil, errors.New("RQ5 build manifest differs from its deployment digest")
	}
	if err := experiment.StrictJSON(value, &manifest); err != nil {
		return manifest, nil, err
	}
	sourceDigest := sha256.Sum256([]byte(manifest.SourceFiles))
	if manifest.SchemaVersion != 1 || !commitPattern.MatchString(manifest.SubmissionCommit) ||
		!sha256Pattern.MatchString(manifest.BinarySHA256) || !sha256Pattern.MatchString(manifest.SourceSHA256) ||
		hex.EncodeToString(sourceDigest[:]) != manifest.SourceSHA256 || manifest.GoVersion == "" ||
		manifest.BuildCommand != rq5DriverBuildCommand || manifest.SourceFiles == "" {
		return manifest, nil, errors.New("RQ5 build manifest identity is invalid")
	}
	lines := strings.Split(manifest.SourceFiles, "\n")
	entries := make([]sourceManifestEntry, 0, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		digest, path, found := strings.Cut(line, "  ")
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if !found || !sha256Pattern.MatchString(digest) || path == "" || filepath.IsAbs(path) ||
			clean != path || clean == "." || strings.HasPrefix(clean, "../") || seen[path] {
			return manifest, nil, errors.New("RQ5 build manifest contains an unsafe or duplicate source entry")
		}
		seen[path] = true
		entries = append(entries, sourceManifestEntry{Path: path, SHA256: digest})
	}
	return manifest, entries, nil
}

func copyManifestSource(source, target, expectedSHA256 string) error {
	value, mode, err := readBoundRegularFile(source, 16<<20)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(value)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return errors.New("source differs from its sealed build-manifest entry")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if _, err = output.Write(value); err == nil {
		err = output.Sync()
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Chmod(target, mode.Perm()&^0o222)
}

func verifyRQ5SourceSnapshot(root string, marker sourceSnapshotCompletion, entries []sourceManifestEntry) error {
	var observed sourceSnapshotCompletion
	if err := decodeJSONFile(filepath.Join(root, ".rq5-source-snapshot.json"), &observed); err != nil || observed != marker {
		return errors.New("RQ5 source snapshot completion marker is invalid")
	}
	expected := make(map[string]string, len(entries)+1)
	expected[".rq5-source-snapshot.json"] = "marker"
	for _, entry := range entries {
		expected[entry.Path] = entry.SHA256
		value, _, err := readBoundRegularFile(filepath.Join(root, filepath.FromSlash(entry.Path)), 16<<20)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(value)
		if hex.EncodeToString(digest[:]) != entry.SHA256 {
			return fmt.Errorf("RQ5 source snapshot changed: %s", entry.Path)
		}
	}
	seen := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("RQ5 source snapshot contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, ok := expected[relative]; !ok {
			return fmt.Errorf("RQ5 source snapshot contains unexpected file %s", relative)
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(expected) {
		return errors.New("RQ5 source snapshot file set is incomplete")
	}
	return nil
}

func readBoundRegularFile(path string, maximum int64) ([]byte, os.FileMode, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errors.New("bound file is absent, non-regular, or a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, 0, errors.New("bound file changed while opening")
	}
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(value)) > maximum {
		return nil, 0, errors.New("bound file exceeded its byte limit")
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return nil, 0, errors.New("bound file changed while reading")
	}
	return value, before.Mode(), nil
}

func lockDeployment(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, ".driver.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func unlockDeployment(file *os.File) {
	if file != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}

func (state *driverState) composeProject(ctx context.Context, project string, environment []string,
	arguments ...string) ([]byte, error) {
	if !safeProject.MatchString(project) {
		return nil, errors.New("unsafe RQ5 Compose project")
	}
	args := []string{"compose", "--project-name", project, "--file", state.composeFile}
	args = append(args, arguments...)
	command := exec.CommandContext(ctx, "docker", args...)
	command.Dir = state.repoRoot
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if stdout.Len() > 64<<20 || stderr.Len() > 8<<20 {
		return nil, errors.New("RQ5 Compose command output exceeded its bound")
	}
	if err != nil {
		return nil, fmt.Errorf("RQ5 Compose command failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func (state *driverState) fixtureCompose(ctx context.Context, arguments ...string) ([]byte, error) {
	return state.composeProject(ctx, state.fixtureProject, state.composeEnv, arguments...)
}

func (state *driverState) ensureFixtureStack(ctx context.Context, build bool) error {
	if err := state.ensureBusinessNetwork(ctx); err != nil {
		return err
	}
	if build {
		if _, err := state.composeProject(ctx, rq5RuntimeImageBuildProject, state.composeEnv,
			"build", "--provenance=false", "--build-arg", "SOURCE_DATE_EPOCH="+rq5SourceDateEpoch,
			"phase", "online"); err != nil {
			return err
		}
	}
	if _, err := state.fixtureCompose(ctx, "up", "--detach", "--wait", "business-postgres"); err != nil {
		return err
	}
	return nil
}

func (state *driverState) ensureCycleStack(ctx context.Context, workspace cycleWorkspace,
	environment []string) error {
	if _, err := state.composeProject(ctx, workspace.Project, environment, "up", "--detach", "--wait",
		"--no-build", "--pull", "missing", "control-postgres", "result-object-store", "oa-demo"); err != nil {
		return err
	}
	_, err := state.composeProject(ctx, workspace.Project, environment, "up", "--no-deps", "--no-build",
		"--pull", "missing", "result-object-store-init")
	return err
}

func (state *driverState) reAttestRuntimeImages(ctx context.Context,
	buildManifestSHA256 string) (runtimeImageAttestation, error) {
	attestation, err := state.inspectRuntimeImages(ctx, buildManifestSHA256)
	if err != nil {
		return attestation, err
	}
	path := filepath.Join(state.runRoot, "fixture", "runtime-images.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err := writeJSONExclusive(path, attestation, 0o600); err != nil {
			return attestation, err
		}
	} else if err != nil {
		return attestation, err
	} else {
		var locked runtimeImageAttestation
		if err := decodeJSONFile(path, &locked); err != nil || locked != attestation {
			return attestation, errors.New("RQ5 runtime image identity changed after fixture build")
		}
	}
	state.bindRuntimeImageIDs(attestation)
	return attestation, nil
}

func (state *driverState) inspectRuntimeImages(ctx context.Context,
	buildManifestSHA256 string) (runtimeImageAttestation, error) {
	attestation := runtimeImageAttestation{SchemaVersion: 1, BuildManifestSHA256: buildManifestSHA256}
	images := []struct {
		tag    string
		target *string
	}{
		{state.projectPrefix + "-phase", &attestation.PhaseImageID},
		{state.projectPrefix + "-tool", &attestation.OnlineImageID},
		// oa-demo intentionally executes the same source-built runtime image as
		// online; its independently recorded field proves that Compose did not
		// route it to a historical/demo image.
		{state.projectPrefix + "-tool", &attestation.OAImageID},
	}
	for _, image := range images {
		output, err := runCommand(ctx, state.repoRoot, os.Environ(), "docker", "image", "inspect",
			"--format", "{{.Id}}", image.tag)
		if err != nil {
			return attestation, err
		}
		*image.target = strings.TrimSpace(string(output))
		if !imageIDPattern.MatchString(*image.target) {
			return attestation, errors.New("RQ5 runtime image has no content-addressed Docker image ID")
		}
	}
	binaries := []struct {
		imageID string
		path    string
		digest  *string
		mtime   *int64
	}{
		{attestation.PhaseImageID, "/usr/local/bin/daily-publication-phase",
			&attestation.PhaseBinarySHA256, &attestation.PhaseBinaryMTime},
		{attestation.OnlineImageID, "/usr/local/bin/rq5-online-transition",
			&attestation.OnlineBinarySHA256, &attestation.OnlineBinaryMTime},
		{attestation.OAImageID, "/usr/local/bin/oa-demo",
			&attestation.OABinarySHA256, &attestation.OABinaryMTime},
	}
	for _, binary := range binaries {
		digest, mtime, err := state.inspectRuntimeBinary(ctx, binary.imageID, binary.path)
		if err != nil {
			return attestation, err
		}
		*binary.digest, *binary.mtime = digest, mtime
	}
	return attestation, nil
}

func (state *driverState) inspectRuntimeBinary(ctx context.Context, imageID, binaryPath string) (string, int64, error) {
	if !imageIDPattern.MatchString(imageID) || !strings.HasPrefix(binaryPath, "/usr/local/bin/") ||
		strings.Contains(strings.TrimPrefix(binaryPath, "/usr/local/bin/"), "/") {
		return "", 0, errors.New("unsafe RQ5 runtime binary attestation target")
	}
	output, err := runCommand(ctx, state.repoRoot, os.Environ(), "docker", "run", "--rm", "--network", "none",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pull", "never",
		"--entrypoint", "/bin/sh", imageID, "-ec",
		`/usr/bin/sha256sum "$1"; /usr/bin/stat --format=%Y "$1"`, "rq5-runtime-attestation", binaryPath)
	if err != nil {
		return "", 0, err
	}
	return parseRuntimeBinaryAttestation(output, binaryPath)
}

func parseRuntimeBinaryAttestation(output []byte, binaryPath string) (string, int64, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	fields := []string(nil)
	if len(lines) > 0 {
		fields = strings.Fields(lines[0])
	}
	if len(lines) != 2 || len(fields) != 2 || fields[1] != binaryPath || !sha256Pattern.MatchString(fields[0]) {
		return "", 0, errors.New("RQ5 runtime binary digest attestation is invalid")
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return "", 0, errors.New("RQ5 runtime binary mtime attestation is invalid")
	}
	expectedMTime, _ := strconv.ParseInt(rq5SourceDateEpoch, 10, 64)
	if mtime != expectedMTime {
		return "", 0, errors.New("RQ5 runtime binary mtime differs from SOURCE_DATE_EPOCH")
	}
	return fields[0], mtime, nil
}

func (state *driverState) bindRuntimeImageIDs(attestation runtimeImageAttestation) {
	state.composeEnv = replaceEnvironment(state.composeEnv,
		"DAILY_PUBLICATION_PHASE_IMAGE="+attestation.PhaseImageID,
		"DAILY_PUBLICATION_ONLINE_IMAGE="+attestation.OnlineImageID,
		"DAILY_RQ5_OA_IMAGE="+attestation.OAImageID)
}

func (state *driverState) cleanupCycleProject(ctx context.Context, workspace cycleWorkspace,
	environment []string) error {
	_, downErr := state.composeProject(ctx, workspace.Project, environment,
		"down", "--volumes", "--remove-orphans")
	absent, probeErr := state.cycleProjectAbsent(ctx, workspace.Project)
	var residualErr error
	if probeErr == nil && !absent {
		residualErr = errors.New("RQ5 cycle project still has Docker resources after cleanup")
	}
	return errors.Join(downErr, probeErr, residualErr)
}

func applyFailClosedCycleCleanup(response *driverResponse, returnErr *error, cleanupErr error) {
	if cleanupErr == nil {
		return
	}
	if response.SchemaVersion == 0 {
		response.SchemaVersion = 1
	}
	if response.DriverVersion == "" {
		response.DriverVersion = driverVersion
	}
	response.Status = "fail"
	response.ErrorCode = "rq5_cycle_cleanup_failed"
	*returnErr = errors.Join(*returnErr, cleanupErr)
}

func (state *driverState) cycleProjectAbsent(ctx context.Context, project string) (bool, error) {
	if !safeProject.MatchString(project) || !strings.HasPrefix(project, state.projectPrefix+"-c") {
		return false, errors.New("RQ5 cycle project is outside this deployment prefix")
	}
	queries := [][]string{
		{"ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project=" + project},
		{"volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project=" + project},
		{"network", "ls", "--quiet", "--filter", "label=com.docker.compose.project=" + project},
	}
	for _, arguments := range queries {
		output, err := runCommand(ctx, state.repoRoot, os.Environ(), "docker", arguments...)
		if err != nil {
			return false, err
		}
		if len(bytes.TrimSpace(output)) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (state *driverState) ensureNoResidualCycleProjects(ctx context.Context) error {
	cyclesRoot := filepath.Join(state.runRoot, "cycles")
	entries, err := os.ReadDir(cyclesRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		workspacePath := filepath.Join(cyclesRoot, entry.Name(), "cycle-workspace.json")
		info, statErr := os.Lstat(workspacePath)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recorded RQ5 cycle workspace is not a regular file")
		}
		var workspace cycleWorkspace
		if err := decodeJSONFile(workspacePath, &workspace); err != nil {
			return err
		}
		if workspace.SchemaVersion != 1 || !safeProject.MatchString(workspace.Project) ||
			!strings.HasPrefix(workspace.Project, state.projectPrefix+"-c") ||
			workspace.GatewayContainer != workspace.Project+"-gateway-slot" ||
			!safeProject.MatchString(workspace.GatewayContainer) ||
			workspace.BusinessNetwork != state.businessNetwork {
			return errors.New("recorded RQ5 cycle workspace is outside this deployment")
		}
		absent, err := state.cycleProjectAbsent(ctx, workspace.Project)
		if err != nil {
			return err
		}
		if !absent {
			return fmt.Errorf("recorded RQ5 cycle project %q still has Docker resources", workspace.Project)
		}
	}
	return nil
}

func (state *driverState) ensureBusinessNetwork(ctx context.Context) error {
	ownerBytes := sha256.Sum256([]byte(filepath.Clean(state.runRoot)))
	owner := hex.EncodeToString(ownerBytes[:])
	inspect := exec.CommandContext(ctx, "docker", "network", "inspect", state.businessNetwork,
		"--format", `{{ index .Labels "taskgate.rq5.owner" }}`)
	inspect.Dir = state.repoRoot
	output, err := inspect.Output()
	if err == nil {
		if strings.TrimSpace(string(output)) != owner {
			return errors.New("RQ5 Business network exists with another deployment owner")
		}
		return nil
	}
	command := exec.CommandContext(ctx, "docker", "network", "create", "--internal",
		"--label", "taskgate.rq5.owner="+owner, state.businessNetwork)
	command.Dir = state.repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("create isolated RQ5 Business network: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (state *driverState) newCycleWorkspace(request driverRequest) (cycleWorkspace, error) {
	randomSuffix := make([]byte, 6)
	if _, err := rand.Read(randomSuffix); err != nil {
		return cycleWorkspace{}, err
	}
	project := fmt.Sprintf("%s-c%d-%s", state.projectPrefix, request.CycleIndex, hex.EncodeToString(randomSuffix))
	gatewayContainer := project + "-gateway-slot"
	if !safeProject.MatchString(project) || !safeProject.MatchString(gatewayContainer) {
		return cycleWorkspace{}, errors.New("derived RQ5 cycle project is unsafe")
	}
	return cycleWorkspace{SchemaVersion: 1, Project: project, GatewayContainer: gatewayContainer,
		BusinessNetwork: state.businessNetwork}, nil
}

func (state *driverState) cycleEnvironment(workspace cycleWorkspace) []string {
	return replaceEnvironment(state.composeEnv,
		"DAILY_RQ5_GATEWAY_CALLBACK_URL=http://"+workspace.GatewayContainer+":8083/api/v1/oa/callback")
}

func (state *driverState) bindRuntimeSources(request driverRequest, cycleDirectory string) error {
	boundDirectory := filepath.Join(cycleDirectory, "bound-sources")
	if err := os.Mkdir(boundDirectory, 0o700); err != nil {
		return err
	}
	sources := []struct {
		source, target, expected string
	}{
		{filepath.Join(state.repoRoot, "evaluation", "daily-publication", "sql", "05-generate-daily-data.sh"),
			filepath.Join(boundDirectory, "05-generate-daily-data.sh"), request.GeneratorSHA256},
		{filepath.Join(state.repoRoot, "evaluation", "daily-publication", "config.json"),
			filepath.Join(boundDirectory, "config.json"), request.ConfigSHA256},
	}
	for _, source := range sources {
		if err := copyBoundRuntimeSource(source.source, source.target, source.expected); err != nil {
			return err
		}
	}
	return nil
}

func copyBoundRuntimeSource(source, target, expectedSHA256 string) error {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("RQ5 runtime source is absent, non-regular, or a symlink")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("RQ5 runtime source changed while opening")
	}
	value, err := io.ReadAll(io.LimitReader(file, int64(8<<20)+1))
	if err != nil {
		return err
	}
	if len(value) > 8<<20 {
		return errors.New("RQ5 runtime source exceeded its byte bound")
	}
	after, err := os.Lstat(source)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) {
		return errors.New("RQ5 runtime source changed while hashing")
	}
	digest := sha256.Sum256(value)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return errors.New("RQ5 runtime source differs from its build-manifest digest")
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = output.Write(value); err == nil {
		err = output.Sync()
	}
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (state *driverState) ensureFixture(ctx context.Context) error {
	fixture := filepath.Join(state.runRoot, "fixture")
	if state.fixtureIsComplete(fixture) {
		if err := state.loadSecrets(); err != nil {
			return err
		}
		return state.ensureFixtureStack(ctx, false)
	}
	if err := state.generateDeploymentSecrets(); err != nil {
		return err
	}
	if err := state.bindSecrets(); err != nil {
		return err
	}
	if err := state.ensureFixtureStack(ctx, true); err != nil {
		return err
	}
	images, err := state.inspectRuntimeImages(ctx, state.buildManifestSHA256)
	if err != nil {
		return err
	}
	state.bindRuntimeImageIDs(images)
	temporary, err := os.MkdirTemp(state.runRoot, ".fixture-preparing-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	config := filepath.Join(state.repoRoot, "evaluation", "daily-publication", "config.json")
	harness := filepath.Join(state.repoRoot, "evaluation", "daily-publication", "harness.py")
	if _, err := runCommand(ctx, state.repoRoot, state.composeEnv, "python3", harness, "render-inputs",
		"--config", config, "--rows", "345000", "--output-dir", filepath.Join(temporary, "candidate-inputs")); err != nil {
		return err
	}
	dataset, err := state.fixtureCompose(ctx, "exec", "-T", "business-postgres", "psql", "--username", "postgres",
		"--dbname", "taskgate_daily", "--no-psqlrc", "--quiet", "--tuples-only", "--no-align",
		"--file", "/evaluation/dataset-manifest.sql")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "dataset-manifest.json"), bytes.TrimSpace(dataset), 0o600); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	if _, err := state.fixtureCompose(ctx, "run", "--rm", "--pull", "never", "--no-deps", "--user", uid,
		"--volume", temporary+":/evidence:rw", "preparer", "prepare",
		"-input-dir", "/evidence/candidate-inputs", "-approved-dir", "/evidence/approved-inputs",
		"-artifact-dir", "/evidence/artifacts", "-calibration-dir", "/evidence/calibration",
		"-manifest", "/evidence/preparation.json"); err != nil {
		return err
	}
	for _, day := range []string{"day0", "day1", "day2", "day3"} {
		installDSN := "postgres://postgres:" + state.secrets.BusinessPassword +
			"@business-postgres:5432/taskgate_daily_" + day + "?sslmode=disable"
		installEnvironment := replaceEnvironment(state.composeEnv, "DAILY_RQ5_INSTALL_DSN="+installDSN)
		if _, err := state.composeProject(ctx, state.fixtureProject, installEnvironment,
			"run", "--rm", "--pull", "never", "--no-deps", "--user", uid,
			"--volume", temporary+":/evidence:ro", "installer", "-artifact-dir", "/evidence/artifacts",
			"-input", "/evidence/approved-inputs/"+day+".json"); err != nil {
			return err
		}
	}
	datasetManifestSHA256, err := experiment.FileSHA256(filepath.Join(temporary, "dataset-manifest.json"))
	if err != nil {
		return err
	}
	completion := fixtureCompletion{SchemaVersion: 1, DriverVersion: driverVersion,
		FixtureSHA256: rq5fixture.FixtureSHA256(), DatasetManifestSHA256: datasetManifestSHA256}
	// The marker is deliberately written last. A fixture without this exact
	// source-bound marker is never reused as formal evidence.
	if err := writeJSONExclusive(filepath.Join(temporary, "fixture-complete.json"), completion, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, fixture); err != nil {
		return err
	}
	return writeJSONExclusive(filepath.Join(state.secretRoot, "deployment-secrets.json"), state.secrets, 0o600)
}

func (state *driverState) fixtureIsComplete(fixture string) bool {
	markerPath := filepath.Join(fixture, "fixture-complete.json")
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	var marker fixtureCompletion
	if decodeJSONFile(markerPath, &marker) != nil || marker.SchemaVersion != 1 ||
		marker.DriverVersion != driverVersion || marker.FixtureSHA256 != rq5fixture.FixtureSHA256() ||
		!sha256Pattern.MatchString(marker.DatasetManifestSHA256) {
		return false
	}
	required := []string{"preparation.json", "dataset-manifest.json"}
	for _, day := range rq5fixture.Days {
		required = append(required, filepath.Join("approved-inputs", day+".json"))
	}
	for _, relative := range required {
		entry, entryErr := os.Lstat(filepath.Join(fixture, relative))
		if entryErr != nil || !entry.Mode().IsRegular() || entry.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	datasetSHA256, err := experiment.FileSHA256(filepath.Join(fixture, "dataset-manifest.json"))
	if err != nil || datasetSHA256 != marker.DatasetManifestSHA256 {
		return false
	}
	return true
}

func (state *driverState) loadSecrets() error {
	if err := decodeJSONFile(filepath.Join(state.secretRoot, "deployment-secrets.json"), &state.secrets); err != nil {
		return err
	}
	return state.bindSecrets()
}

func (state *driverState) generateDeploymentSecrets() error {
	_, queryPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	_, oaPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	randomValue := func(size int) (string, error) {
		value := make([]byte, size)
		if _, err := rand.Read(value); err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(value), nil
	}
	values := make([]string, 10)
	for index := range values {
		values[index], err = randomValue(32)
		if err != nil {
			return err
		}
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return err
	}
	state.secrets = deploymentSecrets{
		SchemaVersion: 1, ReceiptPrivateKey: base64.StdEncoding.EncodeToString(queryPrivateKey),
		DataKey: base64.StdEncoding.EncodeToString(dataKey), BusinessPassword: values[0],
		GatewayDatabasePassword: values[1], ControlPassword: values[2], MinIOPassword: values[3],
		DeliverySigningKey: values[4], OAServiceToken: values[5], OACallbackSecret: values[6],
		OASessionSecret: values[7], OAReceiptPrivateKey: base64.StdEncoding.EncodeToString(oaPrivateKey),
		AlicePassword: values[8], BobPassword: values[9],
	}
	return nil
}

func (state *driverState) bindSecrets() error {
	queryPrivateKey, queryErr := base64.StdEncoding.DecodeString(state.secrets.ReceiptPrivateKey)
	dataKey, dataErr := base64.StdEncoding.DecodeString(state.secrets.DataKey)
	oaPrivateKey, oaErr := base64.StdEncoding.DecodeString(state.secrets.OAReceiptPrivateKey)
	if state.secrets.SchemaVersion != 1 || queryErr != nil || len(queryPrivateKey) != ed25519.PrivateKeySize ||
		dataErr != nil || len(dataKey) != 32 || oaErr != nil || len(oaPrivateKey) != ed25519.PrivateKeySize {
		return errors.New("RQ5 deployment cryptographic keys are invalid")
	}
	for _, value := range []string{state.secrets.BusinessPassword, state.secrets.GatewayDatabasePassword,
		state.secrets.ControlPassword, state.secrets.MinIOPassword, state.secrets.DeliverySigningKey,
		state.secrets.OAServiceToken, state.secrets.OACallbackSecret, state.secrets.OASessionSecret,
		state.secrets.AlicePassword, state.secrets.BobPassword} {
		if len(value) < 32 {
			return errors.New("RQ5 deployment secret is absent or too short")
		}
	}
	oaPublicKey := ed25519.PrivateKey(oaPrivateKey).Public().(ed25519.PublicKey)
	state.composeEnv = replaceEnvironment(state.composeEnv,
		"DAILY_POSTGRES_PASSWORD="+state.secrets.BusinessPassword,
		"DAILY_RQ5_INSTALL_DSN=postgres://postgres:"+state.secrets.BusinessPassword+
			"@business-postgres:5432/taskgate_daily?sslmode=disable",
		"DAILY_GATEWAY_DB_PASSWORD="+state.secrets.GatewayDatabasePassword,
		"DAILY_CONTROL_PASSWORD="+state.secrets.ControlPassword,
		"DAILY_RQ5_MINIO_ROOT_USER=rq5-minio-root",
		"DAILY_RQ5_MINIO_ROOT_PASSWORD="+state.secrets.MinIOPassword,
		"DAILY_RQ5_OBJECT_BUCKET=taskgate-rq5-results",
		"DAILY_RQ5_RECEIPT_KEY_ID=rq5-sequential-ed25519-v1",
		"DAILY_RQ5_RECEIPT_PRIVATE_KEY="+state.secrets.ReceiptPrivateKey,
		"DAILY_RQ5_RESULT_DATA_KEY="+state.secrets.DataKey,
		"DAILY_RQ5_DELIVERY_SIGNING_KEY="+state.secrets.DeliverySigningKey,
		"DAILY_RQ5_OA_SERVICE_TOKEN="+state.secrets.OAServiceToken,
		"DAILY_RQ5_OA_CALLBACK_SECRET="+state.secrets.OACallbackSecret,
		"DAILY_RQ5_OA_SESSION_SECRET="+state.secrets.OASessionSecret,
		"DAILY_RQ5_OA_RECEIPT_KEY_ID=rq5-oa-ed25519-v1",
		"DAILY_RQ5_OA_RECEIPT_PRIVATE_KEY="+state.secrets.OAReceiptPrivateKey,
		"DAILY_RQ5_OA_RECEIPT_PUBLIC_KEY="+base64.StdEncoding.EncodeToString(oaPublicKey),
		"DAILY_RQ5_OA_ALICE_PASSWORD="+state.secrets.AlicePassword,
		"DAILY_RQ5_OA_BOB_PASSWORD="+state.secrets.BobPassword,
		"DAILY_RQ5_GATEWAY_CALLBACK_URL=http://rq5-callback.invalid/api/v1/oa/callback")
	return nil
}

func (state *driverState) runMeasuredPhases(ctx context.Context, request driverRequest, cycleDirectory,
	project string, environment []string) error {
	fixture := filepath.Join(state.runRoot, "fixture")
	input := filepath.Join(fixture, "approved-inputs", request.ToDay+".json")
	artifacts := filepath.Join(cycleDirectory, "artifacts")
	receipt := filepath.Join(cycleDirectory, "receipt")
	uid := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	runPhase := func(phase string, child []string, artifactMode, receiptMode string) error {
		arguments := []string{"run", "--rm", "--pull", "never", "--no-deps", "--user", uid,
			"--volume", input + ":/input/input.json:ro",
			"--volume", artifacts + ":/artifacts:" + artifactMode}
		if receiptMode != "" {
			arguments = append(arguments, "--volume", receipt+":/receipts:"+receiptMode)
		}
		arguments = append(arguments, "phase", "-phase", phase, "-day", request.ToDay,
			"-sample", strconv.Itoa(request.CycleIndex), "--")
		arguments = append(arguments, child...)
		stdout, err := state.composeProject(ctx, project, environment, arguments...)
		if len(stdout) != 0 {
			_ = os.WriteFile(filepath.Join(cycleDirectory, phase+".json"), bytes.TrimSpace(stdout), 0o600)
		}
		if err != nil {
			return err
		}
		var report phaseReport
		if err := decodeJSONFile(filepath.Join(cycleDirectory, phase+".json"), &report); err != nil ||
			report.SchemaVersion != "taskgate-daily-publication-phase-v1" || report.Status != "pass" ||
			report.Phase != phase || report.Day != request.ToDay || report.Sample != request.CycleIndex {
			return errors.New("RQ5 phase emitted an invalid report")
		}
		return nil
	}
	if err := runPhase("build", []string{"/usr/local/bin/v4-offline", "build", "-input", "/input/input.json",
		"-output-dir", "/artifacts"}, "rw", ""); err != nil {
		return err
	}
	if err := runPhase("strict_verify", []string{"/usr/local/bin/v4-offline", "verify", "-input", "/input/input.json",
		"-artifact-dir", "/artifacts", "-receipt", "/receipts/verification.json"}, "ro", "rw"); err != nil {
		return err
	}
	var verify phaseReport
	if err := decodeJSONFile(filepath.Join(cycleDirectory, "strict_verify.json"), &verify); err != nil {
		return err
	}
	var command commandReport
	if err := json.Unmarshal(verify.CommandReport, &command); err != nil || len(command.VerificationReceiptSHA256) != 64 {
		return errors.New("RQ5 strict verification omitted its receipt digest")
	}
	return runPhase("activation", []string{"/usr/local/bin/v4-offline", "activate", "-input", "/input/input.json",
		"-artifact-dir", "/artifacts", "-receipt", "/receipts/verification.json",
		"-receipt-sha256", command.VerificationReceiptSHA256}, "ro", "ro")
}

func (state *driverState) runOnlineCycle(ctx context.Context, request driverRequest, cycleDirectory, output string,
	workspace cycleWorkspace, environment []string) error {
	if err := state.loadSecrets(); err != nil {
		return err
	}
	environment = replaceEnvironment(environment,
		"DAILY_RQ5_RECEIPT_PRIVATE_KEY="+state.secrets.ReceiptPrivateKey,
		"DAILY_RQ5_RESULT_DATA_KEY="+state.secrets.DataKey)
	uid := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	generator := filepath.Join(cycleDirectory, "bound-sources", "05-generate-daily-data.sh")
	config := filepath.Join(cycleDirectory, "bound-sources", "config.json")
	cycleRelative, err := filepath.Rel(state.runRoot, cycleDirectory)
	if err != nil || strings.HasPrefix(cycleRelative, "..") {
		return errors.New("RQ5 cycle path escaped its deployment root")
	}
	outputRelative, err := filepath.Rel(state.runRoot, output)
	if err != nil || strings.HasPrefix(outputRelative, "..") {
		return errors.New("RQ5 output path escaped its deployment root")
	}
	_, err = state.composeProject(ctx, workspace.Project, environment, "run", "--rm", "--pull", "never", "--no-deps",
		"--name", workspace.GatewayContainer, "--user", uid,
		"--volume", state.runRoot+":/evidence:rw",
		"--volume", generator+":/bound/generator:ro", "--volume", config+":/bound/config.json:ro",
		"online", "final-v5-cycle", "-request", "/evidence/"+filepath.ToSlash(filepath.Join(cycleRelative, "request.json")),
		"-input-dir", "/evidence/fixture/approved-inputs", "-fixture-artifact-dir", "/evidence/fixture/artifacts",
		"-target-artifact-dir", "/evidence/"+filepath.ToSlash(filepath.Join(cycleRelative, "artifacts")),
		"-phase-dir", "/evidence/"+filepath.ToSlash(cycleRelative),
		"-dataset-manifest", "/evidence/fixture/dataset-manifest.json", "-generator", "/bound/generator",
		"-config", "/bound/config.json", "-output", "/evidence/"+filepath.ToSlash(outputRelative))
	return err
}

func runCommand(ctx context.Context, directory string, environment []string,
	name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir, command.Env = directory, environment
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, err
	}
	if stdout.Len() > 64<<20 || stderr.Len() > 8<<20 {
		return nil, errors.New("RQ5 helper output exceeded its bound")
	}
	return stdout.Bytes(), nil
}

func replaceEnvironment(base []string, assignments ...string) []string {
	keys := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		name, _, found := strings.Cut(assignment, "=")
		if !found || name == "" {
			continue
		}
		keys[name] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(assignments))
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := keys[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	return append(result, assignments...)
}

func writeJSONExclusive(path string, value any, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(value)
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func decodeJSONFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON file has a trailing value")
	}
	return nil
}
