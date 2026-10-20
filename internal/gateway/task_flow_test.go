package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
)

// TestTaskFlowSaysWhoActsNextAndRevokeVoidsAPendingTask walks a task through
// the steps the 2026-10-20 feedback found opaque: a new task waits for its
// human requester, then for the approver; a revoked pending task stays void
// even if the OA approves it afterwards.
func TestTaskFlowSaysWhoActsNextAndRevokeVoidsAPendingTask(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{oaPublicBaseURL: "https://oa.test/"})
	created := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":     "summarize travel expenses",
		"data_products": []string{"expense_summary"},
		"columns":       map[string][]string{"expense_summary": {"month", "total_amount"}},
		"scopes":        map[string]any{"department": []any{"销售部"}},
	})
	taskID := created["task_id"].(string)
	if created["next_action"] != "human_submit_in_oa" || created["waiting_for"] != "requester_submission" ||
		created["oa_url"] != "http://oa.test/drafts/draft-1" {
		t.Fatalf("request_data_task flow %v (the OA's own link must win)", created)
	}
	status := mustCallGatewayTool(t, harness.service, harness.alice, "get_task_status", map[string]any{"task_id": taskID})
	if status["next_action"] != "human_submit_in_oa" || status["oa_url"] != "https://oa.test/tasks/draft-1" {
		t.Fatalf("pending status %v", status)
	}
	waited := mustCallGatewayTool(t, harness.service, harness.alice, "wait_for_approval", map[string]any{"task_id": taskID, "timeout_seconds": 1})
	if waited["timed_out"] != true || waited["next_action"] != "human_submit_in_oa" {
		t.Fatalf("wait during submission %v", waited)
	}

	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	draft := harness.approval.requests[0]
	submitted := oaCallbackEvent{
		EventID: "flow-submit", TaskID: taskID, DraftID: task.ApprovalRef, Status: "submitted", Actor: "Alice",
		OccurredAt: harness.clock.value, CatalogVersion: harness.catalog.CatalogVersion,
		CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest,
	}
	if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
		t.Fatalf("submit callback %d %s", response.Code, response.Body.String())
	}
	status = mustCallGatewayTool(t, harness.service, harness.alice, "get_task_status", map[string]any{"task_id": taskID})
	if status["next_action"] != "await_approver_decision" || status["approver"] != draft.Approver || status["oa_url"] == nil {
		t.Fatalf("awaiting-approval status %v (approver %q)", status, draft.Approver)
	}

	revoked := mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{"task_id": taskID, "reason": "superseded by a narrower request"})
	if revoked["terminal_reason"] != control.TerminalRevoked {
		t.Fatalf("revoke of a pending task %v", revoked)
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	approved := signedFinalCallback(t, harness, task, draft, "approved", draft.Approver, core)
	if response := sendGatewayCallback(t, harness, approved, ""); response.Code == http.StatusOK {
		t.Fatalf("an approval after revocation was accepted: %d %s", response.Code, response.Body.String())
	}
	if _, err := harness.store.GetGrant(context.Background(), taskID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an approval after revocation issued a grant: %v", err)
	}
	status = mustCallGatewayTool(t, harness.service, harness.alice, "get_task_status", map[string]any{"task_id": taskID})
	if status["next_action"] != "none_terminal" || status["oa_url"] != nil {
		t.Fatalf("revoked status %v", status)
	}
}
