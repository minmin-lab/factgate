package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Draft withdrawal (KB1-LOOP-137's OA reconciliation; a demo feature, used
// only under the Catalog's delegated_authorization block): when TaskGate
// archives a task whose OA draft may still wait for a person -- revoked, or
// expired while pending -- it tells the OA, so the draft leaves the to-do
// list instead of inviting an approval TaskGate would refuse with 409. The
// Gateway stays authoritative; the notice is bookkeeping, not the safety.

// Withdrawal reasons; the values are TaskGate's terminal reasons.
const (
	WithdrawalRevoked = "revoked"
	WithdrawalExpired = "expired"
)

// DraftWithdrawal is the body of POST /api/drafts/{draft_id}/withdraw: the
// Gateway sends it and the OA decodes it, so both sides share this one type.
type DraftWithdrawal struct {
	TaskID     string    `json:"task_id"`
	DraftID    string    `json:"draft_id"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ValidWithdrawalReason reports whether reason is one the OA accepts.
func ValidWithdrawalReason(reason string) bool {
	return reason == WithdrawalRevoked || reason == WithdrawalExpired
}

// DraftWithdrawPath is the OA route that withdraws a draft.
func DraftWithdrawPath(draftID string) string {
	return "/api/drafts/" + url.PathEscape(draftID) + "/withdraw"
}

// DraftWithdrawer is the optional capability of an ApprovalAdapter that can
// withdraw a draft. It is separate from ApprovalAdapter so that adapters
// written before it (the evaluation's capturing adapter among them) keep
// compiling and simply are not notified.
type DraftWithdrawer interface {
	WithdrawDraft(context.Context, DraftWithdrawal) error
}

var _ DraftWithdrawer = (*Client)(nil)

// WithdrawDraft asks the OA to withdraw a draft. A 2xx answer is success, and
// so is 404: the draft is gone already (an OA rebuilt since keeps no drafts).
func (c *Client) WithdrawDraft(ctx context.Context, withdrawal DraftWithdrawal) error {
	if withdrawal.DraftID == "" || withdrawal.TaskID == "" || !ValidWithdrawalReason(withdrawal.Reason) {
		return errors.New("OA draft withdrawal needs a task, a draft and a known reason")
	}
	body, err := json.Marshal(withdrawal)
	if err != nil {
		return fmt.Errorf("encode OA draft withdrawal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+DraftWithdrawPath(withdrawal.DraftID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build OA draft withdrawal: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("OA draft withdrawal: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("OA refused the draft withdrawal: status %d", resp.StatusCode)
}
