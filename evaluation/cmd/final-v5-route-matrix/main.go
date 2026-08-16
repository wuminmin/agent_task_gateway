// Command final-v5-route-matrix derives and executes the exhaustive
// outside-Product route matrix for every Catalog-and-route-cleared profile.
//
// The command deliberately separates three operations:
//
//   - plan derives the profile/Product pairs without touching a deployment;
//   - aggregate turns existing activation evidence into the stable matrix; and
//   - live invokes final-v5-profile-activate once per profile, then aggregates
//     the evidence only after every invocation has returned.
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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	defaultRegistryPath     = "config/profiles/registry.json"
	defaultIntersectionPath = "evaluation/final-v5-wsl2/profiles/product-intersection-v1.json"
	activationRecord        = "taskgate-final-v5-profile-activation-evidence-v1"
	intersectionRecord      = "taskgate-final-v5-product-intersection-v1"
	routeMatrixRecord       = "taskgate-final-v5-outside-product-route-matrix-v1"
	planRecord              = "taskgate-final-v5-outside-product-route-plan-v1"
	productDigestDomain     = "taskgate-final-v5-outside-product-probe-v1\x00"
)

var (
	errLiveEnvironmentSkipped = errors.New("live route-matrix environment is unavailable")
	errRouteMatrixFailed      = errors.New("outside-product route matrix failed")
)

type options struct {
	mode                    string
	deriveOnly              bool
	root                    string
	registryPath            string
	intersectionPath        string
	evidenceDir             string
	outputPath              string
	verifyOnly              bool
	activatorBinary         string
	composeProject          string
	composeFiles            string
	deploymentID            string
	gatewayURL              string
	adminTokenEnv           string
	datasetBinding          string
	profileArtifactRoot     string
	profileArtifactManifest string
	businessDSNEnv          string
	schemaAttestations      string
	probeTokenEnv           string
	readyTimeout            time.Duration
	activationSequenceStart int
	previousProfileID       string
	profileAlias            string
}

func main() {
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fatal(err)
	}
	if err := execute(context.Background(), opts, os.Stdout, os.Stderr, runSubprocess); errors.Is(err, errLiveEnvironmentSkipped) {
		fmt.Fprintln(os.Stderr, "final-v5-route-matrix: SKIPPED:", err)
		os.Exit(3)
	} else if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "final-v5-route-matrix:", err)
	os.Exit(1)
}

func parseFlags(arguments []string, output io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("final-v5-route-matrix", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.mode, "mode", "plan", "plan, aggregate, or live")
	flags.BoolVar(&opts.deriveOnly, "derive-only", false,
		"derive and emit the offline probe plan (alias for -mode plan)")
	flags.StringVar(&opts.root, "root", ".", "repository root")
	flags.StringVar(&opts.registryPath, "registry", defaultRegistryPath, "profile registry path")
	flags.StringVar(&opts.intersectionPath, "product-intersection", defaultIntersectionPath,
		"product-intersection matrix path")
	flags.StringVar(&opts.evidenceDir, "activation-evidence-dir", "",
		"directory containing, or receiving, one activation evidence file per profile")
	flags.StringVar(&opts.outputPath, "out", "", "output path; plan writes stdout when omitted")
	flags.BoolVar(&opts.verifyOnly, "verify", false, "compare output with -out instead of writing it")
	flags.StringVar(&opts.activatorBinary, "activator-binary", "",
		"prebuilt final-v5-profile-activate binary; empty uses go run")
	flags.StringVar(&opts.composeProject, "compose-project", "", "forwarded activation Compose project")
	flags.StringVar(&opts.composeFiles, "compose-files", "compose.yaml", "forwarded colon-separated Compose files")
	flags.StringVar(&opts.deploymentID, "deployment-id", "", "forwarded deployment identity")
	flags.StringVar(&opts.gatewayURL, "gateway-url", "http://127.0.0.1:8082", "forwarded Gateway base URL")
	flags.StringVar(&opts.adminTokenEnv, "admin-token-env", "GATEWAY_ADMIN_TOKEN",
		"forwarded admin-token environment variable name")
	flags.StringVar(&opts.datasetBinding, "dataset-binding", "", "forwarded pilot dataset binding path")
	flags.StringVar(&opts.profileArtifactRoot, "profile-artifact-root", "",
		"directory containing one artifact directory named by profile ID")
	flags.StringVar(&opts.profileArtifactManifest, "profile-artifact-manifest", "",
		"forwarded profile artifact manifest set")
	flags.StringVar(&opts.businessDSNEnv, "business-dsn-env", "TASKGATE_FINAL_V5_BUSINESS_DSN",
		"forwarded Business PostgreSQL DSN environment variable name")
	flags.StringVar(&opts.schemaAttestations, "schema-attestations", "config/profiles/schema-attestations-v1.json",
		"forwarded schema-attestation registry")
	flags.StringVar(&opts.probeTokenEnv, "probe-token-env", "TASKBOUND_ALICE_TOKEN",
		"forwarded outside-Product probe identity environment variable name")
	flags.DurationVar(&opts.readyTimeout, "ready-timeout", 5*time.Minute, "forwarded activation readiness timeout")
	flags.IntVar(&opts.activationSequenceStart, "activation-sequence-start", 1,
		"first activation sequence number")
	flags.StringVar(&opts.previousProfileID, "previous-profile-id", "",
		"profile active before the first live matrix activation")
	flags.StringVar(&opts.profileAlias, "profile-alias", "",
		"activate exactly one planned profile; live mode only and no route-matrix aggregate")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, errors.New("positional arguments are not accepted")
	}
	if opts.deriveOnly {
		opts.mode = "plan"
	}
	return opts, nil
}

type inputSet struct {
	registry           finalv5profile.Registry
	intersection       intersectionReport
	registryPath       string
	intersectionPath   string
	registrySHA256     string
	intersectionSHA256 string
}

func execute(ctx context.Context, opts options, stdout, stderr io.Writer, runner subprocessRunner) error {
	root, err := filepath.Abs(opts.root)
	if err != nil {
		return fmt.Errorf("repository root: %w", err)
	}
	opts.root = root
	if opts.evidenceDir != "" {
		opts.evidenceDir = resolvePath(root, opts.evidenceDir)
	}
	if opts.outputPath != "" {
		opts.outputPath = resolvePath(root, opts.outputPath)
	}
	inputs, err := loadInputs(opts)
	if err != nil {
		return err
	}
	plan, err := deriveProbePlan(inputs.registry, inputs.registrySHA256, inputs.intersectionSHA256)
	if err != nil {
		return err
	}
	if opts.profileAlias != "" {
		if opts.mode != "live" {
			return errors.New("profile-alias is accepted only in live mode")
		}
		filtered := plan.Profiles[:0]
		for _, profile := range plan.Profiles {
			if profile.ProfileAlias == opts.profileAlias {
				filtered = append(filtered, profile)
			}
		}
		if len(filtered) != 1 {
			return fmt.Errorf("profile alias %q is not uniquely planned", opts.profileAlias)
		}
		plan.Profiles = filtered
	}

	switch opts.mode {
	case "plan":
		if opts.verifyOnly && opts.outputPath == "" {
			return errors.New("plan -verify requires -out")
		}
		encoded, err := encodeIndented(plan)
		if err != nil {
			return err
		}
		if opts.outputPath == "" {
			_, err = stdout.Write(encoded)
			return err
		}
		return publish(opts.outputPath, encoded, opts.verifyOnly)
	case "aggregate":
		if opts.evidenceDir == "" {
			return errors.New("aggregate requires -activation-evidence-dir")
		}
		if opts.outputPath == "" {
			return errors.New("aggregate requires -out")
		}
	case "live":
		if err := validateLiveOptions(opts); err != nil {
			return err
		}
		if err := executeLive(ctx, opts, plan, stderr, runner); err != nil {
			// No matrix is encoded or published on a subprocess failure. In
			// particular, a stale or partial run can never become a new PASS.
			return err
		}
		if opts.profileAlias != "" {
			return nil
		}
	default:
		return fmt.Errorf("unsupported mode %q", opts.mode)
	}

	probes, err := aggregateEvidence(opts.evidenceDir, plan, inputs.registry)
	if err != nil {
		return err
	}
	matrix, err := buildRouteMatrix(inputs.registry.ContractRelease, inputs.registrySHA256,
		inputs.intersectionSHA256, plan, probes)
	if err != nil {
		return err
	}
	encoded, err := encodeRouteMatrix(matrix)
	if err != nil {
		return err
	}
	if err := publish(opts.outputPath, encoded, opts.verifyOnly); err != nil {
		return err
	}
	if matrix.Status != "pass" {
		return fmt.Errorf("%w: %d of %d executed probes failed", errRouteMatrixFailed,
			matrix.FailedProbeCount, matrix.ExecutedProbeCount)
	}
	return nil
}

func loadInputs(opts options) (inputSet, error) {
	registryPath := resolvePath(opts.root, opts.registryPath)
	intersectionPath := resolvePath(opts.root, opts.intersectionPath)
	registryPayload, registryDigest, err := readWithSHA256(registryPath)
	if err != nil {
		return inputSet{}, fmt.Errorf("profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := decodeStrict(registryPayload, &registry); err != nil {
		return inputSet{}, fmt.Errorf("profile registry: %w", err)
	}
	intersectionPayload, intersectionDigest, err := readWithSHA256(intersectionPath)
	if err != nil {
		return inputSet{}, fmt.Errorf("product-intersection matrix: %w", err)
	}
	var intersection intersectionReport
	if err := decodeStrict(intersectionPayload, &intersection); err != nil {
		return inputSet{}, fmt.Errorf("product-intersection matrix: %w", err)
	}
	if err := validateIntersection(registry, intersection); err != nil {
		return inputSet{}, err
	}
	return inputSet{registry: registry, intersection: intersection, registryPath: registryPath,
		intersectionPath: intersectionPath, registrySHA256: registryDigest,
		intersectionSHA256: intersectionDigest}, nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}

func readWithSHA256(path string) ([]byte, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(payload)
	return payload, hex.EncodeToString(digest[:]), nil
}

func decodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("document contains more than one JSON value")
		}
		return err
	}
	return nil
}

type intersectionReport struct {
	SchemaVersion    int                `json:"schema_version"`
	Record           string             `json:"record"`
	ContractsVersion string             `json:"contracts_version"`
	RegistryVersion  string             `json:"registry_version"`
	ProfileCount     int                `json:"profile_count"`
	PairCount        int                `json:"pair_count"`
	OverlappingPairs int                `json:"overlapping_pair_count"`
	Pairs            []intersectionPair `json:"pairs"`
}

type intersectionPair struct {
	LeftProfileID               string   `json:"left_profile_id"`
	RightProfileID              string   `json:"right_profile_id"`
	LeftAlias                   string   `json:"left_alias"`
	RightAlias                  string   `json:"right_alias"`
	LeftProducts                []string `json:"left_products"`
	RightProducts               []string `json:"right_products"`
	Intersection                []string `json:"intersection"`
	IntersectionCount           int      `json:"intersection_count"`
	SameQueryLiveTestApplicable bool     `json:"same_query_live_test_applicable"`
}

func validateIntersection(registry finalv5profile.Registry, observed intersectionReport) error {
	if registry.SchemaVersion != 1 || strings.TrimSpace(registry.RegistryVersion) == "" ||
		strings.TrimSpace(registry.ContractRelease) == "" {
		return errors.New("profile registry header is invalid")
	}
	expected, err := expectedIntersection(registry)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observed, expected) {
		return errors.New("product-intersection matrix is not the exact derivation of the cleared registry profiles")
	}
	return nil
}

func expectedIntersection(registry finalv5profile.Registry) (intersectionReport, error) {
	profiles, err := clearedProfiles(registry)
	if err != nil {
		return intersectionReport{}, err
	}
	report := intersectionReport{SchemaVersion: 1, Record: intersectionRecord,
		ContractsVersion: registry.ContractRelease, RegistryVersion: registry.RegistryVersion,
		ProfileCount: len(profiles), Pairs: []intersectionPair{}}
	for left := 0; left < len(profiles); left++ {
		for right := left + 1; right < len(profiles); right++ {
			shared := intersection(profiles[left].Closure.Products, profiles[right].Closure.Products)
			pair := intersectionPair{LeftProfileID: profiles[left].ID, RightProfileID: profiles[right].ID,
				LeftAlias: profiles[left].Alias, RightAlias: profiles[right].Alias,
				LeftProducts:  append([]string(nil), profiles[left].Closure.Products...),
				RightProducts: append([]string(nil), profiles[right].Closure.Products...),
				Intersection:  shared, IntersectionCount: len(shared),
				SameQueryLiveTestApplicable: len(shared) > 0}
			if pair.SameQueryLiveTestApplicable {
				report.OverlappingPairs++
			}
			report.Pairs = append(report.Pairs, pair)
		}
	}
	report.PairCount = len(report.Pairs)
	return report, nil
}

func clearedProfiles(registry finalv5profile.Registry) ([]finalv5profile.Profile, error) {
	seenIDs := map[string]bool{}
	seenAliases := map[string]bool{}
	var profiles []finalv5profile.Profile
	for _, profile := range registry.Profiles {
		if strings.TrimSpace(profile.ID) == "" || seenIDs[profile.ID] {
			return nil, fmt.Errorf("profile registry has an empty or duplicate profile ID %q", profile.ID)
		}
		if !safeAliasBasename(profile.Alias) || seenAliases[profile.Alias] {
			return nil, fmt.Errorf("profile registry has an empty or duplicate alias %q", profile.Alias)
		}
		seenIDs[profile.ID], seenAliases[profile.Alias] = true, true
		products, err := canonicalNames(profile.Closure.Products)
		if err != nil {
			return nil, fmt.Errorf("profile %s Products: %w", profile.ID, err)
		}
		if !reflect.DeepEqual(products, profile.Closure.Products) {
			return nil, fmt.Errorf("profile %s Products are not in canonical order", profile.ID)
		}
		if profile.Status.ClosureComplete && profile.Status.CatalogMaterializable &&
			profile.Status.LiveRouteAvailable {
			if !validSHA256(profile.CatalogSHA256) || !validSHA256(profile.Closure.SHA256) {
				return nil, fmt.Errorf("cleared profile %s has an invalid Catalog or closure digest", profile.ID)
			}
			profiles = append(profiles, profile)
		}
	}
	if len(profiles) == 0 {
		return nil, errors.New("profile registry has no Catalog-and-route-cleared profiles")
	}
	sort.Slice(profiles, func(left, right int) bool { return profiles[left].ID < profiles[right].ID })
	return profiles, nil
}

func safeAliasBasename(alias string) bool {
	return alias != "" && alias == strings.TrimSpace(alias) && alias != "." && alias != ".." &&
		filepath.Base(alias) == alias && !strings.ContainsAny(alias, "/\\:\x00")
}

func canonicalNames(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("set is empty")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != strings.TrimSpace(value) || value == "" || seen[value] {
			return nil, fmt.Errorf("set has an empty, non-canonical, or duplicate member %q", value)
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func intersection(left, right []string) []string {
	present := make(map[string]bool, len(right))
	for _, value := range right {
		present[value] = true
	}
	shared := []string{}
	for _, value := range left {
		if present[value] {
			shared = append(shared, value)
		}
	}
	sort.Strings(shared)
	return shared
}

type probePlan struct {
	SchemaVersion                   int           `json:"schema_version"`
	Record                          string        `json:"record"`
	ContractRelease                 string        `json:"contract_release"`
	ProfileRegistrySHA256           string        `json:"profile_registry_sha256"`
	ProductIntersectionMatrixSHA256 string        `json:"product_intersection_matrix_sha256"`
	ProfileCount                    int           `json:"profile_count"`
	UniqueProductCount              int           `json:"unique_product_count"`
	ExpectedProbeCount              int           `json:"expected_probe_count"`
	Profiles                        []profilePlan `json:"profiles"`
}

type profilePlan struct {
	ProfileID       string   `json:"profile_id"`
	ProfileAlias    string   `json:"profile_alias"`
	ClosureSHA256   string   `json:"closure_sha256"`
	CatalogSHA256   string   `json:"catalog_sha256"`
	ClosureProducts []string `json:"closure_products"`
	OutsideProducts []string `json:"outside_products"`
}

func deriveProbePlan(registry finalv5profile.Registry, registryDigest, intersectionDigest string) (probePlan, error) {
	profiles, err := clearedProfiles(registry)
	if err != nil {
		return probePlan{}, err
	}
	all := map[string]bool{}
	for _, profile := range profiles {
		for _, product := range profile.Closure.Products {
			all[product] = true
		}
	}
	universe := make([]string, 0, len(all))
	for product := range all {
		universe = append(universe, product)
	}
	sort.Strings(universe)
	plan := probePlan{SchemaVersion: 1, Record: planRecord, ContractRelease: registry.ContractRelease,
		ProfileRegistrySHA256: registryDigest, ProductIntersectionMatrixSHA256: intersectionDigest,
		ProfileCount: len(profiles), UniqueProductCount: len(universe), Profiles: []profilePlan{}}
	for _, profile := range profiles {
		own := make(map[string]bool, len(profile.Closure.Products))
		for _, product := range profile.Closure.Products {
			own[product] = true
		}
		outside := make([]string, 0, len(universe)-len(own))
		for _, product := range universe {
			if !own[product] {
				outside = append(outside, product)
			}
		}
		if len(outside) == 0 {
			return probePlan{}, fmt.Errorf("profile %s has no outside Product to probe", profile.ID)
		}
		plan.Profiles = append(plan.Profiles, profilePlan{ProfileID: profile.ID,
			ProfileAlias: profile.Alias, ClosureSHA256: profile.Closure.SHA256,
			CatalogSHA256:   profile.CatalogSHA256,
			ClosureProducts: append([]string(nil), profile.Closure.Products...), OutsideProducts: outside})
		plan.ExpectedProbeCount += len(outside)
	}
	return plan, nil
}

type collectedActivationEvidence struct {
	name     string
	evidence finalv5profile.ActivationEvidence
	plan     profilePlan
	profile  finalv5profile.Profile
}

func aggregateEvidence(directory string, plan probePlan, registry finalv5profile.Registry) ([]routeProbe, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("activation evidence directory: %w", err)
	}
	expected := make(map[string]profilePlan, len(plan.Profiles))
	registryProfiles := make(map[string]finalv5profile.Profile, len(registry.Profiles))
	for _, profile := range registry.Profiles {
		registryProfiles[profile.ID] = profile
	}
	for _, profile := range plan.Profiles {
		expected[profile.ProfileID] = profile
	}
	seen := map[string]bool{}
	var collected []collectedActivationEvidence
	jsonFiles := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		jsonFiles++
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var evidence finalv5profile.ActivationEvidence
		if err := decodeStrict(payload, &evidence); err != nil {
			return nil, fmt.Errorf("activation evidence %s: %w", entry.Name(), err)
		}
		profile, ok := expected[evidence.ProfileID]
		if !ok {
			return nil, fmt.Errorf("activation evidence %s names non-participating profile %q", entry.Name(), evidence.ProfileID)
		}
		if seen[evidence.ProfileID] {
			return nil, fmt.Errorf("activation evidence lists profile %s more than once", evidence.ProfileID)
		}
		if entry.Name() != evidence.ProfileAlias+".json" {
			return nil, fmt.Errorf("activation evidence %s does not use profile alias %q as its basename",
				entry.Name(), evidence.ProfileAlias)
		}
		registryProfile, ok := registryProfiles[evidence.ProfileID]
		if !ok {
			return nil, fmt.Errorf("activation evidence %s names unregistered profile %q", entry.Name(), evidence.ProfileID)
		}
		if err := finalv5profile.ValidateActivationEvidence(evidence, registryProfile); err != nil {
			return nil, fmt.Errorf("activation evidence %s: %w", entry.Name(), err)
		}
		seen[evidence.ProfileID] = true
		collected = append(collected, collectedActivationEvidence{name: entry.Name(), evidence: evidence,
			plan: profile, profile: registryProfile})
	}
	if jsonFiles != len(plan.Profiles) {
		return nil, fmt.Errorf("activation evidence directory has %d JSON files, expected %d", jsonFiles, len(plan.Profiles))
	}
	for _, profile := range plan.Profiles {
		if !seen[profile.ProfileID] {
			return nil, fmt.Errorf("activation evidence is missing profile %s", profile.ProfileID)
		}
	}
	sort.Slice(collected, func(left, right int) bool {
		return collected[left].evidence.ActivationSequence < collected[right].evidence.ActivationSequence
	})
	deploymentID := ""
	for index, record := range collected {
		evidence := record.evidence
		if evidence.DeploymentID == "" || evidence.DeploymentID != strings.TrimSpace(evidence.DeploymentID) {
			return nil, fmt.Errorf("activation evidence %s has an empty or non-canonical deployment_id", record.name)
		}
		if deploymentID == "" {
			deploymentID = evidence.DeploymentID
		} else if evidence.DeploymentID != deploymentID {
			return nil, fmt.Errorf("activation evidence %s belongs to deployment %q, expected %q",
				record.name, evidence.DeploymentID, deploymentID)
		}
		if evidence.ActivationSequence < 1 {
			return nil, fmt.Errorf("activation evidence %s has non-positive activation_sequence %d",
				record.name, evidence.ActivationSequence)
		}
		if index > 0 {
			previous := collected[index-1].evidence
			if evidence.ActivationSequence != previous.ActivationSequence+1 {
				return nil, fmt.Errorf("activation evidence sequences are not unique and contiguous: %d follows %d",
					evidence.ActivationSequence, previous.ActivationSequence)
			}
			if evidence.PreviousProfileID != previous.ProfileID {
				return nil, fmt.Errorf("activation evidence %s previous_profile_id=%q, expected %q",
					record.name, evidence.PreviousProfileID, previous.ProfileID)
			}
		}
	}
	var probes []routeProbe
	for _, record := range collected {
		projected, err := projectEvidence(record.evidence, record.plan, registry.ContractRelease)
		if err != nil {
			return nil, fmt.Errorf("activation evidence %s: %w", record.name, err)
		}
		probes = append(probes, projected...)
	}
	sort.Slice(probes, func(left, right int) bool {
		if probes[left].TargetProfileAlias != probes[right].TargetProfileAlias {
			return probes[left].TargetProfileAlias < probes[right].TargetProfileAlias
		}
		return probes[left].RequestedProduct < probes[right].RequestedProduct
	})
	return probes, nil
}

func projectEvidence(evidence finalv5profile.ActivationEvidence, profile profilePlan, contractRelease string) ([]routeProbe, error) {
	if evidence.SchemaVersion != 1 || evidence.Record != activationRecord || evidence.ContractRelease != contractRelease {
		return nil, errors.New("activation evidence header or contract release is invalid")
	}
	if evidence.ProfileAlias != profile.ProfileAlias || evidence.CatalogSHA256 != profile.CatalogSHA256 ||
		evidence.ClosureSHA256 != profile.ClosureSHA256 {
		return nil, errors.New("activation evidence identifies a different profile, Catalog, or closure")
	}
	expected := make(map[string]bool, len(profile.OutsideProducts))
	for _, product := range profile.OutsideProducts {
		expected[product] = true
	}
	if len(evidence.OutsideProduct) != len(expected) {
		return nil, fmt.Errorf("profile %s has %d probes, expected %d", profile.ProfileID,
			len(evidence.OutsideProduct), len(expected))
	}
	healthyEvidence := evidence.Status == "pass" && evidence.ActivationSmokePassed &&
		!evidence.PublicationEligible && len(evidence.Failures) == 0
	seen := map[string]bool{}
	result := make([]routeProbe, 0, len(evidence.OutsideProduct))
	for _, source := range evidence.OutsideProduct {
		if !expected[source.Product] || seen[source.Product] {
			return nil, fmt.Errorf("profile %s has an unexpected or duplicate outside Product %q", profile.ProfileID, source.Product)
		}
		seen[source.Product] = true
		if source.RequestedProductSHA256 != requestedProductSHA256(source.Product) {
			return nil, fmt.Errorf("outside Product %s has the wrong requested-Product digest", source.Product)
		}
		stableRefusal := stableRefusalClassification(source.Classification)
		closedBeforeTask := healthyEvidence && source.CatalogListAbsent && source.LiveRequestRefused &&
			source.Refused && stableRefusal && validSHA256(source.ResponseSHA256) &&
			source.Observed == "the activated Catalog does not publish this Product"
		result = append(result, routeProbe{CatalogListAbsent: source.CatalogListAbsent,
			LiveRequestRefused: source.LiveRequestRefused,
			NoActiveTask:       closedBeforeTask, NoArtifact: closedBeforeTask, NoAvailable: closedBeforeTask,
			NoBusinessSQL: closedBeforeTask, NoObservation: closedBeforeTask, NoReceipt: closedBeforeTask,
			NoRootLedgerChange: closedBeforeTask, NoSemanticCacheHit: closedBeforeTask,
			RequestedProduct: source.Product, RequestedProductSHA256: source.RequestedProductSHA256,
			ResponseSHA256: source.ResponseSHA256, StableRefusalClassification: source.Classification,
			TargetCatalogSHA256: profile.CatalogSHA256, TargetProfileAlias: profile.ProfileAlias,
			TargetProfileID: profile.ProfileID})
	}
	return result, nil
}

func requestedProductSHA256(product string) string {
	digest := sha256.Sum256([]byte(productDigestDomain + product))
	return hex.EncodeToString(digest[:])
}

func stableRefusalClassification(value string) bool {
	return value == "tool_error"
}

// routeMatrix and routeProbe are ordered to match the existing v1 evidence
// serialization byte for byte. Do not reorder fields without changing the
// record schema and its golden fixture.
type routeMatrix struct {
	ContractRelease                 string       `json:"contract_release"`
	ExecutedProbeCount              int          `json:"executed_probe_count"`
	ExpectedProbeCount              int          `json:"expected_probe_count"`
	FailedProbeCount                int          `json:"failed_probe_count"`
	MatrixSHA256                    string       `json:"matrix_sha256,omitempty"`
	PassedProbeCount                int          `json:"passed_probe_count"`
	Probes                          []routeProbe `json:"probes"`
	ProductIntersectionMatrixSHA256 string       `json:"product_intersection_matrix_sha256"`
	ProfileCount                    int          `json:"profile_count"`
	ProfileRegistrySHA256           string       `json:"profile_registry_sha256"`
	Record                          string       `json:"record"`
	SchemaVersion                   int          `json:"schema_version"`
	Status                          string       `json:"status"`
	UniqueProductCount              int          `json:"unique_product_count"`
}

type routeProbe struct {
	CatalogListAbsent           bool   `json:"catalog_list_absent"`
	LiveRequestRefused          bool   `json:"live_request_refused"`
	NoActiveTask                bool   `json:"no_active_task"`
	NoArtifact                  bool   `json:"no_artifact"`
	NoAvailable                 bool   `json:"no_available"`
	NoBusinessSQL               bool   `json:"no_business_sql"`
	NoObservation               bool   `json:"no_observation"`
	NoReceipt                   bool   `json:"no_receipt"`
	NoRootLedgerChange          bool   `json:"no_root_ledger_change"`
	NoSemanticCacheHit          bool   `json:"no_semantic_cache_hit"`
	RequestedProduct            string `json:"requested_product"`
	RequestedProductSHA256      string `json:"requested_product_sha256"`
	ResponseSHA256              string `json:"response_sha256"`
	StableRefusalClassification string `json:"stable_refusal_classification"`
	TargetCatalogSHA256         string `json:"target_catalog_sha256"`
	TargetProfileAlias          string `json:"target_profile_alias"`
	TargetProfileID             string `json:"target_profile_id"`
}

func buildRouteMatrix(contractRelease, registryDigest, intersectionDigest string,
	plan probePlan, probes []routeProbe) (routeMatrix, error) {
	if !validSHA256(registryDigest) || !validSHA256(intersectionDigest) {
		return routeMatrix{}, errors.New("route matrix inputs do not carry lowercase SHA-256 digests")
	}
	matrix := routeMatrix{ContractRelease: contractRelease, ExpectedProbeCount: plan.ExpectedProbeCount,
		ExecutedProbeCount: len(probes), Probes: append([]routeProbe(nil), probes...),
		ProductIntersectionMatrixSHA256: intersectionDigest, ProfileCount: plan.ProfileCount,
		ProfileRegistrySHA256: registryDigest, Record: routeMatrixRecord, SchemaVersion: 1,
		UniqueProductCount: plan.UniqueProductCount}
	sort.Slice(matrix.Probes, func(left, right int) bool {
		if matrix.Probes[left].TargetProfileAlias != matrix.Probes[right].TargetProfileAlias {
			return matrix.Probes[left].TargetProfileAlias < matrix.Probes[right].TargetProfileAlias
		}
		return matrix.Probes[left].RequestedProduct < matrix.Probes[right].RequestedProduct
	})
	for _, probe := range matrix.Probes {
		if routeProbePassed(probe) {
			matrix.PassedProbeCount++
		} else {
			matrix.FailedProbeCount++
		}
	}
	if matrix.ExecutedProbeCount == matrix.ExpectedProbeCount && matrix.FailedProbeCount == 0 {
		matrix.Status = "pass"
	} else {
		matrix.Status = "fail"
	}
	payload, err := json.MarshalIndent(matrix, "", "  ") // MatrixSHA256 is omitted while empty.
	if err != nil {
		return routeMatrix{}, err
	}
	digest := sha256.Sum256(payload) // The v1 digest excludes a trailing LF.
	matrix.MatrixSHA256 = hex.EncodeToString(digest[:])
	return matrix, nil
}

func routeProbePassed(probe routeProbe) bool {
	return probe.CatalogListAbsent && probe.LiveRequestRefused && probe.NoActiveTask && probe.NoArtifact &&
		probe.NoAvailable && probe.NoBusinessSQL && probe.NoObservation && probe.NoReceipt &&
		probe.NoRootLedgerChange && probe.NoSemanticCacheHit && probe.RequestedProduct != "" &&
		probe.RequestedProductSHA256 == requestedProductSHA256(probe.RequestedProduct) &&
		validSHA256(probe.ResponseSHA256) && stableRefusalClassification(probe.StableRefusalClassification) &&
		validSHA256(probe.TargetCatalogSHA256) && probe.TargetProfileAlias != "" && probe.TargetProfileID != ""
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func encodeRouteMatrix(matrix routeMatrix) ([]byte, error) {
	if !validSHA256(matrix.MatrixSHA256) {
		return nil, errors.New("route matrix has no valid matrix_sha256")
	}
	payload, err := json.MarshalIndent(matrix, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func encodeIndented(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func publish(path string, payload []byte, verifyOnly bool) error {
	if path == "" {
		return errors.New("output path is empty")
	}
	if verifyOnly {
		existing, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, payload) {
			return errors.New("route-matrix output is not byte-identical to regeneration")
		}
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".final-v5-route-matrix-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

type subprocessRunner func(ctx context.Context, directory, executable string, arguments []string,
	stdout, stderr io.Writer) error

func runSubprocess(ctx context.Context, directory, executable string, arguments []string,
	stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir, command.Stdout, command.Stderr = directory, stdout, stderr
	return command.Run()
}

func validateLiveOptions(opts options) error {
	if opts.evidenceDir == "" || (opts.outputPath == "" && opts.profileAlias == "") || opts.composeProject == "" ||
		opts.deploymentID == "" || opts.profileArtifactRoot == "" || opts.profileArtifactManifest == "" {
		return errors.New("live requires activation-evidence-dir, out, compose-project, deployment-id, " +
			"profile-artifact-root, and profile-artifact-manifest")
	}
	if opts.verifyOnly {
		return errors.New("live does not accept -verify")
	}
	if opts.activationSequenceStart < 1 {
		return errors.New("activation-sequence-start must be positive")
	}
	for label, value := range map[string]string{
		"admin-token-env": opts.adminTokenEnv, "probe-token-env": opts.probeTokenEnv,
		"business-dsn-env": opts.businessDSNEnv,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("live %s is empty", label)
		}
	}
	return nil
}

func executeLive(ctx context.Context, opts options, plan probePlan, stderr io.Writer, runner subprocessRunner) error {
	evidenceDirectory := resolvePath(opts.root, opts.evidenceDir)
	registryArgument, err := pathWithinRoot(opts.root, resolvePath(opts.root, opts.registryPath))
	if err != nil {
		return fmt.Errorf("live registry path: %w", err)
	}
	attestationsArgument, err := pathWithinRoot(opts.root, resolvePath(opts.root, opts.schemaAttestations))
	if err != nil {
		return fmt.Errorf("live schema-attestations path: %w", err)
	}
	artifactRoot := resolvePath(opts.root, opts.profileArtifactRoot)
	artifactManifest := resolvePath(opts.root, opts.profileArtifactManifest)
	datasetBinding := ""
	if opts.datasetBinding != "" {
		datasetBinding = resolvePath(opts.root, opts.datasetBinding)
	}
	if err := preflightLiveEnvironment(opts, plan, evidenceDirectory, artifactRoot, artifactManifest); err != nil {
		return err
	}
	if err := os.MkdirAll(evidenceDirectory, 0o700); err != nil {
		return err
	}
	previous := opts.previousProfileID
	for index, profile := range plan.Profiles {
		evidencePath := filepath.Join(evidenceDirectory, profile.ProfileAlias+".json")
		arguments, err := (finalv5profile.ActivationInvocation{Root: opts.root,
			ComposeProject: opts.composeProject, ComposeFiles: opts.composeFiles,
			DeploymentID: opts.deploymentID, ProfileID: profile.ProfileID, RegistryPath: registryArgument,
			GatewayURL: opts.gatewayURL, AdminTokenEnv: opts.adminTokenEnv, PreviousProfileID: previous,
			Sequence: opts.activationSequenceStart + index, EvidenceOut: evidencePath,
			OutsideProducts:         strings.Join(profile.OutsideProducts, ","),
			ProfileArtifactDir:      filepath.Join(artifactRoot, profile.ProfileID),
			ProfileArtifactManifest: artifactManifest, BusinessDSNEnv: opts.businessDSNEnv,
			SchemaAttestations: attestationsArgument, ProbeTokenEnv: opts.probeTokenEnv,
			ReadyTimeout: opts.readyTimeout, DatasetBinding: datasetBinding}).Arguments()
		if err != nil {
			return fmt.Errorf("activation invocation for profile %s: %w", profile.ProfileAlias, err)
		}
		executable := opts.activatorBinary
		if executable == "" {
			executable = "go"
			arguments = append([]string{"run", "-buildvcs=false", "./evaluation/cmd/final-v5-profile-activate"}, arguments...)
		}
		if err := runner(ctx, opts.root, executable, arguments, stderr, stderr); err != nil {
			return fmt.Errorf("activate profile %s: %w", profile.ProfileAlias, err)
		}
		previous = profile.ProfileID
	}
	return nil
}

func preflightLiveEnvironment(opts options, plan probePlan, evidenceDirectory, artifactRoot,
	artifactManifest string) error {
	for _, required := range []struct {
		label string
		name  string
	}{
		{label: "admin token", name: opts.adminTokenEnv},
		{label: "outside-Product probe token", name: opts.probeTokenEnv},
		{label: "Business PostgreSQL DSN", name: opts.businessDSNEnv},
	} {
		value, present := os.LookupEnv(required.name)
		if !present || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s environment variable %s is unset or empty",
				errLiveEnvironmentSkipped, required.label, required.name)
		}
	}
	if err := requireLivePath("profile artifact root", artifactRoot, true); err != nil {
		return err
	}
	if err := requireLivePath("profile artifact manifest", artifactManifest, false); err != nil {
		return err
	}
	for _, profile := range plan.Profiles {
		if !safeAliasBasename(profile.ProfileAlias) {
			return fmt.Errorf("profile alias %q is not a safe basename", profile.ProfileAlias)
		}
		if err := requireLivePath("profile artifact directory for "+profile.ProfileAlias,
			filepath.Join(artifactRoot, profile.ProfileID), true); err != nil {
			return err
		}
		evidencePath := filepath.Join(evidenceDirectory, profile.ProfileAlias+".json")
		if _, err := os.Stat(evidencePath); err == nil {
			return fmt.Errorf("refusing to overwrite existing activation evidence %s", evidencePath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect activation evidence target %s: %w", evidencePath, err)
		}
	}
	return nil
}

func requireLivePath(label, path string, directory bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s %s does not exist", errLiveEnvironmentSkipped, label, path)
	}
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", label, path, err)
	}
	if info.IsDir() != directory {
		kind := "file"
		if directory {
			kind = "directory"
		}
		return fmt.Errorf("%w: %s %s is not a %s", errLiveEnvironmentSkipped, label, path, kind)
	}
	return nil
}

func pathWithinRoot(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside repository root")
	}
	return relative, nil
}
