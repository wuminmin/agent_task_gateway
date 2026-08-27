package experiment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

func TestSplitPublicationPlanRequires125Plus36Plus11WithoutProfileFiction(t *testing.T) {
	plan := splitPublicationPlanFixture()
	if err := ValidateSplitPublicationPlan(plan); err != nil {
		t.Fatalf("valid split publication plan: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*finalv5profile.CampaignPlan)
	}{
		{"profile denominator", func(value *finalv5profile.CampaignPlan) { value.Deployments[0].Cells = value.Deployments[0].Cells[1:] }},
		{"scale denominator", func(value *finalv5profile.CampaignPlan) {
			value.NonProfileCampaigns[0].Cells = value.NonProfileCampaigns[0].Cells[1:]
		}},
		{"compiler processes", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[1].ProcessReplicates = 1 }},
		{"state inheritance", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[0].StateInheritance = true }},
		{"profile binding fiction", func(value *finalv5profile.CampaignPlan) { value.NonProfileCampaigns[0].ProfileBinding = "required" }},
		{"pilot aggregate", func(value *finalv5profile.CampaignPlan) {
			value.PreregisteredAggregates = []finalv5profile.CampaignPreregisteredAggregate{{Cell: "concurrency/shared-root/50/natural_contention"}}
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			mutated := splitPublicationPlanFixture()
			test.mutate(&mutated)
			if err := ValidateSplitPublicationPlan(mutated); err == nil {
				t.Fatal("mutated publication plan was accepted")
			}
		})
	}
}

func TestPublicationProfileRequiresCredentialGatedAdapterStderr(t *testing.T) {
	root := t.TempDir()
	stderrPath := filepath.Join(root, "adapter.log")
	stderrValue := []byte("real_concurrency_measurement_failed: retained cause\n")
	if err := os.WriteFile(stderrPath, stderrValue, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := finalv5publication.ValidateAdapterStderr(stderrValue, []string{"PrivateValue_2039"})
	if err != nil {
		t.Fatal(err)
	}
	scanValue, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	scanPath := filepath.Join(root, "adapter.scan.json")
	if err := os.WriteFile(scanPath, append(scanValue, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	stderrSHA, _ := FileSHA256(stderrPath)
	scanSHA, _ := FileSHA256(scanPath)
	stderrInfo, _ := os.Stat(stderrPath)
	scanInfo, _ := os.Stat(scanPath)
	stderrFile := finalv5profile.CampaignEvidenceFile{Kind: "adapter_stderr", Experiment: "concurrency",
		Path: "adapter.log", SHA256: stderrSHA, Bytes: stderrInfo.Size()}
	scanFile := finalv5profile.CampaignEvidenceFile{Kind: "adapter_stderr_credential_scan", Experiment: "concurrency",
		Path: "adapter.scan.json", SHA256: scanSHA, Bytes: scanInfo.Size()}
	if err := validatePublicationAdapterStderr(root, stderrFile, scanFile); err != nil {
		t.Fatalf("valid publication Adapter stderr: %v", err)
	}

	report.ExactValueSubstringHits = 1
	scanValue, _ = json.Marshal(report)
	if err := os.WriteFile(scanPath, append(scanValue, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePublicationAdapterStderr(root, stderrFile, scanFile); err == nil {
		t.Fatal("publication finalizer accepted a nonzero credential-gate hit")
	}
	report.ExactValueSubstringHits = 0
	scanValue, _ = json.Marshal(report)
	if err := os.WriteFile(scanPath, append(scanValue, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stderrPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validatePublicationAdapterStderr(root, stderrFile, scanFile); err == nil {
		t.Fatal("publication finalizer accepted non-private Adapter stderr")
	}
}

func TestPublicationFinalizerMaterialDispatchIsClosedAndHashBound(t *testing.T) {
	root := t.TempDir()
	qualification := filepath.Join(root, "qualification.json")
	identity := filepath.Join(root, "postgresql-identity.json")
	if err := os.WriteFile(qualification, []byte("qualification\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	qualificationSHA, _ := FileSHA256(qualification)
	identitySHA, _ := FileSHA256(identity)
	dispatch := []map[string]any{{
		"experiments": []string{"scale"}, "attestation_qualification_path": qualification,
		"attestation_qualification_sha256": qualificationSHA, "postgresql_identity_path": identity,
		"postgresql_identity_sha256": identitySHA,
	}}
	raw, _ := json.Marshal(dispatch)
	if err := validatePublicationFinalizerMaterial(raw, []string{"baseline", "scale"}); err != nil {
		t.Fatalf("valid Scale finalizer dispatch: %v", err)
	}
	if err := validatePublicationFinalizerMaterial(nil, []string{"baseline", "scale"}); err == nil {
		t.Fatal("missing Scale finalizer dispatch was accepted")
	}
	dispatch[0]["experiments"] = []string{"artifact"}
	raw, _ = json.Marshal(dispatch)
	if err := validatePublicationFinalizerMaterial(raw, []string{"baseline", "scale"}); err == nil {
		t.Fatal("misrouted Scale finalizer dispatch was accepted")
	}
	dispatch[0]["experiments"] = []string{"scale"}
	dispatch[0]["postgresql_identity_sha256"] = strings.Repeat("0", 64)
	raw, _ = json.Marshal(dispatch)
	if err := validatePublicationFinalizerMaterial(raw, []string{"baseline", "scale"}); err == nil {
		t.Fatal("changed PostgreSQL identity was accepted")
	}
	if err := validatePublicationFinalizerMaterial([]byte(`[]`), []string{"baseline"}); err == nil {
		t.Fatal("ordinary publication profile accepted an invented finalizer dispatch")
	}
}

func splitPublicationPlanFixture() finalv5profile.CampaignPlan {
	plan := finalv5profile.CampaignPlan{ContractRelease: "final-v5-contracts-v1.11"}
	for deployment := 0; deployment < 11; deployment++ {
		cells := 11
		if deployment < 4 {
			cells = 12
		}
		entry := finalv5profile.PlannedDeploy{Alias: fmt.Sprintf("profile-%02d", deployment), Ready: true}
		for cell := 0; cell < cells; cell++ {
			entry.Cells = append(entry.Cells, fmt.Sprintf("profile/%02d/%03d/mode", deployment, cell))
		}
		plan.Deployments = append(plan.Deployments, entry)
	}
	add := func(id, experiment string, count, processes, warmups, samples int) {
		profile := experiment
		entry := finalv5profile.PlannedNonProfileCampaign{ID: id, ExperimentID: experiment,
			ProtocolProfile: profile,
			ExecutionModel:  "deployment_free_process", FreshExecutions: 3, ProcessReplicates: processes,
			WarmupsPerCell: warmups, MeasuredSamplesPerCell: samples, ProfileBinding: "forbidden"}
		for index := 0; index < count; index++ {
			cell := fmt.Sprintf("%s/%s/%03d/mode", experiment, id, index)
			entry.Cells = append(entry.Cells, cell)
			plan.NonProfileCells = append(plan.NonProfileCells, cell)
		}
		plan.NonProfileCampaigns = append(plan.NonProfileCampaigns, entry)
	}
	add("scale-outcome-merkle", "scale", 36, 1, 5, 30)
	add("compiler", "compiler", 11, 5, 1, 100)
	return plan
}
