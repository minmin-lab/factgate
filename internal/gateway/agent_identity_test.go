package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// Switchable agent identities (KB1-LOOP-151; the demo, not a paper claim): a
// token registered under a label acts as the principal a person binds it to
// in the OA, from the next request on; whoami says who that is; every
// structured result carries acting_as. Without the Catalog's
// delegated_authorization block none of it exists.

const agentIdentitySubjects = `  - subject: alice
    attributes:
      department: [finance]
      job_family: [finance_controller, dept_admin]
      clearance: [public, internal, confidential, leadership]
      authority_departments: [finance]
  - subject: staff
    attributes:
      department: [finance]
      job_family: [staff]
      center: [finance-a]
      clearance: [public, internal]
      employee_id: [staff]
      display_name: [周燕]
      title: [会计]
      employee_no: [E0007]
  - subject: sales-staff
    attributes:
      department: [sales]
      job_family: [staff]
      center: [sales-b]
      clearance: [public]
  - subject: retired
    attributes:
      department: [hr]
      job_family: [staff]
`

const (
	demoAgentToken  = "demo-agent-token-lzj"
	staticTokenOfAl = "static-token-alice"
)

type agentIdentityHarness struct {
	*gatewayHarness
	t          *testing.T
	staff      mcp.Principal
	salesStaff mcp.Principal
}

// newAgentIdentityHarness builds the delegated (or plain) harness with three
// more query principals: staff and sales-staff, whom the snapshot knows, a
// ghost it does not know, and retired, disabled.
func newAgentIdentityHarness(t *testing.T, delegated bool) *agentIdentityHarness {
	t.Helper()
	rewrite := chainCatalogWith(t, issueChainRouteYAML)
	if delegated {
		rewrite = delegatedCatalog(rewrite)
	}
	base := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: rewrite, principalAttributes: approverSnapshot(t, agentIdentitySubjects)})
	if (base.catalog.DelegatedAuthorization != nil) != delegated {
		t.Fatalf("delegated_authorization present = %v, want %v", base.catalog.DelegatedAuthorization != nil, delegated)
	}
	ctx := context.Background()
	for _, principal := range []control.Principal{
		{ID: "principal-staff", Subject: "staff", Role: "query"},
		{ID: "principal-sales-staff", Subject: "sales-staff", Role: "query"},
		{ID: "principal-ghost", Subject: "ghost", Role: "query"},
		{ID: "principal-retired", Subject: "retired", Role: "query"},
	} {
		principal.CreatedAt = base.clock.value
		if err := base.store.CreatePrincipal(ctx, principal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := base.store.DisablePrincipal(ctx, "principal-retired", base.clock.value); err != nil {
		t.Fatal(err)
	}
	return &agentIdentityHarness{gatewayHarness: base, t: t,
		staff:      mcp.Principal{ID: "principal-staff", Subject: "staff", Role: "query"},
		salesStaff: mcp.Principal{ID: "principal-sales-staff", Subject: "sales-staff", Role: "query"}}
}

func tokenSHA256(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (h *agentIdentityHarness) register(label, token, principalID string) {
	h.t.Helper()
	if err := h.store.RegisterAgentIdentity(context.Background(), control.AgentIdentity{Label: label, TokenSHA256: tokenSHA256(token),
		PrincipalID: principalID, BoundBy: "wmm", BoundAt: h.clock.value}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *agentIdentityHarness) authenticator() *AgentIdentityAuthenticator {
	return &AgentIdentityAuthenticator{Store: h.store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fallback: mcp.NewStaticAuthenticator([]mcp.TokenIdentity{{Token: staticTokenOfAl, Principal: h.alice}})}
}

// oaAgentIdentity posts a signed body to the OA endpoint; signature "" signs
// it with the callback secret.
func (h *agentIdentityHarness) oaAgentIdentity(body map[string]any, signature string) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	timestamp := strconv.FormatInt(h.clock.value.Unix(), 10)
	if signature == "" {
		signature = approval.Sign([]byte(h.secret), timestamp, raw)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oa/agent-identity", bytes.NewReader(raw))
	eventID, _ := body["event_id"].(string)
	request.Header.Set("X-OA-Event-ID", eventID)
	request.Header.Set("X-OA-Timestamp", timestamp)
	request.Header.Set("X-OA-Signature", signature)
	recorder := httptest.NewRecorder()
	h.service.OAAgentIdentityHandler().ServeHTTP(recorder, request)
	return recorder
}

func (h *agentIdentityHarness) admin(body string) *httptest.ResponseRecorder {
	h.t.Helper()
	recorder := httptest.NewRecorder()
	h.service.AdminAgentIdentityHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/v1/demo/agent-identities", strings.NewReader(body)))
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

func switchHistory(t *testing.T, h *agentIdentityHarness, label string) [][3]string {
	t.Helper()
	rows, err := h.controlDB(t).Query(`SELECT COALESCE(from_principal_id, ''), to_principal_id, switched_by FROM demo_agent_identity_switches WHERE label = $1 ORDER BY id`, label)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var history [][3]string
	for rows.Next() {
		var entry [3]string
		if err := rows.Scan(&entry[0], &entry[1], &entry[2]); err != nil {
			t.Fatal(err)
		}
		history = append(history, entry)
	}
	return history
}

func TestAgentIdentityAuthenticatorFollowsTheBindingFromTheNextRequest(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	ctx := context.Background()
	authenticator := h.authenticator()
	h.register("lzj", demoAgentToken, h.staff.ID)

	principal, ok := authenticator.Authenticate(ctx, demoAgentToken)
	if !ok || principal != (mcp.Principal{ID: h.staff.ID, Subject: "staff", Role: "query", AgentLabel: "lzj"}) {
		t.Fatalf("bound token authenticated as %+v %v", principal, ok)
	}
	if err := h.service.principalEnabled(ctx, principal); err != nil {
		t.Fatalf("the bound principal is not enabled: %v", err)
	}
	if len(h.service.ListTools(principal)) == 0 {
		t.Fatal("the bound principal sees no tools")
	}

	if _, err := h.store.SwitchAgentIdentity(ctx, "lzj", h.salesStaff.ID, "procurement-admin", h.clock.value.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	principal, ok = authenticator.Authenticate(ctx, demoAgentToken)
	if !ok || principal.ID != h.salesStaff.ID || principal.Subject != "sales-staff" || principal.AgentLabel != "lzj" {
		t.Fatalf("after the switch the token authenticated as %+v %v", principal, ok)
	}
	if err := h.service.principalEnabled(ctx, principal); err != nil {
		t.Fatalf("the switched principal is not enabled: %v", err)
	}

	// A token no label holds goes to the static table.
	if principal, ok := authenticator.Authenticate(ctx, staticTokenOfAl); !ok || principal != h.alice {
		t.Fatalf("static token authenticated as %+v %v", principal, ok)
	}
	if _, ok := authenticator.Authenticate(ctx, "unknown-token"); ok {
		t.Fatal("an unknown token authenticated")
	}

	// The binding wins over the static table for the same token.
	h.register("alice-agent", staticTokenOfAl, h.staff.ID)
	if principal, ok := authenticator.Authenticate(ctx, staticTokenOfAl); !ok || principal.ID != h.staff.ID || principal.AgentLabel != "alice-agent" {
		t.Fatalf("a registered static token authenticated as %+v %v, want the binding", principal, ok)
	}

	// A disabled bound principal refuses the token; it does not fall back.
	if _, err := h.store.DisablePrincipal(ctx, h.staff.ID, h.clock.value.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if principal, ok := authenticator.Authenticate(ctx, staticTokenOfAl); ok {
		t.Fatalf("a token bound to a disabled principal authenticated as %+v", principal)
	}
	if principal, ok := authenticator.Authenticate(ctx, demoAgentToken); !ok || principal.ID != h.salesStaff.ID {
		t.Fatalf("the other binding was affected: %+v %v", principal, ok)
	}
}

// Through the MCP server: the same bearer token is a different principal on
// the request after an OA switch, with no session in between.
func TestAgentIdentitySwitchAppliesToTheNextMCPRequest(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	h.register("lzj", demoAgentToken, h.staff.ID)
	server, err := mcp.NewServer(h.authenticator(), h.service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	whoami := func() map[string]any {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`))
		request.Header.Set("Authorization", "Bearer "+demoAgentToken)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		var response struct {
			Result struct {
				Structured map[string]any `json:"structuredContent"`
				IsError    bool           `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusOK || response.Result.IsError {
			t.Fatalf("whoami over MCP: %d %s %v", recorder.Code, recorder.Body.String(), err)
		}
		return response.Result.Structured
	}
	if got := whoami(); got["subject"] != "staff" || got["agent_label"] != "lzj" || got["acting_as"].(map[string]any)["subject"] != "staff" {
		t.Fatalf("before the switch whoami = %v", got)
	}
	if code := h.oaAgentIdentity(map[string]any{"event_id": "evt-switch-1", "action": "switch", "label": "lzj",
		"principal_id": h.salesStaff.ID, "actor": "procurement-admin"}, "").Code; code != http.StatusOK {
		t.Fatalf("switch status %d", code)
	}
	if got := whoami(); got["subject"] != "sales-staff" || got["principal_id"] != h.salesStaff.ID || got["bound_by"] != "procurement-admin" ||
		got["acting_as"].(map[string]any)["department"] != "sales" {
		t.Fatalf("after the switch whoami = %v", got)
	}
}

func TestOAAgentIdentityListsBindingsAndCandidates(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	h.register("lzj", demoAgentToken, h.staff.ID)

	if code := h.oaAgentIdentity(map[string]any{"event_id": "evt-list-bad", "action": "list"}, "deadbeef").Code; code != http.StatusUnauthorized {
		t.Fatalf("bad signature status %d, want 401", code)
	}
	if code := h.oaAgentIdentity(map[string]any{"event_id": "evt-list-unknown", "action": "list", "extra": true}, "").Code; code != http.StatusBadRequest {
		t.Fatalf("unknown field status %d, want 400", code)
	}
	listed := h.oaAgentIdentity(map[string]any{"event_id": "evt-list-1", "action": "list"}, "")
	if listed.Code != http.StatusOK || listed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status %d %s", listed.Code, listed.Body.String())
	}
	body := decodeBody(t, listed)
	wantIdentities := []any{map[string]any{"label": "lzj", "principal_id": h.staff.ID, "subject": "staff", "bound_by": "wmm",
		"bound_at": h.clock.value.UTC().Format(time.RFC3339)}}
	if !reflect.DeepEqual(body["identities"], wantIdentities) {
		t.Fatalf("identities %v, want %v", body["identities"], wantIdentities)
	}
	// Enabled query principals the snapshot knows, by subject: not carol
	// (auditor), ghost (unknown to the snapshot) or retired (disabled).
	wantCandidates := []any{
		map[string]any{"principal_id": h.alice.ID, "subject": "alice", "department": "finance", "job_family": "dept_admin", "center": ""},
		map[string]any{"principal_id": h.salesStaff.ID, "subject": "sales-staff", "department": "sales", "job_family": "staff", "center": "sales-b"},
		map[string]any{"principal_id": h.staff.ID, "subject": "staff", "department": "finance", "job_family": "staff", "center": "finance-a"},
	}
	if !reflect.DeepEqual(body["candidates"], wantCandidates) {
		t.Fatalf("candidates %v, want %v", body["candidates"], wantCandidates)
	}
}

func TestOAAgentIdentitySwitchRebindsAndRecordsHistory(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	h.register("lzj", demoAgentToken, h.staff.ID)
	h.clock.value = h.clock.value.Add(time.Minute)

	switched := h.oaAgentIdentity(map[string]any{"event_id": "evt-switch-1", "action": "switch", "label": "lzj",
		"principal_id": h.salesStaff.ID, "actor": "procurement-admin"}, "")
	if switched.Code != http.StatusOK {
		t.Fatalf("switch status %d %s", switched.Code, switched.Body.String())
	}
	want := map[string]any{"identity": map[string]any{"label": "lzj", "principal_id": h.salesStaff.ID, "subject": "sales-staff",
		"bound_by": "procurement-admin", "bound_at": h.clock.value.UTC().Format(time.RFC3339)}}
	if got := decodeBody(t, switched); !reflect.DeepEqual(got, want) {
		t.Fatalf("switch response %v, want %v", got, want)
	}
	stored, err := h.store.GetAgentIdentity(context.Background(), "lzj")
	if err != nil || stored.PrincipalID != h.salesStaff.ID || stored.TokenSHA256 != tokenSHA256(demoAgentToken) {
		t.Fatalf("stored binding %+v %v", stored, err)
	}
	wantHistory := [][3]string{{"", h.staff.ID, "wmm"}, {h.staff.ID, h.salesStaff.ID, "procurement-admin"}}
	if got := switchHistory(t, h, "lzj"); !reflect.DeepEqual(got, wantHistory) {
		t.Fatalf("switch history %v, want %v", got, wantHistory)
	}

	for name, body := range map[string]map[string]any{
		"auditor is not a candidate":   {"event_id": "evt-bad-1", "action": "switch", "label": "lzj", "principal_id": h.carol.ID, "actor": "procurement-admin"},
		"unknown subject":              {"event_id": "evt-bad-2", "action": "switch", "label": "lzj", "principal_id": "principal-ghost", "actor": "procurement-admin"},
		"disabled principal":           {"event_id": "evt-bad-3", "action": "switch", "label": "lzj", "principal_id": "principal-retired", "actor": "procurement-admin"},
		"no such principal":            {"event_id": "evt-bad-4", "action": "switch", "label": "lzj", "principal_id": "principal-nobody", "actor": "procurement-admin"},
		"unknown label":                {"event_id": "evt-bad-5", "action": "switch", "label": "nobody", "principal_id": h.staff.ID, "actor": "procurement-admin"},
		"empty actor":                  {"event_id": "evt-bad-6", "action": "switch", "label": "lzj", "principal_id": h.staff.ID, "actor": " "},
		"unknown action":               {"event_id": "evt-bad-7", "action": "delete", "label": "lzj"},
		"event id differs from header": {"event_id": "evt-bad-8", "action": "list"},
	} {
		var recorder *httptest.ResponseRecorder
		if name != "event id differs from header" {
			recorder = h.oaAgentIdentity(body, "")
		} else {
			raw, _ := json.Marshal(body)
			timestamp := strconv.FormatInt(h.clock.value.Unix(), 10)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/oa/agent-identity", bytes.NewReader(raw))
			request.Header.Set("X-OA-Event-ID", "evt-other")
			request.Header.Set("X-OA-Timestamp", timestamp)
			request.Header.Set("X-OA-Signature", approval.Sign([]byte(h.secret), timestamp, raw))
			recorder = httptest.NewRecorder()
			h.service.OAAgentIdentityHandler().ServeHTTP(recorder, request)
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d %s, want 400", name, recorder.Code, recorder.Body.String())
		}
		if _, ok := decodeBody(t, recorder)["error"].(string); !ok {
			t.Fatalf("%s: no error message: %s", name, recorder.Body.String())
		}
	}
	if got := switchHistory(t, h, "lzj"); len(got) != 2 {
		t.Fatalf("a refused switch was recorded: %v", got)
	}
}

func TestAgentIdentityEndpointsAreAbsentWithoutTheCatalogBlock(t *testing.T) {
	h := newAgentIdentityHarness(t, false)
	if code := h.oaAgentIdentity(map[string]any{"event_id": "evt-list-1", "action": "list"}, "").Code; code != http.StatusNotFound {
		t.Fatalf("OA endpoint without the block: %d, want 404", code)
	}
	body := `{"label":"lzj","token_sha256":"` + tokenSHA256(demoAgentToken) + `","principal_id":"principal-staff","by":"wmm"}`
	if code := h.admin(body).Code; code != http.StatusNotFound {
		t.Fatalf("admin endpoint without the block: %d, want 404", code)
	}
	if identities, err := h.store.ListAgentIdentities(context.Background()); err != nil || len(identities) != 0 {
		t.Fatalf("identities %v %v", identities, err)
	}
}

func TestAdminAgentIdentityRegistersADigest(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	digest := tokenSHA256(demoAgentToken)
	registered := h.admin(`{"label":"lzj","token_sha256":"` + digest + `","principal_id":"principal-staff","by":"wmm"}`)
	if registered.Code != http.StatusOK {
		t.Fatalf("register status %d %s", registered.Code, registered.Body.String())
	}
	want := map[string]any{"identity": map[string]any{"label": "lzj", "principal_id": h.staff.ID, "subject": "staff",
		"bound_by": "wmm", "bound_at": h.clock.value.UTC().Format(time.RFC3339)}}
	if got := decodeBody(t, registered); !reflect.DeepEqual(got, want) {
		t.Fatalf("register response %v, want %v", got, want)
	}
	if principal, ok := h.authenticator().Authenticate(context.Background(), demoAgentToken); !ok || principal.ID != h.staff.ID || principal.AgentLabel != "lzj" {
		t.Fatalf("registered token authenticated as %+v %v", principal, ok)
	}
	// Re-registering the label rebinds it and keeps its history.
	if code := h.admin(`{"label":"lzj","token_sha256":"` + digest + `","principal_id":"principal-sales-staff","by":"wmm"}`).Code; code != http.StatusOK {
		t.Fatalf("re-register status %d", code)
	}
	if got := switchHistory(t, h, "lzj"); !reflect.DeepEqual(got, [][3]string{{"", h.staff.ID, "wmm"}, {h.staff.ID, h.salesStaff.ID, "wmm"}}) {
		t.Fatalf("history %v", got)
	}

	for name, test := range map[string]struct {
		body string
		code int
	}{
		"principal does not exist":   {`{"label":"x1","token_sha256":"` + tokenSHA256("t1") + `","principal_id":"principal-nobody","by":"wmm"}`, http.StatusBadRequest},
		"auditor principal":          {`{"label":"x2","token_sha256":"` + tokenSHA256("t2") + `","principal_id":"principal-carol","by":"wmm"}`, http.StatusBadRequest},
		"disabled principal":         {`{"label":"x3","token_sha256":"` + tokenSHA256("t3") + `","principal_id":"principal-retired","by":"wmm"}`, http.StatusBadRequest},
		"uppercase digest":           {`{"label":"x4","token_sha256":"` + strings.ToUpper(tokenSHA256("t4")) + `","principal_id":"principal-staff","by":"wmm"}`, http.StatusBadRequest},
		"bad label":                  {`{"label":"X Y","token_sha256":"` + tokenSHA256("t5") + `","principal_id":"principal-staff","by":"wmm"}`, http.StatusBadRequest},
		"missing by":                 {`{"label":"x6","token_sha256":"` + tokenSHA256("t6") + `","principal_id":"principal-staff"}`, http.StatusBadRequest},
		"unknown field (a token)":    {`{"label":"x7","token":"secret","principal_id":"principal-staff","by":"wmm"}`, http.StatusBadRequest},
		"digest under another label": {`{"label":"other","token_sha256":"` + digest + `","principal_id":"principal-staff","by":"wmm"}`, http.StatusConflict},
	} {
		if recorder := h.admin(test.body); recorder.Code != test.code {
			t.Fatalf("%s: status %d %s, want %d", name, recorder.Code, recorder.Body.String(), test.code)
		}
	}
	identities, err := h.store.ListAgentIdentities(context.Background())
	if err != nil || len(identities) != 1 || identities[0].Label != "lzj" {
		t.Fatalf("refused registrations were stored: %+v %v", identities, err)
	}
}

func TestWhoamiTellsTheAgentWhoItIs(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	listed := false
	for _, tool := range h.service.ListTools(h.staff) {
		listed = listed || tool.Name == whoamiTool.Name
	}
	if !listed {
		t.Fatal("whoami is not listed under the Catalog block")
	}
	got := mustCallGatewayTool(t, h.service, h.staff, "whoami", map[string]any{})
	attributes := map[string]any{"department": []string{"finance"}, "job_family": []string{"staff"}, "center": []string{"finance-a"},
		"clearance": []string{"internal", "public"}, "authority_departments": []string{}, "employee_id": []string{"staff"},
		"display_name": []string{"周燕"}, "title": []string{"会计"}, "employee_no": []string{"E0007"}}
	if got["principal_id"] != h.staff.ID || got["subject"] != "staff" || got["role"] != "query" || got["catalog_version"] != h.catalog.CatalogVersion ||
		!reflect.DeepEqual(got["attributes"], attributes) {
		t.Fatalf("whoami %v", got)
	}
	// A subject without a name, post or staff number reports each empty,
	// like any other attribute it lacks.
	sales := mustCallGatewayTool(t, h.service, h.salesStaff, "whoami", map[string]any{})["attributes"].(map[string]any)
	for _, key := range []string{"display_name", "title", "employee_no", "employee_id"} {
		if !reflect.DeepEqual(sales[key], []string{}) {
			t.Fatalf("sales-staff whoami %s = %v, want []", key, sales[key])
		}
	}
	for _, absent := range []string{"agent_label", "bound_by", "bound_at"} {
		if _, ok := got[absent]; ok {
			t.Fatalf("a static token's whoami carries %s: %v", absent, got)
		}
	}

	h.register("lzj", demoAgentToken, h.staff.ID)
	bound := h.staff
	bound.AgentLabel = "lzj"
	got = mustCallGatewayTool(t, h.service, bound, "whoami", map[string]any{})
	if got["agent_label"] != "lzj" || got["bound_by"] != "wmm" || got["bound_at"] != h.clock.value.UTC().Format(time.RFC3339) {
		t.Fatalf("a bound token's whoami %v", got)
	}
	// Switched since this request authenticated: the binding now describes
	// another principal, so its bound_by and bound_at are left out.
	if _, err := h.store.SwitchAgentIdentity(context.Background(), "lzj", h.salesStaff.ID, "procurement-admin", h.clock.value); err != nil {
		t.Fatal(err)
	}
	got = mustCallGatewayTool(t, h.service, bound, "whoami", map[string]any{})
	if _, ok := got["bound_by"]; ok || got["agent_label"] != "lzj" || got["subject"] != "staff" {
		t.Fatalf("a stale binding's whoami %v", got)
	}
	_, err := callGatewayTool(h.service, h.staff, "whoami", map[string]any{"as": "alice"})
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}

func TestWhoamiIsAbsentWithoutTheCatalogBlock(t *testing.T) {
	h := newAgentIdentityHarness(t, false)
	for _, tool := range h.service.ListTools(h.staff) {
		if tool.Name == whoamiTool.Name {
			t.Fatal("whoami is listed without the Catalog block")
		}
	}
	_, err := callGatewayTool(h.service, h.staff, "whoami", map[string]any{})
	requireToolCode(t, err, apierr.CodeForbidden)
	// Exactly what any other unlisted tool gets.
	_, unlisted := callGatewayTool(h.service, h.staff, "no_such_tool", map[string]any{})
	if !reflect.DeepEqual(err, unlisted) {
		t.Fatalf("whoami refusal %v differs from an unlisted tool's %v", err, unlisted)
	}
}

func TestActingAsIsAddedToStructuredResults(t *testing.T) {
	h := newAgentIdentityHarness(t, true)
	bound := h.staff
	bound.AgentLabel = "lzj"
	listed := mustCallGatewayTool(t, h.service, bound, "list_data_products", map[string]any{})
	want := map[string]any{"principal_id": h.staff.ID, "subject": "staff", "agent_label": "lzj", "department": "finance", "job_family": "staff",
		"display_name": "周燕"}
	if !reflect.DeepEqual(listed["acting_as"], want) {
		t.Fatalf("acting_as %v, want %v", listed["acting_as"], want)
	}
	// Everything else is the tool's own result, Go types included.
	direct, err := h.service.listDataProducts(context.Background(), bound, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	withoutActing := make(map[string]any, len(listed))
	for key, value := range listed {
		if key != "acting_as" {
			withoutActing[key] = value
		}
	}
	if !reflect.DeepEqual(withoutActing, direct) {
		t.Fatal("list_data_products under acting_as differs from the tool's own result")
	}
	if _, ok := listed["products"].([]map[string]any); !ok {
		t.Fatalf("products became %T", listed["products"])
	}
	// A static principal: no agent_label; alice has no center and no
	// display_name but a department.
	if got := mustCallGatewayTool(t, h.service, h.alice, "get_sql_capabilities", map[string]any{})["acting_as"]; !reflect.DeepEqual(got,
		map[string]any{"principal_id": h.alice.ID, "subject": "alice", "department": "finance", "job_family": "dept_admin"}) {
		t.Fatalf("alice acting_as %v", got)
	}
	// Errors carry no acting_as: they are not structured results.
	if _, err := callGatewayTool(h.service, bound, "get_task_status", map[string]any{"task_id": "task-nobody"}); err == nil {
		t.Fatal("an unknown task's status succeeded")
	}
}

func TestActingAsKeepsNumbersAndTextByteForByte(t *testing.T) {
	acting := map[string]any{"principal_id": "p", "subject": "s"}
	strip := func(t *testing.T, wrapped any) []byte {
		t.Helper()
		object, ok := wrapped.(map[string]any)
		if !ok || !reflect.DeepEqual(object["acting_as"], acting) {
			t.Fatalf("wrapped %T %v", wrapped, wrapped)
		}
		copy := make(map[string]any, len(object))
		for key, value := range object {
			if key != "acting_as" {
				copy[key] = value
			}
		}
		encoded, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	const big = int64(9007199254740993) // 2^53 + 1: not representable as float64

	// A map result (every tool's) is copied, never re-encoded.
	mapResult := map[string]any{"big": big, "half": 0.5, "text": "<a&b>", "nested": map[string]any{"big": big, "list": []any{big, 0.5}}}
	original, err := json.Marshal(mapResult)
	if err != nil {
		t.Fatal(err)
	}
	if encoded := strip(t, withActingAs(mapResult, acting)); !bytes.Equal(encoded, original) {
		t.Fatalf("map result changed:\n%s\n%s", encoded, original)
	}
	if _, ok := mapResult["acting_as"]; ok {
		t.Fatal("the tool's own map was mutated")
	}

	// Any other object is re-decoded with json.Number. Fields are declared in
	// key order so the struct's encoding and the map's coincide.
	type inner struct {
		Big  int64   `json:"big"`
		Half float64 `json:"half"`
	}
	structResult := struct {
		Big    int64   `json:"big"`
		Half   float64 `json:"half"`
		Nested inner   `json:"nested"`
		Small  float64 `json:"small"`
		Text   string  `json:"text"`
	}{Big: big, Half: 0.5, Nested: inner{Big: -big, Half: 0.5}, Small: 1e-7, Text: "<a&b>"}
	original, err = json.Marshal(structResult)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), "9007199254740993") || !strings.Contains(string(original), "0.5") {
		t.Fatalf("fixture lost its numbers: %s", original)
	}
	if encoded := strip(t, withActingAs(structResult, acting)); !bytes.Equal(encoded, original) {
		t.Fatalf("struct result changed:\n%s\n%s", encoded, original)
	}

	// A result whose JSON is not an object is returned unchanged.
	array := []int64{big}
	if got := withActingAs(array, acting); !reflect.DeepEqual(got, array) {
		t.Fatalf("array result became %v", got)
	}
}

// Without the Catalog block a structured result is the tool's own value: no
// acting_as, the same Go value, the same JSON bytes.
func TestActingAsIsAbsentWithoutTheCatalogBlock(t *testing.T) {
	h := newAgentIdentityHarness(t, false)
	result, err := h.service.CallTool(context.Background(), h.staff, "list_data_products", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	listed, ok := result.Structured.(map[string]any)
	if !ok {
		t.Fatalf("structured %T", result.Structured)
	}
	if _, ok := listed["acting_as"]; ok {
		t.Fatal("acting_as without the Catalog block")
	}
	direct, err := h.service.listDataProducts(context.Background(), h.staff, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Structured, direct) {
		t.Fatal("list_data_products without the block differs from the tool's own result")
	}
	got, _ := json.Marshal(result.Structured)
	want, _ := json.Marshal(direct)
	if !bytes.Equal(got, want) {
		t.Fatal("list_data_products JSON without the block differs from the tool's own JSON")
	}
}
