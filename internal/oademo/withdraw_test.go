package oademo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// Draft withdrawal (KB1-LOOP-137): TaskGate tells the OA that it archived a
// task whose draft still waited -- revoked, or expired while pending. The
// draft becomes withdrawn: off the to-do lists, no submit or decision on its
// page, shown as revoked or expired in the history. The route takes the
// service token; repeating it is idempotent, an unknown draft is 404, and a
// decided draft is left unchanged.

// postWithdrawal posts a withdrawal notice with the given Authorization
// header value ("" sends none) and returns the status and the decoded body.
func postWithdrawal(t *testing.T, base, draftID, authorization string, notice any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(notice)
	request, _ := http.NewRequest(http.MethodPost, base+"/api/drafts/"+draftID+"/withdraw", strings.NewReader(string(body)))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

func storedDraft(server *Server, id string) Draft {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return cloneDraft(server.drafts[id])
}

func TestWithdrawTakesAPendingDraftOffTheToDoListsAndShowsItRevoked(t *testing.T) {
	server, httpServer, _ := delegatedOA(t, true)
	request := staffDraft(t, "finance-admin", "", time.Hour)
	draft, state := createSubmittedDraft(t, httpServer.URL, request)
	if state != "pending" {
		t.Fatalf("state %q", state)
	}
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	notice := approval.DraftWithdrawal{TaskID: request.Manifest.TaskID, DraftID: draft, Reason: approval.WithdrawalRevoked,
		Actor: "staff", OccurredAt: at}

	// The service token is required, exactly as for creating a draft.
	for _, authorization := range []string{"", "Bearer wrong", "service", "Bearer servic", "Bearer service-extra"} {
		if status, _ := postWithdrawal(t, httpServer.URL, draft, authorization, notice); status != http.StatusUnauthorized {
			t.Fatalf("withdrawal with Authorization %q = %d, want 401", authorization, status)
		}
	}
	if got := storedDraft(server, draft); got.State != "pending" || got.Withdrawal != nil {
		t.Fatalf("a refused withdrawal changed the draft: %q %+v", got.State, got.Withdrawal)
	}
	// Malformed notices are refused and change nothing.
	for _, bad := range []any{
		approval.DraftWithdrawal{TaskID: request.Manifest.TaskID, DraftID: "oa_other", Reason: approval.WithdrawalRevoked, Actor: "staff", OccurredAt: at},
		approval.DraftWithdrawal{TaskID: request.Manifest.TaskID, DraftID: draft, Reason: "completed", Actor: "staff", OccurredAt: at},
		approval.DraftWithdrawal{DraftID: draft, Reason: approval.WithdrawalRevoked, Actor: "staff", OccurredAt: at},
		map[string]any{"task_id": request.Manifest.TaskID, "draft_id": draft, "reason": "revoked", "unexpected": true},
	} {
		if status, _ := postWithdrawal(t, httpServer.URL, draft, "Bearer service", bad); status != http.StatusBadRequest {
			t.Fatalf("malformed withdrawal %+v = %d, want 400", bad, status)
		}
	}
	if status, _ := postWithdrawal(t, httpServer.URL, draft, "Bearer service", approval.DraftWithdrawal{TaskID: "task_other", DraftID: draft,
		Reason: approval.WithdrawalRevoked, Actor: "staff", OccurredAt: at}); status != http.StatusConflict {
		t.Fatalf("a withdrawal naming another task = %d, want 409", status)
	}
	if got := storedDraft(server, draft); got.State != "pending" {
		t.Fatalf("a refused withdrawal changed the draft: %q", got.State)
	}

	// Positive controls for the absence checks below: before the withdrawal
	// both to-do lists list the draft and the approver's page offers a decision.
	staff := login(t, httpServer.URL, "staff", "shared")
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	for name, client := range map[string]*http.Client{"staff": staff, "finance-admin": admin} {
		if list := get(t, client, httpServer.URL+"/tasks"); !strings.Contains(list, draft) {
			t.Fatalf("%s's to-do list does not list the pending draft: %s", name, list)
		}
	}
	if page := get(t, admin, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, `name="decision"`) {
		t.Fatalf("the approver's page of the pending draft offers no decision: %s", page)
	}

	status, body := postWithdrawal(t, httpServer.URL, draft, "Bearer service", notice)
	if status != http.StatusOK || body["state"] != "withdrawn" || body["unchanged"] != false || body["draft_id"] != draft {
		t.Fatalf("withdrawal = %d %v", status, body)
	}
	got := storedDraft(server, draft)
	if got.State != "withdrawn" || got.Withdrawal == nil || *got.Withdrawal != notice {
		t.Fatalf("withdrawn draft: state %q withdrawal %+v", got.State, got.Withdrawal)
	}
	const note = "TaskGate 中此任务已由 普通员工（staff） 于 2026-10-28 09:30 UTC 撤销；工单已撤回，无需再提交或审批"
	if got.Note != note {
		t.Fatalf("withdrawn note %q", got.Note)
	}

	for name, client := range map[string]*http.Client{"staff": staff, "finance-admin": admin} {
		if list := get(t, client, httpServer.URL+"/tasks"); strings.Contains(list, draft) {
			t.Fatalf("%s's to-do list still lists the withdrawn draft: %s", name, list)
		}
		history := get(t, client, httpServer.URL+"/history")
		if !strings.Contains(history, draft) || !strings.Contains(history, "<td>已撤销</td>") || !strings.Contains(history, note) {
			t.Fatalf("%s's history does not show the draft revoked: %s", name, history)
		}
		page := get(t, client, httpServer.URL+"/tasks/"+draft)
		if strings.Contains(page, `name="decision"`) || strings.Contains(page, `/submit"`) || strings.Contains(page, "提交申请") ||
			strings.Contains(page, "切换到 ") || strings.Contains(page, `http-equiv="refresh"`) {
			t.Fatalf("%s's page of the withdrawn draft offers an action: %s", name, page)
		}
		if !strings.Contains(page, note) || !strings.Contains(page, `<span class="tag">已撤销</span>`) {
			t.Fatalf("%s's page of the withdrawn draft does not explain it: %s", name, page)
		}
	}
	if status := act(t, admin, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusConflict {
		t.Fatalf("approving a withdrawn draft = %d, want 409", status)
	}

	// Repeating it is idempotent; an unknown draft is 404.
	status, body = postWithdrawal(t, httpServer.URL, draft, "Bearer service", notice)
	if status != http.StatusOK || body["state"] != "withdrawn" || body["unchanged"] != true {
		t.Fatalf("repeated withdrawal = %d %v", status, body)
	}
	if again := storedDraft(server, draft); again.State != "withdrawn" || *again.Withdrawal != notice || again.Note != note {
		t.Fatalf("a repeated withdrawal changed the draft: %+v", again)
	}
	unknown := notice
	unknown.DraftID = "oa_unknown"
	if status, _ := postWithdrawal(t, httpServer.URL, "oa_unknown", "Bearer service", unknown); status != http.StatusNotFound {
		t.Fatalf("withdrawing an unknown draft = %d, want 404", status)
	}
}

func TestWithdrawShowsAnUnsubmittedDraftExpiredAndLeavesADecidedOneUnchanged(t *testing.T) {
	server, httpServer, _ := delegatedOA(t, true)
	request := staffDraft(t, "finance-admin", "", time.Hour)
	unsubmitted := createDraft(t, httpServer.URL, request)
	at := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	status, body := postWithdrawal(t, httpServer.URL, unsubmitted, "Bearer service", approval.DraftWithdrawal{TaskID: request.Manifest.TaskID,
		DraftID: unsubmitted, Reason: approval.WithdrawalExpired, Actor: "system", OccurredAt: at})
	if status != http.StatusOK || body["state"] != "withdrawn" || body["unchanged"] != false {
		t.Fatalf("withdrawing an unsubmitted draft = %d %v", status, body)
	}
	staff := login(t, httpServer.URL, "staff", "shared")
	if list := get(t, staff, httpServer.URL+"/tasks"); strings.Contains(list, unsubmitted) {
		t.Fatalf("the to-do list still lists the expired draft: %s", list)
	}
	if history := get(t, staff, httpServer.URL+"/history"); !strings.Contains(history, "<td>已过期</td>") ||
		!strings.Contains(history, "已于 2026-10-28 10:00 UTC 过期作废") {
		t.Fatalf("history does not show the draft expired: %s", history)
	}
	if status := act(t, staff, httpServer.URL, unsubmitted, "submit", nil); status != http.StatusConflict {
		t.Fatalf("submitting an expired draft = %d, want 409", status)
	}

	// A draft already approved is left as it is.
	decided, _ := createSubmittedDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	if status := act(t, admin, httpServer.URL, decided, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	before := awaitDraft(t, server, decided, func(d Draft) bool { return d.State == "approved" })
	status, body = postWithdrawal(t, httpServer.URL, decided, "Bearer service", approval.DraftWithdrawal{TaskID: request.Manifest.TaskID,
		DraftID: decided, Reason: approval.WithdrawalRevoked, Actor: "staff", OccurredAt: at})
	if status != http.StatusOK || body["state"] != "approved" || body["unchanged"] != true {
		t.Fatalf("withdrawing an approved draft = %d %v", status, body)
	}
	if after := storedDraft(server, decided); after.State != "approved" || after.Withdrawal != nil || after.Note != before.Note ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("withdrawing an approved draft changed it: %+v", after)
	}
}

// The Gateway's client and this OA speak the same withdrawal: the client's
// request withdraws a pending draft, and an unknown draft's 404 is success.
func TestTheGatewayClientWithdrawsADraftOnThisOA(t *testing.T) {
	server, httpServer, _ := delegatedOA(t, false)
	request := staffDraft(t, "finance-admin", "", time.Hour)
	draft, _ := createSubmittedDraft(t, httpServer.URL, request)
	client, err := approval.NewClient(httpServer.URL, "service", nil)
	if err != nil {
		t.Fatal(err)
	}
	notice := approval.DraftWithdrawal{TaskID: request.Manifest.TaskID, DraftID: draft, Reason: approval.WithdrawalExpired,
		Actor: "system", OccurredAt: time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)}
	if err := client.WithdrawDraft(context.Background(), notice); err != nil {
		t.Fatalf("client withdrawal: %v", err)
	}
	if got := storedDraft(server, draft); got.State != "withdrawn" || got.Withdrawal == nil || *got.Withdrawal != notice {
		t.Fatalf("after the client's withdrawal: %q %+v", got.State, got.Withdrawal)
	}
	notice.DraftID = "oa_gone"
	if err := client.WithdrawDraft(context.Background(), notice); err != nil {
		t.Fatalf("an unknown draft's 404 is not success: %v", err)
	}
	wrong, err := approval.NewClient(httpServer.URL, "wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	notice.DraftID = draft
	if err := wrong.WithdrawDraft(context.Background(), notice); err == nil {
		t.Fatal("a wrong service token was accepted")
	}
}
