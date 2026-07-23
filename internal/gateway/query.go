package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type storedQueryResult struct {
	Columns     []dataconnector.Column `json:"columns"`
	Rows        [][]any                `json:"rows"`
	RowCount    int64                  `json:"row_count"`
	DatabaseMS  int64                  `json:"database_ms"`
	ComponentMS map[string]float64     `json:"component_ms,omitempty"`
	Limited     bool                   `json:"limited"`
}

const (
	resultEncodingFailed     = "RESULT_ENCODING_FAILED"
	resultFinalizationFailed = "RESULT_FINALIZATION_FAILED"
	settlementAttemptTimeout = 5 * time.Second
	settlementRetryDelay     = 100 * time.Millisecond
)

func (s *Service) querySQL(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID    string `json:"task_id"`
		RequestID string `json:"request_id"`
		SQL       string `json:"sql"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.SQL) == "" || len(args.SQL) > 100000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "sql 必须为 1 到 100000 个字符"}
	}
	if err := validateRequestID(args.RequestID); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	requestSummary := "query_sql\x00" + task.ID + "\x00" + args.SQL
	return s.executeSQL(ctx, principal, task, args.RequestID, args.SQL, requestSummary, nil)
}

func (s *Service) executePlan(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID       string              `json:"task_id"`
		RequestID    string              `json:"request_id"`
		Plan         queryplan.QueryPlan `json:"plan"`
		OutputFormat string              `json:"output_format,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.OutputFormat != "" && args.OutputFormat != "json" && args.OutputFormat != "table" {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "output_format 仅支持 json 或 table"}
	}
	if err := validateRequestID(args.RequestID); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	requestJSON, err := json.Marshal(args.Plan)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "QueryPlan 无法编码"}
	}
	requestSummary := "execute_plan\x00" + task.ID + "\x00" + string(requestJSON) + "\x00" + defaultString(args.OutputFormat, "json")
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, args.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		replayed, err := s.queryReplayResponse(ctx, existing)
		if err != nil {
			return nil, err
		}
		replayed["query_plan"] = args.Plan
		replayed["output_format"] = defaultString(args.OutputFormat, "json")
		return replayed, nil
	}
	if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	product, ok := s.catalog.LookupProduct(args.Plan.Product)
	if !ok || !contains(grant.ApprovedProducts, args.Plan.Product) {
		return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 请求了任务授权外的数据产品"}
	}
	columns := make(map[string]struct{}, len(grant.ApprovedColumns[args.Plan.Product]))
	for _, column := range grant.ApprovedColumns[args.Plan.Product] {
		columns[column] = struct{}{}
	}
	aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
	for _, aggregate := range product.AllowedAggregates {
		aggregates[strings.ToLower(aggregate)] = struct{}{}
	}
	compiled, err := queryplan.Compile(args.Plan, queryplan.Product{
		Name: args.Plan.Product, Columns: columns, AllowedAggregates: aggregates,
	})
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 无法在任务授权内编译"}
	}
	var exposureContext *planExposureContext
	if grant.Exposure.Enabled() {
		exposureContext, err = buildPlanExposureContext(args.Plan, product, columns, aggregates)
		if err != nil {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 不在可精确计量的数据暴露片段内"}
		}
		compiled = exposureContext.mainSQL
	}
	result, err := s.executeSQL(ctx, principal, task, args.RequestID, compiled, requestSummary, exposureContext)
	if err != nil {
		return nil, err
	}
	result.(map[string]any)["query_plan"] = args.Plan
	result.(map[string]any)["output_format"] = defaultString(args.OutputFormat, "json")
	return result, nil
}

func (s *Service) executeSQL(ctx context.Context, principal mcp.Principal, task control.Task, requestID, agentSQL, requestSummary string, exposureContext *planExposureContext) (any, error) {
	pipelineStarted := time.Now()
	requestDigest := digest(requestSummary)
	// An idempotent retry observes the first durable result/status even if the
	// task has since expired, been revoked, or exhausted its budget.
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, requestID)
	if lookupErr == nil {
		if existing.RequestDigest != requestDigest {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		return s.queryReplayResponse(ctx, existing)
	}
	if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	if task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务目录版本与当前实例不一致；为避免扩权已拒绝查询"}
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if grant.Exposure.Enabled() && exposureContext == nil {
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	protocolGrant, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil || approval.VerifyTaskGrantV1(s.receiptVerifier, protocolGrant) != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久授权证明无效；查询已关闭式拒绝"}
	}
	grantDigest, err := approval.GrantCoreDigest(protocolGrant.Core)
	if err != nil || !storedGrantMatchesProtocol(task, grant, protocolGrant) ||
		protocolGrant.Core.CatalogSHA256 != s.catalog.SHA256 {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权、目录或持久 Grant 不一致；查询已关闭式拒绝"}
	}
	if err := s.validateDelegatedGrant(ctx, task, protocolGrant.Core, s.clock().UTC()); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "委托 Grant 不再受有效父任务约束；查询已关闭式拒绝"}
	}
	grantRemaining := grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining < time.Millisecond {
		return nil, toolError(control.ErrTaskExpired)
	}
	persistedPending, err := decodePersistedPending(task)
	if err != nil {
		return nil, err
	}
	parentCore, err := domain.CoreFromManifest(
		persistedPending.Manifest,
		persistedPending.ManifestDigest,
		protocolGrant.ApprovalReceipt.IssuedAt,
	)
	if err != nil || persistedPending.ManifestDigest != protocolGrant.Core.ManifestDigest ||
		parentCore.CheckNarrowing(protocolGrant.Core) != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务上下文与已签名 Grant 不一致；查询已关闭式拒绝"}
	}
	budget, err := s.store.GetBudget(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	remaining := budget.Remaining()
	if remaining.Queries < 1 || remaining.Rows < 1 || remaining.DBMS < 1 {
		return nil, &mcp.ToolError{Code: apierr.CodeBudgetExhausted, Message: "任务预算已耗尽"}
	}
	componentMS := map[string]float64{}
	policyStarted := time.Now()
	policyGrant, err := s.policyGrant(grant)
	if err != nil {
		return nil, err
	}
	var exposureLedger control.ExposureLedgerSnapshot
	if exposureContext != nil {
		policyGrant, err = exposureContext.extendGrant(policyGrant)
		if err != nil {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		exposureLedger, err = s.store.GetExposureLedger(ctx, task.ID)
		if err != nil || exposureLedger.ProfileVersion != grant.Exposure.ProfileVersion {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	visibleRowLimit := remaining.Rows
	if exposureContext != nil && !exposureContext.grouped {
		visibleRowLimit = min64(visibleRowLimit, exposureLedger.Limits.InfluenceFacts)
	}
	decision, err := engine.Authorize(sqlpolicy.Request{SQL: agentSQL, Grant: policyGrant, RowLimit: visibleRowLimit})
	if err != nil {
		return nil, err
	}
	var provenanceDecision sqlpolicy.Decision
	var provenanceEvidenceRows int64
	if exposureContext != nil {
		provenanceEvidenceRows = decision.RowLimit
		provenancePolicyRows := provenanceEvidenceRows
		if exposureContext.grouped {
			provenanceEvidenceRows = exposureLedger.Limits.InfluenceFacts
			provenancePolicyRows = provenanceEvidenceRows + 1
		}
		if provenanceEvidenceRows < 1 {
			return nil, toolError(control.ErrExposureBudgetExhausted)
		}
		provenanceDecision, err = engine.Authorize(sqlpolicy.Request{
			SQL: exposureContext.provenanceSQL, Grant: policyGrant, RowLimit: provenancePolicyRows,
		})
		if err != nil {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
	}
	componentMS["parse_policy"] = durationMS(time.Since(policyStarted))
	componentMS["authorization"] = durationMS(policyStarted.Sub(pipelineStarted))
	evidence, err := s.datasourceEvidence(ctx, protocolGrant.Core.ApprovedProducts)
	if err != nil {
		return nil, err
	}
	if protocolGrant.Core.DatasourceID != evidence.DatasourceID ||
		protocolGrant.Core.SchemaDigest != evidence.SchemaDigest ||
		grant.DatasourceID != evidence.DatasourceID || grant.SchemaDigest != evidence.SchemaDigest {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权数据源与当前实例不一致；查询已关闭式拒绝"}
	}
	queryID := randomID("query")
	approvedPerQueryTimeout := time.Duration(protocolGrant.Core.Budget.PerQueryTimeoutMS) * time.Millisecond
	requestedTimeout := approvedPerQueryTimeout
	if requestedTimeout > grantRemaining {
		requestedTimeout = grantRemaining
	}
	requestedDBMS := min64(remaining.DBMS, requestedTimeout.Milliseconds())
	if requestedDBMS < 1 {
		return nil, toolError(control.ErrTaskExpired)
	}
	reserveStarted := time.Now()
	reserveRequest := control.ReserveRequest{
		QueryID: queryID, TaskID: task.ID, RequestID: requestID, Actor: principal.Subject,
		RequestDigest: requestDigest, SQLFingerprint: decision.Fingerprint,
		CatalogVersion: task.CatalogVersion, CatalogDigest: protocolGrant.Core.CatalogSHA256,
		DatasourceID: evidence.DatasourceID, SchemaDigest: evidence.SchemaDigest,
		ManifestDigest: protocolGrant.Core.ManifestDigest, GrantDigest: grantDigest, PolicyDecision: "ALLOW",
		RequestedRows: decision.RowLimit, RequestedDBMS: requestedDBMS,
	}
	if exposureContext != nil {
		reserveRequest.Exposure = &control.ExposureReservationRequest{
			ProfileVersion:          exposureLedger.ProfileVersion,
			EstimatedReleaseFacts:   saturatedProduct(decision.RowLimit, int64(len(exposureContext.visibleFields))),
			EstimatedInfluenceFacts: saturatedProduct(provenanceEvidenceRows, int64(len(exposureContext.provenanceFields)+1)),
		}
	}
	reservation, err := s.store.ReserveBudget(ctx, reserveRequest)
	if err != nil {
		return nil, err
	}
	componentMS["reserve"] = durationMS(time.Since(reserveStarted))
	if reservation.Replay && reservation.Record != nil {
		return s.queryReplayResponse(ctx, *reservation.Record)
	}
	timeout := time.Duration(reservation.AllowedDBMS) * time.Millisecond
	if timeout > approvedPerQueryTimeout {
		timeout = approvedPerQueryTimeout
	}
	grantRemaining = grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining <= 0 {
		// The connector has not been invoked, so this reservation may still be
		// released without charging query usage.
		s.releaseQueryBudget(ctx, queryID, "AUTHORIZATION_EXPIRED")
		return nil, toolError(control.ErrTaskExpired)
	}
	if timeout > grantRemaining {
		timeout = grantRemaining
	}
	queryTimeout := timeout + 250*time.Millisecond
	if queryTimeout > grantRemaining {
		queryTimeout = grantRemaining
	}
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	connectorStarted := time.Now()
	var data dataconnector.Result
	var provenanceData dataconnector.Result
	var queryErr error
	if exposureContext == nil {
		data, queryErr = s.connector.Query(queryCtx, dataconnector.QueryRequest{
			SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows,
		})
	} else {
		paired, ok := s.connector.(interface {
			QueryPair(context.Context, dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error)
		})
		if !ok {
			s.releaseQueryBudget(ctx, queryID, "EXPOSURE_SNAPSHOT_UNAVAILABLE")
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		pair, pairErr := paired.QueryPair(queryCtx, dataconnector.QueryPairRequest{
			Visible:    dataconnector.QueryRequest{SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows},
			Provenance: dataconnector.QueryRequest{SQL: provenanceDecision.SQL, StatementTimeout: timeout, MaxRows: provenanceEvidenceRows},
		})
		data, provenanceData, queryErr = pair.Visible, pair.Provenance, pairErr
	}
	connectorFinished := time.Now()
	businessDatabaseDuration := data.DatabaseTime
	provenanceDatabaseDuration := provenanceData.DatabaseTime
	if exposureContext != nil {
		data.DatabaseTime = businessDatabaseDuration + provenanceDatabaseDuration
	}
	settlement := querySettlement(queryID, data, connectorStarted, reservation)
	if queryErr != nil {
		code := string(dataconnector.CodeQueryFailed)
		var connectorErr *dataconnector.Error
		if errors.As(queryErr, &connectorErr) {
			code = string(connectorErr.Code)
		}
		settlement.ErrorCode = code
		if code == string(dataconnector.CodeConnection) || code == string(dataconnector.CodeInvalidQuery) {
			s.releaseQueryBudget(ctx, queryID, code)
		} else {
			s.markQueryIndeterminate(ctx, queryID, code)
		}
		return nil, queryErr
	}
	if exposureContext != nil {
		observation, deriveErr := exposureContext.deriveObservation(data, provenanceData, exposureLedger.ProfileVersion)
		if deriveErr != nil {
			settlement.ErrorCode = "EXPOSURE_PROVENANCE_INVALID"
			s.failQueryBudget(ctx, settlement)
			return nil, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "查询的来源证据不完整，因此结果未释放"}
		}
		settlement.Exposure = &observation
		data, err = exposureContext.visibleResult(data)
		if err != nil {
			settlement.ErrorCode = "EXPOSURE_RESULT_INVALID"
			s.failQueryBudget(ctx, settlement)
			return nil, err
		}
	}
	totalDatabaseDuration := data.DatabaseTime
	if totalDatabaseDuration <= 0 {
		totalDatabaseDuration = connectorFinished.Sub(connectorStarted)
	}
	if exposureContext == nil && businessDatabaseDuration <= 0 {
		businessDatabaseDuration = totalDatabaseDuration
	}
	connectorOverhead := connectorFinished.Sub(connectorStarted) - totalDatabaseDuration
	if connectorOverhead < 0 {
		connectorOverhead = 0
	}
	componentMS["business_postgresql"] = durationMS(businessDatabaseDuration)
	if exposureContext != nil {
		componentMS["provenance_postgresql"] = durationMS(provenanceDatabaseDuration)
	}
	componentMS["connector_overhead"] = durationMS(connectorOverhead)
	stored := storedQueryResult{
		Columns: data.Columns, Rows: data.Rows, RowCount: data.RowCount,
		DatabaseMS: settlement.DBMS, ComponentMS: componentMS,
		Limited: data.Truncated || data.RowCount == reservation.AllowedRows,
	}
	encodingStarted := time.Now()
	plaintext, err := json.Marshal(stored)
	componentMS["result_encoding"] = durationMS(time.Since(encodingStarted))
	if err != nil {
		settlement.ErrorCode = resultEncodingFailed
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	finalizeCtx, finalizeCancel := detachedContext(ctx)
	record, persistedReceipt, finalizeMetrics, err := s.store.FinalizeQueryMeasuredWithReceipt(finalizeCtx, settlement, plaintext, s.terminalReceiptBuilder())
	finalizeCancel()
	if err != nil {
		settlement.ErrorCode = resultFinalizationFailed
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	componentMS["encryption"] = durationMS(finalizeMetrics.Encryption)
	componentMS["settle_persist"] = durationMS(finalizeMetrics.SettlementStore)
	componentMS["receipt_signing"] = durationMS(finalizeMetrics.ReceiptSigning)
	receipt, err := decodeReceiptJSON(persistedReceipt.ReceiptJSON)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"task_id": task.ID, "query_id": queryID, "request_id": requestID, "status": record.Status, "columns": stored.Columns,
		"rows": stored.Rows, "row_count": stored.RowCount, "database_ms": stored.DatabaseMS,
		"component_ms": componentMS, "limited": stored.Limited, "receipt": receipt,
	}
	if charge, exposureErr := s.store.GetExposureCharge(ctx, record.ID); exposureErr == nil {
		result["exposure"] = charge
		if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
			result["exposure_budget"] = ledger
		}
	}
	return result, nil
}

func validateRequestID(requestID string) error {
	if requestID == "" || len(requestID) > 128 || strings.TrimSpace(requestID) != requestID {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "request_id 必须为 1 到 128 个非空白边界字符"}
	}
	for _, value := range requestID {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || strings.ContainsRune("._:-", value) {
			continue
		}
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "request_id 仅支持字母、数字、点、下划线、冒号和连字符"}
	}
	return nil
}

func (s *Service) queryReplayResponse(ctx context.Context, record control.QueryRecord) (map[string]any, error) {
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"task_id": record.TaskID, "query_id": record.ID, "request_id": record.RequestID,
		"status": record.Status, "receipt": receipt, "idempotent_replay": true,
	}
	if charge, exposureErr := s.store.GetExposureCharge(ctx, record.ID); exposureErr == nil {
		result["exposure"] = charge
		if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
			result["exposure_budget"] = ledger
		}
	}
	if record.Status != control.QueryCompleted || record.ResultSHA256 == "" {
		return result, nil
	}
	_, plaintext, err := s.store.GetEncryptedResult(ctx, record.TaskID, record.ID)
	if err != nil {
		// The query status remains the durable answer for a result that could not
		// be encoded or atomically stored after execution.
		return result, nil
	}
	var stored storedQueryResult
	if err := json.Unmarshal(plaintext, &stored); err != nil {
		return nil, err
	}
	result["columns"] = stored.Columns
	result["rows"] = stored.Rows
	result["row_count"] = stored.RowCount
	result["database_ms"] = stored.DatabaseMS
	result["component_ms"] = stored.ComponentMS
	result["limited"] = stored.Limited
	return result, nil
}

func durationMS(value time.Duration) float64 {
	if value <= 0 {
		return 0
	}
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
}

func querySettlement(queryID string, data dataconnector.Result, started time.Time, reservation control.BudgetReservation) control.BudgetSettlement {
	rows := data.RowCount
	if rows < 0 {
		rows = 0
	}
	if rows > reservation.AllowedRows {
		rows = reservation.AllowedRows
	}
	databaseMS := data.DatabaseTime.Milliseconds()
	if data.DatabaseTime <= 0 {
		databaseMS = time.Since(started).Milliseconds()
	}
	if databaseMS < 1 {
		databaseMS = 1
	}
	return control.BudgetSettlement{
		QueryID:      queryID,
		Rows:         rows,
		DBMS:         min64(databaseMS, reservation.AllowedDBMS),
		ObservedDBMS: databaseMS,
	}
}

func (s *Service) failQueryBudget(ctx context.Context, settlement control.BudgetSettlement) {
	failCtx, cancel := detachedContext(ctx)
	_, _, err := s.store.FailBudgetWithReceipt(failCtx, settlement, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	s.logger.Error("fail query budget", "trace_id", mcp.TraceID(ctx), "query_id", settlement.QueryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryFailedQuerySettlement(settlement)
}

func (s *Service) releaseQueryBudget(ctx context.Context, queryID, errorCode string) {
	releaseCtx, cancel := detachedContext(ctx)
	_, _, err := s.store.ReleaseBudgetWithReceipt(releaseCtx, queryID, errorCode, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	s.logger.Error("release query budget", "trace_id", mcp.TraceID(ctx), "query_id", queryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryTerminalQuery(queryID, errorCode, false)
}

func (s *Service) markQueryIndeterminate(ctx context.Context, queryID, errorCode string) {
	markCtx, cancel := detachedContext(ctx)
	_, _, err := s.store.MarkIndeterminateWithReceipt(markCtx, queryID, errorCode, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	s.logger.Error("mark query indeterminate", "trace_id", mcp.TraceID(ctx), "query_id", queryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryTerminalQuery(queryID, errorCode, true)
}

func (s *Service) retryTerminalQuery(queryID, errorCode string, indeterminate bool) {
	defer s.pendingSettles.Add(-1)
	delay := settlementRetryDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.background.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		attemptCtx, cancel := context.WithTimeout(s.background, settlementAttemptTimeout)
		var err error
		if indeterminate {
			_, _, err = s.store.MarkIndeterminateWithReceipt(attemptCtx, queryID, errorCode, s.terminalReceiptBuilder())
		} else {
			_, _, err = s.store.ReleaseBudgetWithReceipt(attemptCtx, queryID, errorCode, s.terminalReceiptBuilder())
		}
		cancel()
		if err == nil || errors.Is(err, control.ErrClosed) {
			return
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (s *Service) retryFailedQuerySettlement(settlement control.BudgetSettlement) {
	defer s.pendingSettles.Add(-1)
	delay := settlementRetryDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.background.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		attemptCtx, cancel := context.WithTimeout(s.background, settlementAttemptTimeout)
		_, _, err := s.store.FailBudgetWithReceipt(attemptCtx, settlement, s.terminalReceiptBuilder())
		cancel()
		if err == nil || errors.Is(err, control.ErrClosed) {
			return
		}
		s.logger.Error("retry failed query budget settlement", "query_id", settlement.QueryID, "error", err)
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (s *Service) policyGrant(grant control.TaskGrant) (sqlpolicy.Grant, error) {
	var scope map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
		return sqlpolicy.Grant{}, err
	}
	result := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(grant.ApprovedProducts))}
	for _, name := range grant.ApprovedProducts {
		product, ok := s.catalog.LookupProduct(name)
		if !ok {
			return sqlpolicy.Grant{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务引用了当前目录中不存在的数据产品"}
		}
		parts := strings.Split(product.ReportingView, ".")
		if len(parts) != 2 {
			return sqlpolicy.Grant{}, errors.New("validated catalog has invalid reporting view")
		}
		for _, scopeName := range product.Scopes {
			if _, present := scope[scopeName]; !present {
				return sqlpolicy.Grant{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务授权缺少目录要求的强制数据范围"}
			}
		}
		predicates, err := scopePredicates(product, scope)
		if err != nil {
			return sqlpolicy.Grant{}, err
		}
		result.Products = append(result.Products, sqlpolicy.ProductGrant{
			LogicalName: name, PhysicalSchema: parts[0], PhysicalView: parts[1],
			ApprovedColumns:   append([]string(nil), grant.ApprovedColumns[name]...),
			AllowedFunctions:  append([]string(nil), product.AllowedFunctions...),
			AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
			AllowedOperators:  append([]string(nil), product.AllowedOperators...),
			MandatoryScope:    predicates,
		})
	}
	return result, nil
}

func scopePredicates(product catalog.Product, scope map[string]any) ([]sqlpolicy.ScopePredicate, error) {
	fieldSet := make(map[string]struct{}, len(product.Fields))
	for _, field := range product.Fields {
		fieldSet[field.Name] = struct{}{}
	}
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	var predicates []sqlpolicy.ScopePredicate
	for _, name := range names {
		if _, relevant := fieldSet[name]; !relevant {
			continue
		}
		switch value := scope[name].(type) {
		case string:
			predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeEqual, Values: []string{value}})
		case []any:
			values := make([]string, 0, len(value))
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					return nil, errors.New("invalid stored enum scope")
				}
				values = append(values, text)
			}
			operator := sqlpolicy.ScopeIn
			if len(values) == 1 {
				operator = sqlpolicy.ScopeEqual
			}
			predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: operator, Values: values})
		case map[string]any:
			if from, _ := value["from"].(string); from != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeGreaterEqual, Values: []string{from}})
			}
			if to, _ := value["to"].(string); to != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeLessEqual, Values: []string{to}})
			}
		default:
			return nil, fmt.Errorf("invalid stored scope value")
		}
	}
	return predicates, nil
}

func (s *Service) getQueryResult(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID  string `json:"task_id"`
		QueryID string `json:"query_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.ownedTask(ctx, principal, args.TaskID); err != nil {
		return nil, err
	}
	record, err := s.store.GetQuery(ctx, args.QueryID)
	if err != nil || record.TaskID != args.TaskID || record.Actor != principal.Subject {
		return nil, notFound()
	}
	_, plaintext, err := s.store.GetEncryptedResult(ctx, args.TaskID, args.QueryID)
	if err != nil {
		return nil, err
	}
	var result storedQueryResult
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return nil, err
	}
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"task_id": args.TaskID, "query_id": args.QueryID, "columns": result.Columns,
		"rows": result.Rows, "row_count": result.RowCount, "database_ms": result.DatabaseMS,
		"limited": result.Limited, "receipt": receipt,
	}, nil
}

func (s *Service) listReceipts(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Cursor string `json:"cursor,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.ownedTask(ctx, principal, args.TaskID); err != nil {
		return nil, err
	}
	page, err := s.store.ListQueriesPage(ctx, args.TaskID, args.Cursor, 100)
	if err != nil {
		return nil, err
	}
	receipts := make([]map[string]any, 0, len(page.Records))
	for _, record := range page.Records {
		receipt, err := s.queryReceipt(ctx, record)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return map[string]any{"task_id": args.TaskID, "receipts": receipts, "next_cursor": page.NextCursor}, nil
}

func (s *Service) listAuditEvents(ctx context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID    string `json:"task_id,omitempty"`
		Actor     string `json:"actor,omitempty"`
		EventType string `json:"event_type,omitempty"`
		Cursor    string `json:"cursor,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	var after int64
	if args.Cursor != "" {
		value, err := strconv.ParseInt(args.Cursor, 10, 64)
		if err != nil || value < 0 {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "cursor 无效"}
		}
		after = value
	}
	events, err := s.store.ListAuditEvents(ctx, control.AuditFilter{TaskID: args.TaskID, Actor: args.Actor, EventType: args.EventType, After: after, Limit: 100})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(events))
	for _, event := range events {
		items = append(items, publicAuditEvent(event, true))
	}
	next := ""
	if len(events) == 100 {
		next = strconv.FormatInt(events[len(events)-1].Sequence, 10)
	}
	return map[string]any{"events": items, "next_cursor": next}, nil
}

func (s *Service) getAuditReceipt(ctx context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		ReceiptID string `json:"receipt_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	record, err := s.store.GetQuery(ctx, args.ReceiptID)
	if err != nil {
		return nil, err
	}
	evidence, err := s.store.GetQueryReceipt(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	events, err := s.store.ListAuditEventsForQuery(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	chain := make([]map[string]any, 0, len(events))
	for _, event := range events {
		chain = append(chain, publicAuditEvent(event, true))
	}
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	publicProof, typedProof, err := s.auditInclusionProof(ctx, evidence.Audit)
	if err != nil {
		return nil, err
	}
	signed, err := decodeSignedQueryReceipt(receipt)
	if err != nil {
		return nil, err
	}
	if err := queryreceipt.VerifyAuditInclusion(signed, typedProof); err != nil {
		return nil, err
	}
	return map[string]any{"receipt": receipt, "audit_chain_events": chain, "audit_inclusion": publicProof}, nil
}

func publicAuditEvent(event control.AuditEvent, includePayload bool) map[string]any {
	result := map[string]any{
		"sequence": event.Sequence, "event_id": event.EventID, "task_id": event.TaskID,
		"query_id": event.QueryID, "actor": event.Actor, "event_type": event.EventType,
		"occurred_at": event.OccurredAt, "previous_hash": event.PreviousHash, "current_hash": event.CurrentHash,
	}
	if includePayload {
		var payload any
		_ = json.Unmarshal(event.Payload, &payload)
		result["payload"] = payload
	}
	return result
}

func (s *Service) auditInclusionProof(ctx context.Context, terminal control.AuditEvent) (map[string]any, auditchain.InclusionProof, error) {
	checkpoint, err := s.store.AuditCheckpoint(ctx)
	if err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	proof := auditchain.InclusionProof{TerminalEvent: terminal, Checkpoint: checkpoint}
	var predecessor any
	if terminal.Sequence > 1 {
		event, err := s.store.GetAuditEvent(ctx, terminal.Sequence-1)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			return nil, auditchain.InclusionProof{}, err
		}
		if err == nil {
			proof.PredecessorEvent = &event
			predecessor = publicAuditEvent(event, true)
		}
	}
	successors, err := s.store.ListAuditEventsRange(ctx, terminal.Sequence, checkpoint.Sequence)
	if err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	proof.SuccessorEvents = successors
	publicSuccessors := make([]map[string]any, 0, len(successors))
	for _, event := range successors {
		publicSuccessors = append(publicSuccessors, publicAuditEvent(event, true))
	}
	if err := auditchain.VerifyInclusion(proof); err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	return map[string]any{
		"terminal_event":    publicAuditEvent(terminal, true),
		"predecessor_event": predecessor,
		"successor_events":  publicSuccessors,
		"checkpoint": map[string]any{
			"sequence": checkpoint.Sequence,
			"hash":     checkpoint.Hash,
		},
	}, proof, nil
}

func (s *Service) queryReceipt(ctx context.Context, record control.QueryRecord) (map[string]any, error) {
	if record.Status == control.QueryReserved {
		return unsignedPublicReceipt(record), nil
	}
	evidence, err := s.store.GetQueryReceipt(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	record = evidence.Query
	if evidence.Receipt != nil {
		return decodeReceiptJSON(evidence.Receipt.ReceiptJSON)
	}
	if record.BudgetAfter == nil || record.CompletedAt == nil {
		return nil, fmt.Errorf("terminal query is missing durable budget or timestamp evidence")
	}
	request, err := s.buildQueryReceiptRequest(control.QueryReceipt{Query: record, Audit: evidence.Audit}, s.clock().UTC())
	if err != nil {
		return nil, err
	}
	persisted, err := s.store.SaveQueryReceipt(ctx, request)
	if err != nil {
		return nil, err
	}
	return decodeReceiptJSON(persisted.ReceiptJSON)
}

func (s *Service) terminalReceiptBuilder() control.TerminalReceiptBuilder {
	return func(evidence control.QueryReceipt) (control.SaveQueryReceiptRequest, error) {
		return BuildQueryReceiptRequest(evidence, s.queryReceiptSigner, s.clock().UTC())
	}
}

func (s *Service) buildQueryReceiptRequest(evidence control.QueryReceipt, signedAt time.Time) (control.SaveQueryReceiptRequest, error) {
	return BuildQueryReceiptRequest(evidence, s.queryReceiptSigner, signedAt)
}

// BuildQueryReceiptRequest signs terminal query evidence and returns the
// immutable Control PG receipt row that should be saved with that evidence.
func BuildQueryReceiptRequest(evidence control.QueryReceipt, signer *queryreceipt.Signer, signedAt time.Time) (control.SaveQueryReceiptRequest, error) {
	record := evidence.Query
	if record.BudgetAfter == nil || record.CompletedAt == nil {
		return control.SaveQueryReceiptRequest{}, fmt.Errorf("terminal query is missing durable budget or timestamp evidence")
	}
	signedAt = signedAt.UTC()
	version := queryreceipt.VersionV3
	var exposureEvidence *queryreceipt.ExposureEvidenceV1
	if evidence.Exposure != nil {
		version = queryreceipt.VersionV4
		exposureEvidence = &queryreceipt.ExposureEvidenceV1{
			RootTaskID: evidence.Exposure.RootTaskID, ProfileVersion: evidence.Exposure.ProfileVersion,
			ActualReleaseFacts: evidence.Exposure.ActualReleaseFacts, ActualInfluenceFacts: evidence.Exposure.ActualInfluenceFacts,
			ChargedReleaseFacts: evidence.Exposure.ChargedReleaseFacts, ChargedInfluenceFacts: evidence.Exposure.ChargedInfluenceFacts,
			ObservationSHA256: evidence.Exposure.ObservationSHA256,
		}
	}
	receipt := queryreceipt.QueryReceiptV1{
		Version: version, ReceiptID: record.ID,
		TaskID: record.TaskID, QueryID: record.ID, RequestID: record.RequestID,
		ManifestDigest: record.ManifestDigest, GrantDigest: record.GrantDigest,
		CatalogDigest: record.CatalogDigest, CatalogVersion: record.CatalogVersion,
		DatasourceID: record.DatasourceID, SchemaDigest: record.SchemaDigest,
		RequestDigest: record.RequestDigest, SQLFingerprint: record.SQLFingerprint,
		PolicyDecision: record.PolicyDecision, BudgetBefore: queryReceiptBudget(record.BudgetBefore),
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: record.ReservedRows, DBMS: record.ReservedDBMS},
		BudgetCharged: queryreceipt.BudgetVectorV1{
			Queries: record.ChargedQueries, Rows: record.ChargedRows, DBMS: record.ChargedDBMS,
		},
		BudgetAfter: queryReceiptBudget(*record.BudgetAfter), RowCount: record.ResultRows,
		DatabaseMS: record.ResultDBMS, ResultHash: record.ResultSHA256,
		Status: string(record.Status), ErrorCode: record.ErrorCode,
		CreatedAt: record.CreatedAt, CompletedAt: *record.CompletedAt,
		AuditSequence: evidence.Audit.Sequence, PreviousAuditHash: evidence.Audit.PreviousHash,
		AuditHash: evidence.Audit.CurrentHash, SignedAt: &signedAt,
		Exposure: exposureEvidence,
	}
	signed, err := signer.Sign(receipt)
	if err != nil {
		return control.SaveQueryReceiptRequest{}, err
	}
	encoded, err := approval.CanonicalJSON(signed)
	if err != nil {
		return control.SaveQueryReceiptRequest{}, err
	}
	return control.SaveQueryReceiptRequest{
		QueryID: record.ID, Version: signed.Version, GatewayKeyID: signed.GatewayKeyID,
		Signature: signed.Signature, SignedAt: signedAt,
		TerminalAuditSequence: evidence.Audit.Sequence, TerminalAuditHash: evidence.Audit.CurrentHash,
		ReceiptJSON: encoded,
	}, nil
}

func decodeReceiptJSON(encoded []byte) (map[string]any, error) {
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeSignedQueryReceipt(value map[string]any) (queryreceipt.QueryReceiptV1, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return queryreceipt.QueryReceiptV1{}, err
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return queryreceipt.QueryReceiptV1{}, err
	}
	return receipt, nil
}

func queryReceiptBudget(snapshot control.BudgetSnapshot) queryreceipt.BudgetStateV1 {
	return queryreceipt.BudgetStateV1{
		Limits: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Limits.Queries, Rows: snapshot.Limits.Rows, DBMS: snapshot.Limits.DBMS,
		},
		Used: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Usage.UsedQueries, Rows: snapshot.Usage.UsedRows, DBMS: snapshot.Usage.UsedDBMS,
		},
		Reserved: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Usage.ReservedQueries, Rows: snapshot.Usage.ReservedRows, DBMS: snapshot.Usage.ReservedDBMS,
		},
	}
}

func unsignedPublicReceipt(record control.QueryRecord) map[string]any {
	result := map[string]any{
		"receipt_id": record.ID, "task_id": record.TaskID, "query_id": record.ID,
		"request_id": record.RequestID, "actor": record.Actor,
		"request_digest": record.RequestDigest, "sql_fingerprint": record.SQLFingerprint,
		"catalog_version": record.CatalogVersion, "catalog_digest": record.CatalogDigest,
		"datasource_id": record.DatasourceID, "schema_digest": record.SchemaDigest,
		"manifest_digest": record.ManifestDigest, "grant_digest": record.GrantDigest,
		"policy_decision": record.PolicyDecision,
		"status":          record.Status, "result_rows": record.ResultRows, "database_ms": record.ResultDBMS,
		"budget_reserved": map[string]any{"queries": int64(1), "rows": record.ReservedRows, "db_ms": record.ReservedDBMS},
		"charged_queries": record.ChargedQueries, "charged_rows": record.ChargedRows,
		"charged_database_ms": record.ChargedDBMS, "result_sha256": record.ResultSHA256,
		"error_code": record.ErrorCode, "created_at": record.CreatedAt, "completed_at": record.CompletedAt,
		"budget_before": publicBudget(record.BudgetBefore),
	}
	if record.BudgetAfter != nil {
		result["budget_after"] = publicBudget(*record.BudgetAfter)
	}
	return result
}

func storedGrantMatchesProtocol(task control.Task, stored control.TaskGrant, protocol approval.TaskGrantV1) bool {
	core := protocol.Core
	if core.TaskID != task.ID || core.TaskID != stored.TaskID || core.AgentID != task.PrincipalID ||
		core.RootTaskID != lineageValue(task.RootTaskID, task.ParentTaskID) || core.ParentTaskID != task.ParentTaskID ||
		core.HumanSubject != stored.Subject || core.DeclaredObjective != task.Objective ||
		core.DeclaredObjective != stored.Purpose || string(core.SensitivityCeiling) != stored.SensitivityCeiling ||
		core.CatalogVersion != task.CatalogVersion || core.CatalogVersion != stored.CatalogVersion ||
		core.CatalogSHA256 != stored.CatalogDigest ||
		core.DatasourceID != stored.DatasourceID || core.SchemaDigest != stored.SchemaDigest ||
		!stored.ExpiresAt.Equal(core.ExpiresAt.UTC().Truncate(time.Microsecond)) ||
		stored.Budget != (control.BudgetLimits{Queries: core.Budget.MaxQueries, Rows: core.Budget.MaxResultRows, DBMS: core.Budget.MaxDBMS}) ||
		stored.Exposure != (control.ExposureGrant{Limits: control.ExposureLimits{ReleaseFacts: core.Budget.MaxReleaseFacts, InfluenceFacts: core.Budget.MaxInfluenceFacts}, ProfileVersion: core.Budget.ExposureProfileVersion}) ||
		!sameStringSet(stored.ApprovedProducts, core.ApprovedProducts) ||
		!sameColumnSets(stored.ApprovedColumns, core.ApprovedColumns) {
		return false
	}
	var storedScope map[string]any
	if err := json.Unmarshal(stored.MandatoryScope, &storedScope); err != nil {
		return false
	}
	storedCanonical, err := approval.CanonicalJSON(storedScope)
	if err != nil {
		return false
	}
	coreCanonical, err := approval.CanonicalJSON(core.MandatoryScope)
	return err == nil && string(storedCanonical) == string(coreCanonical)
}

func (s *Service) ensureActiveTaskFamily(ctx context.Context, task control.Task) error {
	if task.ParentTaskID == "" {
		return nil
	}
	expectedRoot := task.RootTaskID
	seen := map[string]struct{}{task.ID: {}}
	parentID := task.ParentTaskID
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := seen[parentID]; duplicate {
			return control.ErrInvalidStateChange
		}
		seen[parentID] = struct{}{}
		parent, err := s.store.GetTask(ctx, parentID)
		if err != nil {
			return err
		}
		if parent.State != control.TaskActive || parent.RootTaskID != expectedRoot {
			return control.ErrTaskNotActive
		}
		if parent.ParentTaskID == "" {
			if parent.ID != expectedRoot {
				return control.ErrInvalidStateChange
			}
			return nil
		}
		parentID = parent.ParentTaskID
	}
	return control.ErrInvalidStateChange
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func sameColumnSets(left, right map[string][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for product, leftColumns := range left {
		rightColumns, ok := right[product]
		if !ok || !sameStringSet(leftColumns, rightColumns) {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func saturatedProduct(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if left > maxInt64/right {
		return maxInt64
	}
	return left * right
}

func detachedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
