package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
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
	{Name: "describe_data_product", Description: "读取一个逻辑数据产品的字段类型、排序规则、稳定角色和 SQL 白名单。", InputSchema: objectSchema(map[string]any{
		"name": map[string]any{"type": "string", "minLength": 1},
	}, "name"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_sql_capabilities", Description: "读取 TaskGate 报表 SQL profile 及可无损转换为规范 QueryPlan 的受限语法能力。", InputSchema: objectSchema(nil), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "request_data_task", Description: "使用显式产品、字段和范围创建根任务或受父 Grant 约束的委托任务及 OA 草稿。", InputSchema: objectSchema(map[string]any{
		"objective":             map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
		"parent_task_id":        map[string]any{"type": "string", "minLength": 1},
		"delegate_principal_id": map[string]any{"type": "string", "minLength": 1},
		"data_products":         map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true},
		"columns": map[string]any{"type": "object", "minProperties": 1, "additionalProperties": map[string]any{
			"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true,
		}},
		"scopes": map[string]any{"type": "object", "additionalProperties": map[string]any{"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "uniqueItems": true},
			objectSchema(map[string]any{"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}}),
		}}},
	}, "objective", "data_products", "columns", "scopes")},
	{Name: "list_my_tasks", Description: "列出当前 Alice 身份自己的任务。", InputSchema: objectSchema(map[string]any{
		"state":  map[string]any{"type": "string", "enum": []string{"AWAITING_SUBMISSION", "AWAITING_APPROVAL", "ACTIVE", "ARCHIVED"}},
		"cursor": map[string]any{"type": "string"},
	}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_status", Description: "读取自己的任务状态。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "wait_for_approval", Description: "短轮询等待审批，最长 45 秒。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 45},
	}, "task_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_context", Description: "读取 ACTIVE 任务的批准范围、预算和期限。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "rebind_data_task", Description: "为语义 View 已变化的旧任务创建一个全新的根任务和 OA 草稿；旧 Grant、查询、结果与 Receipt 不会被修改。", InputSchema: taskIDSchema()},
	{Name: "query_sql", Description: "执行任务授权范围内的报表 SQL，把加密 Parquet 规范原件保存在 TaskGate 对象存储，并仅返回摘要与 result_id。启用精确暴露记账时，SQL 必须能够无损转换为 TaskGate 规范计划。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "request_id": requestIDSchema(), "sql": map[string]any{"type": "string", "minLength": 1, "maxLength": 100000},
	}, "task_id", "request_id", "sql")},
	{Name: "get_query_result", Description: "读取自己的查询结果元数据和 result_id；完整数据仍保留在 TaskGate 对象存储。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "query_id": map[string]any{"type": "string"},
	}, "task_id", "query_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "preview_result", Description: "按 result_id 读取最多 100 行的有界预览；不交付完整 Parquet。", InputSchema: objectSchema(map[string]any{
		"result_id": map[string]any{"type": "string", "minLength": 1},
		"offset":    map[string]any{"type": "integer", "minimum": 0},
		"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
	}, "result_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "deliver_result", Description: "为已消费的规范结果生成短期 Parquet 交付地址；下载次数不会改变预算、消费状态或 Receipt。", InputSchema: objectSchema(map[string]any{
		"result_id": map[string]any{"type": "string", "minLength": 1},
		"format":    map[string]any{"type": "string", "enum": []string{"parquet"}},
	}, "result_id")},
	{Name: "get_budget", Description: "读取任务预算上限、已用和剩余值。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "list_receipts", Description: "列出自己的查询审计凭证，不含物理表名或数据库凭据。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "cursor": map[string]any{"type": "string"},
	}, "task_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "complete_task", Description: "完成并归档自己的 ACTIVE 任务。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string", "maxLength": 8000},
	}, "task_id")},
	{Name: "revoke_task", Description: "撤销自己的任务并阻止新查询；已在途查询仍由原授权期限和超时约束。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string", "maxLength": 1000},
	}, "task_id")},
}

// executePlanTool is intentionally absent from queryTools: ordinary Agents
// should discover the SQL entry point only. CallTool continues to route
// execute_plan for SDK integrations, deterministic workflows, and tests that
// already submit the canonical internal representation.
var executePlanTool = mcp.Tool{Name: "execute_plan", Description: "执行声明式 QueryPlan，经确定性编译和 SQL 策略验证后查询。", InputSchema: objectSchema(map[string]any{
	"task_id": map[string]any{"type": "string"}, "request_id": requestIDSchema(), "plan": queryPlanSchema(),
	"output_format": map[string]any{"type": "string", "enum": []string{"json", "table"}},
}, "task_id", "request_id", "plan")}

func requestIDSchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9._:-]+$"}
}

func queryPlanSchema() map[string]any {
	filter := objectSchema(map[string]any{
		"column": map[string]any{"type": "string"}, "op": map[string]any{"type": "string"}, "value": map[string]any{},
	}, "column", "op", "value")
	scan := objectSchema(map[string]any{
		"product": map[string]any{"type": "string", "minLength": 1},
		"role":    map[string]any{"type": "string", "minLength": 1},
		"filters": map[string]any{"type": "array", "items": filter},
	}, "product", "role")
	from := objectSchema(map[string]any{
		"scan": scan,
		"join": objectSchema(map[string]any{
			"left": scan, "right": scan,
			"on": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": objectSchema(map[string]any{
				"left": map[string]any{"type": "string"}, "right": map[string]any{"type": "string"},
			}, "left", "right")},
		}, "left", "right", "on"),
		"join_many": objectSchema(map[string]any{
			"sources": map[string]any{"type": "array", "minItems": 2, "maxItems": queryplan.MaxJoinSources, "items": scan},
			"on": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": objectSchema(map[string]any{
				"left": map[string]any{"type": "string"}, "right": map[string]any{"type": "string"},
			}, "left", "right")},
		}, "sources", "on"),
		"union_distinct": objectSchema(map[string]any{
			"role":    map[string]any{"type": "string", "minLength": 1},
			"columns": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": map[string]any{"type": "string"}},
			"left":    scan, "right": scan,
		}, "role", "columns", "left", "right"),
	})
	schema := objectSchema(map[string]any{
		"product": map[string]any{"type": "string", "minLength": 1},
		"from":    from,
		"columns": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "uniqueItems": true},
		"aggregates": map[string]any{"type": "array", "items": objectSchema(map[string]any{
			"function": map[string]any{"type": "string"}, "column": map[string]any{"type": "string"}, "alias": map[string]any{"type": "string"},
			"distinct": map[string]any{"type": "boolean"},
		}, "function", "column", "alias")},
		"filters":  map[string]any{"type": "array", "items": filter},
		"having":   map[string]any{"type": "array", "items": filter},
		"group_by": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "uniqueItems": true},
		"order_by": map[string]any{"type": "array", "items": objectSchema(map[string]any{
			"column": map[string]any{"type": "string"}, "direction": map[string]any{"type": "string", "enum": []string{"", "asc", "desc"}},
		}, "column")},
		"limit":  map[string]any{"type": "integer", "minimum": 0},
		"offset": map[string]any{"type": "integer", "minimum": 0},
	}, "columns")
	schema["oneOf"] = []any{
		map[string]any{"required": []string{"product"}, "not": map[string]any{"required": []string{"from"}}},
		map[string]any{"required": []string{"from"}, "not": map[string]any{"required": []string{"product"}}},
	}
	return schema
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
	if s.principalEnabled(context.Background(), principal) != nil {
		return nil
	}
	switch principal.Role {
	case "query":
		return append(append([]mcp.Tool(nil), queryTools...), retrievalTools...)
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
	if err := s.principalEnabled(ctx, principal); err != nil {
		return mcp.ToolResult{}, toolError(err)
	}
	if principal.Role == "query" {
		switch name {
		case "list_data_products":
			result, err = s.listDataProducts(ctx, principal, raw)
		case "describe_data_product":
			result, err = s.describeDataProduct(ctx, principal, raw)
		case "get_sql_capabilities":
			result, err = s.getSQLCapabilities(ctx, principal, raw)
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
		case "rebind_data_task":
			result, err = s.rebindDataTask(ctx, principal, raw)
		case executePlanTool.Name:
			result, err = s.executePlan(ctx, principal, raw)
		case "query_sql":
			result, err = s.querySQL(ctx, principal, raw)
		case searchTool.Name:
			result, err = s.search(ctx, principal, raw)
		case "get_query_result":
			result, err = s.getQueryResult(ctx, principal, raw)
		case "preview_result":
			result, err = s.previewResult(ctx, principal, raw)
		case "deliver_result":
			result, err = s.deliverResult(ctx, principal, raw)
		case "get_budget":
			result, err = s.getBudget(ctx, principal, raw)
		case "list_receipts":
			result, err = s.listReceipts(ctx, principal, raw)
		case "complete_task":
			result, err = s.completeTask(ctx, principal, raw)
		case "revoke_task":
			result, err = s.revokeTask(ctx, principal, raw)
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

func (s *Service) principalEnabled(ctx context.Context, principal mcp.Principal) error {
	stored, err := s.store.GetPrincipal(ctx, principal.ID)
	if err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return forbidden()
		}
		return err
	}
	if stored.Subject != principal.Subject || stored.Role != principal.Role || stored.DisabledAt != nil {
		return forbidden()
	}
	return nil
}
