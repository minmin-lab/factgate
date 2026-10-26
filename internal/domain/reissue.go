package domain

import "time"

// Reissued is the core re-issued at issuedAt: the same authority, with expiry
// re-based to issuedAt plus the core's own TTL. In an approval chain each
// approver approves or narrows the previous step's core re-issued at their
// own time, so expiry always counts from the approval that issues the grant
// (KB1-LOOP-141).
func (g TaskGrantCoreV1) Reissued(issuedAt time.Time) TaskGrantCoreV1 {
	out := g
	out.ApprovedProducts = append([]string(nil), g.ApprovedProducts...)
	out.ApprovedColumns = cloneAuthorizationColumns(g.ApprovedColumns)
	out.MandatoryScope = cloneAuthorizationScope(g.MandatoryScope)
	out.ExpiresAt = issuedAt.UTC().Add(time.Duration(g.Budget.TaskTTLMS) * time.Millisecond)
	return out
}
