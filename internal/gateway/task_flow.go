package gateway

import (
	"encoding/json"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

// Task-flow hints (KB1-LOOP-137). A new task waits first for its human
// requester to submit the OA draft -- an agent cannot, because the submitted
// callback admits the human subject only (callback.go) -- and then for the
// approver. get_task_status, wait_for_approval and request_data_task say which,
// and what to do next, so an agent does not wait on a step only a person can
// take.

const (
	// waitForApprovalMaxSeconds bounds one wait_for_approval call; a longer
	// wait is a loop of calls. waitForApprovalDefaultSeconds applies when the
	// caller gives no timeout.
	waitForApprovalMaxSeconds     = 45
	waitForApprovalDefaultSeconds = 30
)

// taskFlow says what a task waits for and the next step. It reads the
// approver and the human requester from the task's own request context.
func (s *Service) taskFlow(task control.Task) map[string]any {
	var pending struct {
		Approver string `json:"approver"`
		Manifest struct {
			HumanSubject string `json:"human_subject"`
		} `json:"authorization_manifest"`
	}
	if task.State == control.TaskAwaitingSubmission || task.State == control.TaskAwaitingApproval {
		_ = json.Unmarshal(task.RequestContext, &pending)
	}
	flow := map[string]any{}
	switch task.State {
	case control.TaskAwaitingSubmission:
		flow["waiting_for"], flow["next_action"] = "requester_submission", "human_submit_in_oa"
		flow["next_action_hint"] = "需要申请人" + quotedOrEmpty(pending.Manifest.HumanSubject) + "本人登录 OA 打开草稿并点“提交申请”；agent 不能代为提交。提交后任务进入 AWAITING_APPROVAL，等待审批人" + quotedOrEmpty(pending.Approver) + "。不再需要的任务可用 revoke_task 作废"
	case control.TaskAwaitingApproval:
		flow["waiting_for"], flow["next_action"] = "approver_decision", "await_approver_decision"
		if pending.Approver != "" {
			flow["approver"] = pending.Approver
		}
		flow["next_action_hint"] = "等待审批人" + quotedOrEmpty(pending.Approver) + "在 OA 审批；可再次调用 wait_for_approval（每次至多 45 秒）继续等待，不再需要时用 revoke_task 作废"
	case control.TaskActive:
		flow["next_action"] = "use_task"
		flow["next_action_hint"] = "任务已批准，可在期限内查询；用 get_task_context 查看批准的产品、列、范围与 knowledge_readiness"
	case control.TaskArchived:
		flow["next_action"] = "none_terminal"
		flow["next_action_hint"] = terminalAdvice[control.TerminalReason(task.TerminalReason)]
	}
	if (task.State == control.TaskAwaitingSubmission || task.State == control.TaskAwaitingApproval) &&
		s.oaPublicBaseURL != "" && task.ApprovalRef != "" {
		flow["oa_url"] = approval.DraftPageURL(s.oaPublicBaseURL, task.ApprovalRef)
	}
	return flow
}

var terminalAdvice = map[control.TerminalReason]string{
	control.TerminalCompleted:             "任务已完成归档",
	control.TerminalRevoked:               "任务已撤销",
	control.TerminalExpired:               "任务已过期（期限从批准时起算，不能延长）；需要继续时重新申请",
	control.TerminalRejected:              "审批人已驳回；需要时修改申请后重新 request_data_task",
	control.TerminalBudgetExhausted:       "任务预算已用完；需要时重新申请",
	control.TerminalApproverNotAuthorized: "实际审批人对所批范围无权，TaskGate 未签发授权；请改选有权的审批人（见 list_data_products 的 approval_routes）后重新申请",
	control.TerminalFailed:                "任务失败归档",
}

func quotedOrEmpty(name string) string {
	if name == "" {
		return ""
	}
	return " " + name + " "
}

// mergeTaskFlow adds the flow hints to a task response without overwriting
// what the response already says (request_data_task's oa_url comes from the
// OA itself).
func mergeTaskFlow(result map[string]any, flow map[string]any) {
	for key, value := range flow {
		if _, present := result[key]; !present {
			result[key] = value
		}
	}
}
