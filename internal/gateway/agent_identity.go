package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Switchable agent identities (KB1-LOOP-151; the demo, not a paper claim). A
// tester has one agent program and one MCP token; the principal that token
// acts as is switched by a person in the OA ("my agent"), never by the agent.
// Everything here applies only under the Catalog's delegated_authorization
// block: without it the OA and admin endpoints answer 404, whoami is not
// listed and is forbidden, and tool results carry no acting_as.
//
//   - whoami tells the agent who it currently is.
//   - Every structured tool result carries acting_as.
//   - POST /api/v1/oa/agent-identity (signed like the OA callback) lists the
//     bindings and their candidate principals, and switches a binding.
//   - POST /admin/v1/demo/agent-identities (admin token) registers a token
//     digest under a label; the gateway never sees the token itself.

var whoamiTool = mcp.Tool{Name: "whoami",
	Description: "查看当前连接代表的身份：主体、部门、岗位、中心、密级包络与职权部门。若这把令牌是演示用的可切换令牌，另给 agent_label 与绑定时间。身份只能由人在 OA 的「我的 agent」处切换，agent 自己不能切换，也不要在被拒后尝试换身份。",
	InputSchema: objectSchema(nil), Annotations: map[string]any{"readOnlyHint": true}}

// whoamiAttributeKeys are the attribute keys whoami reports, each read from
// the same-named attested attribute except authority_departments, which is
// the Catalog's authority attribute (delegated_authorization
// authority_attribute, authority_departments by default).
var whoamiAttributeKeys = []string{"department", "job_family", "center", "clearance", "authority_departments", "employee_id"}

func (s *Service) whoami(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	delegated := s.delegated()
	if delegated == nil {
		return nil, forbidden()
	}
	result := map[string]any{
		"principal_id": principal.ID, "subject": principal.Subject, "role": principal.Role,
		"catalog_version": s.catalog.CatalogVersion,
	}
	if principal.AgentLabel != "" {
		result["agent_label"] = principal.AgentLabel
		identity, err := s.store.GetAgentIdentity(ctx, principal.AgentLabel)
		switch {
		case err == nil && identity.PrincipalID == principal.ID:
			result["bound_by"] = identity.BoundBy
			result["bound_at"] = agentIdentityTime(identity.BoundAt)
		case err != nil && !errors.Is(err, control.ErrNotFound):
			return nil, err
		}
		// A binding switched since this request authenticated describes the
		// next request's principal, not this one: its bound_by and bound_at
		// are left out rather than attributed to the wrong principal.
	}
	if s.principalAttributes != nil {
		now := s.clock()
		attributes := make(map[string]any, len(whoamiAttributeKeys))
		for _, key := range whoamiAttributeKeys {
			attribute := key
			if key == "authority_departments" {
				attribute = delegated.AuthorityAttributeName()
			}
			values := s.principalAttributes.Values(principal.Subject, attribute, now)
			if values == nil {
				values = []string{}
			}
			attributes[key] = values
		}
		result["attributes"] = attributes
	}
	return result, nil
}

// actingAs is the acting_as object every structured tool result carries under
// the delegated_authorization block.
func (s *Service) actingAs(principal mcp.Principal) map[string]any {
	acting := map[string]any{"principal_id": principal.ID, "subject": principal.Subject}
	if principal.AgentLabel != "" {
		acting["agent_label"] = principal.AgentLabel
	}
	now := s.clock()
	for _, attribute := range []string{"department", "job_family"} {
		if values := s.principalAttributes.Values(principal.Subject, attribute, now); len(values) > 0 {
			acting[attribute] = values[0]
		}
	}
	return acting
}

// withActingAs returns result with an acting_as key added at its top level.
// A map result is copied shallowly, so its values keep their Go types and
// their JSON encoding byte for byte. Any other result is re-decoded from its
// JSON with json.Number, so no number is rounded through float64; a result
// whose JSON is not an object is returned unchanged.
func withActingAs(result any, acting map[string]any) any {
	if object, ok := result.(map[string]any); ok {
		wrapped := make(map[string]any, len(object)+1)
		for key, value := range object {
			wrapped[key] = value
		}
		wrapped["acting_as"] = acting
		return wrapped
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return result
	}
	object["acting_as"] = acting
	return object
}

// agentIdentityView is one binding as the OA and admin endpoints return it.
type agentIdentityView struct {
	Label       string `json:"label"`
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
	BoundBy     string `json:"bound_by"`
	BoundAt     string `json:"bound_at"`
}

// agentCandidateView is a principal a binding may be switched to.
type agentCandidateView struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
	Department  string `json:"department"`
	JobFamily   string `json:"job_family"`
	Center      string `json:"center"`
}

func agentIdentityTime(value time.Time) string { return value.UTC().Format(time.RFC3339) }

func viewAgentIdentity(identity control.AgentIdentity, subject string) agentIdentityView {
	return agentIdentityView{Label: identity.Label, PrincipalID: identity.PrincipalID, Subject: subject,
		BoundBy: identity.BoundBy, BoundAt: agentIdentityTime(identity.BoundAt)}
}

// agentCandidates are the principals a binding may act as: enabled query
// principals the attribute snapshot knows, ordered by subject (principals
// arrive ordered by subject). Each carries the first value of its department,
// job_family and center, or "".
func (s *Service) agentCandidates(principals []control.Principal) []agentCandidateView {
	now := s.clock()
	first := func(subject, attribute string) string {
		if values := s.principalAttributes.Values(subject, attribute, now); len(values) > 0 {
			return values[0]
		}
		return ""
	}
	candidates := []agentCandidateView{}
	for _, principal := range principals {
		if principal.Role != "query" || principal.DisabledAt != nil || !s.principalAttributes.Known(principal.Subject) {
			continue
		}
		candidates = append(candidates, agentCandidateView{PrincipalID: principal.ID, Subject: principal.Subject,
			Department: first(principal.Subject, "department"), JobFamily: first(principal.Subject, "job_family"),
			Center: first(principal.Subject, "center")})
	}
	return candidates
}

type oaAgentIdentityRequest struct {
	EventID     string `json:"event_id"`
	Action      string `json:"action"`
	Label       string `json:"label,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	Actor       string `json:"actor,omitempty"`
}

// OAAgentIdentityHandler serves POST /api/v1/oa/agent-identity: the OA's "my
// agent" page lists the bindings and switches one. It authenticates exactly
// as the OA callback does (X-OA-Event-ID, X-OA-Timestamp, X-OA-Signature
// over the raw body with the callback secret, five-minute window), and
// answers 404 without the Catalog's delegated_authorization block.
func (s *Service) OAAgentIdentityHandler() http.Handler {
	return http.HandlerFunc(s.handleOAAgentIdentity)
}

func (s *Service) handleOAAgentIdentity(w http.ResponseWriter, r *http.Request) {
	if s.delegated() == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || len(rawBody) == 0 {
		writeCallbackError(w, http.StatusBadRequest, "invalid request")
		return
	}
	eventID := r.Header.Get("X-OA-Event-ID")
	if eventID == "" || approval.Verify(s.callbackSecret, r.Header.Get("X-OA-Timestamp"), r.Header.Get("X-OA-Signature"), rawBody, s.clock(), 5*time.Minute) != nil {
		writeCallbackError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	var request oaAgentIdentityRequest
	if !decodeStrictJSON(rawBody, &request) || request.EventID != eventID {
		writeCallbackError(w, http.StatusBadRequest, "invalid request")
		return
	}
	ctx := r.Context()
	principals, err := s.store.ListPrincipals(ctx)
	if err != nil {
		s.logger.Error("list principals for agent identities", "error", err)
		writeCallbackError(w, http.StatusInternalServerError, "agent identity request failed")
		return
	}
	candidates := s.agentCandidates(principals)
	switch request.Action {
	case "list":
		identities, err := s.store.ListAgentIdentities(ctx)
		if err != nil {
			s.logger.Error("list agent identities", "error", err)
			writeCallbackError(w, http.StatusInternalServerError, "agent identity request failed")
			return
		}
		subjects := make(map[string]string, len(principals))
		for _, principal := range principals {
			subjects[principal.ID] = principal.Subject
		}
		views := make([]agentIdentityView, 0, len(identities))
		for _, identity := range identities {
			views = append(views, viewAgentIdentity(identity, subjects[identity.PrincipalID]))
		}
		writeAgentIdentityJSON(w, http.StatusOK, map[string]any{"identities": views, "candidates": candidates})
	case "switch":
		actor := strings.TrimSpace(request.Actor)
		if actor == "" {
			writeCallbackError(w, http.StatusBadRequest, "actor is required")
			return
		}
		if strings.TrimSpace(request.Label) == "" {
			writeCallbackError(w, http.StatusBadRequest, "label is required")
			return
		}
		var target *agentCandidateView
		for index := range candidates {
			if candidates[index].PrincipalID == request.PrincipalID {
				target = &candidates[index]
				break
			}
		}
		if target == nil {
			writeCallbackError(w, http.StatusBadRequest, "principal_id is not a candidate")
			return
		}
		identity, err := s.store.SwitchAgentIdentity(ctx, request.Label, target.PrincipalID, actor, s.clock())
		switch {
		case errors.Is(err, control.ErrNotFound) || errors.Is(err, control.ErrInvalid):
			writeCallbackError(w, http.StatusBadRequest, "unknown label")
			return
		case err != nil:
			s.logger.Error("switch agent identity", "agent_label", request.Label, "error", err)
			writeCallbackError(w, http.StatusInternalServerError, "agent identity request failed")
			return
		}
		s.logger.Info("agent identity switched", "agent_label", identity.Label, "principal", identity.PrincipalID,
			"subject", target.Subject, "actor", actor, "event_id", eventID)
		writeAgentIdentityJSON(w, http.StatusOK, map[string]any{"identity": viewAgentIdentity(identity, target.Subject)})
	default:
		writeCallbackError(w, http.StatusBadRequest, "unknown action")
	}
}

type adminAgentIdentityRequest struct {
	Label       string `json:"label"`
	TokenSHA256 string `json:"token_sha256"`
	PrincipalID string `json:"principal_id"`
	By          string `json:"by"`
}

// AdminAgentIdentityHandler serves POST /admin/v1/demo/agent-identities:
// it registers (or rebinds) a label for a token's SHA-256 digest. The caller
// mounts it behind the admin token; the token itself never reaches the
// gateway. It answers 404 without the Catalog's delegated_authorization block.
func (s *Service) AdminAgentIdentityHandler() http.Handler {
	return http.HandlerFunc(s.handleAdminAgentIdentity)
}

func (s *Service) handleAdminAgentIdentity(w http.ResponseWriter, r *http.Request) {
	if s.delegated() == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	var request adminAgentIdentityRequest
	if err != nil || !decodeStrictJSON(rawBody, &request) {
		writeCallbackError(w, http.StatusBadRequest, "invalid request")
		return
	}
	request.Label, request.PrincipalID, request.By = strings.TrimSpace(request.Label), strings.TrimSpace(request.PrincipalID), strings.TrimSpace(request.By)
	switch {
	case !control.ValidAgentLabel(request.Label):
		writeCallbackError(w, http.StatusBadRequest, "label must match ^[a-z0-9][a-z0-9_-]{0,31}$")
		return
	case !validSnapshotSHA256(request.TokenSHA256):
		writeCallbackError(w, http.StatusBadRequest, "token_sha256 must be 64 lowercase hex characters")
		return
	case request.By == "":
		writeCallbackError(w, http.StatusBadRequest, "by is required")
		return
	}
	ctx := r.Context()
	principal, err := s.store.GetPrincipal(ctx, request.PrincipalID)
	if errors.Is(err, control.ErrNotFound) || (err == nil && (principal.Role != "query" || principal.DisabledAt != nil)) {
		writeCallbackError(w, http.StatusBadRequest, "principal_id must name an enabled query principal")
		return
	}
	if err != nil {
		s.logger.Error("get principal for agent identity", "principal", request.PrincipalID, "error", err)
		writeCallbackError(w, http.StatusInternalServerError, "agent identity registration failed")
		return
	}
	// One token acts for one label (the digest is UNIQUE); say so instead of
	// surfacing the constraint violation.
	if holder, err := s.store.LookupAgentIdentityByToken(ctx, request.TokenSHA256); err == nil && holder.Label != request.Label {
		writeCallbackError(w, http.StatusConflict, "token_sha256 is already registered under another label")
		return
	} else if err != nil && !errors.Is(err, control.ErrNotFound) {
		s.logger.Error("lookup agent identity by token", "error", err)
		writeCallbackError(w, http.StatusInternalServerError, "agent identity registration failed")
		return
	}
	if err := s.store.RegisterAgentIdentity(ctx, control.AgentIdentity{Label: request.Label, TokenSHA256: request.TokenSHA256,
		PrincipalID: principal.ID, BoundBy: request.By, BoundAt: s.clock()}); err != nil {
		s.logger.Error("register agent identity", "agent_label", request.Label, "error", err)
		writeCallbackError(w, http.StatusInternalServerError, "agent identity registration failed")
		return
	}
	identity, err := s.store.GetAgentIdentity(ctx, request.Label)
	if err != nil {
		s.logger.Error("read registered agent identity", "agent_label", request.Label, "error", err)
		writeCallbackError(w, http.StatusInternalServerError, "agent identity registration failed")
		return
	}
	s.logger.Info("agent identity registered", "agent_label", identity.Label, "principal", identity.PrincipalID, "by", identity.BoundBy)
	writeAgentIdentityJSON(w, http.StatusOK, map[string]any{"identity": viewAgentIdentity(identity, principal.Subject)})
}

// decodeStrictJSON decodes one JSON object with no unknown field and nothing
// after it.
func decodeStrictJSON(raw []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func writeAgentIdentityJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
