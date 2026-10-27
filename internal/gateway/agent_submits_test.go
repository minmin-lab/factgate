package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Agent submission (KB1-LOOP-149; the Catalog's delegated_authorization
// agent_submits, a demo feature): a task is created AWAITING_APPROVAL, the OA
// is asked for a draft that is already submitted, a TASK_SUBMITTED_BY_AGENT
// audit event records who submitted it for whom, and a stray submitted
// callback conflicts. Off, nothing changes (TestAgentSubmitsOffKeepsTheSubmissionStep).

// withAgentSubmits adds agent_submits: true to a rewritten Catalog's
// delegated_authorization block.
func withAgentSubmits(t *testing.T, rewrite func(string) string) func(string) string {
	t.Helper()
	return func(base string) string {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(rewrite(base)), &document); err != nil {
			t.Fatal(err)
		}
		block := yamlField(document.Content[0], "delegated_authorization")
		if block == nil || block.Kind != yaml.MappingNode {
			t.Fatal("the rewritten Catalog has no delegated_authorization block")
		}
		block.Content = append(block.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "agent_submits"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

// newAgentSubmitsIssueHarness is newIssueHarness with agent_submits on; the
// fake OA answers a submitted draft as pending, as the demo OA does.
func newAgentSubmitsIssueHarness(t *testing.T) *issueHarness {
	t.Helper()
	base := newGatewayHarnessWithOptions(t, harnessOptions{
		rewriteCatalog:      withAgentSubmits(t, delegatedCatalog(chainCatalogWith(t, issueChainRouteYAML))),
		principalAttributes: approverSnapshot(t, issueSubjects), oaPublicBaseURL: "https://oa.test/"})
	if base.catalog.DelegatedAuthorization == nil || !base.catalog.DelegatedAuthorization.AgentSubmits {
		t.Fatal("the harness Catalog does not switch agent_submits on")
	}
	base.approval.response.State = "pending"
	staff := mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"}
	if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: staff.ID, Subject: staff.Subject, Role: staff.Role, CreatedAt: base.clock.value}); err != nil {
		t.Fatal(err)
	}
	return &issueHarness{gatewayHarness: base, t: t, staff: staff}
}

// agentSubmission returns the task's TASK_SUBMITTED_BY_AGENT event and payload.
func agentSubmission(t *testing.T, store *control.Store, taskID string) (control.AuditEvent, map[string]any) {
	t.Helper()
	events, err := store.ListAuditEvents(context.Background(), control.AuditFilter{TaskID: taskID, EventType: "TASK_SUBMITTED_BY_AGENT"})
	if err != nil || len(events) != 1 {
		t.Fatalf("agent submission audit events %+v %v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return events[0], payload
}

// requireApprovalFlow checks a response's state and flow against the
// AWAITING_APPROVAL branch of task_flow.go, and that its hint says the agent
// submitted and asks nobody to submit.
func requireApprovalFlow(t *testing.T, label string, result map[string]any, waitingFor, nextAction, approver string) {
	t.Helper()
	hint, _ := result["next_action_hint"].(string)
	if result["state"] != control.TaskAwaitingApproval || result["waiting_for"] != waitingFor || result["next_action"] != nextAction ||
		result["approver"] != approver || !strings.Contains(hint, "agent 已代为提交") || strings.Contains(hint, "提交申请") {
		t.Fatalf("%s: state %v waiting_for %v next_action %v approver %v hint %q", label, result["state"], result["waiting_for"],
			result["next_action"], result["approver"], hint)
	}
}

func TestAgentSubmitsRequestDataTaskAwaitsApprovalFromCreation(t *testing.T) {
	h := newAgentSubmitsIssueHarness(t)
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	requireApprovalFlow(t, "request_data_task", created, "approver_decision", "await_approver_decision", "alice")
	if created["agent_submitted"] != true || created["oa_url"] != "http://oa.test/drafts/draft-1" {
		t.Fatalf("created %v", created)
	}
	task := h.task(created["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	if task.State != control.TaskAwaitingApproval || !draft.Submitted || draft.Approver != "alice" {
		t.Fatalf("task state %s, draft submitted %v approver %q", task.State, draft.Submitted, draft.Approver)
	}
	pending, err := decodePersistedPending(task)
	if err != nil || !pending.AgentSubmitted {
		t.Fatalf("pending context agent_submitted %v %v", pending.AgentSubmitted, err)
	}
	event, payload := agentSubmission(t, h.store, task.ID)
	if event.Actor != h.staff.ID || payload["human_subject"] != "staff" || payload["approver"] != "alice" || payload["agent_id"] != h.staff.ID ||
		payload["self_confirm"] != false || payload["member_confirm"] != false || payload["draft_id"] != task.ApprovalRef {
		t.Fatalf("agent submission event actor %q payload %v", event.Actor, payload)
	}
	status := mustCallGatewayTool(t, h.service, h.staff, "get_task_status", map[string]any{"task_id": task.ID})
	requireApprovalFlow(t, "get_task_status", status, "approver_decision", "await_approver_decision", "alice")
	if status["oa_url"] != "https://oa.test/tasks/draft-1" {
		t.Fatalf("status oa_url %v", status["oa_url"])
	}
	waited := mustCallGatewayTool(t, h.service, h.staff, "wait_for_approval", map[string]any{"task_id": task.ID, "timeout_seconds": 1})
	if waited["timed_out"] != true || waited["next_action"] != "await_approver_decision" {
		t.Fatalf("wait %v", waited)
	}

	// A stray submitted callback (the OA sends none for such a draft) finds
	// the task past submission: a conflict, nothing changes.
	if code := h.callback(task, draft, "submitted", "staff"); code != http.StatusConflict {
		t.Fatalf("a submitted callback for an agent-submitted task = %d, want 409", code)
	}
	if h.task(task.ID).State != control.TaskAwaitingApproval {
		t.Fatalf("state after the refused submission %s", h.task(task.ID).State)
	}
	if code := h.callback(task, draft, "approved", "alice"); code != http.StatusOK || h.task(task.ID).State != control.TaskActive {
		t.Fatalf("approval = %d, state %s", code, h.task(task.ID).State)
	}
	if events, err := h.store.ListApprovalEvents(context.Background(), task.ID); err != nil || len(events) != 1 || events[0].Decision != "approved" {
		t.Fatalf("approval events %+v %v (no submitted event is recorded)", events, err)
	}

	// The discovery surface says so.
	listed := mustCallGatewayTool(t, h.service, h.staff, "list_data_products", map[string]any{})
	if public, _ := listed["delegated_authorization"].(map[string]any); public["agent_submits"] != true {
		t.Fatalf("list_data_products delegated_authorization %v", listed["delegated_authorization"])
	}
	described := map[string]string{}
	for _, tool := range h.service.ListTools(h.staff) {
		described[tool.Name] = tool.Description
	}
	for _, name := range []string{"request_data_task", "request_download_task", "issue_data_authorization", "get_task_status", "wait_for_approval", "revoke_task"} {
		if !strings.Contains(described[name], "本目录启用了 agent_submits") {
			t.Fatalf("%s description does not say agent_submits is on: %q", name, described[name])
		}
	}
	if strings.Contains(queryTools[0].Description+queryTools[3].Description, "agent_submits") {
		t.Fatal("the switch leaked into the static tool descriptions")
	}
}

func TestAgentSubmitsIssuedAuthorizationAndSelfConfirmedChild(t *testing.T) {
	h := newAgentSubmitsIssueHarness(t)
	issued := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, "final-v5-concurrency-v1", "finance"))
	requireApprovalFlow(t, "issue_data_authorization", issued, "approver_decision", "await_approver_decision", "alice")
	root := h.task(issued["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	if root.State != control.TaskAwaitingApproval || !draft.Submitted || draft.IssuedBy != "alice" || root.PrincipalID != h.staff.ID {
		t.Fatalf("issued task state %s, draft submitted %v issued_by %q, principal %q", root.State, draft.Submitted, draft.IssuedBy, root.PrincipalID)
	}
	event, payload := agentSubmission(t, h.store, root.ID)
	if event.Actor != h.alice.ID || payload["human_subject"] != "staff" || payload["issuer_subject"] != "alice" || payload["agent_id"] != h.staff.ID ||
		payload["approver"] != "alice" {
		t.Fatalf("issued submission event actor %q payload %v", event.Actor, payload)
	}
	if events, err := h.store.ListAuditEvents(context.Background(), control.AuditFilter{TaskID: root.ID, EventType: "TASK_ISSUED_FOR"}); err != nil || len(events) != 1 {
		t.Fatalf("the issuance audit event is gone: %+v %v", events, err)
	}
	if code := h.callback(root, draft, "submitted", "alice"); code != http.StatusConflict {
		t.Fatalf("the issuer submitting an agent-submitted task = %d, want 409", code)
	}
	if code := h.callback(root, draft, "approved", "alice"); code != http.StatusOK || h.task(root.ID).State != control.TaskActive {
		t.Fatalf("the issuer approving = %d, state %s", code, h.task(root.ID).State)
	}

	// The employee's daily query: a self-confirmed child, awaiting the
	// employee's own confirmation from its creation.
	childArgs := kbTaskRequest("finance", []string{"internal"})
	childArgs["parent_task_id"] = root.ID
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", childArgs)
	requireApprovalFlow(t, "self-confirmed child", created, "requester_confirmation", "human_confirm_in_oa", "staff")
	child := h.task(created["task_id"].(string))
	childDraft := h.approval.requests[len(h.approval.requests)-1]
	if !childDraft.Submitted || childDraft.Approver != "staff" {
		t.Fatalf("child draft submitted %v approver %q", childDraft.Submitted, childDraft.Approver)
	}
	if _, payload := agentSubmission(t, h.store, child.ID); payload["self_confirm"] != true || payload["approver"] != "staff" {
		t.Fatalf("child submission payload %v", payload)
	}
	if code := h.callback(child, childDraft, "approved", "staff"); code != http.StatusOK || h.task(child.ID).State != control.TaskActive {
		t.Fatalf("the employee confirming = %d, state %s", code, h.task(child.ID).State)
	}
}

func TestAgentSubmitsMemberConfirmedChild(t *testing.T) {
	base := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: withAgentSubmits(t, gradedCatalog(t, true)),
		principalAttributes: approverSnapshot(t, gradedSubjects)})
	base.approval.response.State = "pending"
	h := &gradedHarness{gatewayHarness: base, t: t, principals: map[string]mcp.Principal{}}
	for _, subject := range []string{"hr-admin", "staff"} {
		principal := mcp.Principal{ID: "principal-" + subject, Subject: subject, Role: "query"}
		if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: principal.ID, Subject: subject,
			Role: "query", CreatedAt: base.clock.value}); err != nil {
			t.Fatal(err)
		}
		h.principals[subject] = principal
	}
	result, err := h.request("hr-admin", attendance([]string{"finance", "hr"}, []string{"finance-a", "hr-a"}, []string{"finance-a-head", "staff"}))
	requireSelfService(t, result, err, "hr-admin")
	requireApprovalFlow(t, "self-service root", result, "requester_confirmation", "human_confirm_in_oa", "hr-admin")
	root, rootDraft := h.created(result)
	if code, _ := h.decide(root, rootDraft, nil, "hr-admin"); code != http.StatusOK || h.state(root.ID).State != control.TaskActive {
		t.Fatalf("root confirmation without a submission = %d, state %s", code, h.state(root.ID).State)
	}
	args := attendance([]string{"finance"}, []string{"finance-a"}, []string{"staff"})
	args["parent_task_id"], args["delegate_principal_id"] = root.ID, h.principals["staff"].ID
	result, err = h.request("hr-admin", args)
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalFlow(t, "member child", result, "member_confirmation", "human_confirm_in_oa", "staff")
	task, draft := h.created(result)
	if !draft.Submitted || !draft.MemberConfirm || task.State != control.TaskAwaitingApproval {
		t.Fatalf("member child draft submitted %v member %v state %s", draft.Submitted, draft.MemberConfirm, task.State)
	}
	if event, payload := agentSubmission(t, h.store, task.ID); event.Actor != h.principals["hr-admin"].ID || payload["member_confirm"] != true ||
		payload["human_subject"] != "hr-admin" || payload["agent_id"] != h.principals["staff"].ID {
		t.Fatalf("member child submission actor %q payload %v", event.Actor, payload)
	}
	if code := h.submit(task, draft, "staff"); code != http.StatusConflict {
		t.Fatalf("the member submitting = %d, want 409", code)
	}
	if code, _ := h.decide(task, draft, nil, "staff"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("the member confirming = %d, state %s", code, h.state(task.ID).State)
	}
}

// An OA that took a submitted draft as unsubmitted would leave the task
// waiting for a submission nobody can make: TaskGate refuses to create it.
func TestAgentSubmitsRefusesAnOAThatDidNotTakeTheSubmission(t *testing.T) {
	h := newAgentSubmitsIssueHarness(t)
	h.approval.response.State = "draft"
	_, err := callGatewayTool(h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	requireToolCode(t, err, apierr.CodeApprovalUnavailable)
	if len(h.approval.requests) != 1 || !h.approval.requests[0].Submitted {
		t.Fatalf("draft requests %+v", h.approval.requests)
	}
	tasks, err := h.store.ListTasks(context.Background(), control.TaskFilter{PrincipalID: h.staff.ID, Limit: 10})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("a task was created: %+v %v", tasks, err)
	}
}

// With the block but without agent_submits, the submission step is as before.
func TestAgentSubmitsOffKeepsTheSubmissionStep(t *testing.T) {
	h := newIssueHarness(t, true)
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	if created["state"] != control.TaskAwaitingSubmission || created["next_action"] != "human_submit_in_oa" || created["agent_submitted"] != nil {
		t.Fatalf("created %v", created)
	}
	task := h.task(created["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	if task.State != control.TaskAwaitingSubmission || draft.Submitted || strings.Contains(string(task.RequestContext), "agent_submitted") {
		t.Fatalf("task state %s, draft submitted %v, context %s", task.State, draft.Submitted, task.RequestContext)
	}
	if events, err := h.store.ListAuditEvents(context.Background(), control.AuditFilter{TaskID: task.ID, EventType: "TASK_SUBMITTED_BY_AGENT"}); err != nil || len(events) != 0 {
		t.Fatalf("agent submission events without the switch: %+v %v", events, err)
	}
	if code := h.callback(task, draft, "submitted", "staff"); code != http.StatusOK || h.task(task.ID).State != control.TaskAwaitingApproval {
		t.Fatalf("the requester's submission = %d, state %s", code, h.task(task.ID).State)
	}
	if public, _ := mustCallGatewayTool(t, h.service, h.staff, "list_data_products", map[string]any{})["delegated_authorization"].(map[string]any); public["agent_submits"] != nil {
		t.Fatalf("list_data_products says agent_submits without the switch: %v", public)
	}
	for _, tool := range h.service.ListTools(h.staff) {
		if strings.Contains(tool.Description, "agent_submits") {
			t.Fatalf("%s description mentions agent_submits without the switch", tool.Name)
		}
	}
}
