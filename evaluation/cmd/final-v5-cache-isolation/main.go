// Command final-v5-cache-isolation composes the Final-V5 semantic-cache
// isolation evidence from the formal profile intersection, the exhaustive
// outside-Product route matrix and the PostgreSQL production-lookup proof.
//
// The command deliberately re-derives every count and predicate it publishes.
// Input summary fields are consistency checks, never authorities. Digests bind
// the exact input bytes, including their final newline.
package main

import (
	"bufio"
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
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	defaultRegistryPath         = "config/profiles/registry.json"
	defaultIntersectionPath     = "evaluation/final-v5-wsl2/profiles/product-intersection-v1.json"
	defaultRouteMatrixPath      = "evaluation/final-v5-wsl2/profiles/outside-product-route-matrix-v1.json"
	defaultProductionLookupPath = "evaluation/final-v5-wsl2/profiles/production-lookup-manifest-v1.json"
	defaultSameQueryLivePath    = ""

	intersectionRecord     = "taskgate-final-v5-product-intersection-v1"
	routeMatrixRecord      = "taskgate-final-v5-outside-product-route-matrix-v1"
	productionLookupRecord = "taskgate-final-v5-production-semantic-cache-lookup-v1"
	isolationRecord        = "taskgate-final-v5-semantic-cache-isolation-evidence-v1"
	isolationRecordV2      = "taskgate-final-v5-semantic-cache-isolation-evidence-v2"
	sameQueryLiveRecord    = "taskgate-final-v5-same-query-cross-profile-live-evidence-v1"
	sameQueryLiveRecordV2  = "taskgate-final-v5-same-query-cross-profile-live-evidence-v2"
	proofMode              = "disjoint_formal_profiles_plus_exhaustive_live_route_refusal_plus_production_lookup"
	proofModeV2            = "formal_profile_intersection_plus_same_query_cross_profile_live_plus_exhaustive_live_route_refusal_plus_production_lookup"
	notApplicableStatus    = "not_applicable_by_formal_profile_contract"
	requiredStatus         = "required_but_not_provided"
	passedStatus           = "pass"

	productionTestPackage         = "taskbound.local/agent-data-gateway/internal/control"
	productionTestPackageArgument = "./internal/control"
	productionTestOne             = "TestSemanticCacheMissesUnderAChangedProfileBinding"
	productionTestTwo             = "TestSemanticCacheLookupRequiresACompleteBinding"
	productionTestPattern         = "^(TestSemanticCacheMissesUnderAChangedProfileBinding|TestSemanticCacheLookupRequiresACompleteBinding)$"
	requestedProductDigestDomain  = "taskgate-final-v5-outside-product-probe-v1\x00"
)

var (
	errProductionTestsSkipped  = errors.New("production semantic-cache lookup tests skipped")
	errIsolationEvidenceFailed = errors.New("semantic-cache isolation evidence failed")
)

type options struct {
	root                 string
	registryPath         string
	intersectionPath     string
	routeMatrixPath      string
	productionLookupPath string
	sameQueryLivePath    string
	outputPath           string
	runProductionTests   bool
}

func main() {
	var opts options
	flag.StringVar(&opts.root, "root", ".", "repository root")
	flag.StringVar(&opts.registryPath, "registry", defaultRegistryPath, "profile registry input")
	flag.StringVar(&opts.intersectionPath, "product-intersection", defaultIntersectionPath,
		"product-intersection input")
	flag.StringVar(&opts.routeMatrixPath, "route-matrix", defaultRouteMatrixPath,
		"outside-Product route-matrix input")
	flag.StringVar(&opts.productionLookupPath, "production-lookup-manifest", defaultProductionLookupPath,
		"production semantic-cache lookup manifest input")
	flag.StringVar(&opts.sameQueryLivePath, "same-query-live-evidence", defaultSameQueryLivePath,
		"same-query cross-profile live evidence input (required when profile closures overlap)")
	flag.StringVar(&opts.outputPath, "out", "", "semantic-cache isolation output (required)")
	flag.BoolVar(&opts.runProductionTests, "run-production-tests", false,
		"rerun the two PostgreSQL production-lookup tests before composing evidence")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "final-v5-cache-isolation: positional arguments are not accepted")
		os.Exit(2)
	}
	if err := run(context.Background(), opts, runProductionTests); err != nil {
		if errors.Is(err, errProductionTestsSkipped) {
			fmt.Fprintln(os.Stderr, "final-v5-cache-isolation: SKIPPED:", err)
			// A distinct non-zero exit status prevents a caller from mistaking a
			// skipped database proof for a passing evidence generation.
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "final-v5-cache-isolation:", err)
		os.Exit(1)
	}
}

type productionTestRunner func(context.Context, string) error

func run(ctx context.Context, opts options, testRunner productionTestRunner) error {
	if strings.TrimSpace(opts.root) == "" {
		return errors.New("repository root is empty")
	}
	if strings.TrimSpace(opts.outputPath) == "" {
		return errors.New("-out is required")
	}
	absoluteRoot, err := filepath.Abs(opts.root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	opts.root = filepath.Clean(absoluteRoot)
	if opts.runProductionTests {
		if err := testRunner(ctx, opts.root); err != nil {
			return err
		}
	}

	registryPath := resolvePath(opts.root, opts.registryPath)
	intersectionPath := resolvePath(opts.root, opts.intersectionPath)
	routeMatrixPath := resolvePath(opts.root, opts.routeMatrixPath)
	productionLookupPath := resolvePath(opts.root, opts.productionLookupPath)
	var sameQueryLivePath string
	if strings.TrimSpace(opts.sameQueryLivePath) != "" {
		sameQueryLivePath = resolvePath(opts.root, opts.sameQueryLivePath)
	}
	outputPath := resolvePath(opts.root, opts.outputPath)

	registryBytes, registryDigest, err := readWithDigest(registryPath)
	if err != nil {
		return err
	}
	intersectionBytes, intersectionDigest, err := readWithDigest(intersectionPath)
	if err != nil {
		return err
	}
	routeBytes, routeDigest, err := readWithDigest(routeMatrixPath)
	if err != nil {
		return err
	}
	productionBytes, productionDigest, err := readWithDigest(productionLookupPath)
	if err != nil {
		return err
	}

	var registry registryDocument
	if err := decodeJSON(registryBytes, &registry, false); err != nil {
		return fmt.Errorf("decode profile registry: %w", err)
	}
	profiles, err := validateRegistry(registry)
	if err != nil {
		return err
	}
	var intersection intersectionDocument
	if err := decodeJSON(intersectionBytes, &intersection, true); err != nil {
		return fmt.Errorf("decode product-intersection matrix: %w", err)
	}
	intersectionResult, err := analyzeIntersection(intersection, registry, profiles)
	if err != nil {
		return err
	}

	var route routeMatrixDocument
	if err := decodeJSON(routeBytes, &route, true); err != nil {
		return fmt.Errorf("decode outside-product route matrix: %w", err)
	}
	routeResult, err := analyzeRouteMatrix(route, registry, profiles, registryDigest, intersectionDigest)
	if err != nil {
		return err
	}

	var production productionLookupManifest
	if err := decodeJSON(productionBytes, &production, true); err != nil {
		return fmt.Errorf("decode production lookup manifest: %w", err)
	}
	productionFailures, err := analyzeProductionLookup(production)
	if err != nil {
		return err
	}

	failures := append([]string{}, intersectionResult.Failures...)
	failures = append(failures, routeResult.Failures...)
	failures = append(failures, productionFailures...)
	var sameQueryLiveDigest string
	var sameQueryLivePairCount, sameQueryLivePairFailures int
	var sameQueryLivePassed bool
	if intersectionResult.SameQueryLiveTestApplicable && sameQueryLivePath == "" {
		failures = append(failures,
			"same-query cross-profile live test is applicable but no such live evidence was provided")
	} else if intersectionResult.SameQueryLiveTestApplicable {
		liveBytes, digest, readErr := readWithDigest(sameQueryLivePath)
		if readErr != nil {
			return readErr
		}
		var live sameQueryLiveDocument
		if err := decodeJSON(liveBytes, &live, true); err != nil {
			return fmt.Errorf("decode same-query cross-profile live evidence: %w", err)
		}
		routingIdentity := ""
		if live.SchemaVersion == 2 && live.Record == sameQueryLiveRecordV2 {
			routingIdentity, err = finalv5profile.ProfileRoutingIdentitySHA256(registryBytes)
			if err != nil {
				return err
			}
		}
		liveResult, err := analyzeSameQueryLive(live, registry, profiles, intersection,
			registryDigest, routingIdentity, intersectionDigest)
		if err != nil {
			return err
		}
		sameQueryLiveDigest = digest
		sameQueryLivePairCount = liveResult.PairCount
		sameQueryLivePairFailures = liveResult.FailedPairCount
		sameQueryLivePassed = len(liveResult.Failures) == 0
		failures = append(failures, liveResult.Failures...)
	}

	evidence := semanticCacheIsolationEvidence{
		ChangedCatalogMiss:                    production.ChangedCatalogMiss,
		ChangedGrantMiss:                      production.ChangedGrantMiss,
		ChangedPublicationOrDictionarySetMiss: production.ChangedPublicationOrDictionarySetMiss,
		ChangedTaskMiss:                       production.ChangedTaskMiss,
		ContractRelease:                       registry.ContractRelease,
		Failures:                              failures,
		IncompleteBindingRejected:             production.IncompleteBindingRejected,
		LiveRouteProbeCount:                   routeResult.ExecutedProbeCount,
		LiveRouteProbeFailures:                routeResult.FailedProbeCount,
		OutsideProductRouteMatrixSHA256:       routeDigest,
		OverlappingPairCount:                  intersectionResult.OverlappingPairCount,
		ProductIntersectionMatrixSHA256:       intersectionDigest,
		ProductionLookupManifestSHA256:        productionDigest,
		ProfilePairCount:                      intersectionResult.PairCount,
		ProfileRegistrySHA256:                 registryDigest,
		ProofMode:                             proofMode,
		PublicationEligible:                   false,
		Record:                                isolationRecord,
		SameBindingHit:                        production.SameBindingHit,
		SameQueryLiveTestApplicable:           intersectionResult.SameQueryLiveTestApplicable,
		SameQueryLiveTestStatus:               notApplicableStatus,
		SchemaVersion:                         1,
	}
	if evidence.SameQueryLiveTestApplicable {
		evidence.SameQueryLiveTestStatus = requiredStatus
		if sameQueryLivePath != "" {
			evidence.SchemaVersion = 2
			evidence.Record = isolationRecordV2
			evidence.ProofMode = proofModeV2
			evidence.SameQueryLiveEvidenceSHA256 = sameQueryLiveDigest
			evidence.SameQueryLivePairCount = &sameQueryLivePairCount
			evidence.SameQueryLivePairFailures = &sameQueryLivePairFailures
			if sameQueryLivePassed {
				evidence.SameQueryLiveTestStatus = passedStatus
			}
		}
	}
	evidence.SemanticCacheCatalogBound = len(evidence.Failures) == 0
	if evidence.SemanticCacheCatalogBound {
		evidence.Status = "pass"
	} else {
		evidence.Status = "fail"
	}

	encoded, err := encodeEvidence(evidence)
	if err != nil {
		return err
	}
	if err := writeAtomic(outputPath, encoded); err != nil {
		return fmt.Errorf("write semantic-cache isolation evidence: %w", err)
	}
	fmt.Printf("semantic-cache isolation evidence: %s (%d profile pairs, %d route probes, %d failures)\n",
		evidence.Status, evidence.ProfilePairCount, evidence.LiveRouteProbeCount, len(evidence.Failures))
	if !evidence.SemanticCacheCatalogBound {
		return fmt.Errorf("%w: generated evidence records %d failure(s)",
			errIsolationEvidenceFailed, len(evidence.Failures))
	}
	return nil
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func readWithDigest(path string) ([]byte, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	digest := sha256.Sum256(payload)
	return payload, hex.EncodeToString(digest[:]), nil
}

func decodeJSON(payload []byte, destination any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

type registryDocument struct {
	SchemaVersion   int               `json:"schema_version"`
	RegistryVersion string            `json:"registry_version"`
	ContractRelease string            `json:"contract_release"`
	Profiles        []registryProfile `json:"profiles"`
}

type registryProfile struct {
	ProfileID     string `json:"profile_id"`
	Alias         string `json:"alias"`
	CatalogSHA256 string `json:"catalog_sha256"`
	CatalogPath   string `json:"catalog_path"`
	Closure       struct {
		Products []string `json:"products"`
		SHA256   string   `json:"closure_sha256"`
	} `json:"closure"`
	Status struct {
		ClosureComplete       bool `json:"closure_complete"`
		CatalogMaterializable bool `json:"catalog_materializable"`
		LiveRouteAvailable    bool `json:"live_route_available"`
		ActivationSupported   bool `json:"activation_supported"`
		ActivationSmokePassed bool `json:"activation_smoke_passed"`
	} `json:"status"`
	TargetedRunEligible bool `json:"targeted_run_eligible"`
}

func validateRegistry(registry registryDocument) (map[string]registryProfile, error) {
	if registry.SchemaVersion != 1 || strings.TrimSpace(registry.RegistryVersion) == "" ||
		strings.TrimSpace(registry.ContractRelease) == "" {
		return nil, errors.New("profile registry identity is incomplete")
	}
	if len(registry.Profiles) == 0 {
		return nil, errors.New("profile registry is empty")
	}
	profiles := make(map[string]registryProfile, len(registry.Profiles))
	aliases := map[string]bool{}
	eligibleCount := 0
	for _, profile := range registry.Profiles {
		if strings.TrimSpace(profile.ProfileID) == "" || strings.TrimSpace(profile.Alias) == "" {
			return nil, errors.New("profile registry contains an unnamed profile")
		}
		if _, exists := profiles[profile.ProfileID]; exists {
			return nil, fmt.Errorf("profile registry lists %s twice", profile.ProfileID)
		}
		if aliases[profile.Alias] {
			return nil, fmt.Errorf("profile registry lists alias %s twice", profile.Alias)
		}
		eligible := profile.Status.ClosureComplete && profile.Status.CatalogMaterializable &&
			profile.Status.LiveRouteAvailable
		if eligible {
			eligibleCount++
		}
		if (profile.CatalogSHA256 != "" && !isSHA256(profile.CatalogSHA256)) ||
			(eligible && !isSHA256(profile.CatalogSHA256)) {
			return nil, fmt.Errorf("profile %s Catalog digest is not a lowercase SHA-256", profile.Alias)
		}
		if !isSortedUnique(profile.Closure.Products) || len(profile.Closure.Products) == 0 {
			return nil, fmt.Errorf("profile %s Product closure is empty, unsorted or duplicated", profile.Alias)
		}
		profiles[profile.ProfileID] = profile
		aliases[profile.Alias] = true
	}
	if eligibleCount == 0 {
		return nil, errors.New("profile registry has no closure-complete, catalog-materializable, live-route-available profiles")
	}
	return profiles, nil
}

func eligibleProfiles(profiles map[string]registryProfile) []registryProfile {
	eligible := make([]registryProfile, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Status.ClosureComplete && profile.Status.CatalogMaterializable &&
			profile.Status.LiveRouteAvailable {
			eligible = append(eligible, profile)
		}
	}
	sort.Slice(eligible, func(left, right int) bool {
		return eligible[left].ProfileID < eligible[right].ProfileID
	})
	return eligible
}

type intersectionDocument struct {
	SchemaVersion        int                `json:"schema_version"`
	Record               string             `json:"record"`
	ContractsVersion     string             `json:"contracts_version"`
	RegistryVersion      string             `json:"registry_version"`
	ProfileCount         int                `json:"profile_count"`
	PairCount            int                `json:"pair_count"`
	OverlappingPairCount int                `json:"overlapping_pair_count"`
	Pairs                []intersectionPair `json:"pairs"`
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

type intersectionAnalysis struct {
	PairCount                   int
	OverlappingPairCount        int
	SameQueryLiveTestApplicable bool
	Failures                    []string
}

func analyzeIntersection(document intersectionDocument, registry registryDocument,
	profiles map[string]registryProfile) (intersectionAnalysis, error) {
	if document.SchemaVersion != 1 || document.Record != intersectionRecord {
		return intersectionAnalysis{}, errors.New("product-intersection matrix identity is not recognised")
	}
	if document.ContractsVersion != registry.ContractRelease {
		return intersectionAnalysis{}, fmt.Errorf("product-intersection matrix pins contract %s, registry pins %s",
			document.ContractsVersion, registry.ContractRelease)
	}
	if document.RegistryVersion != registry.RegistryVersion {
		return intersectionAnalysis{}, fmt.Errorf("product-intersection matrix pins registry version %s, registry pins %s",
			document.RegistryVersion, registry.RegistryVersion)
	}

	eligible := eligibleProfiles(profiles)
	eligibleByID := make(map[string]registryProfile, len(eligible))
	expectedPairs := map[string]bool{}
	for _, profile := range eligible {
		eligibleByID[profile.ProfileID] = profile
	}
	for left := 0; left < len(eligible); left++ {
		for right := left + 1; right < len(eligible); right++ {
			expectedPairs[pairKey(eligible[left].ProfileID, eligible[right].ProfileID)] = true
		}
	}

	result := intersectionAnalysis{PairCount: len(document.Pairs), Failures: []string{}}
	if document.ProfileCount != len(eligible) {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"product-intersection profile_count=%d, derived %d", document.ProfileCount, len(eligible)))
	}
	if document.PairCount != len(document.Pairs) {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"product-intersection pair_count=%d, derived %d", document.PairCount, len(document.Pairs)))
	}
	if len(document.Pairs) != len(expectedPairs) {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"product-intersection contains %d pairs, complete eligible-profile matrix requires %d",
			len(document.Pairs), len(expectedPairs)))
	}

	seen := map[string]bool{}
	for index, pair := range document.Pairs {
		left, leftOK := eligibleByID[pair.LeftProfileID]
		right, rightOK := eligibleByID[pair.RightProfileID]
		if !leftOK || !rightOK || pair.LeftProfileID == pair.RightProfileID {
			return intersectionAnalysis{}, fmt.Errorf("product-intersection pair %d names an ineligible or repeated profile", index)
		}
		key := pairKey(pair.LeftProfileID, pair.RightProfileID)
		if seen[key] {
			result.Failures = append(result.Failures, fmt.Sprintf("product-intersection pair %s is duplicated", key))
		}
		seen[key] = true
		if !expectedPairs[key] {
			result.Failures = append(result.Failures, fmt.Sprintf("product-intersection pair %s is unexpected", key))
		}
		if pair.LeftAlias != left.Alias || pair.RightAlias != right.Alias {
			result.Failures = append(result.Failures, fmt.Sprintf("product-intersection pair %s aliases do not match registry", key))
		}
		if !reflect.DeepEqual(pair.LeftProducts, left.Closure.Products) ||
			!reflect.DeepEqual(pair.RightProducts, right.Closure.Products) {
			result.Failures = append(result.Failures, fmt.Sprintf("product-intersection pair %s Product closures do not match registry", key))
		}
		shared := intersectProducts(left.Closure.Products, right.Closure.Products)
		if !reflect.DeepEqual(pair.Intersection, shared) {
			result.Failures = append(result.Failures, fmt.Sprintf("product-intersection pair %s intersection is not derived", key))
		}
		if pair.IntersectionCount != len(shared) {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"product-intersection pair %s intersection_count=%d, derived %d",
				key, pair.IntersectionCount, len(shared)))
		}
		applicable := len(shared) != 0
		if pair.SameQueryLiveTestApplicable != applicable {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"product-intersection pair %s same_query_live_test_applicable=%t, derived %t",
				key, pair.SameQueryLiveTestApplicable, applicable))
		}
		if applicable {
			result.OverlappingPairCount++
			result.SameQueryLiveTestApplicable = true
		}
	}
	for _, key := range sortedBoolMapKeys(expectedPairs) {
		if !seen[key] {
			result.Failures = append(result.Failures, "product-intersection pair "+key+" is missing")
		}
	}
	if document.OverlappingPairCount != result.OverlappingPairCount {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"product-intersection overlapping_pair_count=%d, derived %d",
			document.OverlappingPairCount, result.OverlappingPairCount))
	}
	return result, nil
}

func pairKey(left, right string) string {
	if left > right {
		left, right = right, left
	}
	return left + "/" + right
}

func intersectProducts(left, right []string) []string {
	present := map[string]bool{}
	for _, product := range right {
		present[product] = true
	}
	intersection := []string{}
	for _, product := range left {
		if present[product] {
			intersection = append(intersection, product)
		}
	}
	sort.Strings(intersection)
	return intersection
}

type sameQueryLiveDocument struct {
	SchemaVersion                   int                 `json:"schema_version"`
	Record                          string              `json:"record"`
	ContractRelease                 string              `json:"contract_release"`
	ProfileRegistrySHA256           string              `json:"profile_registry_sha256"`
	ProfileRoutingIdentitySHA256    string              `json:"profile_routing_identity_sha256,omitempty"`
	ProductIntersectionMatrixSHA256 string              `json:"product_intersection_matrix_sha256"`
	DeploymentID                    string              `json:"deployment_id"`
	QueryTemplateSHA256             string              `json:"query_template_sha256"`
	PairCount                       int                 `json:"pair_count"`
	PassedPairCount                 int                 `json:"passed_pair_count"`
	FailedPairCount                 int                 `json:"failed_pair_count"`
	Pairs                           []sameQueryLivePair `json:"pairs"`
	Failures                        []string            `json:"failures"`
	Status                          string              `json:"status"`
}

type sameQueryLivePair struct {
	LeftProfileID                     string                    `json:"left_profile_id"`
	RightProfileID                    string                    `json:"right_profile_id"`
	LeftAlias                         string                    `json:"left_alias"`
	RightAlias                        string                    `json:"right_alias"`
	SharedProducts                    []string                  `json:"shared_products"`
	SelectedProduct                   string                    `json:"selected_product"`
	QuerySHA256                       string                    `json:"query_sha256"`
	LeftCatalogSHA256                 string                    `json:"left_catalog_sha256"`
	RightCatalogSHA256                string                    `json:"right_catalog_sha256"`
	FirstCacheKeySHA256               string                    `json:"first_cache_key_sha256"`
	SecondCacheKeySHA256              string                    `json:"second_cache_key_sha256"`
	FirstSQLFingerprintSHA256         string                    `json:"first_sql_fingerprint_sha256"`
	SecondSQLFingerprintSHA256        string                    `json:"second_sql_fingerprint_sha256"`
	SecondSourceQueryIsSelf           bool                      `json:"second_source_query_is_self"`
	SecondSemanticReplayAudits        int                       `json:"second_semantic_replay_audits"`
	SecondSettlementAudits            int                       `json:"second_settlement_audits"`
	SecondBusinessVisibleCallsDelta   int64                     `json:"second_business_visible_calls_delta"`
	SecondBusinessCompanionCallsDelta int64                     `json:"second_business_companion_calls_delta"`
	SecondSemanticReplay              bool                      `json:"second_semantic_replay"`
	SecondIdempotentReplay            bool                      `json:"second_idempotent_replay"`
	SecondNovelExecution              bool                      `json:"second_novel_execution"`
	LeftTaskFinalization              sameQueryTaskFinalization `json:"left_task_finalization,omitempty"`
	RightTaskFinalization             sameQueryTaskFinalization `json:"right_task_finalization,omitempty"`
	Status                            string                    `json:"status"`
}

type sameQueryTaskFinalization struct {
	BudgetProfile           string `json:"budget_profile"`
	PolicyMaxQueries        int64  `json:"policy_max_queries"`
	PolicySource            string `json:"policy_source"`
	ObservedTaskState       string `json:"observed_task_state"`
	ObservedTerminalReason  string `json:"observed_terminal_reason"`
	UsedQueries             int64  `json:"used_queries"`
	RemainingQueries        int64  `json:"remaining_queries"`
	SemanticVerdictCaptured bool   `json:"semantic_verdict_captured"`
	CompleteTaskCalled      bool   `json:"complete_task_called"`
	FinalTaskState          string `json:"final_task_state"`
	FinalTerminalReason     string `json:"final_terminal_reason"`
	Disposition             string `json:"disposition"`
	Status                  string `json:"status"`
}

type sameQueryLiveAnalysis struct {
	PairCount       int
	FailedPairCount int
	Failures        []string
}

func analyzeSameQueryLive(document sameQueryLiveDocument, registry registryDocument,
	profiles map[string]registryProfile, intersection intersectionDocument,
	registryDigest, routingIdentity, intersectionDigest string) (sameQueryLiveAnalysis, error) {
	legacy := document.SchemaVersion == 1 && document.Record == sameQueryLiveRecord
	policyBound := document.SchemaVersion == 2 && document.Record == sameQueryLiveRecordV2
	if !legacy && !policyBound {
		return sameQueryLiveAnalysis{}, errors.New("same-query live evidence identity is not recognised")
	}
	registryBound := legacy && document.ProfileRegistrySHA256 == registryDigest || policyBound &&
		isSHA256(document.ProfileRegistrySHA256) && document.ProfileRoutingIdentitySHA256 == routingIdentity
	if document.ContractRelease != registry.ContractRelease || !registryBound ||
		document.ProductIntersectionMatrixSHA256 != intersectionDigest {
		return sameQueryLiveAnalysis{}, errors.New("same-query live evidence does not bind the current contract, registry and product-intersection bytes")
	}
	if strings.TrimSpace(document.DeploymentID) == "" || !isSHA256(document.QueryTemplateSHA256) {
		return sameQueryLiveAnalysis{}, errors.New("same-query live evidence omits deployment or query-template identity")
	}
	expected := map[string]intersectionPair{}
	for _, pair := range intersection.Pairs {
		if pair.SameQueryLiveTestApplicable {
			expected[pair.LeftProfileID+"/"+pair.RightProfileID] = pair
		}
	}
	result := sameQueryLiveAnalysis{PairCount: len(document.Pairs), Failures: []string{}}
	seen := map[string]bool{}
	passed := 0
	for _, pair := range document.Pairs {
		key := pair.LeftProfileID + "/" + pair.RightProfileID
		want, found := expected[key]
		if !found || seen[key] {
			result.Failures = append(result.Failures, "same-query live pair "+key+" is unexpected or duplicated")
			continue
		}
		seen[key] = true
		left, leftOK := profiles[pair.LeftProfileID]
		right, rightOK := profiles[pair.RightProfileID]
		selected := false
		for _, product := range want.Intersection {
			selected = selected || product == pair.SelectedProduct
		}
		finalizationValid := legacy || (validSameQueryTaskFinalization(pair.LeftTaskFinalization) &&
			validSameQueryTaskFinalization(pair.RightTaskFinalization))
		novel := leftOK && rightOK && pair.LeftAlias == want.LeftAlias && pair.RightAlias == want.RightAlias &&
			reflect.DeepEqual(pair.SharedProducts, want.Intersection) && selected &&
			pair.LeftCatalogSHA256 == left.CatalogSHA256 && pair.RightCatalogSHA256 == right.CatalogSHA256 &&
			pair.LeftCatalogSHA256 != pair.RightCatalogSHA256 && isSHA256(pair.QuerySHA256) &&
			isSHA256(pair.FirstCacheKeySHA256) && isSHA256(pair.SecondCacheKeySHA256) &&
			pair.FirstCacheKeySHA256 != pair.SecondCacheKeySHA256 &&
			isSHA256(pair.FirstSQLFingerprintSHA256) &&
			pair.FirstSQLFingerprintSHA256 == pair.SecondSQLFingerprintSHA256 &&
			pair.SecondSourceQueryIsSelf && pair.SecondSemanticReplayAudits == 0 &&
			pair.SecondSettlementAudits == 1 && pair.SecondBusinessVisibleCallsDelta == 1 &&
			pair.SecondBusinessCompanionCallsDelta == 1 && !pair.SecondSemanticReplay &&
			!pair.SecondIdempotentReplay && finalizationValid
		if !novel || pair.SecondNovelExecution != novel || pair.Status != passedStatus {
			result.Failures = append(result.Failures, "same-query live pair "+key+" did not prove a catalog-bound novel second execution")
			continue
		}
		passed++
	}
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			result.Failures = append(result.Failures, "same-query live pair "+key+" is missing")
		}
	}
	result.FailedPairCount = len(expected) - passed
	if result.FailedPairCount < 0 {
		result.FailedPairCount = len(result.Failures)
	}
	if document.PairCount != len(document.Pairs) || document.PassedPairCount != passed ||
		document.FailedPairCount != result.FailedPairCount || document.Status != passedStatus ||
		len(document.Failures) != 0 {
		result.Failures = append(result.Failures, "same-query live evidence summary does not match the derived pair results")
	}
	return result, nil
}

func validSameQueryTaskFinalization(evidence sameQueryTaskFinalization) bool {
	if evidence.BudgetProfile == "" || evidence.PolicyMaxQueries < 1 ||
		!validPolicySource(evidence.PolicySource) || !evidence.SemanticVerdictCaptured ||
		evidence.UsedQueries != 1 || evidence.RemainingQueries != evidence.PolicyMaxQueries-1 ||
		evidence.FinalTaskState != "ARCHIVED" || evidence.Status != passedStatus {
		return false
	}
	switch evidence.Disposition {
	case "complete_active_task":
		return evidence.PolicyMaxQueries > 1 && evidence.ObservedTaskState == "ACTIVE" &&
			evidence.ObservedTerminalReason == "" && evidence.CompleteTaskCalled &&
			evidence.FinalTerminalReason == "completed"
	case "accept_automatic_budget_archive":
		return evidence.PolicyMaxQueries == 1 && evidence.ObservedTaskState == "ARCHIVED" &&
			evidence.ObservedTerminalReason == "budget_exhausted" && !evidence.CompleteTaskCalled &&
			evidence.FinalTerminalReason == "budget_exhausted"
	default:
		return false
	}
}

func validPolicySource(source string) bool {
	separator := strings.LastIndexByte(source, ':')
	if separator <= 0 || !strings.HasPrefix(source, "config/profiles/") ||
		!strings.HasSuffix(source[:separator], ".catalog.yaml") {
		return false
	}
	line, err := strconv.Atoi(source[separator+1:])
	return err == nil && line > 0
}

type routeMatrixDocument struct {
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

func (probe routeProbe) negativeAssertionsHold() bool {
	return probe.CatalogListAbsent && probe.LiveRequestRefused && probe.NoActiveTask &&
		probe.NoArtifact && probe.NoAvailable && probe.NoBusinessSQL && probe.NoObservation &&
		probe.NoReceipt && probe.NoRootLedgerChange && probe.NoSemanticCacheHit
}

type routeAnalysis struct {
	ExecutedProbeCount int
	FailedProbeCount   int
	Failures           []string
}

type observedRouteProbe struct {
	pass  bool
	count int
}

func analyzeRouteMatrix(document routeMatrixDocument, registry registryDocument,
	profiles map[string]registryProfile, registryDigest, intersectionDigest string) (routeAnalysis, error) {
	if document.SchemaVersion != 1 || document.Record != routeMatrixRecord {
		return routeAnalysis{}, errors.New("outside-product route matrix identity is not recognised")
	}
	if document.ContractRelease != registry.ContractRelease {
		return routeAnalysis{}, fmt.Errorf("outside-product route matrix pins contract %s, registry pins %s",
			document.ContractRelease, registry.ContractRelease)
	}
	if document.ProfileRegistrySHA256 != registryDigest {
		return routeAnalysis{}, fmt.Errorf("outside-product route matrix pins registry %s, recomputed %s",
			document.ProfileRegistrySHA256, registryDigest)
	}
	if document.ProductIntersectionMatrixSHA256 != intersectionDigest {
		return routeAnalysis{}, fmt.Errorf("outside-product route matrix pins product-intersection %s, recomputed %s",
			document.ProductIntersectionMatrixSHA256, intersectionDigest)
	}
	wantMatrixDigest, err := routeMatrixDigest(document)
	if err != nil {
		return routeAnalysis{}, err
	}
	if document.MatrixSHA256 != wantMatrixDigest {
		return routeAnalysis{}, fmt.Errorf("outside-product route matrix matrix_sha256=%s, recomputed %s",
			document.MatrixSHA256, wantMatrixDigest)
	}

	eligible := eligibleProfiles(profiles)
	eligibleByID := make(map[string]registryProfile, len(eligible))
	productUniverse := map[string]bool{}
	for _, profile := range eligible {
		eligibleByID[profile.ProfileID] = profile
		for _, product := range profile.Closure.Products {
			productUniverse[product] = true
		}
	}
	expected := map[string]bool{}
	for _, profile := range eligible {
		inside := stringSet(profile.Closure.Products)
		for product := range productUniverse {
			if !inside[product] {
				expected[probeKey(profile.ProfileID, product)] = true
			}
		}
	}
	if len(expected) == 0 {
		return routeAnalysis{}, errors.New("derived outside-product route probe set is empty")
	}

	result := routeAnalysis{ExecutedProbeCount: len(document.Probes), Failures: []string{}}
	observed := map[string]observedRouteProbe{}
	unexpected := 0
	profileSet := map[string]bool{}
	productSet := map[string]bool{}
	previousOrderKey := ""
	for index, probe := range document.Probes {
		profile, knownProfile := eligibleByID[probe.TargetProfileID]
		key := probeKey(probe.TargetProfileID, probe.RequestedProduct)
		valid := true
		if !knownProfile {
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %d names ineligible profile %s", index, probe.TargetProfileID))
			valid = false
		} else {
			profileSet[probe.TargetProfileID] = true
			if probe.TargetProfileAlias != profile.Alias || probe.TargetCatalogSHA256 != profile.CatalogSHA256 {
				result.Failures = append(result.Failures, fmt.Sprintf("route probe %s target identity does not match registry", key))
				valid = false
			}
			if stringSet(profile.Closure.Products)[probe.RequestedProduct] {
				result.Failures = append(result.Failures, fmt.Sprintf("route probe %s requests a Product inside its own closure", key))
				valid = false
			}
		}
		productSet[probe.RequestedProduct] = true
		orderKey := probe.TargetProfileAlias + "\x00" + probe.RequestedProduct
		if index > 0 && previousOrderKey >= orderKey {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"route probe %s is not in canonical profile-alias/Product order", key))
			valid = false
		}
		previousOrderKey = orderKey
		if !expected[key] {
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %s is not in the derived probe set", key))
			valid = false
			unexpected++
		}
		if probe.RequestedProductSHA256 != requestedProductDigest(probe.RequestedProduct) {
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %s requested Product digest is not derived", key))
			valid = false
		}
		if !isSHA256(probe.ResponseSHA256) {
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %s response digest is not a lowercase SHA-256", key))
			valid = false
		}
		if !stableRefusalClassification(probe.StableRefusalClassification) {
			result.Failures = append(result.Failures, fmt.Sprintf(
				"route probe %s has non-refusal or unstable classification %q",
				key, probe.StableRefusalClassification))
			valid = false
		}
		if !probe.negativeAssertionsHold() {
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %s does not establish all ten negative assertions", key))
			valid = false
		}
		entry := observed[key]
		entry.count++
		entry.pass = entry.pass || valid
		observed[key] = entry
	}

	passed := 0
	failed := unexpected
	for _, key := range sortedBoolMapKeys(expected) {
		entry := observed[key]
		if entry.count == 1 && entry.pass {
			passed++
			continue
		}
		failed++
		if entry.count == 0 {
			result.Failures = append(result.Failures, "route probe "+key+" is missing")
		} else if entry.count > 1 {
			failed += entry.count - 1
			result.Failures = append(result.Failures, fmt.Sprintf("route probe %s occurs %d times", key, entry.count))
		}
	}
	result.FailedProbeCount = failed

	if document.ExecutedProbeCount != len(document.Probes) {
		result.Failures = append(result.Failures, fmt.Sprintf("route executed_probe_count=%d, derived %d",
			document.ExecutedProbeCount, len(document.Probes)))
	}
	if document.ExpectedProbeCount != len(expected) {
		result.Failures = append(result.Failures, fmt.Sprintf("route expected_probe_count=%d, derived %d",
			document.ExpectedProbeCount, len(expected)))
	}
	if document.FailedProbeCount != failed {
		result.Failures = append(result.Failures, fmt.Sprintf("route failed_probe_count=%d, derived %d",
			document.FailedProbeCount, failed))
	}
	if document.PassedProbeCount != passed {
		result.Failures = append(result.Failures, fmt.Sprintf("route passed_probe_count=%d, derived %d",
			document.PassedProbeCount, passed))
	}
	if document.ProfileCount != len(profileSet) || len(profileSet) != len(eligible) {
		result.Failures = append(result.Failures, fmt.Sprintf("route profile_count=%d, observed %d, derived %d",
			document.ProfileCount, len(profileSet), len(eligible)))
	}
	if document.UniqueProductCount != len(productSet) || len(productSet) != len(productUniverse) {
		result.Failures = append(result.Failures, fmt.Sprintf("route unique_product_count=%d, observed %d, derived %d",
			document.UniqueProductCount, len(productSet), len(productUniverse)))
	}
	wantStatus := "pass"
	if len(result.Failures) != 0 {
		wantStatus = "fail"
	}
	if document.Status != wantStatus {
		result.Failures = append(result.Failures, fmt.Sprintf("route status=%q, derived %q", document.Status, wantStatus))
	}
	return result, nil
}

func probeKey(profileID, product string) string { return profileID + "/" + product }

func requestedProductDigest(product string) string {
	digest := sha256.Sum256([]byte(requestedProductDigestDomain + product))
	return hex.EncodeToString(digest[:])
}

// routeMatrixDigest is the v1 matrix identity: SHA-256 over the indented JSON
// bytes of the complete record with matrix_sha256 omitted and no trailing LF.
// The isolation evidence separately digests the complete route-matrix file.
func routeMatrixDigest(document routeMatrixDocument) (string, error) {
	document.MatrixSHA256 = ""
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode outside-product route matrix digest input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func stableRefusalClassification(value string) bool {
	if value == "tool_error" {
		return true
	}
	if strings.HasPrefix(value, "http_") {
		status, err := strconv.Atoi(strings.TrimPrefix(value, "http_"))
		return err == nil && status >= 300 && status <= 599
	}
	if strings.HasPrefix(value, "jsonrpc_error_") {
		code := strings.TrimPrefix(value, "jsonrpc_error_")
		_, err := strconv.Atoi(code)
		return err == nil && code != ""
	}
	return false
}

type productionLookupManifest struct {
	BackedBy                              string   `json:"backed_by"`
	ChangedCatalogMiss                    bool     `json:"changed_catalog_miss"`
	ChangedGrantMiss                      bool     `json:"changed_grant_miss"`
	ChangedPublicationOrDictionarySetMiss bool     `json:"changed_publication_or_dictionary_set_miss"`
	ChangedTaskMiss                       bool     `json:"changed_task_miss"`
	IncompleteBindingRejected             bool     `json:"incomplete_binding_rejected"`
	Record                                string   `json:"record"`
	SameBindingHit                        bool     `json:"same_binding_hit"`
	SameBindingHitAfterProbes             bool     `json:"same_binding_hit_after_probes"`
	TestPackage                           string   `json:"test_package"`
	Tests                                 []string `json:"tests"`
}

func analyzeProductionLookup(manifest productionLookupManifest) ([]string, error) {
	if manifest.Record != productionLookupRecord || manifest.TestPackage != "internal/control" ||
		manifest.BackedBy != "live PostgreSQL through the production publish and lookup path" {
		return nil, errors.New("production lookup manifest identity is not recognised")
	}
	expectedTests := []string{productionTestOne, productionTestTwo}
	if !reflect.DeepEqual(manifest.Tests, expectedTests) {
		return nil, fmt.Errorf("production lookup manifest tests are %v, expected %v", manifest.Tests, expectedTests)
	}
	failures := []string{}
	for _, check := range []struct {
		label  string
		passed bool
	}{
		{"production lookup changed_catalog_miss is false", manifest.ChangedCatalogMiss},
		{"production lookup changed_grant_miss is false", manifest.ChangedGrantMiss},
		{"production lookup changed_publication_or_dictionary_set_miss is false", manifest.ChangedPublicationOrDictionarySetMiss},
		{"production lookup changed_task_miss is false", manifest.ChangedTaskMiss},
		{"production lookup incomplete_binding_rejected is false", manifest.IncompleteBindingRejected},
		{"production lookup same_binding_hit is false", manifest.SameBindingHit},
		{"production lookup same_binding_hit_after_probes is false", manifest.SameBindingHitAfterProbes},
	} {
		if !check.passed {
			failures = append(failures, check.label)
		}
	}
	return failures, nil
}

// semanticCacheIsolationEvidence fields deliberately follow the exact order of
// the existing v1 record. json.MarshalIndent therefore reproduces its bytes.
type semanticCacheIsolationEvidence struct {
	ChangedCatalogMiss                    bool     `json:"changed_catalog_miss"`
	ChangedGrantMiss                      bool     `json:"changed_grant_miss"`
	ChangedPublicationOrDictionarySetMiss bool     `json:"changed_publication_or_dictionary_set_miss"`
	ChangedTaskMiss                       bool     `json:"changed_task_miss"`
	ContractRelease                       string   `json:"contract_release"`
	Failures                              []string `json:"failures"`
	IncompleteBindingRejected             bool     `json:"incomplete_binding_rejected"`
	LiveRouteProbeCount                   int      `json:"live_route_probe_count"`
	LiveRouteProbeFailures                int      `json:"live_route_probe_failures"`
	OutsideProductRouteMatrixSHA256       string   `json:"outside_product_route_matrix_sha256"`
	OverlappingPairCount                  int      `json:"overlapping_pair_count"`
	ProductIntersectionMatrixSHA256       string   `json:"product_intersection_matrix_sha256"`
	ProductionLookupManifestSHA256        string   `json:"production_lookup_manifest_sha256"`
	ProfilePairCount                      int      `json:"profile_pair_count"`
	ProfileRegistrySHA256                 string   `json:"profile_registry_sha256"`
	ProofMode                             string   `json:"proof_mode"`
	PublicationEligible                   bool     `json:"publication_eligible"`
	Record                                string   `json:"record"`
	SameBindingHit                        bool     `json:"same_binding_hit"`
	SameQueryLiveEvidenceSHA256           string   `json:"same_query_live_evidence_sha256,omitempty"`
	SameQueryLivePairCount                *int     `json:"same_query_live_pair_count,omitempty"`
	SameQueryLivePairFailures             *int     `json:"same_query_live_pair_failures,omitempty"`
	SameQueryLiveTestApplicable           bool     `json:"same_query_live_test_applicable"`
	SameQueryLiveTestStatus               string   `json:"same_query_live_test_status"`
	SchemaVersion                         int      `json:"schema_version"`
	SemanticCacheCatalogBound             bool     `json:"semantic_cache_catalog_bound"`
	Status                                string   `json:"status"`
}

func encodeEvidence(evidence semanticCacheIsolationEvidence) ([]byte, error) {
	if evidence.Failures == nil {
		evidence.Failures = []string{}
	}
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func writeAtomic(path string, payload []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".semantic-cache-isolation-*.tmp")
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
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func isSortedUnique(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || (index > 0 && values[index-1] >= value) {
			return false
		}
	}
	return true
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedBoolMapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

type productionCommandRunner func(context.Context, string, string, ...string) ([]byte, []byte, error)

func executeProductionCommand(ctx context.Context, directory, executable string,
	arguments ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = directory
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func runProductionTests(ctx context.Context, root string) error {
	return runProductionTestsWithRunner(ctx, root, executeProductionCommand)
}

func runProductionTestsWithRunner(ctx context.Context, root string, runner productionCommandRunner) error {
	script := filepath.Join(root, "scripts", "db-test-env.sh")
	verifyStdout, verifyStderr, verifyErr := runner(ctx, root, script, "verify")
	if verifyErr != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: db-test environment verification failed: %s",
			errProductionTestsSkipped, commandFailureReason(verifyStdout, verifyStderr, verifyErr))
	}

	stdout, stderr, commandErr := runner(ctx, root, script, "test", "-json",
		productionTestPackageArgument, "-run", productionTestPattern, "-count=1")
	parseErr := parseProductionTestJSON(bytes.NewReader(stdout))
	if parseErr != nil {
		if errors.Is(parseErr, errProductionTestsSkipped) {
			return parseErr
		}
		if commandErr != nil {
			return fmt.Errorf("production lookup test run failed (%s): %w",
				commandFailureReason(stdout, stderr, commandErr), parseErr)
		}
		return parseErr
	}
	if commandErr != nil {
		return fmt.Errorf("production lookup test command failed: %s",
			commandFailureReason(stdout, stderr, commandErr))
	}
	return nil
}

func commandFailureReason(stdout, stderr []byte, commandErr error) string {
	for _, payload := range [][]byte{stderr, stdout} {
		lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
		for index := len(lines) - 1; index >= 0; index-- {
			line := strings.Join(strings.Fields(lines[index]), " ")
			if line == "" {
				continue
			}
			const maximumReasonBytes = 240
			if len(line) > maximumReasonBytes {
				line = line[:maximumReasonBytes] + "..."
			}
			return line
		}
	}
	if commandErr != nil {
		return commandErr.Error()
	}
	return "command failed without diagnostic output"
}

func parseProductionTestJSON(input io.Reader) error {
	wanted := []string{productionTestOne, productionTestTwo}
	statuses := map[string]string{}
	outputs := map[string][]string{}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("production lookup test stream contains non-JSON output: %w", err)
		}
		if event.Package != productionTestPackage ||
			(event.Test != productionTestOne && event.Test != productionTestTwo) {
			continue
		}
		if event.Action == "output" {
			outputs[event.Test] = append(outputs[event.Test], strings.TrimSpace(event.Output))
		}
		switch event.Action {
		case "pass", "fail", "skip":
			statuses[event.Test] = event.Action
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read production lookup test stream: %w", err)
	}

	failed := []string{}
	missing := []string{}
	skipped := []string{}
	for _, test := range wanted {
		switch statuses[test] {
		case "pass":
		case "fail":
			failed = append(failed, test)
		case "skip":
			reason := lastNonEmpty(outputs[test])
			if reason == "" {
				reason = "no skip reason was reported"
			}
			skipped = append(skipped, test+": "+reason)
		default:
			missing = append(missing, test)
		}
	}
	if len(failed) != 0 {
		return fmt.Errorf("production lookup tests failed: %s", strings.Join(failed, ", "))
	}
	if len(missing) != 0 {
		return fmt.Errorf("production lookup test results are missing: %s", strings.Join(missing, ", "))
	}
	if len(skipped) != 0 {
		return fmt.Errorf("%w: %s", errProductionTestsSkipped, strings.Join(skipped, "; "))
	}
	return nil
}

func lastNonEmpty(values []string) string {
	for index := len(values) - 1; index >= 0; index-- {
		if value := strings.TrimSpace(values[index]); value != "" {
			return value
		}
	}
	return ""
}
