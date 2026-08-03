package finalv5profile

import (
	"strings"
	"testing"
)

func activationProfile(t *testing.T) Profile {
	t.Helper()
	registry := loadRegistry(t)
	return profileByAlias(t, registry, "result-heavy")
}

func passingEvidence(t *testing.T, profile Profile) ActivationEvidence {
	t.Helper()
	expected := ExpectedArtifacts(profile)
	return ActivationEvidence{SchemaVersion: 1, Record: ActivationEvidenceVersion,
		ContractRelease: "final-v5-contracts-v1.2", CampaignClass: "pilot", PublicationEligible: false,
		DeploymentID: "deployment-01", ActivationSequence: 2, PreviousProfileID: "profile-0d88c4e9d8b7561b",
		ProfileID: profile.ID, ProfileAlias: profile.Alias, ClosureSHA256: profile.Closure.SHA256,
		CatalogSHA256: profile.CatalogSHA256, CatalogFileSHA256: strings.Repeat("a", 64),
		ExpectedProducts: profile.Closure.Products, ObservedProducts: profile.Closure.Products,
		ExpectedPublications: profile.Closure.Publications, ObservedPublications: profile.Closure.Publications,
		ExpectedHotArtifacts: expected, ObservedHotArtifacts: expected,
		ExpectedHotBytes: profile.TotalHotBytes, ActualHotBytes: profile.TotalHotBytes,
		HotLimitBytes: MaxHotBytesPerInstance,
		CacheIsolation: CacheIsolation{ProcessRestarted: true, PreviousProcessNonce: strings.Repeat("1", 64),
			ProcessNonce: strings.Repeat("2", 64), PreviousCacheNamespace: strings.Repeat("3", 64),
			CacheNamespace: strings.Repeat("4", 64), PreviousCacheUnreachable: true,
			SemanticCacheCatalogBound: true, PreviousHotArtifactsRetired: true},
		OutsideProduct:        []OutsideProductProbe{{Product: "provsql_orders", Refused: true}},
		ActivationSmokePassed: true, Status: "pass"}
}

func TestActivationEvidenceAcceptsAnExactActivation(t *testing.T) {
	profile := activationProfile(t)
	if err := ValidateActivationEvidence(passingEvidence(t, profile), profile); err != nil {
		t.Fatalf("an exact activation was rejected: %v", err)
	}
}

// Every way an activation can be wrong must fail closed.
func TestActivationEvidenceFailsClosed(t *testing.T) {
	profile := activationProfile(t)
	for name, mutate := range map[string]func(*ActivationEvidence){
		"publication eligible": func(evidence *ActivationEvidence) {
			evidence.PublicationEligible = true
		},
		"claims workload targeted validation": func(evidence *ActivationEvidence) {
			evidence.WorkloadTargetedValidationPassed = true
		},
		"different Catalog digest": func(evidence *ActivationEvidence) {
			evidence.CatalogSHA256 = strings.Repeat("b", 64)
		},
		"extra activated Product": func(evidence *ActivationEvidence) {
			evidence.ObservedProducts = append(append([]string(nil), evidence.ObservedProducts...), "provsql_orders")
		},
		"missing activated Publication": func(evidence *ActivationEvidence) {
			evidence.ObservedPublications = nil
		},
		"extra HOT artifact": func(evidence *ActivationEvidence) {
			evidence.ObservedHotArtifacts = append(append([]ObservedArtifact(nil), evidence.ObservedHotArtifacts...),
				ObservedArtifact{Identity: "provsql-lineitem-v1", Digest: strings.Repeat("c", 64), Bytes: 1})
		},
		"HOT bytes differ from the registry": func(evidence *ActivationEvidence) {
			evidence.ActualHotBytes = evidence.ExpectedHotBytes + 1
		},
		"HOT bytes above the per-instance limit": func(evidence *ActivationEvidence) {
			evidence.ExpectedHotBytes = MaxHotBytesPerInstance + 1
			evidence.ActualHotBytes = MaxHotBytesPerInstance + 1
		},
		"raised HOT limit": func(evidence *ActivationEvidence) {
			evidence.HotLimitBytes = MaxHotBytesPerInstance * 2
		},
		"in-flight query at switch": func(evidence *ActivationEvidence) {
			evidence.DrainBefore.InflightQueries = 1
		},
		"pending artifact at switch": func(evidence *ActivationEvidence) {
			evidence.DrainBefore.PendingArtifacts = 1
		},
		"open reservation at switch": func(evidence *ActivationEvidence) {
			evidence.DrainBefore.OpenReservations = 1
		},
		"process not restarted": func(evidence *ActivationEvidence) {
			evidence.CacheIsolation.ProcessRestarted = false
		},
		"reused process nonce": func(evidence *ActivationEvidence) {
			evidence.CacheIsolation.PreviousProcessNonce = evidence.CacheIsolation.ProcessNonce
		},
		"reused cache namespace": func(evidence *ActivationEvidence) {
			evidence.CacheIsolation.PreviousCacheNamespace = evidence.CacheIsolation.CacheNamespace
		},
		"semantic cache not catalog bound": func(evidence *ActivationEvidence) {
			evidence.CacheIsolation.SemanticCacheCatalogBound = false
		},
		"no outside-Product probe": func(evidence *ActivationEvidence) {
			evidence.OutsideProduct = nil
		},
		"outside Product served": func(evidence *ActivationEvidence) {
			evidence.OutsideProduct = []OutsideProductProbe{{Product: "provsql_orders", Refused: false}}
		},
		"probed its own closure": func(evidence *ActivationEvidence) {
			evidence.OutsideProduct = []OutsideProductProbe{{Product: "final_v5_result_heavy", Refused: true}}
		},
		"failed status": func(evidence *ActivationEvidence) {
			evidence.Status = "fail"
			evidence.Failures = []string{"something went wrong"}
		},
	} {
		evidence := passingEvidence(t, profile)
		mutate(&evidence)
		if err := ValidateActivationEvidence(evidence, profile); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// An activation smoke must never imply that the profile's workload cells ran.
func TestActivationSmokeDoesNotImplyTargetedValidation(t *testing.T) {
	registry := loadRegistry(t)
	for _, profile := range registry.Profiles {
		if profile.Status.TargetedValidationPassed {
			t.Fatalf("profile %q claims targeted validation before any targeted run", profile.Alias)
		}
		if profile.Routable {
			t.Fatalf("profile %q is routable before targeted validation", profile.Alias)
		}
		if profile.TargetedRunEligible != profile.Status.TargetedRunEligible() {
			t.Fatalf("profile %q targeted_run_eligible is inconsistent", profile.Alias)
		}
	}
}

// targeted_run_eligible is exactly the first four states and never includes the
// targeted validation it is meant to authorize.
func TestTargetedRunEligibleIsTheFirstFourStates(t *testing.T) {
	full := ProfileStatus{ClosureComplete: true, CatalogMaterializable: true,
		LiveRouteAvailable: true, ActivationSupported: true}
	if !full.TargetedRunEligible() || full.Routable() {
		t.Fatalf("four-state status = eligible:%t routable:%t", full.TargetedRunEligible(), full.Routable())
	}
	full.TargetedValidationPassed = true
	if !full.Routable() {
		t.Fatal("a five-state status is not routable")
	}
	for _, clear := range []func(*ProfileStatus){
		func(status *ProfileStatus) { status.ClosureComplete = false },
		func(status *ProfileStatus) { status.CatalogMaterializable = false },
		func(status *ProfileStatus) { status.LiveRouteAvailable = false },
		func(status *ProfileStatus) { status.ActivationSupported = false },
	} {
		status := full
		clear(&status)
		if status.TargetedRunEligible() || status.Routable() {
			t.Fatalf("status %+v stayed eligible", status)
		}
	}
}
