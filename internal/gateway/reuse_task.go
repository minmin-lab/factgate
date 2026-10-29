package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Reusing a covering task (the demo only, not a paper claim; the author's
// observation of 2026-10-29). An administrator pre-provisions standing
// authorizations, but an agent that never calls list_my_tasks requests a new
// task anyway and waits for an approval nobody needed. With the gateway
// switch GATEWAY_REUSE_COVERING_TASK on (Config.ReuseCoveringTask; it is not
// a Catalog key, so it changes no Catalog digest and strands no task), a
// plain root request -- request_data_task without parent_task_id or
// delegate_principal_id; never an issuance or a download task -- that one of
// the requester's ACTIVE tasks already covers is answered with that task: no
// task row, no OA draft, one TASK_REQUEST_REUSED_EXISTING audit event on the
// reused task. Budget and exposure accounting simply continue on it. Off,
// createTask never calls into this file and every tool description stays as
// it is.
//
// A task covers a request when, read from its stored grant:
//   - every requested product is approved, with every requested column;
//   - for every scope the request names (after the ordered-scope closure the
//     request gets, scope_order.go), the approved values are a superset (a
//     date range: within the approved bounds); a scope the grant leaves
//     unrestricted covers any value;
//   - every scope the grant restricts that applies to a requested product (a
//     published field or a mandatory scope of it) is named by the request: a
//     request leaving it open asks for more than the grant admits. A grant's
//     restriction on a dimension no requested product publishes is moot.
//   - the grant has not expired.
//
// Products the grant approves beyond the request do not matter. Each scope
// dimension is checked on its own, so a grant covers only requests inside the
// product of its per-dimension sets, which is exactly what it admits. A task
// that covers is reused only when query_sql would accept it: same Catalog
// version and digest, verified grant evidence, an active family and View
// binding, and not a download task. Several covering tasks: the one expiring
// latest (the lowest task id on a tie).

const taskRequestReusedEvent = "TASK_REQUEST_REUSED_EXISTING"

// reuseCoveringTaskDescriptions is what the switch adds to the tool
// descriptions; nothing when it is off.
var reuseCoveringTaskDescriptions = map[string]string{
	"request_data_task": "本网关启用了授权复用：不带 parent_task_id 与 delegate_principal_id 的申请，若已被你名下一项生效中的授权（含管理员预先开通的授权单）完全覆盖——产品、字段、各范围都不超出且未过期——直接返回那项授权（reused_existing_task: true，state 为 ACTIVE，reuse_note 说明复用了哪项），不新建任务、无需审批，可立即用返回的 task_id 查询。",
	"list_my_tasks":     "本网关启用了授权复用：列表里也有管理员预先为你开通的常设授权（ACTIVE 的授权单），查数前可先看是否已有覆盖所需产品与范围的生效授权并直接用其 task_id；被覆盖的 request_data_task 申请也会自动复用它。",
}

func reuseCoveringTaskTool(base mcp.Tool) mcp.Tool {
	if extra, ok := reuseCoveringTaskDescriptions[base.Name]; ok {
		base.Description += extra
	}
	return base
}

// coveringTask is a reusable task found for a request, with its stored grant
// and verified grant evidence.
type coveringTask struct {
	task          control.Task
	grant         control.TaskGrant
	protocol      approval.TaskGrantV1
	approvedScope map[string]any
}

// answerWithCoveringTask answers a request the requester's ACTIVE tasks
// already cover, or returns nil when none does. columns and scope are the
// request after normalization and closure; requested are its Catalog products.
func (s *Service) answerWithCoveringTask(ctx context.Context, principal mcp.Principal, args taskRequestArgs,
	requested []catalog.Product, columns map[string][]string, scope map[string]any, scopeClosure map[string][]string,
	now time.Time) (map[string]any, error) {
	covering, err := s.findCoveringTask(ctx, principal, args.DataProducts, requested, columns, scope, now)
	if err != nil || covering == nil {
		return nil, err
	}
	products := append([]string(nil), args.DataProducts...)
	sort.Strings(products)
	requestedColumns := cloneColumns(columns)
	for product := range requestedColumns {
		sort.Strings(requestedColumns[product])
	}
	audit := map[string]any{
		"requested_objective": args.Objective, "reused_task_id": covering.task.ID,
		"reused_objective": covering.task.Objective, "human_subject": principal.Subject, "agent_id": principal.ID,
		"requested_products": products, "requested_columns": requestedColumns, "requested_scopes": scope,
		"grant_expires_at": covering.grant.ExpiresAt,
	}
	if len(scopeClosure) > 0 {
		audit["scope_closure"] = scopeClosure
	}
	payload, err := json.Marshal(audit)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.AppendAuditEvent(ctx, control.AuditEvent{TaskID: covering.task.ID, Actor: principal.ID,
		EventType: taskRequestReusedEvent, OccurredAt: now, Payload: payload}); err != nil {
		return nil, err
	}
	return s.reusedTaskResponse(ctx, args, covering, scopeClosure)
}

// findCoveringTask pages through the principal's ACTIVE tasks and returns the
// reusable one that covers the request and expires latest, nil if none.
func (s *Service) findCoveringTask(ctx context.Context, principal mcp.Principal, products []string,
	requested []catalog.Product, columns map[string][]string, scope map[string]any, now time.Time) (*coveringTask, error) {
	const page = 500
	relevant := publishedScopeNames(requested)
	var best *coveringTask
	afterID := ""
	for {
		tasks, err := s.store.ListTasks(ctx, control.TaskFilter{PrincipalID: principal.ID, State: control.TaskActive,
			AfterID: afterID, Limit: page})
		if err != nil {
			return nil, err
		}
		for _, task := range tasks {
			afterID = task.ID
			candidate, err := s.coveringCandidate(ctx, task, products, columns, scope, relevant, now)
			if err != nil {
				return nil, err
			}
			if candidate != nil && (best == nil || candidate.grant.ExpiresAt.After(best.grant.ExpiresAt)) {
				best = candidate
			}
		}
		if len(tasks) < page {
			return best, nil
		}
	}
}

// coveringCandidate returns the task when it covers the request and query_sql
// would accept it, nil when it does not.
func (s *Service) coveringCandidate(ctx context.Context, task control.Task, products []string, columns map[string][]string,
	scope map[string]any, relevant map[string]bool, now time.Time) (*coveringTask, error) {
	if task.State != control.TaskActive || task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, nil
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if errors.Is(err, control.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if grant.ExpiresAt.Sub(now) < time.Millisecond {
		return nil, nil
	}
	if _, isDownload, listErr := downloadListOf(grant); listErr != nil || isDownload {
		return nil, nil
	}
	var approved map[string]any
	if len(grant.MandatoryScope) > 0 {
		if err := json.Unmarshal(grant.MandatoryScope, &approved); err != nil {
			return nil, nil
		}
	}
	if !grantCoversRequest(grant, approved, products, columns, scope, relevant) {
		return nil, nil
	}
	// It covers; reuse it only if a query on it would not be refused.
	reason := ""
	protocol, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	switch {
	case err != nil || approval.VerifyTaskGrantV1(s.receiptVerifier, protocol) != nil ||
		!storedGrantMatchesProtocol(task, grant, protocol) || protocol.Core.CatalogSHA256 != s.catalog.SHA256:
		reason = "grant evidence does not verify against this Catalog"
	case s.ensureActiveTaskFamily(ctx, task) != nil || s.validateDelegatedGrant(ctx, task, protocol.Core, now) != nil:
		reason = "task family is not active"
	case grant.ViewBindingDigest != "":
		if status, statusErr := s.store.GetTaskViewBindingStatus(ctx, task.ID); statusErr != nil || status.Status != control.TaskViewBindingActive {
			reason = "View binding is not active"
		}
	}
	if reason != "" {
		s.logger.Info("covering task not reused", "task_id", task.ID, "reason", reason)
		return nil, nil
	}
	return &coveringTask{task: task, grant: grant, protocol: protocol, approvedScope: approved}, nil
}

// publishedScopeNames is every name a requested product publishes as a field
// or declares as a mandatory scope: the scope dimensions a grant's
// restriction can apply to.
func publishedScopeNames(products []catalog.Product) map[string]bool {
	names := map[string]bool{}
	for _, product := range products {
		for _, name := range product.Scopes {
			names[name] = true
		}
		for _, field := range product.Fields {
			names[field.Name] = true
		}
	}
	return names
}

// grantCoversRequest is the coverage rule above, on a stored grant and its
// decoded approved scope.
func grantCoversRequest(grant control.TaskGrant, approved map[string]any, products []string, columns map[string][]string,
	scope map[string]any, relevant map[string]bool) bool {
	for _, product := range products {
		if !contains(grant.ApprovedProducts, product) {
			return false
		}
		for _, column := range columns[product] {
			if !contains(grant.ApprovedColumns[product], column) {
				return false
			}
		}
	}
	for name, requested := range scope {
		if granted, restricted := approved[name]; restricted && !scopeValueCovers(granted, requested) {
			return false
		}
	}
	for name := range approved {
		if _, named := scope[name]; !named && relevant[name] {
			return false
		}
	}
	return true
}

// scopeValueCovers reports whether a granted scope value admits everything a
// requested one asks for: a superset of an enum set, or date bounds that
// contain the requested range (an empty bound is open).
func scopeValueCovers(granted, requested any) bool {
	if grantedValues, ok := scopeEnumValues(granted); ok {
		requestedValues, ok := scopeEnumValues(requested)
		if !ok || len(requestedValues) == 0 {
			return false
		}
		for _, value := range requestedValues {
			if !contains(grantedValues, value) {
				return false
			}
		}
		return true
	}
	grantedFrom, grantedTo, ok := scopeDateBounds(granted)
	if !ok {
		return false
	}
	requestedFrom, requestedTo, ok := scopeDateBounds(requested)
	if !ok {
		return false
	}
	if grantedFrom != "" && (requestedFrom == "" || requestedFrom < grantedFrom) {
		return false
	}
	if grantedTo != "" && (requestedTo == "" || requestedTo > grantedTo) {
		return false
	}
	return true
}

// scopeEnumValues reads an enum scope value: a set, or a single string.
func scopeEnumValues(raw any) ([]string, bool) {
	if value, ok := raw.(string); ok {
		return []string{value}, true
	}
	return scopeStringValues(raw)
}

// scopeDateBounds reads a date-range scope value, {from, to} with either
// bound possibly empty, as normalizeDateRange leaves it or as it decodes from
// a stored grant.
func scopeDateBounds(raw any) (string, string, bool) {
	switch typed := raw.(type) {
	case map[string]string:
		for key := range typed {
			if key != "from" && key != "to" {
				return "", "", false
			}
		}
		return typed["from"], typed["to"], true
	case map[string]any:
		bounds := map[string]string{}
		for key, value := range typed {
			text, ok := value.(string)
			if !ok || (key != "from" && key != "to") {
				return "", "", false
			}
			bounds[key] = text
		}
		return bounds["from"], bounds["to"], true
	default:
		return "", "", false
	}
}

// reusedTaskResponse is request_data_task's response for a reused task: the
// usual fields read from the task, its private request context and its
// grant, with the approved products, columns and scopes, and the reuse
// marker.
func (s *Service) reusedTaskResponse(ctx context.Context, args taskRequestArgs, covering *coveringTask,
	scopeClosure map[string][]string) (map[string]any, error) {
	task, grant, core := covering.task, covering.grant, covering.protocol.Core
	result := map[string]any{
		"task_id": task.ID, "state": task.State, "objective": task.Objective,
		"reused_existing_task": true,
		"reuse_note":           "已复用你名下生效中的授权「" + task.Objective + "」，无需审批",
		"requested_objective":  args.Objective,
		"sensitivity":          grant.SensitivityCeiling, "catalog_version": task.CatalogVersion,
		"datasource_id": grant.DatasourceID, "schema_digest": grant.SchemaDigest,
		"manifest_digest": core.ManifestDigest, "budget_source": "reused_existing_task",
		"root_task_id": defaultString(task.RootTaskID, task.ID), "parent_task_id": task.ParentTaskID,
		"delegate_principal_id": task.PrincipalID, "expires_at": grant.ExpiresAt,
		"approved_products": append([]string(nil), grant.ApprovedProducts...),
		"approved_columns":  cloneColumns(grant.ApprovedColumns),
		"scopes":            covering.approvedScope,
		// Nothing is left to approve: the task is already ACTIVE.
		"approval_chain": []map[string]any{},
		"budget": map[string]any{"max_queries": core.Budget.MaxQueries, "max_rows": core.Budget.MaxResultRows,
			"max_db_ms": core.Budget.MaxDBMS, "query_timeout_ms": core.Budget.PerQueryTimeoutMS,
			"task_ttl_seconds":  core.Budget.TaskTTLMS / 1000,
			"max_release_facts": core.Budget.MaxReleaseFacts, "max_influence_facts": core.Budget.MaxInfluenceFacts,
			"max_outcome_facts":        core.Budget.MaxOutcomeFacts,
			"exposure_profile_version": core.Budget.ExposureProfileVersion,
			"predicate_footprint":      core.Budget.PredicateFootprint},
		"next_action_hint": "已复用生效中的授权，无需新建任务或等待审批：直接用该 task_id 查询（query_sql、search、fetch、aggregate）；get_task_context 可看批准的产品、列、范围与期限",
	}
	if persisted, err := decodePersistedPending(task); err == nil {
		pending := persisted.pendingContext
		result["approval_mode"], result["approver"] = string(pending.ApprovalMode), pending.Approver
		if name := pending.BudgetProfile; name != "" {
			result["budget_profile"] = name
		} else if name := s.budgetProfileName(pending.Budget); name != "" {
			result["budget_profile"] = name
		}
		if pending.SelfConfirm {
			result["self_confirm"] = true
		}
		if pending.MemberConfirm {
			result["member_confirm"] = true
		}
		if pending.IssuedBy != nil {
			result["issued_by"], result["human_subject"] = pending.IssuedBy.Subject, persisted.Manifest.HumanSubject
		}
	}
	if s.delegated() != nil {
		if _, kind, err := s.listedTask(ctx, task); err != nil {
			return nil, err
		} else if kind != "" {
			result["kind"] = kind
		}
	}
	if grant.ViewBindingDigest != "" {
		result["view_binding_digest"] = grant.ViewBindingDigest
	}
	if readiness := s.knowledgeReadiness(grant.ApprovedProducts, grant.ApprovedColumns, grant.Exposure.ProfileVersion, "approved"); len(readiness) > 0 {
		result["knowledge_readiness"] = readiness
	}
	if len(scopeClosure) > 0 {
		result["scope_closure"] = scopeClosure
	}
	if s.oaPublicBaseURL != "" && task.ApprovalRef != "" {
		result["oa_url"] = approval.DraftPageURL(s.oaPublicBaseURL, task.ApprovalRef)
	}
	mergeTaskFlow(result, s.taskFlow(task))
	return result, nil
}
