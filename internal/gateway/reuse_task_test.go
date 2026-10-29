package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Reusing a covering task (GATEWAY_REUSE_COVERING_TASK; the demo, not a
// paper claim). The harness is the ordered-scope one (delegated block,
// agent_submits, kb-annual-v1, classification ordered public < internal <
// secret < confidential < top_secret) with staff a finance controller, so
// staff may also ask for other departments; alice, finance's administrator,
// issues staff's standing authorizations.

const reuseSubjects = `  - subject: alice
    attributes:
      department: [finance]
      job_family: [dept_admin]
      clearance: [public, internal, secret, confidential]
  - subject: staff
    attributes:
      department: [finance]
      job_family: [staff, finance_controller]
      clearance: [public, internal, secret, confidential, top_secret]
  - subject: finance-staff
    attributes:
      department: [finance]
      job_family: [staff]
      clearance: [public, internal, secret]
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
`

const reuseIssuedObjective = "分管条线经营数据查询"

func newReuseHarness(t *testing.T, on bool) *orderedScopeHarness {
	t.Helper()
	base := newGatewayHarnessWithOptions(t, harnessOptions{
		rewriteCatalog: withAgentSubmits(t, withAnnualProfile(t, withOrderedClassification(t,
			delegatedCatalog(chainCatalogWith(t, orderedChainRouteYAML))))),
		principalAttributes: approverSnapshot(t, reuseSubjects)})
	base.approval.response.State = "pending"
	base.service.reuseCoveringTask = on
	h := &orderedScopeHarness{issueHarness: &issueHarness{gatewayHarness: base, t: t,
		staff: mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"}},
		financeStaff: mcp.Principal{ID: "principal-finance-staff", Subject: "finance-staff", Role: "query"}}
	for _, principal := range []mcp.Principal{h.staff, h.financeStaff} {
		if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: principal.ID, Subject: principal.Subject,
			Role: principal.Role, CreatedAt: base.clock.value}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// draftOf is the OA draft filed for a task.
func (h *orderedScopeHarness) draftOf(taskID string) approval.DraftRequest {
	h.t.Helper()
	for index := len(h.approval.requests) - 1; index >= 0; index-- {
		if h.approval.requests[index].Manifest.TaskID == taskID {
			return h.approval.requests[index]
		}
	}
	h.t.Fatalf("no OA draft for %s", taskID)
	return approval.DraftRequest{}
}

// approveCreated approves a newly created task through every approver it
// names (each step of its chain, or its one approver) and requires ACTIVE.
func (h *orderedScopeHarness) approveCreated(created map[string]any) string {
	h.t.Helper()
	id, _ := created["task_id"].(string)
	approvers := []string{}
	if steps, ok := created["approval_chain"].([]map[string]any); ok && len(steps) > 0 {
		for _, step := range steps {
			approvers = append(approvers, step["approver"].(string))
		}
	} else {
		approvers = append(approvers, created["approver"].(string))
	}
	draft := h.draftOf(id)
	for _, approver := range approvers {
		if code := h.callback(h.task(id), draft, "approved", approver); code != http.StatusOK {
			h.t.Fatalf("%s approving %s = %d", approver, id, code)
		}
	}
	if state := h.task(id).State; state != control.TaskActive {
		h.t.Fatalf("task %s after %v: %s, want ACTIVE", id, approvers, state)
	}
	return id
}

// issueStanding has alice issue staff a one-year finance authorization at
// the given grade (closed downward) and approves it.
func (h *orderedScopeHarness) issueStanding(classification ...string) string {
	h.t.Helper()
	args := issueArgs(h.staff.ID, annualProfile, "finance")
	values := make([]any, 0, len(classification))
	for _, value := range classification {
		values = append(values, value)
	}
	args["scopes"].(map[string]any)["classification"] = values
	args["objective"] = reuseIssuedObjective
	issued := mustCallGatewayTool(h.t, h.service, h.alice, issueTool.Name, args)
	if issued["reused_existing_task"] != nil {
		h.t.Fatalf("an issuance was answered by reuse: %v", issued)
	}
	return h.approveCreated(issued)
}

// ownRequest is staff's own request over departments and grades.
func ownRequest(departments []string, classification ...string) map[string]any {
	args := kbTaskRequest(departments[0], classification)
	values := make([]any, 0, len(departments))
	for _, department := range departments {
		values = append(values, department)
	}
	args["scopes"].(map[string]any)["kb_department"] = values
	args["objective"] = "查本条线月度 KPI"
	return args
}

// taskCount counts every task row.
func (h *orderedScopeHarness) taskCount() int {
	h.t.Helper()
	tasks, err := h.store.ListTasks(context.Background(), control.TaskFilter{Limit: 500})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(tasks)
}

// reuseEvents are the TASK_REQUEST_REUSED_EXISTING events on a task.
func (h *orderedScopeHarness) reuseEvents(taskID string) []control.AuditEvent {
	h.t.Helper()
	events, err := h.store.ListAuditEvents(context.Background(), control.AuditFilter{TaskID: taskID, EventType: taskRequestReusedEvent})
	if err != nil {
		h.t.Fatal(err)
	}
	return events
}

// requireReused checks a response reuses want without creating anything.
func (h *orderedScopeHarness) requireReused(label string, args map[string]any, want string) map[string]any {
	h.t.Helper()
	tasks, drafts, events := h.taskCount(), len(h.approval.requests), len(h.reuseEvents(want))
	result := mustCallGatewayTool(h.t, h.service, h.staff, "request_data_task", args)
	if result["reused_existing_task"] != true || result["task_id"] != want || result["state"] != control.TaskActive {
		h.t.Fatalf("%s: %v, want reuse of %s", label, result, want)
	}
	if got := h.taskCount(); got != tasks {
		h.t.Fatalf("%s: %d task rows after reuse, want %d", label, got, tasks)
	}
	if got := len(h.approval.requests); got != drafts {
		h.t.Fatalf("%s: %d OA drafts after reuse, want %d", label, got, drafts)
	}
	if got := len(h.reuseEvents(want)); got != events+1 {
		h.t.Fatalf("%s: %d reuse audit events, want %d", label, got, events+1)
	}
	return result
}

// requireCreated checks a response is a new task, not a reuse.
func (h *orderedScopeHarness) requireCreated(label string, principal mcp.Principal, tool string, args map[string]any, notWant ...string) map[string]any {
	h.t.Helper()
	tasks := h.taskCount()
	result := mustCallGatewayTool(h.t, h.service, principal, tool, args)
	if _, present := result["reused_existing_task"]; present {
		h.t.Fatalf("%s: reused %v", label, result)
	}
	id, _ := result["task_id"].(string)
	for _, existing := range notWant {
		if id == existing {
			h.t.Fatalf("%s: answered with the existing task %s", label, existing)
		}
	}
	if result["state"] == control.TaskActive || h.task(id).State == control.TaskActive {
		h.t.Fatalf("%s: a new task is already ACTIVE: %v", label, result)
	}
	if got := h.taskCount(); got != tasks+1 {
		h.t.Fatalf("%s: %d task rows, want %d", label, got, tasks+1)
	}
	return result
}

func TestReuseCoveringTaskOffStillCreatesATask(t *testing.T) {
	h := newReuseHarness(t, false)
	standing := h.issueStanding("secret")
	h.requireCreated("covered request, switch off", h.staff, "request_data_task", ownRequest([]string{"finance"}, "secret"), standing)
	if events := h.reuseEvents(standing); len(events) != 0 {
		t.Fatalf("reuse audit events with the switch off: %+v", events)
	}
}

func TestReuseCoveringTaskReusesAnExactCover(t *testing.T) {
	h := newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	result := h.requireReused("exact cover", ownRequest([]string{"finance"}, "secret"), standing)
	for key, want := range map[string]any{
		"reuse_note": "已复用你名下生效中的授权「" + reuseIssuedObjective + "」，无需审批", "objective": reuseIssuedObjective,
		"requested_objective": "查本条线月度 KPI", "kind": taskKindAuthorization, "issued_by": "alice", "human_subject": "staff",
		"budget_profile": annualProfile, "budget_source": "reused_existing_task", "next_action": "use_task",
		"delegate_principal_id": h.staff.ID, "root_task_id": standing,
	} {
		if result[key] != want {
			t.Fatalf("reused %s = %v, want %v (%v)", key, result[key], want, result)
		}
	}
	if steps, ok := result["approval_chain"].([]map[string]any); !ok || len(steps) != 0 {
		t.Fatalf("reused approval_chain %v", result["approval_chain"])
	}
	grant, err := h.store.GetGrant(context.Background(), standing)
	if err != nil {
		t.Fatal(err)
	}
	if expires, _ := result["expires_at"].(time.Time); !expires.Equal(grant.ExpiresAt) {
		t.Fatalf("reused expires_at %v, grant %v", result["expires_at"], grant.ExpiresAt)
	}
	if ttl := result["budget"].(map[string]any)["task_ttl_seconds"]; ttl != int64(8760*3600) {
		t.Fatalf("reused budget TTL %v", ttl)
	}
	scopes, _ := result["scopes"].(map[string]any)
	if got, _ := scopeStringValues(scopes["classification"]); strings.Join(got, ",") != "internal,public,secret" {
		t.Fatalf("reused scopes %v", scopes)
	}
	if _, present := result["scope_closure"]; !present {
		t.Fatalf("the request's closure is not reported: %v", result)
	}
	events := h.reuseEvents(standing)
	if len(events) != 1 || events[0].Actor != h.staff.ID {
		t.Fatalf("reuse audit events %+v", events)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload["requested_objective"] != "查本条线月度 KPI" ||
		payload["reused_task_id"] != standing || payload["reused_objective"] != reuseIssuedObjective || payload["human_subject"] != "staff" {
		t.Fatalf("reuse audit payload %s %v", events[0].Payload, err)
	}
	// The reused task is the one the agent then uses.
	if taskContext := mustCallGatewayTool(t, h.service, h.staff, "get_task_context", map[string]any{"task_id": standing}); taskContext["task"].(map[string]any)["task_id"] != standing {
		t.Fatalf("get_task_context of the reused task %v", taskContext)
	}
}

func TestReuseCoveringTaskRefusesAWiderRequest(t *testing.T) {
	h := newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	extraColumn := ownRequest([]string{"finance"}, "secret")
	columns := extraColumn["columns"].(map[string][]string)
	columns["policy_kb_chunks"] = append(append([]string(nil), columns["policy_kb_chunks"]...), "labels")
	extraProduct := ownRequest([]string{"finance"}, "internal")
	extraProduct["data_products"] = []string{"policy_kb_chunks", "policy_kb_documents", "policy_kb_values"}
	extraProduct["columns"].(map[string][]string)["policy_kb_values"] = []string{"doc_id", "version", "field", "kb_department", "classification"}
	for label, args := range map[string]map[string]any{
		"an extra department": ownRequest([]string{"finance", "sales"}, "secret"),
		"a higher grade":      ownRequest([]string{"finance"}, "confidential"),
		"an extra column":     extraColumn,
		"an extra product":    extraProduct,
	} {
		h.requireCreated(label, h.staff, "request_data_task", args, standing)
	}
	if events := h.reuseEvents(standing); len(events) != 0 {
		t.Fatalf("wider requests left reuse events: %+v", events)
	}
}

func TestReuseCoveringTaskReusesANarrowerRequest(t *testing.T) {
	h := newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	narrower := ownRequest([]string{"finance"}, "public")
	narrower["data_products"] = []string{"policy_kb_chunks"}
	narrower["columns"] = map[string][]string{"policy_kb_chunks": {"doc_id", "chunk_index", "kb_department"}}
	h.requireReused("one product, fewer columns, a lower grade", narrower, standing)

	// A day task of staff's own over two departments covers one of them; the
	// finance-only authorization does not cover sales.
	wide := h.approveCreated(h.requireCreated("finance and sales", h.staff, "request_data_task",
		ownRequest([]string{"finance", "sales"}, "internal"), standing))
	result := h.requireReused("sales only", ownRequest([]string{"sales"}, "internal"), wide)
	if result["kind"] != taskKindTask {
		t.Fatalf("reused day task kind %v", result["kind"])
	}
}

func TestReuseCoveringTaskAppliesTheOrderedClosure(t *testing.T) {
	h := newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	result := h.requireReused("internal under a secret grant", ownRequest([]string{"finance"}, "internal"), standing)
	if closure, _ := result["scope_closure"].(map[string][]string); strings.Join(closure["classification"], ",") != "public" {
		t.Fatalf("scope_closure %v", result["scope_closure"])
	}

	h = newReuseHarness(t, true)
	internal := h.issueStanding("internal")
	h.requireCreated("secret under an internal grant", h.staff, "request_data_task", ownRequest([]string{"finance"}, "secret"), internal)
	h.requireReused("public under an internal grant", ownRequest([]string{"finance"}, "public"), internal)

	// The approver dropped public from a secret authorization: a request for
	// secret, closed to public and internal, is not covered, although its
	// literal value is.
	h = newReuseHarness(t, true)
	args := issueArgs(h.staff.ID, annualProfile, "finance")
	args["scopes"].(map[string]any)["classification"] = []any{"secret"}
	issued := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, args)
	narrowed := h.task(issued["task_id"].(string))
	draft := h.draftOf(narrowed.ID)
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, h.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	core.MandatoryScope["classification"] = []string{"internal", "secret"}
	if code := sendGatewayCallback(t, h.gatewayHarness, signedFinalCallback(t, h.gatewayHarness, narrowed, draft, "narrowed", "alice", core), "").Code; code != http.StatusOK ||
		h.task(narrowed.ID).State != control.TaskActive {
		t.Fatalf("narrowed approval = %d, state %s", code, h.task(narrowed.ID).State)
	}
	h.requireCreated("secret, closed past the narrowed grant", h.staff, "request_data_task", ownRequest([]string{"finance"}, "secret"), narrowed.ID)
}

func TestReuseCoveringTaskIgnoresExpiredAndArchivedTasks(t *testing.T) {
	h := newReuseHarness(t, true)
	day := h.approveCreated(h.requireCreated("first day task", h.staff, "request_data_task", ownRequest([]string{"finance"}, "internal")))
	h.requireReused("while it lasts", ownRequest([]string{"finance"}, "internal"), day)
	grant, err := h.store.GetGrant(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.value = grant.ExpiresAt.Add(time.Second)
	h.requireCreated("after its grant expired", h.staff, "request_data_task", ownRequest([]string{"finance"}, "internal"), day)

	h = newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	mustCallGatewayTool(t, h.service, h.staff, "revoke_task", map[string]any{"task_id": standing, "reason": "调岗"})
	if state := h.task(standing).State; state != control.TaskArchived {
		t.Fatalf("revoked authorization %s", state)
	}
	h.requireCreated("after it was revoked", h.staff, "request_data_task", ownRequest([]string{"finance"}, "internal"), standing)
}

func TestReuseCoveringTaskNeverAnswersChildIssuedOrDownloadRequests(t *testing.T) {
	h := newReuseHarness(t, true)
	standing := h.issueStanding("secret")
	// alice's own task covers what she then issues staff.
	aliceOwn := h.approveCreated(h.requireCreated("alice's own task", h.alice, "request_data_task", ownRequest([]string{"finance"}, "secret")))
	// An ACTIVE download task covers a plain request of its products and
	// columns, but a download task cannot be queried: never reused.
	downloadArgs := map[string]any{
		"objective": "下载一份文档", "knowledge_product": "policy_kb", "documents": []map[string]any{{"doc_id": "D-1", "version": 1}},
		"scopes": map[string]any{"kb_department": []any{"finance"}, "classification": []any{"secret"}},
	}
	activeDownload := h.approveCreated(h.requireCreated("the first download task", h.staff, "request_download_task", downloadArgs, standing))
	downloadShaped := ownRequest([]string{"finance"}, "secret")
	downloadShaped["columns"].(map[string][]string)["policy_kb_documents"] = []string{"doc_id", "version", "title", "chunk_count", "kb_department", "classification"}
	downloadCover := h.approveCreated(h.requireCreated("a plain request shaped like the download task", h.staff, "request_data_task",
		downloadShaped, standing, activeDownload))
	// A download request is never answered by reuse, although staff's plain
	// task covers its products and columns.
	if download := h.requireCreated("a second download task", h.staff, "request_download_task", downloadArgs, standing, activeDownload, downloadCover); download["task_kind"] != "download" {
		t.Fatalf("download %v", download)
	}
	child := ownRequest([]string{"finance"}, "internal")
	child["parent_task_id"] = standing
	if created := h.requireCreated("a child", h.staff, "request_data_task", child, standing); created["self_confirm"] != true {
		t.Fatalf("child %v", created)
	}
	delegated := ownRequest([]string{"finance"}, "internal")
	delegated["parent_task_id"], delegated["delegate_principal_id"] = standing, h.financeStaff.ID
	if created := h.requireCreated("a member's child", h.staff, "request_data_task", delegated, standing); created["member_confirm"] != true {
		t.Fatalf("member child %v", created)
	}
	issue := issueArgs(h.staff.ID, annualProfile, "finance")
	issue["scopes"].(map[string]any)["classification"] = []any{"secret"}
	h.requireCreated("an identical issuance", h.alice, issueTool.Name, issue, standing, aliceOwn)
	for _, id := range []string{standing, aliceOwn, activeDownload, downloadCover} {
		if events := h.reuseEvents(id); len(events) != 0 {
			t.Fatalf("reuse events on %s for child, issued or download requests: %+v", id, events)
		}
	}
	// The plain covering task is reused for a plain request; the download
	// task, which covers it too, is not.
	h.requireReused("staff's plain request", downloadShaped, downloadCover)
}

func TestReuseCoveringTaskPicksTheLatestExpiry(t *testing.T) {
	// A day task and a one-year authorization both cover: the authorization.
	h := newReuseHarness(t, false)
	h.approveCreated(h.requireCreated("day task", h.staff, "request_data_task", ownRequest([]string{"finance"}, "secret")))
	standing := h.issueStanding("secret")
	h.service.reuseCoveringTask = true
	h.requireReused("both cover, the authorization lasts longer", ownRequest([]string{"finance"}, "internal"), standing)

	// Two covering day tasks, read in task-id order (ids are random): the one
	// approved a minute later lasts longer and is returned, whether its id
	// sorts first or last.
	for _, laterSortsFirst := range []bool{false, true} {
		h := newReuseHarness(t, false)
		created := map[string]map[string]any{}
		for range 2 {
			result := h.requireCreated("day task", h.staff, "request_data_task", ownRequest([]string{"finance"}, "secret"))
			created[result["task_id"].(string)] = result
		}
		ids := make([]string, 0, 2)
		for id := range created {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		earlier, later := ids[0], ids[1]
		if laterSortsFirst {
			earlier, later = ids[1], ids[0]
		}
		h.approveCreated(created[earlier])
		h.clock.value = h.clock.value.Add(time.Minute)
		h.approveCreated(created[later])
		h.service.reuseCoveringTask = true
		h.requireReused(fmt.Sprintf("the later-approved day task sorting first=%v", laterSortsFirst),
			ownRequest([]string{"finance"}, "internal"), later)
	}
}

// Off, every tool description is as it was; on, request_data_task and
// list_my_tasks each gain exactly their sentence, with and without the
// delegated block.
func TestReuseCoveringTaskToolDescriptions(t *testing.T) {
	for label, service := range map[string]*Service{
		"delegated": newReuseHarness(t, false).service,
		"plain":     newGatewayHarness(t).service,
	} {
		principal := mcp.Principal{ID: "principal-alice", Subject: "alice", Role: "query"}
		service.reuseCoveringTask = false
		off := service.ListTools(principal)
		service.reuseCoveringTask = true
		on := service.ListTools(principal)
		if len(off) == 0 || len(off) != len(on) {
			t.Fatalf("%s: %d tools off, %d on", label, len(off), len(on))
		}
		for index := range off {
			name := off[index].Name
			if strings.Contains(off[index].Description, "授权复用") {
				t.Fatalf("%s: %s mentions reuse with the switch off", label, name)
			}
			if want := off[index].Description + reuseCoveringTaskDescriptions[name]; on[index].Name != name || on[index].Description != want {
				t.Fatalf("%s: %s description on = %q, want %q", label, name, on[index].Description, want)
			}
		}
		for _, name := range []string{"request_data_task", "list_my_tasks"} {
			if !strings.Contains(reuseCoveringTaskDescriptions[name], "授权复用") {
				t.Fatalf("%s has no reuse sentence", name)
			}
		}
	}
	if strings.Contains(queryTools[3].Description+queryTools[4].Description, "授权复用") {
		t.Fatal("the switch leaked into the static tool descriptions")
	}
}

// The coverage rule on its own: products, columns, enum sets, date bounds,
// unrestricted and moot dimensions.
func TestReuseCoverageRule(t *testing.T) {
	grant := control.TaskGrant{
		ApprovedProducts: []string{"payslip", "dept_kpi"},
		ApprovedColumns:  map[string][]string{"payslip": {"period", "employee_id", "net_pay"}, "dept_kpi": {"metric"}},
	}
	approved := map[string]any{
		"kb_department": []any{"finance", "sales"}, "employee_id": []any{"e1", "e2"},
		"period": map[string]any{"from": "2026-01-01", "to": "2026-12-31"}, "region": []any{"north"},
	}
	// payslip publishes kb_department, employee_id, period and center, not region.
	relevant := map[string]bool{"kb_department": true, "employee_id": true, "period": true, "center": true, "net_pay": true}
	request := func(edit func(products *[]string, columns map[string][]string, scope map[string]any)) bool {
		products := []string{"payslip"}
		columns := map[string][]string{"payslip": {"period", "net_pay"}}
		scope := map[string]any{"kb_department": []string{"finance"}, "employee_id": []string{"e1"},
			"period": map[string]string{"from": "2026-03-01", "to": "2026-03-31"}}
		if edit != nil {
			edit(&products, columns, scope)
		}
		return grantCoversRequest(grant, approved, products, columns, scope, relevant)
	}
	for _, tc := range []struct {
		label string
		edit  func(*[]string, map[string][]string, map[string]any)
		want  bool
	}{
		{"inside every dimension", nil, true},
		{"the grant's whole sets", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["kb_department"], s["employee_id"] = []string{"finance", "sales"}, []string{"e1", "e2"}
			s["period"] = map[string]string{"from": "2026-01-01", "to": "2026-12-31"}
		}, true},
		{"another employee", func(_ *[]string, _ map[string][]string, s map[string]any) { s["employee_id"] = []string{"e1", "e3"} }, false},
		{"another department", func(_ *[]string, _ map[string][]string, s map[string]any) { s["kb_department"] = []string{"hr"} }, false},
		{"an unapproved column", func(_ *[]string, c map[string][]string, _ map[string]any) {
			c["payslip"] = []string{"period", "bonus"}
		}, false},
		{"an unapproved product", func(p *[]string, c map[string][]string, _ map[string]any) {
			*p = append(*p, "leave_record")
			c["leave_record"] = []string{"period"}
		}, false},
		{"a dimension the grant leaves open", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["center"] = []string{"finance-a"}
		}, true},
		{"employee_id left open on a product that publishes it", func(_ *[]string, _ map[string][]string, s map[string]any) {
			delete(s, "employee_id")
		}, false},
		{"period before the grant", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["period"] = map[string]string{"from": "2025-12-01", "to": "2026-03-31"}
		}, false},
		{"period after the grant", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["period"] = map[string]string{"from": "2026-03-01", "to": "2027-01-31"}
		}, false},
		{"an open-ended period", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["period"] = map[string]string{"from": "2026-03-01", "to": ""}
		}, false},
		{"an enum where the grant has dates", func(_ *[]string, _ map[string][]string, s map[string]any) {
			s["period"] = []string{"2026-03"}
		}, false},
		{"an empty value set", func(_ *[]string, _ map[string][]string, s map[string]any) { s["employee_id"] = []string{} }, false},
	} {
		if got := request(tc.edit); got != tc.want {
			t.Fatalf("%s: covers = %v, want %v", tc.label, got, tc.want)
		}
	}
	// A grant open at one end admits any bound there.
	if !scopeValueCovers(map[string]any{"from": "2026-01-01", "to": ""}, map[string]string{"from": "2026-02-01", "to": "2030-01-01"}) ||
		scopeValueCovers(map[string]any{"from": "2026-01-01", "to": ""}, map[string]string{"from": "", "to": "2026-02-01"}) {
		t.Fatal("open-ended grant bounds")
	}
	// A single string is a one-value set.
	if !scopeValueCovers([]any{"finance", "sales"}, "sales") || scopeValueCovers("finance", []string{"finance", "sales"}) {
		t.Fatal("single-string enum values")
	}
}
