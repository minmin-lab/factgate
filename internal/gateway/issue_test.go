package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Issued authorizations (KB1-LOOP-147; the delegated-authorization demo).
// alice is finance's administrator (the chain's finance approver); staff is a
// finance employee with their own MCP principal.

const issueChainRouteYAML = `
requester_attribute: department
department_scope: kb_department
directors: {engineering: engineering-admin, finance: alice, hr: hr-admin, sales: sales-admin}
director_authority: {attribute: department, role: {attribute: job_family, any_of: [dept_admin]}}
escalations:
  - role: director
    label: 部门主任
    scope: classification
    values: [confidential, leadership]
    leaders: {engineering: engineering-director, finance: finance-director, hr: hr-director, sales: sales-director}
    leader_authority: {attribute: department, role: {attribute: job_family, any_of: [director]}}
`

const issueSubjects = `  - subject: alice
    attributes:
      department: [finance]
      job_family: [dept_admin, finance_controller]
      clearance: [public, internal, confidential, leadership]
  - subject: staff
    attributes:
      department: [finance]
      job_family: [staff]
      clearance: [public, internal, confidential]
  - subject: finance-director
    attributes: {department: [finance], job_family: [director]}
  - subject: sales-director
    attributes: {department: [sales], job_family: [director]}
  - subject: engineering-director
    attributes: {department: [engineering], job_family: [director]}
  - subject: hr-director
    attributes: {department: [hr], job_family: [director]}
  - subject: sales-admin
    attributes: {department: [sales], job_family: [dept_admin]}
  - subject: engineering-admin
    attributes: {department: [engineering], job_family: [dept_admin]}
  - subject: hr-admin
    attributes: {department: [hr], job_family: [dept_admin]}
  - subject: sales-clerk
    attributes:
      department: [sales]
      job_family: [staff, finance_controller]
      clearance: [public, internal]
  - subject: sales-staff
    attributes:
      department: [sales]
      job_family: [staff]
      clearance: [public, internal]
`

type issueHarness struct {
	*gatewayHarness
	t     *testing.T
	staff mcp.Principal
}

func newIssueHarness(t *testing.T, delegated bool) *issueHarness {
	t.Helper()
	rewrite := chainCatalogWith(t, issueChainRouteYAML)
	if delegated {
		rewrite = delegatedCatalog(rewrite)
	}
	base := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: rewrite, principalAttributes: approverSnapshot(t, issueSubjects)})
	staff := mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"}
	if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: staff.ID, Subject: staff.Subject, Role: staff.Role, CreatedAt: base.clock.value}); err != nil {
		t.Fatal(err)
	}
	return &issueHarness{gatewayHarness: base, t: t, staff: staff}
}

func issueArgs(forPrincipal, profile string, departments ...string) map[string]any {
	args := kbTaskRequest("finance", []string{"internal"})
	values := make([]any, 0, len(departments))
	for _, department := range departments {
		values = append(values, department)
	}
	args["scopes"].(map[string]any)["kb_department"] = values
	args["objective"] = "为财务部员工开通一年的制度与合同查阅"
	args["for_principal_id"], args["budget_profile"] = forPrincipal, profile
	delete(args, "approver")
	return args
}

func (h *issueHarness) callback(task control.Task, draft approval.DraftRequest, status, actor string) int {
	h.t.Helper()
	if status == "submitted" {
		event := oaCallbackEvent{EventID: "submit-" + task.ID + "-" + actor, TaskID: task.ID, DraftID: task.ApprovalRef, Status: "submitted",
			Actor: actor, OccurredAt: h.clock.value, CatalogVersion: h.catalog.CatalogVersion,
			CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest}
		return sendGatewayCallback(h.t, h.gatewayHarness, event, "").Code
	}
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, h.clock.value)
	if err != nil {
		h.t.Fatal(err)
	}
	event := signedFinalCallback(h.t, h.gatewayHarness, task, draft, status, actor, core)
	event.EventID += "-" + actor
	event.ApprovalReceipt.ReceiptID += "-" + actor
	signed, err := approval.DemoReceiptSigner([]byte(h.secret)).SignReceipt(*event.ApprovalReceipt)
	if err != nil {
		h.t.Fatal(err)
	}
	event.ApprovalReceipt = &signed
	return sendGatewayCallback(h.t, h.gatewayHarness, event, "").Code
}

func (h *issueHarness) task(id string) control.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func TestAdministratorIssuesAnAuthorizationForAnEmployeeOfTheirDepartment(t *testing.T) {
	h := newIssueHarness(t, true)
	listed := false
	for _, tool := range h.service.ListTools(h.alice) {
		listed = listed || tool.Name == issueTool.Name
	}
	if !listed {
		t.Fatal("the issue tool is not listed with the Catalog block")
	}
	result := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, "final-v5-concurrency-v1", "finance"))
	if result["approver"] != "alice" || result["issued_by"] != "alice" || result["human_subject"] != "staff" ||
		result["budget_profile"] != "final-v5-concurrency-v1" || result["budget_source"] != "issued_profile" {
		t.Fatalf("issued task %v", result)
	}
	if ttl := result["budget"].(map[string]any)["task_ttl_seconds"]; ttl != int64(7200) {
		t.Fatalf("issued task TTL %v, want the chosen profile's 2h", ttl)
	}
	root := h.task(result["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	if root.PrincipalID != h.staff.ID || draft.Manifest.HumanSubject != "staff" || draft.Manifest.AgentID != h.staff.ID ||
		draft.IssuedBy != "alice" || draft.Approver != "alice" {
		t.Fatalf("issued task principal %q, draft human %q agent %q issued_by %q approver %q",
			root.PrincipalID, draft.Manifest.HumanSubject, draft.Manifest.AgentID, draft.IssuedBy, draft.Approver)
	}
	events, err := h.store.ListAuditEvents(context.Background(), control.AuditFilter{TaskID: root.ID, EventType: "TASK_ISSUED_FOR"})
	if err != nil || len(events) != 1 || events[0].Actor != h.alice.ID {
		t.Fatalf("issuance audit events %+v %v", events, err)
	}
	var issued map[string]any
	if err := json.Unmarshal(events[0].Payload, &issued); err != nil || issued["issuer_subject"] != "alice" || issued["human_subject"] != "staff" {
		t.Fatalf("issuance audit payload %s %v", events[0].Payload, err)
	}
	// The issuer submits and approves in one OA login; nobody else may submit.
	if code := h.callback(root, draft, "submitted", "sales-admin"); code != http.StatusForbidden {
		t.Fatalf("a stranger submitting = %d, want 403", code)
	}
	if code := h.callback(root, draft, "submitted", "alice"); code != http.StatusOK {
		t.Fatalf("the issuer submitting = %d", code)
	}
	if code := h.callback(root, draft, "approved", "alice"); code != http.StatusOK || h.task(root.ID).State != control.TaskActive {
		t.Fatalf("the issuer approving = %d, state %s", code, h.task(root.ID).State)
	}
	// Both the employee and the issuer see the task; only the employee uses it.
	for _, principal := range []mcp.Principal{h.staff, h.alice} {
		if status := mustCallGatewayTool(t, h.service, principal, "get_task_status", map[string]any{"task_id": root.ID}); status["state"] != control.TaskActive {
			t.Fatalf("%s sees %v", principal.Subject, status["state"])
		}
	}
	if _, err := callGatewayTool(h.service, h.alice, "get_task_context", map[string]any{"task_id": root.ID}); err == nil {
		t.Fatal("the issuer could read the employee's task context")
	}
	// The employee's daily query is a self-confirmed child of the issued task.
	childArgs := kbTaskRequest("finance", []string{"internal"})
	childArgs["parent_task_id"] = root.ID
	childResult := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", childArgs)
	child := h.task(childResult["task_id"].(string))
	childDraft := h.approval.requests[len(h.approval.requests)-1]
	if childResult["self_confirm"] != true || childDraft.Approver != "staff" || child.RootTaskID != root.ID {
		t.Fatalf("child %v draft approver %q root %q", childResult, childDraft.Approver, child.RootTaskID)
	}
	if code := h.callback(child, childDraft, "submitted", "staff"); code != http.StatusOK {
		t.Fatalf("the employee submitting the child = %d", code)
	}
	if code := h.callback(child, childDraft, "approved", "staff"); code != http.StatusOK || h.task(child.ID).State != control.TaskActive {
		t.Fatalf("the employee confirming the child = %d, state %s", code, h.task(child.ID).State)
	}
	// The issuer revokes the authorization; the employee's child is blocked
	// by the family check on its next use.
	revoked := mustCallGatewayTool(t, h.service, h.alice, "revoke_task", map[string]any{"task_id": root.ID, "reason": "测试撤销"})
	if revoked["state"] != control.TaskArchived || h.task(root.ID).State != control.TaskArchived {
		t.Fatalf("revocation %v", revoked)
	}
	if cascaded, _ := revoked["cascaded_task_ids"].([]string); len(cascaded) != 1 || cascaded[0] != child.ID || h.task(child.ID).State != control.TaskArchived {
		t.Fatalf("the employee's child after the issuer's revocation: cascaded %v, state %s", revoked["cascaded_task_ids"], h.task(child.ID).State)
	}
	_, err = callGatewayTool(h.service, h.staff, "get_task_context", map[string]any{"task_id": child.ID})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
}

func TestIssuedAuthorizationsAreConfinedToTheIssuersDepartment(t *testing.T) {
	h := newIssueHarness(t, true)
	before := len(h.approval.requests)
	_, err := callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, "final-v5-concurrency-v1", "finance", "sales"))
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	_, err = callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, "no-such-profile", "finance"))
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	_, err = callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(h.alice.ID, "final-v5-concurrency-v1", "finance"))
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	// staff holds no issuer role.
	_, err = callGatewayTool(h.service, h.staff, issueTool.Name, issueArgs(h.alice.ID, "final-v5-concurrency-v1", "finance"))
	requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	if len(h.approval.requests) != before {
		t.Fatalf("refused issuances produced %d drafts", len(h.approval.requests)-before)
	}
}

func TestIssuingIsAbsentWithoutTheCatalogBlock(t *testing.T) {
	h := newIssueHarness(t, false)
	for _, tool := range h.service.ListTools(h.alice) {
		if tool.Name == issueTool.Name {
			t.Fatal("the issue tool is listed without the Catalog block")
		}
	}
	_, err := callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, "final-v5-concurrency-v1", "finance"))
	requireToolCode(t, err, apierr.CodeForbidden)
}

// An authorization is issued by the data's department (KB1-LOOP-148): the
// employee may belong to another department, the chain starts at the
// issuer's, and the employee's own envelope must cover the scope.
func TestAdministratorIssuesTheirDepartmentsDataToAnotherDepartmentsEmployee(t *testing.T) {
	h := newIssueHarness(t, true)
	employees := map[string]mcp.Principal{}
	for _, subject := range []string{"sales-clerk", "sales-staff"} {
		principal := mcp.Principal{ID: "principal-" + subject, Subject: subject, Role: "query"}
		if err := h.store.CreatePrincipal(context.Background(), control.Principal{ID: principal.ID, Subject: subject, Role: "query", CreatedAt: h.clock.value}); err != nil {
			t.Fatal(err)
		}
		employees[subject] = principal
	}
	result := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, issueArgs(employees["sales-clerk"].ID, "final-v5-concurrency-v1", "finance"))
	root := h.task(result["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	if result["approver"] != "alice" || result["human_subject"] != "sales-clerk" || draft.Approver != "alice" || draft.IssuedBy != "alice" ||
		root.PrincipalID != employees["sales-clerk"].ID {
		t.Fatalf("cross-department issuance %v draft approver %q issued_by %q", result, draft.Approver, draft.IssuedBy)
	}
	if code := h.callback(root, draft, "submitted", "alice"); code != http.StatusOK {
		t.Fatalf("the issuer submitting = %d", code)
	}
	if code := h.callback(root, draft, "approved", "alice"); code != http.StatusOK || h.task(root.ID).State != control.TaskActive {
		t.Fatalf("the issuer approving = %d, state %s", code, h.task(root.ID).State)
	}
	// The scope still has to be the issuer's department.
	_, err := callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(employees["sales-clerk"].ID, "final-v5-concurrency-v1", "sales"))
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	// An employee whose envelope does not reach finance cannot be given it.
	_, err = callGatewayTool(h.service, h.alice, issueTool.Name, issueArgs(employees["sales-staff"].ID, "final-v5-concurrency-v1", "finance"))
	requireToolCode(t, err, apierr.CodeNotEligible)
}
