package experiment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
)

func TestProfileCampaignRunnerWritesVersionedPilotStamp(t *testing.T) {
	config := Config{
		SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "real_system",
		CampaignID: "profile-campaign-stamp", ExperimentID: "baseline",
		Deployments: 1, Samples: 1, RandomSeed: 20260817, FreshRootPerSample: true,
		Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"novel"}}},
	}
	t.Setenv("TASKGATE_TEST_ADAPTER", "1")
	t.Setenv("TASKGATE_EXPERIMENT_CLASS", "pilot")
	t.Setenv("TASKGATE_CAMPAIGN_ID", config.CampaignID)
	path := filepath.Join(t.TempDir(), "profile-campaign.jsonl")
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], path); err != nil {
		t.Fatal(err)
	}
	records, err := ReadProfileCampaignSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].CampaignClass != "pilot" ||
		records[0].Sample.SchemaVersion != SampleSchemaVersion {
		t.Fatalf("runner did not write the versioned pilot envelope: %#v", records)
	}
}

func TestProfileCampaignGateAcceptsAllEightExperimentShapes(t *testing.T) {
	tests := map[string][]Sample{
		"baseline": {
			profileGateSample("baseline", "S1/SF1/direct", "S1", "SF1", "direct", "postgresql"),
			profileGateBaselineTaskGateSample(),
		},
		"artifact":    {profileGateArtifactSample()},
		"scale":       {profileGateScaleSample()},
		"provsql":     profileGateProvSQLSamples(),
		"rls":         profileGateRLSSamples(),
		"attack":      profileGateAttackSamples(t),
		"concurrency": {profileGateConcurrencySample()},
		"rq5":         profileGateRQ5Samples(),
	}
	for _, experimentID := range []string{"baseline", "artifact", "scale", "provsql", "rls", "attack", "concurrency", "rq5"} {
		t.Run(experimentID, func(t *testing.T) {
			samples := tests[experimentID]
			records := WrapRetainedSamplesForProfileCampaignAudit(samples, "pilot")
			selected := make([]string, len(samples))
			for index := range samples {
				selected[index] = samples[index].ExperimentID + "/" + samples[index].CellID
			}
			if err := ValidateProfileCampaignExperimentGate(experimentID, selected, records, 1); err != nil {
				t.Fatal(err)
			}
			t.Logf("P30-LG-STAGE: offline_gate_%s=pass n=%d source=constructed_real_shape", experimentID, len(records))
		})
	}
}

func TestProfileCampaignGateRejectsCrossExperimentTerminalDefects(t *testing.T) {
	honest := profileGateArtifactSample()
	selected := []string{"artifact/" + honest.CellID}

	t.Run("wrong cell set", func(t *testing.T) {
		records := WrapRetainedSamplesForProfileCampaignAudit([]Sample{honest}, "pilot")
		if err := ValidateProfileCampaignExperimentGate("artifact", []string{"artifact/result-heavy/10k-x4/novel"}, records, 1); err == nil {
			t.Fatal("gate accepted a cell outside the assigned set")
		}
	})
	t.Run("wrong class", func(t *testing.T) {
		record := NewProfileCampaignSampleV1("publication", honest)
		if err := ValidateProfileCampaignExperimentGate("artifact", selected, []ProfileCampaignSampleV1{record}, 1); err == nil {
			t.Fatal("gate accepted a non-pilot runner stamp")
		}
	})
	t.Run("fake pass", func(t *testing.T) {
		fake := honest
		fake.TaskGateAcceptanceV3 = nil
		records := WrapRetainedSamplesForProfileCampaignAudit([]Sample{fake}, "pilot")
		if err := ValidateProfileCampaignExperimentGate("artifact", selected, records, 1); err == nil {
			t.Fatal("gate accepted an Artifact pass without finalizer acceptance")
		}
	})
	t.Run("unexpected rejection", func(t *testing.T) {
		rejected := honest
		rejected.SchemaVersion = TaskGateRejectionSampleSchemaVersion
		rejected.Status = "fail"
		rejected.TaskGateAcceptanceV3 = nil
		rejected.TaskGateRejectionV1 = &TaskGateRejectionV1{}
		records := WrapRetainedSamplesForProfileCampaignAudit([]Sample{rejected}, "pilot")
		if err := ValidateProfileCampaignExperimentGate("artifact", selected, records, 1); err == nil {
			t.Fatal("gate accepted a finalizer-rejected cell")
		}
	})
}

func TestProfileCampaignGateDispatchesAttackExpectedRejectionsInsideEvidence(t *testing.T) {
	sample := profileGateAttackSamples(t)[1]
	selected := []string{"attack/" + sample.CellID}
	records := WrapRetainedSamplesForProfileCampaignAudit([]Sample{sample}, "pilot")
	if err := ValidateProfileCampaignExperimentGate("attack", selected, records, 1); err != nil {
		t.Fatal(err)
	}

	mutated := sample
	evidence := *sample.AttackVerification
	evidence.Steps = append([]AttackStepEvidence(nil), evidence.Steps...)
	mutated.AttackVerification = &evidence
	for index := range evidence.Steps {
		if evidence.Steps[index].Rejected {
			evidence.Steps[index].Rejected = false
			evidence.Steps[index].Accepted = true
			break
		}
	}
	if err := ValidateProfileCampaignExperimentGate("attack", selected,
		WrapRetainedSamplesForProfileCampaignAudit([]Sample{mutated}, "pilot"), 1); err == nil {
		t.Fatal("gate accepted an Attack TaskGate arm that converted an expected rejection into acceptance")
	}
}

func TestProfileCampaignWriterKeepsNestedSampleSchemaAndReadCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	w, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	sample := profileGateBaselineTaskGateSample()
	if err := w.WriteProfileCampaignSample("pilot", sample); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadProfileCampaignSamples(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Sample.SchemaVersion != SampleSchemaVersion || records[0].CampaignClass != "pilot" {
		t.Fatalf("profile campaign envelope changed the nested schema: %#v", records)
	}
	legacyReader, err := ReadSamples([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyReader) != 1 || legacyReader[0].SchemaVersion != SampleSchemaVersion || legacyReader[0].CellID != sample.CellID {
		t.Fatal("ReadSamples did not preserve compatibility with the explicit campaign envelope")
	}
}

func profileGateSample(experimentID, cellID, workloadID, scale, mode, system string) Sample {
	sample := validTestSample()
	sample.ExperimentID = experimentID
	sample.CellID = cellID
	sample.WorkloadID = workloadID
	sample.Scale = scale
	sample.Mode = mode
	sample.System = system
	sample.ResultSHA256 = strings.Repeat("a", 64)
	sample.PhysicalSQLSHA256 = strings.Repeat("b", 64)
	sample.LogicalSQLSHA256 = strings.Repeat("c", 64)
	sample.QueryPlanSHA256 = strings.Repeat("d", 64)
	return sample
}

func profileGateBaselineTaskGateSample() Sample {
	sample := profileGateSample("baseline", "S1/SF1/novel", "S1", "SF1", "novel", "taskgate")
	sample.BaselineVerification = &BaselineVerificationEvidence{}
	return sample
}

func profileGateArtifactSample() Sample {
	sample := profileGateSample("artifact", "result-heavy/100x4/novel", "result-heavy", "100x4", "novel", "taskgate")
	sample.SchemaVersion = FinalizedSampleSchemaVersion
	sample.TaskGateAcceptanceV3 = &FinalizationV3{}
	sample.ArtifactVerification = &ArtifactVerificationEvidence{Version: artifactEvidenceVersionV2}
	return sample
}

func profileGateScaleSample() Sample {
	sample := profileGateSample("scale", "dependency-e2e/10k-overlap-0/novel", "dependency-e2e", "10k-overlap-0", "novel", "taskgate")
	sample.SchemaVersion = FinalizedSampleSchemaVersion
	sample.TaskGateAcceptanceV3 = &FinalizationV3{}
	sample.ScaleVerification = &ScaleVerificationEvidence{Version: scaleDependencyEvidenceVersionV2}
	return sample
}

func profileGateProvSQLSamples() []Sample {
	result := make([]Sample, 0, 3)
	for _, value := range []struct{ mode, system string }{{"direct", "postgresql"}, {"provsql", "provsql"}, {"taskgate", "taskgate"}} {
		sample := profileGateSample("provsql", "nonce-join-group/1k/"+value.mode, "nonce-join-group", "1k", value.mode, value.system)
		sample.ProvSQLVerification = &ProvSQLVerificationEvidence{}
		if value.mode == "taskgate" {
			sample.TaskGateAcceptanceV3 = &FinalizationV3{}
		}
		result = append(result, sample)
	}
	return result
}

func profileGateRLSSamples() []Sample {
	direct := profileGateSample("rls", "policy-denied-control/single/rls", "policy-denied-control", "single", "rls", "postgresql")
	direct.RLSVerification = &RLSVerificationEvidence{}
	bounded := profileGateSample("rls", "policy-denied-control/single/bounded", "policy-denied-control", "single", "bounded", "taskgate")
	bounded.RLSVerification = &RLSVerificationEvidence{}
	return []Sample{direct, bounded}
}

func profileGateAttackSamples(t *testing.T) []Sample {
	t.Helper()
	manifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, found := manifest.Lookup("B-equivalent-sql", "variants-v1")
	if !found {
		t.Fatal("frozen Attack B case is absent")
	}
	build := func(mode, system string) Sample {
		sample := profileGateSample("attack", "B-equivalent-sql/variants-v1/"+mode,
			"B-equivalent-sql", "variants-v1", mode, system)
		evidence := &AttackVerificationEvidence{Steps: make([]AttackStepEvidence, len(attackCase.Steps))}
		for index, expected := range attackCase.Steps {
			rejected := system == "taskgate" && expected.Classification == "expected_rejection"
			evidence.Steps[index] = AttackStepEvidence{Accepted: !rejected, Rejected: rejected}
		}
		sample.AttackVerification = evidence
		return sample
	}
	return []Sample{build("direct", "postgresql"), build("novel", "taskgate")}
}

func profileGateConcurrencySample() Sample {
	sample := profileGateSample("concurrency", "serial-control/1/serial", "serial-control", "1", "serial", "taskgate")
	sample.ConcurrencyVerification = &ConcurrencyVerification{}
	return sample
}

func profileGateRQ5Samples() []Sample {
	result := make([]Sample, 0, 2)
	for _, mode := range []string{"build", "retained"} {
		sample := profileGateSample("rq5", "online-transition-v1/single/"+mode,
			"online-transition-v1", "single", mode, "taskgate")
		sample.RQ5Verification = &RQ5VerificationEvidence{}
		result = append(result, sample)
	}
	return result
}
