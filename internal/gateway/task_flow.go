package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

// Task-flow hints (KB1-LOOP-137). A new task waits first for its human
// requester to submit the OA draft -- an agent cannot, because the submitted
// callback admits the human subject only (callback.go) -- and then for the
// approver. get_task_status, wait_for_approval and request_data_task say which,
// and what to do next, so an agent does not wait on a step only a person can
// take. Under the Catalog's delegated_authorization agent_submits
// (KB1-LOOP-149) the agent submits at creation instead: the task starts
// AWAITING_APPROVAL and its hint says a person only approves.

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
		Approver      string `json:"approver"`
		SelfConfirm   bool   `json:"self_confirm"`
		MemberConfirm bool   `json:"member_confirm"`
		// AgentSubmitted: the agent submitted the task at creation.
		AgentSubmitted bool `json:"agent_submitted"`
		Manifest       struct {
			HumanSubject string `json:"human_subject"`
			ParentTaskID string `json:"parent_task_id"`
		} `json:"authorization_manifest"`
	}
	if task.State == control.TaskAwaitingSubmission || task.State == control.TaskAwaitingApproval {
		_ = json.Unmarshal(task.RequestContext, &pending)
	}
	flow := map[string]any{}
	switch task.State {
	case control.TaskAwaitingSubmission:
		flow["waiting_for"], flow["next_action"] = "requester_submission", "human_submit_in_oa"
		switch {
		case pending.MemberConfirm:
			flow["waiting_for"], flow["approver"] = "member_submission", pending.Approver
			flow["next_action_hint"] = "这是委托给项目成员的子任务，由成员" + quotedOrEmpty(pending.Approver) + "本人确认：成员登录 OA 打开草稿，点“提交申请”再点“批准”即可生效；agent 不能代为操作。不再需要的任务可用 revoke_task 作废"
		case pending.SelfConfirm && pending.Manifest.ParentTaskID == "":
			flow["next_action_hint"] = "该申请在申请人" + quotedOrEmpty(pending.Manifest.HumanSubject) + "本人职权内（或只含本人记录），由本人确认：登录 OA 打开草稿，点“提交申请”再点“批准”即可生效；agent 不能代为操作。不再需要的任务可用 revoke_task 作废"
		case pending.SelfConfirm:
			flow["next_action_hint"] = "这是项目内的子任务，由申请人" + quotedOrEmpty(pending.Manifest.HumanSubject) + "本人确认：登录 OA 打开草稿，点“提交申请”再点“批准”即可生效；agent 不能代为操作。不再需要的任务可用 revoke_task 作废"
		default:
			flow["next_action_hint"] = "需要申请人" + quotedOrEmpty(pending.Manifest.HumanSubject) + "本人登录 OA 打开草稿并点“提交申请”；agent 不能代为提交。提交后任务进入 AWAITING_APPROVAL，等待审批人" + quotedOrEmpty(pending.Approver) + "。不再需要的任务可用 revoke_task 作废"
		}
	case control.TaskAwaitingApproval:
		if pending.MemberConfirm {
			flow["waiting_for"], flow["next_action"] = "member_confirmation", "human_confirm_in_oa"
			flow["approver"] = pending.Approver
			flow["next_action_hint"] = "等待项目成员" + quotedOrEmpty(pending.Approver) + "本人在 OA 点“批准”确认；可再次调用 wait_for_approval（每次至多 45 秒）继续等待，不再需要时用 revoke_task 作废"
			if pending.AgentSubmitted {
				flow["next_action_hint"] = "agent 已代为提交。这是委托给项目成员的子任务，由成员" + quotedOrEmpty(pending.Approver) + "本人确认：成员登录 OA 打开草稿点“批准”即可生效；" + agentSubmittedWait
			}
			break
		}
		if pending.SelfConfirm {
			flow["waiting_for"], flow["next_action"] = "requester_confirmation", "human_confirm_in_oa"
			flow["approver"] = pending.Approver
			flow["next_action_hint"] = "等待申请人" + quotedOrEmpty(pending.Approver) + "本人在 OA 点“批准”确认；可再次调用 wait_for_approval（每次至多 45 秒）继续等待，不再需要时用 revoke_task 作废"
			if pending.AgentSubmitted {
				flow["next_action_hint"] = "agent 已代为提交。该申请由申请人" + quotedOrEmpty(pending.Approver) + "本人确认：本人登录 OA 打开草稿点“批准”即可生效；" + agentSubmittedWait
			}
			break
		}
		flow["waiting_for"], flow["next_action"] = "approver_decision", "await_approver_decision"
		if pending.Approver != "" {
			flow["approver"] = pending.Approver
		}
		flow["next_action_hint"] = "等待审批人" + quotedOrEmpty(pending.Approver) + "在 OA 审批；可再次调用 wait_for_approval（每次至多 45 秒）继续等待，不再需要时用 revoke_task 作废"
		if pending.AgentSubmitted {
			flow["next_action_hint"] = "agent 已代为提交。等待审批人" + quotedOrEmpty(pending.Approver) + "审批：审批人登录 OA 打开草稿点“批准”即可生效（也可缩小后批准或拒绝）；" + agentSubmittedWait
		}
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

// agentSubmittedWait ends the hint of a task the agent submitted.
const agentSubmittedWait = "申请人无需再提交。可再次调用 wait_for_approval（每次至多 45 秒）继续等待，不再需要时用 revoke_task 作废"

var terminalAdvice = map[control.TerminalReason]string{
	control.TerminalCompleted:             "任务已完成归档",
	control.TerminalRevoked:               "任务已撤销",
	control.TerminalExpired:               "任务已过期（批准后的期限或待审期限已到，不能延长）；需要继续时重新申请",
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

// taskFlowFor is taskFlow plus, for an approval-chain task, where its chain
// stands: the approver it waits for now and every step with its status.
func (s *Service) taskFlowFor(ctx context.Context, task control.Task) map[string]any {
	flow := s.taskFlow(task)
	if task.State != control.TaskAwaitingSubmission && task.State != control.TaskAwaitingApproval {
		return flow
	}
	persisted, err := decodePersistedPending(task)
	if err != nil || persisted.Chain == nil {
		return flow
	}
	progress, err := s.chainProgressOf(ctx, task, persisted)
	if err != nil {
		return flow
	}
	flow["approval_chain"] = publicChainProgress(progress)
	if task.State == control.TaskAwaitingApproval && progress.Next != nil {
		flow["approver"] = progress.Next.Approver
		flow["next_action_hint"] = fmt.Sprintf("审批链进行中：已批 %d 级，共 %d 级；等待 %s（%s）在 OA 审批。可再次调用 wait_for_approval 继续等待，不再需要时用 revoke_task 作废",
			len(progress.Approvals), len(progress.Steps), progress.Next.Approver, stepLabel(*progress.Next))
		if persisted.AgentSubmitted {
			flow["next_action_hint"] = "agent 已代为提交，申请人无需再提交。" + flow["next_action_hint"].(string)
		}
	}
	return flow
}
