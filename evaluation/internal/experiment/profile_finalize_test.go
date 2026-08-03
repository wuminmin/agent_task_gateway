package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildProfileRun writes a real run directory -- config.json plus raw JSONL --
// so these tests exercise FinalizeRun end to end rather than calling the
// matched-pair helper directly.
func buildProfileRun(t *testing.T, direct, bdg *ProfileBinding) string {
	t.Helper()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "profile_activation_smoke",
		CampaignID: "profile-run", ExperimentID: "baseline", Deployments: 1, Warmups: 0, Samples: 1,
		RandomSeed: 20260801,
		Workloads:  []Workload{{ID: "S6", Scales: []string{"100x4"}, Modes: []string{"direct", "novel"}}}}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := NewJSONLWriter(filepath.Join(runDir, "raw", "deployment-01.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for index, arm := range []struct {
		system  string
		mode    string
		binding *ProfileBinding
	}{{"postgresql", "direct", direct}, {"taskgate", "novel", bdg}} {
		sample := validTestSample()
		sample.CampaignID, sample.ExperimentID = config.CampaignID, config.ExperimentID
		sample.CellID, sample.SampleID = "S6/100x4/"+arm.mode, "deployment-01-"+arm.mode
		sample.WorkloadID, sample.Scale, sample.Mode = "S6", "100x4", arm.mode
		sample.System, sample.PairID, sample.PairedSystemOrder = arm.system, "pair-1", arm.mode
		sample.RootGroupID, sample.OrderPosition = "novel", index+1
		sample.RandomSeed, sample.PublicationEligible = config.RandomSeed, false
		sample.ProfileBinding = arm.binding
		if err := writer.Write(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return runDir
}

func resultHeavyBinding(t *testing.T) ProfileBinding {
	t.Helper()
	publications, err := CanonicalPublicationSetSHA256([]string{"final-v5-result-heavy-v1"})
	if err != nil {
		t.Fatal(err)
	}
	return ProfileBinding{Version: ProfileBindingVersion, ProfileID: "profile-a86cd4df5cad6e26",
		ClosureSHA256: sha256Hex([]byte("closure")), CatalogSHA256: sha256Hex([]byte("catalog")),
		DatasetBindingSHA256: sha256Hex([]byte("dataset")), PublicationIdentity: publications}
}

func finalizeProfileRun(t *testing.T, direct, bdg *ProfileBinding) Summary {
	t.Helper()
	summary, err := FinalizeRun(buildProfileRun(t, direct, bdg))
	if err != nil {
		t.Fatalf("FinalizeRun returned %v", err)
	}
	return summary
}

func hasReason(summary Summary, needle string) bool {
	for _, reason := range summary.Reasons {
		if strings.Contains(reason, needle) {
			return true
		}
	}
	return false
}

// A profile-enabled run whose arms agree must not be failed by the profile rule.
func TestFinalizeRunAcceptsMatchingProfileArms(t *testing.T) {
	binding := resultHeavyBinding(t)
	other := binding
	summary := finalizeProfileRun(t, &binding, &other)
	if hasReason(summary, "different deployment profiles") || hasReason(summary, "no deployment profile binding") {
		t.Fatalf("matching arms produced a profile failure: %v", summary.Reasons)
	}
}

// Every way two arms can disagree must reach the summary through FinalizeRun.
func TestFinalizeRunFailsOnProfileMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*ProfileBinding){
		"different profile_id": func(binding *ProfileBinding) {
			binding.ProfileID = "profile-0000000000000000"
		},
		"different closure digest": func(binding *ProfileBinding) {
			binding.ClosureSHA256 = sha256Hex([]byte("other-closure"))
		},
		"different catalog digest": func(binding *ProfileBinding) {
			binding.CatalogSHA256 = sha256Hex([]byte("other-catalog"))
		},
		"different dataset binding": func(binding *ProfileBinding) {
			binding.DatasetBindingSHA256 = sha256Hex([]byte("other-dataset"))
		},
		"different publication set": func(binding *ProfileBinding) {
			set, err := CanonicalPublicationSetSHA256([]string{"provsql-orders-v1", "provsql-lineitem-v1"})
			if err != nil {
				t.Fatal(err)
			}
			binding.PublicationIdentity = set
		},
	} {
		direct := resultHeavyBinding(t)
		bdg := resultHeavyBinding(t)
		mutate(&bdg)
		summary := finalizeProfileRun(t, &direct, &bdg)
		if summary.Status != "fail" {
			t.Fatalf("%s produced status %q", name, summary.Status)
		}
		if !hasReason(summary, "different deployment profiles") {
			t.Fatalf("%s did not name the profile mismatch: %v", name, summary.Reasons)
		}
		// The whole cell is invalid and both arms are retained.
		for cell, distribution := range summary.Cells {
			if distribution.N != 0 {
				t.Fatalf("%s left cell %s with %d passing samples", name, cell, distribution.N)
			}
		}
	}
}

// A missing binding on either arm fails the run rather than being ignored.
func TestFinalizeRunFailsOnMissingProfileBinding(t *testing.T) {
	binding := resultHeavyBinding(t)
	for name, pair := range map[string][2]*ProfileBinding{
		"Direct arm unbound": {nil, &binding},
		"BDG arm unbound":    {&binding, nil},
		"both arms unbound":  {nil, nil},
	} {
		summary := finalizeProfileRun(t, pair[0], pair[1])
		if summary.Status != "fail" {
			t.Fatalf("%s produced status %q", name, summary.Status)
		}
		if !hasReason(summary, "no deployment profile binding") {
			t.Fatalf("%s did not name the missing binding: %v", name, summary.Reasons)
		}
	}
}

// Artifact and Baseline S6 must not be finalized against two different
// Result-heavy profiles.
func TestFinalizeRunRejectsArtifactAndBaselineS6ProfileSplit(t *testing.T) {
	baseline := resultHeavyBinding(t)
	artifact := resultHeavyBinding(t)
	artifact.CatalogSHA256 = sha256Hex([]byte("a-second-result-heavy-catalog"))
	summary := finalizeProfileRun(t, &baseline, &artifact)
	if summary.Status != "fail" || !hasReason(summary, "different deployment profiles") {
		t.Fatalf("a split Result-heavy environment was accepted: status=%q reasons=%v",
			summary.Status, summary.Reasons)
	}
}

// A synthetic framework smoke carries no profile and must stay finalizable.
func TestSyntheticSmokeDoesNotRequireProfileBinding(t *testing.T) {
	if profileBindingRequired(Config{CampaignClass: "pilot", PilotKind: "synthetic_smoke"}) {
		t.Fatal("synthetic smoke was made profile-enabled")
	}
	for _, config := range []Config{
		{CampaignClass: "publication"},
		{CampaignClass: "pilot", PilotKind: "real_system"},
		{CampaignClass: "pilot", PilotKind: "profile_activation_smoke"},
		{CampaignClass: "pilot", PilotKind: "artifact_targeted"},
	} {
		if !profileBindingRequired(config) {
			t.Fatalf("%+v was not profile-enabled", config)
		}
	}
}

// The Publication-set identity is a canonical set digest, not one name.
func TestCanonicalPublicationSetIdentity(t *testing.T) {
	forward, err := CanonicalPublicationSetSHA256([]string{"provsql-orders-v1", "provsql-lineitem-v1"})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := CanonicalPublicationSetSHA256([]string{"provsql-lineitem-v1", "provsql-orders-v1"})
	if err != nil || forward != reversed {
		t.Fatalf("set identity depends on input order: %q vs %q", forward, reversed)
	}
	single, err := CanonicalPublicationSetSHA256([]string{"provsql-orders-v1"})
	if err != nil || single == forward {
		t.Fatalf("a one-member set collided with a two-member set")
	}
	if !validSHA256(forward) {
		t.Fatalf("set identity %q is not SHA-256", forward)
	}
	for name, input := range map[string][]string{
		"empty set":      {},
		"empty member":   {"provsql-orders-v1", "  "},
		"duplicate name": {"provsql-orders-v1", "provsql-orders-v1"},
	} {
		if _, err := CanonicalPublicationSetSHA256(input); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// The Runner rejects a sample whose binding differs from the activated profile,
// including a difference in only one member.
func TestRunnerRejectsSampleBindingDrift(t *testing.T) {
	activated := resultHeavyBinding(t)
	operation := AdapterOperation{ProfileBinding: &activated}
	matching := activated
	if err := validateOperationProfileBinding(operation, Sample{ProfileBinding: &matching}); err != nil {
		t.Fatalf("a matching sample was rejected: %v", err)
	}
	drifted := activated
	drifted.CatalogSHA256 = sha256Hex([]byte("drift"))
	if err := validateOperationProfileBinding(operation, Sample{ProfileBinding: &drifted}); err == nil {
		t.Fatal("a sample that ran against a different Catalog was accepted")
	}
	if err := validateOperationProfileBinding(operation, Sample{}); err == nil {
		t.Fatal("an unbound sample was accepted for a profile-enabled operation")
	}
	// An adapter must not invent a binding the orchestrator never requested.
	if err := validateOperationProfileBinding(AdapterOperation{}, Sample{ProfileBinding: &matching}); err == nil {
		t.Fatal("an adapter-invented profile binding was accepted")
	}
}
