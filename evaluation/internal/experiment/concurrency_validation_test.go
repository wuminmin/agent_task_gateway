package experiment

import (
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/internal/control"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

func TestConcurrencyVerificationAcceptsEveryFrozenCell(t *testing.T) {
	for _, cell := range concurrencyfixture.FrozenCells {
		cell := cell
		t.Run(cell.WorkloadID+"/"+cell.Scale+"/"+cell.Mode, func(t *testing.T) {
			sample := validConcurrencyValidationSample(t, cell)
			if err := ValidateConcurrencyEvidence(sample); err != nil {
				t.Fatal(err)
			}
			reasons, failed := validateExperimentEvidence(sample)
			if failed {
				t.Fatalf("campaign validation rejected valid evidence: %v", reasons)
			}
		})
	}
}

func TestConcurrencyExpectedResultUsesPublicCanonicalHasher(t *testing.T) {
	digest, err := CanonicalResultHash(concurrencyfixture.ExpectedContenderRows())
	if err != nil {
		t.Fatal(err)
	}
	if digest != concurrencyfixture.ExpectedContenderResultSHA256() {
		t.Fatalf("fixture result hash %s differs from public canonical hash %s", concurrencyfixture.ExpectedContenderResultSHA256(), digest)
	}
}

func TestConcurrencyVerificationRejectsEvidenceMutations(t *testing.T) {
	natural, _ := concurrencyfixture.Lookup("shared-root", "10", "natural_contention")
	mutations := []struct {
		name   string
		mutate func(*Sample)
	}{
		{name: "client-only offered width", mutate: func(value *Sample) {
			value.ConcurrencyVerification.ServiceArrivals--
		}},
		{name: "counter substitutes for service telemetry", mutate: func(value *Sample) {
			value.Counters["service_clients_observed"]--
		}},
		{name: "lock waiter substitutes for natural CAS", mutate: func(value *Sample) {
			e := value.ConcurrencyVerification
			e.RootLockWaitersObserved = 1
			e.ProductionCASAttempts, e.ProductionCASConflicts, e.ProductionCASRetries = 1, 0, 0
			e.NaturalCASAttempts, e.NaturalCASConflicts, e.NaturalCASRetries = 1, 0, 0
			value.Counters["forced_queue_waiters"] = 1
			value.Counters["cas_attempts"], value.Counters["cas_conflicts"], value.Counters["cas_retries"] = 1, 0, 0
			for index := range e.Contenders {
				e.Contenders[index].CASAttempts, e.Contenders[index].CASConflicts, e.Contenders[index].CASRetries = 0, 0, 0
			}
			e.Contenders[0].CASAttempts = 1
		}},
		{name: "natural arm has no conflict", mutate: func(value *Sample) {
			e := value.ConcurrencyVerification
			e.ProductionCASAttempts, e.ProductionCASConflicts, e.ProductionCASRetries = 1, 0, 0
			e.NaturalCASAttempts, e.NaturalCASConflicts, e.NaturalCASRetries = 1, 0, 0
			value.Counters["cas_attempts"], value.Counters["cas_conflicts"], value.Counters["cas_retries"] = 1, 0, 0
			for index := range e.Contenders {
				e.Contenders[index].CASAttempts, e.Contenders[index].CASConflicts, e.Contenders[index].CASRetries = 0, 0, 0
			}
			e.Contenders[0].CASAttempts = 1
		}},
		{name: "missing contender", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders = value.ConcurrencyVerification.Contenders[1:]
		}},
		{name: "missing verifier manifest", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].VerifierManifest = nil
		}},
		{name: "missing availability verdict", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].ArtifactAvailable = false
		}},
		{name: "missing availability digest", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].AvailabilityAuditSHA256 = ""
		}},
		{name: "missing artifact intention", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].ArtifactIntentSHA256 = ""
		}},
		{name: "wrong canonical result", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].ResultSHA256 = digestForConcurrencyTest("wrong-result")
		}},
		{name: "different task", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].TaskIDHash = digestForConcurrencyTest("other-task")
		}},
		{name: "different root", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].RootTaskIDHash = digestForConcurrencyTest("other-root")
		}},
		{name: "raw baseline receipt retained", mutate: func(value *Sample) {
			value.BaselineVerification = &BaselineVerificationEvidence{}
		}},
		{name: "duplicate request", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Contenders[3].RequestIDHash = value.ConcurrencyVerification.Contenders[2].RequestIDHash
		}},
		{name: "mutated B plus one root", mutate: func(value *Sample) {
			value.ConcurrencyVerification.AfterRejectedOverflow.Epoch++
		}},
		{name: "overflow leaked result", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Overflow.ResultSHA256 = digestForConcurrencyTest("leaked")
		}},
		{name: "overflow lost release audit", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Overflow.ReleaseAudits = 0
		}},
		{name: "overflow lost failure receipt", mutate: func(value *Sample) {
			value.ConcurrencyVerification.Overflow.Receipts = 0
		}},
		{name: "final member omitted", mutate: func(value *Sample) {
			value.ConcurrencyVerification.FinalRootFactHashes = value.ConcurrencyVerification.FinalRootFactHashes[1:]
		}},
		{name: "persisted final root relabeled", mutate: func(value *Sample) {
			value.ConcurrencyVerification.FinalRootSetSHA256 = digestForConcurrencyTest("other-root-set")
		}},
	}
	for _, mutation := range mutations {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			value := validConcurrencyValidationSample(t, natural)
			mutation.mutate(&value)
			if err := ValidateConcurrencyEvidence(value); err == nil {
				t.Fatal("mutated concurrency evidence was accepted")
			}
		})
	}
}

func TestConcurrencyForcedWaiterIsSeparateFromProductionCAS(t *testing.T) {
	forced, _ := concurrencyfixture.Lookup("shared-root", "10", "forced_queue_safety")
	sample := validConcurrencyValidationSample(t, forced)
	if err := ValidateConcurrencyEvidence(sample); err != nil {
		t.Fatal(err)
	}
	sample.ConcurrencyVerification.NaturalCASAttempts = sample.ConcurrencyVerification.ProductionCASAttempts
	if err := ValidateConcurrencyEvidence(sample); err == nil {
		t.Fatal("forced-lock waiter arm accepted fabricated natural-CAS evidence")
	}
}

func validConcurrencyValidationSample(t *testing.T, cell concurrencyfixture.Cell) Sample {
	t.Helper()
	operation := concurrencyfixture.RoundIdentity{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID:   cell.WorkloadID + "/" + cell.Scale + "/" + cell.Mode,
		SampleID: "sample-" + cell.Scale + "-" + cell.Mode, Iteration: 1, ProcessReplicate: 1,
		PairID: "pair-01", RootGroupID: "root-group-01",
	}
	roundSHA := concurrencyfixture.RoundSHA256(operation)
	rootTaskIDHash := digestForConcurrencyTest("one-root-task")
	initial := RootLedgerSnapshot{RootObservationSetSHA256: emptyRootObservationSetSHA256()}
	before := concurrencyRootSnapshot(3, 4, 3, digestForConcurrencyTest("before-outcome-set"))
	members := []string{
		digestForConcurrencyTest("predicate-atom"), digestForConcurrencyTest("prefix-one"),
		digestForConcurrencyTest("prefix-two"), digestForConcurrencyTest("prefix-three"),
		digestForConcurrencyTest("contender-composite"),
	}
	set, err := control.BuildOutcomeHashSetV5(members)
	if err != nil {
		t.Fatal(err)
	}
	atBoundary := concurrencyRootSnapshot(4, 5, 4, set.Set.SetSHA256)
	expectedResult := concurrencyfixture.ExpectedContenderResultSHA256()
	observation := digestForConcurrencyTest("same-observation")
	predicateSet := digestForConcurrencyTest("same-predicate-set")
	composite := members[len(members)-1]
	contenders := make([]ConcurrencyContenderEvidence, cell.Width)
	requestDigests := make([]string, cell.Width)
	for index := range contenders {
		queryDigest := digestForConcurrencyTest("query-" + strconv.Itoa(index+1))
		resultIDDigest := digestForConcurrencyTest("result-id-" + strconv.Itoa(index+1))
		requestDigests[index] = digestForConcurrencyTest("request-" + strconv.Itoa(index+1))
		receiptDigest := digestForConcurrencyTest("receipt-" + strconv.Itoa(index+1))
		intentDigest := digestForConcurrencyTest("intent-" + strconv.Itoa(index+1))
		manifest := &RedactedVerifierManifest{
			VerifierVersion: "taskgate-final-v5-composite-verifier-v1",
			QueryIDHash:     queryDigest, ResultIDHash: resultIDDigest, RootTaskIDHash: rootTaskIDHash,
			ReceiptSHA256: receiptDigest, ObservationSHA256: observation,
			ReleaseSetSHA256:    atBoundary.ReleaseSetSHA256,
			DependencySetSHA256: atBoundary.DependencySetSHA256,
			OutcomeSetSHA256:    atBoundary.OutcomeSetSHA256, ArtifactIntentSHA256: intentDigest,
			ObjectKeySHA256:           digestForConcurrencyTest("object-key-" + strconv.Itoa(index+1)),
			CanonicalCiphertextSHA256: digestForConcurrencyTest("ciphertext-" + strconv.Itoa(index+1)),
			CanonicalCiphertextSize:   200 + int64(index),
			ReleasedParquetSHA256:     digestForConcurrencyTest("parquet-" + strconv.Itoa(index+1)),
			ReleasedParquetSize:       100 + int64(index), SchemaSHA256: digestForConcurrencyTest("schema"),
			TerminalAuditSequence: int64(10 + index*3), RegistrationAuditSequence: int64(11 + index*3),
			AvailabilityAuditSequence: int64(12 + index*3), VerificationResult: "pass",
		}
		contenders[index] = ConcurrencyContenderEvidence{
			Index: index + 1, ParticipantSHA256: concurrencyfixture.ParticipantSHA256(roundSHA, index+1),
			TaskIDHash: rootTaskIDHash, RootTaskIDHash: rootTaskIDHash, RequestIDHash: requestDigests[index],
			QueryIDHash: queryDigest, ResultIDHash: resultIDDigest, ResultSHA256: expectedResult,
			ObservationSHA256: observation, CompositeOutcomeSHA256: composite, PredicateSetSHA256: predicateSet,
			RootEpoch: 4, ActualOutcomeFacts: 2, ReceiptVersion: "8", ReceiptSHA256: receiptDigest,
			ArtifactIntentSHA256:    intentDigest,
			AvailabilityAuditSHA256: digestForConcurrencyTest("availability-" + strconv.Itoa(index+1)),
			ReceiptVerified:         true, ArtifactAvailable: true, VerifierManifest: manifest,
		}
	}
	contenders[0].ChargedOutcomeFacts = 1
	productionAttempts, productionConflicts, productionRetries := int64(1), int64(0), int64(0)
	contenders[0].CASAttempts = 1
	rootWaiters := int64(0)
	if cell.Mode == "natural_contention" {
		contenders[1].CASAttempts, contenders[1].CASConflicts, contenders[1].CASRetries = 1, 1, 1
		productionAttempts, productionConflicts, productionRetries = 2, 1, 1
	} else if cell.Mode == "forced_queue_safety" {
		rootWaiters = 1
	}
	naturalAttempts, naturalConflicts, naturalRetries := int64(0), int64(0), int64(0)
	if cell.Mode == "natural_contention" {
		naturalAttempts, naturalConflicts, naturalRetries = productionAttempts, productionConflicts, productionRetries
	}
	first := contenders[0]
	sample := Sample{
		SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
		ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
		Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate,
		PairID: operation.PairID, RootGroupID: operation.RootGroupID,
		System: "taskgate", Mode: cell.Mode, WorkloadID: cell.WorkloadID, Scale: cell.Scale, Status: "pass",
		ResultSHA256: expectedResult, RowCount: 1, ColumnCount: 2, RootTaskIDHash: rootTaskIDHash,
		RootEpochBefore: before.Epoch, RootEpochAfter: atBoundary.Epoch,
		RootSetSHA256Before: rootLedgerSetSHA256(before), RootSetSHA256After: rootLedgerSetSHA256(atBoundary),
		ReceiptVersion: first.ReceiptVersion, ReceiptSHA256: first.ReceiptSHA256,
		ArtifactIntentSHA256:    first.ArtifactIntentSHA256,
		AvailabilityAuditSHA256: first.AvailabilityAuditSHA256,
		ReceiptVerified:         true, ArtifactAvailable: true,
		ArtifactSHA256:       first.VerifierManifest.ReleasedParquetSHA256,
		ObjectSHA256:         first.VerifierManifest.CanonicalCiphertextSHA256,
		ParquetBytes:         first.VerifierManifest.ReleasedParquetSize,
		EncryptedObjectBytes: first.VerifierManifest.CanonicalCiphertextSize,
		Counters: map[string]int64{
			"cas_attempts": productionAttempts, "cas_conflicts": productionConflicts,
			"cas_retries": productionRetries, "barrier_clients": int64(cell.Width),
			"service_clients_observed": int64(cell.Width), "offered_concurrency_observed": 1,
			"forced_queue_waiters": rootWaiters,
		},
	}
	sample.ConcurrencyVerification = &ConcurrencyVerification{
		Version: concurrencyVerificationVersion, FixtureSHA256: concurrencyfixture.FixtureSHA256(),
		PlansSHA256: concurrencyfixture.PlansSHA256(), ProbeVersion: gatewayapp.ConcurrencyProbeVersion,
		GatewayInstanceSHA256: digestForConcurrencyTest("gateway-instance"), RoundSHA256: roundSHA,
		RootTaskIDHash: rootTaskIDHash, ContenderRequestSetSHA256: canonicalStringSetSHA256(requestDigests),
		ExpectedWidth: int64(cell.Width), HTTPActiveCapacity: 64, HTTPQueueCapacity: 512,
		ControlPoolCapacity: 64, ConnectorPoolCapacity: 64,
		ServiceArrivals: int64(cell.Width), ServiceUniqueParticipants: int64(cell.Width),
		ServiceParticipantSetSHA256: concurrencyfixture.ParticipantSetSHA256(roundSHA, cell.Width),
		ServicePeakBarrierWaiting:   int64(cell.Width), ServicePeakActive: minConcurrencyTestInt64(64, int64(cell.Width)),
		ServicePeakQueued: maxConcurrencyTestInt64(0, int64(cell.Width)-64), ServiceCompleted: int64(cell.Width),
		PeakControlPoolInUse:    minConcurrencyTestInt64(64, int64(cell.Width)),
		RootLockWaitersObserved: rootWaiters,
		ProductionCASAttempts:   productionAttempts, ProductionCASConflicts: productionConflicts,
		ProductionCASRetries: productionRetries, NaturalCASAttempts: naturalAttempts,
		NaturalCASConflicts: naturalConflicts, NaturalCASRetries: naturalRetries,
		InitialRoot: initial, BeforeBoundary: before, AtBoundary: atBoundary, AfterRejectedOverflow: atBoundary,
		ResourceBudgetProfile: concurrencyfixture.BudgetProfile,
		ResourceMaxQueries:    concurrencyfixture.ResourceMaxQueries,
		BudgetLimit:           concurrencyfixture.RootBudgetLimit, UsageBefore: concurrencyfixture.UsageBeforeBoundary,
		Accepted: int64(cell.Width), UsageAfter: concurrencyfixture.ExpectedFinalOutcome,
		ChargedWinners: 1, ZeroNoveltySettlements: int64(cell.Width - 1), ExpectedResultSHA256: expectedResult,
		FinalRootFactHashes: append([]string(nil), members...), FinalRootSetSHA256: set.Set.SetSHA256,
		Contenders: contenders,
		Overflow: ConcurrencyOverflowEvidence{
			Attempts: 1, Rejected: 1, ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED", Found: true,
			QueryIDHash: digestForConcurrencyTest("overflow-query"), Status: "FAILED",
			ReservationStatus: "RELEASED", ReleaseAudits: 1, FailureAudits: 1, Receipts: 1,
		},
	}
	return sample
}

func concurrencyRootSnapshot(epoch, outcome, observations int64, outcomeSet string) RootLedgerSnapshot {
	return RootLedgerSnapshot{
		Epoch: epoch, DictionarySetSHA256: digestForConcurrencyTest("dictionary"),
		ReleaseSetSHA256: digestForConcurrencyTest("release-set"), ReleaseCardinality: 1,
		DependencySetSHA256: digestForConcurrencyTest("dependency-set"), DependencyCardinality: 1,
		OutcomeSetSHA256: outcomeSet, OutcomeCardinality: outcome,
		RootObservationSetSHA256: digestForConcurrencyTest("observations-" + strconv.FormatInt(observations, 10)),
		RootObservationCount:     observations,
	}
}

func digestForConcurrencyTest(label string) string {
	return sha256Hex([]byte("concurrency-test\x00" + label))
}

func minConcurrencyTestInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxConcurrencyTestInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func TestConcurrencyErrorMessageDoesNotDescribeChildTasks(t *testing.T) {
	sample := validConcurrencyValidationSample(t, concurrencyfixture.FrozenCells[1])
	sample.ConcurrencyVerification.Contenders[0].TaskIDHash = digestForConcurrencyTest("different-task")
	err := ValidateConcurrencyEvidence(sample)
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "child-task") {
		t.Fatalf("validator should describe the one-root invariant directly: %v", err)
	}
}
