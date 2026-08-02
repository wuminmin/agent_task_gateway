package experiment

import (
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestAttackDirectThresholdEvidenceRecomputesAndRejectsMutations(t *testing.T) {
	if err := ValidateAttackEvidence(validDirectAttackSample(t, "E-threshold", "preregistered-v1")); err != nil {
		t.Fatalf("valid direct threshold evidence: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Sample)
	}{
		{name: "corpus digest", mutate: func(sample *Sample) { sample.AttackVerification.CorpusSHA256 = testAttackDigest("other-corpus") }},
		{name: "product binding", mutate: func(sample *Sample) { sample.AttackVerification.Product = "expense_summary" }},
		{name: "fabricated direct profile", mutate: func(sample *Sample) { sample.AttackVerification.BudgetProfile = "detail-manual-v5" }},
		{name: "frozen SQL", mutate: func(sample *Sample) {
			sample.AttackVerification.Steps[0].LogicalSQLSHA256 = testAttackDigest("other-sql")
		}},
		{name: "row set", mutate: func(sample *Sample) {
			sample.AttackVerification.Steps[0].RowSetSHA256 = testAttackDigest("other-row-set")
		}},
		{name: "primary sequence", mutate: func(sample *Sample) {
			sample.AttackVerification.PrimaryResultSHA256 = testAttackDigest("other-primary")
		}},
		{name: "threshold oracle", mutate: func(sample *Sample) { *sample.AttackVerification.Steps[0].ScalarInt64++ }},
		{name: "trace state", mutate: func(sample *Sample) { sample.Trace[0].PriorStateSHA256 = testAttackDigest("other-state") }},
		{name: "top SQL binding", mutate: func(sample *Sample) { sample.PhysicalSQLSHA256 = testAttackDigest("other-sequence") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sample := validDirectAttackSample(t, "E-threshold", "preregistered-v1")
			test.mutate(&sample)
			if err := ValidateAttackEvidence(sample); err == nil {
				t.Fatal("mutated threshold evidence was accepted")
			}
		})
	}
}

func TestAttackDecompositionAndEquivalentSQLRejectExactRelationDrift(t *testing.T) {
	pagination := validDirectAttackSample(t, "A-pagination", "complete-to-pages")
	if err := ValidateAttackEvidence(pagination); err != nil {
		t.Fatalf("valid pagination evidence: %v", err)
	}
	foreign := testAttackRow(t, "foreign-row")
	pagination.AttackVerification.Steps[1].RowSHA256[0] = foreign
	pagination.AttackVerification.Steps[1].RowSetSHA256 = mustAttackRowSet(t, pagination.AttackVerification.Steps[1].RowSHA256)
	partitions := []string{}
	for _, step := range pagination.AttackVerification.Steps {
		if step.Role == "partition" {
			partitions = append(partitions, step.RowSHA256...)
		}
	}
	pagination.AttackVerification.DecomposedRowSetSHA256 = mustAttackRowSet(t, partitions)
	if err := ValidateAttackEvidence(pagination); err == nil {
		t.Fatal("pagination accepted a self-consistent but different decomposed union")
	}

	equivalent := validDirectAttackSample(t, "B-equivalent-sql", "variants-v1")
	if err := ValidateAttackEvidence(equivalent); err != nil {
		t.Fatalf("valid equivalent-SQL evidence: %v", err)
	}
	equivalent.AttackVerification.Steps[2].ResultSHA256 = testAttackDigest("different-equivalent-result")
	equivalent.Trace[2].ResultSHA256 = equivalent.AttackVerification.Steps[2].ResultSHA256
	if err := ValidateAttackEvidence(equivalent); err == nil {
		t.Fatal("equivalent SQL accepted a different exact result")
	}
}

func TestAttackLoweringRejectionRequiresZeroSideEffects(t *testing.T) {
	manifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, _ := manifest.Lookup("B-equivalent-sql", "variants-v1")
	expected := attackCase.Steps[len(attackCase.Steps)-1]
	fresh := RootLedgerSnapshot{RootObservationSetSHA256: emptyRootObservationSetSHA256()}
	snapshot := AttackControlSnapshot{
		TaskIDHash: testAttackDigest("task"), RootTaskIDHash: testAttackDigest("task"),
		Product: "final_v5_attack_expense_detail", BudgetProfile: "final-v5-attack-medium-v1",
		MaxQueries: 10, MaxOutcomeFacts: 10,
		Business: BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 4, CompanionCalls: 4},
		Root:     fresh,
	}
	projection := AttackRejectedQueryEvidence{}
	step := AttackStepEvidence{
		Rejected: true, ObservedErrorCode: expected.ExpectedErrorCode, ObservedErrorReason: expectedAttackErrorReason(expected),
		TraceIDHash: testAttackDigest("trace"), RequestIDHash: testAttackDigest("request"),
		Before: &snapshot, After: &snapshot, RejectedQuery: &projection,
	}
	sample := Sample{System: "taskgate"}
	if err := validateAttackRejectedStep(sample, expected, step); err != nil {
		t.Fatalf("valid lowering rejection: %v", err)
	}
	changed := snapshot
	changed.QueryRecords++
	step.After = &changed
	if err := validateAttackRejectedStep(sample, expected, step); err == nil {
		t.Fatal("lowering rejection accepted a retained query record")
	}
}

func TestAttackExposureRejectionRetainsOnlyFailureProjection(t *testing.T) {
	manifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, _ := manifest.Lookup("E-threshold", "preregistered-v1")
	expected := attackCase.Steps[len(attackCase.Steps)-1]
	root := validRootLedgerSnapshot()
	root.OutcomeCardinality = 5
	before := AttackControlSnapshot{
		TaskIDHash: testAttackDigest("child-task"), RootTaskIDHash: testAttackDigest("root-task"),
		Product: "expense_detail", BudgetProfile: "detail-manual-v5", MaxQueries: 5, MaxOutcomeFacts: 5,
		Business: BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 5, CompanionCalls: 5},
		Root:     root, QueryRecords: 5, Settlements: 5, Observations: 5, Receipts: 5, Artifacts: 5,
		AvailableArtifacts: 5, SuccessfulAudits: 20, CanonicalObjects: 5,
	}
	after := before
	after.Business.VisibleCalls++
	after.Business.CompanionCalls++
	after.UsedQueries++
	after.QueryRecords++
	after.Receipts++
	after.FailureAudits += 2
	projection := AttackRejectedQueryEvidence{
		Found: true, QueryIDHash: testAttackDigest("failed-query"), Status: "FAILED",
		ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED", ReservationStatus: "RELEASED", FailureAudits: 2, Receipts: 1,
	}
	step := AttackStepEvidence{
		Rejected: true, ObservedErrorCode: expected.ExpectedErrorCode, ObservedErrorReason: expectedAttackErrorReason(expected),
		TraceIDHash: testAttackDigest("trace"), RequestIDHash: testAttackDigest("request"),
		Before: &before, After: &after, RejectedQuery: &projection,
	}
	sample := Sample{System: "taskgate"}
	if err := validateAttackRejectedStep(sample, expected, step); err != nil {
		t.Fatalf("valid exposure rejection: %v", err)
	}
	mutated := projection
	mutated.Artifacts = 1
	step.RejectedQuery = &mutated
	if err := validateAttackRejectedStep(sample, expected, step); err == nil {
		t.Fatal("exposure rejection accepted an artifact")
	}
	step.RejectedQuery = &projection
	exhausted := before
	exhausted.UsedQueries = exhausted.MaxQueries
	afterExhausted := after
	afterExhausted.UsedQueries = exhausted.UsedQueries + 1
	step.Before, step.After = &exhausted, &afterExhausted
	if err := validateAttackRejectedStep(sample, expected, step); err == nil {
		t.Fatal("outcome rejection accepted a task whose resource query budget was already exhausted")
	}
}

func TestAttackEUsesExactRootFiveWithActiveParentAndIndependentChildBudget(t *testing.T) {
	manifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, _ := manifest.Lookup("E-threshold", "preregistered-v1")
	rootHash, childHash := testAttackDigest("root-task"), testAttackDigest("child-task")
	steps := make([]AttackStepEvidence, len(attackCase.Steps))
	for index := 0; index < 5; index++ {
		beforeRoot, afterRoot := validRootLedgerSnapshot(), validRootLedgerSnapshot()
		beforeRoot.OutcomeCardinality, afterRoot.OutcomeCardinality = int64(index), int64(index+1)
		taskHash, usedQueries := rootHash, int64(index-1)
		if index == 0 {
			taskHash, usedQueries = childHash, 0
		}
		before := AttackControlSnapshot{
			TaskIDHash: taskHash, RootTaskIDHash: rootHash, Product: "expense_detail", BudgetProfile: "detail-manual-v5",
			MaxQueries: 5, UsedQueries: usedQueries, MaxOutcomeFacts: 5, Root: beforeRoot,
		}
		after := before
		after.UsedQueries++
		after.Root = afterRoot
		steps[index] = AttackStepEvidence{Accepted: true, RootTaskIDHash: rootHash, Before: &before, After: &after}
	}
	terminalRoot := validRootLedgerSnapshot()
	terminalRoot.OutcomeCardinality = 5
	childBefore := AttackControlSnapshot{
		TaskIDHash: childHash, RootTaskIDHash: rootHash, Product: "expense_detail", BudgetProfile: "detail-manual-v5",
		MaxQueries: 5, UsedQueries: 1, MaxOutcomeFacts: 5, Root: terminalRoot,
	}
	childAfter := childBefore
	childAfter.UsedQueries = 2
	steps[5] = AttackStepEvidence{
		Rejected: true, ObservedErrorCode: "EXPOSURE_BUDGET_EXHAUSTED", Before: &childBefore, After: &childAfter,
	}
	sample := Sample{
		System: "taskgate", WorkloadID: "E-threshold", RootTaskIDHash: rootHash,
		AttackVerification: &AttackVerificationEvidence{
			Product: "expense_detail", BudgetProfile: "detail-manual-v5", RootTaskIDHash: rootHash, Steps: steps,
		},
	}
	if err := validateAttackCatalogBinding(sample, attackCase); err != nil {
		t.Fatalf("valid root/child budget binding: %v", err)
	}
	steps[0].Before.TaskIDHash, steps[0].After.TaskIDHash = rootHash, rootHash
	if err := validateAttackCatalogBinding(sample, attackCase); err == nil {
		t.Fatal("E accepted a first step routed to the root instead of the frozen child")
	}
	steps[0].Before.TaskIDHash, steps[0].After.TaskIDHash = childHash, childHash
	otherChild := testAttackDigest("other-child-task")
	steps[5].Before.TaskIDHash, steps[5].After.TaskIDHash = otherChild, otherChild
	if err := validateAttackCatalogBinding(sample, attackCase); err == nil {
		t.Fatal("E accepted different prefix and terminal child tasks")
	}
	steps[5].Before.TaskIDHash, steps[5].After.TaskIDHash = childHash, childHash
	steps[2].Before.TaskIDHash, steps[2].After.TaskIDHash = childHash, childHash
	if err := validateAttackCatalogBinding(sample, attackCase); err == nil {
		t.Fatal("E accepted a primer routed away from the frozen root task")
	}
	steps[2].Before.TaskIDHash, steps[2].After.TaskIDHash = rootHash, rootHash
	steps[5].Before.MaxQueries = 1
	if err := validateAttackCatalogBinding(sample, attackCase); err == nil {
		t.Fatal("E accepted a mutated child resource ceiling")
	}
}

func TestAttackEOutcomeSequenceProvesPrimerAndZeroDeltaReplays(t *testing.T) {
	build := func() []AttackStepEvidence {
		released := func(beforeOutcome, afterOutcome, charged int64, plan, result, context, set, composite string,
			chargedPredicate, chargedComposite int64, replay bool) AttackStepEvidence {
			before := AttackControlSnapshot{Root: RootLedgerSnapshot{OutcomeCardinality: beforeOutcome}}
			after := AttackControlSnapshot{Root: RootLedgerSnapshot{OutcomeCardinality: afterOutcome}}
			return AttackStepEvidence{
				Accepted: true, Before: &before, After: &after, ChargedOutcomeFacts: charged,
				PredicateAtomCount: 1, CompositeCount: 1, PlanSHA256: plan, ResultSHA256: result,
				SemanticReplay: replay,
				Verification: &BaselineVerificationEvidence{Receipt: queryreceipt.QueryReceiptV1{
					Exposure: &queryreceipt.ExposureEvidenceV1{
						PredicateContextSHA256: context, PredicateSetSHA256: set,
						ActualPredicateAtomCount: 1, ChargedPredicateAtomCount: chargedPredicate,
						CompositeOutcomeSHA256: composite, ActualCompositeCount: 1,
						ChargedCompositeCount: chargedComposite,
					},
				}},
			}
		}
		context320, set320 := testAttackDigest("context-320"), testAttackDigest("set-320")
		primerPlan, primerResult, primerComposite := testAttackDigest("detail-plan"), testAttackDigest("detail-result"), testAttackDigest("detail-composite")
		steps := []AttackStepEvidence{
			released(0, 2, 2, testAttackDigest("plan-300"), testAttackDigest("result-300"),
				testAttackDigest("context-300"), testAttackDigest("set-300"), testAttackDigest("composite-300"), 1, 1, false),
			released(2, 4, 2, testAttackDigest("plan-320"), testAttackDigest("result-320"),
				context320, set320, testAttackDigest("composite-320"), 1, 1, false),
			released(4, 5, 1, primerPlan, primerResult, context320, set320, primerComposite, 0, 1, false),
			released(5, 5, 0, primerPlan, primerResult, context320, set320, primerComposite, 0, 0, true),
			released(5, 5, 0, primerPlan, primerResult, context320, set320, primerComposite, 0, 0, true),
		}
		before, after := AttackControlSnapshot{Root: RootLedgerSnapshot{OutcomeCardinality: 5}},
			AttackControlSnapshot{Root: RootLedgerSnapshot{OutcomeCardinality: 5}}
		steps = append(steps, AttackStepEvidence{Rejected: true, Before: &before, After: &after})
		return steps
	}
	if err := validateAttackEOutcomeSequence(build()); err != nil {
		t.Fatalf("valid exact E outcome sequence: %v", err)
	}
	mutations := []struct {
		name   string
		mutate func([]AttackStepEvidence)
	}{
		{name: "primer charged a new atom", mutate: func(steps []AttackStepEvidence) {
			steps[2].Verification.Receipt.Exposure.ChargedPredicateAtomCount = 1
		}},
		{name: "primer reused composite", mutate: func(steps []AttackStepEvidence) {
			steps[2].Verification.Receipt.Exposure.CompositeOutcomeSHA256 = steps[1].Verification.Receipt.Exposure.CompositeOutcomeSHA256
		}},
		{name: "exact replay changed root", mutate: func(steps []AttackStepEvidence) {
			steps[3].After.Root.OutcomeCardinality = 6
		}},
		{name: "rewrite charged composite", mutate: func(steps []AttackStepEvidence) {
			steps[4].Verification.Receipt.Exposure.ChargedCompositeCount = 1
		}},
		{name: "B+1 started below five", mutate: func(steps []AttackStepEvidence) {
			steps[5].Before.Root.OutcomeCardinality = 4
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			steps := build()
			test.mutate(steps)
			if err := validateAttackEOutcomeSequence(steps); err == nil {
				t.Fatal("mutated E outcome sequence was accepted")
			}
		})
	}
}

func TestAttackRequestIDModesBindNovelAnchor(t *testing.T) {
	root := validRootLedgerSnapshot()
	snapshot := AttackControlSnapshot{
		Business: BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 5, CompanionCalls: 5},
		Root:     root, QueryRecords: 1, Settlements: 1, Observations: 1, Receipts: 1, Artifacts: 1,
		AvailableArtifacts: 1, SuccessfulAudits: 4, CanonicalObjects: 1,
	}
	anchors := [3]string{testAttackDigest("anchor-request"), testAttackDigest("anchor-query"), testAttackDigest("anchor-result")}
	base := AttackVerificationEvidence{
		AnchorRequestIDHash: anchors[0], AnchorQueryIDHash: anchors[1], AnchorResultIDHash: anchors[2],
		Steps: []AttackStepEvidence{{Accepted: true, Before: &snapshot, After: &snapshot}},
	}
	for _, mode := range []string{"novel", "semantic_replay", "idempotent_replay"} {
		t.Run(mode, func(t *testing.T) {
			evidence := base
			evidence.Steps = append([]AttackStepEvidence(nil), base.Steps...)
			step := &evidence.Steps[0]
			switch mode {
			case "novel":
				step.RequestIDHash, step.QueryIDHash, step.ResultIDHash = anchors[0], anchors[1], anchors[2]
			case "semantic_replay":
				step.RequestIDHash, step.QueryIDHash, step.ResultIDHash = testAttackDigest("new-request"), testAttackDigest("new-query"), testAttackDigest("new-result")
				step.SemanticReplay = true
			case "idempotent_replay":
				step.RequestIDHash, step.QueryIDHash, step.ResultIDHash = anchors[0], anchors[1], anchors[2]
				step.IdempotentReplay = true
			}
			sample := Sample{Mode: mode, AttackVerification: &evidence}
			if err := validateAttackRequestIdentity(sample); err != nil {
				t.Fatalf("valid %s identity evidence: %v", mode, err)
			}
			step.QueryIDHash = anchors[1]
			if mode == "semantic_replay" {
				if err := validateAttackRequestIdentity(sample); err == nil {
					t.Fatal("semantic replay accepted novel query identity reuse")
				}
			}
		})
	}
}

func TestAttackReplayTopLevelChargeIsOperationDeltaNotStoredReceiptCharge(t *testing.T) {
	root := validRootLedgerSnapshot()
	snapshot := AttackControlSnapshot{
		Business: BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 5, CompanionCalls: 5},
		Root:     root, QueryRecords: 1, Settlements: 1, Observations: 1, Receipts: 1, Artifacts: 1,
		AvailableArtifacts: 1, SuccessfulAudits: 4, CanonicalObjects: 1,
	}
	step := AttackStepEvidence{
		Accepted: true, Before: &snapshot, After: &snapshot, RootTaskIDHash: testAttackDigest("root-task"),
		ArtifactSHA256: testAttackDigest("parquet"), ObjectSHA256: testAttackDigest("object"),
		ParquetBytes: 100, EncryptedObjectBytes: 140, ReceiptVersion: "8", ReceiptSHA256: testAttackDigest("receipt"),
		ArtifactIntentSHA256: testAttackDigest("intent"), AvailabilitySHA256: testAttackDigest("availability"),
	}
	evidence := &AttackVerificationEvidence{Steps: []AttackStepEvidence{step}, FinalRoot: &root}
	sample := Sample{
		Mode: "semantic_replay", SemanticReplay: true, AttackVerification: evidence,
		RootEpochBefore: root.Epoch, RootEpochAfter: root.Epoch,
		RootSetSHA256Before: rootLedgerSetSHA256(root), RootSetSHA256After: rootLedgerSetSHA256(root),
		ReleaseSetSHA256: root.ReleaseSetSHA256, DependencySetSHA256: root.DependencySetSHA256, OutcomeSetSHA256: root.OutcomeSetSHA256,
		ActualReleaseFacts: root.ReleaseCardinality, ActualDependencyFacts: root.DependencyCardinality, ActualOutcomeFacts: root.OutcomeCardinality,
		RootTaskIDHash: step.RootTaskIDHash, ArtifactSHA256: step.ArtifactSHA256, ObjectSHA256: step.ObjectSHA256,
		ParquetBytes: step.ParquetBytes, EncryptedObjectBytes: step.EncryptedObjectBytes, ReceiptVersion: step.ReceiptVersion,
		ReceiptSHA256: step.ReceiptSHA256, ArtifactIntentSHA256: step.ArtifactIntentSHA256,
		AvailabilityAuditSHA256: step.AvailabilitySHA256, ReceiptVerified: true, ArtifactAvailable: true,
	}
	if err := validateAttackTaskgateSummary(sample); err != nil {
		t.Fatalf("zero-delta semantic replay summary: %v", err)
	}
	evidence.ObservedOutcome = root.OutcomeCardinality
	if err := validateAttackTaskgateSummary(sample); err == nil {
		t.Fatal("non-threshold summary accepted threshold-only observed outcome evidence")
	}
	sample.WorkloadID = "E-threshold"
	if err := validateAttackTaskgateSummary(sample); err != nil {
		t.Fatalf("threshold summary rejected its observed root outcome: %v", err)
	}
	sample.WorkloadID, evidence.ObservedOutcome = "", 0
	sample.ChargedOutcomeFacts = root.OutcomeCardinality
	if err := validateAttackTaskgateSummary(sample); err == nil {
		t.Fatal("semantic replay accepted the original receipt charge as this operation's charge")
	}
}

func TestAttackReleasedAuditDeltaDistinguishesSemanticReplay(t *testing.T) {
	novel := expectedAttackReleasedSnapshotDelta(AttackStepEvidence{})
	replay := expectedAttackReleasedSnapshotDelta(AttackStepEvidence{SemanticReplay: true})
	if novel.SuccessfulAudits != 4 || replay.SuccessfulAudits != 3 {
		t.Fatalf("novel/replay successful audit deltas = %d/%d", novel.SuccessfulAudits, replay.SuccessfulAudits)
	}
	novel.SuccessfulAudits = 0
	replay.SuccessfulAudits = 0
	if novel != replay {
		t.Fatalf("semantic replay changed a non-audit release delta: novel=%+v replay=%+v", novel, replay)
	}
}

func validDirectAttackSample(t *testing.T, workloadID, scale string) Sample {
	t.Helper()
	manifest, err := finalv5attack.Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, found := manifest.Lookup(workloadID, scale)
	if !found {
		t.Fatalf("unknown attack case %s/%s", workloadID, scale)
	}
	sample := Sample{
		ExperimentID: "attack", CampaignID: "campaign", DeploymentID: "deployment-01", System: "postgresql",
		Mode: "direct", WorkloadID: workloadID, Scale: scale, Status: "pass",
		PhysicalSQLSHA256: attackSQLSequenceDigest(attackCase, true), LogicalSQLSHA256: attackSQLSequenceDigest(attackCase, false),
		QueryPlanSHA256: sha256Hex([]byte(finalv5attack.CorpusSHA256 + "\x00" + workloadID + "\x00" + scale)),
	}
	evidence := &AttackVerificationEvidence{
		Version: attackVerificationVersion, CorpusID: finalv5attack.CorpusID, CorpusSHA256: finalv5attack.CorpusSHA256,
		DatasetID: finalv5attack.DatasetID, Product: expectedAttackBindingFor(workloadID).product,
		ExpectedThresholds: append([]int64(nil), attackCase.Thresholds...),
		OutcomeCeiling:     attackCase.OutcomeCeiling,
	}
	primary := []string{}
	traceState := sha256Hex([]byte("TASKGATE-FINAL-V5-ATTACK-TRACE-V1\x00" + finalv5attack.CorpusSHA256))
	for index, expected := range attackCase.Steps {
		rows, result, scalar := directAttackFixtureRows(t, workloadID, expected, attackCase)
		step := AttackStepEvidence{
			Index: index + 1, VariantID: expected.ID, Classification: expected.Classification, Role: expected.Role,
			LogicalSQLSHA256: sha256Hex([]byte(expected.LogicalSQL)), DirectSQLSHA256: sha256Hex([]byte(expected.DirectSQL)),
			Accepted: true, RowCount: int64(len(rows)), ColumnCount: directAttackFixtureColumns(workloadID, expected),
			ResultSHA256: result, RowSHA256: rows, RowSetSHA256: mustAttackRowSet(t, rows), ScalarInt64: scalar,
		}
		evidence.Steps = append(evidence.Steps, step)
		if expected.Primary {
			primary = append(primary, result)
			sample.RowCount += step.RowCount
			if sample.ColumnCount == 0 {
				sample.ColumnCount = step.ColumnCount
			}
		}
		if expected.Threshold > 0 {
			evidence.ObservedThresholdResults = append(evidence.ObservedThresholdResults, *scalar)
		}
		next := "TASKGATE-FINAL-V5-ATTACK-END-V1"
		if index+1 < len(attackCase.Steps) {
			next = attackCase.Steps[index+1].DirectSQL
		}
		sample.Trace = append(sample.Trace, TraceStep{
			Index: index + 1, ConcreteSQL: expected.DirectSQL, PriorStateSHA256: traceState,
			ResultSHA256: result, NextSQLSHA256: sha256Hex([]byte(next)),
		})
		traceState = sha256Hex([]byte(traceState + "\x00" + result))
	}
	evidence.PrimaryResultSHA256, err = finalv5attack.PrimaryResultSHA256(primary)
	if err != nil {
		t.Fatal(err)
	}
	sample.ResultSHA256 = evidence.PrimaryResultSHA256
	if workloadID == "A-pagination" || workloadID == "D-split-union" {
		complete, partitions := []string{}, []string{}
		for _, step := range evidence.Steps {
			if step.Role == "complete" {
				complete = append(complete, step.RowSHA256...)
			}
			if step.Role == "partition" {
				partitions = append(partitions, step.RowSHA256...)
			}
		}
		evidence.CompleteRowSetSHA256 = mustAttackRowSet(t, complete)
		evidence.DecomposedRowSetSHA256 = mustAttackRowSet(t, partitions)
	}
	sample.AttackVerification = evidence
	return sample
}

func directAttackFixtureRows(t *testing.T, workloadID string, step finalv5attack.Step, attackCase finalv5attack.Case) ([]string, string, *int64) {
	t.Helper()
	switch workloadID {
	case "A-pagination":
		values := map[string][]string{
			"complete": {"r1", "r2", "r3", "r4", "r5", "r6"},
			"page-1":   {"r1", "r2"}, "page-2": {"r3", "r4"}, "page-3": {"r5", "r6"},
			"page-overlap": {"r2", "r3"}, "page-1-replay": {"r1", "r2"},
		}[step.ID]
		rows := testAttackRows(t, values...)
		resultID := step.ID
		if step.ID == "page-1-replay" {
			resultID = "page-1"
		}
		return rows, testAttackDigest("result:" + resultID), nil
	case "B-equivalent-sql":
		return testAttackRows(t, "b-row"), testAttackDigest("b-result"), nil
	case "D-split-union":
		values := map[string][]string{
			"complete":  {"r1", "r2", "r3", "r4", "r5", "r6"},
			"split-low": {"r1", "r2", "r3"}, "split-high": {"r4", "r5", "r6"},
			"public-union-negative": {"r1", "r2", "r3", "r4", "r5", "r6"},
		}[step.ID]
		return testAttackRows(t, values...), testAttackDigest("result:" + step.ID), nil
	case "E-threshold":
		if strings.HasPrefix(step.ID, "outcome-primer-320-") {
			return testAttackRows(t, "primer-1", "primer-2", "primer-3", "primer-4", "primer-5", "primer-6"),
				testAttackDigest("result:outcome-primer-320"), nil
		}
		value := int64(5)
		if step.Threshold > 0 {
			for index, threshold := range attackCase.Thresholds {
				if threshold == step.Threshold {
					value = attackCase.ThresholdResults[index]
				}
			}
		}
		return testAttackRows(t, fmt.Sprintf("scalar:%d", value)), testAttackDigest(fmt.Sprintf("result:%d", value)), &value
	default:
		t.Fatalf("unsupported direct fixture %s", workloadID)
		return nil, "", nil
	}
}

func directAttackFixtureColumns(workloadID string, step finalv5attack.Step) int {
	if workloadID == "E-threshold" && !strings.HasPrefix(step.ID, "outcome-primer-320-") {
		return 1
	}
	return 2
}

func testAttackRows(t *testing.T, values ...string) []string {
	t.Helper()
	rows := make([]string, 0, len(values))
	for _, value := range values {
		rows = append(rows, testAttackRow(t, value))
	}
	return rows
}

func testAttackRow(t *testing.T, value string) string {
	t.Helper()
	row, err := finalv5attack.RowSHA256(testAttackDigest("canonical-row:" + value))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func mustAttackRowSet(t *testing.T, rows []string) string {
	t.Helper()
	digest, err := finalv5attack.RowSetSHA256(rows)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func testAttackDigest(value string) string { return sha256Hex([]byte(value)) }
