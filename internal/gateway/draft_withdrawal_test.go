package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// OA reconciliation (KB1-LOOP-137; the delegated-authorization demo, not a
// paper claim): under the Catalog's delegated_authorization block a revoked
// task -- and each descendant its revocation cascades to -- and a task past
// the block's pending_ttl are reported to the OA, which withdraws the draft.
// Without the block nothing is sent and pending tasks never expire; a failed
// notice never fails the revocation.

// withdrawingApproval is the harness's fake OA with the optional withdrawal
// capability; it records every withdrawal and answers with err.
type withdrawingApproval struct {
	*fakeApproval
	mu          sync.Mutex
	withdrawals []approval.DraftWithdrawal
	err         error
}

func (adapter *withdrawingApproval) WithdrawDraft(_ context.Context, withdrawal approval.DraftWithdrawal) error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.withdrawals = append(adapter.withdrawals, withdrawal)
	return adapter.err
}

func (adapter *withdrawingApproval) recorded() []approval.DraftWithdrawal {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return append([]approval.DraftWithdrawal(nil), adapter.withdrawals...)
}

// installWithdrawer gives the harness's service the withdrawing fake OA; the
// drafts it creates are still the harness's fakeApproval's.
func installWithdrawer(harness *gatewayHarness) *withdrawingApproval {
	adapter := &withdrawingApproval{fakeApproval: harness.approval}
	harness.service.approval = adapter
	return adapter
}

// withPendingTTL adds pending_ttl to a rewritten Catalog's
// delegated_authorization block.
func withPendingTTL(t *testing.T, rewrite func(string) string, ttl string) func(string) string {
	t.Helper()
	return func(base string) string {
		var document yaml.Node
		if err := yaml.Unmarshal([]byte(rewrite(base)), &document); err != nil {
			t.Fatal(err)
		}
		block := yamlField(document.Content[0], "delegated_authorization")
		if block == nil || block.Kind != yaml.MappingNode {
			t.Fatal("the rewritten Catalog has no delegated_authorization block")
		}
		block.Content = append(block.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "pending_ttl"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ttl})
		encoded, err := yaml.Marshal(&document)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
}

// newWithdrawalHarness is the agent-submits issue harness (tasks are created
// AWAITING_APPROVAL) with pending_ttl when given, and the withdrawing fake OA.
func newWithdrawalHarness(t *testing.T, pendingTTL string) (*issueHarness, *withdrawingApproval) {
	t.Helper()
	rewrite := withAgentSubmits(t, delegatedCatalog(chainCatalogWith(t, issueChainRouteYAML)))
	if pendingTTL != "" {
		rewrite = withPendingTTL(t, rewrite, pendingTTL)
	}
	base := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: rewrite, principalAttributes: approverSnapshot(t, issueSubjects)})
	if base.catalog.DelegatedAuthorization == nil || base.catalog.DelegatedAuthorization.PendingTTL != pendingTTL {
		t.Fatalf("the harness Catalog's delegated_authorization: %+v", base.catalog.DelegatedAuthorization)
	}
	base.approval.response.State = "pending"
	staff := mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"}
	if err := base.store.CreatePrincipal(context.Background(), control.Principal{ID: staff.ID, Subject: staff.Subject, Role: staff.Role, CreatedAt: base.clock.value}); err != nil {
		t.Fatal(err)
	}
	h := &issueHarness{gatewayHarness: base, t: t, staff: staff}
	return h, installWithdrawer(base)
}

// requestTask files a finance/internal task for staff (a child when parent is
// set) and returns it with its draft request.
func (h *issueHarness) requestTask(parent string) (control.Task, approval.DraftRequest) {
	h.t.Helper()
	args := kbTaskRequest("finance", []string{"internal"})
	if parent != "" {
		args["parent_task_id"] = parent
	}
	created := mustCallGatewayTool(h.t, h.service, h.staff, "request_data_task", args)
	return h.task(created["task_id"].(string)), h.approval.requests[len(h.approval.requests)-1]
}

func TestRevokeWithdrawsThePendingTasksDraft(t *testing.T) {
	h, oa := newWithdrawalHarness(t, "")
	task, _ := h.requestTask("")
	if task.State != control.TaskAwaitingApproval || task.ApprovalRef == "" {
		t.Fatalf("the requested task is %s with draft %q", task.State, task.ApprovalRef)
	}
	if got := oa.recorded(); len(got) != 0 {
		t.Fatalf("withdrawals before the revocation: %+v", got)
	}
	revoked := mustCallGatewayTool(t, h.service, h.staff, "revoke_task", map[string]any{"task_id": task.ID, "reason": "不再需要"})
	archived := h.task(task.ID)
	if revoked["state"] != control.TaskArchived || archived.TerminalReason != control.TerminalRevoked {
		t.Fatalf("revocation %v, stored %s %s", revoked, archived.State, archived.TerminalReason)
	}
	want := approval.DraftWithdrawal{TaskID: task.ID, DraftID: task.ApprovalRef, Reason: approval.WithdrawalRevoked, Actor: "staff",
		OccurredAt: archived.UpdatedAt.UTC()}
	if got := oa.recorded(); len(got) != 1 || got[0] != want {
		t.Fatalf("withdrawals %+v, want [%+v]", got, want)
	}
}

func TestRevokingARootWithdrawsItsPendingChildsDraft(t *testing.T) {
	h, oa := newWithdrawalHarness(t, "")
	root, rootDraft := h.requestTask("")
	if code := h.callback(root, rootDraft, "approved", "alice"); code != http.StatusOK || h.task(root.ID).State != control.TaskActive {
		t.Fatalf("approving the root = %d, state %s", code, h.task(root.ID).State)
	}
	child, _ := h.requestTask(root.ID)
	if child.State != control.TaskAwaitingApproval || child.RootTaskID != root.ID || child.ApprovalRef == "" || child.ApprovalRef == root.ApprovalRef {
		t.Fatalf("child %s root %q draft %q (root draft %q)", child.State, child.RootTaskID, child.ApprovalRef, root.ApprovalRef)
	}
	revoked := mustCallGatewayTool(t, h.service, h.staff, "revoke_task", map[string]any{"task_id": root.ID})
	if cascaded, _ := revoked["cascaded_task_ids"].([]string); len(cascaded) != 1 || cascaded[0] != child.ID {
		t.Fatalf("cascaded %v", revoked["cascaded_task_ids"])
	}
	got := oa.recorded()
	byDraft := map[string]approval.DraftWithdrawal{}
	for _, withdrawal := range got {
		byDraft[withdrawal.DraftID] = withdrawal
	}
	childWithdrawal, ok := byDraft[child.ApprovalRef]
	if !ok || childWithdrawal.TaskID != child.ID || childWithdrawal.Reason != approval.WithdrawalRevoked || childWithdrawal.Actor != "staff" ||
		!childWithdrawal.OccurredAt.Equal(h.task(child.ID).UpdatedAt) {
		t.Fatalf("the child's draft was not withdrawn: %+v", got)
	}
	// The root is notified too; the OA leaves its approved draft unchanged.
	if rootWithdrawal, ok := byDraft[root.ApprovalRef]; !ok || rootWithdrawal.TaskID != root.ID || len(got) != 2 {
		t.Fatalf("withdrawals %+v, want the root's and the child's", got)
	}
}

// normalizedRevocation is a revoke_task response with its task id replaced,
// so two harnesses' responses compare byte for byte.
func normalizedRevocation(t *testing.T, result map[string]any, taskID string) string {
	t.Helper()
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(encoded), taskID, "TASK")
}

func TestRevokeSendsNoWithdrawalWithoutTheCatalogBlock(t *testing.T) {
	revoke := func(withdrawer bool) (string, *withdrawingApproval) {
		h := newIssueHarness(t, false)
		if h.catalog.DelegatedAuthorization != nil {
			t.Fatal("the harness Catalog declares delegated_authorization")
		}
		var oa *withdrawingApproval
		if withdrawer {
			oa = installWithdrawer(h.gatewayHarness)
		}
		created := mustCallGatewayTool(t, h.service, h.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
		taskID := created["task_id"].(string)
		if task := h.task(taskID); task.State != control.TaskAwaitingSubmission || task.ApprovalRef == "" {
			t.Fatalf("task %s draft %q", task.State, task.ApprovalRef)
		}
		revoked := mustCallGatewayTool(t, h.service, h.staff, "revoke_task", map[string]any{"task_id": taskID, "reason": "测试撤销"})
		return normalizedRevocation(t, revoked, taskID), oa
	}
	plain, _ := revoke(false)
	withWithdrawer, oa := revoke(true)
	if got := oa.recorded(); len(got) != 0 {
		t.Fatalf("withdrawals without the Catalog block: %+v", got)
	}
	if plain != withWithdrawer {
		t.Fatalf("the revocation differs with a withdrawing OA:\n%s\n%s", plain, withWithdrawer)
	}
	if !strings.Contains(plain, `"terminal_reason":"revoked"`) || !strings.Contains(plain, `"cascaded_task_ids":[]`) {
		t.Fatalf("unexpected revocation %s", plain)
	}
}

func TestRevokeSucceedsWhenTheWithdrawalFails(t *testing.T) {
	h, oa := newWithdrawalHarness(t, "")
	oa.err = errors.New("OA unavailable")
	task, _ := h.requestTask("")
	revoked, err := callGatewayTool(h.service, h.staff, "revoke_task", map[string]any{"task_id": task.ID})
	if err != nil || revoked["state"] != control.TaskArchived || h.task(task.ID).State != control.TaskArchived {
		t.Fatalf("revocation with a failing OA: %v %v", revoked, err)
	}
	if got := oa.recorded(); len(got) != 1 || got[0].DraftID != task.ApprovalRef {
		t.Fatalf("withdrawal attempts %+v", got)
	}
}

func TestSweepPendingExpiryArchivesTasksPastPendingTTL(t *testing.T) {
	h, oa := newWithdrawalHarness(t, "1h")
	ctx := context.Background()
	start := h.clock.value
	old, _ := h.requestTask("") // created 2h before the sweep
	active, activeDraft := h.requestTask("")
	if code := h.callback(active, activeDraft, "approved", "alice"); code != http.StatusOK || h.task(active.ID).State != control.TaskActive {
		t.Fatalf("approving = %d, state %s", code, h.task(active.ID).State)
	}
	unsubmitted := control.Task{ID: "task_awaiting_submission_seed", PrincipalID: h.staff.ID, Objective: "seeded unsubmitted task",
		State: control.TaskAwaitingSubmission, CatalogVersion: h.catalog.CatalogVersion, Sensitivity: "internal",
		RequestContext: json.RawMessage(`{}`), ApprovalRef: "draft-seeded", CreatedAt: start, UpdatedAt: start}
	if err := h.store.CreateTask(ctx, unsubmitted); err != nil {
		t.Fatal(err)
	}
	h.clock.value = start.Add(time.Hour)
	boundary, _ := h.requestTask("") // exactly pending_ttl old at the sweep
	h.clock.value = start.Add(110 * time.Minute)
	fresh, _ := h.requestTask("") // created 10 minutes before the sweep
	now := start.Add(2 * time.Hour)
	h.clock.value = now

	archived, err := h.service.SweepPendingExpiry(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{old.ID: true, boundary.ID: true, unsubmitted.ID: true}
	if len(archived) != len(want) {
		t.Fatalf("archived %v, want %v", archived, want)
	}
	for _, id := range archived {
		if !want[id] {
			t.Fatalf("archived %v, want %v", archived, want)
		}
		task := h.task(id)
		if task.State != control.TaskArchived || task.TerminalReason != control.TerminalExpired {
			t.Fatalf("task %s is %s %s", id, task.State, task.TerminalReason)
		}
		events, err := h.store.ListAuditEvents(ctx, control.AuditFilter{TaskID: id, EventType: "TASK_STATE_CHANGED"})
		if err != nil || len(events) == 0 {
			t.Fatalf("state change events %v %v", events, err)
		}
		last := events[len(events)-1]
		var payload struct {
			To     control.TaskState      `json:"to"`
			Reason control.TerminalReason `json:"terminal_reason"`
			Detail map[string]string      `json:"detail"`
		}
		if err := json.Unmarshal(last.Payload, &payload); err != nil || last.Actor != "system" || payload.To != control.TaskArchived ||
			payload.Reason != control.TerminalExpired || payload.Detail["stage"] != "pending" || payload.Detail["pending_ttl"] != "1h" {
			t.Fatalf("expiry audit event of %s: actor %q payload %s (%v)", id, last.Actor, last.Payload, err)
		}
	}
	if task := h.task(fresh.ID); task.State != control.TaskAwaitingApproval {
		t.Fatalf("the 10-minute-old task is %s", task.State)
	}
	if task := h.task(active.ID); task.State != control.TaskActive {
		t.Fatalf("the active task is %s", task.State)
	}
	got := oa.recorded()
	if len(got) != len(want) {
		t.Fatalf("withdrawals %+v, want one per archived task", got)
	}
	for _, withdrawal := range got {
		task := h.task(withdrawal.TaskID)
		if !want[withdrawal.TaskID] || withdrawal.DraftID != task.ApprovalRef || withdrawal.Reason != approval.WithdrawalExpired ||
			withdrawal.Actor != "system" || !withdrawal.OccurredAt.Equal(task.UpdatedAt) {
			t.Fatalf("withdrawal %+v of task %s (draft %q)", withdrawal, task.ID, task.ApprovalRef)
		}
	}
	// A second sweep finds nothing left to expire.
	if again, err := h.service.SweepPendingExpiry(ctx, now); err != nil || len(again) != 0 || len(oa.recorded()) != len(want) {
		t.Fatalf("second sweep %v %v, withdrawals %d", again, err, len(oa.recorded()))
	}
}

func TestSweepPendingExpiryDoesNothingWithoutPendingTTL(t *testing.T) {
	ctx := context.Background()
	h, oa := newWithdrawalHarness(t, "")
	task, _ := h.requestTask("")
	later := h.clock.value.Add(10000 * time.Hour)
	if archived, err := h.service.SweepPendingExpiry(ctx, later); err != nil || len(archived) != 0 {
		t.Fatalf("sweep without pending_ttl: %v %v", archived, err)
	}
	if h.task(task.ID).State != control.TaskAwaitingApproval || len(oa.recorded()) != 0 {
		t.Fatalf("sweep without pending_ttl changed %s or withdrew %+v", h.task(task.ID).State, oa.recorded())
	}

	plain := newIssueHarness(t, false)
	created := mustCallGatewayTool(t, plain.service, plain.staff, "request_data_task", kbTaskRequest("finance", []string{"internal"}))
	if archived, err := plain.service.SweepPendingExpiry(ctx, later); err != nil || len(archived) != 0 {
		t.Fatalf("sweep without the Catalog block: %v %v", archived, err)
	}
	if state := plain.task(created["task_id"].(string)).State; state != control.TaskAwaitingSubmission {
		t.Fatalf("sweep without the Catalog block changed the task to %s", state)
	}
	var none *Service
	if archived, err := none.SweepPendingExpiry(ctx, later); err != nil || archived != nil {
		t.Fatalf("sweep of a nil service: %v %v", archived, err)
	}
}
