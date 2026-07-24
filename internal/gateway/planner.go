package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const maxV2Candidates = 16

type plannerWeights struct {
	AnswerCompleteness float64 `json:"answer_completeness"`
	QueryCoverage      float64 `json:"query_coverage"`
}

type planExposureEnvelope struct {
	TaskID     string          `json:"task_id"`
	RequestID  string          `json:"request_id,omitempty"`
	Candidates json.RawMessage `json:"candidates"`
	Weights    *plannerWeights `json:"weights,omitempty"`
}

type v2UtilityEvidence struct {
	AnswerCompleteness float64 `json:"answer_completeness"`
	QueryCoverage      float64 `json:"query_coverage"`
}

type v2CandidateRequest struct {
	ID              string              `json:"id"`
	Requirement     string              `json:"requirement"`
	Plan            queryplan.QueryPlan `json:"plan"`
	UtilityEvidence v2UtilityEvidence   `json:"utility_evidence"`
}

type bufferedCandidateResult struct {
	ID          string                 `json:"id"`
	Requirement string                 `json:"requirement"`
	Plan        queryplan.QueryPlan    `json:"plan"`
	Columns     []dataconnector.Column `json:"columns"`
	Rows        [][]any                `json:"rows"`
	RowCount    int64                  `json:"row_count"`
	Limited     bool                   `json:"limited"`
}

type storedRepresentationResult struct {
	Results    []bufferedCandidateResult `json:"results"`
	DatabaseMS int64                     `json:"database_ms"`
}

func (s *Service) planExposure(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var envelope planExposureEnvelope
	if err := decodeArgs(raw, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Candidates) == 0 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "candidates 不能为空"}
	}
	task, err := s.ownedTask(ctx, principal, envelope.TaskID)
	if err != nil {
		return nil, err
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if !grant.Exposure.Enabled() {
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	if grant.Exposure.ProfileVersion == exposure.ProfileV2 {
		return s.planExposureV2(ctx, principal, task, grant, envelope)
	}
	return s.planExposureV1(ctx, task, grant, envelope)
}

func (s *Service) planExposureV1(ctx context.Context, task control.Task, grant control.TaskGrant, envelope planExposureEnvelope) (any, error) {
	if task.State != control.TaskActive {
		return nil, toolError(control.ErrTaskNotActive)
	}
	var candidates []exposure.Candidate
	if err := strictJSON(envelope.Candidates, &candidates); err != nil || len(candidates) == 0 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "V1 candidates 不符合标量成本契约"}
	}
	approved := make(map[string]struct{}, len(grant.ApprovedProducts))
	for _, product := range grant.ApprovedProducts {
		approved[product] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, ok := approved[candidate.Product]; !ok {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "候选表示包含任务授权外的数据产品"}
		}
	}
	ledger, err := s.store.GetExposureLedger(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	weights := exposure.UtilityWeights{AnswerCompleteness: 0.5, QueryCoverage: 0.5}
	if envelope.Weights != nil {
		weights.AnswerCompleteness = envelope.Weights.AnswerCompleteness
		weights.QueryCoverage = envelope.Weights.QueryCoverage
	}
	remaining := ledger.Remaining()
	plan, err := exposure.Optimize(candidates, remaining.ReleaseFacts, remaining.InfluenceFacts, weights)
	if err != nil {
		if errors.Is(err, exposure.ErrInvalid) {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "候选成本或可测量效用不符合规划契约"}
		}
		return nil, err
	}
	return map[string]any{"task_id": task.ID, "root_task_id": ledger.RootTaskID,
		"profile_version": ledger.ProfileVersion, "budget_remaining": remaining, "weights": weights, "plan": plan}, nil
}

func (s *Service) planExposureV2(ctx context.Context, principal mcp.Principal, task control.Task, grant control.TaskGrant, envelope planExposureEnvelope) (any, error) {
	if task.State != control.TaskActive {
		return nil, toolError(control.ErrTaskNotActive)
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	if task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务目录版本与当前实例不一致"}
	}
	var requested []v2CandidateRequest
	if err := strictJSON(envelope.Candidates, &requested); err != nil || len(requested) == 0 || len(requested) > maxV2Candidates {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "V2 candidates 必须是 1 到 16 个服务端执行的 QueryPlan"}
	}
	weights := exposure.UtilityWeights{AnswerCompleteness: 0.5, QueryCoverage: 0.5}
	if envelope.Weights != nil {
		weights.AnswerCompleteness = envelope.Weights.AnswerCompleteness
		weights.QueryCoverage = envelope.Weights.QueryCoverage
	}
	if weights.AnswerCompleteness < 0 || weights.QueryCoverage < 0 || weights.AnswerCompleteness+weights.QueryCoverage <= 0 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "V2 utility weights 非法"}
	}
	inputJSON, _ := json.Marshal(struct {
		Candidates []v2CandidateRequest    `json:"candidates"`
		Weights    exposure.UtilityWeights `json:"weights"`
	}{requested, weights})
	requestSummary := "plan_exposure_v2\x00" + task.ID + "\x00" + string(inputJSON)
	if envelope.RequestID == "" {
		envelope.RequestID = "plan-v2-" + digest(requestSummary)[:24]
	}
	if err := validateRequestID(envelope.RequestID); err != nil {
		return nil, err
	}
	if existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, envelope.RequestID); lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		return s.queryReplayResponse(ctx, existing)
	} else if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}

	protocolGrant, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil || approval.VerifyTaskGrantV1(s.receiptVerifier, protocolGrant) != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久授权证明无效"}
	}
	grantDigest, err := approval.GrantCoreDigest(protocolGrant.Core)
	if err != nil || !storedGrantMatchesProtocol(task, grant, protocolGrant) || protocolGrant.Core.CatalogSHA256 != s.catalog.SHA256 {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权、目录或持久 Grant 不一致"}
	}
	if err := s.validateDelegatedGrant(ctx, task, protocolGrant.Core, s.clock().UTC()); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "委托 Grant 不再有效"}
	}
	evidence, err := s.datasourceEvidence(ctx, protocolGrant.Core.ApprovedProducts)
	if err != nil {
		return nil, err
	}
	if evidence.DatasourceID != grant.DatasourceID || evidence.SchemaDigest != grant.SchemaDigest ||
		evidence.DatasourceID != protocolGrant.Core.DatasourceID || evidence.SchemaDigest != protocolGrant.Core.SchemaDigest {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权数据源与当前实例不一致"}
	}
	resource, err := s.store.GetBudget(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	remaining := resource.Remaining()
	if remaining.Queries < 1 || remaining.Rows < 1 || remaining.DBMS < 1 {
		return nil, toolError(control.ErrBudgetExhausted)
	}
	ledger, err := s.store.GetExposureLedger(ctx, task.ID)
	if err != nil || ledger.ProfileVersion != exposure.ProfileV2 {
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}

	basePolicyGrant, err := s.policyGrant(grant)
	if err != nil {
		return nil, err
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	contexts := make([]*planExposureContext, 0, len(requested))
	batchRequest := dataconnector.QueryBatchRequest{Candidates: make([]dataconnector.QueryPairRequest, 0, len(requested))}
	planParts := make([]string, 0, len(requested))
	snapshotParts := make([]string, 0, len(requested))
	seenIDs := make(map[string]struct{}, len(requested))
	physicalSource := ""
	perCandidateRows := remaining.Rows / int64(len(requested))
	if perCandidateRows < 1 {
		return nil, toolError(control.ErrBudgetExhausted)
	}
	statementTimeout := time.Duration(protocolGrant.Core.Budget.PerQueryTimeoutMS) * time.Millisecond
	for _, candidate := range requested {
		if strings.TrimSpace(candidate.ID) == "" || strings.TrimSpace(candidate.Requirement) == "" ||
			candidate.ID != strings.TrimSpace(candidate.ID) || candidate.Requirement != strings.TrimSpace(candidate.Requirement) {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "候选 id 和 requirement 非法"}
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "候选 id 重复"}
		}
		seenIDs[candidate.ID] = struct{}{}
		if candidate.UtilityEvidence.AnswerCompleteness < 0 || candidate.UtilityEvidence.AnswerCompleteness > 1 ||
			candidate.UtilityEvidence.QueryCoverage < 0 || candidate.UtilityEvidence.QueryCoverage > 1 {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "候选 utility_evidence 超出 0..1"}
		}
		product, ok := s.catalog.LookupProduct(candidate.Plan.Product)
		if !ok || !contains(grant.ApprovedProducts, candidate.Plan.Product) {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "候选 QueryPlan 请求了授权外产品"}
		}
		if physicalSource == "" {
			physicalSource = product.Source
		} else if physicalSource != product.Source {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "V2 候选必须位于同一事务数据源"}
		}
		columns := make(map[string]struct{}, len(grant.ApprovedColumns[product.Name]))
		for _, column := range grant.ApprovedColumns[product.Name] {
			columns[column] = struct{}{}
		}
		aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
		for _, aggregate := range product.AllowedAggregates {
			aggregates[strings.ToLower(aggregate)] = struct{}{}
		}
		planContext, err := buildPlanExposureContext(candidate.Plan, product, columns, aggregates)
		if err != nil || planContext.configureV2(columns, aggregates) != nil {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "候选 QueryPlan 不在 V2 可证明片段内"}
		}
		policyGrant, err := planContext.extendGrant(basePolicyGrant)
		if err != nil {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		visibleDecision, err := engine.Authorize(sqlpolicy.Request{SQL: planContext.mainSQL, Grant: policyGrant, RowLimit: perCandidateRows})
		if err != nil {
			return nil, err
		}
		provenanceRows := perCandidateRows
		provenancePolicyRows := provenanceRows
		if planContext.grouped {
			provenanceRows = ledger.Limits.InfluenceFacts
			provenancePolicyRows = provenanceRows + 1
		}
		provenanceDecision, err := engine.Authorize(sqlpolicy.Request{SQL: planContext.provenanceSQL, Grant: policyGrant, RowLimit: provenancePolicyRows})
		if err != nil {
			return nil, err
		}
		batchRequest.Candidates = append(batchRequest.Candidates, dataconnector.QueryPairRequest{
			Visible:    dataconnector.QueryRequest{SQL: visibleDecision.SQL, StatementTimeout: statementTimeout, MaxRows: perCandidateRows},
			Provenance: dataconnector.QueryRequest{SQL: provenanceDecision.SQL, StatementTimeout: statementTimeout, MaxRows: provenanceRows},
		})
		contexts = append(contexts, planContext)
		planParts = append(planParts, candidate.ID+"\x00"+planContext.planDigest+"\x00"+visibleDecision.Fingerprint+"\x00"+provenanceDecision.Fingerprint)
		snapshotParts = append(snapshotParts, product.FactNamespace+"\x00"+product.Snapshot+"\x00"+product.LineageManifestDigest)
	}
	sort.Strings(planParts)
	sort.Strings(snapshotParts)
	snapshotBundleSHA := digest(exposure.ProfileV2 + "\x00" + s.catalog.SHA256 + "\x00" + strings.Join(snapshotParts, "\x01"))

	queryID := randomID("query")
	requestedDBMS := min64(remaining.DBMS, statementTimeout.Milliseconds()*int64(len(requested))*2)
	if requestedDBMS < 1 {
		return nil, toolError(control.ErrBudgetExhausted)
	}
	reservation, err := s.store.ReserveBudget(ctx, control.ReserveRequest{QueryID: queryID, TaskID: task.ID,
		RequestID: envelope.RequestID, Actor: principal.Subject, RequestDigest: digest(requestSummary),
		SQLFingerprint: digest(strings.Join(planParts, "\x01")), CatalogVersion: task.CatalogVersion,
		CatalogDigest: protocolGrant.Core.CatalogSHA256, DatasourceID: evidence.DatasourceID, SchemaDigest: evidence.SchemaDigest,
		ManifestDigest: protocolGrant.Core.ManifestDigest, GrantDigest: grantDigest, PolicyDecision: "ALLOW",
		RequestedRows: remaining.Rows, RequestedDBMS: requestedDBMS,
		Exposure: &control.ExposureReservationRequest{ProfileVersion: exposure.ProfileV2,
			EstimatedReleaseFacts: ledger.Limits.ReleaseFacts, EstimatedInfluenceFacts: ledger.Limits.InfluenceFacts}})
	if err != nil {
		return nil, err
	}
	if reservation.Replay && reservation.Record != nil {
		return s.queryReplayResponse(ctx, *reservation.Record)
	}
	batchConnector, ok := s.connector.(interface {
		QueryBatch(context.Context, dataconnector.QueryBatchRequest) (dataconnector.QueryBatchResult, error)
	})
	if !ok {
		s.releaseQueryBudget(ctx, queryID, "EXPOSURE_V2_SNAPSHOT_UNAVAILABLE")
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	timeout := time.Duration(reservation.AllowedDBMS)*time.Millisecond + 250*time.Millisecond
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	started := time.Now()
	batch, queryErr := batchConnector.QueryBatch(queryCtx, batchRequest)
	cancel()
	if queryErr != nil {
		code := "DATA_CONNECTOR_QUERY_FAILED"
		var connectorErr *dataconnector.Error
		if errors.As(queryErr, &connectorErr) {
			code = string(connectorErr.Code)
		}
		if code == string(dataconnector.CodeConnection) || code == string(dataconnector.CodeInvalidQuery) {
			s.releaseQueryBudget(ctx, queryID, code)
		} else {
			s.markQueryIndeterminate(ctx, queryID, code)
		}
		return nil, queryErr
	}
	if len(batch.Candidates) != len(requested) {
		s.markQueryIndeterminate(ctx, queryID, "EXPOSURE_V2_BATCH_INCOMPLETE")
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	totalRows, totalDatabaseMS := batchResourceUsage(batch, time.Since(started))
	if totalRows > reservation.AllowedRows {
		totalRows = reservation.AllowedRows
	}
	failEvidence := func() {
		settlement := control.BudgetSettlement{QueryID: queryID, Rows: totalRows, ChargeRows: totalRows,
			DBMS: min64(totalDatabaseMS, reservation.AllowedDBMS), ObservedDBMS: totalDatabaseMS,
			ErrorCode: "EXPOSURE_V2_PROVENANCE_INVALID"}
		s.failQueryBudget(ctx, settlement)
	}

	effectCandidates := make([]exposure.EffectCandidate, 0, len(requested))
	buffered := make(map[string]bufferedCandidateResult, len(requested))
	for index, pair := range batch.Candidates {
		observation, err := contexts[index].deriveObservation(pair.Visible, pair.Provenance, exposure.ProfileV2)
		if err != nil {
			failEvidence()
			return nil, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "候选来源证据不完整，未释放任何结果"}
		}
		visible, err := contexts[index].visibleResult(pair.Visible)
		if err != nil {
			failEvidence()
			return nil, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "候选结果与来源证据不一致，未释放任何结果"}
		}
		candidate := requested[index]
		effectCandidates = append(effectCandidates, exposure.EffectCandidate{ID: candidate.ID, Requirement: candidate.Requirement,
			AnswerCompleteness: candidate.UtilityEvidence.AnswerCompleteness, QueryCoverage: candidate.UtilityEvidence.QueryCoverage,
			Effect: observation, PlanDigest: contexts[index].planDigest})
		buffered[candidate.ID] = bufferedCandidateResult{ID: candidate.ID, Requirement: candidate.Requirement,
			Plan: candidate.Plan, Columns: visible.Columns, Rows: visible.Rows, RowCount: visible.RowCount,
			Limited: visible.Truncated || visible.RowCount == perCandidateRows}
	}
	summaries := make([]map[string]any, 0, len(effectCandidates))
	for _, candidate := range effectCandidates {
		effectDigest, _ := exposure.ObservationDigest(candidate.Effect)
		summaries = append(summaries, map[string]any{"id": candidate.ID, "requirement": candidate.Requirement,
			"plan_digest": candidate.PlanDigest, "effect_digest": effectDigest,
			"answer_completeness": candidate.AnswerCompleteness, "query_coverage": candidate.QueryCoverage})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i]["id"].(string) < summaries[j]["id"].(string) })
	summaryJSON, _ := json.Marshal(summaries)
	candidatesSHA := sha256.Sum256(summaryJSON)
	settlement := control.BudgetSettlement{QueryID: queryID, ChargeRows: totalRows,
		DBMS: min64(totalDatabaseMS, reservation.AllowedDBMS), ObservedDBMS: totalDatabaseMS}
	planning := control.RepresentationPlanningRequest{Candidates: effectCandidates, Weights: weights,
		CandidatesSHA256: hex.EncodeToString(candidatesSHA[:]), SnapshotBundleSHA256: snapshotBundleSHA}
	finalizeCtx, finalizeCancel := detachedContext(ctx)
	record, persistedReceipt, plan, _, err := s.store.FinalizePlannedQueryWithReceipt(finalizeCtx, settlement, planning,
		func(plan exposure.ExactPlan) ([]byte, int64, error) {
			stored := storedRepresentationResult{DatabaseMS: totalDatabaseMS}
			var releasedRows int64
			for _, selected := range plan.Selected {
				result, present := buffered[selected.ID]
				if !present {
					return nil, 0, fmt.Errorf("selected candidate %q is absent from buffer", selected.ID)
				}
				stored.Results = append(stored.Results, result)
				releasedRows += result.RowCount
			}
			encoded, err := json.Marshal(stored)
			return encoded, releasedRows, err
		}, s.terminalReceiptBuilder())
	finalizeCancel()
	if err != nil {
		settlement.ErrorCode = "EXPOSURE_V2_PLANNING_OR_SETTLEMENT_FAILED"
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	receipt, err := decodeReceiptJSON(persistedReceipt.ReceiptJSON)
	if err != nil {
		return nil, err
	}
	results := make([]bufferedCandidateResult, 0, len(plan.Selected))
	for _, selected := range plan.Selected {
		results = append(results, buffered[selected.ID])
	}
	response := map[string]any{"task_id": task.ID, "root_task_id": ledger.RootTaskID, "query_id": queryID,
		"request_id": envelope.RequestID, "status": record.Status, "profile_version": exposure.ProfileV2,
		"planner_version": plan.PlannerVersion, "plan": plan, "results": results, "database_ms": totalDatabaseMS,
		"receipt": receipt}
	if charge, chargeErr := s.store.GetExposureCharge(ctx, queryID); chargeErr == nil {
		response["exposure"] = charge
	}
	if latest, ledgerErr := s.store.GetExposureLedger(ctx, task.ID); ledgerErr == nil {
		response["exposure_budget"] = latest
	}
	return response, nil
}

func strictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON")
		}
		return err
	}
	return nil
}

func batchResourceUsage(batch dataconnector.QueryBatchResult, elapsed time.Duration) (int64, int64) {
	var rows, databaseMS int64
	for _, pair := range batch.Candidates {
		rows += pair.Visible.RowCount
		databaseDuration := pair.Visible.DatabaseTime + pair.Provenance.DatabaseTime
		if databaseDuration > 0 {
			databaseMS += maxPlannerInt64(1, databaseDuration.Milliseconds())
		}
	}
	if databaseMS == 0 {
		databaseMS = maxPlannerInt64(1, elapsed.Milliseconds())
	}
	return rows, databaseMS
}

func maxPlannerInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
