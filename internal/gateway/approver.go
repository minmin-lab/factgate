package gateway

import (
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Approver authority (docs/catalog-guide.md, approval routes). Two gates
// govern a task: the requester's eligibility envelope decides what may be
// requested (compileEligibility), and the resolved approver must be allowed to
// approve what is requested. The second gate is checked here, before any OA
// draft exists:
//
//   - the approver is never the human subject of the task (separation of
//     duties), whatever the route says;
//   - a route with verify_approver requires the approver to be a subject of the
//     attested attribute snapshot and eligible, under the same envelopes as a
//     requester, for everything the task requests. A leadership grant can then
//     only be approved by someone who could request leadership data.
//
// The approver's verdict is kept in the task's private record beside the
// requester's.
func (s *Service) authorizeApprover(policy catalog.TaskPolicy, humanSubject string, scope map[string]any, now time.Time, chosen bool) (*eligibilityVerdict, *bool, error) {
	approver := strings.TrimSpace(policy.ApprovalRoute.Approver)
	if approver == "" {
		return nil, nil, nil
	}
	if strings.EqualFold(approver, strings.TrimSpace(humanSubject)) {
		return nil, nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批人不能是申请人本人"}
	}
	if !policy.ApprovalRoute.VerifyApprover && !chosen {
		return nil, nil, nil
	}
	verdict, err := s.approverEligibility(policy.Products, approver, scope, now)
	authorized := err == nil
	if chosen {
		// A chosen approver is verified again when the approval arrives; the
		// request goes ahead, and the result says whether it can succeed.
		return verdict, &authorized, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return verdict, &authorized, nil
}

// approverEligibility compiles the requester envelopes for an approver: the
// approver must be a subject of the attested snapshot and eligible for every
// requested product under the task's scope.
func (s *Service) approverEligibility(products []catalog.Product, approver string, scope map[string]any, now time.Time) (*eligibilityVerdict, error) {
	if s.principalAttributes == nil || !s.principalAttributes.Known(approver) {
		return nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批人不在主体属性快照中，无法核验其审批权限"}
	}
	verdict, err := s.compileEligibility(products, approver, scope, now)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized,
			Message: "审批人 " + approver + " 无权审批此任务：其资格包络不覆盖所申请的范围（例如密级）"}
	}
	return verdict, nil
}

// approvalAuthorized is the check the approval callback runs before issuing a
// grant: the actual approver, against the approved (possibly narrowed) scope.
// A narrowing that removes what the approver may not approve makes the
// approval good.
func (s *Service) approvalAuthorized(productNames []string, approver string, approvedScope map[string]any, now time.Time) bool {
	if s.catalog == nil {
		return false
	}
	products, _, err := s.catalog.ResolveProducts(productNames)
	if err != nil {
		return false
	}
	_, err = s.approverEligibility(products, strings.TrimSpace(approver), approvedScope, now)
	return err == nil
}

// scopeValueSets reads a normalized mandatory scope as value sets, the form a
// route's scope condition is matched against; date ranges carry no values.
func scopeValueSets(scope map[string]any) map[string][]string {
	sets := make(map[string][]string, len(scope))
	for name, raw := range scope {
		if values, ok := scopeStringValues(raw); ok {
			sets[name] = values
		}
	}
	return sets
}
