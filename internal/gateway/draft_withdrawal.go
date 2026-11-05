package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

// OA reconciliation (KB1-LOOP-137; the delegated-authorization demo, not a
// paper claim). Under the Catalog's delegated_authorization block, a task
// archived while its OA draft may still wait for a person -- revoked, with
// its cascaded descendants, or past the block's pending_ttl -- is reported to
// the OA, which withdraws the draft. Without the block nothing is sent and
// pending tasks never expire, as in the formal round.

// draftWithdrawalTimeout bounds one withdrawal notice, so an unreachable OA
// delays a revocation by at most this much per draft.
const draftWithdrawalTimeout = 5 * time.Second

// notifyDraftWithdrawn tells the OA to withdraw an archived task's draft. It
// is bookkeeping, not the safety: the Gateway already refuses (409) any later
// callback on the archived task, so a failure is only logged.
func (s *Service) notifyDraftWithdrawn(ctx context.Context, task control.Task, reason control.TerminalReason, actor string) {
	if s.delegated() == nil || task.ApprovalRef == "" {
		return
	}
	withdrawer, ok := s.approval.(approval.DraftWithdrawer)
	if !ok {
		return
	}
	occurredAt := task.UpdatedAt
	if occurredAt.IsZero() {
		occurredAt = s.clock()
	}
	notifyCtx, cancel := context.WithTimeout(ctx, draftWithdrawalTimeout)
	defer cancel()
	err := withdrawer.WithdrawDraft(notifyCtx, approval.DraftWithdrawal{
		TaskID: task.ID, DraftID: task.ApprovalRef, Reason: string(reason), Actor: actor, OccurredAt: occurredAt.UTC(),
	})
	if err != nil {
		s.logger.Warn("OA draft withdrawal failed; the task stays archived", "task_id", task.ID, "draft_id", task.ApprovalRef,
			"reason", string(reason), "error", err)
	}
}

// pendingTTL is the Catalog's delegated_authorization pending_ttl; zero when
// the block or the field is absent.
func (s *Service) pendingTTL() (time.Duration, string) {
	delegated := s.delegated()
	if delegated == nil {
		return 0, ""
	}
	return delegated.PendingTTLDuration(), delegated.PendingTTL
}

// SweepPendingExpiry archives, as expired, every task still awaiting its
// submission or approval pending_ttl after its creation, and asks the OA to
// withdraw its draft. It returns the archived task ids. Without the
// delegated_authorization block or its pending_ttl it does nothing. ACTIVE
// tasks are the grant-expiry sweep's (cmd/gateway sweepExpired).
func (s *Service) SweepPendingExpiry(ctx context.Context, now time.Time) ([]string, error) {
	ttl, declared := s.pendingTTL()
	if ttl <= 0 {
		return nil, nil
	}
	payload, _ := json.Marshal(map[string]any{"stage": "pending", "pending_ttl": declared})
	archived := []string{}
	for _, state := range []control.TaskState{control.TaskAwaitingSubmission, control.TaskAwaitingApproval} {
		afterID := ""
		for {
			page, err := s.store.ListTasks(ctx, control.TaskFilter{State: state, AfterID: afterID, Limit: 500})
			if err != nil {
				return archived, err
			}
			for _, task := range page {
				afterID = task.ID
				// Expired once CreatedAt + pending_ttl <= now.
				if waited := now.Sub(task.CreatedAt); waited < ttl {
					continue
				}
				updated, err := s.store.TransitionTask(ctx, control.TaskTransition{
					TaskID: task.ID, ExpectedFrom: state, To: control.TaskArchived,
					Reason: control.TerminalExpired, Actor: "system", Payload: payload,
				})
				switch {
				case err == nil:
					archived = append(archived, task.ID)
					s.notifyDraftWithdrawn(ctx, updated, control.TerminalExpired, "system")
				case errors.Is(err, control.ErrInvalidStateChange), errors.Is(err, control.ErrNotFound):
					// Changed meanwhile; the next sweep sees its new state.
				default:
					s.logger.Warn("archive pending task past pending_ttl", "task_id", task.ID, "error", err)
				}
			}
			if len(page) < 500 {
				break
			}
		}
	}
	return archived, nil
}
