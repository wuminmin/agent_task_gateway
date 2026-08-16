package finalv5profile

import (
	"testing"
	"time"
)

func passingActivationInvocation() ActivationInvocation {
	return ActivationInvocation{Root: "/repo", ComposeProject: "project", ComposeFiles: "compose.yaml",
		DeploymentID: "deployment", ProfileID: "profile-right", RegistryPath: "config/profiles/registry.json",
		GatewayURL: "http://gateway", AdminTokenEnv: "ADMIN", PreviousProfileID: "profile-left", Sequence: 13,
		EvidenceOut: "/evidence/013.json", OutsideProducts: "expense_detail",
		ProfileArtifactDir: "/artifacts/profile-right", ProfileArtifactManifest: "/artifacts/manifest.json",
		BusinessDSNEnv: "BUSINESS_DSN", SchemaAttestations: "config/profiles/schema-attestations-v1.json",
		ProbeTokenEnv: "ALICE", ReadyTimeout: 10 * time.Minute, DatasetBinding: "/binding.json"}
}

func TestActivationInvocationCarriesTheVerifiedActivatorContract(t *testing.T) {
	arguments, err := passingActivationInvocation().Arguments()
	if err != nil {
		t.Fatal(err)
	}
	wantPairs := map[string]string{"-previous-profile-id": "profile-left", "-profile-id": "profile-right",
		"-activation-sequence": "13", "-outside-products": "expense_detail",
		"-profile-artifact-dir": "/artifacts/profile-right", "-profile-artifact-manifest": "/artifacts/manifest.json",
		"-business-dsn-env": "BUSINESS_DSN", "-schema-attestations": "config/profiles/schema-attestations-v1.json",
		"-probe-token-env": "ALICE", "-dataset-binding": "/binding.json"}
	for flag, want := range wantPairs {
		found := false
		for index := 0; index+1 < len(arguments); index++ {
			if arguments[index] == flag && arguments[index+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("activation arguments omit %s=%s: %v", flag, want, arguments)
		}
	}
	for _, forbidden := range []string{"docker", "compose", "stop", "up", "complete_task"} {
		for _, argument := range arguments {
			if argument == forbidden {
				t.Fatalf("activation invocation grew independent %s logic: %v", forbidden, arguments)
			}
		}
	}
}

func TestActivationInvocationRejectsEveryRequiredSwitchInput(t *testing.T) {
	for name, mutate := range map[string]func(*ActivationInvocation){
		"profile":                     func(value *ActivationInvocation) { value.ProfileID = "" },
		"predecessor evidence output": func(value *ActivationInvocation) { value.EvidenceOut = "" },
		"outside Product probe":       func(value *ActivationInvocation) { value.OutsideProducts = "" },
		"artifact directory":          func(value *ActivationInvocation) { value.ProfileArtifactDir = "" },
		"artifact manifest":           func(value *ActivationInvocation) { value.ProfileArtifactManifest = "" },
		"schema attestation":          func(value *ActivationInvocation) { value.SchemaAttestations = "" },
		"sequence":                    func(value *ActivationInvocation) { value.Sequence = 0 },
		"readiness timeout":           func(value *ActivationInvocation) { value.ReadyTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invocation := passingActivationInvocation()
			mutate(&invocation)
			if _, err := invocation.Arguments(); err == nil {
				t.Fatal("incomplete activation invocation was accepted")
			}
		})
	}
}
