package control

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// TestApprovalStepRecordsWithoutChangingState: a chain step is claimed,
// recorded and audited, leaves the task awaiting approval, replays its stored
// response, and is refused when another approval landed first.
func TestApprovalStepRecordsWithoutChangingState(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	clock := fixedClock{value: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	store := openTestStore(t, path, testCipher(t, 2), WithClock(clock))
	ctx := context.Background()
	expires := clock.value.Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_chain", expires)
	zero, one := 0, 1
	step := ApprovalCallback{
		EventID: "oa_step_1", RawPayload: []byte(`{"status":"approved","actor":"finance-director"}`),
		ExpectedState: TaskAwaitingApproval, NewState: TaskAwaitingApproval, Step: true, ApprovedSteps: &zero,
		Response: []byte(`{"ok":true,"granted":false,"next_approver":"sales-director"}`),
		Event:    ApprovalEvent{TaskID: "task_chain", Actor: "finance-director", Decision: "approved", Payload: []byte(`{"status":"approved"}`)},
	}
	claim, err := store.ApplyApprovalCallback(ctx, step)
	if err != nil || claim.Status != CallbackCompleted {
		t.Fatalf("approval step: %+v %v", claim, err)
	}
	task, err := store.GetTask(ctx, "task_chain")
	if err != nil || task.State != TaskAwaitingApproval {
		t.Fatalf("task after a step: %+v %v", task, err)
	}
	replay, err := store.ApplyApprovalCallback(ctx, step)
	if err != nil || !replay.Replay || !bytes.Equal(replay.Response, step.Response) {
		t.Fatalf("step replay: %+v %v", replay, err)
	}
	// The same chain position again, under another event: one approval is
	// now recorded, so a callback derived from zero is out of order.
	stale := step
	stale.EventID, stale.RawPayload = "oa_step_stale", []byte(`{"status":"approved","actor":"finance-director","again":true}`)
	if _, err := store.ApplyApprovalCallback(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("an out-of-order step: got %v, want ErrConflict", err)
	}
	next := step
	next.EventID, next.RawPayload, next.ApprovedSteps = "oa_step_2", []byte(`{"status":"approved","actor":"sales-director"}`), &one
	next.Event.Actor = "sales-director"
	if _, err := store.ApplyApprovalCallback(ctx, next); err != nil {
		t.Fatalf("the next step: %v", err)
	}
	events, err := store.ListApprovalEvents(ctx, "task_chain")
	if err != nil || len(events) != 2 || events[0].Actor != "finance-director" || events[1].Actor != "sales-director" {
		t.Fatalf("approval events %+v %v", events, err)
	}
	var audited int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE task_id='task_chain' AND event_type='APPROVAL_STEP_APPLIED'`).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("step audit events %d %v", audited, err)
	}
	bad := next
	bad.EventID, bad.RawPayload, bad.ApprovedSteps = "oa_step_bad", []byte(`{"bad":true}`), nil
	bad.NewState = TaskActive
	if _, err := store.ApplyApprovalCallback(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a step that changes state: got %v, want ErrInvalid", err)
	}
	// The final approval still moves the task out of AWAITING_APPROVAL.
	approveTask(t, store, "task_chain", expires, BudgetLimits{Queries: 1, Rows: 1, DBMS: 1})
	if task, err := store.GetTask(ctx, "task_chain"); err != nil || task.State != TaskActive {
		t.Fatalf("task after the final approval: %+v %v", task, err)
	}
}
