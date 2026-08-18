package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

func TestProfileCampaignEvidenceBindsTheFixedCommitAndEveryDeploymentFile(t *testing.T) {
	root := t.TempDir()
	commit := strings.Repeat("a", 40)
	profile := PlannedDeploy{ProfileID: "profile-1111111111111111", Alias: "rls-bounded",
		CatalogPath: "config/profiles/rls-bounded.catalog.yaml", CatalogSHA256: strings.Repeat("b", 64),
		Experiments: []string{"rls"}, Cells: []string{"rls/workload/scale/bounded"}, Ready: true}
	plan := CampaignPlan{ContractRelease: "release", Deployments: []PlannedDeploy{profile}}
	if err := os.Mkdir(filepath.Join(root, "source"), 0o700); err != nil {
		t.Fatal(err)
	}
	overridesPayload := []byte("{\"schema_version\":1,\"record\":\"taskgate-profile-deployment-overrides-v1\",\"profiles\":{\"concurrency-expense-detail\":{\"environment\":{\"GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE\":10,\"GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE\":512,\"GATEWAY_CONNECTOR_MAX_CONNECTIONS\":32,\"GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS\":32}}}}\n")
	overridesPath := filepath.Join(root, "source", "deployment-overrides-v1.json")
	if err := os.WriteFile(overridesPath, overridesPayload, 0o600); err != nil {
		t.Fatal(err)
	}
	overridesDigest := sha256.Sum256(overridesPayload)
	rawFile := campaignFixtureFile(t, root, "raw_jsonl", "rls", "raw.jsonl", campaignEnvelopeFixture(
		campaignSampleFixture(profile, "workload/scale/bounded", false), "pilot", true))
	imageID := "sha256:" + strings.Repeat("1", 64)
	contextSHA := strings.Repeat("2", 64)
	sourceSHA := strings.Repeat("3", 64)
	datasetSHA := strings.Repeat("d", 64)
	registrySHA := strings.Repeat("4", 64)
	builderBase := "golang@sha256:" + strings.Repeat("5", 64)
	runtimeBase := "debian@sha256:" + strings.Repeat("6", 64)
	formalManifest := campaignFixtureFile(t, root, "formal_gateway_build_manifest", "", "formal-build.json", map[string]any{
		"schema_version": 1, "submission_commit": commit, "clean_tree_at_build": true,
		"build_context_sha256": contextSHA, "source_manifest_sha256": sourceSHA, "image_id": imageID,
		"image_tag": "taskgate:test", "platform": "linux/amd64", "build_target": "gateway",
		"builder_base_image": builderBase, "runtime_base_image": runtimeBase,
		"dataset_binding_sha256": datasetSHA, "profile_registry_sha256": registrySHA,
	})
	formalRuntime := campaignFixtureFile(t, root, "formal_gateway_runtime", "", "formal-runtime.json", map[string]any{
		"version": "taskgate-gateway-runtime-identity-v1", "submission_commit": commit, "clean_tree_at_build": true,
		"build_context_sha256": contextSHA, "source_manifest_sha256": sourceSHA, "build_target": "gateway",
		"local_image_id": imageID, "container_image_id": imageID, "builder_base_image": builderBase,
		"runtime_base_image": runtimeBase, "aggregate_sha256": strings.Repeat("7", 64),
	})
	formalOverride := campaignFixtureRawFile(t, root, "formal_gateway_compose_override", "", "formal-override.yaml",
		[]byte(fmt.Sprintf("services:\n  gateway:\n    image: %q\n    pull_policy: never\n    build: !reset null\n", imageID)))
	formalLog := campaignFixtureRawFile(t, root, "formal_gateway_build_log", "", "formal-build.log", []byte("formal build: pass\n"))
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
			"dataset_binding_sha256": datasetSHA, "publication_identity": strings.Repeat("e", 64),
		}),
		campaignFixtureFile(t, root, "activation_evidence", "", "activation.json", map[string]any{
			"record": ActivationEvidenceVersion, "status": "pass", "profile_id": profile.ProfileID,
			"catalog_sha256": profile.CatalogSHA256, "activation_smoke_passed": true,
		}),
		campaignFixtureFile(t, root, "environment", "", "environment.json", map[string]any{
			"git_commit": commit, "git_status": []string{}, "publication_eligible": false,
		}),
		campaignFixtureFile(t, root, "fresh_proof", "", "fresh.json", map[string]any{
			"schema_version": 1, "formal_gateway_built": true,
			"formal_gateway": map[string]any{
				"image_id": imageID, "build_manifest_path": filepath.Join(root, formalManifest.Path),
				"build_manifest_sha256":   formalManifest.SHA256,
				"compose_override_path":   filepath.Join(root, formalOverride.Path),
				"compose_override_sha256": formalOverride.SHA256,
				"runtime_identity_path":   filepath.Join(root, formalRuntime.Path),
				"runtime_identity_sha256": formalRuntime.SHA256,
				"build_context_sha256":    contextSHA, "source_manifest_sha256": sourceSHA,
				"dataset_binding_sha256": datasetSHA, "profile_registry_sha256": registrySHA,
			},
		}),
		campaignFixtureFile(t, root, "gateway_image", "", "gateway-image.json", map[string]any{
			"record_kind": "taskgate-pilot-gateway-image-observation-v1", "experiment_class": "pilot",
			"provenance_assertion": "observation_only_not_publication_verification",
			"formal_gateway_built": true, "formal_build_label": "v1",
			"container_image_id": imageID, "image_id": imageID,
		}),
		formalRuntime,
		formalManifest,
		formalOverride,
		formalLog,
		campaignFixtureFile(t, root, "cleanup", "", "cleanup.json", map[string]any{
			"status": "pass", "containers": 0, "volumes": 0, "networks": 0,
		}),
		campaignFixtureFile(t, root, "deployment_configuration", "", "deployment-configuration.json", ProfileDeploymentConfig{
			SchemaVersion: 1, Record: ProfileDeploymentConfigVersion,
			SourcePath: "source/deployment-overrides-v1.json", SourceSHA256: hex.EncodeToString(overridesDigest[:]),
			ProfileAlias: profile.Alias, Environment: map[string]int64{},
		}),
		campaignFixtureFile(t, root, "selected_cells", "rls", "cells.json", []string{profile.Cells[0]}),
		rawFile,
		campaignFixtureFile(t, root, "launcher_gate", "rls", "rls.gate.json", map[string]any{
			"schema_version": 1, "status": "pass", "experiment_id": "rls", "campaign_class": "pilot",
			"samples": 1, "selected_cells": 1, "input_sha256": rawFile.SHA256,
		}),
		campaignFixtureRawFile(t, root, "adapter_stderr", "rls", "rls.stderr.log", nil),
		campaignFixtureFile(t, root, "adapter_stderr_credential_scan", "rls", "rls.stderr-scan.json",
			finalv5publication.AdapterStderrCredentialScan{SchemaVersion: 1,
				Record: finalv5publication.AdapterStderrCredentialScanVersion, Status: "pass",
				InputSHA256: hex.EncodeToString(sha256.New().Sum(nil)), InputBytes: 0, SensitiveValuesChecked: 1}),
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

func TestPreregisteredAggregateRequiresAllRoundsAndAtLeastOnePass(t *testing.T) {
	root := t.TempDir()
	contract, digest, err := concurrencyfixture.LoadPreregistration(
		filepath.Join("..", "..", "..", concurrencyfixture.PreregistrationSourcePath))
	if err != nil {
		t.Fatal(err)
	}
	plan := CampaignPlan{}
	plan.Deployments = []PlannedDeploy{{Alias: contract.ProfileAlias, Cells: []string{contract.Cell}}}
	if err := plan.AttachPreregistration(contract, digest); err != nil {
		t.Fatal(err)
	}
	aggregate := plan.PreregisteredAggregates[0]
	caseIndex := 0
	makeRecords := func(passAt int, duplicateRound bool) []ProfileCampaignDeploymentRecord {
		caseIndex++
		caseDir := filepath.Join(root, fmt.Sprintf("case-%d", caseIndex))
		if err := os.Mkdir(caseDir, 0o700); err != nil {
			t.Fatal(err)
		}
		records := make([]ProfileCampaignDeploymentRecord, contract.Rounds)
		for repetition := 1; repetition <= contract.Rounds; repetition++ {
			status, code := contract.MissStatus, contract.MissErrorCode
			if repetition == passAt {
				status, code = contract.SuccessStatus, ""
			}
			roundLabel := repetition
			if duplicateRound && repetition == contract.Rounds {
				roundLabel = 1
			}
			path := filepath.Join(caseDir, "round-"+strconv.Itoa(repetition)+".jsonl")
			sample := map[string]any{
				"schema_version": 1, "campaign_id": "p43", "deployment_id": "deployment-01",
				"experiment_id": "concurrency", "cell_id": "shared-root/50/natural_contention",
				"sample_id": fmt.Sprintf("deployment-01-r%03d-sample", repetition),
				"iteration": 1, "process_replicate": 1, "status": status, "error_code": code,
				"publication_eligible": false,
				"concurrency_verification": map[string]any{
					"fixture_sha256": concurrencyfixture.FixtureSHA256(),
					"round_sha256":   digestForCampaignTest("round-" + strconv.Itoa(roundLabel)),
				},
			}
			campaignWriteJSONL(t, path, campaignEnvelopeFixture(sample, "pilot", true))
			payload, _ := os.ReadFile(path)
			rawDigest := sha256.Sum256(payload)
			records[repetition-1] = ProfileCampaignDeploymentRecord{
				ProfileAlias: contract.ProfileAlias, Repetition: repetition,
				Files: []CampaignEvidenceFile{{Kind: "raw_jsonl", Experiment: "concurrency",
					Path:   filepath.ToSlash(filepath.Join(filepath.Base(caseDir), filepath.Base(path))),
					SHA256: hex.EncodeToString(rawDigest[:]), Bytes: int64(len(payload))}},
			}
		}
		return records
	}

	selected := map[string]bool{contract.ProfileAlias: true}
	evidence, err := validatePreregisteredAggregates(root, "p43", []CampaignPreregisteredAggregate{aggregate},
		selected, makeRecords(contract.Rounds, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].RoundsRetained != 11 || evidence[0].ObservedPasses != 1 ||
		evidence[0].ObservedMisses != 10 || evidence[0].Status != "pass" {
		t.Fatalf("aggregate evidence = %+v", evidence)
	}
	allMiss, err := validatePreregisteredAggregates(root, "p43", []CampaignPreregisteredAggregate{aggregate},
		selected, makeRecords(0, false))
	if err != nil || len(allMiss) != 1 || allMiss[0].Status != "invalid" ||
		allMiss[0].ErrorCode != concurrencyfixture.PreregisteredMissCode ||
		allMiss[0].Reason != "offered concurrency not observed in 11 preregistered rounds" {
		t.Fatalf("all-miss aggregate = %+v, err=%v", allMiss, err)
	}
	if _, err := validatePreregisteredAggregates(root, "p43", []CampaignPreregisteredAggregate{aggregate},
		selected, makeRecords(contract.Rounds, true)); err == nil {
		t.Fatal("duplicate fresh-deployment round identity was accepted")
	}
}

func digestForCampaignTest(label string) string {
	digest := sha256.Sum256([]byte(label))
	return hex.EncodeToString(digest[:])
}

func TestProfileCampaignCleanupValidatesEveryRQ5ResourceFamily(t *testing.T) {
	root := t.TempDir()
	cleanupPath := "cleanup.json"
	cleanup := map[string]any{
		"status": "pass", "containers": 0, "volumes": 0, "networks": 0,
		"rq5": map[string]any{
			"status": "pass",
			"residual": map[string]any{"containers": 0, "volumes": 0,
				"project_networks": 0, "external_networks": 0},
		},
	}
	file := campaignFixtureFile(t, root, "cleanup", "", cleanupPath, cleanup)
	if err := validateCleanup(root, []CampaignEvidenceFile{file}); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"containers", "volumes", "project_networks", "external_networks"} {
		t.Run(family, func(t *testing.T) {
			mutated := map[string]any{
				"status": "pass", "containers": 0, "volumes": 0, "networks": 0,
				"rq5": map[string]any{
					"status": "pass",
					"residual": map[string]any{"containers": 0, "volumes": 0,
						"project_networks": 0, "external_networks": 0},
				},
			}
			mutated["rq5"].(map[string]any)["residual"].(map[string]any)[family] = 1
			campaignWriteJSON(t, filepath.Join(root, cleanupPath), mutated)
			if err := validateCleanup(root, []CampaignEvidenceFile{file}); err == nil {
				t.Fatalf("RQ5 %s residue passed cleanup validation", family)
			}
		})
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
	if err := validateCampaignJSONL(rawPath, "p31", "rq5", profile, observed,
		rq5CampaignJSONLOptions{Mapping: &retained, CatalogSHA256: profile.CatalogSHA256}); err != nil {
		t.Fatal(err)
	}
	if !observed["rq5/online-transition-v1/single/build"] {
		t.Fatalf("observed campaign coordinates = %v", observed)
	}

	sample["cell_id"] = "daily-publication-v5/345000/unknown"
	campaignWriteJSONL(t, filepath.Join(root, "rq5-unknown.jsonl"), campaignEnvelopeFixture(sample, "pilot", true))
	if err := validateCampaignJSONL(filepath.Join(root, "rq5-unknown.jsonl"), "p31", "rq5", profile,
		map[string]bool{}, rq5CampaignJSONLOptions{Mapping: &retained, CatalogSHA256: profile.CatalogSHA256}); err == nil {
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

func campaignFixtureRawFile(t *testing.T, root, kind, experiment, name string, payload []byte) CampaignEvidenceFile {
	t.Helper()
	path := filepath.Join(root, name)
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
