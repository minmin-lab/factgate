package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
)

// Child self-confirmation (KB1-LOOP-147; the delegated-authorization demo,
// not a paper claim). With the Catalog's delegated_authorization block, a
// child task is confirmed by the tree's own human subject; the root is still
// approved by someone else, and a Catalog without the block refuses a
// requester as approver of anything.

const delegatedAuthorizationYAML = "\ndelegated_authorization:\n  issuer_role: {attribute: job_family, any_of: [dept_admin]}\n  child_self_confirm: true\n"

// delegatedCatalog appends the delegated-authorization block to a rewritten
// catalog.
func delegatedCatalog(base func(string) string) func(string) string {
	return func(text string) string { return base(text) + delegatedAuthorizationYAML }
}

type selfConfirmHarness struct {
	*gatewayHarness
	t *testing.T
}

func newSelfConfirmHarness(t *testing.T, delegated bool) *selfConfirmHarness {
	t.Helper()
	rewrite := routedCatalog(t, "bob", "dana")
	if delegated {
		rewrite = delegatedCatalog(rewrite)
	}
	return &selfConfirmHarness{t: t, gatewayHarness: newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: rewrite,
		principalAttributes: approverSnapshot(t, aliceSubject+bobSubject+danaSubject)})}
}

// request files a task (a child of parent when parent is set) and submits it.
func (h *selfConfirmHarness) request(parent, approver string) (map[string]any, control.Task, approval.DraftRequest, error) {
	h.t.Helper()
	args := kbTaskRequest("finance", []string{"internal"})
	if parent != "" {
		args["parent_task_id"] = parent
	}
	if approver != "" {
		args["approver"] = approver
	}
	result, err := callGatewayTool(h.service, h.alice, "request_data_task", args)
	if err != nil {
		return nil, control.Task{}, approval.DraftRequest{}, err
	}
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
	return result, task, draft, nil
}

func (h *selfConfirmHarness) approve(task control.Task, draft approval.DraftRequest, actor string) int {
	h.t.Helper()
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, h.clock.value)
	if err != nil {
		h.t.Fatal(err)
	}
	return sendGatewayCallback(h.t, h.gatewayHarness, signedFinalCallback(h.t, h.gatewayHarness, task, draft, "approved", actor, core), "").Code
}

func (h *selfConfirmHarness) state(taskID string) control.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), taskID)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func TestChildTaskIsConfirmedByItsOwnRequesterUnderDelegatedAuthorization(t *testing.T) {
	h := newSelfConfirmHarness(t, true)
	// The root is approved by bob, as before: naming oneself is still refused.
	if _, _, _, err := h.request("", "alice"); err == nil {
		t.Fatal("a root task approved by its requester was accepted")
	} else {
		requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	}
	rootResult, root, rootDraft, err := h.request("", "")
	if err != nil || rootResult["self_confirm"] != nil || rootDraft.Approver != "bob" {
		t.Fatalf("root %v %v approver %q", err, rootResult["self_confirm"], rootDraft.Approver)
	}
	if code := h.approve(root, rootDraft, "bob"); code != http.StatusOK || h.state(root.ID).State != control.TaskActive {
		t.Fatalf("root approval = %d, state %s", code, h.state(root.ID).State)
	}

	// The child names its requester as approver and says so.
	result, child, draft, err := h.request(root.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Approver != "alice" || result["approver"] != "alice" || result["self_confirm"] != true || result["waiting_for"] != "requester_submission" {
		t.Fatalf("child draft approver %q result %v", draft.Approver, result)
	}
	status := mustCallGatewayTool(t, h.service, h.alice, "get_task_status", map[string]any{"task_id": child.ID})
	if status["waiting_for"] != "requester_confirmation" || status["approver"] != "alice" {
		t.Fatalf("child status %v", status)
	}
	// bob, who is not the child's approver, may not confirm it.
	if code := h.approve(child, draft, "bob"); code != http.StatusForbidden {
		t.Fatalf("bob confirming the child = %d, want 403", code)
	}
	if code := h.approve(child, draft, "alice"); code != http.StatusOK {
		t.Fatalf("alice confirming the child = %d", code)
	}
	if got := h.state(child.ID); got.State != control.TaskActive || got.RootTaskID != root.ID {
		t.Fatalf("child after confirmation %+v", got)
	}
	grant, err := h.store.GetGrant(context.Background(), child.ID)
	if err != nil || grant.Subject != "alice" {
		t.Fatalf("child grant %+v %v", grant, err)
	}
	protocol, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil || protocol.ApprovalReceipt.ApproverID != "alice" {
		t.Fatalf("child receipt approver %v %v", err, protocol.ApprovalReceipt.ApproverID)
	}
	// A child may not send its confirmation to someone else.
	if _, _, _, err := h.request(root.ID, "bob"); err == nil {
		t.Fatal("a self-confirmed child naming another approver was accepted")
	} else {
		requireToolCode(t, err, apierr.CodeInvalidRequest)
	}
	// Naming oneself explicitly is the same as not naming anyone.
	if _, _, named, err := h.request(root.ID, "alice"); err != nil || named.Approver != "alice" {
		t.Fatalf("child naming its requester: %v approver %q", err, named.Approver)
	}
}

func TestChildSelfConfirmationIsRefusedWithoutTheCatalogBlock(t *testing.T) {
	h := newSelfConfirmHarness(t, false)
	_, root, rootDraft, err := h.request("", "")
	if err != nil {
		t.Fatal(err)
	}
	if code := h.approve(root, rootDraft, "bob"); code != http.StatusOK {
		t.Fatalf("root approval = %d", code)
	}
	result, _, draft, err := h.request(root.ID, "")
	if err != nil || draft.Approver != "bob" || result["self_confirm"] != nil {
		t.Fatalf("child without the block: %v approver %q self_confirm %v", err, draft.Approver, result["self_confirm"])
	}
	if _, _, _, err := h.request(root.ID, "alice"); err == nil {
		t.Fatal("a child approved by its requester was accepted without the block")
	} else {
		requireToolCode(t, err, apierr.CodeApproverNotAuthorized)
	}
}

// The callback side fails closed: a pending record whose approver is the
// human subject is refused unless the request side marked it self-confirmed.
func TestCallbackRefusesAnUnmarkedSelfApproval(t *testing.T) {
	event := oaCallbackEvent{Status: "approved", Actor: "alice"}
	unmarked := pendingContext{ApprovalMode: "manual", Approver: "alice"}
	if validCallbackActor(event, unmarked, "alice") {
		t.Fatal("an unmarked self-approval was allowed")
	}
	marked := pendingContext{ApprovalMode: "manual", Approver: "alice", SelfConfirm: true}
	if !validCallbackActor(event, marked, "alice") {
		t.Fatal("a marked self-confirmation was refused")
	}
	if validCallbackActor(oaCallbackEvent{Status: "approved", Actor: "bob"}, marked, "alice") {
		t.Fatal("someone else decided a self-confirmed task")
	}
	other := pendingContext{ApprovalMode: "manual", Approver: "bob"}
	if !validCallbackActor(oaCallbackEvent{Status: "approved", Actor: "bob"}, other, "alice") {
		t.Fatal("an ordinary approval was refused")
	}
}

// A child's TTL is cut to the parent's remaining time when requested, but a
// grant is re-issued from its decision time: a decision that comes later
// outlives the parent unless its TTL is shortened, which the OA does as a
// narrowing (approval.DraftRequest.ParentExpiresAt).
func TestAChildDecidedLaterMustShortenItsTTLToTheParent(t *testing.T) {
	h := newSelfConfirmHarness(t, true)
	_, root, rootDraft, err := h.request("", "")
	if err != nil {
		t.Fatal(err)
	}
	if code := h.approve(root, rootDraft, "bob"); code != http.StatusOK {
		t.Fatalf("root approval = %d", code)
	}
	rootGrant, err := h.store.GetGrant(context.Background(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, child, draft, err := h.request(root.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if draft.ParentExpiresAt == nil || !draft.ParentExpiresAt.Equal(rootGrant.ExpiresAt) {
		t.Fatalf("child draft parent expiry %v, want the root grant's %v", draft.ParentExpiresAt, rootGrant.ExpiresAt)
	}
	// The decision comes a minute later: as made, the child would outlive the root.
	h.clock.value = h.clock.value.Add(time.Minute)
	asMade, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, h.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	if code := sendGatewayCallback(t, h.gatewayHarness, signedFinalCallback(t, h.gatewayHarness, child, draft, "approved", "alice", asMade), "").Code; code != http.StatusConflict {
		t.Fatalf("a child decided a minute later as made = %d, want 409", code)
	}
	// Shortened to the parent's remaining time, as a narrowing, it is accepted.
	shortened := asMade
	shortened.Budget.TaskTTLMS = rootGrant.ExpiresAt.Sub(h.clock.value).Milliseconds()
	shortened.ExpiresAt = h.clock.value.UTC().Add(time.Duration(shortened.Budget.TaskTTLMS) * time.Millisecond)
	if code := sendGatewayCallback(t, h.gatewayHarness, signedFinalCallback(t, h.gatewayHarness, child, draft, "narrowed", "alice", shortened), "").Code; code != http.StatusOK {
		t.Fatalf("the shortened child decision = %d", code)
	}
	if got := h.state(child.ID); got.State != control.TaskActive {
		t.Fatalf("child after the shortened decision %s", got.State)
	}
}
