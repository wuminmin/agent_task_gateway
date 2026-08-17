package main

import (
	"errors"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func TestAttackRealExecutionFailureRetainsPartialEvidence(t *testing.T) {
	diagnostics := captureAdapterDiagnostics(t)
	operation := experiment.AdapterOperation{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "attack", CellID: "A/direct",
		SampleID: "sample-1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair-1",
		PairedSystemOrder: "direct,novel", RootGroupID: "root-1", Mode: "direct", WorkloadID: "A-pagination",
		Scale: "complete-to-pages",
	}
	partial := baseSample(operation, "postgresql")
	partial.AttackVerification = &experiment.AttackVerificationEvidence{
		Version: attackEvidenceVersion,
		Steps:   []experiment.AttackStepEvidence{{Index: 1, VariantID: "complete", Accepted: true}},
	}
	failed := failedAttackSample(operation, partial, errors.New("backend timeout containing private detail"))
	if failed.Status != "fail" || failed.ErrorCode != "attack_real_execution_failure" ||
		failed.AttackVerification == nil || len(failed.AttackVerification.Steps) != 1 ||
		failed.AttackVerification.Steps[0].VariantID != "complete" {
		t.Fatalf("partial evidence was dropped or relabeled: %+v", failed)
	}
	if failed.Reason == "" || failed.Reason == "backend timeout containing private detail" {
		t.Fatalf("failure reason is absent or leaked backend text: %q", failed.Reason)
	}
	if got := diagnostics.String(); !strings.Contains(got, "backend timeout containing private detail") {
		t.Fatalf("attack backend cause was not retained in adapter stderr: %q", got)
	}
}

func TestAttackInvariantFailureRetainsEvidenceWithDistinctCode(t *testing.T) {
	diagnostics := captureAdapterDiagnostics(t)
	operation := experiment.AdapterOperation{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "attack", CellID: "E/novel",
		SampleID: "sample-1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair-1",
		PairedSystemOrder: "direct,novel", RootGroupID: "root-1", Mode: "novel", WorkloadID: "E-threshold",
		Scale: "preregistered-v1",
	}
	partial := baseSample(operation, "taskgate")
	partial.AttackVerification = &experiment.AttackVerificationEvidence{Version: attackEvidenceVersion}
	failed := failedAttackSample(operation, partial, &attackInvariantError{reason: "private validator detail"})
	if failed.Status != "fail" || failed.ErrorCode != "attack_invariant_violation" || failed.AttackVerification == nil ||
		strings.Contains(failed.Reason, "private validator detail") {
		t.Fatalf("invariant failure was not retained distinctly: %+v", failed)
	}
	if got := diagnostics.String(); !strings.Contains(got, "private validator detail") {
		t.Fatalf("attack invariant cause was not retained in adapter stderr: %q", got)
	}
}

func TestAttackUnsupportedCellIsRetainedAsInvalid(t *testing.T) {
	corpus, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	operation := experiment.AdapterOperation{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "attack", CellID: "unsupported/direct",
		SampleID: "sample-1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair-1",
		PairedSystemOrder: "direct,novel", RootGroupID: "root-1", Mode: "direct", WorkloadID: "unsupported",
		Scale: "unsupported",
	}
	sample := (&attackAdapter{corpus: corpus, states: map[string]*attackState{}}).Execute(t.Context(), operation)
	if sample.Status != "invalid" || sample.ErrorCode != "unsupported_source_controlled_attack_cell" {
		t.Fatalf("unsupported attack cell was dropped or relabeled: %+v", sample)
	}
	if sample.ExperimentID != operation.ExperimentID || sample.CellID != operation.CellID || sample.SampleID != operation.SampleID {
		t.Fatalf("invalid attack sample lost operation identity: %+v", sample)
	}
}

func TestAttackPrefixFinalizationRetainsCompletedStepsAcrossLaterFailure(t *testing.T) {
	corpus, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, found := corpus.Lookup("E-threshold", "preregistered-v1")
	if !found {
		t.Fatal("frozen E cell is absent")
	}
	operation := experiment.AdapterOperation{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "attack", CellID: "E/novel",
		SampleID: "sample-1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair-1",
		PairedSystemOrder: "direct,novel", RootGroupID: "root-1", Mode: "novel", WorkloadID: "E-threshold",
		Scale: "preregistered-v1",
	}
	completed := experiment.AttackStepEvidence{
		Index: 1, VariantID: attackCase.Steps[0].ID, Classification: attackCase.Steps[0].Classification,
		Role: attackCase.Steps[0].Role, Accepted: true, ResultSHA256: sha("completed-prefix"),
	}
	marker := errors.New("later setup or step failed")
	partial, retainedErr := retainedAttackPrefix(operation, attackCase, []experiment.AttackStepEvidence{completed},
		&attackState{rootTaskID: "root-task", budgetProfile: "detail-manual-v5"}, 1, marker)
	if !errors.Is(retainedErr, marker) {
		t.Fatalf("retained failure cause = %v", retainedErr)
	}
	failed := failedAttackSample(operation, partial, retainedErr)
	if failed.Status != "fail" || failed.AttackVerification == nil || len(failed.AttackVerification.Steps) != 1 ||
		len(failed.Trace) != 1 || failed.AttackVerification.Steps[0].VariantID != "threshold-300" {
		t.Fatalf("terminal setup failure discarded its completed prefix: %+v", failed)
	}
}

func TestAttackErrorReasonIsStableAndClientSafe(t *testing.T) {
	if got := safeAttackErrorReason("EXPOSURE_BUDGET_EXHAUSTED", "host-specific text"); got != "ROOT_OUTCOME_CEILING_EXCEEDED" {
		t.Fatalf("budget reason = %q", got)
	}
	if got := safeAttackErrorReason("SQL_NOT_LOWERABLE", "SET_OPERATION_UNSUPPORTED"); got != "SET_OPERATION_UNSUPPORTED" {
		t.Fatalf("lowering reason = %q", got)
	}
	if got := safeAttackErrorReason("INTERNAL_ERROR", "secret detail"); got != "UNREGISTERED_STRUCTURED_REJECTION" {
		t.Fatalf("unregistered reason leaked raw text: %q", got)
	}
}

func TestAttackResultMetadataNormalizationMatchesSignedControlBytes(t *testing.T) {
	got, err := normalizeAttackResultMetadata([]byte(`{ "plan_digest": "abc", "display_columns": ["receipt_no", "amount"] }`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"display_columns":["receipt_no","amount"],"plan_digest":"abc"}`
	if string(got) != want || shaBytes(got) != sha(want) {
		t.Fatalf("normalized metadata = %s", got)
	}
	if _, err := normalizeAttackResultMetadata([]byte(`{"plan_digest":`)); err == nil {
		t.Fatal("invalid JSON metadata was accepted")
	}
}

func TestAttackObservedOutcomeIsThresholdOnly(t *testing.T) {
	root := experiment.RootLedgerSnapshot{OutcomeCardinality: 7}
	if got := attackObservedThresholdOutcome("A-pagination", root); got != 0 {
		t.Fatalf("non-threshold observed outcome = %d", got)
	}
	if got := attackObservedThresholdOutcome("E-threshold", root); got != 7 {
		t.Fatalf("threshold observed outcome = %d", got)
	}
}

func TestAttackDecompositionRowsCollectsExactMultiplicity(t *testing.T) {
	steps := []experiment.AttackStepEvidence{
		{Accepted: true, Role: "complete", RowSHA256: []string{"c1", "c2", "c3", "c4", "c5", "c6"}},
		{Accepted: true, Role: "partition", RowSHA256: []string{"p1", "p2"}},
		{Accepted: true, Role: "partition", RowSHA256: []string{"p3", "p4"}},
		{Accepted: true, Role: "partition", RowSHA256: []string{"p5", "p6"}},
		{Accepted: true, Role: "overlap", RowSHA256: []string{"ignored-overlap-1", "ignored-overlap-2"}},
		{Accepted: true, Role: "replay", RowSHA256: []string{"ignored-replay-1", "ignored-replay-2"}},
	}
	complete, decomposed := attackDecompositionRows(steps)
	if len(complete) != 6 || len(decomposed) != 6 {
		t.Fatalf("collected complete/decomposed rows = %d/%d, want exact 6/6", len(complete), len(decomposed))
	}
	if complete[0] != "c1" || complete[5] != "c6" || decomposed[0] != "p1" || decomposed[5] != "p6" {
		t.Fatalf("collector changed exact row sequence: complete=%v decomposed=%v", complete, decomposed)
	}
}

func TestAttackCatalogBindingsKeepECeilingOnProductionProfile(t *testing.T) {
	ad := expectedAttackCatalogBinding("A-pagination")
	e := expectedAttackCatalogBinding("E-threshold")
	if ad.Product != "final_v5_attack_expense_detail" || ad.BudgetProfile != "final-v5-attack-medium-v1" ||
		ad.MaxQueries != 10 || ad.MaxOutcomeFacts != 10 {
		t.Fatalf("A--D binding = %+v", ad)
	}
	if e.Product != "expense_detail" || e.BudgetProfile != "detail-manual-v5" ||
		e.MaxQueries != 5 || e.MaxOutcomeFacts != 5 {
		t.Fatalf("E binding = %+v", e)
	}
}
