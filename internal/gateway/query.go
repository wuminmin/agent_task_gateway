package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type storedQueryResult struct {
	Columns    []dataconnector.Column `json:"columns"`
	Rows       [][]any                `json:"rows"`
	RowCount   int64                  `json:"row_count"`
	DatabaseMS int64                  `json:"database_ms"`
	Limited    bool                   `json:"limited"`
}

const (
	resultEncodingFailed     = "RESULT_ENCODING_FAILED"
	resultFinalizationFailed = "RESULT_FINALIZATION_FAILED"
	settlementAttemptTimeout = 5 * time.Second
	settlementRetryDelay     = 100 * time.Millisecond
)

func (s *Service) querySQL(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
		SQL    string `json:"sql"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.SQL) == "" || len(args.SQL) > 100000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "sql 必须为 1 到 100000 个字符"}
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	return s.executeSQL(ctx, principal, task, args.SQL, args.SQL)
}

func (s *Service) executePlan(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID       string              `json:"task_id"`
		Plan         queryplan.QueryPlan `json:"plan"`
		OutputFormat string              `json:"output_format,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.OutputFormat != "" && args.OutputFormat != "json" && args.OutputFormat != "table" {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "output_format 仅支持 json 或 table"}
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
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
	requestJSON, err := json.Marshal(args.Plan)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "QueryPlan 无法编码"}
	}
	result, err := s.executeSQL(ctx, principal, task, compiled, string(requestJSON))
	if err != nil {
		return nil, err
	}
	result.(map[string]any)["query_plan"] = args.Plan
	result.(map[string]any)["output_format"] = defaultString(args.OutputFormat, "json")
	return result, nil
}

func (s *Service) executeSQL(ctx context.Context, principal mcp.Principal, task control.Task, agentSQL, requestSummary string) (any, error) {
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务目录版本与当前实例不一致；为避免扩权已拒绝查询"}
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	grantRemaining := grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining < time.Millisecond {
		return nil, toolError(control.ErrTaskExpired)
	}
	pending, err := decodePending(task)
	if err != nil {
		return nil, err
	}
	budget, err := s.store.GetBudget(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	remaining := budget.Remaining()
	if remaining.Queries < 1 || remaining.Rows < 1 || remaining.DBMS < 1 {
		return nil, &mcp.ToolError{Code: apierr.CodeBudgetExhausted, Message: "任务预算已耗尽"}
	}
	policyGrant, functions, operators, err := s.policyGrant(grant)
	if err != nil {
		return nil, err
	}
	engine := sqlpolicy.New(sqlpolicy.Config{AllowedFunctions: functions, AllowedOperators: operators})
	decision, err := engine.Authorize(sqlpolicy.Request{SQL: agentSQL, Grant: policyGrant, RowLimit: remaining.Rows})
	if err != nil {
		return nil, err
	}
	queryID := randomID("query")
	requestedTimeout := pending.Budget.PerQueryTimeout
	if requestedTimeout > grantRemaining {
		requestedTimeout = grantRemaining
	}
	requestedDBMS := min64(remaining.DBMS, requestedTimeout.Milliseconds())
	if requestedDBMS < 1 {
		return nil, toolError(control.ErrTaskExpired)
	}
	reservation, err := s.store.ReserveBudget(ctx, control.ReserveRequest{
		QueryID: queryID, TaskID: task.ID, Actor: principal.Subject,
		RequestDigest: digest(requestSummary), SQLFingerprint: decision.Fingerprint,
		CatalogVersion: task.CatalogVersion, PolicyDecision: "ALLOW",
		RequestedRows: decision.RowLimit, RequestedDBMS: requestedDBMS,
	})
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(reservation.AllowedDBMS) * time.Millisecond
	if timeout > pending.Budget.PerQueryTimeout {
		timeout = pending.Budget.PerQueryTimeout
	}
	grantRemaining = grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining <= 0 {
		// The connector has not been invoked, so this reservation may still be
		// released without charging query usage.
		releaseCtx, releaseCancel := detachedContext(ctx)
		_, releaseErr := s.store.ReleaseBudget(releaseCtx, queryID, "AUTHORIZATION_EXPIRED")
		releaseCancel()
		if releaseErr != nil {
			s.logger.Error("release expired query budget", "trace_id", mcp.TraceID(ctx), "query_id", queryID, "error", releaseErr)
		}
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
	started := time.Now()
	data, queryErr := s.connector.Query(queryCtx, dataconnector.QueryRequest{
		SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows,
	})
	settlement := querySettlement(queryID, data, started, reservation)
	if queryErr != nil {
		code := string(dataconnector.CodeQueryFailed)
		var connectorErr *dataconnector.Error
		if errors.As(queryErr, &connectorErr) {
			code = string(connectorErr.Code)
		}
		settlement.ErrorCode = code
		s.settleQueryBudget(ctx, settlement)
		return nil, queryErr
	}
	stored := storedQueryResult{
		Columns: data.Columns, Rows: data.Rows, RowCount: data.RowCount,
		DatabaseMS: settlement.DBMS, Limited: data.Truncated || data.RowCount == reservation.AllowedRows,
	}
	plaintext, err := json.Marshal(stored)
	if err != nil {
		settlement.ErrorCode = resultEncodingFailed
		s.settleQueryBudget(ctx, settlement)
		return nil, err
	}
	finalizeCtx, finalizeCancel := detachedContext(ctx)
	record, err := s.store.FinalizeQuery(finalizeCtx, settlement, plaintext)
	finalizeCancel()
	if err != nil {
		settlement.ErrorCode = resultFinalizationFailed
		s.settleQueryBudget(ctx, settlement)
		return nil, err
	}
	return map[string]any{
		"task_id": task.ID, "query_id": queryID, "columns": stored.Columns,
		"rows": stored.Rows, "row_count": stored.RowCount, "database_ms": stored.DatabaseMS,
		"limited": stored.Limited, "receipt": publicReceipt(record),
	}, nil
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
		QueryID: queryID,
		Rows:    rows,
		DBMS:    min64(databaseMS, reservation.AllowedDBMS),
	}
}

// settleQueryBudget makes one detached attempt before handing a failed
// durable settlement to a background retry loop. SettleBudget is idempotent,
// so retrying is safe even when a database commit succeeded but its result was
// lost to the caller.
func (s *Service) settleQueryBudget(ctx context.Context, settlement control.BudgetSettlement) {
	settleCtx, settleCancel := detachedContext(ctx)
	_, err := s.store.SettleBudget(settleCtx, settlement)
	settleCancel()
	if err == nil {
		return
	}
	s.logger.Error("settle query budget", "trace_id", mcp.TraceID(ctx), "query_id", settlement.QueryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}

	s.pendingSettles.Add(1)
	go s.retryQuerySettlement(settlement)
}

func (s *Service) retryQuerySettlement(settlement control.BudgetSettlement) {
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
		_, err := s.store.SettleBudget(attemptCtx, settlement)
		cancel()
		if err == nil || errors.Is(err, control.ErrClosed) {
			return
		}
		s.logger.Error("retry query budget settlement", "query_id", settlement.QueryID, "error", err)
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (s *Service) policyGrant(grant control.TaskGrant) (sqlpolicy.Grant, []string, []string, error) {
	var scope map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
		return sqlpolicy.Grant{}, nil, nil, err
	}
	result := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(grant.ApprovedProducts))}
	functionSet := make(map[string]struct{})
	operatorSet := make(map[string]struct{})
	for _, name := range grant.ApprovedProducts {
		product, ok := s.catalog.LookupProduct(name)
		if !ok {
			return sqlpolicy.Grant{}, nil, nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务引用了当前目录中不存在的数据产品"}
		}
		parts := strings.Split(product.ReportingView, ".")
		if len(parts) != 2 {
			return sqlpolicy.Grant{}, nil, nil, errors.New("validated catalog has invalid reporting view")
		}
		for _, scopeName := range product.Scopes {
			if _, present := scope[scopeName]; !present {
				return sqlpolicy.Grant{}, nil, nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务授权缺少目录要求的强制数据范围"}
			}
		}
		predicates, err := scopePredicates(product, scope)
		if err != nil {
			return sqlpolicy.Grant{}, nil, nil, err
		}
		result.Products = append(result.Products, sqlpolicy.ProductGrant{
			LogicalName: name, PhysicalSchema: parts[0], PhysicalView: parts[1],
			ApprovedColumns: append([]string(nil), grant.ApprovedColumns[name]...), MandatoryScope: predicates,
		})
		for _, function := range append(append([]string(nil), product.AllowedFunctions...), product.AllowedAggregates...) {
			functionSet[strings.ToLower(function)] = struct{}{}
		}
		for _, operator := range product.AllowedOperators {
			operatorSet[operator] = struct{}{}
		}
	}
	return result, sortedSet(functionSet), sortedSet(operatorSet), nil
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
	return map[string]any{
		"task_id": args.TaskID, "query_id": args.QueryID, "columns": result.Columns,
		"rows": result.Rows, "row_count": result.RowCount, "database_ms": result.DatabaseMS,
		"limited": result.Limited, "receipt": publicReceipt(record),
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
	records, err := s.store.ListQueries(ctx, args.TaskID, 500)
	if err != nil {
		return nil, err
	}
	start := 0
	if args.Cursor != "" {
		start = len(records)
		for index, record := range records {
			if record.ID == args.Cursor {
				start = index + 1
				break
			}
		}
	}
	end := start + 100
	if end > len(records) {
		end = len(records)
	}
	receipts := make([]map[string]any, 0, end-start)
	for _, record := range records[start:end] {
		receipts = append(receipts, publicReceipt(record))
	}
	next := ""
	if end < len(records) && end > start {
		next = records[end-1].ID
	}
	return map[string]any{"task_id": args.TaskID, "receipts": receipts, "next_cursor": next}, nil
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
		var payload any
		_ = json.Unmarshal(event.Payload, &payload)
		items = append(items, map[string]any{
			"sequence": event.Sequence, "event_id": event.EventID, "task_id": event.TaskID,
			"query_id": event.QueryID, "actor": event.Actor, "event_type": event.EventType,
			"payload": payload, "occurred_at": event.OccurredAt,
			"previous_hash": event.PreviousHash, "current_hash": event.CurrentHash,
		})
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
	events, err := s.store.ListAuditEvents(ctx, control.AuditFilter{TaskID: record.TaskID, Limit: 500})
	if err != nil {
		return nil, err
	}
	chain := make([]map[string]any, 0)
	for _, event := range events {
		if event.QueryID == record.ID {
			chain = append(chain, map[string]any{
				"sequence": event.Sequence, "event_id": event.EventID, "event_type": event.EventType,
				"occurred_at": event.OccurredAt, "previous_hash": event.PreviousHash, "current_hash": event.CurrentHash,
			})
		}
	}
	return map[string]any{"receipt": publicReceipt(record), "audit_chain_events": chain}, nil
}

func publicReceipt(record control.QueryRecord) map[string]any {
	result := map[string]any{
		"receipt_id": record.ID, "task_id": record.TaskID, "actor": record.Actor,
		"request_digest": record.RequestDigest, "sql_fingerprint": record.SQLFingerprint,
		"catalog_version": record.CatalogVersion, "policy_decision": record.PolicyDecision,
		"status": record.Status, "result_rows": record.ResultRows, "database_ms": record.ResultDBMS,
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

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
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

func detachedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
