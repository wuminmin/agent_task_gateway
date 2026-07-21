package gateway

import (
	"context"
	"encoding/json"

	"taskbound.local/agent-data-gateway/internal/mcp"
)

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

var queryTools = []mcp.Tool{
	{Name: "list_data_products", Description: "列出可申请的逻辑数据产品、字段、说明和敏感等级。", InputSchema: objectSchema(nil), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "request_data_task", Description: "创建任务和 OA 草稿。未给出 data_products 时由 DeepSeek 将目标转成候选 TaskIntent，最终策略由 Gateway 确定。", InputSchema: objectSchema(map[string]any{
		"objective":     map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
		"data_products": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "uniqueItems": true},
		"requested_budget": objectSchema(map[string]any{
			"max_queries": map[string]any{"type": "integer", "minimum": 1},
			"max_rows":    map[string]any{"type": "integer", "minimum": 1},
		}),
	}, "objective")},
	{Name: "list_my_tasks", Description: "列出当前 Alice 身份自己的任务。", InputSchema: objectSchema(map[string]any{
		"state":  map[string]any{"type": "string", "enum": []string{"AWAITING_SUBMISSION", "AWAITING_APPROVAL", "ACTIVE", "ARCHIVED"}},
		"cursor": map[string]any{"type": "string"},
	}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_status", Description: "读取自己的任务状态。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "wait_for_approval", Description: "短轮询等待审批，最长 45 秒。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 45},
	}, "task_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_context", Description: "读取 ACTIVE 任务的批准范围、预算和期限。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "query_data", Description: "将自然语言问题转换为声明式 QueryPlan，经本地确定性编译和 SQL 策略验证后查询。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "question": map[string]any{"type": "string", "minLength": 1, "maxLength": 8000},
		"output_format": map[string]any{"type": "string", "enum": []string{"json", "table"}},
	}, "task_id", "question")},
	{Name: "query_sql", Description: "在任务授权内执行单条 PostgreSQL SELECT；只能引用逻辑数据产品。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "sql": map[string]any{"type": "string", "minLength": 1, "maxLength": 100000},
	}, "task_id", "sql")},
	{Name: "get_query_result", Description: "读取自己的加密保存查询结果。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "query_id": map[string]any{"type": "string"},
	}, "task_id", "query_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_budget", Description: "读取任务预算上限、已用和剩余值。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "list_receipts", Description: "列出自己的查询审计凭证，不含物理表名或数据库凭据。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "cursor": map[string]any{"type": "string"},
	}, "task_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "complete_task", Description: "完成并归档自己的 ACTIVE 任务。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string", "maxLength": 8000},
	}, "task_id")},
}

var auditTools = []mcp.Tool{
	{Name: "list_audit_events", Description: "审计员读取不可更新的 Hash Chain 事件。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "actor": map[string]any{"type": "string"}, "event_type": map[string]any{"type": "string"}, "cursor": map[string]any{"type": "string"},
	}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_audit_receipt", Description: "审计员读取查询凭证；不返回原始敏感结果。", InputSchema: objectSchema(map[string]any{
		"receipt_id": map[string]any{"type": "string"},
	}, "receipt_id"), Annotations: map[string]any{"readOnlyHint": true}},
}

func taskIDSchema() map[string]any {
	return objectSchema(map[string]any{"task_id": map[string]any{"type": "string"}}, "task_id")
}

func (s *Service) ListTools(principal mcp.Principal) []mcp.Tool {
	switch principal.Role {
	case "query":
		return append([]mcp.Tool(nil), queryTools...)
	case "auditor":
		return append([]mcp.Tool(nil), auditTools...)
	default:
		return nil
	}
}

func (s *Service) CallTool(ctx context.Context, principal mcp.Principal, name string, raw json.RawMessage) (mcp.ToolResult, error) {
	var (
		result any
		err    error
	)
	if principal.Role == "query" {
		switch name {
		case "list_data_products":
			result, err = s.listDataProducts(ctx, principal, raw)
		case "request_data_task":
			result, err = s.requestDataTask(ctx, principal, raw)
		case "list_my_tasks":
			result, err = s.listMyTasks(ctx, principal, raw)
		case "get_task_status":
			result, err = s.getTaskStatus(ctx, principal, raw)
		case "wait_for_approval":
			result, err = s.waitForApproval(ctx, principal, raw)
		case "get_task_context":
			result, err = s.getTaskContext(ctx, principal, raw)
		case "query_data":
			result, err = s.queryData(ctx, principal, raw)
		case "query_sql":
			result, err = s.querySQL(ctx, principal, raw)
		case "get_query_result":
			result, err = s.getQueryResult(ctx, principal, raw)
		case "get_budget":
			result, err = s.getBudget(ctx, principal, raw)
		case "list_receipts":
			result, err = s.listReceipts(ctx, principal, raw)
		case "complete_task":
			result, err = s.completeTask(ctx, principal, raw)
		default:
			err = forbidden()
		}
	} else if principal.Role == "auditor" {
		switch name {
		case "list_audit_events":
			result, err = s.listAuditEvents(ctx, principal, raw)
		case "get_audit_receipt":
			result, err = s.getAuditReceipt(ctx, principal, raw)
		default:
			err = forbidden()
		}
	} else {
		err = forbidden()
	}
	if err != nil {
		return mcp.ToolResult{}, toolError(err)
	}
	return mcp.ToolResult{Structured: result}, nil
}
