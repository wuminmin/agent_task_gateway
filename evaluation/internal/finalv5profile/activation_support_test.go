package finalv5profile

import (
	"strings"
	"testing"
)

// activation_supported used to be one global constant, so proving the activator
// worked once marked every profile -- including four that had never been
// activated -- as supported. These cases pin the replacement: a per-profile
// claim that fails closed on every way it could be wrong.

const (
	supportedClosure = "a86cd4df5cad6e2685d1930caeccdbe3a865194fcef33faeca86b7c2c592bdbd"
	supportedCatalog = "533837084c0df141a0fac6e74788a4c2b9eb84611c1f96d4c760806745b4f709"
)

func validSupportManifest() ActivationSupport {
	return ActivationSupport{SchemaVersion: 1, Record: ActivationSupportRecord,
		ContractRelease:                      "final-v5-contracts-v1.2",
		ProfileRegistrySHA256:                strings.Repeat("1", 64),
		ActivationImplementationAvailable:    true,
		ActivationSmokeManifestSHA256:        strings.Repeat("2", 64),
		OutsideProductRouteMatrixSHA256:      strings.Repeat("3", 64),
		SemanticCacheIsolationEvidenceSHA256: strings.Repeat("4", 64),
		OutsideProductRouteMatrixStatus:      "pass",
		SemanticCacheIsolationStatus:         "pass",
		SemanticCacheCatalogBound:            true,
		Profiles: []ProfileActivationSupport{{
			ProfileID: ProfileID(supportedClosure), ProfileAlias: "result-heavy",
			CatalogSHA256: supportedCatalog, ClosureSHA256: supportedClosure,
			ActivationSupported: true, ActivationSmokePassed: true,
			ActivationEvidenceSHA256: []string{strings.Repeat("5", 64)}}},
	}
}

func TestActivationSupportManifestAcceptsAProvenProfile(t *testing.T) {
	support := validSupportManifest()
	byID, err := support.SupportedProfiles()
	if err != nil {
		t.Fatalf("a complete manifest was rejected: %v", err)
	}
	supported, reason := ActivationSupportFor(byID, ProfileID(supportedClosure), supportedCatalog, supportedClosure)
	if !supported || reason.Code != "" {
		t.Fatalf("a proven profile was not supported: %v", reason)
	}
}

// 1. A profile the manifest never mentions can never become supported. This is
//    the case that produced the wrong state before: the activator existed, so
//    every profile inherited support.
func TestProfileAbsentFromManifestIsNeverSupported(t *testing.T) {
	byID, err := validSupportManifest().SupportedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	supported, reason := ActivationSupportFor(byID, "profile-0000000000000000",
		strings.Repeat("a", 64), strings.Repeat("b", 64))
	if supported {
		t.Fatal("a profile absent from the manifest was reported as supported")
	}
	if reason.Code != "profile_absent_from_activation_support_manifest" {
		t.Fatalf("reason = %q", reason.Code)
	}
}

// 3. Evidence recorded against a different profile Catalog does not transfer.
// 4. Neither does evidence recorded against a different closure.
func TestActivationSupportIsBoundToTheProvenIdentity(t *testing.T) {
	byID, err := validSupportManifest().SupportedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		catalog, closure, code string
	}{
		"catalog drift": {strings.Repeat("d", 64), supportedClosure, "activation_evidence_catalog_drift"},
		"closure drift": {supportedCatalog, strings.Repeat("e", 64), "activation_evidence_closure_drift"},
	} {
		t.Run(name, func(t *testing.T) {
			supported, reason := ActivationSupportFor(byID, ProfileID(supportedClosure),
				testCase.catalog, testCase.closure)
			if supported {
				t.Fatalf("%s was accepted", name)
			}
			if reason.Code != testCase.code {
				t.Fatalf("reason = %q, want %q", reason.Code, testCase.code)
			}
		})
	}
}

// 2, 4, 6, 7. A manifest that could not justify its own claims is rejected
// outright rather than partially honoured.
func TestActivationSupportManifestFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*ActivationSupport){
		"missing evidence digest": func(support *ActivationSupport) {
			support.Profiles[0].ActivationEvidenceSHA256 = nil
		},
		"malformed evidence digest": func(support *ActivationSupport) {
			support.Profiles[0].ActivationEvidenceSHA256 = []string{"not-a-digest"}
		},
		"profile id not derived from its closure": func(support *ActivationSupport) {
			support.Profiles[0].ProfileID = "profile-1111111111111111"
		},
		"no catalog digest": func(support *ActivationSupport) {
			support.Profiles[0].CatalogSHA256 = ""
		},
		"supported without a passed smoke": func(support *ActivationSupport) {
			support.Profiles[0].ActivationSmokePassed = false
		},
		"supported and blocked at once": func(support *ActivationSupport) {
			support.Profiles[0].Reason = "live_activation_smoke_not_executed"
		},
		"route matrix failed": func(support *ActivationSupport) {
			support.OutsideProductRouteMatrixStatus = "fail"
		},
		"route matrix had failing probes": func(support *ActivationSupport) {
			support.OutsideProductRouteMatrixFailedCount = 1
		},
		"semantic cache isolation failed": func(support *ActivationSupport) {
			support.SemanticCacheIsolationStatus = "fail"
		},
		"semantic cache not catalog bound": func(support *ActivationSupport) {
			support.SemanticCacheCatalogBound = false
		},
		"implementation unavailable": func(support *ActivationSupport) {
			support.ActivationImplementationAvailable = false
		},
		"unsupported profile with no reason": func(support *ActivationSupport) {
			support.Profiles[0].ActivationSupported = false
			support.Profiles[0].ActivationSmokePassed = false
			support.Profiles[0].Reason = ""
		},
		"unsupported profile claiming a passed smoke": func(support *ActivationSupport) {
			support.Profiles[0].ActivationSupported = false
			support.Profiles[0].Reason = "blocked"
		},
		"duplicate profile": func(support *ActivationSupport) {
			support.Profiles = append(support.Profiles, support.Profiles[0])
		},
		"no contract release": func(support *ActivationSupport) {
			support.ContractRelease = ""
		},
		"wrong record": func(support *ActivationSupport) {
			support.Record = "taskgate-something-else"
		},
		"no profiles": func(support *ActivationSupport) {
			support.Profiles = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			support := validSupportManifest()
			mutate(&support)
			if err := support.Validate(); err == nil {
				t.Fatalf("invalid manifest %q was accepted", name)
			}
			if _, err := support.SupportedProfiles(); err == nil {
				t.Fatalf("invalid manifest %q produced a support set", name)
			}
		})
	}
}

// An unsupported entry is legitimate and must be preserved, with its reason. It
// is the honest record of a profile that has not been activated.
func TestActivationSupportManifestKeepsUnsupportedProfiles(t *testing.T) {
	support := validSupportManifest()
	support.Profiles = append(support.Profiles, ProfileActivationSupport{
		ProfileID: "profile-2222222222222222", ProfileAlias: "exposure-scale",
		ActivationSupported: false, ActivationSmokePassed: false,
		Reason:  "catalog_materializable_false; live_activation_smoke_not_executed",
		Blocked: []string{"catalog_materializable_false", "live_activation_smoke_not_executed"}})
	byID, err := support.SupportedProfiles()
	if err != nil {
		t.Fatalf("a manifest with an unsupported entry was rejected: %v", err)
	}
	supported, reason := ActivationSupportFor(byID, "profile-2222222222222222",
		strings.Repeat("a", 64), strings.Repeat("b", 64))
	if supported {
		t.Fatal("an explicitly unsupported profile was reported as supported")
	}
	if reason.Code != "live_activation_smoke_not_executed" || !strings.Contains(reason.Detail, "catalog_materializable_false") {
		t.Fatalf("reason = %+v", reason)
	}
}

// ApplyActivationSupport is what the registry generator calls. It must be able
// to demote a profile, not only promote one, and must keep exactly one
// activation_supported reason.
func TestApplyActivationSupportDemotesAndKeepsOneReason(t *testing.T) {
	byID, err := validSupportManifest().SupportedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	profiles := []Profile{
		{ID: ProfileID(supportedClosure), Alias: "result-heavy", CatalogSHA256: supportedCatalog,
			Closure: Closure{SHA256: supportedClosure},
			Status: ProfileStatus{ClosureComplete: true, CatalogMaterializable: true,
				LiveRouteAvailable: true, ActivationSupported: false,
				UnresolvedReasons: []UnresolvedReason{{State: "activation_supported",
					Code: "live_activation_smoke_not_executed"}}}},
		// Pre-set to true to prove Apply can take it away again.
		{ID: "profile-3333333333333333", Alias: "exposure-scale", CatalogSHA256: strings.Repeat("f", 64),
			Closure: Closure{SHA256: strings.Repeat("f", 64)},
			Status: ProfileStatus{ClosureComplete: true, CatalogMaterializable: false,
				ActivationSupported: true, ActivationSmokePassed: true}},
	}
	ApplyActivationSupport(profiles, byID)

	if !profiles[0].Status.ActivationSupported || !profiles[0].Status.ActivationSmokePassed {
		t.Fatal("a proven profile was not promoted")
	}
	if !profiles[0].TargetedRunEligible || profiles[0].Routable {
		t.Fatalf("eligible=%t routable=%t", profiles[0].TargetedRunEligible, profiles[0].Routable)
	}
	for _, reason := range profiles[0].Status.UnresolvedReasons {
		if reason.State == "activation_supported" {
			t.Fatal("a supported profile kept an activation_supported blocker")
		}
	}
	if profiles[1].Status.ActivationSupported || profiles[1].Status.ActivationSmokePassed {
		t.Fatal("an unproven profile kept its inherited support")
	}
	if profiles[1].TargetedRunEligible || profiles[1].Routable {
		t.Fatal("an unproven profile stayed eligible")
	}
	blockers := 0
	for _, reason := range profiles[1].Status.UnresolvedReasons {
		if reason.State == "activation_supported" {
			blockers++
		}
	}
	if blockers != 1 {
		t.Fatalf("unproven profile carries %d activation_supported reasons, want exactly 1", blockers)
	}
}

// Encoding must be deterministic: the manifest is source controlled and is
// regenerated in verification.
func TestActivationSupportEncodingIsDeterministic(t *testing.T) {
	support := validSupportManifest()
	first, err := EncodeActivationSupport(support)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeActivationSupport(support)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("activation support encoding is not deterministic")
	}
	decoded, err := DecodeActivationSupport(first)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(decoded.Profiles) != 1 || !decoded.Profiles[0].ActivationSupported {
		t.Fatalf("round trip lost the claim: %+v", decoded.Profiles)
	}
}

// A document carrying an unknown field is refused rather than silently ignored,
// so a renamed or dropped gate cannot pass unnoticed.
func TestActivationSupportRejectsUnknownFields(t *testing.T) {
	if _, err := DecodeActivationSupport([]byte(`{"schema_version":1,"unexpected":true}`)); err == nil {
		t.Fatal("an unknown manifest field was accepted")
	}
}
