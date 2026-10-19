package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

// Approver authority: the requested classification chooses the approval route
// (leadership -> dana, otherwise bob), and a route with verify_approver lets
// only an approver who is eligible for the whole request approve it. The
// requester never approves their own task. Every refusal happens before an
// OA draft exists.

func approverSnapshot(t *testing.T, subjects string) *principalattr.Snapshot {
	t.Helper()
	snapshot, err := principalattr.Parse([]byte("version: taskgate-principal-attributes-v1\nsubjects:\n" + subjects))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

const (
	aliceSubject = "  - subject: alice\n    attributes:\n      department: [finance]\n      job_family: [director, finance_controller]\n      clearance: [public, internal, confidential, leadership]\n"
	bobSubject   = "  - subject: bob\n    attributes:\n      department: [finance]\n      job_family: [manager, finance_controller]\n      clearance: [public, internal, confidential]\n"
	danaSubject  = "  - subject: dana\n    attributes:\n      department: [finance]\n      job_family: [executive, finance_controller]\n      clearance: [public, internal, confidential, leadership]\n"
)

// routedCatalog rewrites the knowledge catalog's high-sensitivity routes: the
// unconditioned route to fallback and a leadership route to leadership, both
// verifying their approver. It edits the YAML node tree so every other scalar
// (digests included) keeps its exact form.
func routedCatalog(t *testing.T, fallback, leadership string) func(string) string {
	t.Helper()
	knowledge := kbCatalogRewrite(t)
	scalar := func(value string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: value} }
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
			routes := root.Content[index+1]
			for _, route := range routes.Content {
				fields := map[string]*yaml.Node{}
				for field := 0; field+1 < len(route.Content); field += 2 {
					fields[route.Content[field].Value] = route.Content[field+1]
				}
				if fields["sensitivity"] != nil && fields["sensitivity"].Value == "high" && fields["products"] == nil {
					fields["approver"].Value = fallback
					route.Content = append(route.Content, scalar("verify_approver"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
				}
			}
			routes.Content = append(routes.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				scalar("sensitivity"), scalar("high"), scalar("mode"), scalar("manual"), scalar("approver"), scalar(leadership),
				scalar("budget_profile"), scalar("detail-manual-v5"),
				scalar("verify_approver"), {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
				scalar("scope_condition"), {Kind: yaml.MappingNode, Content: []*yaml.Node{
					scalar("classification"), {Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar("leadership")}},
				}},
			}})
		}
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

func TestClassificationChoosesTheApproverAndTheApproverMustBeEligible(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: routedCatalog(t, "bob", "dana"),
		principalAttributes: approverSnapshot(t, aliceSubject+bobSubject+danaSubject)})
	if _, err := callGatewayTool(harness.service, harness.alice, "request_data_task", kbTaskRequest("finance", []string{"internal", "public"})); err != nil {
		t.Fatalf("non-leadership request: %v", err)
	}
	if _, err := callGatewayTool(harness.service, harness.alice, "request_data_task", kbTaskRequest("finance", []string{"internal", "leadership"})); err != nil {
		t.Fatalf("leadership request: %v", err)
	}
	if len(harness.approval.requests) != 2 {
		t.Fatalf("drafts = %d, want 2", len(harness.approval.requests))
	}
	if got := harness.approval.requests[0].Approver; got != "bob" {
		t.Fatalf("non-leadership request routed to %q, want bob", got)
	}
	if got := harness.approval.requests[1].Approver; got != "dana" {
		t.Fatalf("leadership request routed to %q, want dana", got)
	}
}

func TestApproverWithoutTheClearanceIsRefusedBeforeAnyDraft(t *testing.T) {
	for name, tc := range map[string]struct {
		fallback, leadership string
		subjects             string
		classification       []string
	}{
		// A catalog that routes leadership to bob, who lacks leadership clearance.
		"misrouted leadership": {"bob", "bob", aliceSubject + bobSubject, []string{"leadership"}},
		// The requester is the route's approver.
		"self approval": {"alice", "dana", aliceSubject + danaSubject, []string{"internal"}},
		// A verifying route whose approver the snapshot does not know.
		"unknown approver": {"bob", "dana", aliceSubject + bobSubject, []string{"leadership"}},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: routedCatalog(t, tc.fallback, tc.leadership),
				principalAttributes: approverSnapshot(t, tc.subjects)})
			_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", kbTaskRequest("finance", tc.classification))
			requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
			if len(harness.approval.requests) != 0 {
				t.Fatalf("a refused request produced %d OA drafts", len(harness.approval.requests))
			}
		})
	}
}

// The positive control of the misrouted case: the same catalog admits the
// leadership request once bob holds leadership clearance, so the refusal above
// comes from the approver's envelope and nothing else.
func TestApproverWithTheClearanceIsAdmitted(t *testing.T) {
	cleared := "  - subject: bob\n    attributes:\n      department: [finance]\n      job_family: [manager, finance_controller]\n      clearance: [public, internal, confidential, leadership]\n"
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: routedCatalog(t, "bob", "bob"),
		principalAttributes: approverSnapshot(t, aliceSubject+cleared)})
	if _, err := callGatewayTool(harness.service, harness.alice, "request_data_task", kbTaskRequest("finance", []string{"leadership"})); err != nil {
		t.Fatalf("a cleared approver was refused: %v", err)
	}
	if len(harness.approval.requests) != 1 || harness.approval.requests[0].Approver != "bob" {
		t.Fatalf("drafts %+v", harness.approval.requests)
	}
}

// A requester learns the routing before asking and the resolved approver after:
// list_data_products publishes the routes, and the request result names the
// approver the scope routed to.
func TestRoutingIsPublishedAndTheResolvedApproverReturned(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: routedCatalog(t, "bob", "dana"),
		principalAttributes: approverSnapshot(t, aliceSubject+bobSubject+danaSubject)})
	listing := mustCallGatewayTool(t, harness.service, harness.alice, "list_data_products", map[string]any{})
	routes, _ := listing["approval_routes"].([]map[string]any)
	conditioned := 0
	for _, route := range routes {
		if route["scope_condition"] != nil {
			conditioned++
			if route["approver"] != "dana" {
				t.Fatalf("the published leadership route names %v", route["approver"])
			}
		}
	}
	if conditioned != 1 || listing["approval_routing"] == "" {
		t.Fatalf("published routing: %d conditioned routes, note %q", conditioned, listing["approval_routing"])
	}
	for classification, approver := range map[string]string{"internal": "bob", "leadership": "dana"} {
		result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", kbTaskRequest("finance", []string{classification}))
		if result["approver"] != approver {
			t.Fatalf("%s request returned approver %v, want %s", classification, result["approver"], approver)
		}
	}
}

// The requester may name any approver (as a misrouted approval flow would);
// the approval then runs, and the data products' rules decide at grant time:
// bob approving a leadership task archives it with approver_not_authorized and
// issues no grant; dana's approval grants; bob narrowing leadership away makes
// his approval good; naming oneself is refused at once.
func TestChosenApproverIsVerifiedWhenTheApprovalArrives(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: routedCatalog(t, "bob", "dana"),
		principalAttributes: approverSnapshot(t, aliceSubject+bobSubject+danaSubject)})
	request := func(approver string, classification []string) (map[string]any, control.Task, approval.DraftRequest) {
		t.Helper()
		args := kbTaskRequest("finance", classification)
		args["approver"] = approver
		result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", args)
		task, err := harness.store.GetTask(context.Background(), result["task_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		draft := harness.approval.requests[len(harness.approval.requests)-1]
		submitted := oaCallbackEvent{EventID: "submit-" + task.ID, TaskID: task.ID, DraftID: task.ApprovalRef, Status: "submitted",
			Actor: harness.alice.Subject, OccurredAt: harness.clock.value, CatalogVersion: harness.catalog.CatalogVersion,
			CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest}
		if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
			t.Fatalf("submit callback = %d %s", response.Code, response.Body.String())
		}
		return result, task, draft
	}
	core := func(draft approval.DraftRequest) approval.TaskGrantCoreV1 {
		t.Helper()
		value, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	state := func(task control.Task) control.Task {
		t.Helper()
		current, err := harness.store.GetTask(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}

	// bob chosen for leadership: the request goes ahead, flagged unauthorized.
	result, task, draft := request("bob", []string{"internal", "leadership"})
	if draft.Approver != "bob" || result["approver_chosen_by_requester"] != true || *(result["approver_authorized"].(*bool)) {
		t.Fatalf("chosen bob: draft approver %q, result %v / %v", draft.Approver, result["approver_chosen_by_requester"], result["approver_authorized"])
	}
	response := sendGatewayCallback(t, harness, signedFinalCallback(t, harness, task, draft, "approved", "bob", core(draft)), "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "approver_not_authorized") {
		t.Fatalf("bob's approval callback = %d %s", response.Code, response.Body.String())
	}
	if current := state(task); current.State != control.TaskArchived || current.TerminalReason != control.TerminalApproverNotAuthorized {
		t.Fatalf("after bob's approval: %s / %s", current.State, current.TerminalReason)
	}
	if _, err := harness.store.GetGrant(context.Background(), task.ID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("bob's approval issued a grant: %v", err)
	}

	// dana chosen for leadership: granted.
	_, task, draft = request("dana", []string{"internal", "leadership"})
	if response := sendGatewayCallback(t, harness, signedFinalCallback(t, harness, task, draft, "approved", "dana", core(draft)), ""); response.Code != http.StatusOK {
		t.Fatalf("dana's approval callback = %d %s", response.Code, response.Body.String())
	}
	if current := state(task); current.State != control.TaskActive {
		t.Fatalf("after dana's approval: %s / %s", current.State, current.TerminalReason)
	}

	// bob narrows leadership away: his approval of what remains is good.
	_, task, draft = request("bob", []string{"internal", "leadership"})
	narrowed := core(draft)
	narrowed.MandatoryScope["classification"] = []any{"internal"}
	if response := sendGatewayCallback(t, harness, signedFinalCallback(t, harness, task, draft, "narrowed", "bob", narrowed), ""); response.Code != http.StatusOK {
		t.Fatalf("bob's narrowing callback = %d %s", response.Code, response.Body.String())
	}
	if current := state(task); current.State != control.TaskActive {
		t.Fatalf("after bob narrowed leadership away: %s / %s", current.State, current.TerminalReason)
	}

	// Naming oneself is refused before any draft.
	before := len(harness.approval.requests)
	args := kbTaskRequest("finance", []string{"internal"})
	args["approver"] = "alice"
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", args)
	requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	if len(harness.approval.requests) != before {
		t.Fatal("a self-approval request produced an OA draft")
	}
}
