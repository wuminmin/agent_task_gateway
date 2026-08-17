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
		campaignFixtureFile(t, root, "raw_jsonl", "rls", "raw.jsonl", campaignEnvelopeFixture(
			campaignSampleFixture(profile, "workload/scale/bounded", false), "pilot", true)),
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

func TestCampaignEvidenceJSONLAcceptsBareSample(t *testing.T) {
	profile := campaignJSONLTestProfile([]string{"rls/workload/scale/bounded"})
	sample := campaignSampleFixture(profile, "workload/scale/bounded", false)
	sample["campaign_class"] = "pilot"
	path := filepath.Join(t.TempDir(), "bare.jsonl")
	campaignWriteJSONL(t, path, sample)
	observed := map[string]bool{}
	if err := validateCampaignJSONL(path, "p30", "rls", profile, observed); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed cells = %d, want 1", len(observed))
	}
}

func TestCampaignEvidenceJSONLAcceptsMixedEnvelopeAndBareSamples(t *testing.T) {
	profile := campaignJSONLTestProfile([]string{
		"rls/workload/scale/bounded",
		"rls/workload-2/scale/bounded",
	})
	enveloped := campaignEnvelopeFixture(campaignSampleFixture(profile, "workload/scale/bounded", false), "pilot", true)
	bare := campaignSampleFixture(profile, "workload-2/scale/bounded", false)
	bare["campaign_class"] = "pilot"
	path := filepath.Join(t.TempDir(), "mixed.jsonl")
	campaignWriteJSONL(t, path, enveloped, bare)
	observed := map[string]bool{}
	if err := validateCampaignJSONL(path, "p30", "rls", profile, observed); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 {
		t.Fatalf("observed cells = %d, want 2", len(observed))
	}
}

func TestCampaignEvidenceJSONLRejectsMalformedEnvelopes(t *testing.T) {
	profile := campaignJSONLTestProfile([]string{"rls/workload/scale/bounded"})
	unknownField := campaignEnvelopeFixture(
		campaignSampleFixture(profile, "workload/scale/bounded", false), "pilot", true)
	unknownField["unexpected"] = true
	tests := []struct {
		name     string
		envelope map[string]any
	}{
		{name: "missing record", envelope: campaignEnvelopeFixture(
			campaignSampleFixture(profile, "workload/scale/bounded", false), "pilot", false)},
		{name: "wrong campaign class", envelope: campaignEnvelopeFixture(
			campaignSampleFixture(profile, "workload/scale/bounded", false), "publication", true)},
		{name: "publication eligible nested sample", envelope: campaignEnvelopeFixture(
			campaignSampleFixture(profile, "workload/scale/bounded", true), "pilot", true)},
		{name: "unknown envelope field", envelope: unknownField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.jsonl")
			campaignWriteJSONL(t, path, test.envelope)
			if err := validateCampaignJSONL(path, "p30", "rls", profile, map[string]bool{}); err == nil {
				t.Fatal("malformed profile campaign envelope was accepted")
			}
		})
	}
}

func TestCampaignEvidenceMergerUsesRetainedRQ5CoordinateMap(t *testing.T) {
	root := t.TempDir()
	sourceMap := filepath.Join("..", "..", "..", RQ5WorkloadCellMapSourcePath)
	payload, err := os.ReadFile(sourceMap)
	if err != nil {
		t.Fatal(err)
	}
	retainedMapPath := filepath.Join(root, "rq5-map.json")
	if err := os.WriteFile(retainedMapPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	mapping, mappingSHA, err := LoadRQ5WorkloadCellMap(retainedMapPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := PlannedDeploy{ProfileID: "profile-1111111111111111", CatalogSHA256: strings.Repeat("b", 64),
		Experiments: []string{"rq5"}, Cells: []string{
			"rq5/online-transition-v1/single/build",
			"rq5/online-transition-v1/single/retained",
		}}
	translation, err := NewRQ5CellTranslationEvidence(mapping, mappingSHA, "rq5-map.json", profile.Cells)
	if err != nil {
		t.Fatal(err)
	}
	files := []CampaignEvidenceFile{
		{Kind: "rq5_cell_map", Experiment: "rq5", Path: "rq5-map.json",
			SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(payload))},
		campaignFixtureFile(t, root, "selected_cells", "rq5", "campaign-cells.json", profile.Cells),
		campaignFixtureFile(t, root, "runner_selected_cells", "rq5", "runner-cells.json", translation.ExperimentCells),
		campaignFixtureFile(t, root, "rq5_cell_translation", "rq5", "translation.json", translation),
	}
	retained, err := validateRQ5CoordinateEvidence(root, profile, files)
	if err != nil {
		t.Fatal(err)
	}

	sample := map[string]any{
		"schema_version": 1, "campaign_id": "p31", "deployment_id": "deployment-01",
		"experiment_id": "rq5", "cell_id": "daily-publication-v5/345000/build_verify_activate",
		"status": "pass", "publication_eligible": false,
		"profile_binding": map[string]any{"profile_id": profile.ProfileID, "catalog_sha256": profile.CatalogSHA256},
	}
	rawPath := filepath.Join(root, "rq5.jsonl")
	campaignWriteJSONL(t, rawPath, campaignEnvelopeFixture(sample, "pilot", true))
	observed := map[string]bool{}
	if err := validateCampaignJSONL(rawPath, "p31", "rq5", profile, observed, &retained); err != nil {
		t.Fatal(err)
	}
	if !observed["rq5/online-transition-v1/single/build"] {
		t.Fatalf("observed campaign coordinates = %v", observed)
	}

	sample["cell_id"] = "daily-publication-v5/345000/unknown"
	campaignWriteJSONL(t, filepath.Join(root, "rq5-unknown.jsonl"), campaignEnvelopeFixture(sample, "pilot", true))
	if err := validateCampaignJSONL(filepath.Join(root, "rq5-unknown.jsonl"), "p31", "rq5", profile,
		map[string]bool{}, &retained); err == nil {
		t.Fatal("evidence merger accepted an unknown RQ5 experiment coordinate")
	}
}

func campaignJSONLTestProfile(cells []string) PlannedDeploy {
	return PlannedDeploy{
		ProfileID:     "profile-1111111111111111",
		CatalogSHA256: strings.Repeat("b", 64),
		Cells:         cells,
	}
}

func campaignSampleFixture(profile PlannedDeploy, cellID string, publicationEligible bool) map[string]any {
	return map[string]any{
		"schema_version":       1,
		"campaign_id":          "p30",
		"deployment_id":        "deployment-01",
		"experiment_id":        "rls",
		"cell_id":              cellID,
		"sample_id":            "deployment-01-p01-sample-0001",
		"iteration":            1,
		"process_replicate":    1,
		"status":               "pass",
		"publication_eligible": publicationEligible,
		"profile_binding": map[string]any{
			"version":        "taskgate-final-v5-profile-binding-v1",
			"profile_id":     profile.ProfileID,
			"catalog_sha256": profile.CatalogSHA256,
		},
		"rls_verification":       map[string]any{"version": "taskgate-final-v5-rls-evidence-v1"},
		"taskgate_acceptance_v3": nil,
		"taskgate_rejection_v1":  nil,
	}
}

func campaignEnvelopeFixture(sample map[string]any, campaignClass string, includeRecord bool) map[string]any {
	envelope := map[string]any{
		"schema_version": 1,
		"campaign_class": campaignClass,
		"sample":         sample,
	}
	if includeRecord {
		envelope["record"] = profileCampaignSampleV1Record
	}
	return envelope
}

func campaignWriteJSONL(t *testing.T, path string, values ...any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
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
