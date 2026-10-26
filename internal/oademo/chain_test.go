package oademo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// Approval chains (KB1-LOOP-141): TaskGate answers each approval with the next
// approver; the OA reassigns the draft to them, and each approver approves or
// narrows the previous approver's grant re-issued at their own time. Earlier
// approvers may still open the draft but not decide it.

type fakeGateway struct {
	mu      sync.Mutex
	events  []callbackEvent
	answers map[string]fakeAnswer // draft/actor/status -> answer
}

type fakeAnswer struct {
	status int
	body   string
}

func (g *fakeGateway) answer(draft, actor, status string, code int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answers[draft+"/"+actor+"/"+status] = fakeAnswer{code, body}
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var event callbackEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.events = append(g.events, event)
	answer, ok := g.answers[event.DraftID+"/"+event.Actor+"/"+event.Status]
	g.mu.Unlock()
	if !ok {
		answer = fakeAnswer{http.StatusOK, `{"ok":true}`}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	_, _ = w.Write([]byte(answer.body))
}

func (g *fakeGateway) event(t *testing.T, draft, actor, status string) callbackEvent {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, event := range g.events {
		if event.DraftID == draft && event.Actor == actor && event.Status == status {
			return event
		}
	}
	t.Fatalf("no %s callback by %s for %s", status, actor, draft)
	return callbackEvent{}
}

func chainOA(t *testing.T) (*Server, *httptest.Server, *fakeGateway) {
	t.Helper()
	gateway := &fakeGateway{answers: map[string]fakeAnswer{}}
	callback := httptest.NewServer(gateway)
	t.Cleanup(callback.Close)
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, AlicePassword: "alice-pass", BobPassword: "bob-pass", DanaPassword: "dana-pass",
		ExtraUsers: []ExtraUser{{Name: "leader-rd", Role: "approver", Password: "leader-pass", DisplayName: "研发线分管领导"}}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return server, httpServer, gateway
}

func createDraft(t *testing.T, base string, request DraftRequest) string {
	t.Helper()
	body, _ := json.Marshal(request)
	post, _ := http.NewRequest(http.MethodPost, base+"/api/drafts", strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer service")
	response, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var created map[string]any
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&created) != nil {
		t.Fatalf("create draft: status %d", response.StatusCode)
	}
	return created["draft_id"].(string)
}

// awaitDraft waits for the asynchronous callback to settle the draft.
func awaitDraft(t *testing.T, server *Server, id string, settled func(Draft) bool) Draft {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.mu.RLock()
		draft := cloneDraft(server.drafts[id])
		server.mu.RUnlock()
		if settled(draft) {
			return draft
		}
		if time.Now().After(deadline) {
			t.Fatalf("draft %s did not settle: state %q approver %q note %q", id, draft.State, draft.Approver, draft.Note)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApprovalChainReassignsTheDraftAndContinuesFromThePreviousGrant(t *testing.T) {
	server, httpServer, gateway := chainOA(t)
	request := testDraftRequest(t)
	draft := createDraft(t, httpServer.URL, request)
	gateway.answer(draft, "bob", "approved", http.StatusOK,
		`{"ok":true,"granted":false,"pending":true,"next_approver":"dana","next_role":"department_director","remaining_steps":2}`)
	gateway.answer(draft, "dana", "narrowed", http.StatusOK,
		`{"ok":true,"granted":false,"pending":true,"next_approver":"leader-rd","next_role":"leader","next_role_label":"分管领导（研发线）","remaining_steps":1}`)

	alice := login(t, httpServer.URL, "alice", "alice-pass")
	if status := act(t, alice, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	bob := login(t, httpServer.URL, "bob", "bob-pass")
	if status := decide(t, bob, httpServer.URL, draft, server); status != http.StatusOK {
		t.Fatalf("bob approve: %d", status)
	}
	settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.Approver == "dana" })
	if settled.State != "pending" || !strings.Contains(settled.Note, "dana") || !strings.Contains(settled.Note, "被申请部门审批人") {
		t.Fatalf("after bob: state %q note %q", settled.State, settled.Note)
	}
	if status, _ := getStatus(t, bob, httpServer.URL+"/tasks/"+draft); status != http.StatusOK {
		t.Fatalf("bob can no longer open the draft he approved: %d", status)
	}
	if list := get(t, bob, httpServer.URL+"/tasks"); !strings.Contains(list, draft) {
		t.Fatal("bob's list dropped the draft he approved")
	}
	if status := decide(t, bob, httpServer.URL, draft, server); status != http.StatusForbidden {
		t.Fatalf("bob deciding again after reassignment: %d, want 403", status)
	}

	dana := login(t, httpServer.URL, "dana", "dana-pass")
	page := get(t, dana, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, "上一级批准的范围") || !strings.Contains(page, "bob 批准") {
		t.Fatal("dana's page does not show the previous grant and the history")
	}
	columns, _ := json.Marshal(request.Manifest.ApprovedColumns)
	narrow := url.Values{"decision": {"narrow"}, "approved_products": {`["expense_summary"]`}, "approved_columns": {string(columns)},
		"mandatory_scope": {`{"department":["销售部"]}`}, "max_queries": {"5"}, "max_result_rows": {"500"}, "max_db_ms": {"30000"},
		"per_query_timeout_ms": {"5000"}, "task_ttl_ms": {"1800000"}}
	if status := act(t, dana, httpServer.URL, draft, "decision", narrow); status != http.StatusOK {
		t.Fatalf("dana narrow: %d", status)
	}
	if reassigned := awaitDraft(t, server, draft, func(d Draft) bool { return d.Approver == "leader-rd" && d.State == "pending" }); !strings.Contains(reassigned.Note, "分管领导（研发线）") {
		t.Fatalf("the gateway's next_role_label was not shown: note %q", reassigned.Note)
	}

	leader := login(t, httpServer.URL, "leader-rd", "leader-pass")
	page = get(t, leader, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, `name="max_queries" type="number" min="1" max="5" value="5"`) ||
		!strings.Contains(page, "[&#34;销售部&#34;]") || strings.Contains(page, "市场部&#34;") {
		t.Fatal("the leader's narrow form is not prefilled from dana's narrowed grant")
	}
	if !strings.Contains(page, "研发线分管领导") {
		t.Fatal("the leader's display name is not shown")
	}
	if status := decide(t, leader, httpServer.URL, draft, server); status != http.StatusOK {
		t.Fatalf("leader approve: %d", status)
	}
	final := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" && d.Approver == "leader-rd" })
	if len(final.History) != 3 || final.History[1].Decision != "narrowed" {
		t.Fatalf("history %+v", final.History)
	}

	// What TaskGate checks: each step's grant is the previous step's grant
	// re-issued at the step's receipt time (approve), or a narrowing of it.
	bobEvent := gateway.event(t, draft, "bob", "approved")
	danaEvent := gateway.event(t, draft, "dana", "narrowed")
	leaderEvent := gateway.event(t, draft, "leader-rd", "approved")
	danaParent := bobEvent.ApprovedGrant.Reissued(danaEvent.ApprovalReceipt.IssuedAt)
	if err := danaParent.CheckNarrowing(*danaEvent.ApprovedGrant); err != nil {
		t.Fatalf("dana's grant does not narrow bob's: %v", err)
	}
	want, _ := approval.GrantCoreDigest(danaEvent.ApprovedGrant.Reissued(leaderEvent.ApprovalReceipt.IssuedAt))
	got, _ := approval.GrantCoreDigest(*leaderEvent.ApprovedGrant)
	if got != want || leaderEvent.ApprovalReceipt.ApprovedGrantDigest != got {
		t.Fatal("the leader's approval is not dana's grant re-issued, or its receipt does not bind it")
	}
	if leaderEvent.ApprovedGrant.Budget.MaxQueries != 5 {
		t.Fatal("the leader's approval lost dana's narrowing")
	}
}

func TestApprovalAnswersWithoutAGrantAreShownOnTheDraft(t *testing.T) {
	server, httpServer, gateway := chainOA(t)
	refused := createDraft(t, httpServer.URL, testDraftRequest(t))
	gateway.answer(refused, "bob", "approved", http.StatusOK, `{"ok":true,"granted":false,"terminal_reason":"approver_not_authorized"}`)
	failing := createDraft(t, httpServer.URL, testDraftRequest(t))
	gateway.answer(failing, "bob", "approved", http.StatusConflict, `{"error":"callback actor is not allowed"}`)
	alice := login(t, httpServer.URL, "alice", "alice-pass")
	bob := login(t, httpServer.URL, "bob", "bob-pass")
	for _, draft := range []string{refused, failing} {
		if status := act(t, alice, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
			t.Fatalf("submit: %d", status)
		}
		if status := decide(t, bob, httpServer.URL, draft, server); status != http.StatusOK {
			t.Fatalf("approve: %d", status)
		}
	}
	if settled := awaitDraft(t, server, refused, func(d Draft) bool { return d.State != "approved" }); settled.State != "not_granted" ||
		!strings.Contains(settled.Note, "无权") {
		t.Fatalf("a refused approval: state %q note %q", settled.State, settled.Note)
	}
	if settled := awaitDraft(t, server, failing, func(d Draft) bool { return d.Note != "" }); settled.State != "approved" ||
		!strings.Contains(settled.Note, "callback actor is not allowed") {
		t.Fatalf("a failed callback: state %q note %q", settled.State, settled.Note)
	}
}
