package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

func (s *Service) listDataProducts(_ context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	products := make([]map[string]any, 0, len(s.catalog.Products))
	for _, product := range s.catalog.ListProducts() {
		sensitivity, err := product.EffectiveSensitivity()
		if err != nil {
			return nil, err
		}
		fields := make([]map[string]any, 0, len(product.Fields))
		for _, field := range product.Fields {
			fieldSensitivity := field.Sensitivity
			if fieldSensitivity == "" {
				fieldSensitivity = product.Sensitivity
			}
			fields = append(fields, map[string]any{
				"name": field.Name, "type": field.Type, "description": field.Description,
				"sensitivity": fieldSensitivity,
			})
		}
		products = append(products, map[string]any{
			"name": product.Name, "description": product.Description, "sensitivity": sensitivity,
			"fields": fields, "scopes": product.Scopes,
		})
	}
	return map[string]any{
		"catalog_version": s.catalog.CatalogVersion,
		"products":        products,
		"scopes":          append([]catalog.Scope(nil), s.catalog.Scopes...),
	}, nil
}

type requestedBudgetArgs struct {
	MaxQueries *int64 `json:"max_queries,omitempty"`
	MaxRows    *int64 `json:"max_rows,omitempty"`
}

func (s *Service) requestDataTask(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		Objective       string               `json:"objective"`
		DataProducts    []string             `json:"data_products"`
		Columns         map[string][]string  `json:"columns"`
		Scopes          map[string]any       `json:"scopes"`
		RequestedBudget *requestedBudgetArgs `json:"requested_budget,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	args.Objective = strings.TrimSpace(args.Objective)
	if args.Objective == "" || len(args.Objective) > 4000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "objective 必须为 1 到 4000 个字符"}
	}
	if len(args.DataProducts) == 0 || args.Columns == nil || args.Scopes == nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "data_products、columns 和 scopes 必须显式提供"}
	}
	seenProducts := make(map[string]struct{}, len(args.DataProducts))
	for index, product := range args.DataProducts {
		product = strings.TrimSpace(product)
		if product == "" {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "data_products 不能包含空值"}
		}
		if _, duplicate := seenProducts[product]; duplicate {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "data_products 不能包含重复产品"}
		}
		args.DataProducts[index] = product
		seenProducts[product] = struct{}{}
	}
	requestedBudget := budgetRequest(args.RequestedBudget)
	policy, err := s.catalog.ResolveTaskPolicy(args.DataProducts, requestedBudget)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "请求的数据产品或预算不符合目录策略"}
	}
	columns, err := resolveColumns(policy.Products, args.Columns)
	if err != nil {
		return nil, err
	}
	scope, err := s.normalizeScopes(policy.Products, args.Scopes)
	if err != nil {
		return nil, err
	}

	taskID := randomID("task")
	correlation := randomID("callback")
	draftRequest := approval.DraftRequest{
		TaskID: taskID, Requester: principal.Subject, Objective: args.Objective,
		DataProducts: append([]string(nil), args.DataProducts...), ApprovedColumns: cloneColumns(columns),
		MandatoryScope: cloneScope(scope), Sensitivity: string(policy.Sensitivity),
		Budget:       approvalDraftBudget(policy.Budget),
		ApprovalMode: string(policy.ApprovalRoute.Mode), Approver: policy.ApprovalRoute.Approver,
		CatalogVersion: s.catalog.CatalogVersion, CallbackContext: correlation,
	}
	snapshotSHA256, err := approval.AuthorizationSnapshotSHA256(draftRequest)
	if err != nil {
		return nil, err
	}
	draftRequest.AuthorizationSnapshotSHA256 = snapshotSHA256
	if err := approval.ValidateAuthorizationSnapshot(draftRequest); err != nil {
		return nil, fmt.Errorf("build OA authorization snapshot: %w", err)
	}
	pending := pendingContext{
		Products: args.DataProducts, Columns: columns, MandatoryScope: scope, Budget: policy.Budget,
		Sensitivity: policy.Sensitivity, ApprovalMode: policy.ApprovalRoute.Mode,
		Approver: policy.ApprovalRoute.Approver, CallbackContext: correlation,
	}
	pendingJSON, err := json.Marshal(persistedPendingContext{
		pendingContext: pending, AuthorizationSnapshotSHA256: snapshotSHA256,
	})
	if err != nil {
		return nil, err
	}
	draft, err := s.approval.CreateDraft(ctx, draftRequest)
	if err != nil {
		return nil, err
	}
	requestedJSON, _ := json.Marshal(args.RequestedBudget)
	if string(requestedJSON) == "null" {
		requestedJSON = []byte(`{}`)
	}
	now := s.clock().UTC()
	if err := s.store.CreateTask(ctx, control.Task{
		ID: taskID, PrincipalID: principal.ID, Objective: args.Objective,
		State: control.TaskAwaitingSubmission, CatalogVersion: s.catalog.CatalogVersion,
		Sensitivity: string(policy.Sensitivity), RequestedBudget: requestedJSON,
		RequestContext: pendingJSON, ApprovalRef: draft.DraftID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return nil, err
	}
	return map[string]any{
		"task_id": taskID, "state": control.TaskAwaitingSubmission, "oa_url": draft.URL,
		"approval_mode": string(policy.ApprovalRoute.Mode), "sensitivity": string(policy.Sensitivity),
		"catalog_version": s.catalog.CatalogVersion,
		"budget": map[string]any{"max_queries": policy.Budget.MaxQueries, "max_rows": policy.Budget.MaxRows,
			"max_db_ms": policy.Budget.MaxDBTime.Milliseconds(), "query_timeout_ms": policy.Budget.PerQueryTimeout.Milliseconds(),
			"task_ttl_seconds": int64(policy.Budget.TaskTTL.Seconds())},
	}, nil
}

func budgetRequest(explicit *requestedBudgetArgs) *domain.BudgetRequest {
	if explicit == nil {
		return nil
	}
	request := &domain.BudgetRequest{}
	if explicit.MaxQueries != nil {
		request.MaxQueries = explicit.MaxQueries
	}
	if explicit.MaxRows != nil {
		request.MaxRows = explicit.MaxRows
	}
	return request
}

func resolveColumns(products []catalog.Product, requested map[string][]string) (map[string][]string, error) {
	productMap := make(map[string]catalog.Product, len(products))
	for _, product := range products {
		productMap[product.Name] = product
	}
	for name := range requested {
		if _, ok := productMap[name]; !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "columns 包含未申请产品"}
		}
	}
	resolved := make(map[string][]string, len(products))
	for _, product := range products {
		available := make(map[string]struct{}, len(product.Fields))
		for _, field := range product.Fields {
			available[field.Name] = struct{}{}
		}
		columns := requested[product.Name]
		if len(columns) == 0 {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "每个申请产品都必须显式提供至少一个字段"}
		}
		seen := make(map[string]struct{}, len(columns))
		for _, column := range columns {
			if _, ok := available[column]; !ok {
				return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "columns 包含目录外字段"}
			}
			if _, duplicate := seen[column]; duplicate {
				return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "columns 包含重复字段"}
			}
			seen[column] = struct{}{}
		}
		resolved[product.Name] = append([]string(nil), columns...)
	}
	return resolved, nil
}

func (s *Service) normalizeScopes(products []catalog.Product, input map[string]any) (map[string]any, error) {
	if input == nil {
		input = map[string]any{}
	}
	allowedScopes := make(map[string]struct{})
	requiredScopes := make(map[string]struct{})
	productFields := make(map[string]struct{})
	for _, product := range products {
		for _, name := range product.Scopes {
			allowedScopes[name] = struct{}{}
			requiredScopes[name] = struct{}{}
		}
		for _, field := range product.Fields {
			productFields[field.Name] = struct{}{}
		}
	}
	definitions := make(map[string]catalog.Scope, len(s.catalog.Scopes))
	for _, definition := range s.catalog.Scopes {
		definitions[definition.Name] = definition
		if _, published := productFields[definition.Name]; published {
			allowedScopes[definition.Name] = struct{}{}
		}
	}
	result := make(map[string]any, len(input))
	for name, raw := range input {
		if _, ok := allowedScopes[name]; !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "scopes 包含未批准的数据范围"}
		}
		definition, ok := definitions[name]
		if !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "目录范围定义不存在"}
		}
		switch definition.Type {
		case catalog.ScopeTypeEnum:
			values, err := stringValues(raw)
			if err != nil || len(values) == 0 {
				return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "枚举范围必须包含至少一个合法值"}
			}
			allowedValues := make(map[string]struct{}, len(definition.AllowedValues))
			for _, value := range definition.AllowedValues {
				allowedValues[value] = struct{}{}
			}
			for _, value := range values {
				if _, ok := allowedValues[value]; !ok {
					return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "枚举范围值不在目录允许列表中"}
				}
			}
			result[name] = values
		case catalog.ScopeTypeDateRange:
			dateRange, err := normalizeDateRange(raw, definition)
			if err != nil {
				return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "日期范围不符合目录边界"}
			}
			result[name] = dateRange
		default:
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "不支持的目录范围类型"}
		}
	}
	for name := range requiredScopes {
		if _, present := result[name]; !present {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "任务目标缺少目录要求的强制数据范围"}
		}
	}
	return result, nil
}

func stringValues(raw any) ([]string, error) {
	var values []string
	switch typed := raw.(type) {
	case string:
		values = []string{typed}
	case []any:
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return nil, errors.New("non-string scope value")
			}
			values = append(values, value)
		}
	case []string:
		values = append(values, typed...)
	default:
		return nil, errors.New("invalid scope value")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return nil, errors.New("empty scope value")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("duplicate scope value")
		}
		seen[value] = struct{}{}
	}
	sort.Strings(values)
	return values, nil
}

func cloneScope(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for name, value := range source {
		switch typed := value.(type) {
		case []string:
			result[name] = append([]string(nil), typed...)
		case []any:
			result[name] = append([]any(nil), typed...)
		case map[string]string:
			copy := make(map[string]string, len(typed))
			for key, item := range typed {
				copy[key] = item
			}
			result[name] = copy
		case map[string]any:
			copy := make(map[string]any, len(typed))
			for key, item := range typed {
				copy[key] = item
			}
			result[name] = copy
		default:
			result[name] = typed
		}
	}
	return result
}

func normalizeDateRange(raw any, definition catalog.Scope) (map[string]string, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var value struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || (value.From == "" && value.To == "") {
		return nil, errors.New("invalid date range")
	}
	parse := func(input string) (time.Time, error) {
		if input == "" {
			return time.Time{}, nil
		}
		return time.Parse("2006-01-02", input)
	}
	from, err := parse(value.From)
	if err != nil {
		return nil, err
	}
	to, err := parse(value.To)
	if err != nil {
		return nil, err
	}
	minimum, _ := parse(definition.Min)
	maximum, _ := parse(definition.Max)
	if !from.IsZero() && !minimum.IsZero() && from.Before(minimum) || !to.IsZero() && !maximum.IsZero() && to.After(maximum) || !from.IsZero() && !to.IsZero() && from.After(to) {
		return nil, errors.New("date range exceeds catalog")
	}
	return map[string]string{"from": value.From, "to": value.To}, nil
}

func (s *Service) listMyTasks(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		State  control.TaskState `json:"state,omitempty"`
		Cursor string            `json:"cursor,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	tasks, err := s.store.ListTasks(ctx, control.TaskFilter{PrincipalID: principal.ID, State: args.State, AfterID: args.Cursor, Limit: 100})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, publicTask(task))
	}
	next := ""
	if len(tasks) == 100 {
		next = tasks[len(tasks)-1].ID
	}
	return map[string]any{"tasks": items, "next_cursor": next}, nil
}

func (s *Service) getTaskStatus(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	return publicTask(task), nil
}

func (s *Service) waitForApproval(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID         string `json:"task_id"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.TimeoutSeconds < 0 || args.TimeoutSeconds > 45 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "timeout_seconds 必须在 0 到 45 之间"}
	}
	if args.TimeoutSeconds == 0 {
		args.TimeoutSeconds = 30
	}
	deadline := time.NewTimer(time.Duration(args.TimeoutSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := s.ownedTask(ctx, principal, args.TaskID)
		if err != nil {
			return nil, err
		}
		if task.State == control.TaskActive || task.State == control.TaskArchived {
			result := publicTask(task)
			result["timed_out"] = false
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			result := publicTask(task)
			result["timed_out"] = true
			return result, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) getTaskContext(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
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
	budget, err := s.store.GetBudget(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var mandatoryScope any
	if err := json.Unmarshal(grant.MandatoryScope, &mandatoryScope); err != nil {
		return nil, err
	}
	return map[string]any{
		"task": publicTask(task), "subject": grant.Subject, "purpose": grant.Purpose,
		"approved_products": grant.ApprovedProducts, "approved_columns": grant.ApprovedColumns,
		"mandatory_scope": mandatoryScope, "sensitivity_ceiling": grant.SensitivityCeiling,
		"expires_at": grant.ExpiresAt, "catalog_version": grant.CatalogVersion,
		"approval_receipt": grant.ApprovalReceipt, "budget": publicBudget(budget),
	}, nil
}

func (s *Service) getBudget(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.ownedTask(ctx, principal, args.TaskID); err != nil {
		return nil, err
	}
	budget, err := s.store.GetBudget(ctx, args.TaskID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task_id": args.TaskID, "budget": publicBudget(budget)}, nil
}

func (s *Service) completeTask(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID  string `json:"task_id"`
		Summary string `json:"summary,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "只有 ACTIVE 任务可以完成"}
	}
	if len(args.Summary) > 8000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "summary 过长"}
	}
	payload, _ := json.Marshal(map[string]any{"summary": args.Summary})
	updated, err := s.store.TransitionTask(ctx, control.TaskTransition{
		TaskID: task.ID, ExpectedFrom: control.TaskActive, To: control.TaskArchived,
		Reason: control.TerminalCompleted, Actor: principal.Subject, Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	return publicTask(updated), nil
}

type persistedPendingContext struct {
	pendingContext
	AuthorizationSnapshotSHA256 string `json:"authorization_snapshot_sha256"`
}

func approvalDraftBudget(budget domain.Budget) approval.DraftBudget {
	return approval.DraftBudget{
		MaxQueries: budget.MaxQueries, MaxRows: budget.MaxRows,
		MaxDBMS: budget.MaxDBTime.Milliseconds(), QueryTimeoutMS: budget.PerQueryTimeout.Milliseconds(),
		TaskTTLMS: budget.TaskTTL.Milliseconds(),
	}
}

func authorizationDraftForPending(task control.Task, requester string, pending pendingContext) approval.DraftRequest {
	return approval.DraftRequest{
		TaskID: task.ID, Requester: requester, Objective: task.Objective,
		DataProducts: append([]string(nil), pending.Products...), ApprovedColumns: cloneColumns(pending.Columns),
		MandatoryScope: cloneScope(pending.MandatoryScope), Sensitivity: string(pending.Sensitivity),
		Budget: approvalDraftBudget(pending.Budget), ApprovalMode: string(pending.ApprovalMode), Approver: pending.Approver,
		CatalogVersion: task.CatalogVersion, CallbackContext: pending.CallbackContext,
	}
}

func decodePersistedPending(task control.Task) (persistedPendingContext, error) {
	var persisted persistedPendingContext
	decoder := json.NewDecoder(strings.NewReader(string(task.RequestContext)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&persisted); err != nil {
		return persisted, err
	}
	pending := persisted.pendingContext
	if len(pending.Products) == 0 || len(pending.Columns) == 0 || pending.Budget.Validate() != nil || pending.CallbackContext == "" {
		return persisted, fmt.Errorf("invalid pending task context")
	}
	if len(persisted.AuthorizationSnapshotSHA256) != 64 {
		return persisted, fmt.Errorf("invalid pending authorization snapshot")
	}
	return persisted, nil
}

func decodePending(task control.Task) (pendingContext, error) {
	persisted, err := decodePersistedPending(task)
	return persisted.pendingContext, err
}
