package gateway

import (
	"context"
	"errors"
	"io"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

const ordinalReplayPreparationFailed = "ORDINAL_REPLAY_PREPARATION_FAILED"

// ordinalReplayOutcome makes reservation ownership explicit. A normal cache
// miss is the only outcome that returns the still-RESERVED query to the caller.
// Terminated means this function has initiated RELEASED/FAILED settlement;
// Completed means the atomic V4 finalization committed, even if building the
// public response subsequently returns an error.
type ordinalReplayOutcome uint8

const (
	ordinalReplayTerminated ordinalReplayOutcome = iota
	ordinalReplayContinueNovel
	ordinalReplayCompleted
)

type ordinalReplaySpool interface {
	io.Writer
	Seal() error
	Spilled() bool
	Open() (io.ReadCloser, error)
	Bytes() ([]byte, error)
	Close() error
}

type ordinalReplaySpoolFactory func(baseDir, taskID, queryID string, threshold int64) (ordinalReplaySpool, error)

func newOrdinalReplaySpool(baseDir, taskID, queryID string, threshold int64) (ordinalReplaySpool, error) {
	return newEncryptedQuerySpool(baseDir, taskID, queryID, threshold)
}

// tryOrdinalSemanticReplay is deliberately placed after authorization and
// resource reservation. A hit reuses only committed observation/result
// materialization; the new request receives a fresh query record, audit event,
// receipt, query-AAD encryption and query/row resource charge.
// tryOrdinalSemanticReplay is the entry point for callers that hold no prepared
// operation, and therefore for callers whose replay may not complete.
//
// semanticBinding is a parameter rather than nil because a replay that hits
// settles a COMPLETED query, and a completed query states which physical
// statements it authorized. A caller with no binding to supply can still use
// this to exercise a miss or a failure; a hit will be refused when the receipt
// is signed, which is the right place for that refusal rather than a silently
// undescribed settlement.
func (s *Service) tryOrdinalSemanticReplay(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64, semanticBinding *control.QueryExecutionBinding) (
	map[string]any, ordinalReplayOutcome, error) {
	return s.tryOrdinalSemanticReplayWithSpoolAndMetadata(ctx, task, requestID, queryID, grantDigest, cacheKey,
		dictionarySetDigest, reservation, componentMS, nil, newOrdinalReplaySpool, nil, semanticBinding)
}

func (s *Service) tryOrdinalSemanticReplayForQuery(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64, metadata *queryResponseMetadata, pipeline *queryPipelineMeasurement,
	semanticBinding *control.QueryExecutionBinding) (map[string]any, ordinalReplayOutcome, error) {
	return s.tryOrdinalSemanticReplayWithSpoolAndMetadata(ctx, task, requestID, queryID, grantDigest, cacheKey,
		dictionarySetDigest, reservation, componentMS, metadata, newOrdinalReplaySpool, pipeline, semanticBinding)
}

func (s *Service) tryOrdinalSemanticReplayWithSpool(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64, spoolFactory ordinalReplaySpoolFactory,
	semanticBinding *control.QueryExecutionBinding) (
	response map[string]any, outcome ordinalReplayOutcome, replayErr error) {
	return s.tryOrdinalSemanticReplayWithSpoolAndMetadata(ctx, task, requestID, queryID, grantDigest, cacheKey,
		dictionarySetDigest, reservation, componentMS, nil, spoolFactory, nil, semanticBinding)
}

func (s *Service) tryOrdinalSemanticReplayWithSpoolAndMetadata(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64, metadata *queryResponseMetadata, spoolFactory ordinalReplaySpoolFactory,
	pipeline *queryPipelineMeasurement, semanticBinding *control.QueryExecutionBinding) (
	response map[string]any, outcome ordinalReplayOutcome, replayErr error) {
	// Until either a normal miss transfers ownership back to executeSQL or the
	// atomic finalizer commits, every error must terminally settle this fresh
	// reservation. The cleanup mode changes to FAILED only after a committed
	// source result has been authenticated and validated as a replay hit.
	ownsReservation := true
	failOnAbort := false
	abortSettlement := control.BudgetSettlement{QueryID: queryID}
	defer func() {
		if !ownsReservation {
			return
		}
		outcome = ordinalReplayTerminated
		if failOnAbort {
			s.failQueryBudget(ctx, abortSettlement)
			return
		}
		s.releaseQueryBudget(ctx, queryID, ordinalReplayPreparationFailed)
	}()

	continueNovel := func() (map[string]any, ordinalReplayOutcome, error) {
		ownsReservation = false
		return nil, ordinalReplayContinueNovel, nil
	}
	if spoolFactory == nil {
		return nil, ordinalReplayTerminated, errors.New("ordinal replay spool factory is unavailable")
	}
	started := time.Now()
	lookup := control.OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: task.ID, GrantDigest: grantDigest,
		CatalogDigest: s.catalog.SHA256, DictionarySetDigest: dictionarySetDigest,
	}
	if reservation.Exposure != nil {
		lookup.ProfileVersion = reservation.Exposure.ProfileVersion
	}
	materialization, err := s.store.LookupOrdinalMaterialization(ctx, lookup)
	if errors.Is(err, control.ErrNotFound) {
		// Retention purge, expiry, and key erasure intentionally turn replay into
		// a miss. Remove only an unusable row with this exact authorization binding
		// so the novel path can publish its fresh committed materialization.
		if _, evictErr := s.store.DeleteUnusableOrdinalMaterialization(ctx, lookup); evictErr != nil {
			return nil, ordinalReplayTerminated, evictErr
		}
		componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
		return continueNovel()
	}
	if err != nil {
		return nil, ordinalReplayTerminated, err
	}
	if pipeline != nil {
		pipeline.prepareFinished = time.Now()
	}
	var stored storedQueryResult
	var sourceErr error
	var legacyDecodeErr error
	if s.resultArtifacts != nil {
		var artifact control.ResultArtifact
		artifact, sourceErr = s.store.GetResultArtifactByQuery(ctx, materialization.SourceQueryID)
		if sourceErr == nil {
			stored, sourceErr = s.loadArtifactResult(ctx, artifact, artifact.ParquetSize)
		}
	}
	if s.resultArtifacts == nil || errors.Is(sourceErr, control.ErrNotFound) {
		var plaintext []byte
		_, plaintext, sourceErr = s.store.GetEncryptedResult(ctx, task.ID, materialization.SourceQueryID)
		if sourceErr == nil {
			stored, legacyDecodeErr = decodeStoredQueryResult(plaintext)
		}
	}
	if sourceErr != nil {
		if !errors.Is(sourceErr, control.ErrNotFound) && !errors.Is(sourceErr, control.ErrCipherUnavailable) &&
			!errors.Is(sourceErr, control.ErrCiphertextInvalid) && !errors.Is(sourceErr, resultartifact.ErrObjectNotFound) &&
			!errors.Is(sourceErr, resultartifact.ErrArtifactIntegrity) {
			return nil, ordinalReplayTerminated, sourceErr
		}
		// Result cleanup or key erasure racing the lookup is a deterministic
		// miss only after eviction succeeds. Never ignore an eviction failure:
		// returning while RESERVED would block every later query for this task.
		if evictErr := s.store.DeleteOrdinalMaterialization(ctx, task.ID, cacheKey); evictErr != nil &&
			!errors.Is(evictErr, control.ErrNotFound) {
			return nil, ordinalReplayTerminated, evictErr
		}
		componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
		return continueNovel()
	}
	if legacyDecodeErr != nil || stored.RowCount != materialization.RowCount ||
		stored.RowCount < 0 || stored.RowCount > reservation.AllowedRows || stored.RowCount != int64(len(stored.Rows)) {
		if evictErr := s.store.DeleteOrdinalMaterialization(ctx, task.ID, cacheKey); evictErr != nil &&
			!errors.Is(evictErr, control.ErrNotFound) {
			return nil, ordinalReplayTerminated, evictErr
		}
		componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
		return continueNovel()
	}
	if metadata != nil && len(metadata.SemanticColumns) > 0 {
		if alignErr := alignStoredSemanticColumns(&stored, metadata.SemanticColumns); alignErr != nil {
			if evictErr := s.store.DeleteOrdinalMaterialization(ctx, task.ID, cacheKey); evictErr != nil &&
				!errors.Is(evictErr, control.ErrNotFound) {
				return nil, ordinalReplayTerminated, evictErr
			}
			componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
			return continueNovel()
		}
	}
	if err := applyQueryResponseMetadata(&stored, metadata); err != nil {
		return nil, ordinalReplayTerminated, err
	}
	// From this point onward replay has consumed a committed, authenticated
	// source result. A local encoding/finalization failure is charged as FAILED
	// with bounded replay usage, matching the novel result-finalization path.
	failOnAbort = true
	abortSettlement.Rows = stored.RowCount
	abortSettlement.DBMS = 1
	abortSettlement.ErrorCode = resultEncodingFailed
	if stored.ComponentMS == nil {
		stored.ComponentMS = make(map[string]float64)
	}
	componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
	componentMS["semantic_replay"] = componentMS["semantic_replay_lookup"]
	stored.ComponentMS = componentMS
	stored.DatabaseMS = 1
	if s.resultArtifacts != nil {
		if pipeline != nil {
			pipeline.executeFinished = time.Now()
		}
		settlement := control.BudgetSettlement{
			QueryID: queryID, Rows: stored.RowCount, DBMS: 1, ObservedDBMS: 0,
			OrdinalObservationRef: &control.OrdinalObservationReference{
				ObservationSHA256:   materialization.Observation.ObservationSHA256,
				DictionarySetDigest: materialization.Observation.DictionarySetDigest,
			},
			// A semantic replay authorizes both targets -- deriving the replay key
			// requires it -- and executes neither. Its binding records exactly that,
			// so the observer must see a zero target delta for this query and the
			// path is distinguishable from a novel execution that happened to
			// produce the same rows.
			ExecutionBinding: semanticBinding,
		}
		// finalizeArtifactQuery owns every terminal settlement from here.
		ownsReservation = false
		result, finalizeErr := s.finalizeArtifactQuery(ctx, task, requestID, settlement, stored, componentMS, pipeline)
		if finalizeErr != nil {
			record, recordErr := s.store.GetQuery(context.WithoutCancel(ctx), queryID)
			if recordErr == nil && record.Status == control.QueryCompleted {
				return nil, ordinalReplayCompleted, finalizeErr
			}
			return nil, ordinalReplayTerminated, finalizeErr
		}
		result["semantic_replay"] = true
		return result, ordinalReplayCompleted, nil
	}
	encodingStarted := time.Now()
	resultSpool, err := spoolFactory(s.spoolDirectory, task.ID, queryID, s.spoolThreshold)
	if err == nil {
		err = writeStoredQueryResult(resultSpool, stored)
	}
	if err == nil {
		err = resultSpool.Seal()
	}
	componentMS["result_encoding"] = durationMS(time.Since(encodingStarted))
	if err != nil {
		if resultSpool != nil {
			_ = resultSpool.Close()
		}
		return nil, ordinalReplayTerminated, err
	}
	defer resultSpool.Close()
	settlement := control.BudgetSettlement{
		QueryID: queryID, Rows: stored.RowCount, DBMS: 1, ObservedDBMS: 0,
		OrdinalObservationRef: &control.OrdinalObservationReference{
			ObservationSHA256:   materialization.Observation.ObservationSHA256,
			DictionarySetDigest: materialization.Observation.DictionarySetDigest,
		},
		ExecutionBinding: semanticBinding,
	}
	finalizeCtx, cancel := s.detachedContext(ctx)
	var record control.QueryRecord
	var persistedReceipt control.PersistedQueryReceipt
	var metrics control.FinalizeQueryMetrics
	abortSettlement.ErrorCode = resultFinalizationFailed
	if resultSpool.Spilled() {
		var plaintextReader io.ReadCloser
		plaintextReader, err = resultSpool.Open()
		if err == nil {
			record, persistedReceipt, metrics, err = s.store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(finalizeCtx,
				settlement, plaintextReader, nil, s.terminalReceiptBuilder())
			_ = plaintextReader.Close()
		}
	} else {
		var encoded []byte
		encoded, err = resultSpool.Bytes()
		if err == nil {
			record, persistedReceipt, metrics, err = s.store.FinalizeOrdinalQueryMeasuredWithReceipt(finalizeCtx,
				settlement, encoded, nil, s.terminalReceiptBuilder())
		}
	}
	cancel()
	if err != nil {
		return nil, ordinalReplayTerminated, err
	}
	// The root ledger, query result, audit event, and receipt are committed in
	// one transaction. Relinquish cleanup ownership before any response-only
	// decoding/readback so an error below can never attempt to release or fail
	// an already COMPLETED reservation.
	ownsReservation = false
	outcome = ordinalReplayCompleted
	componentMS["encryption"] = durationMS(metrics.Encryption)
	componentMS["settle_persist"] = durationMS(metrics.SettlementStore)
	componentMS["receipt_signing"] = durationMS(metrics.ReceiptSigning)
	componentMS["exposure_reservation_lock"] = durationMS(metrics.ExposureReservationLock)
	componentMS["exposure_ledger_lock"] = durationMS(metrics.ExposureLedgerLock)
	componentMS["exposure_fact_store"] = durationMS(metrics.ExposureFactStore)
	receipt, err := decodeReceiptJSON(persistedReceipt.ReceiptJSON)
	if err != nil {
		return nil, ordinalReplayCompleted, err
	}
	result := map[string]any{
		"task_id": task.ID, "query_id": queryID, "request_id": requestID, "status": record.Status,
		"rows": stored.Rows, "row_count": stored.RowCount,
		"database_ms": stored.DatabaseMS, "component_ms": componentMS, "limited": stored.Limited,
		"receipt": receipt, "semantic_replay": true,
	}
	if err := addStoredResponseMetadata(result, stored); err != nil {
		return nil, ordinalReplayCompleted, err
	}
	charge, chargeErr := s.store.GetExposureCharge(ctx, record.ID)
	if chargeErr != nil {
		return nil, ordinalReplayCompleted, &mcp.ToolError{Code: apierr.CodeConflict, Message: "ordinal replay 已提交但暴露证据不可读取"}
	}
	result["exposure"] = charge
	if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
		result["exposure_budget"] = ledger
	}
	return result, ordinalReplayCompleted, nil
}
