package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testActivatedProfile() *ProfileBinding {
	publication, _ := CanonicalPublicationSetSHA256([]string{"final-v5-result-heavy-v1"})
	return &ProfileBinding{Version: ProfileBindingVersion,
		ProfileID:     "profile-a86cd4df5cad6e26",
		ClosureSHA256: strings.Repeat("a", 64), CatalogSHA256: strings.Repeat("b", 64),
		DatasetBindingSHA256: strings.Repeat("c", 64), PublicationIdentity: publication}
}

func artifactTargetedConfig() Config {
	return Config{SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "artifact_targeted",
		CampaignID: "pilot-artifact-targeted-local-only", ExperimentID: "artifact",
		Deployments: 1, Warmups: 0, Samples: 1, RandomSeed: 20260804, FreshRootPerSample: true,
		Workloads: []Workload{{ID: "result-heavy", Scales: []string{"100x4"}, Modes: []string{"novel"}}}}
}

// The orchestrator owns the deployment profile: every operation it emits must
// carry the activated binding, so an Adapter can only ever echo it back.
func TestOperationsCarryTheActivatedProfileBinding(t *testing.T) {
	position := 0
	profile := testActivatedProfile()
	operations := buildOperations(artifactTargetedConfig(), "deployment-01", 1, 1, &position, profile)
	if len(operations) == 0 {
		t.Fatal("no operations were built")
	}
	for _, operation := range operations {
		if operation.ProfileBinding == nil {
			t.Fatalf("operation %s carries no profile binding", operation.SampleID)
		}
		if !operation.ProfileBinding.Equal(*profile) {
			t.Fatalf("operation %s carries a different profile", operation.SampleID)
		}
	}
}

// An unbound run still builds unbound operations, so the many existing pilot
// paths are unchanged.
func TestOperationsStayUnboundWithoutAProfile(t *testing.T) {
	position := 0
	operations := buildOperations(artifactTargetedConfig(), "deployment-01", 1, 1, &position, nil)
	for _, operation := range operations {
		if operation.ProfileBinding != nil {
			t.Fatalf("operation %s acquired a profile it was not given", operation.SampleID)
		}
	}
}

// A sample may never claim a profile the operation did not request, and may
// never disagree with the one it did. A matching profile_id with a different
// Catalog digest is a different deployment.
func TestSampleProfileBindingMustEchoTheOperation(t *testing.T) {
	profile := testActivatedProfile()
	operation := AdapterOperation{SampleID: "s-1", ProfileBinding: profile}

	if err := validateOperationProfileBinding(operation, Sample{ProfileBinding: profile}); err != nil {
		t.Fatalf("a faithful echo was rejected: %v", err)
	}
	if err := validateOperationProfileBinding(operation, Sample{}); err == nil {
		t.Fatal("a sample with no profile binding was accepted for a bound operation")
	}
	if err := validateOperationProfileBinding(AdapterOperation{SampleID: "s-1"},
		Sample{ProfileBinding: profile}); err == nil {
		t.Fatal("a sample claimed a profile the operation never requested")
	}
	for name, mutate := range map[string]func(*ProfileBinding){
		"catalog":     func(b *ProfileBinding) { b.CatalogSHA256 = strings.Repeat("d", 64) },
		"closure":     func(b *ProfileBinding) { b.ClosureSHA256 = strings.Repeat("d", 64) },
		"profile id":  func(b *ProfileBinding) { b.ProfileID = "profile-0000000000000000" },
		"dataset":     func(b *ProfileBinding) { b.DatasetBindingSHA256 = strings.Repeat("d", 64) },
		"publication": func(b *ProfileBinding) { b.PublicationIdentity = strings.Repeat("d", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			drifted := *profile
			mutate(&drifted)
			if err := validateOperationProfileBinding(operation, Sample{ProfileBinding: &drifted}); err == nil {
				t.Fatalf("a sample disagreeing on %s was accepted", name)
			}
		})
	}
}

func TestRunnerProfileBindingFileIsStrict(t *testing.T) {
	if binding, err := loadRunnerProfileBinding(""); binding != nil || err != nil {
		t.Fatalf("an absent path = %v, %v", binding, err)
	}
	directory := t.TempDir()
	for name, payload := range map[string]string{
		"unknown field":  `{"version":"taskgate-final-v5-profile-binding-v1","unexpected":1}`,
		"incomplete":     `{"version":"taskgate-final-v5-profile-binding-v1","profile_id":"profile-a86cd4df5cad6e26"}`,
		"malformed json": `{`,
		"empty":          `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadRunnerProfileBinding(path); err == nil {
				t.Fatalf("%s binding was accepted", name)
			}
		})
	}
	if _, err := loadRunnerProfileBinding(filepath.Join(directory, "absent.json")); err == nil {
		t.Fatal("a missing binding file was accepted")
	}
}
