package oademo

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Agent-submitted drafts (KB1-LOOP-149; the Catalog's delegated_authorization
// agent_submits): TaskGate sends submitted=true, the draft is born pending,
// carries a note saying who it was submitted for and who acts now, offers no
// submission and no "switch and submit", is listed under its approver, and
// is approved directly. No submitted callback is ever sent for it.

// createSubmittedDraft posts a draft with submitted=true and returns its id
// and the state the OA answered.
func createSubmittedDraft(t *testing.T, base string, request DraftRequest) (string, string) {
	t.Helper()
	request.Submitted = true
	body, _ := json.Marshal(request)
	if !strings.Contains(string(body), `"submitted":true`) {
		t.Fatalf("the request does not carry submitted: %s", body)
	}
	post, _ := http.NewRequest(http.MethodPost, base+"/api/drafts", strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer service")
	response, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var created map[string]any
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&created) != nil {
		t.Fatalf("create a submitted draft: status %d", response.StatusCode)
	}
	return created["draft_id"].(string), created["state"].(string)
}

func TestAnAgentSubmittedDraftIsBornPendingAndOnlyApproved(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, true)
	draft, state := createSubmittedDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	if state != "pending" {
		t.Fatalf("an agent-submitted draft was created %q, want pending", state)
	}
	const note = "agent 已代 普通员工（staff） 提交，等待 财务部管理员（finance-admin） 审批"
	staff := login(t, httpServer.URL, "staff", "shared")
	list := get(t, staff, httpServer.URL+"/tasks")
	if !strings.Contains(list, draft) || !strings.Contains(list, "待审批") || !strings.Contains(list, note) {
		t.Fatalf("the requester's to-do list: %s", list)
	}
	page := get(t, staff, httpServer.URL+"/tasks/"+draft)
	if strings.Contains(page, "提交申请") || strings.Contains(page, "并提交<") || strings.Contains(page, `/submit"`) ||
		strings.Contains(page, `name="decision"`) {
		t.Fatalf("the requester's page offers a submission or a decision: %s", page)
	}
	if !strings.Contains(page, note) || !strings.Contains(page, ">切换到 财务部管理员 并审批<") || !strings.Contains(page, "agent 代申请人提交") {
		t.Fatalf("the requester's page does not say who acts now: %s", page)
	}
	// The overview lists it under its approver, never under the requester.
	at := strings.Index(list, "其他身份的待办")
	if at < 0 || !strings.Contains(list[at:], "<h3>财务部管理员（1 张）</h3>") || strings.Contains(list[at:], "<h3>普通员工（") {
		t.Fatalf("the overview does not list the draft under its approver: %s", list)
	}
	// Submitting it again is a conflict, as for any pending draft.
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusConflict {
		t.Fatalf("submitting an agent-submitted draft = %d, want 409", status)
	}
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	if list := get(t, admin, httpServer.URL+"/tasks"); !strings.Contains(list, draft) || !strings.Contains(list, note) {
		t.Fatalf("the approver's to-do list: %s", list)
	}
	page = get(t, admin, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, `name="decision" value="approved"`) || strings.Contains(page, "提交申请") || strings.Contains(page, "切换到 ") {
		t.Fatalf("the approver's page: %s", page)
	}
	if status := act(t, admin, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	if event := gateway.event(t, draft, "finance-admin", "approved"); event.ApprovalReceipt == nil || event.ApprovalReceipt.ApproverID != "finance-admin" {
		t.Fatalf("approval receipt %+v", event.ApprovalReceipt)
	}
	if settled.Note != "" || !settled.AgentSubmitted {
		t.Fatalf("after the approval: note %q, agent_submitted %v", settled.Note, settled.AgentSubmitted)
	}
	gateway.mu.Lock()
	for _, event := range gateway.events {
		if event.Status == "submitted" {
			gateway.mu.Unlock()
			t.Fatalf("a submitted callback was sent for an agent-submitted draft: %+v", event)
		}
	}
	gateway.mu.Unlock()
	for _, viewer := range []*http.Client{staff, admin} {
		history := get(t, viewer, httpServer.URL+"/history")
		if !strings.Contains(history, draft) || !strings.Contains(history, "已批准") || !strings.Contains(history, "agent 代为提交") ||
			!strings.Contains(history, "财务部管理员（finance-admin） 批准（") {
			t.Fatalf("history: %s", history)
		}
	}
}

func TestAnAgentSubmittedSelfConfirmationIsConfirmedDirectly(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, false)
	draft, state := createSubmittedDraft(t, httpServer.URL, staffDraft(t, "staff", "", 24*time.Hour))
	if state != "pending" {
		t.Fatalf("state %q", state)
	}
	staff := login(t, httpServer.URL, "staff", "shared")
	page := get(t, staff, httpServer.URL+"/tasks/"+draft)
	if strings.Contains(page, "提交申请") || !strings.Contains(page, ">确认<") ||
		!strings.Contains(page, "agent 已代 普通员工（staff） 提交，等待本人确认") || !strings.Contains(page, "等待 普通员工（staff）本人确认") {
		t.Fatalf("a self-confirmed agent-submitted draft: %s", page)
	}
	if status := act(t, staff, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("confirm: %d", status)
	}
	awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	gateway.event(t, draft, "staff", "approved")

	member := staffDraft(t, "finance-admin", "", time.Hour)
	member.MemberConfirm = true
	memberDraft, _ := createSubmittedDraft(t, httpServer.URL, member)
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	page = get(t, admin, httpServer.URL+"/tasks/"+memberDraft)
	if strings.Contains(page, "提交申请") || !strings.Contains(page, ">确认<") ||
		!strings.Contains(page, "agent 已代 普通员工（staff） 提交，等待项目成员 财务部管理员（finance-admin） 本人确认") {
		t.Fatalf("a member-confirmed agent-submitted draft: %s", page)
	}
}

// Without submitted the OA is unchanged: the draft waits for its submission.
func TestADraftWithoutSubmittedStillWaitsForItsSubmission(t *testing.T) {
	_, httpServer, _ := delegatedOA(t, false)
	request := staffDraft(t, "finance-admin", "", time.Hour)
	body, _ := json.Marshal(request)
	if strings.Contains(string(body), "submitted") {
		t.Fatalf("an unsubmitted request serializes the field: %s", body)
	}
	draft := createDraft(t, httpServer.URL, request)
	staff := login(t, httpServer.URL, "staff", "shared")
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, "提交申请") || strings.Contains(page, "agent 已代") {
		t.Fatalf("an ordinary draft: %s", page)
	}
}
