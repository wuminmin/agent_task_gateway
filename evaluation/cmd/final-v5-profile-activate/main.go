// Command final-v5-profile-activate restarts the Catalog-bound services of one
// deployment on exactly one profile Catalog and proves, from the running
// Gateway rather than from the registry, that it activated that closure.
//
// It never edits evidence to make a Catalog look activated: every observed
// value comes from the Gateway's read-only activation diagnostic.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const diagnosticPath = "/admin/v1/evaluation/profile-activation"

type options struct {
	root             string
	composeProject   string
	composeFiles     string
	deploymentID     string
	profileID        string
	registryPath     string
	gatewayURL       string
	adminToken       string
	datasetBinding   string
	previousProfile  string
	sequence         int
	outputPath       string
	outsideProducts  string
	artifactDir      string
	artifactManifest string
	probeToken       string
	readyTimeout     time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.root, "root", ".", "repository root")
	flag.StringVar(&opts.composeProject, "compose-project", "", "docker compose project name")
	flag.StringVar(&opts.composeFiles, "compose-files", "compose.yaml", "colon-separated compose files")
	flag.StringVar(&opts.deploymentID, "deployment-id", "", "deployment identity")
	flag.StringVar(&opts.profileID, "profile-id", "", "profile to activate")
	flag.StringVar(&opts.registryPath, "registry", "config/profiles/registry.json", "profile registry path")
	flag.StringVar(&opts.gatewayURL, "gateway-url", "http://127.0.0.1:8082", "gateway base URL")
	flag.StringVar(&opts.adminToken, "admin-token-env", "GATEWAY_ADMIN_TOKEN", "env var holding the admin token")
	flag.StringVar(&opts.datasetBinding, "dataset-binding", "", "pilot dataset binding file")
	flag.StringVar(&opts.previousProfile, "previous-profile-id", "", "profile active before this activation")
	flag.IntVar(&opts.sequence, "activation-sequence", 1, "1-based activation sequence in this deployment")
	flag.StringVar(&opts.outputPath, "evidence-out", "", "activation evidence output path")
	flag.StringVar(&opts.outsideProducts, "outside-products", "", "comma-separated Products that must be refused")
	flag.StringVar(&opts.artifactDir, "profile-artifact-dir", "", "exact per-profile snapshot artifact directory")
	flag.StringVar(&opts.artifactManifest, "profile-artifact-manifest", "", "profile artifact manifest path")
	flag.StringVar(&opts.probeToken, "probe-token-env", "TASKBOUND_ALICE_TOKEN", "env var holding the pilot agent token")
	flag.DurationVar(&opts.readyTimeout, "ready-timeout", 5*time.Minute, "readiness timeout")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	if err := run(opts); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "final-v5-profile-activate:", err)
	os.Exit(1)
}

func run(opts options) error {
	if opts.composeProject == "" || opts.deploymentID == "" || opts.profileID == "" || opts.outputPath == "" {
		return errors.New("compose-project, deployment-id, profile-id and evidence-out are required")
	}
	registry, err := loadRegistry(filepath.Join(opts.root, opts.registryPath))
	if err != nil {
		return err
	}
	profile, err := finalv5profile.LookupProfileByID(registry, opts.profileID)
	if err != nil {
		return err
	}
	// Activation refuses a profile the Catalog layer has not cleared. The
	// remaining two states -- activation_supported and targeted validation --
	// are what this run and a later targeted run establish.
	if !profile.Status.ClosureComplete || !profile.Status.CatalogMaterializable || !profile.Status.LiveRouteAvailable {
		return fmt.Errorf("profile %q is not cleared for activation: %+v", profile.Alias, profile.Status)
	}
	catalogPath := filepath.Join(opts.root, profile.CatalogPath)
	catalogFileDigest, err := fileSHA256(catalogPath)
	if err != nil {
		return fmt.Errorf("profile Catalog: %w", err)
	}
	expected := finalv5profile.ExpectedArtifacts(profile)
	if profile.TotalHotBytes > finalv5profile.MaxHotBytesPerInstance {
		return fmt.Errorf("profile %q expects %d HOT bytes, above the %d byte %s",
			profile.Alias, profile.TotalHotBytes, finalv5profile.MaxHotBytesPerInstance, finalv5profile.HotLimitScope)
	}

	evidence := finalv5profile.ActivationEvidence{SchemaVersion: 1,
		Record: finalv5profile.ActivationEvidenceVersion, ContractRelease: registry.ContractRelease,
		CampaignClass: "pilot", PublicationEligible: false, DeploymentID: opts.deploymentID,
		ActivationSequence: opts.sequence, PreviousProfileID: opts.previousProfile,
		ProfileID: profile.ID, ProfileAlias: profile.Alias, ClosureSHA256: profile.Closure.SHA256,
		CatalogSHA256: profile.CatalogSHA256, CatalogFileSHA256: catalogFileDigest,
		ExpectedProducts: profile.Closure.Products, ExpectedPublications: profile.Closure.Publications,
		ExpectedHotArtifacts: expected, ExpectedHotBytes: profile.TotalHotBytes,
		HotLimitBytes: finalv5profile.MaxHotBytesPerInstance, Status: "fail"}
	if evidence.PublicationSetSHA, err = experiment.CanonicalPublicationSetSHA256(profile.Closure.Publications); err != nil {
		return err
	}
	if opts.datasetBinding != "" {
		if evidence.DatasetBindingSHA, err = fileSHA256(opts.datasetBinding); err != nil {
			return fmt.Errorf("dataset binding: %w", err)
		}
	}

	// Bind the exact per-profile artifact directory before anything is started.
	if opts.artifactDir != "" {
		if opts.artifactManifest == "" {
			return errors.New("profile-artifact-manifest is required with profile-artifact-dir")
		}
		artifactManifest, err := loadArtifactManifest(opts.artifactManifest, profile.ID)
		if err != nil {
			return err
		}
		if err := finalv5profile.VerifyProfileArtifactDirectory(opts.artifactDir, artifactManifest); err != nil {
			return fmt.Errorf("profile artifact directory: %w", err)
		}
		evidence.ProfileArtifactDirectorySHA256 = artifactManifest.DirectorySHA256
		evidence.ProfileArtifactManifestSHA256, err = fileSHA256(opts.artifactManifest)
		if err != nil {
			return err
		}
		absolute, err := filepath.Abs(opts.artifactDir)
		if err != nil {
			return err
		}
		evidence.MountedArtifactIdentity = absolute
		evidence.ExpectedSourcePublications = artifactManifest.Publications
	}

	ctx := context.Background()
	started := time.Now().UTC()
	evidence.ActivationStarted = started.Format(time.RFC3339Nano)

	// The previous profile is verified, never assumed. A declared predecessor
	// that cannot be read, or that is not the profile actually running, stops
	// the switch: an unverifiable predecessor makes the whole activation chain
	// unverifiable.
	previous, previousErr := readDiagnostic(ctx, opts)
	switch {
	case opts.previousProfile != "":
		if previousErr != nil {
			evidence.Failures = append(evidence.Failures,
				"declared previous profile could not be read: "+previousErr.Error())
			return writeEvidence(opts.outputPath, evidence)
		}
		declared, err := finalv5profile.LookupProfileByID(registry, opts.previousProfile)
		if err != nil {
			evidence.Failures = append(evidence.Failures, err.Error())
			return writeEvidence(opts.outputPath, evidence)
		}
		observedPrevious, err := resolveObservedProfile(registry, previous)
		if err != nil {
			evidence.Failures = append(evidence.Failures, "previous profile: "+err.Error())
			return writeEvidence(opts.outputPath, evidence)
		}
		if observedPrevious.ID != declared.ID {
			evidence.Failures = append(evidence.Failures, fmt.Sprintf(
				"the running Gateway serves profile %s, the caller declared %s", observedPrevious.ID, declared.ID))
			return writeEvidence(opts.outputPath, evidence)
		}
		if strings.TrimSpace(previous.Activation.ProcessNonce) == "" ||
			strings.TrimSpace(previous.Activation.CacheNamespace) == "" {
			evidence.Failures = append(evidence.Failures, "previous Gateway reported no process nonce or cache namespace")
			return writeEvidence(opts.outputPath, evidence)
		}
	case previousErr == nil && previous != nil:
		// A Gateway is already serving a profile but the caller declared none.
		// Resolve it exactly rather than pretending this is a first activation.
		observedPrevious, err := resolveObservedProfile(registry, previous)
		if err != nil {
			evidence.Failures = append(evidence.Failures,
				"a Gateway is already running but its profile is unresolvable: "+err.Error())
			return writeEvidence(opts.outputPath, evidence)
		}
		evidence.PreviousProfileID = observedPrevious.ID
	}
	if previous != nil {
		evidence.CacheIsolation.PreviousProcessNonce = previous.Activation.ProcessNonce
		evidence.CacheIsolation.PreviousCacheNamespace = previous.Activation.CacheNamespace
		// The drain gate is read from the profile that is about to be stopped.
		evidence.DrainObserverVersion = previous.Drain.Version
		evidence.DrainObservationStatus = previous.Drain.Status
		evidence.DrainObservationSHA256 = previous.Drain.SHA256
		if previous.Drain.Counts != nil {
			evidence.DrainBefore = finalv5profile.DrainCounts{
				InflightQueries:   previous.Drain.Counts.InflightQueries,
				PendingArtifacts:  previous.Drain.Counts.PendingArtifacts,
				OpenReservations:  previous.Drain.Counts.OpenReservations,
				ActiveServedRoots: previous.Drain.Counts.ActiveServedRoots}
		}
		if evidence.DrainObservationStatus != finalv5profile.DrainObservationObserved ||
			!evidence.DrainBefore.Complete() {
			evidence.Failures = append(evidence.Failures,
				"the outgoing profile drain could not be observed; refusing to stop it")
			return writeEvidence(opts.outputPath, evidence)
		}
		if !evidence.DrainBefore.Clean() {
			evidence.Failures = append(evidence.Failures,
				"the outgoing profile still holds in-flight work; refusing to switch")
			return writeEvidence(opts.outputPath, evidence)
		}
	} else {
		// A first activation has no outgoing Gateway to ask, so the gates are
		// read straight from Control. They are never assumed to be zero.
		counts, digest, initialized, err := observeControlDrain(ctx, opts)
		if err != nil {
			evidence.DrainObservationStatus = "unavailable"
			evidence.DrainObserverVersion = drainObserverVersion
			evidence.Failures = append(evidence.Failures, err.Error())
			return writeEvidence(opts.outputPath, evidence)
		}
		evidence.DrainObserverVersion = drainObserverVersion
		if !initialized {
			evidence.DrainObserverVersion = drainObserverVersion + "-uninitialized-control"
		}
		evidence.DrainObservationStatus = finalv5profile.DrainObservationObserved
		evidence.DrainBefore = counts
		evidence.DrainObservationSHA256 = digest
		if !evidence.DrainBefore.Clean() {
			evidence.Failures = append(evidence.Failures,
				"Control still holds in-flight work before the first activation")
			return writeEvidence(opts.outputPath, evidence)
		}
	}
	if err := stopCatalogBoundServices(ctx, opts); err != nil {
		evidence.Failures = append(evidence.Failures, "stop catalog-bound services: "+err.Error())
		return writeEvidence(opts.outputPath, evidence)
	}
	if err := startCatalogBoundServices(ctx, opts, catalogPath, opts.artifactDir); err != nil {
		evidence.Failures = append(evidence.Failures, "restart catalog-bound services: "+err.Error())
		return writeEvidence(opts.outputPath, evidence)
	}
	current, err := waitForDiagnostic(ctx, opts)
	if err != nil {
		evidence.Failures = append(evidence.Failures, "read activation diagnostic: "+err.Error())
		return writeEvidence(opts.outputPath, evidence)
	}
	readinessAt := time.Now().UTC()
	evidence.ReadinessAt = readinessAt.Format(time.RFC3339Nano)
	evidence.ActivationEnded = readinessAt.Format(time.RFC3339Nano)
	// Activation duration is recorded here and nowhere else. It never enters a
	// query pipeline measurement or a measured sample.
	evidence.ActivationMS = float64(readinessAt.Sub(started).Microseconds()) / 1000

	evidence.ObservedProducts = current.Activation.Products
	evidence.ObservedPublications = current.Activation.Publications
	evidence.ObservedLoaderPublications = current.Activation.Publications
	for _, artifact := range current.Activation.HotArtifacts {
		evidence.ObservedHotArtifacts = append(evidence.ObservedHotArtifacts,
			finalv5profile.ObservedArtifact{Identity: artifact.Publication,
				Digest: artifact.HotIndexDigest, Bytes: artifact.Bytes})
	}
	evidence.ActualHotBytes = current.Activation.ActualHotBytes
	evidence.ProcessNonce = current.Activation.ProcessNonce
	evidence.ProcessStartedUnix = current.Activation.ProcessStartedUnix
	evidence.CacheIsolation.ProcessNonce = current.Activation.ProcessNonce
	evidence.CacheIsolation.CacheNamespace = current.Activation.CacheNamespace
	evidence.CacheIsolation.ProcessRestarted = previous == nil ||
		previous.Activation.ProcessNonce != current.Activation.ProcessNonce
	evidence.CacheIsolation.PreviousCacheUnreachable = evidence.CacheIsolation.ProcessRestarted
	// SemanticCacheCatalogBound is only true when the switch actually changed
	// the namespace every in-process and persisted lookup is keyed by.
	evidence.CacheIsolation.SemanticCacheCatalogBound = previous == nil ||
		(previous.Activation.CacheNamespace != current.Activation.CacheNamespace &&
			previous.Activation.CatalogSHA256 != current.Activation.CatalogSHA256)
	evidence.CacheIsolation.PreviousHotArtifactsRetired = previous == nil ||
		!sharesArtifact(previous.Activation.HotArtifacts, current.Activation.HotArtifacts)
	if current.Activation.CatalogSHA256 != profile.CatalogSHA256 {
		evidence.Failures = append(evidence.Failures, fmt.Sprintf(
			"gateway activated Catalog %s, expected %s", current.Activation.CatalogSHA256, profile.CatalogSHA256))
	}
	evidence.GatewayImageID, evidence.GatewayContainerID = containerIdentity(ctx, opts)

	for _, product := range splitProducts(opts.outsideProducts) {
		absent, observed := probeOutsideProduct(current, product)
		liveRefused, classification, responseDigest := liveOutsideProductProbe(ctx, opts, product)
		probe := finalv5profile.OutsideProductProbe{Product: product,
			CatalogListAbsent: absent, LiveRequestRefused: liveRefused,
			Refused: absent && liveRefused, Observed: observed,
			Classification: classification, ResponseSHA256: responseDigest,
			RequestedProductSHA256: productDigest(product)}
		evidence.OutsideProduct = append(evidence.OutsideProduct, probe)
	}
	if len(evidence.Failures) == 0 {
		evidence.Status = "pass"
		evidence.ActivationSmokePassed = true
	}
	if err := finalv5profile.ValidateActivationEvidence(evidence, profile); err != nil {
		evidence.Status = "fail"
		evidence.ActivationSmokePassed = false
		evidence.Failures = append(evidence.Failures, err.Error())
	}
	if err := writeEvidence(opts.outputPath, evidence); err != nil {
		return err
	}
	if evidence.Status != "pass" {
		return fmt.Errorf("profile %q activation failed: %s", profile.Alias, strings.Join(evidence.Failures, "; "))
	}
	return nil
}

// diagnosticResponse mirrors the Gateway's read-only activation view.
type diagnosticResponse struct {
	SchemaVersion   int    `json:"schema_version"`
	Diagnostic      string `json:"diagnostic"`
	ReadinessStatus string `json:"readiness_status"`
	Activation      struct {
		CatalogSHA256 string   `json:"catalog_sha256"`
		Products      []string `json:"products"`
		Publications  []string `json:"publications"`
		HotArtifacts  []struct {
			Publication    string `json:"publication"`
			ManifestDigest string `json:"manifest_digest"`
			HotIndexDigest string `json:"hot_index_digest"`
			Bytes          int64  `json:"bytes"`
		} `json:"hot_artifacts"`
		ActualHotBytes     int64  `json:"actual_hot_bytes"`
		HotLimitBytes      int64  `json:"configured_hot_limit_bytes"`
		CacheNamespace     string `json:"cache_namespace_digest"`
		ProcessNonce       string `json:"process_instance_nonce"`
		ProcessStartedUnix int64  `json:"process_started_unix"`
	} `json:"activation"`
	Drain struct {
		Version string `json:"observer_version"`
		Status  string `json:"status"`
		SHA256  string `json:"observation_sha256"`
		Error   string `json:"error,omitempty"`
		Counts  *struct {
			InflightQueries   *int64 `json:"inflight_queries"`
			PendingArtifacts  *int64 `json:"pending_artifacts"`
			OpenReservations  *int64 `json:"open_reservations"`
			ActiveServedRoots *int64 `json:"active_served_roots"`
		} `json:"counts"`
	} `json:"drain"`
}

// resolveObservedProfile reverse-resolves which registered profile a running
// Gateway is actually serving, from what it reports holding. The expected
// profile ID is never written into evidence as if the Gateway had reported it:
// zero or several matches is an activation failure.
func resolveObservedProfile(registry finalv5profile.Registry, response *diagnosticResponse) (finalv5profile.Profile, error) {
	observedArtifacts := make([]finalv5profile.ObservedArtifact, 0, len(response.Activation.HotArtifacts))
	for _, artifact := range response.Activation.HotArtifacts {
		observedArtifacts = append(observedArtifacts, finalv5profile.ObservedArtifact{
			Identity: artifact.Publication, Digest: artifact.HotIndexDigest, Bytes: artifact.Bytes})
	}
	var matched []finalv5profile.Profile
	for _, profile := range registry.Profiles {
		if !profile.Status.CatalogMaterializable || profile.CatalogSHA256 != response.Activation.CatalogSHA256 {
			continue
		}
		if !equalSets(profile.Closure.Products, response.Activation.Products) ||
			!equalSets(profile.Closure.Publications, response.Activation.Publications) {
			continue
		}
		if !equalArtifactSets(finalv5profile.ExpectedArtifacts(profile), observedArtifacts) {
			continue
		}
		matched = append(matched, profile)
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return finalv5profile.Profile{}, fmt.Errorf(
			"the running Gateway serves Catalog %s, which matches no registered profile", response.Activation.CatalogSHA256)
	default:
		return finalv5profile.Profile{}, fmt.Errorf(
			"the running Gateway matches %d registered profiles; profile identity is ambiguous", len(matched))
	}
}

func equalSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	first := append([]string(nil), left...)
	second := append([]string(nil), right...)
	sort.Strings(first)
	sort.Strings(second)
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func equalArtifactSets(left, right []finalv5profile.ObservedArtifact) bool {
	if len(left) != len(right) {
		return false
	}
	first := append([]finalv5profile.ObservedArtifact(nil), left...)
	second := append([]finalv5profile.ObservedArtifact(nil), right...)
	sort.Slice(first, func(a, b int) bool { return first[a].Identity < first[b].Identity })
	sort.Slice(second, func(a, b int) bool { return second[a].Identity < second[b].Identity })
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func probeOutsideProduct(response *diagnosticResponse, product string) (bool, string) {
	for _, published := range response.Activation.Products {
		if published == product {
			return false, "the activated Catalog publishes this Product"
		}
	}
	return true, "the activated Catalog does not publish this Product"
}

func sharesArtifact(previous, current []struct {
	Publication    string `json:"publication"`
	ManifestDigest string `json:"manifest_digest"`
	HotIndexDigest string `json:"hot_index_digest"`
	Bytes          int64  `json:"bytes"`
}) bool {
	for _, before := range previous {
		for _, after := range current {
			if before.HotIndexDigest == after.HotIndexDigest {
				return true
			}
		}
	}
	return false
}

func readDiagnostic(ctx context.Context, opts options) (*diagnosticResponse, error) {
	token := strings.TrimSpace(os.Getenv(opts.adminToken))
	if token == "" {
		return nil, errors.New("admin token is required to read the activation diagnostic")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(opts.gatewayURL, "/")+diagnosticPath, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("activation diagnostic returned %d", response.StatusCode)
	}
	value, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var decoded diagnosticResponse
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	if decoded.Diagnostic == "" || decoded.Activation.CatalogSHA256 == "" {
		return nil, errors.New("activation diagnostic is empty")
	}
	return &decoded, nil
}

func waitForDiagnostic(ctx context.Context, opts options) (*diagnosticResponse, error) {
	deadline := time.Now().Add(opts.readyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := readDiagnostic(ctx, opts)
		if err == nil && response.ReadinessStatus == "ready" {
			return response, nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = errors.New("gateway did not become ready")
	}
	return nil, lastErr
}

func composeArgs(opts options) []string {
	args := []string{"compose", "--project-name", opts.composeProject}
	for _, file := range strings.Split(opts.composeFiles, ":") {
		if strings.TrimSpace(file) != "" {
			args = append(args, "--file", file)
		}
	}
	return args
}

// catalogBoundServices are restarted on a profile switch. Business PostgreSQL
// and the object store are deliberately absent: a profile switch must not
// rebuild the Dataset.
var catalogBoundServices = []string{"gateway", "oa-demo"}

func stopCatalogBoundServices(ctx context.Context, opts options) error {
	args := append(composeArgs(opts), "stop")
	return runCommand(ctx, opts.root, "docker", append(args, catalogBoundServices...)...)
}

func startCatalogBoundServices(ctx context.Context, opts options, catalogPath, artifactDir string) error {
	absolute, err := filepath.Abs(catalogPath)
	if err != nil {
		return err
	}
	args := append(composeArgs(opts), "up", "-d", "--force-recreate", "--wait")
	command := exec.CommandContext(ctx, "docker", append(args, catalogBoundServices...)...)
	command.Dir = opts.root
	environment := append(os.Environ(), "TASKGATE_PROFILE_CATALOG="+absolute, "TASKGATE_EXPERIMENT_CLASS=pilot")
	if artifactDir != "" {
		absoluteArtifacts, err := filepath.Abs(artifactDir)
		if err != nil {
			return err
		}
		environment = append(environment, "TASKGATE_PROFILE_ARTIFACT_DIR="+absoluteArtifacts)
	}
	command.Env = environment
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	return command.Run()
}

func containerIdentity(ctx context.Context, opts options) (string, string) {
	args := append(composeArgs(opts), "ps", "--quiet", "gateway")
	var out bytes.Buffer
	command := exec.CommandContext(ctx, "docker", args...)
	command.Dir, command.Stdout = opts.root, &out
	if command.Run() != nil {
		return "", ""
	}
	container := strings.TrimSpace(out.String())
	if container == "" {
		return "", ""
	}
	var image bytes.Buffer
	inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Image}}", container)
	inspect.Stdout = &image
	if inspect.Run() != nil {
		return "", container
	}
	return strings.TrimSpace(image.String()), container
}

func runCommand(ctx context.Context, dir, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir, command.Stdout, command.Stderr = dir, os.Stderr, os.Stderr
	return command.Run()
}

// loadArtifactManifest reads the manifest emitted beside a materialized
// profile directory and refuses one that describes a different profile.
func loadArtifactManifest(path, profileID string) (finalv5profile.ArtifactManifest, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return finalv5profile.ArtifactManifest{}, err
	}
	var document struct {
		Profiles map[string]finalv5profile.ArtifactManifest `json:"profiles"`
	}
	if err := json.Unmarshal(value, &document); err != nil {
		return finalv5profile.ArtifactManifest{}, err
	}
	manifest, found := document.Profiles[profileID]
	if !found {
		return finalv5profile.ArtifactManifest{}, fmt.Errorf("artifact manifest set has no entry for %s", profileID)
	}
	if manifest.ProfileID != profileID {
		return finalv5profile.ArtifactManifest{}, errors.New("artifact manifest identifies a different profile")
	}
	return manifest, nil
}

// observeControlDrain reads the drain gates straight from Control when there is
// no outgoing Gateway to ask. It runs through compose exec, so no DSN is
// constructed here and none reaches the evidence.
func observeControlDrain(ctx context.Context, opts options) (finalv5profile.DrainCounts, string, bool, error) {
	// On a genuinely fresh deployment the Gateway has never started, so it has
	// never migrated the Control schema. That is an observable state, not an
	// unobservable one: with no tables there are provably no in-flight queries,
	// PENDING artifacts, reservations or ACTIVE roots. It is reported as such
	// and recorded, rather than assumed or hard-coded.
	const presence = `SELECT count(*)::bigint FROM information_schema.tables
 WHERE table_schema='public'
   AND table_name IN ('query_records','result_artifacts','query_exposure_reservations','tasks')`
	present, err := controlScalar(ctx, opts, presence)
	if err != nil {
		return finalv5profile.DrainCounts{}, "", false, fmt.Errorf("control drain observation failed: %w", err)
	}
	if present == 0 {
		zero := int64(0)
		counts := finalv5profile.DrainCounts{InflightQueries: &zero, PendingArtifacts: &zero,
			OpenReservations: &zero, ActiveServedRoots: &zero}
		return counts, drainDigest(drainObserverVersion+"-uninitialized-control", 0, 0, 0, 0), false, nil
	}
	if present != 4 {
		return finalv5profile.DrainCounts{}, "", false,
			fmt.Errorf("control drain observation found %d of 4 gate tables", present)
	}
	const query = `SELECT
  (SELECT count(*) FROM query_records WHERE status = 'RESERVED')::bigint || ' ' ||
  (SELECT count(*) FROM result_artifacts WHERE status = 'PENDING')::bigint || ' ' ||
  (SELECT count(*) FROM query_exposure_reservations WHERE status = 'RESERVED')::bigint || ' ' ||
  (SELECT count(*) FROM tasks WHERE state = 'ACTIVE')::bigint`
	out, err := controlQuery(ctx, opts, query)
	if err != nil {
		return finalv5profile.DrainCounts{}, "", true, fmt.Errorf("control drain observation failed: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 4 {
		return finalv5profile.DrainCounts{}, "", true, errors.New("control drain observation returned an unexpected shape")
	}
	values := make([]int64, 4)
	for index, field := range fields {
		parsed, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return finalv5profile.DrainCounts{}, "", true, errors.New("control drain observation is not numeric")
		}
		values[index] = parsed
	}
	counts := finalv5profile.DrainCounts{InflightQueries: &values[0], PendingArtifacts: &values[1],
		OpenReservations: &values[2], ActiveServedRoots: &values[3]}
	return counts, drainDigest(drainObserverVersion, values[0], values[1], values[2], values[3]), true, nil
}

func drainDigest(version string, values ...int64) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00", version)
	for _, value := range values {
		fmt.Fprintf(hash, "%d\x00", value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func controlQuery(ctx context.Context, opts options, query string) (string, error) {
	args := append(composeArgs(opts), "exec", "-T", "control-postgres",
		psqlBinary, "-U", "postgres", "-d", controlDatabase(), "-XAtqc", query)
	var out bytes.Buffer
	command := exec.CommandContext(ctx, "docker", args...)
	command.Dir, command.Stdout, command.Stderr = opts.root, &out, os.Stderr
	if err := command.Run(); err != nil {
		return "", err
	}
	value := strings.TrimSpace(out.String())
	if strings.Contains(value, "ERROR:") {
		return "", errors.New("control query returned an error")
	}
	return value, nil
}

func controlScalar(ctx context.Context, opts options, query string) (int64, error) {
	value, err := controlQuery(ctx, opts, query)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}

const (
	drainObserverVersion = "taskgate-final-v5-drain-observer-v1"
	psqlBinary           = "psql"
)

func controlDatabase() string {
	if value := strings.TrimSpace(os.Getenv("CONTROL_POSTGRES_DB")); value != "" {
		return value
	}
	return "taskbound_gateway"
}

func loadRegistry(path string) (finalv5profile.Registry, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return finalv5profile.Registry{}, err
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(value, &registry); err != nil {
		return finalv5profile.Registry{}, err
	}
	return registry, nil
}

func writeEvidence(path string, evidence finalv5profile.ActivationEvidence) error {
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

func fileSHA256(path string) (string, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:]), nil
}

func productDigest(product string) string {
	digest := sha256.Sum256([]byte("taskgate-final-v5-outside-product-probe-v1\x00" + product))
	return hex.EncodeToString(digest[:])
}

func splitProducts(value string) []string {
	var products []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			products = append(products, trimmed)
		}
	}
	return products
}

// liveOutsideProductProbe asks the running deployment, through the ordinary
// public MCP task-request path, for a Product that belongs to a different
// profile. A Catalog list check alone proves only what the file says; this
// proves the deployment actually refuses to issue a task for it.
//
// It never touches Control directly and never calls an internal Go function in
// place of the public API. It records a stable classification, not the raw
// response: no token, no SQL, no identity, no secret.
func liveOutsideProductProbe(ctx context.Context, opts options, product string) (bool, string, string) {
	token := strings.TrimSpace(os.Getenv(opts.probeToken))
	if token == "" {
		return false, "probe_identity_absent", ""
	}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "request_data_task", "arguments": map[string]any{
			"objective":     "Stage B.2a-Live outside-Product refusal probe",
			"data_products": []string{product},
			"columns":       map[string][]string{product: {"department"}},
			"scopes":        map[string]any{},
		}}})
	if err != nil {
		return false, "probe_encode_failed", ""
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.gatewayURL, "/")+"/mcp", bytes.NewReader(payload))
	if err != nil {
		return false, "probe_request_failed", ""
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return false, "probe_transport_failed", ""
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return false, "probe_read_failed", ""
	}
	digest := sha256.Sum256(body)
	responseDigest := hex.EncodeToString(digest[:])
	if response.StatusCode != http.StatusOK {
		return true, fmt.Sprintf("http_%d", response.StatusCode), responseDigest
	}
	var decoded struct {
		Result *struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false, "probe_decode_failed", responseDigest
	}
	switch {
	case decoded.Error != nil:
		return true, fmt.Sprintf("jsonrpc_error_%d", decoded.Error.Code), responseDigest
	case decoded.Result != nil && decoded.Result.IsError:
		return true, "tool_error", responseDigest
	default:
		// The deployment issued a task for a Product outside its closure.
		return false, "request_accepted", responseDigest
	}
}
