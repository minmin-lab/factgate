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
func (s *Service) authorizeApprover(policy catalog.TaskPolicy, humanSubject string, scope map[string]any, now time.Time) (*eligibilityVerdict, error) {
	approver := strings.TrimSpace(policy.ApprovalRoute.Approver)
	if approver == "" {
		return nil, nil
	}
	if strings.EqualFold(approver, strings.TrimSpace(humanSubject)) {
		return nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批人不能是申请人本人"}
	}
	if !policy.ApprovalRoute.VerifyApprover {
		return nil, nil
	}
	if s.principalAttributes == nil || !s.principalAttributes.Known(approver) {
		return nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized, Message: "审批人不在主体属性快照中，无法核验其审批权限"}
	}
	verdict, err := s.compileEligibility(policy.Products, approver, scope, now)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeApproverNotAuthorized,
			Message: "审批人 " + approver + " 无权审批此任务：其资格包络不覆盖所申请的范围（例如密级）"}
	}
	return verdict, nil
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
