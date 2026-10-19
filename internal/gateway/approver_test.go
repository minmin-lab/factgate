package gateway

import (
	"testing"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
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
