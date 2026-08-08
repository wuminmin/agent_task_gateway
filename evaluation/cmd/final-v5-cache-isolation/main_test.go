package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type commandFixture struct {
	root         string
	opts         options
	registry     registryDocument
	intersection intersectionDocument
	route        routeMatrixDocument
	production   productionLookupManifest
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	fixture := &commandFixture{root: t.TempDir()}
	fixture.opts = options{
		root:                 fixture.root,
		registryPath:         "registry.json",
		intersectionPath:     "intersection.json",
		routeMatrixPath:      "route.json",
		productionLookupPath: "production.json",
		outputPath:           "isolation.json",
	}
	fixture.registry = registryDocument{SchemaVersion: 1,
		RegistryVersion: "taskgate-final-v5-workload-closure-profile-v2",
		ContractRelease: "final-v5-contracts-test"}
	for index, product := range []string{"product_a", "product_b", "product_c"} {
		var profile registryProfile
		profile.ProfileID = "profile-" + string(rune('a'+index))
		profile.Alias = "alias-" + string(rune('a'+index))
		profile.CatalogSHA256 = strings.Repeat(string(rune('a'+index)), 64)
		profile.Closure.Products = []string{product}
		profile.Status.ClosureComplete = true
		profile.Status.CatalogMaterializable = true
		profile.Status.LiveRouteAvailable = true
		fixture.registry.Profiles = append(fixture.registry.Profiles, profile)
	}
	registryBytes := writeJSONFixture(t, fixture.root, fixture.opts.registryPath, fixture.registry)
	registryDigest := digestBytes(registryBytes)

	fixture.intersection = intersectionDocument{SchemaVersion: 1, Record: intersectionRecord,
		ContractsVersion: fixture.registry.ContractRelease,
		RegistryVersion:  fixture.registry.RegistryVersion, ProfileCount: len(fixture.registry.Profiles),
		Pairs: []intersectionPair{}}
	for left := 0; left < len(fixture.registry.Profiles); left++ {
		for right := left + 1; right < len(fixture.registry.Profiles); right++ {
			leftProfile := fixture.registry.Profiles[left]
			rightProfile := fixture.registry.Profiles[right]
			fixture.intersection.Pairs = append(fixture.intersection.Pairs, intersectionPair{
				LeftProfileID: leftProfile.ProfileID, RightProfileID: rightProfile.ProfileID,
				LeftAlias: leftProfile.Alias, RightAlias: rightProfile.Alias,
				LeftProducts:  append([]string(nil), leftProfile.Closure.Products...),
				RightProducts: append([]string(nil), rightProfile.Closure.Products...),
				Intersection:  []string{}, IntersectionCount: 0,
				SameQueryLiveTestApplicable: false,
			})
		}
	}
	fixture.intersection.PairCount = len(fixture.intersection.Pairs)
	intersectionBytes := writeJSONFixture(t, fixture.root, fixture.opts.intersectionPath, fixture.intersection)
	intersectionDigest := digestBytes(intersectionBytes)

	fixture.route = routeMatrixDocument{SchemaVersion: 1, Record: routeMatrixRecord,
		ContractRelease: fixture.registry.ContractRelease, MatrixSHA256: strings.Repeat("f", 64),
		ProductIntersectionMatrixSHA256: intersectionDigest,
		ProfileRegistrySHA256:           registryDigest, Status: "pass", Probes: []routeProbe{}}
	products := []string{"product_a", "product_b", "product_c"}
	for _, profile := range fixture.registry.Profiles {
		inside := stringSet(profile.Closure.Products)
		for _, product := range products {
			if inside[product] {
				continue
			}
			fixture.route.Probes = append(fixture.route.Probes, passingRouteProbe(profile, product))
		}
	}
	fixture.route.ExecutedProbeCount = len(fixture.route.Probes)
	fixture.route.ExpectedProbeCount = len(fixture.route.Probes)
	fixture.route.PassedProbeCount = len(fixture.route.Probes)
	fixture.route.ProfileCount = len(fixture.registry.Profiles)
	fixture.route.UniqueProductCount = len(products)
	refreshRouteMatrixDigest(t, &fixture.route)
	writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)

	fixture.production = productionLookupManifest{
		BackedBy:           "live PostgreSQL through the production publish and lookup path",
		ChangedCatalogMiss: true, ChangedGrantMiss: true,
		ChangedPublicationOrDictionarySetMiss: true, ChangedTaskMiss: true,
		IncompleteBindingRejected: true, Record: productionLookupRecord,
		SameBindingHit: true, SameBindingHitAfterProbes: true,
		TestPackage: "internal/control", Tests: []string{productionTestOne, productionTestTwo},
	}
	writeJSONFixture(t, fixture.root, fixture.opts.productionLookupPath, fixture.production)
	return fixture
}

func passingRouteProbe(profile registryProfile, product string) routeProbe {
	return routeProbe{
		CatalogListAbsent: true, LiveRequestRefused: true, NoActiveTask: true,
		NoArtifact: true, NoAvailable: true, NoBusinessSQL: true, NoObservation: true,
		NoReceipt: true, NoRootLedgerChange: true, NoSemanticCacheHit: true,
		RequestedProduct: product, RequestedProductSHA256: requestedProductDigest(product),
		ResponseSHA256: strings.Repeat("e", 64), StableRefusalClassification: "tool_error",
		TargetCatalogSHA256: profile.CatalogSHA256, TargetProfileAlias: profile.Alias,
		TargetProfileID: profile.ProfileID,
	}
}

func writeJSONFixture(t *testing.T, root, relative string, value any) []byte {
	t.Helper()
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
}

func digestBytes(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func refreshRouteMatrixDigest(t *testing.T, document *routeMatrixDocument) {
	t.Helper()
	digest, err := routeMatrixDigest(*document)
	if err != nil {
		t.Fatal(err)
	}
	document.MatrixSHA256 = digest
}

func readEvidence(t *testing.T, fixture *commandFixture) (semanticCacheIsolationEvidence, []byte) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(fixture.root, fixture.opts.outputPath))
	if err != nil {
		t.Fatal(err)
	}
	var evidence semanticCacheIsolationEvidence
	if err := decodeJSON(payload, &evidence, true); err != nil {
		t.Fatalf("strictly decode generated evidence: %v", err)
	}
	return evidence, payload
}

func requireIsolationFailure(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errIsolationEvidenceFailed) {
		t.Fatalf("run error = %v, want semantic-cache isolation failure sentinel", err)
	}
}

func TestGeneratedEvidenceHasExactSchemaAndRecomputedDigestChain(t *testing.T) {
	fixture := newCommandFixture(t)
	if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
		t.Fatal(err)
	}
	evidence, payload := readEvidence(t, fixture)

	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{
		"changed_catalog_miss", "changed_grant_miss",
		"changed_publication_or_dictionary_set_miss", "changed_task_miss", "contract_release",
		"failures", "incomplete_binding_rejected", "live_route_probe_count",
		"live_route_probe_failures", "outside_product_route_matrix_sha256",
		"overlapping_pair_count", "product_intersection_matrix_sha256",
		"production_lookup_manifest_sha256", "profile_pair_count", "profile_registry_sha256",
		"proof_mode", "publication_eligible", "record", "same_binding_hit",
		"same_query_live_test_applicable", "same_query_live_test_status", "schema_version",
		"semantic_cache_catalog_bound", "status",
	}
	gotKeys := make([]string, 0, len(document))
	for key := range document {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("generated keys = %v, want %v", gotKeys, wantKeys)
	}
	if payload[len(payload)-1] != '\n' || bytes.HasSuffix(payload, []byte("\n\n")) {
		t.Fatal("generated evidence does not end in exactly one LF")
	}
	for path, observed := range map[string]string{
		fixture.opts.registryPath:         evidence.ProfileRegistrySHA256,
		fixture.opts.intersectionPath:     evidence.ProductIntersectionMatrixSHA256,
		fixture.opts.routeMatrixPath:      evidence.OutsideProductRouteMatrixSHA256,
		fixture.opts.productionLookupPath: evidence.ProductionLookupManifestSHA256,
	} {
		raw, err := os.ReadFile(filepath.Join(fixture.root, path))
		if err != nil {
			t.Fatal(err)
		}
		if observed != digestBytes(raw) {
			t.Errorf("%s digest = %s, want %s", path, observed, digestBytes(raw))
		}
	}
	if evidence.Status != "pass" || !evidence.SemanticCacheCatalogBound || len(evidence.Failures) != 0 {
		t.Fatalf("clean fixture did not pass: %+v", evidence)
	}

	// Whitespace is semantically inert JSON but materially different evidence
	// bytes. The digest must change because this chain binds files, not values.
	productionPath := filepath.Join(fixture.root, fixture.opts.productionLookupPath)
	productionBytes, err := os.ReadFile(productionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(productionPath, append(productionBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
		t.Fatal(err)
	}
	updated, _ := readEvidence(t, fixture)
	if updated.ProductionLookupManifestSHA256 == evidence.ProductionLookupManifestSHA256 ||
		updated.ProductionLookupManifestSHA256 != digestBytes(append(productionBytes, '\n')) {
		t.Fatal("production manifest digest was copied or JSON-normalized instead of recomputed from raw bytes")
	}
}

func TestRunNormalizesRelativeRootBeforeResolvingPathsAndRunningLiveTests(t *testing.T) {
	fixture := newCommandFixture(t)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeRoot, err := filepath.Rel(workingDirectory, fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	if relativeRoot == "." || filepath.IsAbs(relativeRoot) {
		t.Fatalf("test did not derive a non-dot relative root: %q", relativeRoot)
	}
	fixture.opts.root = filepath.Join(relativeRoot, ".")
	fixture.opts.runProductionTests = true
	var runnerRoot string
	runner := func(_ context.Context, root string) error {
		runnerRoot = root
		return nil
	}
	if err := run(context.Background(), fixture.opts, runner); err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.Abs(relativeRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot = filepath.Clean(wantRoot)
	if runnerRoot != wantRoot {
		t.Fatalf("production test root = %q, want %q", runnerRoot, wantRoot)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, fixture.opts.outputPath)); err != nil {
		t.Fatalf("relative root did not resolve evidence paths against the repository: %v", err)
	}
}

func TestDerivedRegistryAndRouteProbeSetsCannotBeEmpty(t *testing.T) {
	t.Run("no eligible profiles", func(t *testing.T) {
		fixture := newCommandFixture(t)
		for index := range fixture.registry.Profiles {
			fixture.registry.Profiles[index].Status.LiveRouteAvailable = false
		}
		if _, err := validateRegistry(fixture.registry); err == nil ||
			!strings.Contains(err.Error(), "no closure-complete") {
			t.Fatalf("registry without eligible profiles was accepted: %v", err)
		}
	})

	t.Run("no outside Product probes", func(t *testing.T) {
		fixture := newCommandFixture(t)
		registry := fixture.registry
		registry.Profiles = append([]registryProfile(nil), registry.Profiles[:1]...)
		profiles, err := validateRegistry(registry)
		if err != nil {
			t.Fatal(err)
		}
		document := routeMatrixDocument{
			ContractRelease:                 registry.ContractRelease,
			ProductIntersectionMatrixSHA256: "intersection-digest",
			ProfileRegistrySHA256:           "registry-digest",
			Probes:                          []routeProbe{},
			Record:                          routeMatrixRecord,
			SchemaVersion:                   1,
			Status:                          "pass",
		}
		refreshRouteMatrixDigest(t, &document)
		_, err = analyzeRouteMatrix(document, registry, profiles,
			"registry-digest", "intersection-digest")
		if err == nil || !strings.Contains(err.Error(), "probe set is empty") {
			t.Fatalf("empty derived route proof was accepted: %v", err)
		}
	})
}

func TestMissingDerivedItemsHaveDeterministicOrder(t *testing.T) {
	fixture := newCommandFixture(t)
	profiles, err := validateRegistry(fixture.registry)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("intersection pairs", func(t *testing.T) {
		document := fixture.intersection
		document.Pairs = append([]intersectionPair(nil), document.Pairs[:1]...)
		document.PairCount = len(document.Pairs)
		want := []string{
			"product-intersection pair profile-a/profile-c is missing",
			"product-intersection pair profile-b/profile-c is missing",
		}
		for iteration := 0; iteration < 25; iteration++ {
			result, err := analyzeIntersection(document, fixture.registry, profiles)
			if err != nil {
				t.Fatal(err)
			}
			got := failuresContaining(result.Failures, " is missing")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("iteration %d missing-pair failures = %v, want %v", iteration, got, want)
			}
		}
	})

	t.Run("route probes", func(t *testing.T) {
		document := fixture.route
		document.Probes = append([]routeProbe(nil),
			fixture.route.Probes[1:3]...)
		document.Probes = append(document.Probes, fixture.route.Probes[4:]...)
		document.ExecutedProbeCount = len(document.Probes)
		document.FailedProbeCount = 2
		document.PassedProbeCount = len(document.Probes)
		document.Status = "fail"
		refreshRouteMatrixDigest(t, &document)
		want := []string{
			"route probe profile-a/product_b is missing",
			"route probe profile-b/product_c is missing",
		}
		for iteration := 0; iteration < 25; iteration++ {
			result, err := analyzeRouteMatrix(document, fixture.registry, profiles,
				fixture.route.ProfileRegistrySHA256, fixture.route.ProductIntersectionMatrixSHA256)
			if err != nil {
				t.Fatal(err)
			}
			got := failuresContaining(result.Failures, " is missing")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("iteration %d missing-probe failures = %v, want %v", iteration, got, want)
			}
		}
	})
}

func TestInputSummaryCannotTurnDerivedFailureIntoPass(t *testing.T) {
	fixture := newCommandFixture(t)
	fixture.route.FailedProbeCount = 99
	refreshRouteMatrixDigest(t, &fixture.route)
	writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)
	requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
	evidence, _ := readEvidence(t, fixture)
	if evidence.Status != "fail" || evidence.SemanticCacheCatalogBound {
		t.Fatalf("lying summary produced PASS evidence: %+v", evidence)
	}
	if evidence.LiveRouteProbeFailures != 0 {
		t.Fatalf("published failure count trusted the lying summary: %d", evidence.LiveRouteProbeFailures)
	}
	if !containsText(evidence.Failures, "failed_probe_count=99, derived 0") {
		t.Fatalf("summary mismatch was not recorded: %v", evidence.Failures)
	}
}

func TestEveryRouteNegativeAssertionGatesPass(t *testing.T) {
	mutations := map[string]func(*routeProbe){
		"catalog absent":        func(probe *routeProbe) { probe.CatalogListAbsent = false },
		"live refused":          func(probe *routeProbe) { probe.LiveRequestRefused = false },
		"no active task":        func(probe *routeProbe) { probe.NoActiveTask = false },
		"no artifact":           func(probe *routeProbe) { probe.NoArtifact = false },
		"no available":          func(probe *routeProbe) { probe.NoAvailable = false },
		"no business SQL":       func(probe *routeProbe) { probe.NoBusinessSQL = false },
		"no observation":        func(probe *routeProbe) { probe.NoObservation = false },
		"no receipt":            func(probe *routeProbe) { probe.NoReceipt = false },
		"no root ledger change": func(probe *routeProbe) { probe.NoRootLedgerChange = false },
		"no semantic cache hit": func(probe *routeProbe) { probe.NoSemanticCacheHit = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := newCommandFixture(t)
			mutate(&fixture.route.Probes[0])
			fixture.route.FailedProbeCount = 1
			fixture.route.PassedProbeCount--
			fixture.route.Status = "fail"
			refreshRouteMatrixDigest(t, &fixture.route)
			writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)
			requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
			evidence, _ := readEvidence(t, fixture)
			if evidence.Status != "fail" || evidence.SemanticCacheCatalogBound ||
				evidence.LiveRouteProbeFailures != 1 ||
				!containsText(evidence.Failures, "all ten negative assertions") {
				t.Fatalf("false route assertion did not gate PASS: %+v", evidence)
			}
		})
	}
}

func TestRouteMatrixDigestAndStableClassificationAreRecomputed(t *testing.T) {
	t.Run("inner matrix digest", func(t *testing.T) {
		fixture := newCommandFixture(t)
		fixture.route.MatrixSHA256 = strings.Repeat("0", 64)
		writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)
		err := run(context.Background(), fixture.opts, runProductionTests)
		if err == nil || !strings.Contains(err.Error(), "matrix_sha256") {
			t.Fatalf("copied inner matrix digest was accepted: %v", err)
		}
	})
	t.Run("request accepted is not refusal", func(t *testing.T) {
		fixture := newCommandFixture(t)
		fixture.route.Probes[0].StableRefusalClassification = "request_accepted"
		fixture.route.FailedProbeCount = 1
		fixture.route.PassedProbeCount--
		fixture.route.Status = "fail"
		refreshRouteMatrixDigest(t, &fixture.route)
		writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)
		requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
		evidence, _ := readEvidence(t, fixture)
		if evidence.Status != "fail" || !containsText(evidence.Failures, "unstable classification") {
			t.Fatalf("accepted classification produced PASS evidence: %+v", evidence)
		}
	})
}

func TestEachProductionLookupBooleanGatesPass(t *testing.T) {
	mutations := map[string]func(*productionLookupManifest){
		"changed catalog":           func(value *productionLookupManifest) { value.ChangedCatalogMiss = false },
		"changed grant":             func(value *productionLookupManifest) { value.ChangedGrantMiss = false },
		"changed dictionary":        func(value *productionLookupManifest) { value.ChangedPublicationOrDictionarySetMiss = false },
		"changed task":              func(value *productionLookupManifest) { value.ChangedTaskMiss = false },
		"incomplete binding":        func(value *productionLookupManifest) { value.IncompleteBindingRejected = false },
		"same binding":              func(value *productionLookupManifest) { value.SameBindingHit = false },
		"same binding after probes": func(value *productionLookupManifest) { value.SameBindingHitAfterProbes = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := newCommandFixture(t)
			mutate(&fixture.production)
			writeJSONFixture(t, fixture.root, fixture.opts.productionLookupPath, fixture.production)
			requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
			evidence, _ := readEvidence(t, fixture)
			if evidence.Status != "fail" || evidence.SemanticCacheCatalogBound || len(evidence.Failures) != 1 {
				t.Fatalf("false lookup result did not gate PASS: %+v", evidence)
			}
		})
	}
}

func TestStrictInputSchemasRejectUnknownFields(t *testing.T) {
	fixture := newCommandFixture(t)
	path := filepath.Join(fixture.root, fixture.opts.productionLookupPath)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	document["asserted_pass"] = true
	writeJSONFixture(t, fixture.root, fixture.opts.productionLookupPath, document)
	if err := run(context.Background(), fixture.opts, runProductionTests); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown input field was accepted: %v", err)
	}
}

func TestProductionTestJSONRequiresBothExactTestsToPass(t *testing.T) {
	event := func(action, test, output string) string {
		value, err := json.Marshal(goTestEvent{Action: action, Package: productionTestPackage,
			Test: test, Output: output})
		if err != nil {
			t.Fatal(err)
		}
		return string(value)
	}
	passing := strings.Join([]string{
		event("pass", productionTestOne, ""), event("pass", productionTestTwo, ""),
	}, "\n") + "\n"
	if err := parseProductionTestJSON(strings.NewReader(passing)); err != nil {
		t.Fatalf("two exact PASS actions rejected: %v", err)
	}

	tests := []struct {
		name     string
		stream   string
		wantSkip bool
		want     string
	}{
		{"skip", strings.Join([]string{
			event("output", productionTestOne, "CONTROL_TEST_POSTGRES_DSN is required\n"),
			event("skip", productionTestOne, ""), event("pass", productionTestTwo, ""),
		}, "\n") + "\n", true, "CONTROL_TEST_POSTGRES_DSN"},
		{"missing", event("pass", productionTestOne, "") + "\n", false, "results are missing"},
		{"fail", strings.Join([]string{
			event("fail", productionTestOne, ""), event("pass", productionTestTwo, ""),
		}, "\n") + "\n", false, "tests failed"},
		{"package-only pass", `{"Action":"pass","Package":"` + productionTestPackage + `"}` + "\n",
			false, "results are missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := parseProductionTestJSON(strings.NewReader(test.stream))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want text %q", err, test.want)
			}
			if errors.Is(err, errProductionTestsSkipped) != test.wantSkip {
				t.Fatalf("skip classification = %t, want %t (%v)",
					errors.Is(err, errProductionTestsSkipped), test.wantSkip, err)
			}
		})
	}
}

func TestRunProductionTestsVerifiesEnvironmentBeforeExactTests(t *testing.T) {
	event := func(action, test string) []byte {
		value, err := json.Marshal(goTestEvent{Action: action, Package: productionTestPackage, Test: test})
		if err != nil {
			t.Fatal(err)
		}
		return append(value, '\n')
	}
	passing := append(event("pass", productionTestOne), event("pass", productionTestTwo)...)

	t.Run("verify then pass", func(t *testing.T) {
		type call struct {
			directory  string
			executable string
			arguments  []string
		}
		calls := []call{}
		runner := func(_ context.Context, directory, executable string,
			arguments ...string) ([]byte, []byte, error) {
			calls = append(calls, call{directory: directory, executable: executable,
				arguments: append([]string(nil), arguments...)})
			if reflect.DeepEqual(arguments, []string{"verify"}) {
				return []byte("environment verified\n"), nil, nil
			}
			return passing, nil, nil
		}
		root := filepath.Join(string(filepath.Separator), "repository")
		if err := runProductionTestsWithRunner(context.Background(), root, runner); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 {
			t.Fatalf("command calls = %d, want verify plus test", len(calls))
		}
		if calls[0].directory != root || calls[0].executable != filepath.Join(root, "scripts", "db-test-env.sh") ||
			!reflect.DeepEqual(calls[0].arguments, []string{"verify"}) {
			t.Fatalf("verify call = %+v", calls[0])
		}
		wantTestArguments := []string{"test", "-json", productionTestPackageArgument,
			"-run", productionTestPattern, "-count=1"}
		if !reflect.DeepEqual(calls[1].arguments, wantTestArguments) {
			t.Fatalf("test arguments = %v, want %v", calls[1].arguments, wantTestArguments)
		}
	})

	t.Run("verify failure is skipped", func(t *testing.T) {
		calls := 0
		runner := func(context.Context, string, string, ...string) ([]byte, []byte, error) {
			calls++
			return []byte("partial verification output\n"), []byte("db-test containers are not running\n"),
				errors.New("exit status 1")
		}
		err := runProductionTestsWithRunner(context.Background(), "/repository", runner)
		if !errors.Is(err, errProductionTestsSkipped) ||
			!strings.Contains(err.Error(), "db-test containers are not running") {
			t.Fatalf("verify error = %v, want skipped environment with concise reason", err)
		}
		if calls != 1 {
			t.Fatalf("test command ran after failed verify: %d calls", calls)
		}
	})

	for _, test := range []struct {
		name       string
		testOutput []byte
		testErr    error
		want       string
	}{
		{name: "missing result", testOutput: event("pass", productionTestOne), want: "results are missing"},
		{name: "test failure", testOutput: append(event("fail", productionTestOne),
			event("pass", productionTestTwo)...), testErr: errors.New("exit status 1"), want: "tests failed"},
	} {
		t.Run(test.name+" remains failure", func(t *testing.T) {
			calls := 0
			runner := func(_ context.Context, _, _ string, arguments ...string) ([]byte, []byte, error) {
				calls++
				if reflect.DeepEqual(arguments, []string{"verify"}) {
					return nil, nil, nil
				}
				return test.testOutput, []byte("test diagnostic\n"), test.testErr
			}
			err := runProductionTestsWithRunner(context.Background(), "/repository", runner)
			if err == nil || errors.Is(err, errProductionTestsSkipped) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("test error = %v, want ordinary failure containing %q", err, test.want)
			}
			if calls != 2 {
				t.Fatalf("command calls = %d, want verify plus test", calls)
			}
		})
	}
}

func TestFailedProductionVerificationDoesNotOverwriteOutput(t *testing.T) {
	fixture := newCommandFixture(t)
	fixture.opts.runProductionTests = true
	output := filepath.Join(fixture.root, fixture.opts.outputPath)
	before := []byte("do-not-overwrite\n")
	if err := os.WriteFile(output, before, 0o600); err != nil {
		t.Fatal(err)
	}
	commandRunner := func(context.Context, string, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("Docker environment is absent\n"), errors.New("exit status 1")
	}
	runner := func(ctx context.Context, root string) error {
		return runProductionTestsWithRunner(ctx, root, commandRunner)
	}
	err := run(context.Background(), fixture.opts, runner)
	if !errors.Is(err, errProductionTestsSkipped) {
		t.Fatalf("run error = %v, want skipped production tests", err)
	}
	after, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("failed verification overwrote output: %q", after)
	}
}

func TestUnsuccessfulProductionTestsDoNotOverwriteOutput(t *testing.T) {
	tests := []struct {
		name      string
		runnerErr error
		wantSkip  bool
	}{
		{"skipped", errors.Join(errProductionTestsSkipped, errors.New("database environment absent")), true},
		{"missing result", errors.New("production lookup test results are missing"), false},
		{"failed", errors.New("production lookup tests failed"), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCommandFixture(t)
			fixture.opts.runProductionTests = true
			output := filepath.Join(fixture.root, fixture.opts.outputPath)
			before := []byte("do-not-overwrite\n")
			if err := os.WriteFile(output, before, 0o600); err != nil {
				t.Fatal(err)
			}
			runner := func(context.Context, string) error { return test.runnerErr }
			err := run(context.Background(), fixture.opts, runner)
			if err == nil || errors.Is(err, errProductionTestsSkipped) != test.wantSkip {
				t.Fatalf("run error = %v, want skip=%t", err, test.wantSkip)
			}
			after, readErr := os.ReadFile(output)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("unsuccessful live tests overwrote output: %q", after)
			}
		})
	}
}

func TestV13GoldenReproducesCommittedEvidenceByteForByte(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"registry", "intersection", "route", "production", "isolation"} {
		payload := readCompressedGolden(t, name)
		if err := os.WriteFile(filepath.Join(root, name+".json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := options{root: root, registryPath: "registry.json", intersectionPath: "intersection.json",
		routeMatrixPath: "route.json", productionLookupPath: "production.json", outputPath: "actual.json"}
	if err := run(context.Background(), opts, runProductionTests); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "actual.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(root, "isolation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("v1.3 regeneration is not byte-identical\nactual digest:   %s\nexpected digest: %s",
			digestBytes(actual), digestBytes(expected))
	}
	if digestBytes(actual) != "6d6f281a43512ebe07bd53329c0e275e8c48d6a82587c731b7bde05a574e1712" {
		t.Fatalf("v1.3 evidence digest = %s", digestBytes(actual))
	}
}

func readCompressedGolden(t *testing.T, name string) []byte {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join("testdata", "v1.3", name+".json.gz.base64"))
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return payload
}

func containsText(values []string, text string) bool {
	for _, value := range values {
		if strings.Contains(value, text) {
			return true
		}
	}
	return false
}

func failuresContaining(values []string, text string) []string {
	matched := []string{}
	for _, value := range values {
		if strings.Contains(value, text) {
			matched = append(matched, value)
		}
	}
	return matched
}
