package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
)

const chainRouteYAML = `
requester_attribute: department
department_scope: kb_department
directors: {engineering: engineering-director, finance: finance-director, hr: hr-director, sales: sales-director}
director_authority: {attribute: department, role: {attribute: job_family, any_of: [director]}}
escalation:
  scope: classification
  values: [leadership]
  leaders: {engineering: leader-rd, finance: leader-admin, hr: leader-admin, sales: leader-business}
  leader_authority: {attribute: oversees, role: {attribute: job_family, any_of: [leader]}}
`

const chainSubjects = `  - subject: alice
    attributes:
      department: [finance]
      job_family: [staff, finance_controller]
      clearance: [public, internal, confidential, leadership]
  - subject: finance-director
    attributes: {department: [finance], job_family: [director]}
  - subject: sales-director
    attributes: {department: [sales], job_family: [director]}
  - subject: engineering-director
    attributes: {department: [engineering], job_family: [director]}
  - subject: hr-director
    attributes: {department: [hr], job_family: [director]}
  - subject: leader-admin
    attributes: {job_family: [leader], oversees: [finance, hr]}
  - subject: leader-business
    attributes: {job_family: [leader], oversees: [sales]}
  - subject: leader-rd
    attributes: {job_family: [leader], oversees: [engineering]}
`

// chainCatalog turns the knowledge catalog's high-sensitivity route into an
// approval chain, editing the YAML node tree so every other scalar keeps its
// exact form.
func chainCatalog(t *testing.T) func(string) string {
	t.Helper()
	knowledge := kbCatalogRewrite(t)
	var chain yaml.Node
	if err := yaml.Unmarshal([]byte(chainRouteYAML), &chain); err != nil {
		t.Fatal(err)
	}
	return func(base string) string {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(knowledge(base)), &document); err != nil {
			t.Fatal(err)
		}
		root := document.Content[0]
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Value != "approval_routes" {
				continue
			}
			for _, route := range root.Content[index+1].Content {
				fields := map[string]int{}
				for field := 0; field+1 < len(route.Content); field += 2 {
					fields[route.Content[field].Value] = field
				}
				sensitivity, hasSensitivity := fields["sensitivity"]
				if _, scoped := fields["products"]; scoped || !hasSensitivity || route.Content[sensitivity+1].Value != "high" {
					continue
				}
				kept := route.Content[:0]
				for field := 0; field+1 < len(route.Content); field += 2 {
					if name := route.Content[field].Value; name != "approver" && name != "verify_approver" {
						kept = append(kept, route.Content[field], route.Content[field+1])
					}
				}
				route.Content = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Value: "chain"}, chain.Content[0])
			}
		}
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

type chainHarness struct {
	*gatewayHarness
	t     *testing.T
	steps int
}

func newChainHarness(t *testing.T, subjects string) *chainHarness {
	t.Helper()
	return &chainHarness{t: t, gatewayHarness: newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: chainCatalog(t),
		principalAttributes: approverSnapshot(t, subjects)})}
}

// request files and submits a knowledge task over departments and
// classifications, returning the creation result, the task and its draft.
func (h *chainHarness) request(departments, classification []string, approver string) (map[string]any, control.Task, approval.DraftRequest) {
	h.t.Helper()
	args := kbTaskRequest("finance", classification)
	scopes := args["scopes"].(map[string]any)
	values := make([]any, 0, len(departments))
	for _, department := range departments {
		values = append(values, department)
	}
	scopes["kb_department"] = values
	if approver != "" {
		args["approver"] = approver
	}
	result := mustCallGatewayTool(h.t, h.service, h.alice, "request_data_task", args)
	task, err := h.store.GetTask(context.Background(), result["task_id"].(string))
	if err != nil {
		h.t.Fatal(err)
	}
	draft := h.approval.requests[len(h.approval.requests)-1]
	submitted := oaCallbackEvent{EventID: "submit-" + task.ID, TaskID: task.ID, DraftID: task.ApprovalRef, Status: "submitted",
		Actor: h.alice.Subject, OccurredAt: h.clock.value, CatalogVersion: h.catalog.CatalogVersion,
		CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest}
	if response := sendGatewayCallback(h.t, h.gatewayHarness, submitted, ""); response.Code != http.StatusOK {
		h.t.Fatalf("submit callback = %d %s", response.Code, response.Body.String())
	}
	return result, task, draft
}

// decide sends one chain decision a minute after the last, as the OA does:
// the decision's core is the previous core re-issued now, narrowed by edit.
func (h *chainHarness) decide(task control.Task, draft approval.DraftRequest, base approval.TaskGrantCoreV1, actor string,
	edit func(*approval.TaskGrantCoreV1)) (int, map[string]any, approval.TaskGrantCoreV1) {
	h.t.Helper()
	h.steps++
	h.clock.value = h.clock.value.Add(time.Minute)
	core := base.Reissued(h.clock.value)
	status := "approved"
	if edit != nil {
		edit(&core)
		status = "narrowed"
	}
	event := signedFinalCallback(h.t, h.gatewayHarness, task, draft, status, actor, core)
	event.EventID = event.EventID + "-" + actor
	event.ApprovalReceipt.ReceiptID = event.ApprovalReceipt.ReceiptID + "-" + actor
	signed, err := approval.DemoReceiptSigner([]byte(h.secret)).SignReceipt(*event.ApprovalReceipt)
	if err != nil {
		h.t.Fatal(err)
	}
	event.ApprovalReceipt = &signed
	response := sendGatewayCallback(h.t, h.gatewayHarness, event, "")
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return response.Code, body, core
}

func (h *chainHarness) state(taskID string) control.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), taskID)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func manifestCore(t *testing.T, draft approval.DraftRequest, at time.Time) approval.TaskGrantCoreV1 {
	t.Helper()
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, at)
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func TestApprovalChainRunsTheOrgChartAndGrantsAfterTheLastStep(t *testing.T) {
	h := newChainHarness(t, chainSubjects)
	result, task, draft := h.request([]string{"sales"}, []string{"internal", "leadership"}, "")
	if draft.Approver != "finance-director" || result["approver"] != "finance-director" {
		t.Fatalf("first approver %q / %v, want the requester's own director", draft.Approver, result["approver"])
	}
	steps, _ := result["approval_chain"].([]map[string]any)
	if len(steps) != 3 || steps[1]["approver"] != "sales-director" || steps[2]["approver"] != "leader-business" {
		t.Fatalf("request-time chain %v", result["approval_chain"])
	}

	// Out of turn: the leader may not decide before the directors.
	code, _, _ := h.decide(task, draft, manifestCore(t, draft, h.clock.value), "leader-business", nil)
	if code != http.StatusForbidden {
		t.Fatalf("an out-of-turn approval = %d, want 403", code)
	}
	code, body, first := h.decide(task, draft, manifestCore(t, draft, h.clock.value), "finance-director", nil)
	if code != http.StatusOK || body["next_approver"] != "sales-director" || body["granted"] != false {
		t.Fatalf("first step = %d %v", code, body)
	}
	if got := h.state(task.ID).State; got != control.TaskAwaitingApproval {
		t.Fatalf("task after the first step is %s", got)
	}
	status := mustCallGatewayTool(t, h.service, h.alice, "get_task_status", map[string]any{"task_id": task.ID})
	if status["approver"] != "sales-director" || !strings.Contains(detailsJSON(t, status["approval_chain"]), `"status":"approved"`) {
		t.Fatalf("status after the first step %v", status)
	}
	code, body, second := h.decide(task, draft, first, "sales-director", nil)
	if code != http.StatusOK || body["next_approver"] != "leader-business" {
		t.Fatalf("second step = %d %v", code, body)
	}
	code, body, _ = h.decide(task, draft, second, "leader-business", nil)
	if code != http.StatusOK || body["granted"] == false {
		t.Fatalf("last step = %d %v", code, body)
	}
	if got := h.state(task.ID).State; got != control.TaskActive {
		t.Fatalf("task after the last step is %s", got)
	}
	grant, err := h.store.GetGrant(context.Background(), task.ID)
	if err != nil || !strings.Contains(string(grant.MandatoryScope), "leadership") {
		t.Fatalf("grant %+v %v", grant, err)
	}
	events, err := h.store.ListApprovalEvents(context.Background(), task.ID)
	if err != nil || len(events) != 4 {
		t.Fatalf("approval events %d %v, want submit + three approvals", len(events), err)
	}
}

func TestApprovalChainIsRecomputedAfterANarrowing(t *testing.T) {
	h := newChainHarness(t, chainSubjects)
	_, task, draft := h.request([]string{"sales"}, []string{"internal", "leadership"}, "")
	_, _, first := h.decide(task, draft, manifestCore(t, draft, h.clock.value), "finance-director", nil)
	// The sales director removes leadership: no leader step remains, so this
	// narrowing is the last approval and issues the grant.
	code, body, _ := h.decide(task, draft, first, "sales-director", func(core *approval.TaskGrantCoreV1) {
		core.MandatoryScope["classification"] = []string{"internal"}
	})
	if code != http.StatusOK || body["granted"] == false {
		t.Fatalf("narrowing step = %d %v", code, body)
	}
	grant, err := h.store.GetGrant(context.Background(), task.ID)
	if err != nil || strings.Contains(string(grant.MandatoryScope), "leadership") {
		t.Fatalf("narrowed grant %s %v", grant.MandatoryScope, err)
	}
	// A later step may only narrow what the previous step left.
	_, task, draft = h.request([]string{"sales"}, []string{"internal"}, "")
	_, _, first = h.decide(task, draft, manifestCore(t, draft, h.clock.value), "finance-director", func(core *approval.TaskGrantCoreV1) {
		core.ApprovedColumns["policy_kb_documents"] = []string{"doc_id", "version"}
	})
	widened := first
	widened.ApprovedColumns = cloneColumns(first.ApprovedColumns)
	widened.ApprovedColumns["policy_kb_documents"] = append(widened.ApprovedColumns["policy_kb_documents"], "title")
	if code, _, _ := h.decide(task, draft, widened, "sales-director", nil); code != http.StatusConflict {
		t.Fatalf("a step restoring a column the previous step removed = %d, want 409", code)
	}
}

func TestApprovalChainRefusesAnApproverWithoutTheStepsAuthority(t *testing.T) {
	h := newChainHarness(t, chainSubjects)
	// The requester sends the first step to the sales director, as a
	// misrouted OA would: the approval runs, the grant is refused.
	result, task, draft := h.request([]string{"finance"}, []string{"internal"}, "sales-director")
	if authorized, _ := result["approver_authorized"].(*bool); authorized == nil || *authorized {
		t.Fatalf("the misrouted first approver was not previewed as unauthorized: %v", result["approver_authorized"])
	}
	code, body, _ := h.decide(task, draft, manifestCore(t, draft, h.clock.value), "sales-director", nil)
	if code != http.StatusOK || body["terminal_reason"] != "approver_not_authorized" {
		t.Fatalf("misrouted approval = %d %v", code, body)
	}
	if got := h.state(task.ID); got.State != control.TaskArchived || got.TerminalReason != control.TerminalApproverNotAuthorized {
		t.Fatalf("task %+v", got)
	}
	if _, err := h.store.GetGrant(context.Background(), task.ID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a misrouted approval issued a grant: %v", err)
	}
}

func TestApprovalChainFailsClosedOnAWrongOrgChart(t *testing.T) {
	// The Catalog names hr-director, but the attested attributes do not make
	// them a director: an hr request is refused before any draft.
	subjects := strings.Replace(chainSubjects, "attributes: {department: [hr], job_family: [director]}", "attributes: {department: [hr], job_family: [staff]}", 1)
	h := newChainHarness(t, subjects)
	args := kbTaskRequest("finance", []string{"internal"})
	args["scopes"].(map[string]any)["kb_department"] = []any{"hr"}
	_, err := callGatewayTool(h.service, h.alice, "request_data_task", args)
	requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	if len(h.approval.requests) != 0 {
		t.Fatalf("a wrong org chart produced %d drafts", len(h.approval.requests))
	}
}
