package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Ordered scope dimensions (KB1-LOOP-152; the demo's classification, not a
// paper claim). With classification ordered public < internal < secret <
// confidential < top_secret, a request for a grade carries every grade below
// it into the manifest, whichever tool files it, and the response names what
// was added (scope_closure). Eligibility and delegation see the whole set;
// enforcement is unchanged. A Catalog without any order adds nothing.

var orderedGrades = []string{"public", "internal", "secret", "confidential", "top_secret"}

// orderedChainRouteYAML is issueChainRouteYAML over the five grades: the
// directors' escalation for confidential and top_secret.
const orderedChainRouteYAML = `
requester_attribute: department
department_scope: kb_department
directors: {engineering: engineering-admin, finance: alice, hr: hr-admin, sales: sales-admin}
director_authority: {attribute: department, role: {attribute: job_family, any_of: [dept_admin]}}
escalations:
  - role: director
    label: 部门主任
    scope: classification
    values: [confidential, top_secret]
    leaders: {engineering: engineering-director, finance: finance-director, hr: hr-director, sales: sales-director}
    leader_authority: {attribute: department, role: {attribute: job_family, any_of: [director]}}
`

// orderedSubjects mirror the demo: staff holds all five grades, finance-staff
// three (up to secret), alice (finance's administrator) four.
const orderedSubjects = `  - subject: alice
    attributes:
      department: [finance]
      job_family: [dept_admin]
      clearance: [public, internal, secret, confidential]
  - subject: staff
    attributes:
      department: [finance]
      job_family: [staff]
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

// withOrderedClassification gives a rewritten Catalog's classification scope
// the five grades (allowed_values in byte order, as the demo's) and their
// order.
func withOrderedClassification(t *testing.T, rewrite func(string) string) func(string) string {
	t.Helper()
	sequence := func(values []string) *yaml.Node {
		node := &yaml.Node{Kind: yaml.SequenceNode}
		for _, value := range values {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
		}
		return node
	}
	return func(base string) string {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(rewrite(base)), &document); err != nil {
			t.Fatal(err)
		}
		scopes := yamlField(document.Content[0], "scopes")
		found := false
		for _, scope := range scopes.Content {
			if name := yamlField(scope, "name"); name == nil || name.Value != "classification" {
				continue
			}
			allowed := yamlField(scope, "allowed_values")
			if allowed == nil {
				t.Fatal("classification declares no allowed_values")
			}
			*allowed = *sequence([]string{"confidential", "internal", "public", "secret", "top_secret"})
			scope.Content = append(scope.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "order"}, sequence(orderedGrades))
			found = true
		}
		if !found {
			t.Fatal("the rewritten Catalog has no classification scope")
		}
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

type orderedScopeHarness struct {
	*issueHarness
	financeStaff mcp.Principal
}

// newOrderedScopeHarness is the list_my_tasks harness (delegated block,
// agent_submits, kb-annual-v1) with classification ordered.
func newOrderedScopeHarness(t *testing.T) *orderedScopeHarness {
	t.Helper()
	base := newGatewayHarnessWithOptions(t, harnessOptions{
		rewriteCatalog: withAgentSubmits(t, withAnnualProfile(t, withOrderedClassification(t,
			delegatedCatalog(chainCatalogWith(t, orderedChainRouteYAML))))),
		principalAttributes: approverSnapshot(t, orderedSubjects)})
	if order := base.catalog.ScopeOrder("classification"); !reflect.DeepEqual(order, orderedGrades) {
		t.Fatalf("the harness Catalog orders classification %v", order)
	}
	base.approval.response.State = "pending"
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

// lastDraft is the most recent OA draft.
func (h *orderedScopeHarness) lastDraft() approval.DraftRequest {
	return h.approval.requests[len(h.approval.requests)-1]
}

// manifestScope reads one enum scope of a draft's manifest, sorted.
func manifestScope(t *testing.T, draft approval.DraftRequest, name string) []string {
	t.Helper()
	values, ok := scopeStringValues(draft.Manifest.MandatoryScope[name])
	if !ok {
		t.Fatalf("manifest scope %s = %#v", name, draft.Manifest.MandatoryScope[name])
	}
	return values
}

// requireClosure checks a creation result's scope_closure: absent when want
// is nil, otherwise exactly want for classification.
func requireClosure(t *testing.T, label string, result map[string]any, want []string) {
	t.Helper()
	closure, present := result["scope_closure"]
	if want == nil {
		if present {
			t.Fatalf("%s: scope_closure %v, want none", label, closure)
		}
		return
	}
	if got, _ := closure.(map[string][]string); !reflect.DeepEqual(got, map[string][]string{"classification": want}) {
		t.Fatalf("%s: scope_closure %#v, want classification %v", label, closure, want)
	}
}

func TestOrderedScopeRequestDataTaskClosesDownward(t *testing.T) {
	h := newOrderedScopeHarness(t)
	for _, tc := range []struct {
		requested     []string
		manifest, add []string
	}{
		{[]string{"top_secret"}, []string{"confidential", "internal", "public", "secret", "top_secret"},
			[]string{"public", "internal", "secret", "confidential"}},
		{[]string{"internal"}, []string{"internal", "public"}, []string{"public"}},
		{[]string{"secret", "public"}, []string{"internal", "public", "secret"}, []string{"internal"}},
		{[]string{"public"}, []string{"public"}, nil},
		{[]string{"internal", "public"}, []string{"internal", "public"}, nil},
	} {
		label := strings.Join(tc.requested, "+")
		created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", tc.requested))
		requireClosure(t, label, created, tc.add)
		draft := h.lastDraft()
		if got := manifestScope(t, draft, "classification"); !reflect.DeepEqual(got, tc.manifest) {
			t.Fatalf("%s: manifest classification %v, want %v", label, got, tc.manifest)
		}
		if got := manifestScope(t, draft, "kb_department"); !reflect.DeepEqual(got, []string{"finance"}) {
			t.Fatalf("%s: an unordered scope changed: %v", label, got)
		}
		// The persisted request context holds the same set the manifest does.
		pending, err := decodePending(h.task(created["task_id"].(string)))
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := scopeStringValues(pending.MandatoryScope["classification"]); !reflect.DeepEqual(got, tc.manifest) {
			t.Fatalf("%s: pending classification %v, want %v", label, got, tc.manifest)
		}
	}
}

func TestOrderedScopeRequestDownloadTaskClosesExplicitScopes(t *testing.T) {
	h := newOrderedScopeHarness(t)
	documents := []map[string]any{{"doc_id": "D-1", "version": 1}}
	explicit := mustCallGatewayTool(t, h.service, h.staff, "request_download_task", map[string]any{
		"objective": "下载一份绝密文档", "knowledge_product": "policy_kb", "documents": documents,
		"scopes": map[string]any{"kb_department": []any{"finance"}, "classification": []any{"top_secret"}},
	})
	if explicit["task_kind"] != "download" || explicit["scopes_defaulted_to_envelope"] != false {
		t.Fatalf("explicit download %v", explicit)
	}
	requireClosure(t, "explicit top_secret", explicit, []string{"public", "internal", "secret", "confidential"})
	if got := manifestScope(t, h.lastDraft(), "classification"); !reflect.DeepEqual(got, []string{"confidential", "internal", "public", "secret", "top_secret"}) {
		t.Fatalf("explicit download classification %v", got)
	}
	// A defaulted scope is the envelope, already closed downward here: the
	// closure adds nothing and the response says nothing about it.
	defaulted := mustCallGatewayTool(t, h.service, h.financeStaff, "request_download_task", map[string]any{
		"objective": "下载一份文档", "knowledge_product": "policy_kb", "documents": documents,
	})
	if defaulted["scopes_defaulted_to_envelope"] != true {
		t.Fatalf("defaulted download %v", defaulted)
	}
	requireClosure(t, "defaulted envelope", defaulted, nil)
	if got := manifestScope(t, h.lastDraft(), "classification"); !reflect.DeepEqual(got, []string{"internal", "public", "secret"}) {
		t.Fatalf("defaulted download classification %v", got)
	}
}

func TestOrderedScopeIssueDataAuthorizationCloses(t *testing.T) {
	h := newOrderedScopeHarness(t)
	args := issueArgs(h.staff.ID, annualProfile, "finance")
	args["scopes"].(map[string]any)["classification"] = []any{"secret"}
	issued := mustCallGatewayTool(t, h.service, h.alice, issueTool.Name, args)
	if issued["issued_by"] != "alice" || issued["human_subject"] != "staff" {
		t.Fatalf("issued %v", issued)
	}
	requireClosure(t, "issued secret", issued, []string{"public", "internal"})
	draft := h.lastDraft()
	if got := manifestScope(t, draft, "classification"); !reflect.DeepEqual(got, []string{"internal", "public", "secret"}) {
		t.Fatalf("issued classification %v", got)
	}
	if draft.IssuedBy != "alice" || draft.Manifest.HumanSubject != "staff" {
		t.Fatalf("issued draft issued_by %q human subject %q", draft.IssuedBy, draft.Manifest.HumanSubject)
	}
}

func TestOrderedScopeChildClosureStaysInsideTheParentGrant(t *testing.T) {
	h := newOrderedScopeHarness(t)
	rootArgs := kbTaskRequest("finance", []string{"secret"})
	rootArgs["budget_profile"] = annualProfile
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", rootArgs)
	requireClosure(t, "root secret", created, []string{"public", "internal"})
	root := h.task(created["task_id"].(string))
	if code := h.callback(root, h.lastDraft(), "approved", "alice"); code != http.StatusOK || h.task(root.ID).State != control.TaskActive {
		t.Fatalf("approval of the root = %d, state %s", code, h.task(root.ID).State)
	}
	grant, err := h.store.GetGrant(context.Background(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	var granted map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &granted); err != nil {
		t.Fatal(err)
	}
	if got, _ := scopeStringValues(granted["classification"]); !reflect.DeepEqual(got, []string{"internal", "public", "secret"}) {
		t.Fatalf("root grant classification %v", got)
	}

	// internal closes to public + internal, inside the parent's three grades.
	childArgs := kbTaskRequest("finance", []string{"internal"})
	childArgs["parent_task_id"] = root.ID
	created = mustCallGatewayTool(t, h.service, h.staff, "request_data_task", childArgs)
	requireClosure(t, "child internal", created, []string{"public"})
	child := h.task(created["task_id"].(string))
	if code := h.callback(child, h.lastDraft(), "approved", "staff"); code != http.StatusOK || h.task(child.ID).State != control.TaskActive {
		t.Fatalf("staff confirming the child = %d, state %s", code, h.task(child.ID).State)
	}

	// confidential closes to four grades; staff is eligible for them, but the
	// parent grant holds three, so delegation refuses it before any draft.
	drafts := len(h.approval.requests)
	wider := kbTaskRequest("finance", []string{"confidential"})
	wider["parent_task_id"] = root.ID
	_, err = callGatewayTool(h.service, h.staff, "request_data_task", wider)
	requireToolCode(t, err, apierr.CodePolicyDenied)
	if len(h.approval.requests) != drafts {
		t.Fatal("a refused child produced an OA draft")
	}
}

func TestOrderedScopeClosureIsCheckedForEligibility(t *testing.T) {
	h := newOrderedScopeHarness(t)
	drafts := len(h.approval.requests)
	_, err := callGatewayTool(h.service, h.financeStaff, "request_data_task", kbTaskRequest("finance", []string{"confidential"}))
	requireToolCode(t, err, apierr.CodeNotEligible)
	if len(h.approval.requests) != drafts {
		t.Fatal("an ineligible request produced an OA draft")
	}
	// secret closes to public + internal + secret, all inside the envelope.
	created := mustCallGatewayTool(t, h.service, h.financeStaff, "request_data_task", kbTaskRequest("finance", []string{"secret"}))
	requireClosure(t, "finance-staff secret", created, []string{"public", "internal"})
	if got := manifestScope(t, h.lastDraft(), "classification"); !reflect.DeepEqual(got, []string{"internal", "public", "secret"}) {
		t.Fatalf("finance-staff classification %v", got)
	}
	pending, err := decodePending(h.task(created["task_id"].(string)))
	if err != nil || pending.Eligibility == nil {
		t.Fatalf("pending eligibility %v %v", pending.Eligibility, err)
	}
	for _, rule := range pending.Eligibility.Rules {
		if trace := rule.Envelope["classification"]; !reflect.DeepEqual(trace.Requested, []string{"internal", "public", "secret"}) {
			t.Fatalf("eligibility trace for %s records %v, want the closed set", rule.Product, trace.Requested)
		}
	}
}

func TestOrderedScopeIsPublishedOnTheDiscoverySurface(t *testing.T) {
	h := newOrderedScopeHarness(t)
	const sentence = "classification 是有序维度（public < internal < secret < confidential < top_secret）：填最高一级即可，低于它的各级自动包含"
	described := map[string]string{}
	for _, tool := range h.service.ListTools(h.staff) {
		described[tool.Name] = tool.Description
	}
	for name := range orderedScopeTools {
		if !strings.Contains(described[name], sentence) {
			t.Fatalf("%s description does not name the ordered scope: %q", name, described[name])
		}
	}
	for _, name := range []string{"search", "list_my_tasks", "get_task_status"} {
		if strings.Contains(described[name], "有序维度") {
			t.Fatalf("%s description names the ordered scope", name)
		}
	}
	if strings.Contains(queryTools[3].Description, "有序维度") || strings.Contains(issueTool.Description, "有序维度") ||
		strings.Contains(requestDownloadTaskTool.Description, "有序维度") {
		t.Fatal("the ordered scope leaked into the static tool descriptions")
	}

	for _, name := range []string{"policy_kb_chunks", "policy_kb"} {
		page := mustCallGatewayTool(t, h.service, h.staff, "describe_data_product", map[string]any{"name": name})
		if got, _ := page["scope_order"].(map[string][]string); !reflect.DeepEqual(got, map[string][]string{"classification": orderedGrades}) {
			t.Fatalf("describe_data_product %s scope_order %#v", name, page["scope_order"])
		}
	}
	listed := mustCallGatewayTool(t, h.service, h.staff, "list_data_products", map[string]any{})
	encoded, err := json.Marshal(listed["scopes"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"order":["public","internal","secret","confidential","top_secret"]`) {
		t.Fatalf("list_data_products scopes carry no order: %s", encoded)
	}
}

func TestUnorderedCatalogAddsNoScopeClosure(t *testing.T) {
	h := newListTasksHarness(t)
	if h.catalog.ScopeOrder("classification") != nil {
		t.Fatal("the list_my_tasks harness Catalog orders classification")
	}
	created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"confidential"}))
	if _, present := created["scope_closure"]; present {
		t.Fatalf("an unordered Catalog answered scope_closure: %v", created["scope_closure"])
	}
	draft := h.approval.requests[len(h.approval.requests)-1]
	if got, _ := scopeStringValues(draft.Manifest.MandatoryScope["classification"]); !reflect.DeepEqual(got, []string{"confidential"}) {
		t.Fatalf("an unordered Catalog widened classification to %v", got)
	}
	for _, tool := range h.service.ListTools(h.staff) {
		if strings.Contains(tool.Description, "有序维度") {
			t.Fatalf("%s names an ordered scope without one", tool.Name)
		}
	}
	page := mustCallGatewayTool(t, h.service, h.staff, "describe_data_product", map[string]any{"name": "policy_kb_chunks"})
	if _, present := page["scope_order"]; present {
		t.Fatalf("describe_data_product publishes scope_order without one: %v", page["scope_order"])
	}
	listed := mustCallGatewayTool(t, h.service, h.staff, "list_data_products", map[string]any{})
	if encoded, _ := json.Marshal(listed["scopes"]); strings.Contains(string(encoded), `"order"`) {
		t.Fatalf("list_data_products scopes carry an order: %s", encoded)
	}
}

func TestCloseOrderedScopesKeepsTheValueForm(t *testing.T) {
	ordered := &Service{catalog: &catalog.Catalog{Scopes: []catalog.Scope{
		{Name: "kb_department", Type: catalog.ScopeTypeEnum, AllowedValues: []string{"finance", "hr"}},
		{Name: "classification", Type: catalog.ScopeTypeEnum, AllowedValues: []string{"confidential", "internal", "public", "secret", "top_secret"},
			Order: orderedGrades},
	}}}
	// A persisted context decodes enum sets as []any; the form is kept.
	scope := map[string]any{"kb_department": []any{"hr"}, "classification": []any{"secret"}}
	closed, added := ordered.closeOrderedScopes(scope)
	if !reflect.DeepEqual(closed["classification"], []any{"internal", "public", "secret"}) ||
		!reflect.DeepEqual(closed["kb_department"], []any{"hr"}) ||
		!reflect.DeepEqual(added, map[string][]string{"classification": {"public", "internal"}}) {
		t.Fatalf("closed %#v added %#v", closed, added)
	}
	if !reflect.DeepEqual(scope["classification"], []any{"secret"}) {
		t.Fatalf("the input scope was changed: %#v", scope)
	}
	closed, added = ordered.closeOrderedScopes(map[string]any{"classification": []string{"top_secret"}})
	if !reflect.DeepEqual(closed["classification"], []string{"confidential", "internal", "public", "secret", "top_secret"}) ||
		!reflect.DeepEqual(added["classification"], []string{"public", "internal", "secret", "confidential"}) {
		t.Fatalf("closed %#v added %#v", closed, added)
	}
	// Nothing to add, no ordered scope requested, or no order at all: the
	// same scope comes back and nothing is reported.
	unordered := &Service{catalog: &catalog.Catalog{Scopes: []catalog.Scope{ordered.catalog.Scopes[0],
		{Name: "classification", Type: catalog.ScopeTypeEnum, AllowedValues: ordered.catalog.Scopes[1].AllowedValues}}}}
	for label, tc := range map[string]struct {
		service *Service
		scope   map[string]any
	}{
		"already closed": {ordered, map[string]any{"classification": []string{"internal", "public"}}},
		"not requested":  {ordered, map[string]any{"kb_department": []string{"hr"}}},
		"no order":       {unordered, map[string]any{"classification": []string{"top_secret"}}},
	} {
		closed, added := tc.service.closeOrderedScopes(tc.scope)
		if added != nil || !reflect.DeepEqual(closed, tc.scope) {
			t.Fatalf("%s: closed %#v added %#v", label, closed, added)
		}
	}
}
