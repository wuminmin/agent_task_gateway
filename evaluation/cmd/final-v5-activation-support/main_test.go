package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func committedRegistry(t *testing.T) finalv5profile.Registry {
	t.Helper()
	registry, err := loadRegistry(filepath.Join(repositoryRoot(t), registryPath))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func committedSupport(t *testing.T) finalv5profile.ActivationSupport {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), supportPath))
	if os.IsNotExist(err) {
		// Activation support does not carry across a contract release, so a
		// freshly amended release legitimately has no manifest until its
		// profiles are re-activated. The registry must then claim no support,
		// which TestRegistryClaimsNoSupportWithoutAManifest checks.
		t.Skip("no activation support manifest for this contract release yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	support, err := finalv5profile.DecodeActivationSupport(payload)
	if err != nil {
		t.Fatalf("committed activation support manifest is invalid: %v", err)
	}
	return support
}

// The seven profiles that completed a live activation smoke are supported, and
// the four that did not are not. This is the whole point of the change: a
// working activator is not evidence about a profile it never activated.
func TestCommittedManifestSupportsExactlyTheSevenProvenProfiles(t *testing.T) {
	support := committedSupport(t)
	proven := map[string]bool{"result-heavy": true, "provsql-nonce-join": true, "expense-detail": true,
		"attack-expense-detail": true, "concurrency-expense-detail": true,
		"rls-unlimited": true, "rls-bounded": true}
	unproven := map[string]bool{"analytics-orders": true, "analytics-orders-lineitem": true,
		"depth4-semantic-view": true, "exposure-scale": true}

	seen := map[string]bool{}
	for _, profile := range support.Profiles {
		seen[profile.ProfileAlias] = true
		switch {
		case proven[profile.ProfileAlias]:
			if !profile.ActivationSupported || !profile.ActivationSmokePassed {
				t.Errorf("proven profile %s is not supported", profile.ProfileAlias)
			}
			if len(profile.ActivationEvidenceSHA256) == 0 {
				t.Errorf("proven profile %s carries no evidence digest", profile.ProfileAlias)
			}
		case unproven[profile.ProfileAlias]:
			if profile.ActivationSupported || profile.ActivationSmokePassed {
				t.Errorf("unproven profile %s claims activation support", profile.ProfileAlias)
			}
			if !strings.Contains(profile.Reason, "live_activation_smoke_not_executed") {
				t.Errorf("unproven profile %s reason = %q", profile.ProfileAlias, profile.Reason)
			}
		default:
			t.Errorf("unexpected profile %s in the manifest", profile.ProfileAlias)
		}
	}
	for alias := range proven {
		if !seen[alias] {
			t.Errorf("proven profile %s is missing from the manifest", alias)
		}
	}
	for alias := range unproven {
		if !seen[alias] {
			t.Errorf("unproven profile %s is missing from the manifest", alias)
		}
	}
}

// Result-heavy was activated twice in one smoke -- an initial activation and a
// switch-back -- and both digests belong in its claim.
func TestResultHeavyCarriesBothActivationEvidenceDigests(t *testing.T) {
	for _, profile := range committedSupport(t).Profiles {
		if profile.ProfileAlias != "result-heavy" {
			continue
		}
		if len(profile.ActivationEvidenceSHA256) != 2 {
			t.Fatalf("result-heavy carries %d evidence digests, want 2 (activation and switch-back)",
				len(profile.ActivationEvidenceSHA256))
		}
		if profile.ActivationEvidenceSHA256[0] == profile.ActivationEvidenceSHA256[1] {
			t.Fatal("result-heavy's two evidence digests are the same document")
		}
		return
	}
	t.Fatal("result-heavy is absent from the activation support manifest")
}

// Without a manifest for this contract release, no profile may claim support.
// This is what stops a re-freeze from silently inheriting the previous
// release's activation evidence.
func TestRegistryClaimsNoSupportWithoutAManifest(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repositoryRoot(t), supportPath)); !os.IsNotExist(err) {
		t.Skip("this contract release has an activation support manifest")
	}
	for _, profile := range committedRegistry(t).Profiles {
		if profile.Status.ActivationSupported || profile.Status.ActivationSmokePassed ||
			profile.TargetedRunEligible || profile.Routable {
			t.Errorf("%s claims activation support with no manifest for this release", profile.Alias)
		}
	}
}

// The committed registry must be exactly what the committed manifest derives.
// Nothing may hand-edit a readiness state into the registry.
func TestCommittedRegistryMatchesTheManifest(t *testing.T) {
	registry := committedRegistry(t)
	byID, err := committedSupport(t).SupportedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	supported, eligible, routable := 0, 0, 0
	for _, profile := range registry.Profiles {
		derived, reason := finalv5profile.ActivationSupportFor(byID, profile.ID,
			profile.CatalogSHA256, profile.Closure.SHA256)
		if derived != profile.Status.ActivationSupported {
			t.Errorf("%s: registry activation_supported=%t, manifest derives %t (%s)",
				profile.Alias, profile.Status.ActivationSupported, derived, reason.Code)
		}
		if profile.Status.ActivationSupported {
			supported++
		}
		if profile.TargetedRunEligible {
			eligible++
		}
		if profile.Routable {
			routable++
		}
		// targeted_run_eligible and routable are derived, never stored opinions.
		if profile.TargetedRunEligible != profile.Status.TargetedRunEligible() {
			t.Errorf("%s: targeted_run_eligible is not derived", profile.Alias)
		}
		if profile.Routable != profile.Status.Routable() {
			t.Errorf("%s: routable is not derived", profile.Alias)
		}
	}
	if supported != 7 {
		t.Errorf("registry reports %d activation-supported profiles, want 7", supported)
	}
	if eligible != 7 {
		t.Errorf("registry reports %d targeted-run-eligible profiles, want 7", eligible)
	}
	if routable != 0 {
		t.Errorf("registry reports %d routable profiles, want 0", routable)
	}
}

// A profile is only routable once a targeted run has passed. An activation
// smoke must never be able to produce that state.
func TestActivationSmokeNeverImpliesTargetedValidation(t *testing.T) {
	for _, profile := range committedRegistry(t).Profiles {
		if profile.Status.TargetedValidationPassed {
			t.Errorf("%s claims targeted validation without a targeted run", profile.Alias)
		}
		if profile.Status.ActivationSmokePassed && profile.Status.TargetedValidationPassed {
			t.Errorf("%s promoted an activation smoke into a targeted validation", profile.Alias)
		}
	}
}

// Publication-eligible evidence belongs to a Campaign. Recycling it into a
// pilot readiness state would let Campaign output justify its own preconditions.
func TestPublicationEligibleEvidenceCannotSupportAProfile(t *testing.T) {
	profile := finalv5profile.Profile{ID: "profile-abc", Alias: "result-heavy",
		CatalogSHA256: strings.Repeat("a", 64),
		Closure:       finalv5profile.Closure{SHA256: strings.Repeat("b", 64)},
		Status: finalv5profile.ProfileStatus{ClosureComplete: true,
			CatalogMaterializable: true, LiveRouteAvailable: true}}
	passing := finalv5profile.ActivationEvidence{Status: "pass", ActivationSmokePassed: true,
		ContractRelease: "final-v5-contracts-v1.2", ProfileID: profile.ID,
		ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256}

	claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: passing}},
		"final-v5-contracts-v1.2", map[string]string{})
	if !claim.ActivationSupported {
		t.Fatal("a valid pilot evidence document did not support the profile")
	}

	for name, mutate := range map[string]func(*finalv5profile.ActivationEvidence){
		"publication eligible": func(e *finalv5profile.ActivationEvidence) { e.PublicationEligible = true },
		"failed status":        func(e *finalv5profile.ActivationEvidence) { e.Status = "fail" },
		"smoke not passed":     func(e *finalv5profile.ActivationEvidence) { e.ActivationSmokePassed = false },
		"different contract":   func(e *finalv5profile.ActivationEvidence) { e.ContractRelease = "final-v5-contracts-v1.1" },
		"different profile":    func(e *finalv5profile.ActivationEvidence) { e.ProfileID = "profile-other" },
		"different closure":    func(e *finalv5profile.ActivationEvidence) { e.ClosureSHA256 = strings.Repeat("d", 64) },
		"different catalog":    func(e *finalv5profile.ActivationEvidence) { e.CatalogSHA256 = strings.Repeat("e", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := passing
			mutate(&evidence)
			claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: evidence}},
				"final-v5-contracts-v1.2", map[string]string{})
			if claim.ActivationSupported || claim.ActivationSmokePassed {
				t.Fatalf("%s evidence supported the profile", name)
			}
			if !strings.Contains(claim.Reason, "live_activation_smoke_not_executed") {
				t.Fatalf("reason = %q", claim.Reason)
			}
		})
	}
}

// Passing evidence for a profile that is otherwise blocked must not promote it.
func TestEvidenceCannotPaperOverAnotherBlockedState(t *testing.T) {
	profile := finalv5profile.Profile{ID: "profile-abc", Alias: "analytics-orders",
		CatalogSHA256: strings.Repeat("a", 64),
		Closure:       finalv5profile.Closure{SHA256: strings.Repeat("b", 64)},
		Status: finalv5profile.ProfileStatus{ClosureComplete: true,
			CatalogMaterializable: true, LiveRouteAvailable: false}}
	passing := finalv5profile.ActivationEvidence{Status: "pass", ActivationSmokePassed: true,
		ContractRelease: "final-v5-contracts-v1.2", ProfileID: profile.ID,
		ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256}
	claim := claimFor(profile, []evidenceDocument{{digest: strings.Repeat("c", 64), evidence: passing}},
		"final-v5-contracts-v1.2", map[string]string{})
	if claim.ActivationSupported {
		t.Fatal("a profile with no live route was supported because evidence existed")
	}
	if !strings.Contains(claim.Reason, "live_route_available_false") {
		t.Fatalf("reason = %q", claim.Reason)
	}
}

// The manifest is committed to source control. It must carry no secret and no
// business data -- only identities and digests.
func TestCommittedManifestCarriesNoSecretsOrBusinessData(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join(repositoryRoot(t), supportPath))
	if os.IsNotExist(err) {
		t.Skip("no activation support manifest for this contract release yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(payload))
	for _, forbidden := range []string{"postgres://", "password", "token", "bearer",
		"select ", "insert ", "task-", "parquet", "s3://", "dsn"} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("activation support manifest contains %q", forbidden)
		}
	}
	// Every digest-shaped value must be exactly a digest, never a payload.
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	for key, value := range document {
		if !strings.HasSuffix(key, "_sha256") {
			continue
		}
		text, ok := value.(string)
		if !ok || len(text) != 64 {
			t.Errorf("%s is not a bare SHA-256", key)
		}
	}
}
