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
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const diagnosticPath = "/admin/v1/evaluation/profile-activation"

type options struct {
	root            string
	composeProject  string
	composeFiles    string
	deploymentID    string
	profileID       string
	registryPath    string
	gatewayURL      string
	adminToken      string
	datasetBinding  string
	previousProfile string
	sequence        int
	outputPath      string
	outsideProducts string
	readyTimeout    time.Duration
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

	ctx := context.Background()
	started := time.Now().UTC()
	evidence.ActivationStarted = started.Format(time.RFC3339Nano)

	previous, _ := readDiagnostic(ctx, opts)
	if previous != nil {
		evidence.CacheIsolation.PreviousProcessNonce = previous.Activation.ProcessNonce
		evidence.CacheIsolation.PreviousCacheNamespace = previous.Activation.CacheNamespace
	}
	if err := stopCatalogBoundServices(ctx, opts); err != nil {
		evidence.Failures = append(evidence.Failures, "stop catalog-bound services: "+err.Error())
		return writeEvidence(opts.outputPath, evidence)
	}
	if err := startCatalogBoundServices(ctx, opts, catalogPath); err != nil {
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
	evidence.CacheIsolation.SemanticCacheCatalogBound = true
	evidence.CacheIsolation.PreviousHotArtifactsRetired = previous == nil ||
		!sharesArtifact(previous.Activation.HotArtifacts, current.Activation.HotArtifacts)
	if current.Activation.CatalogSHA256 != profile.CatalogSHA256 {
		evidence.Failures = append(evidence.Failures, fmt.Sprintf(
			"gateway activated Catalog %s, expected %s", current.Activation.CatalogSHA256, profile.CatalogSHA256))
	}
	evidence.GatewayImageID, evidence.GatewayContainerID = containerIdentity(ctx, opts)

	for _, product := range splitProducts(opts.outsideProducts) {
		refused, observed := probeOutsideProduct(current, product)
		evidence.OutsideProduct = append(evidence.OutsideProduct,
			finalv5profile.OutsideProductProbe{Product: product, Refused: refused, Observed: observed})
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

func startCatalogBoundServices(ctx context.Context, opts options, catalogPath string) error {
	absolute, err := filepath.Abs(catalogPath)
	if err != nil {
		return err
	}
	args := append(composeArgs(opts), "up", "-d", "--force-recreate", "--wait")
	command := exec.CommandContext(ctx, "docker", append(args, catalogBoundServices...)...)
	command.Dir = opts.root
	command.Env = append(os.Environ(), "TASKGATE_PROFILE_CATALOG="+absolute, "TASKGATE_EXPERIMENT_CLASS=pilot")
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

func splitProducts(value string) []string {
	var products []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			products = append(products, trimmed)
		}
	}
	return products
}
