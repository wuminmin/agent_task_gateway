package experiment

import (
	"strings"
	"testing"
)

func testBinding() ProfileBinding {
	publications, err := CanonicalPublicationSetSHA256([]string{"final-v5-result-heavy-v1"})
	if err != nil {
		panic(err)
	}
	return ProfileBinding{Version: ProfileBindingVersion, ProfileID: "profile-a86cd4df5cad6e26",
		ClosureSHA256: strings.Repeat("a", 64), CatalogSHA256: strings.Repeat("b", 64),
		DatasetBindingSHA256: strings.Repeat("c", 64), PublicationIdentity: publications}
}

func pairedSamples(direct, bdg ProfileBinding) []Sample {
	return []Sample{
		{CellID: "baseline/S6/100x4", PairID: "pair-1", System: "postgresql", Status: "pass", ProfileBinding: &direct},
		{CellID: "baseline/S6/100x4", PairID: "pair-1", System: "taskgate", Status: "pass", ProfileBinding: &bdg},
	}
}

func TestMatchedPairProfileAcceptsIdenticalArms(t *testing.T) {
	if failures := ValidateMatchedPairProfiles(pairedSamples(testBinding(), testBinding())); len(failures) != 0 {
		t.Fatalf("identical arms were rejected: %+v", failures)
	}
}

// Every field of the matched-pair rule must fail closed on its own.
func TestMatchedPairProfileRejectsEveryMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*ProfileBinding){
		"different profile_id": func(binding *ProfileBinding) {
			binding.ProfileID = "profile-0000000000000000"
		},
		"same profile_id but different Catalog SHA": func(binding *ProfileBinding) {
			binding.CatalogSHA256 = strings.Repeat("d", 64)
		},
		"same Catalog but different closure SHA": func(binding *ProfileBinding) {
			binding.ClosureSHA256 = strings.Repeat("e", 64)
		},
		"different dataset binding": func(binding *ProfileBinding) {
			binding.DatasetBindingSHA256 = strings.Repeat("f", 64)
		},
		"different publication set": func(binding *ProfileBinding) {
			other, err := CanonicalPublicationSetSHA256([]string{"provsql-orders-v1", "provsql-lineitem-v1"})
			if err != nil {
				panic(err)
			}
			binding.PublicationIdentity = other
		},
	} {
		bdg := testBinding()
		mutate(&bdg)
		samples := pairedSamples(testBinding(), bdg)
		failures := ValidateMatchedPairProfiles(samples)
		if len(failures) == 0 {
			t.Fatalf("%s was accepted", name)
		}
		// The whole cell is invalidated, never just the offending arm.
		invalidated, cells := InvalidateMismatchedProfileCells(samples)
		if len(cells) != 1 || cells[0] != "baseline/S6/100x4" {
			t.Fatalf("%s invalidated cells %v", name, cells)
		}
		if len(invalidated) != 2 {
			t.Fatalf("%s dropped an arm: %d samples remain", name, len(invalidated))
		}
		for _, sample := range invalidated {
			if sample.Status != "invalid" || sample.ErrorCode != "matched_pair_profile_mismatch" {
				t.Fatalf("%s left arm %q as status=%q code=%q", name, sample.System, sample.Status, sample.ErrorCode)
			}
		}
	}
}

// Artifact and Baseline S6 must never run against two different Result-heavy
// profiles: they are one cell family sharing one environment.
func TestArtifactAndBaselineS6MustShareOneResultHeavyProfile(t *testing.T) {
	baseline := testBinding()
	artifact := testBinding()
	artifact.ProfileID = "profile-1111111111111111"
	samples := []Sample{
		{CellID: "result-heavy/100x4", PairID: "pair-1", ExperimentID: "baseline", System: "taskgate",
			Status: "pass", ProfileBinding: &baseline},
		{CellID: "result-heavy/100x4", PairID: "pair-1", ExperimentID: "artifact", System: "taskgate",
			Status: "pass", ProfileBinding: &artifact},
	}
	failures := ValidateMatchedPairProfiles(samples)
	if len(failures) == 0 {
		t.Fatal("Artifact and Baseline S6 were allowed to use different Result-heavy profiles")
	}
	if failures[0].Field != "profile_id" {
		t.Fatalf("failure named field %q", failures[0].Field)
	}
}

func TestProfileBindingValidationIsFailClosed(t *testing.T) {
	if err := RequireProfileBinding(Sample{}); err == nil {
		t.Fatal("a sample with no profile binding was accepted")
	}
	for name, mutate := range map[string]func(*ProfileBinding){
		"unsupported version":  func(binding *ProfileBinding) { binding.Version = "v0" },
		"hand written profile": func(binding *ProfileBinding) { binding.ProfileID = "result-heavy" },
		"non digest closure":   func(binding *ProfileBinding) { binding.ClosureSHA256 = "abc" },
		"non digest catalog":   func(binding *ProfileBinding) { binding.CatalogSHA256 = "" },
		"non digest dataset":   func(binding *ProfileBinding) { binding.DatasetBindingSHA256 = "0x1" },
		"publication name not a set digest": func(binding *ProfileBinding) {
			binding.PublicationIdentity = "final-v5-result-heavy-v1"
		},
	} {
		binding := testBinding()
		mutate(&binding)
		if err := RequireProfileBinding(Sample{ProfileBinding: &binding}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	valid := testBinding()
	if err := RequireProfileBinding(Sample{ProfileBinding: &valid}); err != nil {
		t.Fatalf("a complete binding was rejected: %v", err)
	}
}

// A single unpaired arm cannot be compared, but it must still carry a binding
// so a later pairing can be checked at all.
func TestUnpairedArmIsNotSilentlyAccepted(t *testing.T) {
	binding := testBinding()
	single := []Sample{{CellID: "baseline/S6/100x4", PairID: "pair-1", System: "postgresql",
		Status: "pass", ProfileBinding: &binding}}
	if failures := ValidateMatchedPairProfiles(single); len(failures) != 0 {
		t.Fatalf("a single arm produced failures: %+v", failures)
	}
	unbound := []Sample{{CellID: "baseline/S6/100x4", PairID: "pair-1", System: "postgresql", Status: "pass"}}
	if err := RequireProfileBinding(unbound[0]); err == nil {
		t.Fatal("an unbound Direct arm was accepted")
	}
}
