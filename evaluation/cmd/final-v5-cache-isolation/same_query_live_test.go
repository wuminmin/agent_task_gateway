package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func installSingleOverlap(t *testing.T, fixture *commandFixture) sameQueryLiveDocument {
	t.Helper()
	fixture.registry.Profiles[1].Closure.Products = []string{"product_a"}
	registryBytes := writeJSONFixture(t, fixture.root, fixture.opts.registryPath, fixture.registry)
	registryDigest := digestBytes(registryBytes)

	fixture.intersection.Pairs = nil
	fixture.intersection.OverlappingPairCount = 0
	for left := 0; left < len(fixture.registry.Profiles); left++ {
		for right := left + 1; right < len(fixture.registry.Profiles); right++ {
			leftProfile, rightProfile := fixture.registry.Profiles[left], fixture.registry.Profiles[right]
			shared := intersectProducts(leftProfile.Closure.Products, rightProfile.Closure.Products)
			fixture.intersection.Pairs = append(fixture.intersection.Pairs, intersectionPair{
				LeftProfileID: leftProfile.ProfileID, RightProfileID: rightProfile.ProfileID,
				LeftAlias: leftProfile.Alias, RightAlias: rightProfile.Alias,
				LeftProducts:  append([]string(nil), leftProfile.Closure.Products...),
				RightProducts: append([]string(nil), rightProfile.Closure.Products...),
				Intersection:  shared, IntersectionCount: len(shared),
				SameQueryLiveTestApplicable: len(shared) > 0,
			})
			if len(shared) > 0 {
				fixture.intersection.OverlappingPairCount++
			}
		}
	}
	fixture.intersection.PairCount = len(fixture.intersection.Pairs)
	intersectionBytes := writeJSONFixture(t, fixture.root, fixture.opts.intersectionPath, fixture.intersection)
	intersectionDigest := digestBytes(intersectionBytes)

	fixture.route.ProfileRegistrySHA256 = registryDigest
	fixture.route.ProductIntersectionMatrixSHA256 = intersectionDigest
	fixture.route.Probes = nil
	products := []string{"product_a", "product_c"}
	for _, profile := range fixture.registry.Profiles {
		inside := stringSet(profile.Closure.Products)
		for _, product := range products {
			if !inside[product] {
				fixture.route.Probes = append(fixture.route.Probes, passingRouteProbe(profile, product))
			}
		}
	}
	fixture.route.ExecutedProbeCount = len(fixture.route.Probes)
	fixture.route.ExpectedProbeCount = len(fixture.route.Probes)
	fixture.route.PassedProbeCount = len(fixture.route.Probes)
	fixture.route.FailedProbeCount = 0
	fixture.route.ProfileCount = len(fixture.registry.Profiles)
	fixture.route.UniqueProductCount = len(products)
	fixture.route.Status = passedStatus
	refreshRouteMatrixDigest(t, &fixture.route)
	writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)

	left, right := fixture.registry.Profiles[0], fixture.registry.Profiles[1]
	digestA, digestB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	return sameQueryLiveDocument{
		SchemaVersion: 1, Record: sameQueryLiveRecord,
		ContractRelease:       fixture.registry.ContractRelease,
		ProfileRegistrySHA256: registryDigest, ProductIntersectionMatrixSHA256: intersectionDigest,
		DeploymentID: "deployment-test", QueryTemplateSHA256: strings.Repeat("3", 64),
		PairCount: 1, PassedPairCount: 1, FailedPairCount: 0, Status: passedStatus,
		Failures: []string{},
		Pairs: []sameQueryLivePair{{
			LeftProfileID: left.ProfileID, RightProfileID: right.ProfileID,
			LeftAlias: left.Alias, RightAlias: right.Alias,
			SharedProducts: []string{"product_a"}, SelectedProduct: "product_a",
			QuerySHA256: strings.Repeat("4", 64), LeftCatalogSHA256: left.CatalogSHA256,
			RightCatalogSHA256: right.CatalogSHA256, FirstCacheKeySHA256: digestA,
			SecondCacheKeySHA256: digestB, FirstSQLFingerprintSHA256: strings.Repeat("5", 64),
			SecondSQLFingerprintSHA256: strings.Repeat("5", 64), SecondSourceQueryIsSelf: true,
			SecondSemanticReplayAudits: 0, SecondSettlementAudits: 1,
			SecondBusinessVisibleCallsDelta: 1, SecondBusinessCompanionCallsDelta: 1,
			SecondSemanticReplay: false, SecondIdempotentReplay: false,
			SecondNovelExecution: true, Status: passedStatus,
		}},
	}
}

func TestSameQueryLiveEvidenceThreeStates(t *testing.T) {
	t.Run("missing evidence fails with legacy status and schema", func(t *testing.T) {
		fixture := newCommandFixture(t)
		installSingleOverlap(t, fixture)
		requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
		evidence, _ := readEvidence(t, fixture)
		if evidence.SchemaVersion != 1 || evidence.Record != isolationRecord ||
			evidence.SameQueryLiveTestStatus != requiredStatus || evidence.SameQueryLiveEvidenceSHA256 != "" ||
			evidence.SameQueryLivePairCount != nil || evidence.SameQueryLivePairFailures != nil ||
			!reflect.DeepEqual(evidence.Failures, []string{
				"same-query cross-profile live test is applicable but no such live evidence was provided"}) {
			t.Fatalf("missing live evidence changed the legacy failure bytes: %+v", evidence)
		}
	})

	t.Run("claimed pass with a cross-profile hit fails", func(t *testing.T) {
		fixture := newCommandFixture(t)
		live := installSingleOverlap(t, fixture)
		live.Pairs[0].SecondSemanticReplay = true
		fixture.opts.sameQueryLivePath = "live.json"
		writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
		requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
		evidence, _ := readEvidence(t, fixture)
		if evidence.Status != "fail" || evidence.SameQueryLiveTestStatus != requiredStatus ||
			!containsText(evidence.Failures, "did not prove a catalog-bound novel second execution") {
			t.Fatalf("fake cross-profile hit was accepted: %+v", evidence)
		}
	})

	t.Run("all overlap pairs proving novel execution pass", func(t *testing.T) {
		fixture := newCommandFixture(t)
		live := installSingleOverlap(t, fixture)
		fixture.opts.sameQueryLivePath = "live.json"
		liveBytes := writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
		if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
			t.Fatal(err)
		}
		evidence, _ := readEvidence(t, fixture)
		if evidence.Status != passedStatus || evidence.SchemaVersion != 2 || evidence.Record != isolationRecordV2 ||
			evidence.SameQueryLiveTestStatus != passedStatus ||
			evidence.SameQueryLiveEvidenceSHA256 != digestBytes(liveBytes) ||
			evidence.SameQueryLivePairCount == nil || *evidence.SameQueryLivePairCount != 1 ||
			evidence.SameQueryLivePairFailures == nil || *evidence.SameQueryLivePairFailures != 0 {
			t.Fatalf("complete same-query live evidence did not pass: %+v", evidence)
		}
	})
}
