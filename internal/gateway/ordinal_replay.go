package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
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
func (s *Service) tryOrdinalSemanticReplay(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64) (map[string]any, ordinalReplayOutcome, error) {
	return s.tryOrdinalSemanticReplayWithSpool(ctx, task, requestID, queryID, grantDigest, cacheKey,
		dictionarySetDigest, reservation, componentMS, newOrdinalReplaySpool)
}

func (s *Service) tryOrdinalSemanticReplayWithSpool(ctx context.Context, task control.Task, requestID, queryID,
	grantDigest, cacheKey, dictionarySetDigest string, reservation control.BudgetReservation,
	componentMS map[string]float64, spoolFactory ordinalReplaySpoolFactory) (
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
		return nil, ordinalReplayTerminated, errors.New("V4 replay spool factory is unavailable")
	}
	started := time.Now()
	lookup := control.OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: task.ID, GrantDigest: grantDigest,
		CatalogDigest: s.catalog.SHA256, DictionarySetDigest: dictionarySetDigest,
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
	_, plaintext, err := s.store.GetEncryptedResult(ctx, task.ID, materialization.SourceQueryID)
	if err != nil {
		if !errors.Is(err, control.ErrNotFound) && !errors.Is(err, control.ErrCipherUnavailable) {
			return nil, ordinalReplayTerminated, err
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
	var stored storedQueryResult
	if err := json.Unmarshal(plaintext, &stored); err != nil || stored.RowCount != materialization.RowCount ||
		stored.RowCount < 0 || stored.RowCount > reservation.AllowedRows || stored.RowCount != int64(len(stored.Rows)) {
		if evictErr := s.store.DeleteOrdinalMaterialization(ctx, task.ID, cacheKey); evictErr != nil &&
			!errors.Is(evictErr, control.ErrNotFound) {
			return nil, ordinalReplayTerminated, evictErr
		}
		componentMS["semantic_replay_lookup"] = durationMS(time.Since(started))
		return continueNovel()
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
		"columns": stored.Columns, "rows": stored.Rows, "row_count": stored.RowCount,
		"database_ms": stored.DatabaseMS, "component_ms": componentMS, "limited": stored.Limited,
		"receipt": receipt, "semantic_replay": true,
	}
	charge, chargeErr := s.store.GetExposureCharge(ctx, record.ID)
	if chargeErr != nil {
		return nil, ordinalReplayCompleted, &mcp.ToolError{Code: apierr.CodeConflict, Message: "V4 replay 已提交但暴露证据不可读取"}
	}
	result["exposure"] = charge
	if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
		result["exposure_budget"] = ledger
	}
	return result, ordinalReplayCompleted, nil
}
