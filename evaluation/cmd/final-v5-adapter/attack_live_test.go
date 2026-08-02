package main

import (
	"context"
	"os"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// TestAttackAdapterLivePreflight runs every frozen Attack arm through the real
// Business PostgreSQL, OA, TaskGate, Control, MinIO, V8 verifier, and Parquet
// path. Ordinary unit runs skip it; the Stage-B gate enables it only on a fresh
// Compose topology with the documented TASKGATE_FINAL_V5_* environment.
func TestAttackAdapterLivePreflight(t *testing.T) {
	if os.Getenv("TASKGATE_FINAL_V5_ATTACK_LIVE") != "1" {
		t.Skip("TASKGATE_FINAL_V5_ATTACK_LIVE=1 is required")
	}
	adapter, err := newAttackAdapter(context.Background())
	if err != nil {
		t.Fatalf("initialize real Attack adapter: %v", err)
	}
	defer adapter.Close()

	for caseIndex, attackCase := range adapter.corpus.Cases {
		modes := []string{"direct", "novel"}
		if attackCase.WorkloadID == "C-request-id" {
			modes = []string{"novel", "semantic_replay", "idempotent_replay"}
		}
		for modeIndex, mode := range modes {
			operation := experiment.AdapterOperation{
				SchemaVersion: 1, CampaignClass: "pilot", ProcessReplicate: 1,
				CampaignID: "stage-b-attack-live", DeploymentID: "stage-b-attack-live-deployment",
				ExperimentID: "attack", CellID: attackCase.WorkloadID + "/" + attackCase.Scale + "/" + mode,
				SampleID:  "stage-b-attack-live-" + attackCase.WorkloadID + "-" + attackCase.Scale + "-" + mode,
				Iteration: 1, OrderPosition: caseIndex*3 + modeIndex + 1, RandomSeed: 20260802,
				PairID:            "stage-b-attack-pair-" + attackCase.WorkloadID + "-" + attackCase.Scale,
				PairedSystemOrder: "direct,novel", RootGroupID: "stage-b-attack-root-" + attackCase.WorkloadID + "-" + attackCase.Scale,
				Mode: mode, WorkloadID: attackCase.WorkloadID, Scale: attackCase.Scale,
			}
			sample := adapter.Execute(context.Background(), operation)
			if sample.Status != "pass" {
				if sample.ErrorCode == "attack_invariant_violation" {
					retained := sample
					retained.Status, retained.ErrorCode, retained.Reason = "pass", "", ""
					if evidence := sample.AttackVerification; evidence != nil && len(evidence.Steps) > 0 {
						last := evidence.Steps[len(evidence.Steps)-1]
						beforeOutcome, afterOutcome := int64(-1), int64(-1)
						if last.Before != nil {
							beforeOutcome = last.Before.Root.OutcomeCardinality
						}
						if last.After != nil {
							afterOutcome = last.After.Root.OutcomeCardinality
						}
						t.Fatalf("real Attack %s/%s/%s retained evidence invariant: %v; steps=%d last=%s accepted=%t rejected=%t code=%s root_outcome=%d/%d",
							attackCase.WorkloadID, attackCase.Scale, mode, experiment.ValidateAttackEvidence(retained),
							len(evidence.Steps), last.VariantID, last.Accepted, last.Rejected, last.ObservedErrorCode,
							beforeOutcome, afterOutcome)
					}
					t.Fatalf("real Attack %s/%s/%s retained evidence invariant: %v", attackCase.WorkloadID,
						attackCase.Scale, mode, experiment.ValidateAttackEvidence(retained))
				}
				if evidence := sample.AttackVerification; evidence != nil && len(evidence.Steps) > 0 {
					last := evidence.Steps[len(evidence.Steps)-1]
					beforeOutcome, afterOutcome := int64(-1), int64(-1)
					if last.Before != nil {
						beforeOutcome = last.Before.Root.OutcomeCardinality
					}
					if last.After != nil {
						afterOutcome = last.After.Root.OutcomeCardinality
					}
					t.Fatalf("real Attack %s/%s/%s = %s/%s: %s; steps=%d last=%s accepted=%t rejected=%t code=%s root_outcome=%d/%d",
						attackCase.WorkloadID, attackCase.Scale, mode, sample.Status, sample.ErrorCode, sample.Reason,
						len(evidence.Steps), last.VariantID, last.Accepted, last.Rejected, last.ObservedErrorCode,
						beforeOutcome, afterOutcome)
				}
				t.Fatalf("real Attack %s/%s/%s = %s/%s: %s", attackCase.WorkloadID, attackCase.Scale, mode,
					sample.Status, sample.ErrorCode, sample.Reason)
			}
			if err := experiment.ValidateAttackEvidence(sample); err != nil {
				t.Fatalf("real Attack %s/%s/%s evidence: %v", attackCase.WorkloadID, attackCase.Scale, mode, err)
			}
			if err := sample.Validate(); err != nil {
				t.Fatalf("real Attack %s/%s/%s runner sample contract: %v", attackCase.WorkloadID, attackCase.Scale, mode, err)
			}
			if attackCase.WorkloadID == "E-threshold" && mode == "novel" {
				steps := sample.AttackVerification.Steps
				terminal := steps[len(steps)-1]
				if !terminal.Rejected || terminal.ObservedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" ||
					terminal.ObservedErrorReason != "ROOT_OUTCOME_CEILING_EXCEEDED" ||
					terminal.Before.Root.OutcomeCardinality != 5 || terminal.After.Root.OutcomeCardinality != 5 ||
					terminal.Before.UsedQueries != 1 || terminal.After.UsedQueries != 2 ||
					terminal.Before.TaskIDHash == terminal.Before.RootTaskIDHash {
					t.Fatalf("E terminal child/root/query-budget proof differs: %+v", terminal)
				}
			}
		}
	}
}
