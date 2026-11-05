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
	{Name: "list_data_products", Description: "列出可申请的逻辑数据产品、字段、说明和敏感等级，以及知识产品（knowledge_products）：知识产品名、关系、检索方式与 k_max、属性值域，和 search/fetch/aggregate 各需任务授权的关系与列（task_requirements）。", InputSchema: objectSchema(nil), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "describe_data_product", Description: "读取一个逻辑数据产品的字段类型、排序规则、稳定角色和 SQL 白名单；name 为知识产品名（如 policy_kb）时返回该知识产品的检索说明与 task_requirements。", InputSchema: objectSchema(map[string]any{
		"name": map[string]any{"type": "string", "minLength": 1},
	}, "name"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_sql_capabilities", Description: "读取 TaskGate 报表 SQL profile 及可无损转换为规范 QueryPlan 的受限语法能力。", InputSchema: objectSchema(nil), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "request_data_task", Description: "使用显式产品、字段和范围创建根任务或受父 Grant 约束的委托任务及 OA 草稿。approver 可选：指定审批人（缺省按目录路由）；无论谁审批，TaskGate 都在签发授权前按数据产品规则核验审批人，无权者批准也不签发。目录启用 delegated_authorization.child_self_confirm 时，带 parent_task_id 的子任务由申请人本人在 OA 确认（返回 self_confirm: true），根任务仍由他人审批。申请涉及知识产品时，返回值的 knowledge_readiness 预告该申请能否 search/fetch/aggregate、缺哪些关系或列，可在提交前改正。", InputSchema: objectSchema(map[string]any{
		"objective":             map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
		"approver":              map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
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
	{Name: "list_my_tasks", Description: "列出自己名下的任务：自己发起的任务，以及管理员为自己开的授权单。可按 state 过滤。", InputSchema: objectSchema(map[string]any{
		"state":  map[string]any{"type": "string", "enum": []string{"AWAITING_SUBMISSION", "AWAITING_APPROVAL", "ACTIVE", "ARCHIVED"}},
		"cursor": map[string]any{"type": "string"},
	}), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_status", Description: "读取自己的任务状态；未批准的任务另给 waiting_for、next_action、next_action_hint 与 oa_url：AWAITING_SUBMISSION 表示须由申请人本人在 OA 提交草稿（agent 不能代交），AWAITING_APPROVAL 表示等待审批人决定。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "wait_for_approval", Description: "短轮询等待审批，单次最长 45 秒（缺省 30 秒），任务变为 ACTIVE 或归档即返回；更久的等待请循环调用。返回同 get_task_status 的 waiting_for 与 next_action：若是 human_submit_in_oa，继续等待没有用，须由申请人本人先在 OA 提交草稿。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": waitForApprovalMaxSeconds},
	}, "task_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "get_task_context", Description: "读取 ACTIVE 任务的批准范围、预算和期限。", InputSchema: taskIDSchema(), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "rebind_data_task", Description: "为语义 View 已变化的旧任务创建一个全新的根任务和 OA 草稿；旧 Grant、查询、结果与 Receipt 不会被修改。", InputSchema: taskIDSchema()},
	{Name: "query_sql", Description: "执行任务授权范围内的报表 SQL，把加密 Parquet 规范原件保存在 TaskGate 对象存储，并仅返回摘要与 result_id。启用精确暴露记账时，SQL 必须能够无损转换为 TaskGate 规范计划。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "request_id": requestIDSchema(), "sql": map[string]any{"type": "string", "minLength": 1, "maxLength": 100000},
	}, "task_id", "request_id", "sql")},
	{Name: "get_query_result", Description: "读取自己的查询结果元数据和 result_id；完整数据仍保留在 TaskGate 对象存储。", InputSchema: objectSchema(map[string]any{
		"task_id": map[string]any{"type": "string"}, "query_id": map[string]any{"type": "string"},
	}, "task_id", "query_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "preview_result", Description: "按 result_id 读取最多 100 行的有界预览（缺省 20 行），用 offset 分页：返回 has_more 与 next_offset，按 next_offset 续读直到 has_more 为 false；预览不计费，不交付完整 Parquet（完整结果用 deliver_result）。", InputSchema: objectSchema(map[string]any{
		"result_id": map[string]any{"type": "string", "minLength": 1},
		"offset":    map[string]any{"type": "integer", "minimum": 0},
		"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
	}, "result_id"), Annotations: map[string]any{"readOnlyHint": true}},
	{Name: "deliver_result", Description: "为已消费的规范结果生成短期 Parquet 交付地址（有效期见 download_url_expires_at，缺省约 5 分钟）；地址过期后再调用即得新地址，不计费；下载次数不会改变预算、消费状态或 Receipt。", InputSchema: objectSchema(map[string]any{
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
	{Name: "revoke_task", Description: "撤销自己的任务并阻止新查询；已在途查询仍由原授权期限和超时约束。也可用于作废尚未提交或尚在审批中的任务（撤销后 OA 再批准也不会签发授权；OA 草稿本身不会被删除）。未批准的任务不会自动过期，不再需要时请撤销，可附 reason。", InputSchema: objectSchema(map[string]any{
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

// delegatedRequestDataTaskTool is request_data_task as a Catalog with the
// delegated_authorization block offers it (KB1-LOOP-148): an optional
// budget_profile, and the rules that make a request self-service or
// member-confirmed; since KB1-LOOP-150 also that a chosen profile makes a
// project and no profile a day task, naming the Catalog's profiles. A Catalog
// without the block lists the plain tool.
func delegatedRequestDataTaskTool(base mcp.Tool, vocabulary budgetVocabulary) mcp.Tool {
	properties := map[string]any{}
	if existing, ok := base.InputSchema["properties"].(map[string]any); ok {
		for name, schema := range existing {
			properties[name] = schema
		}
	}
	properties["budget_profile"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
	required, _ := base.InputSchema["required"].([]string)
	base.InputSchema = objectSchema(properties, required...)
	base.Description += "本目录启用了长期授权模型（delegated_authorization）：budget_profile 可选，取目录里与路由同一 exposure 版本的预算档（如一年期）；" +
		"申请只含本人的个人记录、或整条审批链都在申请人本人职权内（含中心主任本中心、数据管家部门的人员本人）时，任务由申请人本人确认（返回 self_service: true）；" +
		"带 parent_task_id 并用 delegate_principal_id 委托给其他成员的子任务，由该成员本人确认、资格按该成员判定（返回 member_confirm: true）。" +
		"要立项目（长期、可反复在其下开子任务）就填 budget_profile" + vocabulary.chosenExample() + "；不填则是按审批路由缺省预算档" + vocabulary.defaultList() + "的日常任务。"
	return base
}

// delegatedListMyTasksTool is list_my_tasks as a Catalog with the
// delegated_authorization block offers it (KB1-LOOP-150): every task is said
// to be a project, a day task, a child inside a project or an issued
// authorization, with its budget profile, products, scopes and approval
// roles, and the list can be confined to one project or one kind. The terms
// and the Catalog's budget profiles are in the description, so an agent has
// the words. A Catalog without the block lists the plain tool.
func delegatedListMyTasksTool(base mcp.Tool, vocabulary budgetVocabulary) mcp.Tool {
	properties := map[string]any{}
	if existing, ok := base.InputSchema["properties"].(map[string]any); ok {
		for name, schema := range existing {
			properties[name] = schema
		}
	}
	properties["root_task_id"] = map[string]any{"type": "string", "minLength": 1}
	properties["kind"] = map[string]any{"type": "string", "enum": []string{taskKindProject, taskKindTask, taskKindChild, taskKindAuthorization, taskKindRoot}}
	required, _ := base.InputSchema["required"].([]string)
	base.InputSchema = objectSchema(properties, required...)
	base.Description += "本目录启用了长期授权模型。术语：项目 = 申请时选了预算档（budget_profile）的根任务" + vocabulary.chosenExample() +
		"；日常任务 = 申请时没选、按审批路由缺省预算档" + vocabulary.defaultList() + "的根任务；子任务 = 挂在项目下（parent_task_id）的任务；授权单 = 管理员代开给员工的根任务。" +
		"每条任务另给 kind（project 项目 / task 日常任务 / child 子任务 / authorization 授权单；本功能上线前建的根任务分不出项目与日常任务，给 root）、" +
		"budget_profile、products、scopes、approver、self_confirm、member_confirm、issued_by、agent_submitted。" +
		"回答“有哪些项目”时用 kind=project 过滤；root_task_id 可列某个项目及其子任务。"
	return base
}

// agentSubmitsDescriptions is what the task tools add to their description
// when the Catalog's delegated_authorization.agent_submits is on
// (KB1-LOOP-149): the agent submits a task at creation, so no task waits for
// a person's submission. Off, the descriptions stay as they are.
var agentSubmitsDescriptions = map[string]string{
	"request_data_task":        "本目录启用了 agent_submits：任务创建即由 agent 代申请人提交，状态为 AWAITING_APPROVAL（返回 agent_submitted: true），无需人提交，审批人（或本人、受托成员）在 OA 点“批准”即可生效。",
	"request_download_task":    "本目录启用了 agent_submits：任务创建即由 agent 代申请人提交，状态为 AWAITING_APPROVAL，无需人提交，审批人在 OA 点“批准”即可生效。",
	"issue_data_authorization": "本目录启用了 agent_submits：授权单创建即由 agent 提交，状态为 AWAITING_APPROVAL，无需人提交，发单人在 OA 直接批准。",
	"get_task_status":          "本目录启用了 agent_submits：新任务创建即 AWAITING_APPROVAL，无需人提交，不会处于 AWAITING_SUBMISSION。",
	"wait_for_approval":        "本目录启用了 agent_submits：新任务创建即 AWAITING_APPROVAL，无需人提交，不会返回 human_submit_in_oa。",
	"revoke_task":              "本目录启用了 agent_submits：新任务创建即 AWAITING_APPROVAL（无需人提交），未批准的任务都在审批中，同样可以作废。",
}

func agentSubmitsTool(base mcp.Tool) mcp.Tool {
	if extra, ok := agentSubmitsDescriptions[base.Name]; ok {
		base.Description += extra
	}
	return base
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
		tools := append(append(append([]mcp.Tool(nil), queryTools...), retrievalToolsFor(s.catalog)...), downloadTools...)
		if s.delegated() != nil {
			vocabulary := budgetVocabularyOf(s.catalog)
			for index := range tools {
				switch tools[index].Name {
				case "request_data_task":
					tools[index] = delegatedRequestDataTaskTool(tools[index], vocabulary)
				case "list_my_tasks":
					tools[index] = delegatedListMyTasksTool(tools[index], vocabulary)
				}
			}
			tools = append(tools, issueTool, whoamiTool)
		}
		if ordered := s.orderedScopesDescription(); ordered != "" {
			for index := range tools {
				if orderedScopeTools[tools[index].Name] {
					tools[index].Description += ordered
				}
			}
		}
		if s.agentSubmits() {
			for index := range tools {
				tools[index] = agentSubmitsTool(tools[index])
			}
		}
		if s.reuseCoveringTask {
			for index := range tools {
				tools[index] = reuseCoveringTaskTool(tools[index])
			}
		}
		return tools
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
		case issueTool.Name:
			result, err = s.issueDataAuthorization(ctx, principal, raw)
		case whoamiTool.Name:
			// Listed and served only under the delegated_authorization block;
			// without it, forbidden like every other unlisted tool.
			if s.delegated() == nil {
				err = forbidden()
			} else {
				result, err = s.whoami(ctx, principal, raw)
			}
		case executePlanTool.Name:
			result, err = s.executePlan(ctx, principal, raw)
		case "query_sql":
			result, err = s.querySQL(ctx, principal, raw)
		case searchTool.Name:
			result, err = s.search(ctx, principal, raw)
		case fetchTool.Name:
			result, err = s.fetch(ctx, principal, raw)
		case aggregateTool.Name:
			result, err = s.aggregate(ctx, principal, raw)
		case requestDownloadTaskTool.Name:
			result, err = s.requestDownloadTask(ctx, principal, raw)
		case downloadDocumentTool.Name:
			// The one tool whose result carries an embedded file resource.
			toolResult, downloadErr := s.downloadDocument(ctx, principal, raw)
			if downloadErr != nil {
				return mcp.ToolResult{}, toolError(downloadErr)
			}
			return toolResult, nil
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
	if s.delegated() == nil || result == nil {
		return mcp.ToolResult{Structured: result}, nil
	}
	// Under the delegated_authorization block every structured result says
	// whom it was produced for (KB1-LOOP-151): the agent's identity can be
	// switched in the OA between two calls.
	return mcp.ToolResult{Structured: withActingAs(result, s.actingAs(principal))}, nil
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
