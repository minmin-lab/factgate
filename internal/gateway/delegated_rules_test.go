package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Delegated-authorization rules (KB1-LOOP-148; the demo's
// delegated_authorization block, not a paper claim): authority levels,
// center heads, personal records, the data steward and member confirmation.
// The test Catalog has the five standard grades, the second demo round's
// three escalation layers, and a personal-record product (attendance) with
// center and employee_id scopes stewarded by hr. As the author decided, the
// director layer is level 0: directors rank with department administrators.

const gradedChainRouteYAML = `
requester_attribute: department
department_scope: kb_department
directors: {engineering: engineering-admin, finance: finance-admin, hr: hr-admin, sales: sales-admin}
director_authority: {attribute: authority_departments, role: {attribute: job_family, any_of: [dept_admin]}}
escalations:
  - role: director
    label: 部门主任
    level: 0
    scope: classification
    values: [confidential, secret, top_secret]
    leaders: {engineering: engineering-director, finance: finance-director, hr: hr-director, sales: sales-director}
    leader_authority: {attribute: authority_departments, role: {attribute: job_family, any_of: [director]}}
  - role: vice_president
    label: 分管领导
    scope: classification
    values: [confidential, top_secret]
    leaders: {engineering: vp-rd, finance: vp-admin, hr: vp-admin, sales: vp-business}
    leader_authority: {attribute: authority_departments, role: {attribute: job_family, any_of: [vice_president]}}
  - role: general_manager
    label: 总经理
    scope: classification
    values: [top_secret]
    leaders: {engineering: general-manager, finance: general-manager, hr: general-manager, sales: general-manager}
    leader_authority: {attribute: authority_departments, role: {attribute: job_family, any_of: [general_manager]}}
`

const attendanceFragmentYAML = `
scopes:
  - name: center
    type: enum
    description: center below a department
    allowed_values: [engineering-a, engineering-b, finance-a, finance-b, hr-a, hr-b, sales-a, sales-b]
  - name: employee_id
    type: enum
    description: owner of a personal record
    allowed_values: [finance-a-head, finance-admin, hr-admin, sales-clerk, staff, staff-b]
snapshot_publications:
  - name: hr-attendance-test-v1
    source: travel_demo
    source_namespace: hr.attendance
    snapshot: hr-attendance-test-v1
    ordinal_sidecar: taskgate_ordinal.hr_attendance_test_v1
    sidecar_digest: bbbb111111111111111111111111111111111111111111111111111111111111
    dictionary_digest: bbbb222222222222222222222222222222222222222222222222222222222222
    manifest_digest: bbbb333333333333333333333333333333333333333333333333333333333333
products:
  - name: attendance_records
    source: travel_demo
    reporting_view: reporting.hr_attendance_test_v1
    description: monthly attendance per employee
    sensitivity: high
    snapshot: hr-attendance-test-v1
    snapshot_publication: hr-attendance-test-v1
    entity_key: [period, employee_id]
    fact_namespace: hr.attendance
    stable_relation_role: attendance_records
    steward_department: hr
    scopes: [kb_department, classification, center, employee_id]
    fields:
      - {name: period, type: text, collation: en_US.utf8, collation_version: "2.36", description: period}
      - {name: kb_department, type: text, collation: en_US.utf8, collation_version: "2.36", description: kb_department}
      - {name: classification, type: text, collation: en_US.utf8, collation_version: "2.36", description: classification}
      - {name: center, type: text, collation: en_US.utf8, collation_version: "2.36", description: center}
      - {name: employee_id, type: text, collation: en_US.utf8, collation_version: "2.36", description: employee_id}
      - {name: days_present, type: integer, description: days present}
    eligibility:
      scope_envelope:
        kb_department:
          from_attribute: department
          widen_if:
            - {attribute: job_family, any_of: [finance_controller]}
            - {attribute: payroll_reach, any_of: [all_employees]}
        classification:
          from_attribute: clearance
        employee_id:
          from_attribute: employee_id
          widen_if:
            - {attribute: job_family, any_of: [center_head, dept_admin]}
budget_profiles:
  - name: legacy-resource-test-v1
    max_queries: 5
    max_rows: 100
    max_db_time: 10s
    query_timeout: 5s
    task_ttl: 30m
`

const gradedDelegatedYAML = `
delegated_authorization:
  issuer_role: {attribute: job_family, any_of: [dept_admin]}
  child_self_confirm: true
  center_scope: center
  personal_scope: employee_id
`

const gradedSubjects = `  - subject: finance-admin
    attributes:
      department: [finance]
      job_family: [dept_admin, finance_controller]
      authority_departments: [finance]
      employee_id: [finance-admin]
      clearance: [public, internal, secret, confidential, top_secret]
  - subject: finance-director
    attributes:
      department: [finance]
      job_family: [director, finance_controller]
      authority_departments: [finance]
      clearance: [public, internal, secret, confidential]
  - subject: finance-a-head
    attributes:
      department: [finance]
      job_family: [center_head]
      center: [finance-a]
      authority_center: [finance-a]
      employee_id: [finance-a-head]
      clearance: [public, internal]
  - subject: staff
    attributes:
      department: [finance]
      job_family: [staff]
      center: [finance-a]
      employee_id: [staff]
      clearance: [public, internal]
  - subject: hr-admin
    attributes:
      department: [hr]
      job_family: [dept_admin]
      authority_departments: [hr]
      payroll_reach: [all_employees]
      employee_id: [hr-admin]
      clearance: [public, internal, secret, confidential]
  - subject: general-manager
    attributes:
      department: [finance]
      job_family: [general_manager, finance_controller]
      authority_departments: [engineering, finance, hr, sales]
      clearance: [public, internal, secret, confidential, top_secret]
  - subject: vp-admin
    attributes: {department: [finance], job_family: [vice_president], authority_departments: [finance, hr]}
  - subject: vp-business
    attributes: {department: [sales], job_family: [vice_president], authority_departments: [sales]}
  - subject: vp-rd
    attributes: {department: [engineering], job_family: [vice_president], authority_departments: [engineering]}
  - subject: sales-admin
    attributes: {department: [sales], job_family: [dept_admin], authority_departments: [sales]}
  - subject: engineering-admin
    attributes: {department: [engineering], job_family: [dept_admin], authority_departments: [engineering]}
  - subject: sales-director
    attributes: {department: [sales], job_family: [director], authority_departments: [sales]}
  - subject: hr-director
    attributes: {department: [hr], job_family: [director], authority_departments: [hr]}
  - subject: engineering-director
    attributes: {department: [engineering], job_family: [director], authority_departments: [engineering]}
`

// gradedCatalog is the knowledge Catalog with the graded chain, the five
// standard grades, the attendance product and, when delegated, the block.
func gradedCatalog(t *testing.T, delegated bool) func(string) string {
	t.Helper()
	chained := chainCatalogWith(t, gradedChainRouteYAML)
	fragment := attendanceFragmentYAML
	if delegated {
		fragment += gradedDelegatedYAML
	}
	return func(base string) string {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(chained(base)), &document); err != nil {
			t.Fatal(err)
		}
		root := document.Content[0]
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Value != "scopes" {
				continue
			}
			for _, scope := range root.Content[index+1].Content {
				if yamlField(scope, "name") == nil || yamlField(scope, "name").Value != "classification" {
					continue
				}
				grades := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
				for _, grade := range []string{"public", "internal", "secret", "confidential", "top_secret"} {
					grades.Content = append(grades.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: grade})
				}
				*yamlField(scope, "allowed_values") = *grades
			}
		}
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		merged, err := catalog.MergeYAMLFragment(encoded, []byte(fragment))
		if err != nil {
			t.Fatal(err)
		}
		return string(merged)
	}
}

func yamlField(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

type gradedHarness struct {
	*gatewayHarness
	t          *testing.T
	principals map[string]mcp.Principal
}

func newGradedHarness(t *testing.T, delegated bool) *gradedHarness {
	t.Helper()
	base := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: gradedCatalog(t, delegated),
		principalAttributes: approverSnapshot(t, gradedSubjects)})
	h := &gradedHarness{gatewayHarness: base, t: t, principals: map[string]mcp.Principal{}}
	for _, subject := range []string{"finance-admin", "finance-director", "finance-a-head", "staff", "hr-admin", "general-manager"} {
		principal := mcp.Principal{ID: "principal-" + subject, Subject: subject, Role: "query"}
		if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: principal.ID, Subject: subject,
			Role: "query", CreatedAt: base.clock.value}); err != nil {
			t.Fatal(err)
		}
		h.principals[subject] = principal
	}
	return h
}

func anyValues(values ...string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// documents is a knowledge request over departments at one grade.
func documents(grade string, departments ...string) map[string]any {
	args := kbTaskRequest("finance", []string{grade})
	args["scopes"].(map[string]any)["kb_department"] = anyValues(departments...)
	return args
}

// attendance is a personal-record request.
func attendance(departments, centers, employees []string) map[string]any {
	return map[string]any{
		"objective":     "统计考勤",
		"data_products": []string{"attendance_records"},
		"columns":       map[string][]string{"attendance_records": {"period", "kb_department", "employee_id", "days_present"}},
		"scopes": map[string]any{"kb_department": anyValues(departments...), "classification": anyValues("internal"),
			"center": anyValues(centers...), "employee_id": anyValues(employees...)},
	}
}

func (h *gradedHarness) request(subject string, args map[string]any) (map[string]any, error) {
	h.t.Helper()
	return callGatewayTool(h.service, h.principals[subject], "request_data_task", args)
}

// chainOf lists a created task's chain approvers.
func chainOf(result map[string]any) string {
	steps, _ := result["approval_chain"].([]map[string]any)
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step["approver"].(string))
	}
	return strings.Join(names, ",")
}

func (h *gradedHarness) created(result map[string]any) (control.Task, approval.DraftRequest) {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), result["task_id"].(string))
	if err != nil {
		h.t.Fatal(err)
	}
	for _, draft := range h.approval.requests {
		if draft.Manifest.TaskID == task.ID {
			return task, draft
		}
	}
	h.t.Fatalf("no draft for %s", task.ID)
	return task, approval.DraftRequest{}
}

func (h *gradedHarness) submit(task control.Task, draft approval.DraftRequest, actor string) int {
	h.t.Helper()
	event := oaCallbackEvent{EventID: "submit-" + task.ID + "-" + actor, TaskID: task.ID, DraftID: task.ApprovalRef, Status: "submitted",
		Actor: actor, OccurredAt: h.clock.value, CatalogVersion: h.catalog.CatalogVersion,
		CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest}
	return sendGatewayCallback(h.t, h.gatewayHarness, event, "").Code
}

// decide approves base (the manifest's core when nil) as actor.
func (h *gradedHarness) decide(task control.Task, draft approval.DraftRequest, base *approval.TaskGrantCoreV1, actor string) (int, approval.TaskGrantCoreV1) {
	h.t.Helper()
	core := manifestCore(h.t, draft, h.clock.value)
	if base != nil {
		core = base.Reissued(h.clock.value)
	}
	event := signedFinalCallback(h.t, h.gatewayHarness, task, draft, "approved", actor, core)
	event.EventID += "-" + actor
	event.ApprovalReceipt.ReceiptID += "-" + actor
	signed, err := approval.DemoReceiptSigner([]byte(h.secret)).SignReceipt(*event.ApprovalReceipt)
	if err != nil {
		h.t.Fatal(err)
	}
	event.ApprovalReceipt = &signed
	return sendGatewayCallback(h.t, h.gatewayHarness, event, "").Code, core
}

func (h *gradedHarness) state(taskID string) control.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), taskID)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func (h *gradedHarness) receiptApprover(taskID string) string {
	h.t.Helper()
	grant, err := h.store.GetGrant(context.Background(), taskID)
	if err != nil {
		h.t.Fatal(err)
	}
	protocol, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil {
		h.t.Fatal(err)
	}
	return protocol.ApprovalReceipt.ApproverID
}

func requireSelfService(t *testing.T, result map[string]any, err error, subject string) {
	t.Helper()
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if result["self_service"] != true || result["self_confirm"] != true || result["approver"] != subject || result["approval_chain"] != nil {
		t.Fatalf("not self-service for %s: approver %v self_service %v chain %v", subject, result["approver"], result["self_service"], result["approval_chain"])
	}
}

func TestAuthorityLevelsShapeTheChain(t *testing.T) {
	h := newGradedHarness(t, true)

	// An administrator's own department below their level: self-service,
	// confirmed by the administrator in one OA login.
	result, err := h.request("finance-admin", documents("internal", "finance"))
	requireSelfService(t, result, err, "finance-admin")
	task, draft := h.created(result)
	if code := h.submit(task, draft, "finance-admin"); code != http.StatusOK {
		t.Fatalf("self-service submit = %d", code)
	}
	status := mustCallGatewayTool(t, h.service, h.principals["finance-admin"], "get_task_status", map[string]any{"task_id": task.ID})
	if status["waiting_for"] != "requester_confirmation" || status["approver"] != "finance-admin" {
		t.Fatalf("self-service status %v", status)
	}
	if code, _ := h.decide(task, draft, nil, "finance-director"); code != http.StatusForbidden {
		t.Fatalf("someone else deciding a self-service task = %d, want 403", code)
	}
	if code, _ := h.decide(task, draft, nil, "finance-admin"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("self-service confirmation = %d, state %s", code, h.state(task.ID).State)
	}
	if approver := h.receiptApprover(task.ID); approver != "finance-admin" {
		t.Fatalf("self-service receipt approver %q", approver)
	}

	// Directors rank with administrators: secret is the administrator's
	// own; confidential needs the vice president, top secret also the general
	// manager.
	result, err = h.request("finance-admin", documents("secret", "finance"))
	requireSelfService(t, result, err, "finance-admin")
	result, err = h.request("finance-admin", documents("confidential", "finance"))
	if err != nil || chainOf(result) != "vp-admin" || result["self_service"] != nil {
		t.Fatalf("administrator's confidential request: %v chain %q", err, chainOf(result))
	}
	result, err = h.request("finance-admin", documents("top_secret", "finance"))
	if err != nil || chainOf(result) != "vp-admin,general-manager" {
		t.Fatalf("administrator's top secret request: %v chain %q", err, chainOf(result))
	}
	// Across departments the administrator's own side goes to the next layer
	// in org-chart order, their director, whatever its level.
	result, err = h.request("finance-admin", documents("internal", "finance", "sales"))
	if err != nil || chainOf(result) != "finance-director,sales-admin" {
		t.Fatalf("administrator across departments: %v chain %q", err, chainOf(result))
	}
	task, draft = h.created(result)
	if code := h.submit(task, draft, "finance-admin"); code != http.StatusOK {
		t.Fatalf("submit = %d", code)
	}
	code, first := h.decide(task, draft, nil, "finance-director")
	if code != http.StatusOK || h.state(task.ID).State != control.TaskAwaitingApproval {
		t.Fatalf("first step = %d, state %s", code, h.state(task.ID).State)
	}
	h.clock.value = h.clock.value.Add(time.Minute)
	if code, _ := h.decide(task, draft, &first, "sales-admin"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("second step = %d, state %s", code, h.state(task.ID).State)
	}

	// A director's own department: confidential needs only the vice president.
	result, err = h.request("finance-director", documents("confidential", "finance"))
	if err != nil || chainOf(result) != "vp-admin" {
		t.Fatalf("director's confidential request: %v chain %q", err, chainOf(result))
	}
	task, draft = h.created(result)
	if code := h.submit(task, draft, "finance-director"); code != http.StatusOK {
		t.Fatalf("submit = %d", code)
	}
	if code, _ := h.decide(task, draft, nil, "vp-admin"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("vice president's step = %d, state %s", code, h.state(task.ID).State)
	}
	result, err = h.request("finance-director", documents("secret", "finance"))
	requireSelfService(t, result, err, "finance-director")
	// A director's cross-department request: their side goes to the vice
	// president, the next layer after theirs.
	result, err = h.request("finance-director", documents("internal", "finance", "sales"))
	if err != nil || chainOf(result) != "vp-admin,sales-admin" {
		t.Fatalf("director across departments: %v chain %q", err, chainOf(result))
	}
	task, draft = h.created(result)
	if code := h.submit(task, draft, "finance-director"); code != http.StatusOK {
		t.Fatalf("submit = %d", code)
	}
	h.clock.value = h.clock.value.Add(time.Minute)
	code, first = h.decide(task, draft, nil, "vp-admin")
	if code != http.StatusOK || h.state(task.ID).State != control.TaskAwaitingApproval {
		t.Fatalf("vice president's step = %d, state %s", code, h.state(task.ID).State)
	}
	h.clock.value = h.clock.value.Add(time.Minute)
	if code, _ := h.decide(task, draft, &first, "sales-admin"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("sales administrator's step = %d, state %s", code, h.state(task.ID).State)
	}

	// The general manager holds every department at the top level.
	result, err = h.request("general-manager", documents("top_secret", "engineering", "finance", "hr", "sales"))
	requireSelfService(t, result, err, "general-manager")

	// Naming someone else on a self-service request is refused.
	named := documents("internal", "finance")
	named["approver"] = "finance-director"
	_, err = h.request("finance-admin", named)
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}

func TestCenterHeadsPersonalRecordsAndTheSteward(t *testing.T) {
	h := newGradedHarness(t, true)

	// A center head's own center: self-service; the whole department: the
	// department administrator.
	result, err := h.request("finance-a-head", attendance([]string{"finance"}, []string{"finance-a"}, []string{"finance-a-head", "staff"}))
	requireSelfService(t, result, err, "finance-a-head")
	result, err = h.request("finance-a-head", attendance([]string{"finance"}, []string{"finance-a", "finance-b"}, []string{"finance-a-head", "staff", "staff-b"}))
	if err != nil || chainOf(result) != "finance-admin" {
		t.Fatalf("center head's whole department: %v chain %q", err, chainOf(result))
	}

	// An employee's own records: self-service, confirmed by the employee;
	// someone else's records are outside their envelope.
	result, err = h.request("staff", attendance([]string{"finance"}, []string{"finance-a"}, []string{"staff"}))
	requireSelfService(t, result, err, "staff")
	task, draft := h.created(result)
	if code := h.submit(task, draft, "staff"); code != http.StatusOK {
		t.Fatalf("submit = %d", code)
	}
	if code, _ := h.decide(task, draft, nil, "staff"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("employee's own confirmation = %d, state %s", code, h.state(task.ID).State)
	}
	_, err = h.request("staff", attendance([]string{"finance"}, []string{"finance-a"}, []string{"finance-a-head"}))
	requireToolCode(t, err, apierr.CodeNotEligible)

	// The steward department's administrator: company-wide records are hr's.
	result, err = h.request("hr-admin", attendance([]string{"engineering", "finance", "hr", "sales"},
		[]string{"engineering-a", "finance-a", "hr-a", "sales-a"}, []string{"finance-admin", "staff", "staff-b"}))
	requireSelfService(t, result, err, "hr-admin")

	// A mixed task (documents and records) is routed by department, even
	// when the records are the requester's own: the documents do not declare
	// the personal scope.
	mixed := attendance([]string{"finance", "sales"}, []string{"finance-a"}, []string{"finance-admin"})
	mixed["data_products"] = []string{"attendance_records", "policy_kb_documents"}
	mixed["columns"].(map[string][]string)["policy_kb_documents"] = []string{"doc_id", "version", "title", "kb_department", "classification"}
	result, err = h.request("finance-admin", mixed)
	if err != nil || chainOf(result) != "finance-director,sales-admin" {
		t.Fatalf("mixed task: %v chain %q", err, chainOf(result))
	}
	// Records only and only the requester's own: self-service, whatever the
	// departments.
	result, err = h.request("finance-admin", attendance([]string{"finance", "sales"}, []string{"finance-a"}, []string{"finance-admin"}))
	requireSelfService(t, result, err, "finance-admin")
	// Someone else's records: sales is outside finance-admin's authority and
	// goes to the steward, hr; finance-admin's own side goes one level up.
	result, err = h.request("finance-admin", attendance([]string{"finance", "sales"}, []string{"finance-a"}, []string{"staff"}))
	if err != nil || chainOf(result) != "finance-director,hr-admin" {
		t.Fatalf("records across departments: %v chain %q", err, chainOf(result))
	}
}

func TestMemberConfirmsTheirOwnDelegatedChild(t *testing.T) {
	h := newGradedHarness(t, true)
	// hr opens a company-wide attendance project (self-service, steward).
	result, err := h.request("hr-admin", attendance([]string{"finance", "hr"}, []string{"finance-a", "hr-a"}, []string{"finance-a-head", "staff"}))
	requireSelfService(t, result, err, "hr-admin")
	root, rootDraft := h.created(result)
	if code := h.submit(root, rootDraft, "hr-admin"); code != http.StatusOK {
		t.Fatalf("root submit = %d", code)
	}
	if code, _ := h.decide(root, rootDraft, nil, "hr-admin"); code != http.StatusOK || h.state(root.ID).State != control.TaskActive {
		t.Fatalf("root confirmation = %d, state %s", code, h.state(root.ID).State)
	}

	child := func(employee string) map[string]any {
		args := attendance([]string{"finance"}, []string{"finance-a"}, []string{employee})
		args["parent_task_id"], args["delegate_principal_id"] = root.ID, h.principals["staff"].ID
		return args
	}
	// The member's envelope bounds the child: someone else's record is refused.
	_, err = h.request("hr-admin", child("finance-a-head"))
	requireToolCode(t, err, apierr.CodeNotEligible)

	result, err = h.request("hr-admin", child("staff"))
	if err != nil {
		t.Fatal(err)
	}
	task, draft := h.created(result)
	if result["member_confirm"] != true || result["approver"] != "staff" || draft.Approver != "staff" || !draft.MemberConfirm ||
		draft.Manifest.HumanSubject != "hr-admin" || task.PrincipalID != h.principals["staff"].ID || result["waiting_for"] != "member_submission" {
		t.Fatalf("member child %v draft approver %q member %v human %q principal %q",
			result, draft.Approver, draft.MemberConfirm, draft.Manifest.HumanSubject, task.PrincipalID)
	}
	if code := h.submit(task, draft, "finance-admin"); code != http.StatusForbidden {
		t.Fatalf("a stranger submitting = %d, want 403", code)
	}
	if code := h.submit(task, draft, "staff"); code != http.StatusOK {
		t.Fatalf("the member submitting = %d", code)
	}
	status := mustCallGatewayTool(t, h.service, h.principals["staff"], "get_task_status", map[string]any{"task_id": task.ID})
	if status["waiting_for"] != "member_confirmation" || status["approver"] != "staff" {
		t.Fatalf("member child status %v", status)
	}
	if code, _ := h.decide(task, draft, nil, "hr-admin"); code != http.StatusForbidden {
		t.Fatalf("the project owner deciding the member's child = %d, want 403", code)
	}
	if code, _ := h.decide(task, draft, nil, "staff"); code != http.StatusOK || h.state(task.ID).State != control.TaskActive {
		t.Fatalf("the member confirming = %d, state %s", code, h.state(task.ID).State)
	}
	grant, err := h.store.GetGrant(context.Background(), task.ID)
	if err != nil || grant.Subject != "hr-admin" || h.receiptApprover(task.ID) != "staff" {
		t.Fatalf("member child grant subject %q receipt approver %q %v", grant.Subject, h.receiptApprover(task.ID), err)
	}
}

func TestRequestedBudgetProfile(t *testing.T) {
	h := newGradedHarness(t, true)
	args := documents("internal", "finance")
	args["budget_profile"] = "final-v5-concurrency-v1"
	result, err := h.request("finance-admin", args)
	if err != nil || result["budget_profile"] != "final-v5-concurrency-v1" || result["budget_source"] != "requested_profile" ||
		result["budget"].(map[string]any)["task_ttl_seconds"] != int64(7200) {
		t.Fatalf("requested profile: %v %v source %v budget %v", err, result["budget_profile"], result["budget_source"], result["budget"])
	}
	for _, profile := range []string{"no-such-profile", "legacy-resource-test-v1"} {
		args["budget_profile"] = profile
		_, err := h.request("finance-admin", args)
		requireToolCode(t, err, apierr.CodeInvalidRequest)
	}
	var schema map[string]any
	for _, tool := range h.service.ListTools(h.principals["finance-admin"]) {
		if tool.Name == "request_data_task" {
			schema = tool.InputSchema
		}
	}
	if _, offered := schema["properties"].(map[string]any)["budget_profile"]; !offered {
		t.Fatal("request_data_task does not offer budget_profile under the Catalog block")
	}
	if _, ok := mustCallGatewayTool(t, h.service, h.principals["staff"], "list_data_products", map[string]any{})["delegated_authorization"]; !ok {
		t.Fatal("list_data_products does not describe the delegated-authorization rules")
	}
}

// Without the Catalog block none of the rules applies.
func TestDelegatedRulesAreOffWithoutTheCatalogBlock(t *testing.T) {
	h := newGradedHarness(t, false)
	args := documents("internal", "finance")
	args["budget_profile"] = "final-v5-concurrency-v1"
	_, err := h.request("finance-admin", args)
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	// The administrator is the first step of their own chain, which refuses.
	_, err = h.request("finance-admin", documents("internal", "finance"))
	requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	// An employee's own records go to the department as before.
	result, err := h.request("staff", attendance([]string{"finance"}, []string{"finance-a"}, []string{"staff"}))
	if err != nil || chainOf(result) != "finance-admin" || result["self_service"] != nil {
		t.Fatalf("employee's own records without the block: %v chain %q", err, chainOf(result))
	}
	for _, tool := range h.service.ListTools(h.principals["staff"]) {
		if tool.Name != "request_data_task" {
			continue
		}
		if _, offered := tool.InputSchema["properties"].(map[string]any)["budget_profile"]; offered {
			t.Fatal("request_data_task offers budget_profile without the Catalog block")
		}
	}
	if _, ok := mustCallGatewayTool(t, h.service, h.principals["staff"], "list_data_products", map[string]any{})["delegated_authorization"]; ok {
		t.Fatal("list_data_products describes delegated rules without the Catalog block")
	}
}

// The callback side: a member-confirmed child's approver may submit, nobody
// else beyond the human subject.
func TestCallbackLetsTheMemberSubmit(t *testing.T) {
	member := pendingContext{ApprovalMode: "manual", Approver: "staff", MemberConfirm: true}
	if !validCallbackActor(oaCallbackEvent{Status: "submitted", Actor: "staff"}, member, "hr-admin") {
		t.Fatal("the member could not submit")
	}
	if !validCallbackActor(oaCallbackEvent{Status: "approved", Actor: "staff"}, member, "hr-admin") {
		t.Fatal("the member could not confirm")
	}
	unmarked := pendingContext{ApprovalMode: "manual", Approver: "staff"}
	if validCallbackActor(oaCallbackEvent{Status: "submitted", Actor: "staff"}, unmarked, "hr-admin") {
		t.Fatal("an approver submitted an unmarked task")
	}
}
