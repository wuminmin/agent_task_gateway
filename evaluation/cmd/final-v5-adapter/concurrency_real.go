package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/control"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

var errConcurrencyObservationIncomplete = errors.New("authenticated service did not observe the complete offered concurrency")

type realConcurrencyBackend struct {
	real       *realAdapter
	probe      concurrencyProbeAPI
	probeToken string
}

type concurrencyCreatedTask = provisionedTask

type concurrencyCallResult struct {
	index       int
	taskID      string
	requestID   string
	participant string
	started     time.Time
	availableMS float64
	response    queryResponse
	err         error
}

func (backend *realConcurrencyBackend) Capacity(ctx context.Context) (gatewayapp.ConcurrencyProbeCapacity, error) {
	if backend == nil || backend.real == nil || backend.probe == nil || len(backend.probeToken) < 32 {
		return gatewayapp.ConcurrencyProbeCapacity{}, errors.New("real concurrency backend is incomplete")
	}
	return backend.probe.Capacity(ctx)
}

func (backend *realConcurrencyBackend) Close() {
	if backend != nil && backend.real != nil {
		backend.real.Close()
	}
}

func (backend *realConcurrencyBackend) Run(ctx context.Context, operation experiment.AdapterOperation, cell concurrencyfixture.Cell) (experiment.Sample, error) {
	if backend == nil || backend.real == nil || backend.probe == nil || cell.Width < 1 {
		return experiment.Sample{}, errors.New("real concurrency backend is incomplete")
	}
	capacity, err := backend.Capacity(ctx)
	if err != nil || validateConcurrencyCapacity(capacity) != nil {
		return experiment.Sample{}, errors.New("authenticated concurrency capacity changed after constructor preflight")
	}
	created, children, err := backend.provisionTaskFamily(ctx, operation, cell.Width)
	if err != nil {
		return experiment.Sample{}, err
	}
	rootTaskIDHash := saltedTaskHash(operation, created.RootTaskID)
	identityPrefix := baseSample(operation, "taskgate")
	identityPrefix.RootTaskIDHash = rootTaskIDHash
	initialRoot, err := backend.real.rootLedgerSnapshot(ctx, created.TaskID)
	if err != nil {
		return identityPrefix, &concurrencyRunError{code: "concurrency_initial_root_snapshot_failed",
			sample: identityPrefix, cause: err}
	}
	roundIdentity := concurrencyRoundIdentity(operation)
	roundSHA := concurrencyfixture.RoundSHA256(roundIdentity)
	prefixResponses, beforeBoundary, err := backend.executePrefix(ctx, operation, created.TaskID, initialRoot)
	if err != nil {
		prefix := partialConcurrencyServiceSample(operation, capacity,
			gatewayapp.ConcurrencyProbeSnapshot{ConcurrencyProbeCapacity: capacity}, roundSHA, rootTaskIDHash,
			initialRoot, beforeBoundary)
		return prefix, &concurrencyRunError{code: "concurrency_prefix_verification_failed", sample: prefix, cause: err}
	}
	rootPrefix := partialConcurrencyServiceSample(operation, capacity,
		gatewayapp.ConcurrencyProbeSnapshot{ConcurrencyProbeCapacity: capacity}, roundSHA, rootTaskIDHash,
		initialRoot, beforeBoundary)
	rootFailure := func(code string, cause error) (experiment.Sample, error) {
		return rootPrefix, &concurrencyRunError{code: code, sample: rootPrefix, cause: cause}
	}
	if _, err := backend.probe.CreateRound(ctx, roundSHA, operation.Mode, cell.Width); err != nil {
		return rootFailure("concurrency_probe_round_creation_failed", err)
	}
	roundCreated := true
	defer func() {
		if roundCreated {
			_ = backend.probe.DeleteRound(context.Background(), roundSHA)
		}
	}()

	var blocker pgx.Tx
	var blockerPID int32
	if operation.Mode == "forced_queue_safety" {
		blocker, err = backend.real.control.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return rootFailure("concurrency_forced_queue_setup_failed", err)
		}
		defer blocker.Rollback(context.Background())
		if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			return rootFailure("concurrency_forced_queue_setup_failed", err)
		}
		var locked string
		if err := blocker.QueryRow(ctx, `SELECT root_task_id FROM v5_exposure_root_heads
WHERE root_task_id=$1 FOR UPDATE`, created.RootTaskID).Scan(&locked); err != nil || locked != created.RootTaskID {
			if err == nil {
				err = errors.New("forced-lock blocker acquired the wrong root")
			}
			return rootFailure("concurrency_forced_queue_setup_failed", err)
		}
	}

	measurementStarted := time.Now()
	calls := backend.launchContenders(ctx, operation, children, roundSHA)
	rootLockWaiters := int64(0)
	if blocker != nil {
		waitSnapshot, waitErr := gatewayapp.WaitForConcurrencyProbeSnapshot(ctx, 10*time.Millisecond,
			func(loadCtx context.Context) (gatewayapp.ConcurrencyProbeSnapshot, error) {
				return backend.probe.Snapshot(loadCtx, roundSHA)
			},
			func(snapshot gatewayapp.ConcurrencyProbeSnapshot) bool {
				return snapshot.Released && snapshot.Arrived == int64(cell.Width) && snapshot.UniqueParticipants == int64(cell.Width)
			})
		if waitErr == nil {
			rootLockWaiters, waitErr = waitForV5RootLockWaiter(ctx, backend.real, blockerPID)
		}
		commitErr := blocker.Commit(ctx)
		blocker = nil
		if waitErr != nil {
			latest, latestErr := backend.probe.Snapshot(context.Background(), roundSHA)
			if latestErr == nil {
				waitSnapshot = latest
			}
			partial := partialConcurrencyServiceSample(operation, capacity, waitSnapshot, roundSHA, rootTaskIDHash,
				initialRoot, beforeBoundary)
			partial.Counters["forced_queue_waiters"] = rootLockWaiters
			partial.ConcurrencyVerification.RootLockWaitersObserved = rootLockWaiters
			partial.ClientFullDrainMS = durationMS(time.Since(measurementStarted))
			return partial, &concurrencyRunError{code: "concurrency_forced_queue_observation_failed", sample: partial, cause: waitErr}
		}
		if commitErr != nil {
			latest, latestErr := backend.probe.Snapshot(context.Background(), roundSHA)
			if latestErr == nil {
				waitSnapshot = latest
			}
			partial := partialConcurrencyServiceSample(operation, capacity, waitSnapshot, roundSHA, rootTaskIDHash,
				initialRoot, beforeBoundary)
			partial.Counters["forced_queue_waiters"] = rootLockWaiters
			partial.ConcurrencyVerification.RootLockWaitersObserved = rootLockWaiters
			partial.ClientFullDrainMS = durationMS(time.Since(measurementStarted))
			return partial, &concurrencyRunError{code: "concurrency_forced_queue_release_failed", sample: partial, cause: commitErr}
		}
	}
	callResults, sampledSnapshot, sampleErr := backend.awaitContendersAndSampleProbe(ctx, roundSHA, calls)
	availableMS := durationMS(time.Since(measurementStarted))
	if sampleErr != nil {
		partial := partialConcurrencyServiceSample(operation, capacity, sampledSnapshot, roundSHA, rootTaskIDHash,
			initialRoot, beforeBoundary)
		if errors.Is(sampleErr, errConcurrencyObservationIncomplete) &&
			sampledSnapshot.ConcurrencyProbeCapacity == capacity {
			recordConcurrencyObservationShortfall(operation, roundSHA, cell.Width, sampledSnapshot,
				concurrencyObservationShortfall(sampledSnapshot, roundSHA, operation.Mode, cell.Width), sampleErr)
			return partial, &concurrencyRunError{code: "offered_concurrency_not_observed", invalid: true,
				sample: partial, cause: sampleErr}
		}
		return partial, &concurrencyRunError{code: "concurrency_service_observation_failed", sample: partial, cause: sampleErr}
	}
	if sampledSnapshot.ConcurrencyProbeCapacity != capacity {
		partial := partialConcurrencyServiceSample(operation, capacity, sampledSnapshot, roundSHA, rootTaskIDHash,
			initialRoot, beforeBoundary)
		return partial, &concurrencyRunError{code: "concurrency_capacity_changed", sample: partial,
			cause: errors.New("authenticated concurrency capacity changed during the measurement")}
	}
	if unmet := concurrencyObservationShortfall(sampledSnapshot, roundSHA, operation.Mode, cell.Width); len(unmet) > 0 {
		partial := partialConcurrencyServiceSample(operation, capacity, sampledSnapshot, roundSHA, rootTaskIDHash,
			initialRoot, beforeBoundary)
		recordConcurrencyObservationShortfall(operation, roundSHA, cell.Width, sampledSnapshot, unmet, nil)
		return partial, &concurrencyRunError{code: "offered_concurrency_not_observed", invalid: true, sample: partial}
	}
	retained := observedConcurrencyServiceSample(operation, capacity, sampledSnapshot, roundSHA, rootTaskIDHash,
		initialRoot, beforeBoundary, rootLockWaiters)
	lateFailure := func(code string, cause error) (experiment.Sample, error) {
		retained.ClientAvailableMS = availableMS
		retained.ClientFullDrainMS = durationMS(time.Since(measurementStarted))
		return retained, &concurrencyRunError{code: code, sample: retained, cause: cause}
	}
	if err := firstConcurrencyContenderFailure(callResults); err != nil {
		return lateFailure("concurrency_contender_call_failed", err)
	}
	probeSnapshot, err := waitForProbeCompletion(ctx, backend.probe, roundSHA, int64(cell.Width))
	if err != nil {
		latest, latestErr := backend.probe.Snapshot(context.Background(), roundSHA)
		if latestErr == nil && latest.Version != "" {
			retained = observedConcurrencyServiceSample(operation, capacity, latest, roundSHA, rootTaskIDHash,
				initialRoot, beforeBoundary, rootLockWaiters)
		}
		return lateFailure("concurrency_probe_completion_failed", err)
	}
	retained = observedConcurrencyServiceSample(operation, capacity, probeSnapshot, roundSHA, rootTaskIDHash,
		initialRoot, beforeBoundary, rootLockWaiters)
	if err := backend.probe.DeleteRound(ctx, roundSHA); err != nil {
		return lateFailure("concurrency_probe_cleanup_failed", err)
	}
	roundCreated = false

	atBoundary, err := backend.real.rootLedgerSnapshot(ctx, created.TaskID)
	if err != nil {
		return lateFailure("concurrency_boundary_snapshot_failed", err)
	}
	updateConcurrencyRetainedRoot(&retained, atBoundary)
	contenders, representative, err := backend.verifyContenders(ctx, operation, created.RootTaskID, beforeBoundary, atBoundary, callResults)
	retained = retainVerifiedConcurrencyPrefix(retained, representative, contenders, atBoundary)
	if err != nil {
		return lateFailure("concurrency_contender_verification_failed", err)
	}
	finalMembers, err := backend.finalOutcomeMembers(ctx, prefixResponses, callResults, atBoundary)
	if err != nil {
		return lateFailure("concurrency_final_root_verification_failed", err)
	}
	retained.ConcurrencyVerification.FinalRootFactHashes = append([]string(nil), finalMembers...)
	retained.ConcurrencyVerification.FinalRootSetSHA256 = atBoundary.OutcomeSetSHA256
	overflow, afterOverflow, err := backend.executeOverflow(ctx, operation, created.TaskID, atBoundary)
	retained.ConcurrencyVerification.Overflow = overflow
	if afterOverflow.OutcomeSetSHA256 != "" {
		retained.ConcurrencyVerification.AfterRejectedOverflow = afterOverflow
		retained.RootEpochAfter = afterOverflow.Epoch
		retained.RootSetSHA256After = rootSetDigest(afterOverflow)
	}
	if err != nil {
		return lateFailure("concurrency_overflow_verification_failed", err)
	}
	sample := buildConcurrencySample(operation, capacity, probeSnapshot, created, rootTaskIDHash, initialRoot,
		beforeBoundary, atBoundary, afterOverflow, rootLockWaiters, finalMembers, contenders,
		representative, availableMS, durationMS(time.Since(measurementStarted)), overflow)
	if sample.Mode == "natural_contention" && sample.ConcurrencyVerification.NaturalCASConflicts < 1 {
		sample.Status = "invalid"
		sample.ErrorCode = "offered_concurrency_not_observed"
		return sample, &concurrencyRunError{code: sample.ErrorCode, invalid: true, sample: sample,
			cause: errors.New("production OutcomeRadix reported no natural CAS conflict")}
	}
	return sample, nil
}

// firstConcurrencyContenderFailure deliberately treats every failed tool call,
// including the public CONFLICT code, as fatal. Only a successful production
// response carries OutcomeRadix CAS attempts/conflicts/retries; CONFLICT also
// covers unrelated Control-store failures and therefore cannot be re-labelled
// or retried into natural-contention evidence by the harness.
func firstConcurrencyContenderFailure(results []concurrencyCallResult) error {
	for _, result := range results {
		if result.err != nil {
			return fmt.Errorf("contender %d failed: %w", result.index, result.err)
		}
	}
	return nil
}

func (backend *realConcurrencyBackend) provisionTaskFamily(ctx context.Context, operation experiment.AdapterOperation,
	width int) (concurrencyCreatedTask, []concurrencyCreatedTask, error) {
	if width < 1 {
		return concurrencyCreatedTask{}, nil, errors.New("concurrency task family width must be positive")
	}
	columns := []string{"receipt_no", "expense_type", "city", "department"}
	root, err := backend.real.provisionCatalogTask(ctx,
		"Final V5 shared-root concurrency / "+operation.PairID, concurrencyfixture.ProductName, columns, "")
	if err != nil {
		return concurrencyCreatedTask{}, nil, err
	}
	if err := validateConcurrencyRootTask(root); err != nil {
		return root, nil, err
	}
	children := make([]concurrencyCreatedTask, width)
	for index := range children {
		child, childErr := backend.real.provisionCatalogTask(ctx,
			fmt.Sprintf("Final V5 delegated concurrency contender %d / %s", index+1, operation.PairID),
			concurrencyfixture.ProductName, columns, root.TaskID)
		if childErr != nil {
			return root, append([]concurrencyCreatedTask(nil), children[:index]...), childErr
		}
		children[index] = child
	}
	if err := validateConcurrencyTaskFamily(root, children, width); err != nil {
		return root, children, err
	}
	return root, children, nil
}

func validateConcurrencyRootTask(root concurrencyCreatedTask) error {
	if root.TaskID == "" || root.RootTaskID != root.TaskID || root.ParentTaskID != "" || root.OAURL == "" ||
		root.BudgetProfile != concurrencyfixture.BudgetProfile || root.Budget.MaxQueries != concurrencyfixture.ResourceMaxQueries ||
		root.Budget.MaxRows < int64(len(concurrencyfixture.PrefixSQL)+501) || root.Budget.MaxDBMS < 1 ||
		root.Budget.QueryTimeoutMS < 1 || root.Budget.TaskTTLSeconds < 1 || root.Budget.MaxReleaseFacts < 1 ||
		root.Budget.MaxInfluenceFacts < 1 || root.Budget.MaxOutcomeFacts != concurrencyfixture.RootBudgetLimit ||
		root.Budget.ExposureProfileVersion != "taskgate-exposure-v5" {
		return errors.New("dedicated concurrency product did not resolve to its exact frozen root budget")
	}
	return nil
}

func validateConcurrencyTaskFamily(root concurrencyCreatedTask, children []concurrencyCreatedTask, width int) error {
	if validateConcurrencyRootTask(root) != nil || width < 1 || len(children) != width {
		return errors.New("concurrency task family omits its exact root or delegated width")
	}
	seen := map[string]bool{root.TaskID: true}
	for _, child := range children {
		if child.TaskID == "" || seen[child.TaskID] || child.ParentTaskID != root.TaskID || child.RootTaskID != root.TaskID ||
			child.BudgetProfile != root.BudgetProfile || child.Budget.MaxQueries != root.Budget.MaxQueries ||
			child.Budget.MaxRows != root.Budget.MaxRows || child.Budget.MaxDBMS != root.Budget.MaxDBMS ||
			child.Budget.QueryTimeoutMS != root.Budget.QueryTimeoutMS || child.Budget.MaxReleaseFacts != root.Budget.MaxReleaseFacts ||
			child.Budget.MaxInfluenceFacts != root.Budget.MaxInfluenceFacts || child.Budget.MaxOutcomeFacts != root.Budget.MaxOutcomeFacts ||
			child.Budget.ExposureProfileVersion != root.Budget.ExposureProfileVersion {
			return errors.New("concurrency delegated child expands, escapes, or duplicates the closed root family")
		}
		seen[child.TaskID] = true
	}
	return nil
}

func (backend *realConcurrencyBackend) executePrefix(ctx context.Context, operation experiment.AdapterOperation, taskID string,
	initial experiment.RootLedgerSnapshot) ([]queryResponse, experiment.RootLedgerSnapshot, error) {
	state := &pairState{taskID: taskID}
	before := initial
	responses := make([]queryResponse, 0, len(concurrencyfixture.PrefixSQL))
	identity := concurrencyRoundIdentity(operation)
	for index, sqlText := range concurrencyfixture.PrefixSQL {
		started := time.Now()
		var response queryResponse
		requestID := concurrencyfixture.RequestID(identity, "prefix", index+1)
		if err := backend.real.alice.call(ctx, "query_sql", map[string]any{
			"task_id": taskID, "request_id": requestID, "sql": sqlText,
		}, &response); err != nil {
			return responses, before, err
		}
		after, err := backend.real.rootLedgerSnapshot(ctx, taskID)
		if err != nil {
			return responses, before, err
		}
		verified, err := backend.real.completeTaskgateSample(ctx, operation, state, before, after,
			started, durationMS(time.Since(started)), sqlText, response)
		if err != nil || verified.Status != "pass" || response.Exposure.ActualOutcomeFacts != 2 ||
			response.Exposure.ChargedOutcomeFacts != int64(2-boolToInt(index > 0)) ||
			after.Epoch != int64(index+1) || after.OutcomeCardinality != int64(index+2) {
			if err == nil {
				err = errors.New("fully verified prefix did not commit the exact B-1 construction")
			}
			return responses, after, err
		}
		responses = append(responses, response)
		before = after
	}
	return responses, before, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (backend *realConcurrencyBackend) launchContenders(ctx context.Context, operation experiment.AdapterOperation,
	children []concurrencyCreatedTask, roundSHA string) <-chan []concurrencyCallResult {
	completed := make(chan []concurrencyCallResult, 1)
	results := make([]concurrencyCallResult, len(children))
	identity := concurrencyRoundIdentity(operation)
	var wait sync.WaitGroup
	for index := range children {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			participant := concurrencyfixture.ParticipantSHA256(roundSHA, index+1)
			requestID := concurrencyfixture.RequestID(identity, "contender", index+1)
			started := time.Now()
			taskID := children[index].TaskID
			result := concurrencyCallResult{index: index + 1, taskID: taskID, requestID: requestID, participant: participant, started: started}
			result.err = backend.real.alice.callWithHeaders(ctx, "query_sql", map[string]any{
				"task_id": taskID, "request_id": requestID, "sql": concurrencyfixture.ContenderSQL,
			}, &result.response, map[string]string{
				gatewayapp.ConcurrencyRoundHeader:         roundSHA,
				gatewayapp.ConcurrencyParticipantHeader:   participant,
				gatewayapp.ConcurrencyAuthorizationHeader: backend.probeToken,
			})
			result.availableMS = durationMS(time.Since(started))
			results[index] = result
		}()
	}
	go func() {
		wait.Wait()
		completed <- results
	}()
	return completed
}

func waitForV5RootLockWaiter(ctx context.Context, adapter *realAdapter, blockerPID int32) (int64, error) {
	if adapter == nil || blockerPID <= 0 {
		return 0, errors.New("root-lock waiter probe is invalid")
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, adapter.timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var maximum int64
	for {
		var waiting int64
		err := adapter.control.QueryRow(deadlineCtx, `WITH RECURSIVE downstream(pid) AS (
 SELECT pid FROM pg_stat_activity
 WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
   AND $1 = ANY(pg_blocking_pids(pid))
 UNION
 SELECT activity.pid FROM pg_stat_activity activity
 JOIN downstream blocker ON blocker.pid = ANY(pg_blocking_pids(activity.pid))
 WHERE activity.datname=current_database() AND activity.state='active' AND activity.wait_event_type='Lock'
)
SELECT count(*) FROM downstream`, blockerPID).Scan(&waiting)
		if err != nil {
			return maximum, err
		}
		if waiting > maximum {
			maximum = waiting
		}
		if maximum > 0 {
			return maximum, nil
		}
		select {
		case <-deadlineCtx.Done():
			return maximum, fmt.Errorf("no real V5 root-lock waiter observed: %w", deadlineCtx.Err())
		case <-ticker.C:
		}
	}
}

func (backend *realConcurrencyBackend) verifyContenders(ctx context.Context, operation experiment.AdapterOperation,
	rootTaskID string, beforeBoundary, atBoundary experiment.RootLedgerSnapshot,
	calls []concurrencyCallResult) ([]experiment.ConcurrencyContenderEvidence, experiment.Sample, error) {
	contenders := make([]experiment.ConcurrencyContenderEvidence, len(calls))
	var representative experiment.Sample
	for index, call := range calls {
		state := &pairState{taskID: call.taskID}
		verified, err := backend.real.completeTaskgateSample(ctx, operation, state, beforeBoundary, atBoundary,
			call.started, call.availableMS, concurrencyfixture.ContenderSQL, call.response)
		if err != nil || verified.Status != "pass" || verified.ResultSHA256 != concurrencyfixture.ExpectedContenderResultSHA256() ||
			verified.BaselineVerification == nil || verified.BaselineVerification.VerifierManifest == nil ||
			call.taskID == rootTaskID || call.response.Exposure.RootTaskID != rootTaskID || call.response.TaskID != call.taskID ||
			call.response.Exposure.ActualOutcomeFacts != 2 || call.response.Exposure.RootEpoch != atBoundary.Epoch {
			if err == nil {
				err = errors.New("contender did not pass its independent V8/result/artifact verification")
			}
			return append([]experiment.ConcurrencyContenderEvidence(nil), contenders[:index]...), representative, err
		}
		// This must happen while the full verified sample still carries its
		// Gateway Receipt. buildConcurrencySample deliberately clears the
		// sample-level verification and retains only a compact Receipt digest.
		if err := validateConcurrencyContenderReceiptBeforeCompression(verified); err != nil {
			return append([]experiment.ConcurrencyContenderEvidence(nil), contenders[:index]...), representative, err
		}
		manifestValue := *verified.BaselineVerification.VerifierManifest
		contenders[index] = experiment.ConcurrencyContenderEvidence{
			Index: index + 1, ParticipantSHA256: call.participant,
			TaskIDHash: saltedTaskHash(operation, call.response.TaskID), RootTaskIDHash: manifestValue.RootTaskIDHash,
			RequestIDHash: saltedIdentityHash(operation, "request", call.requestID),
			QueryIDHash:   manifestValue.QueryIDHash, ResultIDHash: manifestValue.ResultIDHash,
			ResultSHA256: verified.ResultSHA256, ObservationSHA256: call.response.Exposure.ObservationSHA256,
			CompositeOutcomeSHA256:  call.response.Exposure.CompositeOutcomeSHA256,
			PredicateSetSHA256:      call.response.Exposure.PredicateSetSHA256,
			RootEpoch:               call.response.Exposure.RootEpoch,
			ActualOutcomeFacts:      call.response.Exposure.ActualOutcomeFacts,
			ChargedOutcomeFacts:     call.response.Exposure.ChargedOutcomeFacts,
			CASAttempts:             call.response.OutcomeRadix.CASAttempts,
			CASConflicts:            call.response.OutcomeRadix.CASConflicts,
			CASRetries:              call.response.OutcomeRadix.CASRetries,
			ReceiptVersion:          call.response.Receipt.Version,
			ReceiptSHA256:           verified.ReceiptSHA256,
			ArtifactIntentSHA256:    verified.ArtifactIntentSHA256,
			AvailabilityAuditSHA256: verified.AvailabilityAuditSHA256,
			ReceiptVerified:         verified.ReceiptVerified,
			ArtifactAvailable:       verified.ArtifactAvailable,
			VerifierManifest:        &manifestValue,
		}
		if index == 0 {
			representative = verified
		}
	}
	return contenders, representative, nil
}

func validateConcurrencyContenderReceiptBeforeCompression(sample experiment.Sample) error {
	if adapterSampleProfileBinder == nil || !adapterSampleProfileBinder.Active() {
		return nil
	}
	if sample.BaselineVerification == nil {
		return errors.New("verified concurrency contender omitted its Receipt before compression")
	}
	return adapterSampleProfileBinder.RequireReceiptCatalog(sample.BaselineVerification.Receipt)
}

func (backend *realConcurrencyBackend) finalOutcomeMembers(ctx context.Context, prefixes []queryResponse,
	calls []concurrencyCallResult, root experiment.RootLedgerSnapshot) ([]string, error) {
	if len(prefixes) != 3 || len(calls) == 0 || root.OutcomeCardinality != concurrencyfixture.ExpectedFinalOutcome {
		return nil, errors.New("final root member reconstruction lacks its exact boundary inputs")
	}
	contextSHA, predicateSet := calls[0].response.Exposure.PredicateContextSHA256, calls[0].response.Exposure.PredicateSetSHA256
	atom, err := backend.singletonPredicateAtom(ctx, contextSHA, predicateSet)
	if err != nil {
		return nil, err
	}
	members := []string{atom}
	for _, response := range prefixes {
		members = append(members, response.Exposure.CompositeOutcomeSHA256)
	}
	members = append(members, calls[0].response.Exposure.CompositeOutcomeSHA256)
	sort.Strings(members)
	for index, digest := range members {
		if !validDigest(digest) || index > 0 && members[index-1] == digest {
			return nil, errors.New("final root reconstruction contains an invalid or duplicate member")
		}
	}
	rebuilt, err := control.BuildOutcomeHashSetV5(members)
	if err != nil || rebuilt.Set.Cardinality != root.OutcomeCardinality || rebuilt.Set.SetSHA256 != root.OutcomeSetSHA256 {
		if err == nil {
			err = errors.New("reconstructed member list differs from the persisted V5 Merkle root")
		}
		return nil, err
	}
	return members, nil
}

func (backend *realConcurrencyBackend) singletonPredicateAtom(ctx context.Context, contextSHA, predicateSetSHA string) (string, error) {
	if !validDigest(contextSHA) || !validDigest(predicateSetSHA) {
		return "", errors.New("predicate atom lookup lacks a valid context/set binding")
	}
	rows, err := backend.real.control.Query(ctx, `SELECT fact_sha256 FROM v5_outcome_facts
WHERE fact_kind='PREDICATE_ATOM' AND predicate_context_sha256=$1 ORDER BY fact_sha256`, contextSHA)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return "", err
		}
		if singletonPredicateSetSHA256(digest) == predicateSetSHA {
			matches = append(matches, digest)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("predicate-set digest selected %d persisted singleton atoms", len(matches))
	}
	return matches[0], nil
}

func singletonPredicateSetSHA256(atomSHA string) string {
	decoded, err := hex.DecodeString(atomSHA)
	if err != nil || len(decoded) != sha256.Size {
		return ""
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-PREDICATE-SET-V1\x00"))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], 1)
	_, _ = hash.Write(count[:])
	_, _ = hash.Write(decoded)
	return hex.EncodeToString(hash.Sum(nil))
}

// awaitContendersAndSampleProbe continuously samples the production
// Gateway's service and Control-pool counters while the real MCP calls are in
// flight. This is intentionally not a client-side barrier: it only observes
// the authenticated server barrier that every request has already entered.
func (backend *realConcurrencyBackend) awaitContendersAndSampleProbe(ctx context.Context, roundSHA string,
	calls <-chan []concurrencyCallResult) ([]concurrencyCallResult, gatewayapp.ConcurrencyProbeSnapshot, error) {
	if backend == nil || backend.probe == nil || !validDigest(roundSHA) || calls == nil {
		return nil, gatewayapp.ConcurrencyProbeSnapshot{}, errors.New("concurrency service observation is incomplete")
	}
	latest, err := backend.probe.Snapshot(ctx, roundSHA)
	if err != nil {
		return nil, latest, err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var results []concurrencyCallResult
	for {
		if results != nil && latest.Released && latest.Completed == latest.ExpectedWidth &&
			latest.Active == 0 && latest.Queued == 0 && latest.BarrierWaiting == 0 {
			return results, latest, nil
		}
		select {
		case completed := <-calls:
			results = completed
		case <-ticker.C:
			observed, snapshotErr := backend.probe.Snapshot(ctx, roundSHA)
			if snapshotErr != nil {
				return results, latest, snapshotErr
			}
			latest = observed
		case <-ctx.Done():
			return results, latest, fmt.Errorf("%w: %v", errConcurrencyObservationIncomplete, ctx.Err())
		}
	}
}

func exactConcurrencyServiceObservation(snapshot gatewayapp.ConcurrencyProbeSnapshot, roundSHA, mode string, width int) bool {
	return len(concurrencyObservationShortfall(snapshot, roundSHA, mode, width)) == 0
}

// partialConcurrencyServiceSample retains authenticated partial service
// telemetry when the offered width was not actually observed. It deliberately
// does not fabricate contender, V8, root-boundary, or overflow evidence.
func partialConcurrencyServiceSample(operation experiment.AdapterOperation, capacity gatewayapp.ConcurrencyProbeCapacity,
	snapshot gatewayapp.ConcurrencyProbeSnapshot, roundSHA, rootTaskIDHash string,
	initial, before experiment.RootLedgerSnapshot) experiment.Sample {
	cell, _ := concurrencyfixture.Lookup(operation.WorkloadID, operation.Scale, operation.Mode)
	sample := invalidSample(operation, "offered_concurrency_not_observed")
	sample.RootTaskIDHash = rootTaskIDHash
	sample.RootEpochBefore, sample.RootEpochAfter = initial.Epoch, before.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootSetDigest(initial), rootSetDigest(before)
	sample.Counters = map[string]int64{
		"barrier_clients":              snapshot.PeakBarrierWaiting,
		"service_clients_observed":     snapshot.UniqueParticipants,
		"offered_concurrency_observed": 0,
		"forced_queue_waiters":         0,
		"cas_attempts":                 0,
		"cas_conflicts":                0,
		"cas_retries":                  0,
	}
	sample.ConcurrencyVerification = &experiment.ConcurrencyVerification{
		Version: concurrencyEvidenceVersion, FixtureSHA256: concurrencyfixture.FixtureSHA256(),
		PlansSHA256: concurrencyfixture.PlansSHA256(), ProbeVersion: snapshot.Version,
		GatewayInstanceSHA256: snapshot.GatewayInstanceSHA256, RoundSHA256: roundSHA,
		RootTaskIDHash: rootTaskIDHash, ExpectedWidth: int64(cell.Width),
		HTTPActiveCapacity: capacity.HTTPActiveCapacity, HTTPQueueCapacity: capacity.HTTPQueueCapacity,
		ControlPoolCapacity: capacity.ControlPoolCapacity, ConnectorPoolCapacity: capacity.ConnectorPoolCapacity,
		ServiceArrivals: snapshot.Arrived, ServiceUniqueParticipants: snapshot.UniqueParticipants,
		ServiceParticipantSetSHA256: snapshot.ParticipantSetSHA256,
		ServicePeakBarrierWaiting:   snapshot.PeakBarrierWaiting, ServicePeakActive: snapshot.PeakActive,
		ServicePeakQueued: snapshot.PeakQueued, ServiceCompleted: snapshot.Completed,
		ServiceCanceled: snapshot.Canceled, ServiceRejected: snapshot.Rejected,
		PeakControlPoolInUse:       snapshot.PeakControlPoolInUse,
		ControlPoolWaitCountDelta:  snapshot.ControlPoolWaitCountDelta,
		ControlPoolWaitNanoseconds: snapshot.ControlPoolWaitNanoseconds,
		InitialRoot:                initial, BeforeBoundary: before, AtBoundary: before, AfterRejectedOverflow: before,
		ResourceBudgetProfile: concurrencyfixture.BudgetProfile,
		ResourceMaxQueries:    concurrencyfixture.ResourceMaxQueries,
		BudgetLimit:           concurrencyfixture.RootBudgetLimit, UsageBefore: before.OutcomeCardinality,
		UsageAfter: before.OutcomeCardinality, ExpectedResultSHA256: concurrencyfixture.ExpectedContenderResultSHA256(),
	}
	return sample
}

// observedConcurrencyServiceSample upgrades only the authenticated service
// portion of a partial sample. Later root, contender, and overflow fields are
// intentionally left at their last independently observed values.
func observedConcurrencyServiceSample(operation experiment.AdapterOperation, capacity gatewayapp.ConcurrencyProbeCapacity,
	snapshot gatewayapp.ConcurrencyProbeSnapshot, roundSHA, rootTaskIDHash string,
	initial, before experiment.RootLedgerSnapshot, rootLockWaiters int64) experiment.Sample {
	sample := partialConcurrencyServiceSample(operation, capacity, snapshot, roundSHA, rootTaskIDHash, initial, before)
	sample.Counters["offered_concurrency_observed"] = 1
	sample.Counters["forced_queue_waiters"] = rootLockWaiters
	sample.ConcurrencyVerification.RootLockWaitersObserved = rootLockWaiters
	return sample
}

// updateConcurrencyRetainedRoot records a root boundary only after the real
// Control snapshot has been read. It does not claim that the B+1 overflow was
// checked; AfterRejectedOverflow remains at the preceding observed boundary
// until executeOverflow returns one.
func updateConcurrencyRetainedRoot(sample *experiment.Sample, atBoundary experiment.RootLedgerSnapshot) {
	if sample == nil || sample.ConcurrencyVerification == nil {
		return
	}
	sample.ConcurrencyVerification.AtBoundary = atBoundary
	sample.ConcurrencyVerification.UsageAfter = atBoundary.OutcomeCardinality
	sample.RootEpochAfter = atBoundary.Epoch
	sample.RootSetSHA256After = rootSetDigest(atBoundary)
}

// retainVerifiedConcurrencyPrefix merges the compact evidence for only those
// contenders that completed the independent V8/result/artifact verifier. A
// representative sample is copied only after at least one contender passed,
// so late failures retain its real timing/result/receipt/artifact evidence.
func retainVerifiedConcurrencyPrefix(prefix, representative experiment.Sample,
	contenders []experiment.ConcurrencyContenderEvidence, atBoundary experiment.RootLedgerSnapshot) experiment.Sample {
	evidence := prefix.ConcurrencyVerification
	counters := prefix.Counters
	if representative.SchemaVersion != 0 && len(contenders) > 0 {
		prefix = representative
		prefix.ConcurrencyVerification = evidence
		prefix.Counters = counters
	}
	if prefix.ConcurrencyVerification == nil {
		return prefix
	}
	updateConcurrencyRetainedRoot(&prefix, atBoundary)

	retained := append([]experiment.ConcurrencyContenderEvidence(nil), contenders...)
	prefix.ConcurrencyVerification.Contenders = retained
	prefix.ConcurrencyVerification.Accepted = int64(len(retained))
	requestDigests := make([]string, 0, len(retained))
	var attempts, conflicts, retries, charged, zero int64
	for _, contender := range retained {
		requestDigests = append(requestDigests, contender.RequestIDHash)
		attempts += contender.CASAttempts
		conflicts += contender.CASConflicts
		retries += contender.CASRetries
		charged += contender.ChargedOutcomeFacts
		if contender.ChargedOutcomeFacts == 0 {
			zero++
		}
	}
	if len(requestDigests) > 0 {
		prefix.ConcurrencyVerification.ContenderRequestSetSHA256 = concurrencyStringSetSHA256(requestDigests)
	}
	prefix.ConcurrencyVerification.ProductionCASAttempts = attempts
	prefix.ConcurrencyVerification.ProductionCASConflicts = conflicts
	prefix.ConcurrencyVerification.ProductionCASRetries = retries
	prefix.ConcurrencyVerification.ChargedWinners = charged
	prefix.ConcurrencyVerification.ZeroNoveltySettlements = zero
	if prefix.Mode == "natural_contention" {
		prefix.ConcurrencyVerification.NaturalCASAttempts = attempts
		prefix.ConcurrencyVerification.NaturalCASConflicts = conflicts
		prefix.ConcurrencyVerification.NaturalCASRetries = retries
	}
	if prefix.Counters == nil {
		prefix.Counters = map[string]int64{}
	}
	prefix.Counters["cas_attempts"] = attempts
	prefix.Counters["cas_conflicts"] = conflicts
	prefix.Counters["cas_retries"] = retries
	return prefix
}

func (backend *realConcurrencyBackend) executeOverflow(ctx context.Context, operation experiment.AdapterOperation,
	taskID string, atBoundary experiment.RootLedgerSnapshot) (experiment.ConcurrencyOverflowEvidence,
	experiment.RootLedgerSnapshot, error) {
	var evidence experiment.ConcurrencyOverflowEvidence
	identity := concurrencyRoundIdentity(operation)
	requestID := concurrencyfixture.RequestID(identity, "overflow", 1)
	var ignored queryResponse
	callErr := backend.real.alice.call(ctx, "query_sql", map[string]any{
		"task_id": taskID, "request_id": requestID, "sql": concurrencyfixture.OverflowSQL,
	}, &ignored)
	var structured *mcpCallError
	if !errors.As(callErr, &structured) || structured.Code != "EXPOSURE_BUDGET_EXHAUSTED" {
		if callErr == nil {
			callErr = errors.New("B+1 overflow unexpectedly released a result")
		}
		return evidence, experiment.RootLedgerSnapshot{}, fmt.Errorf("exact B+1 overflow: %w", callErr)
	}
	evidence.Attempts, evidence.Rejected, evidence.ErrorCode = 1, 1, structured.Code

	deadlineCtx, cancel := context.WithTimeout(ctx, backend.real.timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var rawQueryID string
	var receiptJSON []byte
	for {
		err := backend.real.control.QueryRow(deadlineCtx, `
SELECT q.id,q.status,COALESCE(q.error_code,''),COALESCE(q.result_sha256,''),
 COALESCE((SELECT status FROM v5_query_exposure_reservations r WHERE r.query_id=q.id),''),
 (SELECT count(*) FROM encrypted_query_results r WHERE r.query_id=q.id),
 (SELECT count(*) FROM encrypted_query_result_chunks c WHERE c.query_id=q.id),
 (SELECT count(*) FROM v5_committed_materializations m WHERE m.source_query_id=q.id),
 (SELECT count(*) FROM v5_query_observations o WHERE o.query_id=q.id),
 (SELECT count(*) FROM v5_root_observations o WHERE o.first_query_id=q.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.query_id=q.id),
 (SELECT count(*) FROM result_artifacts a WHERE a.query_id=q.id AND a.status='AVAILABLE'),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_RESULT_CONSUMED'),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type IN
   ('QUERY_COMPLETED','QUERY_BUDGET_RELEASED','QUERY_RESULT_OBJECT_REGISTERED','QUERY_RESULT_CONSUMED',
    'QUERY_V5_EXPOSURE_SETTLED','QUERY_V5_SEMANTIC_REPLAY')),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_RELEASED'),
 (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_FAILED'),
 (SELECT count(*) FROM query_receipts r WHERE r.query_id=q.id),
 COALESCE((SELECT receipt_json FROM query_receipts r WHERE r.query_id=q.id),''::bytea)
FROM query_records q WHERE q.task_id=$1 AND q.request_id=$2`, taskID, requestID).Scan(
			&rawQueryID, &evidence.Status, &evidence.ErrorCode, &evidence.ResultSHA256,
			&evidence.ReservationStatus, &evidence.EncryptedResults, &evidence.EncryptedChunks,
			&evidence.Materializations, &evidence.QueryObservations, &evidence.RootObservations,
			&evidence.Artifacts, &evidence.AvailableArtifacts, &evidence.AvailabilityAudits,
			&evidence.SuccessfulAudits, &evidence.ReleaseAudits, &evidence.FailureAudits,
			&evidence.Receipts, &receiptJSON)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return evidence, experiment.RootLedgerSnapshot{}, err
		}
		if err == nil && evidence.Status != "RESERVED" && evidence.Receipts == 1 {
			break
		}
		select {
		case <-deadlineCtx.Done():
			return evidence, experiment.RootLedgerSnapshot{}, fmt.Errorf("B+1 failure evidence did not become terminal: %w", deadlineCtx.Err())
		case <-ticker.C:
		}
	}
	evidence.Found = true
	evidence.QueryIDHash = saltedIdentityHash(operation, "query", rawQueryID)
	var receipt queryreceipt.QueryReceiptV1
	if len(receiptJSON) == 0 || json.Unmarshal(receiptJSON, &receipt) != nil || backend.real.verifier.Verify(receipt) != nil ||
		receipt.ReceiptID != rawQueryID || receipt.QueryID != rawQueryID || receipt.TaskID != taskID ||
		receipt.RequestID != requestID || receipt.Status != "FAILED" || receipt.ErrorCode != evidence.ErrorCode ||
		receipt.ResultHash != "" || receipt.ArtifactIntent != nil || receipt.Exposure != nil {
		return evidence, experiment.RootLedgerSnapshot{}, errors.New("B+1 terminal failure receipt is absent or invalid")
	}
	if evidence.Status != "FAILED" || evidence.ErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" ||
		evidence.ReservationStatus != "RELEASED" || evidence.ResultSHA256 != "" ||
		evidence.EncryptedResults != 0 || evidence.EncryptedChunks != 0 || evidence.Materializations != 0 ||
		evidence.QueryObservations != 0 || evidence.RootObservations != 0 || evidence.Artifacts != 0 ||
		evidence.AvailableArtifacts != 0 || evidence.AvailabilityAudits != 0 || evidence.SuccessfulAudits != 0 ||
		evidence.ReleaseAudits != 1 || evidence.FailureAudits != 1 || evidence.Receipts != 1 {
		return evidence, experiment.RootLedgerSnapshot{}, errors.New("B+1 retained projection is not failure-only")
	}
	after, err := backend.real.rootLedgerSnapshot(ctx, taskID)
	if err != nil {
		return evidence, experiment.RootLedgerSnapshot{}, err
	}
	if after != atBoundary {
		return evidence, after, errors.New("B+1 rejected overflow mutated the V5 root")
	}
	return evidence, after, nil
}

func buildConcurrencySample(operation experiment.AdapterOperation, capacity gatewayapp.ConcurrencyProbeCapacity,
	snapshot gatewayapp.ConcurrencyProbeSnapshot, created concurrencyCreatedTask, rootTaskIDHash string,
	initial, beforeBoundary, atBoundary, afterOverflow experiment.RootLedgerSnapshot, rootLockWaiters int64,
	finalMembers []string, contenders []experiment.ConcurrencyContenderEvidence, representative experiment.Sample,
	availableMS, fullDrainMS float64, overflow experiment.ConcurrencyOverflowEvidence) experiment.Sample {
	sample := representative
	sample.ClientAvailableMS, sample.ClientFullDrainMS = availableMS, fullDrainMS
	sample.RootTaskIDHash = rootTaskIDHash
	sample.RootEpochBefore, sample.RootEpochAfter = beforeBoundary.Epoch, atBoundary.Epoch
	sample.RootSetSHA256Before, sample.RootSetSHA256After = rootSetDigest(beforeBoundary), rootSetDigest(atBoundary)
	sample.Status, sample.ErrorCode, sample.Reason = "pass", "", ""
	// Every contender has already passed the full composite verifier. Retain
	// its compact manifest below, not four repeated audit-chain suffixes in the
	// sample-level BaselineVerification (which can grow quadratically at 500).
	sample.BaselineVerification = nil

	var attempts, conflicts, retries, charged, zero int64
	requestDigests := make([]string, 0, len(contenders))
	for _, contender := range contenders {
		attempts += contender.CASAttempts
		conflicts += contender.CASConflicts
		retries += contender.CASRetries
		charged += contender.ChargedOutcomeFacts
		if contender.ChargedOutcomeFacts == 0 {
			zero++
		}
		requestDigests = append(requestDigests, contender.RequestIDHash)
	}
	sample.Counters = map[string]int64{
		"barrier_clients":              snapshot.PeakBarrierWaiting,
		"service_clients_observed":     snapshot.UniqueParticipants,
		"offered_concurrency_observed": 1,
		"forced_queue_waiters":         rootLockWaiters,
		"cas_attempts":                 attempts,
		"cas_conflicts":                conflicts,
		"cas_retries":                  retries,
	}
	naturalAttempts, naturalConflicts, naturalRetries := int64(0), int64(0), int64(0)
	if operation.Mode == "natural_contention" {
		naturalAttempts, naturalConflicts, naturalRetries = attempts, conflicts, retries
	}
	sample.ConcurrencyVerification = &experiment.ConcurrencyVerification{
		Version: concurrencyEvidenceVersion, FixtureSHA256: concurrencyfixture.FixtureSHA256(),
		PlansSHA256: concurrencyfixture.PlansSHA256(), ProbeVersion: snapshot.Version,
		GatewayInstanceSHA256: snapshot.GatewayInstanceSHA256, RoundSHA256: snapshot.RoundSHA256,
		RootTaskIDHash: rootTaskIDHash, ContenderRequestSetSHA256: concurrencyStringSetSHA256(requestDigests),
		ExpectedWidth: int64(len(contenders)), HTTPActiveCapacity: capacity.HTTPActiveCapacity,
		HTTPQueueCapacity: capacity.HTTPQueueCapacity, ControlPoolCapacity: capacity.ControlPoolCapacity,
		ConnectorPoolCapacity: capacity.ConnectorPoolCapacity,
		ServiceArrivals:       snapshot.Arrived, ServiceUniqueParticipants: snapshot.UniqueParticipants,
		ServiceParticipantSetSHA256: snapshot.ParticipantSetSHA256,
		ServicePeakBarrierWaiting:   snapshot.PeakBarrierWaiting, ServicePeakActive: snapshot.PeakActive,
		ServicePeakQueued: snapshot.PeakQueued, ServiceCompleted: snapshot.Completed,
		ServiceCanceled: snapshot.Canceled, ServiceRejected: snapshot.Rejected,
		PeakControlPoolInUse:       snapshot.PeakControlPoolInUse,
		ControlPoolWaitCountDelta:  snapshot.ControlPoolWaitCountDelta,
		ControlPoolWaitNanoseconds: snapshot.ControlPoolWaitNanoseconds,
		RootLockWaitersObserved:    rootLockWaiters,
		ProductionCASAttempts:      attempts, ProductionCASConflicts: conflicts, ProductionCASRetries: retries,
		NaturalCASAttempts: naturalAttempts, NaturalCASConflicts: naturalConflicts, NaturalCASRetries: naturalRetries,
		InitialRoot: initial, BeforeBoundary: beforeBoundary, AtBoundary: atBoundary,
		AfterRejectedOverflow: afterOverflow, ResourceBudgetProfile: created.BudgetProfile,
		ResourceMaxQueries: created.Budget.MaxQueries, BudgetLimit: created.Budget.MaxOutcomeFacts,
		UsageBefore: beforeBoundary.OutcomeCardinality, Accepted: int64(len(contenders)), Rejected: 0,
		UsageAfter: atBoundary.OutcomeCardinality, ChargedWinners: charged,
		ZeroNoveltySettlements: zero, ExpectedResultSHA256: concurrencyfixture.ExpectedContenderResultSHA256(),
		FinalRootFactHashes: append([]string(nil), finalMembers...), FinalRootSetSHA256: atBoundary.OutcomeSetSHA256,
		Contenders: append([]experiment.ConcurrencyContenderEvidence(nil), contenders...), Overflow: overflow,
	}
	return sample
}

func concurrencyStringSetSHA256(values []string) string {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for value := range unique {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ROOT-SET-V1\x00"))
	for _, value := range ordered {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
