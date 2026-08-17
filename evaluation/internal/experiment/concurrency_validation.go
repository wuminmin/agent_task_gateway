package experiment

import (
	"errors"
	"fmt"
	"strconv"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/internal/control"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

const concurrencyVerificationVersion = "taskgate-final-v5-concurrency-verification-v1"

// ValidateConcurrencyEvidence is the adapter-side copy of the campaign gate.
// A real run whose observations violate a frozen invariant is retained as a
// failed sample instead of being mislabeled pass and rejected only later.
func ValidateConcurrencyEvidence(sample Sample) error {
	return validateConcurrencyVerificationStrict(sample)
}

func validateConcurrencyVerificationStrict(sample Sample) error {
	evidence := sample.ConcurrencyVerification
	cell, frozen := concurrencyfixture.Lookup(sample.WorkloadID, sample.Scale, sample.Mode)
	width, widthErr := strconv.ParseInt(sample.Scale, 10, 64)
	if evidence == nil || !frozen || widthErr != nil || width != int64(cell.Width) || width < 1 {
		return errors.New("concurrency evidence does not identify one frozen cell")
	}
	if evidence.Version != concurrencyVerificationVersion || evidence.FixtureSHA256 != concurrencyfixture.FixtureSHA256() ||
		evidence.PlansSHA256 != concurrencyfixture.PlansSHA256() || evidence.ProbeVersion != gatewayapp.ConcurrencyProbeVersion {
		return errors.New("concurrency fixture or production probe binding differs from source control")
	}
	expectedRound := concurrencyfixture.RoundSHA256(concurrencyfixture.RoundIdentity{
		CampaignID: sample.CampaignID, DeploymentID: sample.DeploymentID, ExperimentID: sample.ExperimentID,
		CellID: sample.CellID, SampleID: sample.SampleID, Iteration: sample.Iteration,
		ProcessReplicate: sample.ProcessReplicate, PairID: sample.PairID, RootGroupID: sample.RootGroupID,
	})
	if evidence.RoundSHA256 != expectedRound || !validSHA256(evidence.GatewayInstanceSHA256) ||
		!validSHA256(evidence.RootTaskIDHash) || evidence.RootTaskIDHash != sample.RootTaskIDHash ||
		!validSHA256(evidence.ContenderRequestSetSHA256) {
		return errors.New("concurrency round, Gateway instance, or same-root identity binding is invalid")
	}
	if evidence.ExpectedWidth != width || evidence.HTTPActiveCapacity != int64(concurrencyfixture.ServiceActiveWindow) ||
		evidence.HTTPQueueCapacity < int64(concurrencyfixture.MinimumServiceQueue) ||
		evidence.ControlPoolCapacity < int64(concurrencyfixture.MinimumProductionPoolWidth) ||
		evidence.ConnectorPoolCapacity < int64(concurrencyfixture.MinimumProductionPoolWidth) {
		return errors.New("concurrency run lacks the preregistered 500-client production capacity")
	}
	if evidence.ServiceArrivals != width || evidence.ServiceUniqueParticipants != width ||
		evidence.ServiceParticipantSetSHA256 != concurrencyfixture.ParticipantSetSHA256(expectedRound, int(width)) ||
		evidence.ServicePeakBarrierWaiting != width || evidence.ServiceCompleted != width ||
		evidence.ServiceCanceled != 0 || evidence.ServiceRejected != 0 || evidence.ServicePeakActive < 1 ||
		evidence.ServicePeakActive > evidence.HTTPActiveCapacity || evidence.ServicePeakQueued < 0 ||
		evidence.ServicePeakQueued > evidence.HTTPQueueCapacity || evidence.PeakControlPoolInUse < 1 ||
		evidence.PeakControlPoolInUse > evidence.ControlPoolCapacity || evidence.ControlPoolWaitCountDelta < 0 ||
		evidence.ControlPoolWaitNanoseconds < 0 {
		return errors.New("server-side arrival, barrier, active, queue, completion, or pool telemetry is incomplete")
	}
	if sample.Counters["barrier_clients"] != width || sample.Counters["service_clients_observed"] != width ||
		sample.Counters["offered_concurrency_observed"] != 1 || sample.Counters["forced_queue_waiters"] != evidence.RootLockWaitersObserved ||
		sample.Counters["cas_attempts"] != evidence.ProductionCASAttempts ||
		sample.Counters["cas_conflicts"] != evidence.ProductionCASConflicts ||
		sample.Counters["cas_retries"] != evidence.ProductionCASRetries {
		return errors.New("sample counters do not equal the authenticated service/production response evidence")
	}
	if sample.Mode == "forced_queue_safety" {
		if evidence.RootLockWaitersObserved < 1 || evidence.NaturalCASAttempts != 0 || evidence.NaturalCASConflicts != 0 || evidence.NaturalCASRetries != 0 {
			return errors.New("forced-lock safety evidence was confused with natural CAS evidence")
		}
	} else {
		if evidence.RootLockWaitersObserved != 0 {
			return errors.New("non-forced arm carries root-lock waiter evidence")
		}
		if sample.Mode == "natural_contention" {
			if evidence.NaturalCASAttempts != evidence.ProductionCASAttempts || evidence.NaturalCASConflicts != evidence.ProductionCASConflicts ||
				evidence.NaturalCASRetries != evidence.ProductionCASRetries || evidence.NaturalCASAttempts < 1 ||
				evidence.NaturalCASConflicts < 1 || evidence.NaturalCASRetries != evidence.NaturalCASConflicts {
				return errors.New("natural arm lacks real FinalizeQueryMetrics/OutcomeRadix CAS conflict and retry evidence")
			}
		} else if evidence.NaturalCASAttempts != 0 || evidence.NaturalCASConflicts != 0 || evidence.NaturalCASRetries != 0 ||
			evidence.ProductionCASAttempts != 1 || evidence.ProductionCASConflicts != 0 || evidence.ProductionCASRetries != 0 {
			return errors.New("serial control carries contention or non-serial CAS evidence")
		}
	}

	if err := validateFreshRootLedgerSnapshot(evidence.InitialRoot); err != nil {
		return fmt.Errorf("initial concurrency root: %w", err)
	}
	if err := validateRootLedgerSnapshot(evidence.BeforeBoundary); err != nil {
		return fmt.Errorf("B-1 concurrency root: %w", err)
	}
	if err := validateRootLedgerSnapshot(evidence.AtBoundary); err != nil {
		return fmt.Errorf("B concurrency root: %w", err)
	}
	if evidence.BeforeBoundary.Epoch != 3 || evidence.BeforeBoundary.OutcomeCardinality != concurrencyfixture.UsageBeforeBoundary ||
		evidence.BeforeBoundary.RootObservationCount != 3 || evidence.AtBoundary.Epoch != 4 ||
		evidence.AtBoundary.OutcomeCardinality != concurrencyfixture.ExpectedFinalOutcome ||
		evidence.AtBoundary.RootObservationCount != 4 || evidence.AfterRejectedOverflow != evidence.AtBoundary {
		return errors.New("B-1/B/B+1 root ledger transition is not exact and atomic")
	}
	if evidence.ResourceBudgetProfile != concurrencyfixture.BudgetProfile || evidence.ResourceMaxQueries != concurrencyfixture.ResourceMaxQueries ||
		evidence.BudgetLimit != concurrencyfixture.RootBudgetLimit || evidence.UsageBefore != concurrencyfixture.UsageBeforeBoundary ||
		evidence.UsageAfter != concurrencyfixture.ExpectedFinalOutcome || evidence.Accepted != width || evidence.Rejected != 0 ||
		evidence.ChargedWinners != 1 || evidence.ZeroNoveltySettlements != width-1 {
		return errors.New("B-1 boundary did not yield one charged winner and all zero-novelty followers")
	}
	if evidence.ExpectedResultSHA256 != concurrencyfixture.ExpectedContenderResultSHA256() || sample.ResultSHA256 != evidence.ExpectedResultSHA256 ||
		sample.RowCount != 1 || sample.ColumnCount != 2 || sample.RootEpochBefore != evidence.BeforeBoundary.Epoch ||
		sample.RootEpochAfter != evidence.AtBoundary.Epoch || sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.BeforeBoundary) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.AtBoundary) || sample.BaselineVerification != nil {
		return errors.New("concurrency sample summary differs from the frozen result/root boundary")
	}
	if err := validateConcurrencyContenders(sample, evidence, expectedRound, width); err != nil {
		return err
	}
	if err := validateConcurrencyFinalOutcome(evidence); err != nil {
		return err
	}
	return validateConcurrencyOverflow(evidence)
}

func validateConcurrencyContenders(sample Sample, evidence *ConcurrencyVerification, roundSHA string, width int64) error {
	if int64(len(evidence.Contenders)) != width {
		return errors.New("concurrency evidence omits one or more contenders")
	}
	indexes := map[int]bool{}
	participants, tasks, requests := map[string]bool{}, map[string]bool{}, map[string]bool{}
	queries, results := map[string]bool{}, map[string]bool{}
	var attempts, conflicts, retries, charged, zero int64
	var observation, composite, predicateSet string
	for position, contender := range evidence.Contenders {
		wantIndex := position + 1
		if contender.Index != wantIndex || indexes[contender.Index] || contender.ParticipantSHA256 != concurrencyfixture.ParticipantSHA256(roundSHA, wantIndex) {
			return errors.New("concurrency contender index/participant binding is incomplete")
		}
		indexes[contender.Index] = true
		for name, digest := range map[string]string{
			"participant": contender.ParticipantSHA256, "task": contender.TaskIDHash, "root": contender.RootTaskIDHash,
			"request": contender.RequestIDHash, "query": contender.QueryIDHash, "result": contender.ResultIDHash,
			"result hash": contender.ResultSHA256, "observation": contender.ObservationSHA256,
			"composite": contender.CompositeOutcomeSHA256, "predicate set": contender.PredicateSetSHA256,
			"receipt": contender.ReceiptSHA256, "artifact intent": contender.ArtifactIntentSHA256,
			"availability": contender.AvailabilityAuditSHA256,
		} {
			if !validSHA256(digest) {
				return fmt.Errorf("concurrency contender has invalid %s digest", name)
			}
		}
		if contender.TaskIDHash == evidence.RootTaskIDHash || tasks[contender.TaskIDHash] ||
			contender.RootTaskIDHash != evidence.RootTaskIDHash || participants[contender.ParticipantSHA256] || requests[contender.RequestIDHash] ||
			queries[contender.QueryIDHash] || results[contender.ResultIDHash] {
			return errors.New("concurrency delegated child family is duplicated, open, or bound to another root")
		}
		tasks[contender.TaskIDHash] = true
		participants[contender.ParticipantSHA256], requests[contender.RequestIDHash] = true, true
		queries[contender.QueryIDHash], results[contender.ResultIDHash] = true, true
		manifest := contender.VerifierManifest
		if contender.ResultSHA256 != evidence.ExpectedResultSHA256 ||
			contender.RootEpoch != evidence.AtBoundary.Epoch || contender.ActualOutcomeFacts != 2 ||
			(contender.ChargedOutcomeFacts != 0 && contender.ChargedOutcomeFacts != 1) || contender.CASAttempts < 0 ||
			contender.CASConflicts < 0 || contender.CASRetries < 0 || contender.CASConflicts != contender.CASRetries ||
			!currentReceiptVersion(contender.ReceiptVersion) || !contender.ReceiptVerified || !contender.ArtifactAvailable ||
			validateRedactedManifestStructure(manifest) != nil || manifest.QueryIDHash != contender.QueryIDHash ||
			manifest.ResultIDHash != contender.ResultIDHash || manifest.RootTaskIDHash != contender.RootTaskIDHash ||
			manifest.ReceiptSHA256 != contender.ReceiptSHA256 || manifest.ObservationSHA256 != contender.ObservationSHA256 ||
			manifest.OutcomeSetSHA256 == "" || manifest.ArtifactIntentSHA256 != contender.ArtifactIntentSHA256 {
			return errors.New("concurrency contender lacks a fully bound current Receipt/artifact/availability verifier manifest")
		}
		if position == 0 {
			observation, composite, predicateSet = contender.ObservationSHA256, contender.CompositeOutcomeSHA256, contender.PredicateSetSHA256
			if sample.ReceiptVersion != contender.ReceiptVersion || sample.ReceiptSHA256 != contender.ReceiptSHA256 ||
				sample.ArtifactIntentSHA256 != contender.ArtifactIntentSHA256 || sample.AvailabilityAuditSHA256 != contender.AvailabilityAuditSHA256 ||
				!sample.ReceiptVerified || !sample.ArtifactAvailable || sample.ArtifactSHA256 != manifest.ReleasedParquetSHA256 ||
				sample.ObjectSHA256 != manifest.CanonicalCiphertextSHA256 || sample.ParquetBytes != manifest.ReleasedParquetSize ||
				sample.EncryptedObjectBytes != manifest.CanonicalCiphertextSize {
				return errors.New("sample-level V8/artifact evidence differs from the first fully verified contender")
			}
		} else if contender.ObservationSHA256 != observation || contender.CompositeOutcomeSHA256 != composite || contender.PredicateSetSHA256 != predicateSet {
			return errors.New("same-root contenders are not the exact same normalized observation")
		}
		attempts += contender.CASAttempts
		conflicts += contender.CASConflicts
		retries += contender.CASRetries
		charged += contender.ChargedOutcomeFacts
		if contender.ChargedOutcomeFacts == 0 {
			zero++
		}
	}
	if int64(len(tasks)) != width || canonicalStringSetSHA256(mapKeysTrue(requests)) != evidence.ContenderRequestSetSHA256 || attempts != evidence.ProductionCASAttempts ||
		conflicts != evidence.ProductionCASConflicts || retries != evidence.ProductionCASRetries || charged != evidence.ChargedWinners ||
		zero != evidence.ZeroNoveltySettlements || charged != 1 || zero != width-1 {
		return errors.New("delegated child closure, exactly-one-winner, CAS totals, or settlement totals do not recompute from every contender")
	}
	return nil
}

func validateConcurrencyFinalOutcome(evidence *ConcurrencyVerification) error {
	seen := map[string]bool{}
	for _, digest := range evidence.FinalRootFactHashes {
		if !validSHA256(digest) || seen[digest] {
			return errors.New("final root fact hashes are invalid or duplicated")
		}
		seen[digest] = true
	}
	if int64(len(seen)) != evidence.AtBoundary.OutcomeCardinality || evidence.FinalRootSetSHA256 != evidence.AtBoundary.OutcomeSetSHA256 {
		return errors.New("final root member list does not match the persisted root head")
	}
	rebuilt, err := control.BuildOutcomeHashSetV5(evidence.FinalRootFactHashes)
	if err != nil || rebuilt.Set.Cardinality != evidence.AtBoundary.OutcomeCardinality || rebuilt.Set.SetSHA256 != evidence.FinalRootSetSHA256 {
		return errors.New("production V5 Merkle root does not recompute from the exact final members")
	}
	if len(evidence.Contenders) == 0 || !seen[evidence.Contenders[0].CompositeOutcomeSHA256] {
		return errors.New("final root omits the contender composite outcome")
	}
	return nil
}

func validateConcurrencyOverflow(evidence *ConcurrencyVerification) error {
	overflow := evidence.Overflow
	if overflow.Attempts != 1 || overflow.Rejected != 1 || overflow.ErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" ||
		!overflow.Found || !validSHA256(overflow.QueryIDHash) || overflow.Status != "FAILED" ||
		overflow.ReservationStatus != "RELEASED" || overflow.ResultSHA256 != "" || overflow.EncryptedResults != 0 ||
		overflow.EncryptedChunks != 0 || overflow.Materializations != 0 || overflow.QueryObservations != 0 ||
		overflow.RootObservations != 0 || overflow.Artifacts != 0 || overflow.AvailableArtifacts != 0 ||
		overflow.AvailabilityAudits != 0 || overflow.SuccessfulAudits != 0 || overflow.ReleaseAudits != 1 ||
		overflow.FailureAudits != 1 || overflow.Receipts != 1 {
		return errors.New("B+1 overflow did not retain the exact failure-only Control projection")
	}
	return nil
}

func mapKeysTrue(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value, present := range values {
		if present {
			result = append(result, value)
		}
	}
	return result
}
