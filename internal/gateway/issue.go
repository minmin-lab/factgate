package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Issued authorizations (KB1-LOOP-147; the delegated-authorization demo, not
// a paper claim). A department administrator's agent opens a long-lived
// authorization for an employee of the administrator's own department: the
// task is the employee's (its human subject and principal are the employee's),
// the administrator is its first approver, and the administrator may submit
// the draft, so one OA login submits and approves it. The scope must stay
// inside the administrator's department; a cross-department authorization is
// a project the employee requests themselves. The issuer is recorded in the
// task's private context, on the OA draft and in a TASK_ISSUED_FOR audit
// event; the approval receipt binds the administrator as approver as usual.

var issueTool = mcp.Tool{Name: "issue_data_authorization",
	Description: "部门管理员为本部门员工开一张长期授权单（根任务）：for_principal_id 是员工的主体 id，budget_profile 选目录里的预算档（如一年期）。申请人记为该员工，管理员本人是第一审批人并可代为提交，范围只能是管理员自己的部门（跨部门请让员工自己申请项目）。需要目录启用 delegated_authorization 且管理员具备 issuer_role。",
	InputSchema: objectSchema(map[string]any{
		"for_principal_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"objective":        map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
		"budget_profile":   map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"data_products":    map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true},
		"columns": map[string]any{"type": "object", "minProperties": 1, "additionalProperties": map[string]any{
			"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true,
		}},
		"scopes": map[string]any{"type": "object", "additionalProperties": map[string]any{"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "uniqueItems": true},
			objectSchema(map[string]any{"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"}}),
		}}},
	}, "for_principal_id", "objective", "budget_profile", "data_products", "columns", "scopes")}

// issuanceRequest is what issue_data_authorization adds to a task request.
type issuanceRequest struct {
	ForPrincipalID string
	BudgetProfile  string
}

// issuerRecord names who issued a task for its human subject.
type issuerRecord struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
}

func (s *Service) issueDataAuthorization(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	if s.delegated() == nil {
		return nil, forbidden()
	}
	var args struct {
		ForPrincipalID string              `json:"for_principal_id"`
		Objective      string              `json:"objective"`
		BudgetProfile  string              `json:"budget_profile"`
		DataProducts   []string            `json:"data_products"`
		Columns        map[string][]string `json:"columns"`
		Scopes         map[string]any      `json:"scopes"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	request := taskRequestArgs{Objective: args.Objective, DataProducts: args.DataProducts, Columns: args.Columns, Scopes: args.Scopes,
		issuance: &issuanceRequest{ForPrincipalID: strings.TrimSpace(args.ForPrincipalID), BudgetProfile: strings.TrimSpace(args.BudgetProfile)}}
	return s.createTask(ctx, principal, request, nil)
}

// issuerAuthority checks the issuer against the Catalog's issuer role.
func (s *Service) issuerAuthority(issuer mcp.Principal, now time.Time) error {
	delegated := s.delegated()
	if delegated == nil {
		return forbidden()
	}
	if s.principalAttributes == nil || !s.principalAttributes.Known(issuer.Subject) ||
		!anyIn(s.principalAttributes.Values(issuer.Subject, delegated.IssuerRole.Attribute, now), delegated.IssuerRole.AnyOf) {
		return &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "当前身份不具备代开授权单的角色（目录 delegated_authorization.issuer_role）"}
	}
	return nil
}

// issuedPrincipal resolves the employee an authorization is issued for.
func (s *Service) issuedPrincipal(ctx context.Context, issuer mcp.Principal, forPrincipalID string) (control.Principal, error) {
	employee, err := s.store.GetPrincipal(ctx, forPrincipalID)
	if err != nil || employee.Role != "query" || employee.DisabledAt != nil {
		return control.Principal{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "for_principal_id 必须是已启用的查询主体"}
	}
	if employee.ID == issuer.ID || strings.EqualFold(employee.Subject, issuer.Subject) {
		return control.Principal{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "不能给自己开授权单；自己的申请用 request_data_task"}
	}
	return employee, nil
}

// applyIssuance confines an issued authorization to the issuer's own
// department and replaces the route's budget with the chosen profile. It
// needs a chain route: the chain's org chart is what makes the issuer the
// department's approver.
func (s *Service) applyIssuance(policy *catalog.TaskPolicy, issuer mcp.Principal, scope map[string]any,
	issuance *issuanceRequest, now time.Time) error {
	chain := policy.ApprovalRoute.Chain
	if chain == nil {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "代开授权单需要审批链路由（目录 approval_routes[].chain）"}
	}
	departments := s.principalAttributes.Values(issuer.Subject, chain.RequesterAttribute, now)
	if len(departments) != 1 {
		return &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized,
			Message: fmt.Sprintf("无法确定发单人 %s 所在的部门（属性 %s 须恰有一个值）", issuer.Subject, chain.RequesterAttribute)}
	}
	requested, _ := scopeStringValues(scope[chain.DepartmentScope])
	if len(requested) == 0 {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: fmt.Sprintf("授权单必须给出范围 %s", chain.DepartmentScope)}
	}
	for _, department := range requested {
		if department != departments[0] {
			return &mcp.ToolError{Code: apierr.CodeInvalidRequest,
				Message: fmt.Sprintf("代开单只能分配本部门（%s）的资源，范围含 %s；跨部门请由员工自己发起项目申请", departments[0], department)}
		}
	}
	profile, ok := s.catalog.LookupBudgetProfile(issuance.BudgetProfile)
	if !ok {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "budget_profile 不是目录里的预算档"}
	}
	if profile.ExposureProfileVersion != policy.Budget.ExposureProfileVersion {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "budget_profile 的 exposure_profile_version 与路由的预算档不一致"}
	}
	policy.Budget, policy.BudgetProfile = profile.Budget(), profile.Name
	return nil
}

// issuedOrOwnedTask is ownedTask for the tools an issuer may also use on the
// task they issued: status, waiting and revocation, never queries.
func (s *Service) issuedOrOwnedTask(ctx context.Context, principal mcp.Principal, taskID string) (control.Task, error) {
	task, err := s.ownedTask(ctx, principal, taskID)
	if err == nil {
		return task, nil
	}
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != apierr.CodeNotFound || principal.Role != "query" {
		return control.Task{}, err
	}
	task, loadErr := s.store.GetTask(ctx, taskID)
	if loadErr != nil {
		return control.Task{}, err
	}
	persisted, decodeErr := decodePersistedPending(task)
	if decodeErr != nil || persisted.IssuedBy == nil || persisted.IssuedBy.PrincipalID != principal.ID {
		return control.Task{}, err
	}
	return task, nil
}
