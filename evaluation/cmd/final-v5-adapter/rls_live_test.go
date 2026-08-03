package main

import (
	"context"
	"os"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// TestRLSAdapterLivePreflight runs the complete frozen RLS matrix through the
// real PostgreSQL policy/role plus OA, TaskGate, Control, MinIO, V8 verifier,
// and Parquet path. Ordinary unit runs skip it. The Stage-B gate enables it
// only against a fresh Compose topology with the documented private runtime
// environment and a final digest-consistent Catalog.
func TestRLSAdapterLivePreflight(t *testing.T) {
	if os.Getenv("TASKGATE_FINAL_V5_RLS_LIVE") != "1" {
		t.Skip("TASKGATE_FINAL_V5_RLS_LIVE=1 is required")
	}
	ctx := context.Background()
	adapter, err := newRLSAdapter(ctx)
	if err != nil {
		t.Fatalf("initialize real RLS adapter: %v", err)
	}
	defer adapter.Close()

	type workload struct{ id, scale string }
	workloads := []workload{
		{id: "adaptive-100-v1", scale: "100-queries"},
		{id: "policy-denied-control", scale: "single"},
	}
	position := 0
	for _, workload := range workloads {
		for _, mode := range []string{"rls", "unlimited", "bounded"} {
			position++
			operation := experiment.AdapterOperation{
				SchemaVersion: 1, CampaignClass: "pilot", CampaignID: "stage-b-rls-live",
				DeploymentID: "stage-b-rls-live-deployment", ExperimentID: "rls",
				CellID:    workload.id + "/" + workload.scale + "/" + mode,
				SampleID:  "stage-b-rls-live-" + workload.id + "-" + mode,
				Iteration: 1, ProcessReplicate: 1, OrderPosition: position, RandomSeed: 20260802,
				PairID: "stage-b-rls-pair-" + workload.id, PairedSystemOrder: "rls,unlimited,bounded",
				FreshRootRequired: mode != "rls", RootGroupID: mode,
				WorkloadID: workload.id, Scale: workload.scale, Mode: mode,
			}
			sample := adapter.Execute(ctx, operation)
			if sample.Status != "pass" {
				if sample.ErrorCode == "rls_invariant_violation" {
					if workload.id == "policy-denied-control" && mode == "rls" && sample.RLSVerification == nil {
						_, probeErr := adapter.runDirectPolicyFilter(ctx, operation)
						t.Fatalf("real RLS %s/%s failed before retaining evidence; direct probe: %v", workload.id, mode, probeErr)
					}
					retained := sample
					retained.Status, retained.ErrorCode, retained.Reason = "pass", "", ""
					if evidence := sample.RLSVerification; evidence != nil {
						for index, step := range evidence.Steps {
							if step.Before == nil || step.After == nil {
								continue
							}
							wantReplay := false
							if index > 0 {
								previous := evidence.Steps[index-1].OraclePrefix
								wantReplay = step.OraclePrefix.Release == previous.Release &&
									step.OraclePrefix.Dependency == previous.Dependency && step.OraclePrefix.Outcome == previous.Outcome
							}
							cardinalityMatches := step.After.Root.ReleaseCardinality == step.OraclePrefix.Release.Cardinality &&
								step.After.Root.DependencyCardinality == step.OraclePrefix.Dependency.Cardinality &&
								step.After.Root.OutcomeCardinality == step.OraclePrefix.Outcome.Cardinality
							chargedMatches := step.ChargedReleaseFacts == step.After.Root.ReleaseCardinality-step.Before.Root.ReleaseCardinality &&
								step.ChargedDependencyFacts == step.After.Root.DependencyCardinality-step.Before.Root.DependencyCardinality &&
								step.ChargedOutcomeFacts == step.After.Root.OutcomeCardinality-step.Before.Root.OutcomeCardinality
							dimensionDigestMatches := func(charged, beforeCardinality, afterCardinality int64, beforeSet, afterSet, observationSet string) bool {
								if step.Before.Root.Epoch == 0 && charged == 0 && beforeCardinality == 0 && afterCardinality == 0 {
									return beforeSet == "" && afterSet == observationSet
								}
								return (charged == 0) == (beforeSet == afterSet)
							}
							digestTransitionMatches := dimensionDigestMatches(step.ChargedReleaseFacts,
								step.Before.Root.ReleaseCardinality, step.After.Root.ReleaseCardinality,
								step.Before.Root.ReleaseSetSHA256, step.After.Root.ReleaseSetSHA256, step.ReleaseSetSHA256) &&
								dimensionDigestMatches(step.ChargedDependencyFacts,
									step.Before.Root.DependencyCardinality, step.After.Root.DependencyCardinality,
									step.Before.Root.DependencySetSHA256, step.After.Root.DependencySetSHA256, step.DependencySetSHA256) &&
								dimensionDigestMatches(step.ChargedOutcomeFacts,
									step.Before.Root.OutcomeCardinality, step.After.Root.OutcomeCardinality,
									step.Before.Root.OutcomeSetSHA256, step.After.Root.OutcomeSetSHA256, step.OutcomeSetSHA256)
							visibleDelta, companionDelta := int64(1), int64(1)
							if wantReplay {
								visibleDelta, companionDelta = 0, 0
							}
							businessMatches := step.After.Business.VisibleCalls-step.Before.Business.VisibleCalls == visibleDelta &&
								step.After.Business.CompanionCalls-step.Before.Business.CompanionCalls == companionDelta
							replayMatches := step.SemanticReplay == wantReplay &&
								(!wantReplay || step.Before.Root == step.After.Root)
							if cardinalityMatches && chargedMatches && digestTransitionMatches && businessMatches && replayMatches {
								continue
							}
							t.Fatalf("real RLS %s/%s retained evidence invariant: %v; steps=%d trace=%d success=%d first_rejection=%d stop=%q unrelated=%d results_after_budget=%d sample_rejected=%t mismatch_step=%d root_before=%d/%d/%d root_after=%d/%d/%d charged=%d/%d/%d oracle=%d/%d/%d cardinality_matches=%t charged_matches=%t digest_transition_matches=%t replay=%t/%t business_delta=%d/%d want=%d/%d observation_set_equals_root=%t/%t/%t atoms=%d composites=%d",
								workload.id, mode, experiment.ValidateRLSEvidence(retained), len(evidence.Steps), len(sample.Trace), evidence.SuccessfulQueries,
								evidence.FirstRejectionIndex, evidence.StopReason, evidence.UnrelatedAuthorizationDenials,
								evidence.ResultsAfterBudget, sample.Rejected, index+1,
								step.Before.Root.ReleaseCardinality, step.Before.Root.DependencyCardinality, step.Before.Root.OutcomeCardinality,
								step.After.Root.ReleaseCardinality, step.After.Root.DependencyCardinality, step.After.Root.OutcomeCardinality,
								step.ChargedReleaseFacts, step.ChargedDependencyFacts, step.ChargedOutcomeFacts,
								step.OraclePrefix.Release.Cardinality, step.OraclePrefix.Dependency.Cardinality, step.OraclePrefix.Outcome.Cardinality,
								cardinalityMatches, chargedMatches, digestTransitionMatches, step.SemanticReplay, wantReplay,
								step.After.Business.VisibleCalls-step.Before.Business.VisibleCalls,
								step.After.Business.CompanionCalls-step.Before.Business.CompanionCalls, visibleDelta, companionDelta,
								step.ReleaseSetSHA256 == step.After.Root.ReleaseSetSHA256,
								step.DependencySetSHA256 == step.After.Root.DependencySetSHA256,
								step.OutcomeSetSHA256 == step.After.Root.OutcomeSetSHA256,
								step.PredicateAtomCount, step.CompositeCount)
						}
						if workload.id == "policy-denied-control" {
							t.Fatalf("real RLS %s/%s retained evidence invariant: %v; binding version=%q corpus_id=%t corpus_sha=%t trace_sha=%t dataset_id=%t dataset_sha=%t seed=%d/%d oracle_before=%t",
								workload.id, mode, experiment.ValidateRLSEvidence(retained), evidence.Version,
								evidence.CorpusID == finalv5rls.CorpusID, evidence.CorpusSHA256 == finalv5rls.CorpusSHA256,
								evidence.TraceSHA256 == finalv5rls.TraceSHA256, evidence.DatasetID == finalv5rls.DatasetID,
								evidence.DatasetSHA256 == finalv5rls.DatasetSHA256(adapter.manifest), evidence.PolicySeed, adapter.manifest.Seed,
								evidence.OracleComputedBefore)
						}
					}
					t.Fatalf("real RLS %s/%s retained evidence invariant: %v", workload.id, mode,
						experiment.ValidateRLSEvidence(retained))
				}
				t.Fatalf("real RLS %s/%s = %s/%s: %s", workload.id, mode, sample.Status, sample.ErrorCode, sample.Reason)
			}
			if err := experiment.ValidateRLSEvidence(sample); err != nil {
				t.Fatalf("real RLS %s/%s strict evidence: %v", workload.id, mode, err)
			}
			if err := sample.Validate(); err != nil {
				t.Fatalf("real RLS %s/%s runner sample contract: %v", workload.id, mode, err)
			}
			if workload.id == "adaptive-100-v1" {
				wantSteps, wantSuccess := 100, 100
				if mode == "bounded" {
					wantSteps, wantSuccess = adapter.boundedStop.Index, adapter.boundedStop.SuccessfulQueries
				}
				if len(sample.RLSVerification.Steps) != wantSteps || sample.RLSVerification.SuccessfulQueries != wantSuccess {
					t.Fatalf("real RLS %s primary counters = steps:%d success:%d", mode,
						len(sample.RLSVerification.Steps), sample.RLSVerification.SuccessfulQueries)
				}
				continue
			}
			steps := sample.RLSVerification.Steps
			negative := sample.RLSVerification.NegativeControl
			wantCode := "42501"
			if mode != "rls" {
				wantCode = "COLUMN_NOT_APPROVED"
			}
			if len(steps) != 2 || negative == nil || !negative.PolicyFiltered ||
				steps[0].RowCount != 0 || !steps[0].Accepted || steps[0].Rejected ||
				!steps[1].Rejected || steps[1].ObservedErrorCode != wantCode ||
				negative.ObservedAuthorizationErrorCode != wantCode || !sample.Rejected ||
				!sample.RejectedNoResult || !sample.RejectedNoArtifact || !sample.RejectedNoSuccessfulAudit {
				t.Fatalf("real RLS %s combined policy/rejection control = steps:%+v negative:%+v", mode, steps, negative)
			}
			if mode != "rls" && (steps[0].Verification == nil || steps[1].Before == nil || steps[1].After == nil ||
				steps[1].Before.Root != steps[1].After.Root) {
				t.Fatalf("real RLS %s lacks signed empty artifact or unchanged rejected root", mode)
			}
		}
	}
}
