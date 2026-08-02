package experiment

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestMain(m *testing.M) {
	if os.Getenv("TASKGATE_TEST_ADAPTER") == "1" {
		runTestAdapter()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runTestAdapter() {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var operation AdapterOperation
		if StrictJSON(scanner.Bytes(), &operation) != nil {
			os.Exit(1)
		}
		rootIdentity := strings.Join([]string{operation.DeploymentID, operation.WorkloadID, operation.Scale, strconv.Itoa(operation.ProcessReplicate), strconv.Itoa(operation.Iteration), operation.RootGroupID}, "\x00")
		digestBytes := sha256.Sum256([]byte(rootIdentity))
		digest := hex.EncodeToString(digestBytes[:])
		resultBytes := sha256.Sum256([]byte(operation.WorkloadID + "\x00" + operation.Scale + "\x00" + strconv.Itoa(operation.Iteration)))
		resultDigest := hex.EncodeToString(resultBytes[:])
		sample := Sample{
			SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
			ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
			Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, OrderPosition: operation.OrderPosition,
			RandomSeed: operation.RandomSeed, PairID: operation.PairID, PairedSystemOrder: operation.PairedSystemOrder,
			RootGroupID: operation.RootGroupID,
			System:      "taskgate", Mode: operation.Mode, WorkloadID: operation.WorkloadID,
			Scale: operation.Scale, ClientAvailableMS: 1, ClientFullDrainMS: 2,
			PipelineMS:   map[string]float64{"prepare": .1, "execute_and_derive": .2, "artifact_stage": .1, "control_settlement": .1, "artifact_publication": .1, "response_finalize": .1, "server_total": .8},
			DiagnosticMS: map[string]float64{}, ResultSHA256: resultDigest, RootTaskIDHash: digest, ReceiptVersion: "8",
			ReceiptSHA256: digest, ArtifactIntentSHA256: digest, AvailabilityAuditSHA256: digest,
			ReceiptVerified: true, ArtifactAvailable: true, Status: "pass", PublicationEligible: true,
		}
		if encoder.Encode(sample) != nil {
			os.Exit(1)
		}
	}
	if scanner.Err() != nil {
		os.Exit(1)
	}
}

func TestType7AndDeterministicBootstrap(t *testing.T) {
	values := []float64{1, 2, 3, 4}
	median, err := Type7(values, .5)
	if err != nil || median != 2.5 {
		t.Fatalf("median=%v err=%v", median, err)
	}
	p95, _ := Type7(values, .95)
	if p95 != 3.8499999999999996 {
		t.Fatalf("p95=%v", p95)
	}
	first, err := BootstrapMedian(values, 20260801, 500)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BootstrapMedian(values, 20260801, 500)
	if first != second {
		t.Fatalf("bootstrap is not deterministic: %+v %+v", first, second)
	}
}

func TestMatchedPairAggregationDoesNotDivideArmMedians(t *testing.T) {
	// Pair ratios are 10/1 and 20/10, whose median is 6. Dividing the two
	// independent arm medians would instead produce 15/5.5.
	statistics := summarizePairedSeries([]float64{10.0 / 1.0, 20.0 / 10.0}, []float64{9, 10})
	if statistics.N != 2 || statistics.MedianPairRatio != 6 || statistics.MedianDifferenceMS != 9.5 {
		t.Fatalf("unexpected matched-pair statistics: %+v", statistics)
	}
}

func TestProtocolBindingRejectsShrunkWorkload(t *testing.T) {
	config := Config{Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"direct", "novel"}}}}
	bindTestProtocol(t, &config)
	config.Workloads[0].Modes = []string{"direct"}
	if err := config.ValidateProtocol(protocolRoot()); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("shrunk workload profile was accepted: %v", err)
	}
}

func TestSampleValidationRejectsOverlappingPhaseSumAndReplaySQL(t *testing.T) {
	s := validTestSample()
	s.PipelineMS["server_total"] = 1
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "phase sum") {
		t.Fatalf("invalid phase sum accepted: %v", err)
	}
	s = validTestSample()
	s.SemanticReplay = true
	s.BusinessSQLDelta = 1
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "Business") {
		t.Fatalf("replay SQL accepted: %v", err)
	}
}

func TestSmokeCannotBecomePublicationEvidence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	config := Config{SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "synthetic_smoke", CampaignID: "pilot-test", ExperimentID: "baseline", Deployments: 1, Warmups: 1, Samples: 1, RandomSeed: 20260801, FreshRootPerSample: true, Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"novel"}}}}
	if err := WriteSmoke(config, dir); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(filepath.Join(dir, "generated", "latex", "evidence.tex"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(value), "newcommand") {
		t.Fatalf("pilot emitted numeric macros: %s", value)
	}
	if _, err := SealRun(dir); err == nil {
		t.Fatal("pilot evidence was sealable")
	}
}

func TestAdapterCampaignIsDeterministicAndCreateExclusive(t *testing.T) {
	config := Config{
		SchemaVersion: 1, CampaignClass: "publication", CampaignID: "campaign-test", ExperimentID: "baseline",
		SubmissionCommit: "0123456789abcdef0123456789abcdef01234567", Deployments: 3, Warmups: 5, Samples: 30,
		RandomSeed: 20260801, FreshRootPerSample: true,
		Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"novel"}}},
	}
	bindTestProtocol(t, &config)
	t.Setenv("TASKGATE_TEST_ADAPTER", "1")
	t.Setenv("TASKGATE_EXPERIMENT_CLASS", "publication")
	t.Setenv("TASKGATE_SUBMISSION_COMMIT", config.SubmissionCommit)
	t.Setenv("TASKGATE_CAMPAIGN_ID", config.CampaignID)
	first := filepath.Join(t.TempDir(), "first.jsonl")
	second := filepath.Join(t.TempDir(), "second.jsonl")
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], first); err != nil {
		t.Fatal(err)
	}
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], second); err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := os.ReadFile(first)
	secondBytes, _ := os.ReadFile(second)
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("seeded adapter operation order is not deterministic")
	}
	samples, err := ReadSamples([]string{first})
	if err != nil || len(samples) != 30 {
		t.Fatalf("samples=%d err=%v", len(samples), err)
	}
	if err := ExecuteAdapterCampaign(config, "deployment-01", os.Args[0], first); err == nil {
		t.Fatal("runner overwrote existing raw JSONL")
	}
}

func TestDependencyAwareOrderKeepsReplayAfterNovel(t *testing.T) {
	config := Config{RandomSeed: 20260801, FreshRootPerSample: true, Samples: 1, Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"direct", "idempotent_replay", "novel", "semantic_replay"}}}}
	position := 0
	operations := buildOperations(config, "deployment-01", 1, 1, &position)
	positions := map[string]int{}
	for index, operation := range operations {
		positions[operation.Mode] = index
	}
	if !(positions["novel"] < positions["semantic_replay"] && positions["semantic_replay"] < positions["idempotent_replay"]) {
		t.Fatalf("dependent replay order = %+v", operations)
	}
	if !operations[positions["novel"]].FreshRootRequired || operations[positions["direct"]].FreshRootRequired || operations[positions["semantic_replay"]].FreshRootRequired {
		t.Fatalf("fresh-root anchor flags = %+v", operations)
	}
}

func TestMatchedPairIdentityIsGroupScopedAndRecordsExactOrder(t *testing.T) {
	baseline := Config{RandomSeed: 20260801, FreshRootPerSample: true, Samples: 1, Workloads: []Workload{{ID: "S1", Scales: []string{"tiny"}, Modes: []string{"direct", "novel", "semantic_replay", "pending_recovery"}}}}
	position := 0
	operations := buildOperations(baseline, "deployment-01", 1, 1, &position)
	var baselinePair, recoveryPair string
	for _, operation := range operations {
		if operation.Mode == "pending_recovery" {
			recoveryPair = operation.PairID
		} else {
			if baselinePair == "" {
				baselinePair = operation.PairID
			}
			if operation.PairID != baselinePair || !strings.Contains(operation.PairedSystemOrder, "direct") || !strings.Contains(operation.PairedSystemOrder, "novel") {
				t.Fatalf("baseline pair metadata = %+v", operations)
			}
		}
	}
	if baselinePair == "" || recoveryPair == "" || baselinePair == recoveryPair {
		t.Fatalf("dependency groups reused pair identity: %+v", operations)
	}

	provsql := Config{RandomSeed: 20260801, Samples: 1, Workloads: []Workload{{ID: "nonce", Scales: []string{"1k"}, Modes: []string{"direct", "provsql", "taskgate"}}}}
	position = 0
	operations = buildOperations(provsql, "deployment-01", 1, 1, &position)
	if len(operations) != 3 {
		t.Fatalf("ProvSQL operations = %+v", operations)
	}
	for _, operation := range operations {
		if operation.PairID != operations[0].PairID || !sameStringSet(strings.Split(operation.PairedSystemOrder, ","), []string{"direct", "provsql", "taskgate"}) {
			t.Fatalf("ProvSQL pair metadata = %+v", operations)
		}
	}
}

func TestValidateBaselineSignedSampleRejectsUnsignedCountDrift(t *testing.T) {
	intent := &queryreceipt.ArtifactIntentEvidenceV1{
		IntentSHA256: "intent", ParquetSHA256: "parquet", ObjectSHA256: "object",
		ParquetSize: 101, ObjectSize: 202, RowCount: 3, ColumnCount: 2,
	}
	exposure := &queryreceipt.ExposureEvidenceV1{
		ReleaseSetSHA256: "release", InfluenceSetSHA256: "dependency", OutcomeSetSHA256: "outcome",
		ActualReleaseFacts: 3, ActualInfluenceFacts: 4, ActualOutcomeFacts: 6,
		ChargedReleaseFacts: 1, ChargedInfluenceFacts: 2, ChargedOutcomeFacts: 3,
		ActualPredicateAtomCount: 5, ActualCompositeCount: 1, RootEpoch: 8,
	}
	sample := Sample{
		ArtifactIntentSHA256: intent.IntentSHA256, ArtifactSHA256: intent.ParquetSHA256,
		ObjectSHA256: intent.ObjectSHA256, ParquetBytes: intent.ParquetSize,
		EncryptedObjectBytes: intent.ObjectSize, RowCount: intent.RowCount, ColumnCount: int(intent.ColumnCount),
		ReleaseSetSHA256: exposure.ReleaseSetSHA256, DependencySetSHA256: exposure.InfluenceSetSHA256,
		OutcomeSetSHA256: exposure.OutcomeSetSHA256, ActualReleaseFacts: exposure.ActualReleaseFacts,
		ActualDependencyFacts: exposure.ActualInfluenceFacts, ActualOutcomeFacts: exposure.ActualOutcomeFacts,
		ChargedReleaseFacts: exposure.ChargedReleaseFacts, ChargedDependencyFacts: exposure.ChargedInfluenceFacts,
		ChargedOutcomeFacts: exposure.ChargedOutcomeFacts, PredicateAtomCount: exposure.ActualPredicateAtomCount,
		CompositeCount: exposure.ActualCompositeCount, RootEpochAfter: exposure.RootEpoch,
	}
	if err := validateBaselineSignedSample(intent, exposure, sample); err != nil {
		t.Fatalf("matching signed sample: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Sample)
	}{
		{name: "column count", mutate: func(value *Sample) { value.ColumnCount++ }},
		{name: "predicate count", mutate: func(value *Sample) { value.PredicateAtomCount++ }},
		{name: "composite count", mutate: func(value *Sample) { value.CompositeCount++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := sample
			test.mutate(&mutated)
			if err := validateBaselineSignedSample(intent, exposure, mutated); err == nil {
				t.Fatal("unsigned sample drift was accepted")
			}
		})
	}
}

func TestRecoveryEvidenceRejectsRequeryAndAcceptsStableRecovery(t *testing.T) {
	sample := validTestSample()
	sample.Mode, sample.IdempotentReplay = "pending_recovery", true
	sample.BusinessSQLDelta = 2
	sample.ReceiptSHA256 = strings.Repeat("a", 64)
	sample.ArtifactIntentSHA256 = strings.Repeat("b", 64)
	root := validRootLedgerSnapshot()
	sample.RootEpochBefore, sample.RootEpochAfter = 0, root.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootLedgerSetSHA256(RootLedgerSnapshot{}), rootLedgerSetSHA256(root)
	sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = root.ReleaseSetSHA256, root.DependencySetSHA256, root.OutcomeSetSHA256
	sample.ActualReleaseFacts, sample.ActualDependencyFacts, sample.ActualOutcomeFacts = root.ReleaseCardinality, root.DependencyCardinality, root.OutcomeCardinality
	sample.ObjectSHA256 = strings.Repeat("8", 64)
	sample.EncryptedObjectBytes = 1087
	exposure := validRecoveryExposure(root)
	sample.RootTaskIDHash = redactedTaskSHA256(sample, exposure.RootTaskID)
	recoveryExposure := recoveryExposureSnapshot(sample, exposure)
	intent := &queryreceipt.ArtifactIntentEvidenceV1{ObjectKeySHA256: strings.Repeat("7", 64), ObjectSHA256: sample.ObjectSHA256, ObjectSize: sample.EncryptedObjectBytes, IntentSHA256: sample.ArtifactIntentSHA256, RegistrationAuditSequence: 8}
	sample.BaselineVerification = &BaselineVerificationEvidence{
		Receipt:       queryreceipt.QueryReceiptV1{AuditSequence: 7, Exposure: &exposure, ArtifactIntent: intent},
		TerminalProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 7}}, RegistrationProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 8}},
		AvailabilityProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 9}},
	}
	beforeBusiness := BusinessSQLSnapshot{StatsResetUnixMicro: 100, VisibleCalls: 5, CompanionCalls: 5}
	failureBusiness := BusinessSQLSnapshot{StatsResetUnixMicro: 100, VisibleCalls: 6, CompanionCalls: 6}
	object := CanonicalObjectSnapshot{Exists: true, ObjectKeySHA256: intent.ObjectKeySHA256, CanonicalCiphertextSHA256: sample.ObjectSHA256, CanonicalCiphertextSize: sample.EncryptedObjectBytes, IntentSHA256: sample.ArtifactIntentSHA256}
	sample.RecoveryVerification = &RecoveryVerificationEvidence{
		FailureObserved: true, CanonicalObjectObserved: true,
		ArtifactStatusBefore: "PENDING", ArtifactStatusAfter: "AVAILABLE",
		BusinessCallsBefore: 10, BusinessCallsAtFailure: 12, BusinessCallsAfter: 12,
		QueryRecordsBefore: 0, QueryRecordsAtFailure: 1, QueryRecordsAfter: 1,
		SettlementsAtFailure: 1, SettlementsAfter: 1,
		UsedQueriesBefore: 0, UsedQueriesAtFailure: 1, UsedQueriesAfter: 1,
		ReceiptSHA256AtFailure: sample.ReceiptSHA256, ReceiptSHA256After: sample.ReceiptSHA256,
		IntentSHA256AtFailure: sample.ArtifactIntentSHA256, IntentSHA256After: sample.ArtifactIntentSHA256,
		BusinessBeforeSnapshot: beforeBusiness, BusinessAtFailureSnapshot: failureBusiness, BusinessAfterSnapshot: failureBusiness,
		RootAtFailure: root, RootAfter: root, ExposureAtFailure: recoveryExposure, ExposureAfter: recoveryExposure,
		ObjectAtFailure: object, ObjectAfter: object,
		SettlementAuditSequencesAtFailure: []int64{7}, SettlementAuditSequencesAfter: []int64{7},
		TerminalAuditsAtFailure: 1, TerminalAuditsAfter: 1, RegistrationAuditsAtFailure: 1, RegistrationAuditsAfter: 1,
		AvailabilityAuditsAtFailure: 0, AvailabilityAuditsAfter: 1,
		TerminalAuditSequenceAtFailure: 7, TerminalAuditSequenceAfter: 7, RegistrationAuditSequenceAtFailure: 8, RegistrationAuditSequenceAfter: 8,
		AvailabilityAuditSequenceAtFailure: 0, AvailabilityAuditSequenceAfter: 9,
	}
	if err := validateRecoveryVerification(sample); err != nil {
		t.Fatal(err)
	}
	original := *sample.RecoveryVerification
	tests := []struct {
		name   string
		mutate func(*RecoveryVerificationEvidence)
	}{
		{name: "Business requery", mutate: func(value *RecoveryVerificationEvidence) { value.BusinessAfterSnapshot.VisibleCalls++ }},
		{name: "counter reset", mutate: func(value *RecoveryVerificationEvidence) { value.BusinessAfterSnapshot.StatsResetUnixMicro++ }},
		{name: "root drift", mutate: func(value *RecoveryVerificationEvidence) { value.RootAfter.Epoch++ }},
		{name: "exposure drift", mutate: func(value *RecoveryVerificationEvidence) {
			value.ExposureAfter.ObservationSHA256 = strings.Repeat("f", 64)
		}},
		{name: "object drift", mutate: func(value *RecoveryVerificationEvidence) { value.ObjectAfter.CanonicalCiphertextSize++ }},
		{name: "settlement sequence", mutate: func(value *RecoveryVerificationEvidence) { value.SettlementAuditSequencesAfter = []int64{8} }},
		{name: "availability count", mutate: func(value *RecoveryVerificationEvidence) { value.AvailabilityAuditsAfter++ }},
		{name: "terminal count", mutate: func(value *RecoveryVerificationEvidence) { value.TerminalAuditsAfter++ }},
		{name: "registration sequence", mutate: func(value *RecoveryVerificationEvidence) { value.RegistrationAuditSequenceAfter++ }},
		{name: "availability sequence", mutate: func(value *RecoveryVerificationEvidence) {
			value.AvailabilityAuditSequenceAfter = value.RegistrationAuditSequenceAfter
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := original
			mutated.SettlementAuditSequencesAtFailure = append([]int64(nil), mutated.SettlementAuditSequencesAtFailure...)
			mutated.SettlementAuditSequencesAfter = append([]int64(nil), mutated.SettlementAuditSequencesAfter...)
			test.mutate(&mutated)
			sample.RecoveryVerification = &mutated
			if err := validateRecoveryVerification(sample); err == nil {
				t.Fatal("mutated recovery evidence was accepted")
			}
		})
	}
	sample.RecoveryVerification = &original
	sample.BusinessSQLDelta = 1
	if err := validateRecoveryVerification(sample); err == nil {
		t.Fatal("recovery accepted a forged top-level Business SQL delta")
	}
	sample.BusinessSQLDelta = 2
	sample.RootSetSHA256After = strings.Repeat("f", 64)
	if err := validateRecoveryVerification(sample); err == nil {
		t.Fatal("recovery accepted a top-level root digest not recomputed from its snapshot")
	}
}

func TestSemanticReplayEvidenceFailsClosedOnIndependentSnapshotDrift(t *testing.T) {
	sample := validTestSample()
	sample.Mode, sample.SemanticReplay = "semantic_replay", true
	root := validRootLedgerSnapshot()
	bindSampleToRoot(&sample, root)
	observation := strings.Repeat("6", 64)
	sample.BaselineVerification = &BaselineVerificationEvidence{Receipt: queryreceipt.QueryReceiptV1{Exposure: &queryreceipt.ExposureEvidenceV1{ObservationSHA256: observation}}}
	business := BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 3, VisibleCalls: 9, CompanionCalls: 9}
	sample.ReplayVerification = &ReplayVerificationEvidence{BusinessBefore: business, BusinessAfter: business, RootBefore: root, RootAfter: root, SourceObservationSHA256: observation, ReplayObservationSHA256: observation}
	if err := validateReplayVerification(sample); err != nil {
		t.Fatal(err)
	}
	original := *sample.ReplayVerification
	tests := []struct {
		name   string
		mutate func(*ReplayVerificationEvidence)
	}{
		{name: "visible SQL", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.VisibleCalls++ }},
		{name: "companion SQL", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.CompanionCalls++ }},
		{name: "stats reset", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.StatsResetUnixMicro++ }},
		{name: "deallocation", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.Dealloc++ }},
		{name: "root digest", mutate: func(value *ReplayVerificationEvidence) { value.RootAfter.ReleaseSetSHA256 = strings.Repeat("e", 64) }},
		{name: "root cardinality", mutate: func(value *ReplayVerificationEvidence) { value.RootAfter.DependencyCardinality++ }},
		{name: "observation", mutate: func(value *ReplayVerificationEvidence) { value.ReplayObservationSHA256 = strings.Repeat("e", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := original
			test.mutate(&mutated)
			sample.ReplayVerification = &mutated
			if err := validateReplayVerification(sample); err == nil {
				t.Fatal("mutated semantic replay evidence was accepted")
			}
		})
	}
	sample.ReplayVerification = &original
	sample.RootSetSHA256Before = strings.Repeat("f", 64)
	if err := validateReplayVerification(sample); err == nil {
		t.Fatal("semantic replay accepted a top-level root digest not recomputed from the snapshot")
	}
}

func TestNovelEvidenceRequiresFreshRootAndIndependentExecution(t *testing.T) {
	sample := validTestSample()
	sample.Mode, sample.BusinessSQLDelta = "novel", 2
	fresh := RootLedgerSnapshot{RootObservationSetSHA256: emptyRootObservationSetSHA256()}
	after := validRootLedgerSnapshot()
	sample.RootEpochBefore, sample.RootEpochAfter = fresh.Epoch, after.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootLedgerSetSHA256(fresh), rootLedgerSetSHA256(after)
	sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = after.ReleaseSetSHA256, after.DependencySetSHA256, after.OutcomeSetSHA256
	sample.ActualReleaseFacts, sample.ActualDependencyFacts, sample.ActualOutcomeFacts = after.ReleaseCardinality, after.DependencyCardinality, after.OutcomeCardinality
	beforeBusiness := BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 9, CompanionCalls: 9}
	afterBusiness := BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 10, CompanionCalls: 10}
	evidence := ReplayVerificationEvidence{BusinessBefore: beforeBusiness, BusinessAfter: afterBusiness, RootBefore: fresh, RootAfter: after}
	sample.ReplayVerification = &evidence
	if err := validateNovelVerification(sample); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*ReplayVerificationEvidence)
	}{
		{name: "visible execution missing", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.VisibleCalls-- }},
		{name: "companion execution missing", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.CompanionCalls-- }},
		{name: "counter reset", mutate: func(value *ReplayVerificationEvidence) { value.BusinessAfter.StatsResetUnixMicro++ }},
		{name: "nonfresh epoch", mutate: func(value *ReplayVerificationEvidence) { value.RootBefore.Epoch = 1 }},
		{name: "nonfresh component", mutate: func(value *ReplayVerificationEvidence) { value.RootBefore.ReleaseSetSHA256 = strings.Repeat("f", 64) }},
		{name: "forged empty observation set", mutate: func(value *ReplayVerificationEvidence) {
			value.RootBefore.RootObservationSetSHA256 = strings.Repeat("f", 64)
		}},
		{name: "root after drift", mutate: func(value *ReplayVerificationEvidence) { value.RootAfter.ReleaseCardinality++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := evidence
			test.mutate(&mutated)
			sample.ReplayVerification = &mutated
			if err := validateNovelVerification(sample); err == nil {
				t.Fatal("mutated novel evidence was accepted")
			}
		})
	}
	sample.ReplayVerification = &evidence
	sample.BusinessSQLDelta = 1
	if err := validateNovelVerification(sample); err == nil {
		t.Fatal("novel request accepted a forged top-level Business SQL delta")
	}
}

func TestIdempotentEvidenceFailsClosedOnControlAndIdentityDrift(t *testing.T) {
	sample := validTestSample()
	sample.Mode, sample.IdempotentReplay = "idempotent_replay", true
	root := validRootLedgerSnapshot()
	bindSampleToRoot(&sample, root)
	intent := &queryreceipt.ArtifactIntentEvidenceV1{ResultID: "result-1", ObjectKeySHA256: strings.Repeat("7", 64), ObjectSHA256: strings.Repeat("8", 64), ObjectSize: 1087, IntentSHA256: strings.Repeat("9", 64)}
	exposure := &queryreceipt.ExposureEvidenceV1{ObservationSHA256: strings.Repeat("6", 64)}
	receipt := queryreceipt.QueryReceiptV1{QueryID: "query-1", ArtifactIntent: intent, Exposure: exposure}
	receiptBytes, _ := json.Marshal(receipt)
	target := TerminalIdentitySnapshot{
		Found: true, QueryIDHash: redactedIdentitySHA256(sample, "query", receipt.QueryID), ResultIDHash: redactedIdentitySHA256(sample, "result", intent.ResultID),
		ReceiptSHA256: sha256Hex(receiptBytes), IntentSHA256: intent.IntentSHA256, ObjectKeySHA256: intent.ObjectKeySHA256,
		CommittedObjectSHA256: intent.ObjectSHA256, CanonicalCiphertextSHA256: intent.ObjectSHA256, CanonicalCiphertextSize: intent.ObjectSize,
		ArtifactStatus: "AVAILABLE", ObservationSHA256: exposure.ObservationSHA256,
	}
	snapshot := IdempotentControlSnapshot{
		Business: BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 9, CompanionCalls: 9}, Root: root,
		QueryRecords: 1, ExposureCharges: 1, Observations: 1, Receipts: 1, Artifacts: 1, AvailableArtifacts: 1,
		TerminalAudits: 1, RegistrationAudits: 1, AvailabilityAudits: 1, CanonicalObjects: 1, Target: target,
	}
	sample.BaselineVerification = &BaselineVerificationEvidence{Receipt: receipt}
	sample.IdempotentVerification = &IdempotentVerificationEvidence{Before: snapshot, After: snapshot, Returned: target}
	if err := validateIdempotentVerification(sample); err != nil {
		t.Fatal(err)
	}
	original := *sample.IdempotentVerification
	tests := []struct {
		name   string
		mutate func(*IdempotentVerificationEvidence)
	}{
		{name: "Business SQL", mutate: func(value *IdempotentVerificationEvidence) { value.After.Business.VisibleCalls++ }},
		{name: "Control count", mutate: func(value *IdempotentVerificationEvidence) { value.After.QueryRecords++ }},
		{name: "root", mutate: func(value *IdempotentVerificationEvidence) { value.After.Root.OutcomeCardinality++ }},
		{name: "audit", mutate: func(value *IdempotentVerificationEvidence) { value.After.AvailabilityAudits++ }},
		{name: "object count", mutate: func(value *IdempotentVerificationEvidence) { value.After.CanonicalObjects++ }},
		{name: "returned identity", mutate: func(value *IdempotentVerificationEvidence) { value.Returned.QueryIDHash = strings.Repeat("e", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := original
			test.mutate(&mutated)
			sample.IdempotentVerification = &mutated
			if err := validateIdempotentVerification(sample); err == nil {
				t.Fatal("mutated idempotent evidence was accepted")
			}
		})
	}
	sample.IdempotentVerification = &original
	sample.RootSetSHA256After = strings.Repeat("f", 64)
	if err := validateIdempotentVerification(sample); err == nil {
		t.Fatal("idempotent replay accepted a top-level root digest not recomputed from the snapshot")
	}
}

func TestCrossBindingNegativeControlRejectsReplayOrIdentityReuse(t *testing.T) {
	sample := validTestSample()
	sample.Mode, sample.SemanticReplay = "semantic_replay", true
	firstRoot := strings.Repeat("1", 64)
	firstObservation := strings.Repeat("a", 64)
	sample.RootTaskIDHash = firstRoot
	sample.BaselineVerification = &BaselineVerificationEvidence{Receipt: queryreceipt.QueryReceiptV1{
		GrantDigest: strings.Repeat("5", 64), SQLFingerprint: strings.Repeat("9", 64), CatalogDigest: strings.Repeat("e", 64),
		SchemaDigest: strings.Repeat("f", 64), DatasourceID: "datasource-1", Exposure: &queryreceipt.ExposureEvidenceV1{ObservationSHA256: firstObservation},
	}}
	manifest := validRedactedVerifierManifest()
	manifest.QueryIDHash = strings.Repeat("4", 64)
	manifest.RootTaskIDHash = strings.Repeat("2", 64)
	manifest.ObservationSHA256 = strings.Repeat("b", 64)
	evidence := CrossBindingVerificationEvidence{
		FirstTaskIDHash: firstRoot, SecondTaskIDHash: strings.Repeat("2", 64), FirstRootTaskIDHash: firstRoot, SecondRootTaskIDHash: strings.Repeat("2", 64),
		FirstQueryIDHash: strings.Repeat("3", 64), SecondQueryIDHash: strings.Repeat("4", 64), FirstGrantSHA256: strings.Repeat("5", 64), SecondGrantSHA256: strings.Repeat("6", 64),
		FirstCacheKeySHA256: strings.Repeat("7", 64), SecondCacheKeySHA256: strings.Repeat("8", 64), FirstObservationSHA256: firstObservation, SecondObservationSHA256: strings.Repeat("b", 64),
		FirstSQLFingerprintSHA256: strings.Repeat("9", 64), SecondSQLFingerprintSHA256: strings.Repeat("9", 64), FirstCatalogSHA256: strings.Repeat("e", 64), SecondCatalogSHA256: strings.Repeat("e", 64),
		FirstSchemaSHA256: strings.Repeat("f", 64), SecondSchemaSHA256: strings.Repeat("f", 64), FirstDatasourceIDHash: redactedIdentitySHA256(sample, "datasource", "datasource-1"), SecondDatasourceIDHash: redactedIdentitySHA256(sample, "datasource", "datasource-1"),
		FirstObservationBindingSHA256: rootObservationBindingSHA256(firstRoot, firstObservation), SecondObservationBindingSHA256: rootObservationBindingSHA256(strings.Repeat("2", 64), strings.Repeat("b", 64)),
		FirstSourceQueryIDHash: strings.Repeat("3", 64), SecondSourceQueryIDHash: strings.Repeat("4", 64), SecondRootFirstQueryIDHash: strings.Repeat("4", 64),
		BusinessBefore:   BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 9, CompanionCalls: 9},
		BusinessAfter:    BusinessSQLSnapshot{StatsResetUnixMicro: 100, Dealloc: 2, VisibleCalls: 10, CompanionCalls: 10},
		SettlementAudits: 1, VerifierManifest: &manifest,
	}
	sample.CrossBindingVerification = &evidence
	if err := validateCrossBindingVerification(sample); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*CrossBindingVerificationEvidence)
	}{
		{name: "root reuse", mutate: func(value *CrossBindingVerificationEvidence) {
			value.SecondRootTaskIDHash, value.SecondTaskIDHash = value.FirstRootTaskIDHash, value.FirstTaskIDHash
		}},
		{name: "grant reuse", mutate: func(value *CrossBindingVerificationEvidence) { value.SecondGrantSHA256 = value.FirstGrantSHA256 }},
		{name: "cache reuse", mutate: func(value *CrossBindingVerificationEvidence) { value.SecondCacheKeySHA256 = value.FirstCacheKeySHA256 }},
		{name: "SQL drift", mutate: func(value *CrossBindingVerificationEvidence) {
			value.SecondSQLFingerprintSHA256 = strings.Repeat("a", 64)
		}},
		{name: "product drift", mutate: func(value *CrossBindingVerificationEvidence) { value.SecondDatasourceIDHash = strings.Repeat("a", 64) }},
		{name: "signed SQL mismatch", mutate: func(value *CrossBindingVerificationEvidence) {
			value.FirstSQLFingerprintSHA256, value.SecondSQLFingerprintSHA256 = strings.Repeat("a", 64), strings.Repeat("a", 64)
		}},
		{name: "signed datasource mismatch", mutate: func(value *CrossBindingVerificationEvidence) {
			value.FirstDatasourceIDHash, value.SecondDatasourceIDHash = strings.Repeat("a", 64), strings.Repeat("a", 64)
		}},
		{name: "observation binding reuse", mutate: func(value *CrossBindingVerificationEvidence) {
			value.SecondObservationBindingSHA256 = value.FirstObservationBindingSHA256
		}},
		{name: "unrecomputable observation binding", mutate: func(value *CrossBindingVerificationEvidence) {
			value.SecondObservationBindingSHA256 = strings.Repeat("f", 64)
		}},
		{name: "SQL suppression", mutate: func(value *CrossBindingVerificationEvidence) { value.BusinessAfter.CompanionCalls-- }},
		{name: "semantic replay audit", mutate: func(value *CrossBindingVerificationEvidence) { value.SemanticReplayAudits = 1 }},
		{name: "not first query", mutate: func(value *CrossBindingVerificationEvidence) {
			value.SecondRootFirstQueryIDHash = strings.Repeat("3", 64)
		}},
		{name: "verifier fail", mutate: func(value *CrossBindingVerificationEvidence) {
			copyManifest := *value.VerifierManifest
			copyManifest.VerificationResult = "fail"
			value.VerifierManifest = &copyManifest
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := evidence
			copyManifest := *evidence.VerifierManifest
			mutated.VerifierManifest = &copyManifest
			test.mutate(&mutated)
			sample.CrossBindingVerification = &mutated
			if err := validateCrossBindingVerification(sample); err == nil {
				t.Fatal("mutated cross-binding evidence was accepted")
			}
		})
	}
}

func TestRedactedVerifierManifestCrossBindsHashesSizesAndAuditSequence(t *testing.T) {
	sample := validTestSample()
	sample.RootTaskIDHash = redactedTaskSHA256(sample, "task-1")
	sample.ArtifactSHA256, sample.ObjectSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
	sample.ArtifactIntentSHA256, sample.ReceiptSHA256 = strings.Repeat("c", 64), strings.Repeat("d", 64)
	sample.ParquetBytes, sample.EncryptedObjectBytes = 1039, 1087
	exposure := &queryreceipt.ExposureEvidenceV1{
		RootTaskID: "task-1", ObservationSHA256: strings.Repeat("e", 64), ReleaseSetSHA256: strings.Repeat("1", 64),
		InfluenceSetSHA256: strings.Repeat("2", 64), OutcomeSetSHA256: strings.Repeat("3", 64),
	}
	intent := &queryreceipt.ArtifactIntentEvidenceV1{
		ResultID: "result-1", ParquetSHA256: sample.ArtifactSHA256, ObjectSHA256: sample.ObjectSHA256,
		ParquetSize: sample.ParquetBytes, ObjectSize: sample.EncryptedObjectBytes, SchemaSHA256: strings.Repeat("4", 64),
		ObjectKeySHA256: strings.Repeat("5", 64), IntentSHA256: sample.ArtifactIntentSHA256, RegistrationAuditSequence: 8,
	}
	receipt := queryreceipt.QueryReceiptV1{QueryID: "query-1", AuditSequence: 7, Exposure: exposure, ArtifactIntent: intent}
	receiptBytes, _ := json.Marshal(receipt)
	sample.ReceiptSHA256 = sha256Hex(receiptBytes)
	manifest := RedactedVerifierManifest{
		VerifierVersion: redactedVerifierVersion, QueryIDHash: redactedIdentitySHA256(sample, "query", receipt.QueryID), ResultIDHash: redactedIdentitySHA256(sample, "result", intent.ResultID),
		RootTaskIDHash: sample.RootTaskIDHash, ReceiptSHA256: sample.ReceiptSHA256, ObservationSHA256: exposure.ObservationSHA256,
		ReleaseSetSHA256: exposure.ReleaseSetSHA256, DependencySetSHA256: exposure.InfluenceSetSHA256, OutcomeSetSHA256: exposure.OutcomeSetSHA256,
		ArtifactIntentSHA256: intent.IntentSHA256, ObjectKeySHA256: intent.ObjectKeySHA256, CanonicalCiphertextSHA256: intent.ObjectSHA256,
		CanonicalCiphertextSize: intent.ObjectSize, ReleasedParquetSHA256: intent.ParquetSHA256, ReleasedParquetSize: intent.ParquetSize,
		SchemaSHA256: intent.SchemaSHA256, TerminalAuditSequence: 7, RegistrationAuditSequence: 8, AvailabilityAuditSequence: 9, VerificationResult: "pass",
	}
	sample.BaselineVerification = &BaselineVerificationEvidence{
		Receipt: receipt, TerminalProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 7}},
		RegistrationProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 8}}, AvailabilityProof: auditchain.InclusionProof{TerminalEvent: auditchain.Event{Sequence: 9}},
		VerifierManifest: &manifest,
	}
	if err := validateRedactedVerifierManifest(sample); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*RedactedVerifierManifest)
	}{
		{name: "receipt", mutate: func(value *RedactedVerifierManifest) { value.ReceiptSHA256 = strings.Repeat("f", 64) }},
		{name: "object hash", mutate: func(value *RedactedVerifierManifest) { value.CanonicalCiphertextSHA256 = strings.Repeat("f", 64) }},
		{name: "object size", mutate: func(value *RedactedVerifierManifest) { value.CanonicalCiphertextSize++ }},
		{name: "Parquet", mutate: func(value *RedactedVerifierManifest) { value.ReleasedParquetSHA256 = strings.Repeat("f", 64) }},
		{name: "schema", mutate: func(value *RedactedVerifierManifest) { value.SchemaSHA256 = strings.Repeat("f", 64) }},
		{name: "availability sequence", mutate: func(value *RedactedVerifierManifest) {
			value.AvailabilityAuditSequence = value.RegistrationAuditSequence
		}},
		{name: "result", mutate: func(value *RedactedVerifierManifest) { value.VerificationResult = "fail" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := manifest
			test.mutate(&mutated)
			sample.BaselineVerification.VerifierManifest = &mutated
			if err := validateRedactedVerifierManifest(sample); err == nil {
				t.Fatal("mutated verifier manifest was accepted")
			}
		})
	}
}

func TestExperimentEvidenceRejectsRecomputedOverlapAndMissingConcurrencyProof(t *testing.T) {
	overlap := validTestSample()
	overlap.ExperimentID = "scale"
	overlap.WorkloadID = "dependency-e2e"
	overlap.Scale = "100k-overlap-50"
	overlap.ResultSHA256 = strings.Repeat("a", 64)
	overlap.ActualDependencyFacts = 100
	overlap.ChargedDependencyFacts = 60
	if reasons, failed := validateExperimentEvidence(overlap); !failed || len(reasons) == 0 {
		t.Fatal("mislabeled overlap passed final evidence validation")
	}
	concurrency := validTestSample()
	concurrency.ExperimentID = "concurrency"
	concurrency.Mode = "natural_contention"
	concurrency.Scale = "500"
	concurrency.ResultSHA256 = strings.Repeat("b", 64)
	concurrency.Counters = map[string]int64{"cas_attempts": 1, "cas_conflicts": 0, "cas_retries": 0, "barrier_clients": 499, "offered_concurrency_observed": 0}
	if reasons, failed := validateExperimentEvidence(concurrency); !failed || len(reasons) == 0 {
		t.Fatal("unobserved width-500 concurrency passed final evidence validation")
	}
}

func TestPublicationFinalizerSealsCompleteEvidenceAndRejectsRootReuse(t *testing.T) {
	passing := buildPublicationEvidence(t, false)
	summary, err := FinalizeRun(passing)
	if err != nil || summary.Status != "pass" || !summary.PublicationEligible {
		t.Fatalf("passing summary=%+v err=%v", summary, err)
	}
	if _, err := SealRun(passing); err != nil {
		t.Fatal(err)
	}
	failing := buildPublicationEvidence(t, true)
	summary, err = FinalizeRun(failing)
	if err != nil || summary.Status != "fail" || summary.PublicationEligible {
		t.Fatalf("mutated summary=%+v err=%v", summary, err)
	}
	if _, err := SealRun(failing); err == nil {
		t.Fatal("mutated publication evidence was sealable")
	}
}

func buildPublicationEvidence(t *testing.T, reuseRoot bool) string {
	t.Helper()
	runDir := t.TempDir()
	for _, directory := range []string{"environment", "deployments", "raw"} {
		if err := os.MkdirAll(filepath.Join(runDir, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	commit := "0123456789abcdef0123456789abcdef01234567"
	config := Config{SchemaVersion: 1, CampaignClass: "publication", CampaignID: "publication-test", ExperimentID: "rls", SubmissionCommit: commit, Deployments: 3, Samples: 3, RandomSeed: 20260801, FreshRootPerSample: true, Workloads: []Workload{{ID: "trace", Scales: []string{"100"}, Modes: []string{"rls", "unlimited", "bounded"}}}}
	bindTestProtocol(t, &config)
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "adapter.sha256"), []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	oracleTrace := make([]finalv5oracle.Observation, 100)
	for index := range oracleTrace {
		oracleTrace[index] = finalv5oracle.Observation{Release: []string{strings.Repeat("1", 64)}, Dependency: []string{strings.Repeat("2", 64)}, Outcome: []string{strings.Repeat("3", 64)}}
	}
	oracleResult, err := finalv5oracle.Evaluate(oracleTrace)
	if err != nil {
		t.Fatal(err)
	}
	policiesJSON := json.RawMessage(`[{"policyname":"department_scope"}]`)
	windowsHostPath := filepath.Join(runDir, "environment", "windows-host.json")
	if err := os.WriteFile(windowsHostPath, []byte("{\"host\":\"test\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	windowsHostSHA, _ := FileSHA256(windowsHostPath)
	for deployment := 1; deployment <= 3; deployment++ {
		deploymentID := fmt.Sprintf("deployment-%02d", deployment)
		environment := EnvironmentManifest{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: deploymentID, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), GitCommit: commit, GitStatus: []string{}, PublicationEligible: true, Host: map[string]any{"os": "test"}, Software: map[string]any{"go": "test"}, Storage: map[string]any{"fs": "test"}, Datasets: map[string]any{"dataset_sha256": strings.Repeat("c", 64), "catalog_sha256": strings.Repeat("d", 64)}}
		environmentPath := filepath.Join(runDir, "environment", deploymentID+".json")
		if err := WriteEnvironment(environmentPath, environment); err != nil {
			t.Fatal(err)
		}
		beforePath := filepath.Join(runDir, "environment", deploymentID+".vmstat-before.txt")
		afterPath := filepath.Join(runDir, "environment", deploymentID+".vmstat-after.txt")
		if err := os.WriteFile(beforePath, []byte("pswpin 0\npswpout 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(afterPath, []byte("pswpin 0\npswpout 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		environmentSHA, _ := FileSHA256(environmentPath)
		beforeSHA, _ := FileSHA256(beforePath)
		afterSHA, _ := FileSHA256(afterPath)
		inspectPath := filepath.Join(runDir, "environment", deploymentID+".fresh.volume-inspect.json")
		volumes := make([]DockerVolumeProof, 5)
		inspectObjects := make([]map[string]any, 5)
		var volumeSetLines []string
		for index := range volumes {
			name, createdAt := fmt.Sprintf("volume-%d-%d", deployment, index), UTCNow()
			object := map[string]any{"Name": name, "CreatedAt": createdAt, "Driver": "local", "Labels": map[string]any{"deployment": deploymentID}}
			canonical, _ := json.Marshal(object)
			volumes[index] = DockerVolumeProof{Name: name, CreatedAt: createdAt, Driver: "local", InspectSHA256: sha256Hex(canonical)}
			inspectObjects[index] = object
			volumeSetLines = append(volumeSetLines, strings.Join([]string{name, createdAt, "local", volumes[index].InspectSHA256}, "\t")+"\n")
		}
		inspectBytes, _ := json.MarshalIndent(inspectObjects, "", "  ")
		if err := os.WriteFile(inspectPath, append(inspectBytes, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		inspectSHA, _ := FileSHA256(inspectPath)
		composePath := filepath.Join(runDir, "environment", deploymentID+".fresh.compose-config.yaml")
		if err := os.WriteFile(composePath, []byte("name: "+deploymentID+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		composeSHA, _ := FileSHA256(composePath)
		datasetPath := filepath.Join(runDir, "environment", deploymentID+".fresh.dataset-fingerprint.txt")
		if err := os.WriteFile(datasetPath, []byte("dataset-"+deploymentID), 0o600); err != nil {
			t.Fatal(err)
		}
		datasetSHA, _ := FileSHA256(datasetPath)
		sort.Strings(volumeSetLines)
		proof := FreshDeploymentProof{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: deploymentID, CapturedAt: UTCNow(), ComposeProjectName: fmt.Sprintf("project-%d", deployment), ComposeConfigSHA256: composeSHA, Volumes: volumes, VolumeSetSHA256: sha256Hex([]byte(strings.Join(volumeSetLines, ""))), VolumeInspectSHA256: inspectSHA, ControlPGSystemIdentifier: fmt.Sprintf("control-%d", deployment), BusinessPGSystemIdentifier: fmt.Sprintf("business-%d", deployment), ControlInitialCounts: map[string]int64{"tasks": 0, "query_records": 0, "root_heads": 0, "result_artifacts": 0}, DatasetFingerprintSHA256: datasetSHA, SnapshotArtifactVolumeSHA256: strings.Repeat("9", 64)}
		proofBytes, _ := json.Marshal(proof)
		proofPath := filepath.Join(runDir, "environment", deploymentID+".fresh.json")
		if err := os.WriteFile(proofPath, proofBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		proofSHA, _ := FileSHA256(proofPath)
		manifest := DeploymentManifest{SchemaVersion: 1, CampaignID: config.CampaignID, DeploymentID: deploymentID, FreshDeployment: true, FreshDeploymentProofSHA256: proofSHA, EnvironmentSHA256: environmentSHA, WindowsEnvironmentSHA256: windowsHostSHA, VMStatBeforeSHA256: beforeSHA, VMStatAfterSHA256: afterSHA, StartedAt: UTCNow(), FinishedAt: UTCNow()}
		if err := WriteDeployment(filepath.Join(runDir, "deployments", deploymentID+".json"), manifest); err != nil {
			t.Fatal(err)
		}
		writer, err := NewJSONLWriter(filepath.Join(runDir, "raw", deploymentID+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		position := 0
		for iteration := 1; iteration <= 3; iteration++ {
			for _, mode := range config.Workloads[0].Modes {
				position++
				sample := validTestSample()
				sample.CampaignID, sample.DeploymentID, sample.ExperimentID = config.CampaignID, deploymentID, "rls"
				sample.CellID, sample.SampleID, sample.Iteration, sample.OrderPosition, sample.RandomSeed = "trace/100/"+mode, fmt.Sprintf("%s-%d", deploymentID, position), iteration, position, config.RandomSeed
				sample.PairID = fmt.Sprintf("%s-pair-%d", deploymentID, iteration)
				sample.RootGroupID = mode
				sample.Mode, sample.WorkloadID, sample.Scale, sample.ResultSHA256, sample.PublicationEligible = mode, "trace", "100", digest, true
				sample.RLSVerification = &RLSVerificationEvidence{RelRowSecurity: true, BaselineRole: "rls_reader", TableOwnerRole: "postgres", PoliciesJSON: policiesJSON, PoliciesSHA256: sha256Hex(policiesJSON), OracleComputedBefore: true, OracleTrace: oracleTrace, OracleResult: oracleResult}
				if mode == "rls" {
					sample.System = "postgresql"
				} else {
					sample.System = "taskgate"
					rootIdentity := fmt.Sprintf("%s-%s-%d", deploymentID, mode, iteration)
					if reuseRoot && deployment == 1 && iteration == 2 && mode == "unlimited" {
						rootIdentity = fmt.Sprintf("%s-%s-%d", deploymentID, mode, 1)
					}
					rootBytes := sha256.Sum256([]byte(rootIdentity))
					sample.RootTaskIDHash = hex.EncodeToString(rootBytes[:])
					if mode == "bounded" {
						sample.RLSVerification.StopReason = "EXPOSURE_BUDGET_EXHAUSTED"
						sample.Rejected, sample.RejectedNoResult, sample.RejectedNoArtifact, sample.RejectedNoSuccessfulAudit = true, true, true, true
					} else {
						sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = oracleResult.Release.SetSHA256, oracleResult.Dependency.SetSHA256, oracleResult.Outcome.SetSHA256
						sample.ReceiptVersion, sample.ReceiptSHA256, sample.ArtifactIntentSHA256, sample.AvailabilityAuditSHA256 = "8", digest, digest, digest
						sample.ReceiptVerified, sample.ArtifactAvailable = true, true
						sample.ArtifactSHA256, sample.ObjectSHA256 = digest, digest
					}
				}
				traceSteps := 100
				if mode == "bounded" {
					traceSteps = 2
				}
				for step := 1; step <= traceSteps; step++ {
					trace := TraceStep{Index: step, ConcreteSQL: fmt.Sprintf("SELECT %d", step), PriorStateSHA256: digest, ResultSHA256: digest, NextSQLSHA256: digest}
					if sample.System == "taskgate" {
						trace.PlanSHA256, trace.ObservationSHA256 = digest, digest
						trace.ReleaseSetSHA256, trace.DependencySetSHA256, trace.OutcomeSetSHA256 = digest, digest, digest
					}
					if mode == "bounded" && step == traceSteps {
						trace.Rejected, trace.NoResult, trace.NoAvailableArtifact, trace.NoSuccessfulAudit = true, true, true, true
					}
					sample.Trace = append(sample.Trace, trace)
				}
				if err := writer.Write(sample); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return runDir
}

func validTestSample() Sample {
	return Sample{SchemaVersion: 1, CampaignID: "c", DeploymentID: "deployment-01", ExperimentID: "baseline", CellID: "S1/tiny/novel", SampleID: "s1", Iteration: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair-1", PairedSystemOrder: "unpaired", RootGroupID: "novel", System: "taskgate", Mode: "novel", WorkloadID: "S1", Scale: "tiny", PipelineMS: map[string]float64{"prepare": 1, "execute_and_derive": 1, "artifact_stage": 1, "control_settlement": 1, "artifact_publication": 1, "response_finalize": 1, "server_total": 7}, DiagnosticMS: map[string]float64{}, Status: "pass", PublicationEligible: false}
}

func validRootLedgerSnapshot() RootLedgerSnapshot {
	return RootLedgerSnapshot{
		Epoch: 1, DictionarySetSHA256: strings.Repeat("0", 64), ReleaseSetSHA256: strings.Repeat("1", 64), ReleaseCardinality: 3,
		DependencySetSHA256: strings.Repeat("2", 64), DependencyCardinality: 4, OutcomeSetSHA256: strings.Repeat("3", 64), OutcomeCardinality: 2,
		RootObservationSetSHA256: strings.Repeat("4", 64), RootObservationCount: 1,
	}
}

func bindSampleToRoot(sample *Sample, root RootLedgerSnapshot) {
	sample.RootEpochBefore, sample.RootEpochAfter = root.Epoch, root.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootLedgerSetSHA256(root), rootLedgerSetSHA256(root)
	sample.ReleaseSetSHA256, sample.DependencySetSHA256, sample.OutcomeSetSHA256 = root.ReleaseSetSHA256, root.DependencySetSHA256, root.OutcomeSetSHA256
	sample.ActualReleaseFacts, sample.ActualDependencyFacts, sample.ActualOutcomeFacts = root.ReleaseCardinality, root.DependencyCardinality, root.OutcomeCardinality
}

func validRecoveryExposure(root RootLedgerSnapshot) queryreceipt.ExposureEvidenceV1 {
	return queryreceipt.ExposureEvidenceV1{
		RootTaskID: "task-1", ProfileVersion: "taskgate-exposure-v5", ActualReleaseFacts: root.ReleaseCardinality,
		ActualInfluenceFacts: root.DependencyCardinality, ActualOutcomeFacts: root.OutcomeCardinality,
		ChargedReleaseFacts: root.ReleaseCardinality, ChargedInfluenceFacts: root.DependencyCardinality, ChargedOutcomeFacts: root.OutcomeCardinality,
		ObservationSHA256: strings.Repeat("6", 64), DictionarySetSHA256: root.DictionarySetSHA256, ReleaseSetSHA256: root.ReleaseSetSHA256,
		InfluenceSetSHA256: root.DependencySetSHA256, OutcomeSetSHA256: root.OutcomeSetSHA256, RootEpoch: root.Epoch,
		PredicateProfileVersion: "taskgate-predicate-footprint-v1", PredicateContextSHA256: strings.Repeat("7", 64), PredicateSetSHA256: strings.Repeat("8", 64),
		ActualPredicateAtomCount: 1, ChargedPredicateAtomCount: 1, CompositeOutcomeSHA256: strings.Repeat("9", 64), ActualCompositeCount: 1, ChargedCompositeCount: 1,
	}
}

func validRedactedVerifierManifest() RedactedVerifierManifest {
	return RedactedVerifierManifest{
		VerifierVersion: redactedVerifierVersion, QueryIDHash: strings.Repeat("1", 64), ResultIDHash: strings.Repeat("2", 64), RootTaskIDHash: strings.Repeat("3", 64),
		ReceiptSHA256: strings.Repeat("4", 64), ObservationSHA256: strings.Repeat("5", 64), ReleaseSetSHA256: strings.Repeat("6", 64), DependencySetSHA256: strings.Repeat("7", 64),
		OutcomeSetSHA256: strings.Repeat("8", 64), ArtifactIntentSHA256: strings.Repeat("9", 64), ObjectKeySHA256: strings.Repeat("a", 64),
		CanonicalCiphertextSHA256: strings.Repeat("b", 64), CanonicalCiphertextSize: 1087, ReleasedParquetSHA256: strings.Repeat("c", 64), ReleasedParquetSize: 1039,
		SchemaSHA256: strings.Repeat("d", 64), TerminalAuditSequence: 7, RegistrationAuditSequence: 8, AvailabilityAuditSequence: 9, VerificationResult: "pass",
	}
}

func bindTestProtocol(t *testing.T, config *Config) {
	t.Helper()
	root := t.TempDir()
	config.ProtocolVersion = finalProtocolVersion
	config.ProtocolProfile = "test"
	workloads, err := json.Marshal(map[string]any{
		"schema_version": 2,
		"profiles":       map[string]any{"test": map[string]any{"workloads": config.Workloads}},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"protocol-v1.yaml":         []byte("schema_version: 1\nprotocol_id: taskgate-final-v5-wsl2-v1\n"),
		"workloads-v1.yaml":        append(workloads, '\n'),
		"acceptance-rules-v1.yaml": []byte("schema_version: 1\n"),
		"statistics-v1.yaml":       []byte("schema_version: 1\n"),
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(root, name), value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config.ProtocolSHA256, _ = FileSHA256(filepath.Join(root, "protocol-v1.yaml"))
	config.WorkloadSHA256, _ = FileSHA256(filepath.Join(root, "workloads-v1.yaml"))
	config.AcceptanceSHA256, _ = FileSHA256(filepath.Join(root, "acceptance-rules-v1.yaml"))
	config.StatisticsSHA256, _ = FileSHA256(filepath.Join(root, "statistics-v1.yaml"))
	prior := protocolRootOverride
	protocolRootOverride = root
	t.Cleanup(func() { protocolRootOverride = prior })
}
