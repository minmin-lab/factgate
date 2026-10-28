package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// AgentIdentityAuthenticator resolves a bearer token through the demo's
// switchable agent identities first (KB1-LOOP-151, control migration 023) and
// falls back to the static authenticator for any token no label holds. The
// binding wins on purpose: registering an existing token's digest under a
// label makes it switchable without touching the client's configuration. The
// gateway assembles it only when the Catalog declares
// delegated_authorization; the formal round keeps the static authenticator.
//
// Every request is authenticated afresh, so a switch applies from the next
// request on.
type AgentIdentityAuthenticator struct {
	Store    *control.Store
	Fallback mcp.Authenticator
	// Logger records lookup failures; nil uses slog.Default.
	Logger *slog.Logger
}

func (a *AgentIdentityAuthenticator) Authenticate(ctx context.Context, token string) (mcp.Principal, bool) {
	if a == nil || a.Store == nil {
		return mcp.Principal{}, false
	}
	sum := sha256.Sum256([]byte(token))
	identity, err := a.Store.LookupAgentIdentityByToken(ctx, hex.EncodeToString(sum[:]))
	if errors.Is(err, control.ErrNotFound) {
		if a.Fallback == nil {
			return mcp.Principal{}, false
		}
		return a.Fallback.Authenticate(ctx, token)
	}
	if err != nil {
		a.logger().Warn("agent identity lookup failed", "error", err)
		return mcp.Principal{}, false
	}
	stored, err := a.Store.GetPrincipal(ctx, identity.PrincipalID)
	if err != nil {
		a.logger().Warn("agent identity principal lookup failed", "agent_label", identity.Label, "principal", identity.PrincipalID, "error", err)
		return mcp.Principal{}, false
	}
	// A disabled bound principal refuses the token outright: it never falls
	// through to the static table, which may know the same token under the
	// principal the label was bound to before.
	if stored.DisabledAt != nil {
		return mcp.Principal{}, false
	}
	return mcp.Principal{ID: stored.ID, Subject: stored.Subject, Role: stored.Role, AgentLabel: identity.Label}, true
}

func (a *AgentIdentityAuthenticator) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}
