package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Approval chains (KB1-LOOP-141; a demo feature, not a paper claim).
//
// A chain route has the OA approve a task in sequence: the requester's own
// department approver, then the approver of every other requested
// department, then, for each escalation layer the task's scope triggers (for
// example confidential or leadership data), the person in charge of each
// requested department on that layer. TaskGate is the authority on
// the chain: after each approval it derives, from the Catalog's org chart and
// the scope that approval left (an approver may narrow), who must approve
// next and tells the OA; it checks every approver's authority against the
// attested principal attributes; and it issues the grant only when no step
// remains. The OA protocol is unchanged: one approval callback per approver,
// each with its own signed receipt.

// pendingChain is the private routing record of a chain task.
type pendingChain struct {
	Route catalog.ApprovalChain `json:"route"`
	// RequesterDepartment is the requester's attested department at request
	// time; their own director approves first.
	RequesterDepartment string `json:"requester_department"`
	// ChosenFirst replaces the first approver, as a misrouted OA would; it is
	// verified like every step when it approves.
	ChosenFirst string `json:"chosen_first,omitempty"`
	// RequesterSubject switches on the delegated-authorization rules
	// (KB1-LOOP-148): the chain is ApprovalChain.StepsFor this requester,
	// with the authority, centers and stewards recorded below at request time,
	// so every step re-derives it the same way. Empty (an issued task, a
	// Catalog without the block, a record written before the rules) keeps
	// ApprovalChain.Steps.
	RequesterSubject string `json:"requester_subject,omitempty"`
	// Authority is the requester's departments of authority.
	Authority []string `json:"authority,omitempty"`
	// CenterScope is set when every requested product declares the Catalog's
	// center scope; Centers are the centers the requester heads.
	CenterScope string   `json:"center_scope,omitempty"`
	Centers     []string `json:"centers,omitempty"`
	// Stewards are the steward departments when every requested product
	// declares one.
	Stewards []string `json:"stewards,omitempty"`
}

func (chain pendingChain) steps(scope map[string][]string) ([]catalog.ApprovalStep, error) {
	var (
		steps []catalog.ApprovalStep
		err   error
	)
	switch {
	case chain.RequesterSubject == "":
		steps, err = chain.Route.Steps(chain.RequesterDepartment, scope)
	case chain.Route.CenterCovers(chain.RequesterDepartment, scope, chain.CenterScope, chain.Centers):
		// A center head's own center: no step remains.
	default:
		steps, err = chain.Route.StepsFor(chain.RequesterDepartment, chain.RequesterSubject, chain.Authority,
			chain.Route.StewardScope(chain.RequesterDepartment, chain.Authority, chain.Stewards, scope))
	}
	if err != nil {
		return nil, err
	}
	if chain.ChosenFirst != "" && len(steps) > 0 {
		steps[0].Approver = chain.ChosenFirst
	}
	return steps, nil
}

// chainApproval is one recorded approval of a chain task.
type chainApproval struct {
	Actor    string
	Decision string
	At       time.Time
	Core     domain.TaskGrantCoreV1
}

// chainProgress is where a chain task stands.
type chainProgress struct {
	Approvals []chainApproval
	// Current is the core the last approval issued, nil before the first.
	Current *domain.TaskGrantCoreV1
	// Steps is the chain the current scope needs; Next is its first step not
	// yet approved, nil when every step is.
	Steps []catalog.ApprovalStep
	Next  *catalog.ApprovalStep
}

func (progress chainProgress) approved(approver string) bool {
	for _, approval := range progress.Approvals {
		if strings.EqualFold(approval.Actor, approver) {
			return true
		}
	}
	return false
}

// remaining lists the steps of chain for scope that no recorded approval,
// nor extra, has approved.
func (progress chainProgress) remaining(chain pendingChain, scope map[string][]string, extra string) ([]catalog.ApprovalStep, error) {
	steps, err := chain.steps(scope)
	if err != nil {
		return nil, err
	}
	var out []catalog.ApprovalStep
	for _, step := range steps {
		if !progress.approved(step.Approver) && !strings.EqualFold(step.Approver, extra) {
			out = append(out, step)
		}
	}
	return out, nil
}

// chainProgressOf reads a chain task's approvals from its approval events.
func (s *Service) chainProgressOf(ctx context.Context, task control.Task, persisted persistedPendingContext) (chainProgress, error) {
	chain := *persisted.pendingContext.Chain
	events, err := s.store.ListApprovalEvents(ctx, task.ID)
	if err != nil {
		return chainProgress{}, err
	}
	var progress chainProgress
	for _, event := range events {
		if event.Decision != "approved" && event.Decision != "narrowed" {
			continue
		}
		var recorded oaCallbackEvent
		decoder := json.NewDecoder(bytes.NewReader(event.Payload))
		if err := decoder.Decode(&recorded); err != nil || recorded.ApprovedGrant == nil {
			return chainProgress{}, fmt.Errorf("approval event %s carries no approved grant", event.EventID)
		}
		progress.Approvals = append(progress.Approvals, chainApproval{Actor: event.Actor, Decision: event.Decision,
			At: event.CreatedAt, Core: *recorded.ApprovedGrant})
	}
	scope := scopeValueSets(persisted.Manifest.MandatoryScope)
	if count := len(progress.Approvals); count > 0 {
		current := progress.Approvals[count-1].Core
		progress.Current = &current
		scope = scopeValueSets(current.MandatoryScope)
	}
	if progress.Steps, err = chain.steps(scope); err != nil {
		return chainProgress{}, err
	}
	for index := range progress.Steps {
		if !progress.approved(progress.Steps[index].Approver) {
			next := progress.Steps[index]
			progress.Next = &next
			break
		}
	}
	return progress, nil
}

// chainStepAuthorized checks a step's approver against the attested
// attributes: the authority attribute holds every department of the step and
// the role condition holds.
func (s *Service) chainStepAuthorized(chain catalog.ApprovalChain, step catalog.ApprovalStep, approver string, now time.Time) bool {
	authority, ok := chain.Authority(step)
	approver = strings.ToLower(strings.TrimSpace(approver))
	if !ok || s.principalAttributes == nil || !s.principalAttributes.Known(approver) {
		return false
	}
	held := s.principalAttributes.Values(approver, authority.Attribute, now)
	for _, department := range step.Departments {
		if !contains(held, department) {
			return false
		}
	}
	return anyIn(s.principalAttributes.Values(approver, authority.Role.Attribute, now), authority.Role.AnyOf)
}

func anyIn(values, wanted []string) bool {
	for _, value := range values {
		if contains(wanted, value) {
			return true
		}
	}
	return false
}

// chainOrigin says where a new task's chain starts.
type chainOrigin struct {
	// DepartmentOf is the subject whose attested department starts the
	// chain: the requester, or the issuer of an issued task.
	DepartmentOf string
	// Rules applies the delegated-authorization rules for the requester
	// (pendingChain.RequesterSubject); off for an issued task and for a
	// Catalog without the block.
	Rules bool
	// Products are the requested products, whose declarations decide whether
	// the center and steward rules apply.
	Products []catalog.Product
}

// planApprovalChain derives a new task's chain. Every Catalog-routed approver
// must hold its step's authority before a draft exists (a wrong org chart
// fails closed); a requester-chosen first approver is only previewed and is
// verified when they approve. Nobody approves their own request. Under the
// delegated-authorization rules the chain may be empty: selfService is then
// true and the requester confirms the task themselves.
func (s *Service) planApprovalChain(route catalog.ApprovalChain, humanSubject string, origin chainOrigin, chosenFirst string,
	scope map[string][]string, now time.Time) (chain pendingChain, public []map[string]any, selfService bool, err error) {
	if s.principalAttributes == nil {
		return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeConflict, Message: "审批链需要主体属性快照，本实例未配置"}
	}
	if origin.DepartmentOf == "" {
		origin.DepartmentOf = humanSubject
	}
	departments := s.principalAttributes.Values(origin.DepartmentOf, route.RequesterAttribute, now)
	if len(departments) != 1 {
		return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized,
			Message: fmt.Sprintf("无法确定 %s 所在的部门（属性 %s 须恰有一个值），审批链无法起始", origin.DepartmentOf, route.RequesterAttribute)}
	}
	chain = pendingChain{Route: route, RequesterDepartment: departments[0], ChosenFirst: chosenFirst}
	if delegated := s.delegated(); origin.Rules && delegated != nil {
		chain.RequesterSubject = strings.ToLower(strings.TrimSpace(humanSubject))
		chain.Authority = sortedValues(s.principalAttributes.Values(humanSubject, delegated.AuthorityAttributeName(), now))
		if delegated.CenterScope != "" && everyProductDeclares(origin.Products, delegated.CenterScope) {
			chain.CenterScope = delegated.CenterScope
			chain.Centers = sortedValues(s.principalAttributes.Values(humanSubject, delegated.CenterAttributeName(), now))
		}
		chain.Stewards = stewardDepartments(origin.Products)
	}
	steps, err := chain.steps(scope)
	if err != nil {
		return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批链无法按目录的组织架构排出：" + err.Error()}
	}
	if len(steps) == 0 {
		if chain.RequesterSubject == "" {
			return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批链没有任何环节"}
		}
		return chain, nil, true, nil
	}
	public = make([]map[string]any, 0, len(steps))
	for index, step := range steps {
		if strings.EqualFold(step.Approver, humanSubject) {
			return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "申请人不能审批自己的申请"}
		}
		authorized := s.chainStepAuthorized(route, step, step.Approver, now)
		if !authorized && !(index == 0 && chosenFirst != "") {
			return pendingChain{}, nil, false, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized,
				Message: fmt.Sprintf("目录组织架构中的审批人 %s 不具备该环节（%s）的审批权限", step.Approver, step.Role)}
		}
		entry := publicApprovalStep(step, "pending")
		entry["approver_authorized"] = authorized
		public = append(public, entry)
	}
	return chain, public, false, nil
}

// everyProductDeclares reports whether every product declares scope among
// its mandatory scopes.
func everyProductDeclares(products []catalog.Product, scope string) bool {
	if len(products) == 0 {
		return false
	}
	for _, product := range products {
		if !contains(product.Scopes, scope) {
			return false
		}
	}
	return true
}

// stewardDepartments lists the steward departments when every product
// declares one, nil otherwise (a mixed task is routed by department).
func stewardDepartments(products []catalog.Product) []string {
	var stewards []string
	for _, product := range products {
		if product.StewardDepartment == "" {
			return nil
		}
		if !contains(stewards, product.StewardDepartment) {
			stewards = append(stewards, product.StewardDepartment)
		}
	}
	return sortedValues(stewards)
}

func sortedValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// publicApprovalStep is a chain step as the requester sees it.
func publicApprovalStep(step catalog.ApprovalStep, status string) map[string]any {
	return map[string]any{"role": step.Role, "role_label": stepLabel(step),
		"departments": append([]string(nil), step.Departments...), "approver": step.Approver, "status": status}
}

// approvalStepLabels names the director steps and the single-layer form's
// leader step; an escalation layer carries its own label on the step.
var approvalStepLabels = map[string]string{
	catalog.ApprovalStepRequesterDirector:  "申请人所在部门审批人",
	catalog.ApprovalStepDepartmentDirector: "被申请部门审批人",
	catalog.ApprovalStepLeader:             "分管领导",
}

// stepLabel is how a step is shown to people: the layer's label, else the
// built-in label of its role, else the role itself.
func stepLabel(step catalog.ApprovalStep) string {
	if step.Label != "" {
		return step.Label
	}
	if label, ok := approvalStepLabels[step.Role]; ok {
		return label
	}
	return step.Role
}

// publicChainProgress lists a chain task's approvals and the steps its
// current scope still needs.
func publicChainProgress(progress chainProgress) []map[string]any {
	out := make([]map[string]any, 0, len(progress.Steps))
	for _, step := range progress.Steps {
		status := "pending"
		if progress.approved(step.Approver) {
			status = "approved"
		} else if progress.Next != nil && step.Approver == progress.Next.Approver {
			status = "current"
		}
		out = append(out, publicApprovalStep(step, status))
	}
	return out
}
