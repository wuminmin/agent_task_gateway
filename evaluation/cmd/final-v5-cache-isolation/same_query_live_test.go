package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func installSingleOverlap(t *testing.T, fixture *commandFixture) sameQueryLiveDocument {
	t.Helper()
	// The single overlap models the frozen test's real domain: both profiles
	// share provsql_orders and neither publishes expense_detail.
	fixture.registry.Profiles[0].Closure.Products = []string{"provsql_orders"}
	fixture.registry.Profiles[1].Closure.Products = []string{"provsql_orders"}
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
				SameQueryLiveTestApplicable: finalv5profile.SameQueryLiveTestApplicable(
					leftProfile.Closure.Products, rightProfile.Closure.Products, shared),
			})
			if finalv5profile.SameQueryLiveTestApplicable(
				leftProfile.Closure.Products, rightProfile.Closure.Products, shared) {
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
	products := []string{"provsql_orders", "product_c"}
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
			SharedProducts: []string{"provsql_orders"}, SelectedProduct: "provsql_orders",
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

func TestSameQueryLiveV2RequiresPolicyBoundTaskFinalization(t *testing.T) {
	passingFinalization := func(maxQueries int64) sameQueryTaskFinalization {
		return sameQueryTaskFinalization{
			BudgetProfile: "source-controlled-budget", PolicyMaxQueries: maxQueries,
			PolicySource: "config/profiles/test.catalog.yaml:42", ObservedTaskState: "ACTIVE",
			UsedQueries: 1, RemainingQueries: maxQueries - 1, SemanticVerdictCaptured: true,
			CompleteTaskCalled: true, FinalTaskState: "ARCHIVED", FinalTerminalReason: "completed",
			Disposition: "complete_active_task", Status: passedStatus,
		}
	}

	t.Run("complete source-controlled finalization passes", func(t *testing.T) {
		fixture := newCommandFixture(t)
		live := installSingleOverlap(t, fixture)
		live.SchemaVersion, live.Record = 2, sameQueryLiveRecordV2
		live.ProfileRoutingIdentitySHA256 = fixtureRoutingIdentity(t, fixture)
		live.Pairs[0].LeftTaskFinalization = passingFinalization(128)
		live.Pairs[0].RightTaskFinalization = passingFinalization(128)
		fixture.opts.sameQueryLivePath = "live-v2.json"
		writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
		if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
			t.Fatal(err)
		}
		evidence, _ := readEvidence(t, fixture)
		if evidence.Status != passedStatus || evidence.SameQueryLiveTestStatus != passedStatus {
			t.Fatalf("policy-bound v2 live evidence did not pass: %+v", evidence)
		}
	})

	for name, mutate := range map[string]func(*sameQueryTaskFinalization){
		"missing Catalog source": func(value *sameQueryTaskFinalization) { value.PolicySource = "" },
		"verdict not captured":   func(value *sameQueryTaskFinalization) { value.SemanticVerdictCaptured = false },
		"TASK_NOT_ACTIVE swallowed": func(value *sameQueryTaskFinalization) {
			value.ObservedTaskState = "ARCHIVED"
			value.ObservedTerminalReason = "completed"
			value.CompleteTaskCalled = false
		},
		"query count is not one": func(value *sameQueryTaskFinalization) { value.UsedQueries = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCommandFixture(t)
			live := installSingleOverlap(t, fixture)
			live.SchemaVersion, live.Record = 2, sameQueryLiveRecordV2
			live.ProfileRoutingIdentitySHA256 = fixtureRoutingIdentity(t, fixture)
			live.Pairs[0].LeftTaskFinalization = passingFinalization(128)
			live.Pairs[0].RightTaskFinalization = passingFinalization(128)
			mutate(&live.Pairs[0].RightTaskFinalization)
			fixture.opts.sameQueryLivePath = "live-v2-invalid.json"
			writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
			requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
			evidence, _ := readEvidence(t, fixture)
			if evidence.SameQueryLiveTestStatus == passedStatus ||
				!containsText(evidence.Failures, "did not prove a catalog-bound novel second execution") {
				t.Fatalf("invalid policy-bound task finalization passed: %+v", evidence)
			}
		})
	}

	t.Run("one-query automatic archive passes without complete_task", func(t *testing.T) {
		value := passingFinalization(1)
		value.ObservedTaskState, value.ObservedTerminalReason = "ARCHIVED", "budget_exhausted"
		value.CompleteTaskCalled = false
		value.FinalTerminalReason = "budget_exhausted"
		value.Disposition = "accept_automatic_budget_archive"
		if !validSameQueryTaskFinalization(value) {
			t.Fatal("source-controlled max_queries=1 automatic archive was rejected")
		}
	})
}

func TestSameQueryLiveV3BindsLeftDrainToTheVerifiedProfileSwitch(t *testing.T) {
	passing := func(beforeSwitch bool) sameQueryTaskFinalization {
		return sameQueryTaskFinalization{BudgetProfile: "source-controlled-budget", PolicyMaxQueries: 128,
			PolicySource: "config/profiles/test.catalog.yaml:42", ObservedTaskState: "ACTIVE",
			UsedQueries: 1, RemainingQueries: 127, QueryEvidenceCaptured: true,
			SemanticVerdictCaptured: !beforeSwitch, RequiredBeforeSwitch: beforeSwitch,
			CompleteTaskCalled: true, FinalTaskState: "ARCHIVED", FinalTerminalReason: "completed",
			ActivationDrainReady: true, Disposition: "complete_active_task", Status: passedStatus}
	}
	newLive := func(t *testing.T) (*commandFixture, sameQueryLiveDocument) {
		t.Helper()
		fixture := newCommandFixture(t)
		live := installSingleOverlap(t, fixture)
		live.SchemaVersion, live.Record = 3, sameQueryLiveRecordV3
		live.ProfileRoutingIdentitySHA256 = fixtureRoutingIdentity(t, fixture)
		live.Pairs[0].LeftTaskFinalization = passing(true)
		live.Pairs[0].RightTaskFinalization = passing(false)
		fixture.opts.sameQueryLivePath = "live-v3.json"
		return fixture, live
	}

	t.Run("left drained before switch and right closed after verdict pass", func(t *testing.T) {
		fixture, live := newLive(t)
		writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
		if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
			t.Fatal(err)
		}
	})

	for name, mutate := range map[string]func(*sameQueryLivePair){
		"left query evidence absent": func(pair *sameQueryLivePair) {
			pair.LeftTaskFinalization.QueryEvidenceCaptured = false
		},
		"left not required before switch": func(pair *sameQueryLivePair) {
			pair.LeftTaskFinalization.RequiredBeforeSwitch = false
		},
		"left falsely claims later verdict": func(pair *sameQueryLivePair) {
			pair.LeftTaskFinalization.SemanticVerdictCaptured = true
		},
		"left activation drain not ready": func(pair *sameQueryLivePair) {
			pair.LeftTaskFinalization.ActivationDrainReady = false
		},
		"left remains active": func(pair *sameQueryLivePair) {
			pair.LeftTaskFinalization.FinalTaskState = "ACTIVE"
		},
		"right verdict absent": func(pair *sameQueryLivePair) {
			pair.RightTaskFinalization.SemanticVerdictCaptured = false
		},
		"right mislabeled pre-switch": func(pair *sameQueryLivePair) {
			pair.RightTaskFinalization.RequiredBeforeSwitch = true
		},
		"right activation drain not ready": func(pair *sameQueryLivePair) {
			pair.RightTaskFinalization.ActivationDrainReady = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, live := newLive(t)
			mutate(&live.Pairs[0])
			writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
			requireIsolationFailure(t, run(context.Background(), fixture.opts, runProductionTests))
			evidence, _ := readEvidence(t, fixture)
			if evidence.SameQueryLiveTestStatus == passedStatus ||
				!containsText(evidence.Failures, "did not prove a catalog-bound novel second execution") {
				t.Fatalf("invalid v3 switch-bound finalization passed: %+v", evidence)
			}
		})
	}
}

func TestSameQueryLiveV2SurvivesRegistryReadinessFixedPoint(t *testing.T) {
	fixture := newCommandFixture(t)
	live := installSingleOverlap(t, fixture)
	live.SchemaVersion, live.Record = 2, sameQueryLiveRecordV2
	live.ProfileRoutingIdentitySHA256 = fixtureRoutingIdentity(t, fixture)
	live.Pairs[0].LeftTaskFinalization = passingActiveFinalization(128)
	live.Pairs[0].RightTaskFinalization = passingActiveFinalization(128)
	originalRegistryDigest := live.ProfileRegistrySHA256

	fixture.registry.Profiles[0].Status.ActivationSupported = true
	fixture.registry.Profiles[0].Status.ActivationSmokePassed = true
	fixture.registry.Profiles[0].TargetedRunEligible = true
	registryBytes := writeJSONFixture(t, fixture.root, fixture.opts.registryPath, fixture.registry)
	currentRegistryDigest := digestBytes(registryBytes)
	if currentRegistryDigest == originalRegistryDigest {
		t.Fatal("test readiness mutation did not move the full registry digest")
	}
	if currentRouting := fixtureRoutingIdentity(t, fixture); currentRouting != live.ProfileRoutingIdentitySHA256 {
		t.Fatal("readiness-only registry fixed point moved the routing identity")
	}
	fixture.route.ProfileRegistrySHA256 = currentRegistryDigest
	refreshRouteMatrixDigest(t, &fixture.route)
	writeJSONFixture(t, fixture.root, fixture.opts.routeMatrixPath, fixture.route)
	fixture.opts.sameQueryLivePath = "live-v2-pre-fixed-point.json"
	writeJSONFixture(t, fixture.root, fixture.opts.sameQueryLivePath, live)
	if err := run(context.Background(), fixture.opts, runProductionTests); err != nil {
		t.Fatal(err)
	}
	evidence, _ := readEvidence(t, fixture)
	if evidence.ProfileRegistrySHA256 != currentRegistryDigest || evidence.SameQueryLiveTestStatus != passedStatus {
		t.Fatalf("fixed-point composition did not bind the current registry: %+v", evidence)
	}
}

func passingActiveFinalization(maxQueries int64) sameQueryTaskFinalization {
	return sameQueryTaskFinalization{
		BudgetProfile: "source-controlled-budget", PolicyMaxQueries: maxQueries,
		PolicySource: "config/profiles/test.catalog.yaml:42", ObservedTaskState: "ACTIVE",
		UsedQueries: 1, RemainingQueries: maxQueries - 1, SemanticVerdictCaptured: true,
		CompleteTaskCalled: true, FinalTaskState: "ARCHIVED", FinalTerminalReason: "completed",
		Disposition: "complete_active_task", Status: passedStatus,
	}
}

func fixtureRoutingIdentity(t *testing.T, fixture *commandFixture) string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(fixture.root, fixture.opts.registryPath))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := finalv5profile.ProfileRoutingIdentitySHA256(payload)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
