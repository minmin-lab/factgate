package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// list_my_tasks under the delegated-authorization model (KB1-LOOP-150; the
// demo, not a paper claim): each task says whether it is a project (a root
// task whose budget profile was chosen), a day task (a root task on its
// route's default profile), a child inside a project, or an authorization an
// administrator issued, with its budget profile, products, scopes and
// approval roles; root_task_id and kind filter. Without the Catalog block the
// listing is publicTask, as before.

const annualProfile = "kb-annual-v1"

// annualProfileYAML is a one-year budget profile for the test Catalog: the
// KB route's detail-manual-v5 limits with a year's TTL, as the demo Catalog's
// kb-annual-v1 is kb-large-v1's limits with a year's TTL.
const annualProfileYAML = `
name: kb-annual-v1
max_queries: 5
max_rows: 100
max_db_time: 15s
query_timeout: 5s
task_ttl: 8760h
max_release_facts: 500
max_influence_facts: 2500
max_outcome_facts: 5
exposure_profile_version: taskgate-exposure-v5
predicate_footprint: {version: taskgate-predicate-footprint-v1, max_raw_literals_per_query: 500, max_unique_atoms_per_query: 4, max_atom_payload_bytes: 4096, max_total_atom_payload_bytes: 16384}
`

// withAnnualProfile adds kb-annual-v1 to a rewritten Catalog's budget profiles.
func withAnnualProfile(t *testing.T, rewrite func(string) string) func(string) string {
	t.Helper()
	return func(base string) string {
		var document, profile yaml.Node
		if err := yaml.Unmarshal([]byte(rewrite(base)), &document); err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal([]byte(annualProfileYAML), &profile); err != nil {
			t.Fatal(err)
		}
		profiles := yamlField(document.Content[0], "budget_profiles")
		if profiles == nil || profiles.Kind != yaml.SequenceNode {
			t.Fatal("the rewritten Catalog has no budget_profiles list")
		}
		profiles.Content = append(profiles.Content, profile.Content[0])
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

// newListTasksHarness is the issue harness with the demo's switches: the
// delegated_authorization block with agent_submits, and kb-annual-v1.
func newListTasksHarness(t *testing.T) *issueHarness {
	t.Helper()
	base := newGatewayHarnessWithOptions(t, harnessOptions{
		rewriteCatalog:      withAgentSubmits(t, withAnnualProfile(t, delegatedCatalog(chainCatalogWith(t, issueChainRouteYAML)))),
		principalAttributes: approverSnapshot(t, issueSubjects)})
	if _, ok := base.catalog.LookupBudgetProfile(annualProfile); !ok || base.catalog.DelegatedAuthorization == nil {
		t.Fatal("the harness Catalog lacks kb-annual-v1 or the delegated_authorization block")
	}
	base.approval.response.State = "pending"
	staff := mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"}
	if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: staff.ID, Subject: staff.Subject, Role: staff.Role, CreatedAt: base.clock.value}); err != nil {
		t.Fatal(err)
	}
	return &issueHarness{gatewayHarness: base, t: t, staff: staff}
}

type projectTree struct{ root, child, authorization, day string }

// buildProjectTree gives staff a one-year project (finance, public + internal;
// alice approves it narrowed to internal), a child inside it that staff
// confirmed, an authorization alice issued staff, awaiting alice, and a day
// task (no budget_profile), awaiting alice.
func buildProjectTree(t *testing.T, h *issueHarness) projectTree {
	t.Helper()
	projectArgs := kbTaskRequest("finance", []string{"internal", "public"})
	projectArgs["budget_profile"] = annualProfile
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", projectArgs)
	if created["budget_profile"] != annualProfile || created["approver"] != "alice" {
		t.Fatalf("project %v", created)
	}
	root := h.task(created["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, h.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	core.MandatoryScope["classification"] = []string{"internal"}
	if code := sendGatewayCallback(t, h.gatewayHarness, signedFinalCallback(t, h.gatewayHarness, root, draft, "narrowed", "alice", core), "").Code; code != http.StatusOK ||
		h.task(root.ID).State != control.TaskActive {
		t.Fatalf("narrowed approval of the project = %d, state %s", code, h.task(root.ID).State)
	}

	childArgs := kbTaskRequest("finance", []string{"internal"})
	childArgs["parent_task_id"] = root.ID
	created = mustCallGatewayTool(t, h.service, h.staff, "request_data_task", childArgs)
	child := h.task(created["task_id"].(string))
	if code := h.callback(child, h.approval.requests[len(h.approval.requests)-1], "approved", "staff"); code != http.StatusOK ||
		h.task(child.ID).State != control.TaskActive {
		t.Fatalf("staff confirming the child = %d, state %s", code, h.task(child.ID).State)
	}

	issued := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, annualProfile, "finance"))
	day := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	if day["budget_profile"] != "detail-manual-v5" || day["budget_source"] != "catalog_profile" {
		t.Fatalf("day task %v", day)
	}
	return projectTree{root: root.ID, child: child.ID, authorization: issued["task_id"].(string), day: day["task_id"].(string)}
}

// persistedTopLevel reads a task's private request context as a JSON object.
func persistedTopLevel(t *testing.T, task control.Task) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(task.RequestContext, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// listTasks calls list_my_tasks and returns its entries by task_id as an MCP
// client reads them, through JSON.
func listTasks(t *testing.T, h *gatewayHarness, principal mcp.Principal, args map[string]any) map[string]map[string]any {
	t.Helper()
	result := mustCallGatewayTool(t, h.service, principal, "list_my_tasks", args)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, item := range decoded.Tasks {
		byID[item["task_id"].(string)] = item
	}
	if len(byID) != len(decoded.Tasks) {
		t.Fatalf("list_my_tasks %v repeats a task", args)
	}
	return byID
}

func requireListedIDs(t *testing.T, label string, listed map[string]map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(listed))
	for id := range listed {
		got = append(got, id)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s lists %v, want %v", label, got, want)
	}
}

// requireListed checks an entry's fields; a nil want means the field is absent.
func requireListed(t *testing.T, label string, item map[string]any, want map[string]any) {
	t.Helper()
	for field, value := range want {
		got, present := item[field]
		if value == nil {
			if present {
				t.Fatalf("%s: %s = %v, want it absent (entry %v)", label, field, got, item)
			}
			continue
		}
		if !present || got != value {
			t.Fatalf("%s: %s = %v, want %v (entry %v)", label, field, got, value, item)
		}
	}
}

// listedStrings reads a JSON string list (a product list or a scope's values).
func listedStrings(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, _ := value.(string)
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

func requireScope(t *testing.T, label string, item map[string]any, name string, want ...string) {
	t.Helper()
	scopes, _ := item["scopes"].(map[string]any)
	if got := listedStrings(scopes[name]); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: scope %s = %v, want %v (scopes %v)", label, name, got, want, item["scopes"])
	}
}

func TestListMyTasksTellsProjectsChildrenAndAuthorizationsApart(t *testing.T) {
	h := newListTasksHarness(t)
	tree := buildProjectTree(t, h)

	all := listTasks(t, h.gatewayHarness, h.staff, map[string]any{})
	requireListedIDs(t, "staff's tasks", all, tree.root, tree.child, tree.authorization, tree.day)
	root, child, authorization, day := all[tree.root], all[tree.child], all[tree.authorization], all[tree.day]
	requireListed(t, "project", root, map[string]any{"kind": "project", "state": "ACTIVE", "budget_profile": annualProfile,
		"approver": "alice", "self_confirm": false, "member_confirm": false, "agent_submitted": true,
		"root_task_id": tree.root, "parent_task_id": "", "issued_by": nil, "human_subject": nil, "detail_unavailable": nil})
	// alice narrowed the project to internal: the grant's scope, not the request's.
	requireScope(t, "project", root, "classification", "internal")
	requireScope(t, "project", root, "kb_department", "finance")
	if got := listedStrings(root["products"]); strings.Join(got, ",") != "policy_kb_chunks,policy_kb_documents" {
		t.Fatalf("project products %v", root["products"])
	}
	// A child is requested under its route's profile (detail-manual-v5); its
	// budget is that profile intersected with the project's grant.
	requireListed(t, "child", child, map[string]any{"kind": "child", "state": "ACTIVE", "budget_profile": "detail-manual-v5",
		"approver": "staff", "self_confirm": true, "member_confirm": false, "agent_submitted": true,
		"root_task_id": tree.root, "parent_task_id": tree.root, "issued_by": nil})
	requireScope(t, "child", child, "classification", "internal")
	requireListed(t, "authorization", authorization, map[string]any{"kind": "authorization", "state": "AWAITING_APPROVAL",
		"budget_profile": annualProfile, "approver": "alice", "issued_by": "alice", "human_subject": "staff",
		"self_confirm": false, "member_confirm": false, "agent_submitted": true,
		"root_task_id": tree.authorization, "parent_task_id": ""})
	requireScope(t, "authorization", authorization, "kb_department", "finance")
	requireScope(t, "authorization", authorization, "classification", "internal")
	requireListed(t, "day task", day, map[string]any{"kind": "task", "state": "AWAITING_APPROVAL", "budget_profile": "detail-manual-v5",
		"approver": "alice", "self_confirm": false, "agent_submitted": true, "root_task_id": tree.day, "parent_task_id": "", "issued_by": nil})
	// What the listing reads: the profile and whether it was chosen, recorded
	// at creation.
	for id, want := range map[string][2]any{tree.root: {annualProfile, true}, tree.day: {"detail-manual-v5", nil},
		tree.authorization: {annualProfile, true}} {
		fields := persistedTopLevel(t, h.task(id))
		if fields["budget_profile"] != want[0] || fields["budget_profile_chosen"] != want[1] {
			t.Fatalf("task %s records budget_profile %v chosen %v, want %v %v", id, fields["budget_profile"], fields["budget_profile_chosen"], want[0], want[1])
		}
	}

	requireListedIDs(t, "root_task_id", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"root_task_id": tree.root}), tree.root, tree.child)
	requireListedIDs(t, "kind project", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "project"}), tree.root)
	requireListedIDs(t, "kind task", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "task"}), tree.day)
	requireListedIDs(t, "kind root before any pre-150 record", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "root"}))
	requireListedIDs(t, "kind child", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "child"}), tree.child)
	requireListedIDs(t, "kind authorization", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "authorization"}), tree.authorization)
	requireListedIDs(t, "root_task_id and kind project",
		listTasks(t, h.gatewayHarness, h.staff, map[string]any{"root_task_id": tree.root, "kind": "project"}), tree.root)
	requireListedIDs(t, "state ACTIVE", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"state": "ACTIVE"}), tree.root, tree.child)
	_, err := callGatewayTool(h.service, h.staff, "list_my_tasks", map[string]any{"kind": "projects"})
	requireToolCode(t, err, apierr.CodeInvalidRequest)

	// A root task recorded before the profile was (the project's context
	// without the two fields) is neither project nor task: kind root, its
	// budget_profile found by its limits.
	preContext := persistedTopLevel(t, h.task(tree.root))
	delete(preContext, "budget_profile")
	delete(preContext, "budget_profile_chosen")
	encodedPre, err := json.Marshal(preContext)
	if err != nil {
		t.Fatal(err)
	}
	// A task whose private context does not decode is listed without detail,
	// and no kind filter matches it.
	for _, task := range []control.Task{
		{ID: "task-pre150", PrincipalID: h.staff.ID, Objective: "before KB1-LOOP-150", State: control.TaskAwaitingApproval,
			CatalogVersion: h.catalog.CatalogVersion, RequestContext: encodedPre, CreatedAt: h.clock.value, UpdatedAt: h.clock.value},
		{ID: "task-legacy", PrincipalID: h.staff.ID, Objective: "legacy", State: control.TaskAwaitingSubmission,
			CatalogVersion: h.catalog.CatalogVersion, RequestContext: json.RawMessage(`{}`), CreatedAt: h.clock.value, UpdatedAt: h.clock.value},
	} {
		if err := h.store.CreateTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	all = listTasks(t, h.gatewayHarness, h.staff, map[string]any{})
	requireListedIDs(t, "staff's tasks with older records", all, tree.root, tree.child, tree.authorization, tree.day, "task-pre150", "task-legacy")
	requireListed(t, "pre-150 root", all["task-pre150"], map[string]any{"kind": "root", "budget_profile": annualProfile, "detail_unavailable": nil})
	requireListed(t, "legacy", all["task-legacy"], map[string]any{"detail_unavailable": true, "kind": nil, "products": nil, "approver": nil})
	requireListedIDs(t, "kind root with older records", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "root"}), "task-pre150")
	requireListedIDs(t, "kind project with older records", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "project"}), tree.root)

	// The tool says so; the static tool does not.
	var listed *mcp.Tool
	for _, tool := range h.service.ListTools(h.staff) {
		if tool.Name == "list_my_tasks" {
			listed = &tool
		}
	}
	if listed == nil {
		t.Fatal("list_my_tasks is not listed")
	}
	properties, _ := listed.InputSchema["properties"].(map[string]any)
	if properties["root_task_id"] == nil || properties["kind"] == nil || properties["state"] == nil ||
		!strings.HasPrefix(listed.Description, queryTools[4].Description) || !strings.Contains(listed.Description, "本目录启用了长期授权模型") {
		t.Fatalf("list_my_tasks under the block: %+v", listed)
	}
	// The vocabulary comes from the Catalog: kb-annual-v1 is no route's
	// default, detail-manual-v5 is the KB route's.
	for _, tool := range h.service.ListTools(h.staff) {
		for _, want := range map[string][]string{
			"list_my_tasks":     {"kind=project", "项目 = 申请时选了预算档", "kb-annual-v1（365 天）", "detail-manual-v5（15 分钟）", "日常任务"},
			"request_data_task": {"要立项目", "kb-annual-v1（365 天）", "detail-manual-v5（15 分钟）", "日常任务"},
		}[tool.Name] {
			if !strings.Contains(tool.Description, want) {
				t.Fatalf("%s description lacks %q: %s", tool.Name, want, tool.Description)
			}
		}
	}
	if static, _ := queryTools[4].InputSchema["properties"].(map[string]any); queryTools[4].Name != "list_my_tasks" ||
		static["root_task_id"] != nil || strings.Contains(queryTools[4].Description, "长期授权") {
		t.Fatal("the switch leaked into the static list_my_tasks tool")
	}
}

// An authorization is its employee's task (its principal is the employee's):
// the administrator who issued it does not list it, and follows it by its
// task_id with get_task_status.
func TestListMyTasksLeavesAnIssuedAuthorizationToItsEmployee(t *testing.T) {
	h := newListTasksHarness(t)
	issued := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, issueArgs(h.staff.ID, annualProfile, "finance"))
	id := issued["task_id"].(string)
	requireListedIDs(t, "the issuer's tasks", listTasks(t, h.gatewayHarness, h.alice, map[string]any{}))
	requireListedIDs(t, "the issuer's authorizations", listTasks(t, h.gatewayHarness, h.alice, map[string]any{"kind": "authorization"}))
	if status := mustCallGatewayTool(t, h.service, h.alice, "get_task_status", map[string]any{"task_id": id}); status["state"] != control.TaskAwaitingApproval {
		t.Fatalf("the issuer's status of the authorization %v", status)
	}
	requireListedIDs(t, "the employee's authorizations", listTasks(t, h.gatewayHarness, h.staff, map[string]any{"kind": "authorization"}), id)
}

// A chain task names the approver it waits for now, as get_task_status does.
func TestListMyTasksNamesTheChainsCurrentApprover(t *testing.T) {
	h := newListTasksHarness(t)
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"confidential"}))
	steps, _ := created["approval_chain"].([]map[string]any)
	if len(steps) != 2 || steps[0]["approver"] != "alice" || steps[1]["approver"] != "finance-director" {
		t.Fatalf("chain %v", created["approval_chain"])
	}
	task := h.task(created["task_id"].(string))
	draft := h.approval.requests[len(h.approval.requests)-1]
	requireListed(t, "before any approval", listTasks(t, h.gatewayHarness, h.staff, map[string]any{})[task.ID],
		map[string]any{"kind": "task", "approver": "alice", "state": "AWAITING_APPROVAL"})
	if code := h.callback(task, draft, "approved", "alice"); code != http.StatusOK || h.task(task.ID).State != control.TaskAwaitingApproval {
		t.Fatalf("alice's approval = %d, state %s", code, h.task(task.ID).State)
	}
	requireListed(t, "after alice", listTasks(t, h.gatewayHarness, h.staff, map[string]any{})[task.ID],
		map[string]any{"kind": "task", "approver": "finance-director", "state": "AWAITING_APPROVAL"})
	if status := mustCallGatewayTool(t, h.service, h.staff, "get_task_status", map[string]any{"task_id": task.ID}); status["approver"] != "finance-director" {
		t.Fatalf("get_task_status approver %v", status["approver"])
	}
}

// Without the Catalog block the listing and the tool are as before, and the
// new filters are refused.
func TestListMyTasksWithoutTheCatalogBlockKeepsThePlainListing(t *testing.T) {
	h := newIssueHarness(t, false)
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	id := created["task_id"].(string)
	result := mustCallGatewayTool(t, h.service, h.staff, "list_my_tasks", map[string]any{})
	tasks, ok := result["tasks"].([]map[string]any)
	if !ok || len(tasks) != 1 || tasks[0]["task_id"] != id {
		t.Fatalf("plain listing %v", result)
	}
	plain := publicTask(h.task(id))
	if len(tasks[0]) != len(plain) {
		t.Fatalf("plain entry has %d fields, publicTask %d: %v", len(tasks[0]), len(plain), tasks[0])
	}
	for field := range plain {
		if _, present := tasks[0][field]; !present {
			t.Fatalf("plain entry lacks %s: %v", field, tasks[0])
		}
	}
	for _, args := range []map[string]any{{"root_task_id": id}, {"kind": "root"}} {
		_, err := callGatewayTool(h.service, h.staff, "list_my_tasks", args)
		requireToolCode(t, err, apierr.CodeInvalidRequest)
		if !strings.Contains(err.Error(), "只在目录启用 delegated_authorization 时可用") {
			t.Fatalf("list_my_tasks %v without the block: %v", args, err)
		}
	}
	if fields := persistedTopLevel(t, h.task(id)); fields["budget_profile"] != nil || fields["budget_profile_chosen"] != nil {
		t.Fatalf("the budget profile was recorded without the block: %v %v", fields["budget_profile"], fields["budget_profile_chosen"])
	}
	for _, tool := range h.service.ListTools(h.staff) {
		if tool.Name == "request_data_task" && strings.Contains(tool.Description, "要立项目") {
			t.Fatalf("request_data_task mentions projects without the block: %s", tool.Description)
		}
		if tool.Name != "list_my_tasks" {
			continue
		}
		properties, _ := tool.InputSchema["properties"].(map[string]any)
		if tool.Description != "列出自己名下的任务：自己发起的任务，以及管理员为自己开的授权单。可按 state 过滤。" ||
			properties["root_task_id"] != nil || properties["kind"] != nil {
			t.Fatalf("list_my_tasks without the block: %+v", tool)
		}
	}
}
