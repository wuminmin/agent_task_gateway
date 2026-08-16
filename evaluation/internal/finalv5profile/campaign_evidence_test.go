package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileCampaignEvidenceBindsTheFixedCommitAndEveryDeploymentFile(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	profile := PlannedDeploy{ProfileID: "profile-1111111111111111", Alias: "rls-bounded",
		CatalogPath: "config/profiles/rls-bounded.catalog.yaml", CatalogSHA256: strings.Repeat("b", 64),
		Experiments: []string{"rls"}, Cells: []string{"rls/workload/scale/bounded"}, Ready: true}
	plan := CampaignPlan{ContractRelease: "release", Deployments: []PlannedDeploy{profile}}
	files := []CampaignEvidenceFile{
		campaignFixtureFile(t, root, "config", "rls", "config.json", map[string]any{
			"schema_version": 1, "campaign_class": "pilot", "pilot_kind": "real_system",
			"campaign_id": "p30", "submission_commit": commit, "experiment_id": "rls", "deployments": 1,
			"warmups": 0, "samples": 1, "random_seed": 1, "fresh_root_per_sample": true,
			"workloads": []any{map[string]any{"id": "workload", "scales": []string{"scale"}, "modes": []string{"bounded"}}},
		}),
		campaignFixtureFile(t, root, "profile_binding", "", "profile-binding.json", map[string]any{
			"version": "taskgate-final-v5-profile-binding-v1", "profile_id": profile.ProfileID,
			"closure_sha256": strings.Repeat("c", 64), "catalog_sha256": profile.CatalogSHA256,
			"dataset_binding_sha256": strings.Repeat("d", 64), "publication_identity": strings.Repeat("e", 64),
		}),
		campaignFixtureFile(t, root, "activation_evidence", "", "activation.json", map[string]any{
			"record": ActivationEvidenceVersion, "status": "pass", "profile_id": profile.ProfileID,
			"catalog_sha256": profile.CatalogSHA256, "activation_smoke_passed": true,
		}),
		campaignFixtureFile(t, root, "environment", "", "environment.json", map[string]any{
			"git_commit": commit, "git_status": []string{}, "publication_eligible": false,
		}),
		campaignFixtureFile(t, root, "fresh_proof", "", "fresh.json", map[string]any{"schema_version": 1}),
		campaignFixtureFile(t, root, "gateway_image", "", "gateway-image.json", map[string]any{
			"record_kind": "taskgate-pilot-gateway-image-observation-v1", "experiment_class": "pilot",
			"provenance_assertion": "observation_only_not_publication_verification",
			"container_image_id":   "sha256:image", "image_id": "sha256:image",
		}),
		campaignFixtureFile(t, root, "cleanup", "", "cleanup.json", map[string]any{
			"status": "pass", "containers": 0, "volumes": 0, "networks": 0,
		}),
		campaignFixtureFile(t, root, "selected_cells", "rls", "cells.json", []string{profile.Cells[0]}),
		campaignFixtureFile(t, root, "raw_jsonl", "rls", "raw.jsonl", json.RawMessage(`{"campaign_id":"p30","campaign_class":"pilot","deployment_id":"deployment-01","experiment_id":"rls","cell_id":"workload/scale/bounded","status":"pass","publication_eligible":false,"profile_binding":{"profile_id":"profile-1111111111111111","catalog_sha256":"`+profile.CatalogSHA256+`"}}`)),
	}
	record := ProfileCampaignDeploymentRecord{SchemaVersion: 1, CampaignID: "p30", CampaignClass: "pilot",
		SubmissionCommit: commit, ComposeProject: "project", ProfileID: profile.ProfileID, ProfileAlias: profile.Alias,
		CatalogPath: profile.CatalogPath, CatalogSHA256: profile.CatalogSHA256, Repetition: 1,
		Cells: append([]string(nil), profile.Cells...), Files: files}
	recordPath := filepath.Join(root, "deployment-record.json")
	campaignWriteJSON(t, recordPath, record)
	manifest, err := MergeProfileCampaignEvidence(plan, strings.Repeat("f", 64), root, "p30", commit, 1,
		[]string{profile.Alias}, []string{recordPath})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "pass" || !manifest.CompleteMatrix || len(manifest.Deployments) != 1 ||
		manifest.PublicationEligible || manifest.FormalCampaign {
		t.Fatalf("merged manifest = %+v", manifest)
	}

	var mutated ProfileCampaignDeploymentRecord
	payload, _ := os.ReadFile(recordPath)
	if err := json.Unmarshal(payload, &mutated); err != nil {
		t.Fatal(err)
	}
	mutated.SubmissionCommit = strings.Repeat("9", 40)
	campaignWriteJSON(t, recordPath, mutated)
	if _, err := MergeProfileCampaignEvidence(plan, strings.Repeat("f", 64), root, "p30", commit, 1,
		[]string{profile.Alias}, []string{recordPath}); err == nil {
		t.Fatal("a per-deployment commit drift was merged")
	}
}

func campaignFixtureFile(t *testing.T, root, kind, experiment, name string, value any) CampaignEvidenceFile {
	t.Helper()
	path := filepath.Join(root, name)
	var payload []byte
	if raw, ok := value.(json.RawMessage); ok {
		payload = append(append([]byte(nil), raw...), '\n')
	} else {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		payload = append(encoded, '\n')
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return CampaignEvidenceFile{Kind: kind, Experiment: experiment, Path: name,
		SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(payload))}
}

func campaignWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
