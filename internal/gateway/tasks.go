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
			"fields": fields, "scopes": product.Scopes, "snapshot": product.Snapshot,
			"entity_key": append([]string(nil), product.EntityKey...),
		})
	}
	return map[string]any{
		"catalog_version": s.catalog.CatalogVersion,
		"products":        products,
		"scopes":          append([]catalog.Scope(nil), s.catalog.Scopes...),
	}, nil
}

type requestedBudgetArgs struct {
	MaxQueries        *int64 `json:"max_queries,omitempty"`
	MaxRows           *int64 `json:"max_rows,omitempty"`
	MaxReleaseFacts   *int64 `json:"max_release_facts,omitempty"`
	MaxInfluenceFacts *int64 `json:"max_influence_facts,omitempty"`
	MaxOutcomeFacts   *int64 `json:"max_outcome_facts,omitempty"`
}

func (s *Service) requestDataTask(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		Objective           string               `json:"objective"`
		ParentTaskID        string               `json:"parent_task_id,omitempty"`
		DelegatePrincipalID string               `json:"delegate_principal_id,omitempty"`
		DataProducts        []string             `json:"data_products"`
		Columns             map[string][]string  `json:"columns"`
		Scopes              map[string]any       `json:"scopes"`
		RequestedBudget     *requestedBudgetArgs `json:"requested_budget,omitempty"`
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
	now := s.clock().UTC()
	taskPrincipalID := principal.ID
	humanSubject := principal.Subject
	rootTaskID := ""
	var parentCore *domain.TaskGrantCoreV1
	if args.ParentTaskID == "" {
		if args.DelegatePrincipalID != "" {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "delegate_principal_id 只能与 parent_task_id 一起使用"}
		}
	} else {
		parentTask, err := s.ownedTask(ctx, principal, args.ParentTaskID)
		if err != nil {
			return nil, err
		}
		if parentTask.State != control.TaskActive || parentTask.CatalogVersion != s.catalog.CatalogVersion {
			return nil, toolError(control.ErrTaskNotActive)
		}
		parentGrant, err := s.store.GetGrant(ctx, parentTask.ID)
		if err != nil {
			return nil, err
		}
		protocol, err := approval.DecodeTaskGrantV1(parentGrant.ApprovalReceipt)
		if err != nil || approval.VerifyTaskGrantV1(s.receiptVerifier, protocol) != nil ||
			!storedGrantMatchesProtocol(parentTask, parentGrant, protocol) || protocol.Core.ValidateAt(now) != nil {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "父任务的持久授权证明无效或已经过期"}
		}
		delegateID := strings.TrimSpace(args.DelegatePrincipalID)
		if delegateID == "" {
			delegateID = principal.ID
		}
		delegate, err := s.store.GetPrincipal(ctx, delegateID)
		if err != nil || delegate.Role != "query" || delegate.DisabledAt != nil {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "委托目标必须是已启用的查询主体"}
		}
		taskPrincipalID = delegate.ID
		humanSubject = protocol.Core.HumanSubject
		rootTaskID = parentTask.RootTaskID
		if rootTaskID == "" {
			rootTaskID = parentTask.ID
		}
		parentCore = &protocol.Core
	}
	requestedBudget := budgetRequest(args.RequestedBudget)
	policy, err := s.catalog.ResolveTaskPolicy(args.DataProducts, requestedBudget)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "请求的数据产品或预算不符合目录策略"}
	}
	if parentCore != nil {
		policy.Budget, err = constrainDelegatedBudget(policy.Budget, *parentCore, args.RequestedBudget, now)
		if err != nil {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "委托任务试图扩大父任务的授权预算"}
		}
	}
	columns, err := resolveColumns(policy.Products, args.Columns)
	if err != nil {
		return nil, err
	}
	scope, err := s.normalizeScopes(policy.Products, args.Scopes)
	if err != nil {
		return nil, err
	}
	evidence, err := s.datasourceEvidence(ctx, args.DataProducts)
	if err != nil {
		return nil, err
	}

	// Products, columns, and enum scopes are sets in the authorization model.
	// Normalize them before hashing because RFC 8785 preserves array order.
	sort.Strings(args.DataProducts)
	for product := range columns {
		sort.Strings(columns[product])
	}
	normalizeScopeSets(scope)

	taskID := randomID("task")
	correlation := randomID("callback")
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: taskID,
		RootTaskID: rootTaskID, ParentTaskID: args.ParentTaskID,
		HumanSubject: humanSubject, AgentID: taskPrincipalID, DeclaredObjective: args.Objective,
		Products: append([]string(nil), args.DataProducts...), ApprovedColumns: cloneColumns(columns),
		MandatoryScope: cloneScope(scope), Sensitivity: policy.Sensitivity,
		Budget: authorizationBudget(policy.Budget), CatalogVersion: s.catalog.CatalogVersion,
		CatalogSHA256: s.catalog.SHA256, DatasourceID: evidence.DatasourceID,
		SchemaDigest: evidence.SchemaDigest, CallbackContext: correlation,
		Nonce: strings.TrimPrefix(randomID(""), "_"),
	}
	draftRequest := approval.DraftRequest{
		Manifest:     manifest,
		ApprovalMode: string(policy.ApprovalRoute.Mode), Approver: policy.ApprovalRoute.Approver,
	}
	snapshotSHA256, err := approval.AuthorizationSnapshotSHA256(draftRequest)
	if err != nil {
		return nil, err
	}
	draftRequest.ManifestDigest = snapshotSHA256
	if err := approval.ValidateAuthorizationSnapshot(draftRequest); err != nil {
		return nil, fmt.Errorf("build OA authorization manifest: %w", err)
	}
	if parentCore != nil {
		candidate, err := domain.CoreFromManifest(manifest, snapshotSHA256, now)
		if err != nil || parentCore.CheckDelegation(candidate) != nil {
			return nil, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "委托任务扩大了父 Grant 的产品、字段、范围或期限"}
		}
	}
	pending := pendingContext{
		Products: args.DataProducts, Columns: columns, MandatoryScope: scope, Budget: policy.Budget,
		Sensitivity: policy.Sensitivity, DatasourceID: evidence.DatasourceID, SchemaDigest: evidence.SchemaDigest,
		ApprovalMode: policy.ApprovalRoute.Mode,
		Approver:     policy.ApprovalRoute.Approver, CallbackContext: correlation,
	}
	pendingJSON, err := json.Marshal(persistedPendingContext{
		pendingContext: pending, Manifest: manifest, ManifestDigest: snapshotSHA256,
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
	if err := s.store.CreateTask(ctx, control.Task{
		ID: taskID, PrincipalID: taskPrincipalID, Objective: args.Objective,
		State: control.TaskAwaitingSubmission, CatalogVersion: s.catalog.CatalogVersion,
		Sensitivity: string(policy.Sensitivity), RequestedBudget: requestedJSON,
		RequestContext: pendingJSON, ApprovalRef: draft.DraftID, CreatedAt: now, UpdatedAt: now,
		RootTaskID: rootTaskID, ParentTaskID: args.ParentTaskID,
	}); err != nil {
		return nil, err
	}
	return map[string]any{
		"task_id": taskID, "state": control.TaskAwaitingSubmission, "oa_url": draft.URL,
		"approval_mode": string(policy.ApprovalRoute.Mode), "sensitivity": string(policy.Sensitivity),
		"catalog_version": s.catalog.CatalogVersion, "datasource_id": evidence.DatasourceID,
		"schema_digest": evidence.SchemaDigest, "manifest_digest": snapshotSHA256,
		"root_task_id": defaultString(rootTaskID, taskID), "parent_task_id": args.ParentTaskID,
		"delegate_principal_id": taskPrincipalID,
		"budget": map[string]any{"max_queries": policy.Budget.MaxQueries, "max_rows": policy.Budget.MaxRows,
			"max_db_ms": policy.Budget.MaxDBTime.Milliseconds(), "query_timeout_ms": policy.Budget.PerQueryTimeout.Milliseconds(),
			"task_ttl_seconds":  int64(policy.Budget.TaskTTL.Seconds()),
			"max_release_facts": policy.Budget.MaxReleaseFacts, "max_influence_facts": policy.Budget.MaxInfluenceFacts,
			"max_outcome_facts":        policy.Budget.MaxOutcomeFacts,
			"exposure_profile_version": policy.Budget.ExposureProfileVersion},
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
	if explicit.MaxReleaseFacts != nil {
		request.MaxReleaseFacts = explicit.MaxReleaseFacts
	}
	if explicit.MaxInfluenceFacts != nil {
		request.MaxInfluenceFacts = explicit.MaxInfluenceFacts
	}
	if explicit.MaxOutcomeFacts != nil {
		request.MaxOutcomeFacts = explicit.MaxOutcomeFacts
	}
	return request
}

func constrainDelegatedBudget(candidate domain.Budget, parent domain.TaskGrantCoreV1, explicit *requestedBudgetArgs, now time.Time) (domain.Budget, error) {
	if err := parent.ValidateAt(now); err != nil {
		return domain.Budget{}, err
	}
	if explicit != nil {
		if (explicit.MaxQueries != nil && *explicit.MaxQueries > parent.Budget.MaxQueries) ||
			(explicit.MaxRows != nil && *explicit.MaxRows > parent.Budget.MaxResultRows) ||
			(explicit.MaxReleaseFacts != nil && *explicit.MaxReleaseFacts > parent.Budget.MaxReleaseFacts) ||
			(explicit.MaxInfluenceFacts != nil && *explicit.MaxInfluenceFacts > parent.Budget.MaxInfluenceFacts) ||
			(explicit.MaxOutcomeFacts != nil && *explicit.MaxOutcomeFacts > parent.Budget.MaxOutcomeFacts) {
			return domain.Budget{}, domain.ErrBudgetExpansion
		}
	}
	remainingTTL := parent.ExpiresAt.Sub(now.UTC())
	if remainingTTL < time.Millisecond {
		return domain.Budget{}, domain.ErrGrantExpired
	}
	candidate.MaxQueries = min64(candidate.MaxQueries, parent.Budget.MaxQueries)
	candidate.MaxRows = min64(candidate.MaxRows, parent.Budget.MaxResultRows)
	candidate.MaxDBTime = minTaskDuration(candidate.MaxDBTime, time.Duration(parent.Budget.MaxDBMS)*time.Millisecond)
	candidate.PerQueryTimeout = minTaskDuration(candidate.PerQueryTimeout, time.Duration(parent.Budget.PerQueryTimeoutMS)*time.Millisecond)
	candidate.TaskTTL = minTaskDuration(candidate.TaskTTL, remainingTTL.Truncate(time.Millisecond))
	if parent.Budget.MaxReleaseFacts == 0 {
		candidate.MaxReleaseFacts = 0
		candidate.MaxInfluenceFacts = 0
		candidate.MaxOutcomeFacts = 0
		candidate.ExposureProfileVersion = ""
	} else {
		if candidate.ExposureProfileVersion != parent.Budget.ExposureProfileVersion {
			return domain.Budget{}, domain.ErrBudgetExpansion
		}
		candidate.MaxReleaseFacts = min64(candidate.MaxReleaseFacts, parent.Budget.MaxReleaseFacts)
		candidate.MaxInfluenceFacts = min64(candidate.MaxInfluenceFacts, parent.Budget.MaxInfluenceFacts)
		candidate.MaxOutcomeFacts = min64(candidate.MaxOutcomeFacts, parent.Budget.MaxOutcomeFacts)
	}
	if err := candidate.Validate(); err != nil {
		return domain.Budget{}, err
	}
	return candidate, nil
}

func minTaskDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
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
	finalGrant, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil {
		return nil, fmt.Errorf("decode persisted task grant: %w", err)
	}
	return map[string]any{
		"task": publicTask(task), "subject": grant.Subject, "purpose": grant.Purpose,
		"approved_products": grant.ApprovedProducts, "approved_columns": grant.ApprovedColumns,
		"mandatory_scope": mandatoryScope, "sensitivity_ceiling": grant.SensitivityCeiling,
		"expires_at": grant.ExpiresAt, "catalog_version": grant.CatalogVersion,
		"catalog_digest": grant.CatalogDigest, "datasource_id": grant.DatasourceID,
		"schema_digest":   grant.SchemaDigest,
		"manifest_digest": finalGrant.Core.ManifestDigest, "task_grant": finalGrant,
		"approval_receipt": finalGrant.ApprovalReceipt, "budget": publicBudget(budget),
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
	result := map[string]any{"task_id": args.TaskID, "budget": publicBudget(budget)}
	if ledger, exposureErr := s.store.GetExposureLedger(ctx, args.TaskID); exposureErr == nil {
		result["exposure_budget"] = ledger
	} else if !errors.Is(exposureErr, control.ErrNotFound) {
		return nil, exposureErr
	}
	return result, nil
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

func (s *Service) revokeTask(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Reason string `json:"reason,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Reason) > 1000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "reason 过长"}
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	if task.State == control.TaskArchived {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务已经处于终态"}
	}
	payload, _ := json.Marshal(map[string]any{
		"reason":              args.Reason,
		"in_flight_semantics": "not_cancelled; bounded by the approved timeout and grant expiry",
	})
	updated, err := s.store.TransitionTask(ctx, control.TaskTransition{
		TaskID: task.ID, ExpectedFrom: task.State, To: control.TaskArchived,
		Reason: control.TerminalRevoked, Actor: principal.Subject, Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	result := publicTask(updated)
	result["in_flight_queries_cancelled"] = false
	return result, nil
}

type persistedPendingContext struct {
	pendingContext
	Manifest       approval.AuthorizationManifestV1 `json:"authorization_manifest"`
	ManifestDigest string                           `json:"manifest_digest"`
}

func authorizationBudget(budget domain.Budget) approval.AuthorizationBudgetV1 {
	return approval.AuthorizationBudgetV1{
		MaxQueries: budget.MaxQueries, MaxResultRows: budget.MaxRows,
		MaxDBMS: budget.MaxDBTime.Milliseconds(), PerQueryTimeoutMS: budget.PerQueryTimeout.Milliseconds(),
		TaskTTLMS: budget.TaskTTL.Milliseconds(), MaxReleaseFacts: budget.MaxReleaseFacts,
		MaxInfluenceFacts: budget.MaxInfluenceFacts, MaxOutcomeFacts: budget.MaxOutcomeFacts,
		ExposureProfileVersion: budget.ExposureProfileVersion,
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
	if len(pending.Products) == 0 || len(pending.Columns) == 0 || pending.Budget.Validate() != nil ||
		pending.DatasourceID == "" || !validSnapshotSHA256(pending.SchemaDigest) || pending.CallbackContext == "" {
		return persisted, fmt.Errorf("invalid pending task context")
	}
	if err := persisted.Manifest.Validate(); err != nil {
		return persisted, fmt.Errorf("invalid pending authorization manifest: %w", err)
	}
	digest, err := approval.ManifestDigest(persisted.Manifest)
	if err != nil || !sameSnapshotSHA256(digest, persisted.ManifestDigest) {
		return persisted, fmt.Errorf("invalid pending authorization manifest digest")
	}
	return persisted, nil
}

func decodePending(task control.Task) (pendingContext, error) {
	persisted, err := decodePersistedPending(task)
	return persisted.pendingContext, err
}

func normalizeScopeSets(scope map[string]any) {
	for name, value := range scope {
		switch typed := value.(type) {
		case []string:
			sort.Strings(typed)
			scope[name] = typed
		case []any:
			sort.Slice(typed, func(i, j int) bool {
				left, _ := typed[i].(string)
				right, _ := typed[j].(string)
				return left < right
			})
			scope[name] = typed
		}
	}
}
