package oademo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

func TestOAManifestDigestMatchesSharedFixedVector(t *testing.T) {
	encoded, err := os.ReadFile("../../testdata/authorization-manifest-v1-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Manifest approval.AuthorizationManifestV1 `json:"manifest"`
		Digest   string                           `json:"digest"`
	}
	if err := json.Unmarshal(encoded, &vector); err != nil {
		t.Fatal(err)
	}
	digest, err := approval.ManifestDigest(vector.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if digest != vector.Digest {
		t.Fatalf("OA manifest digest = %s, shared vector = %s", digest, vector.Digest)
	}
}

func TestCreateDraftAuthenticatesAndDisplaysCompleteManifest(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()

	server, err := New(Config{
		ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, AlicePassword: "alice-pass", BobPassword: "bob-pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	draftRequest := testDraftRequest(t)
	bodyBytes, err := json.Marshal(draftRequest)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(bodyBytes)))
	request.Header.Set("Authorization", "Bearer wrong")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.StatusCode)
	}

	tampered := draftRequest
	tampered.Manifest = cloneManifest(draftRequest.Manifest)
	tampered.Manifest.ApprovedColumns["expense_summary"] = []string{"month", "department", "total_amount"}
	tamperedBody, _ := json.Marshal(tampered)
	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(tamperedBody)))
	request.Header.Set("Authorization", "Bearer service")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered manifest status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(bodyBytes)))
	request.Header.Set("Authorization", "Bearer service")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create draft status = %d", response.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil || created["draft_id"] == "" {
		t.Fatalf("created = %#v, err=%v", created, err)
	}
	draftID := created["draft_id"].(string)
	server.mu.RLock()
	stored := cloneDraft(server.drafts[draftID])
	server.mu.RUnlock()
	if stored.ManifestDigest != draftRequest.ManifestDigest || stored.Manifest.Budget.MaxResultRows != 500 || len(stored.Manifest.ApprovedColumns["expense_summary"]) != 2 {
		t.Fatalf("stored OA authorization manifest is incomplete: %+v", stored)
	}

	page := httptest.NewRecorder()
	server.render(page, "task", map[string]any{
		"Session": sessionData{Username: "alice", Role: "requester", CSRF: "csrf"},
		"Draft":   stored,
	})
	for _, visible := range []string{
		"summarize travel expenses", "expense_summary", "total_amount", "department", "high",
		"agent:expense-research", "alice", "10", "500", "30000", "5000", "1800000",
		draftRequest.ManifestDigest,
	} {
		if !strings.Contains(page.Body.String(), visible) {
			t.Fatalf("OA detail page does not display %q: %s", visible, page.Body.String())
		}
	}
}

func TestNarrowedGrantFromFormContractsScopeColumnsAndBudgets(t *testing.T) {
	request := testDraftRequest(t)
	draft := &Draft{Manifest: request.Manifest, ManifestDigest: request.ManifestDigest, ApprovalMode: "manual", Approver: "bob"}
	issuedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	form := url.Values{
		"approved_products":    {`["expense_summary"]`},
		"approved_columns":     {`{"expense_summary":["total_amount"]}`},
		"mandatory_scope":      {`{"department":["销售部"]}`},
		"max_queries":          {"2"},
		"max_result_rows":      {"100"},
		"max_db_ms":            {"10000"},
		"per_query_timeout_ms": {"2000"},
		"task_ttl_ms":          {"600000"},
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/decision", strings.NewReader(form.Encode()))
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := httpRequest.ParseForm(); err != nil {
		t.Fatal(err)
	}
	grant, err := narrowedGrantFromForm(draft, httpRequest, issuedAt)
	if err != nil {
		t.Fatalf("safe narrowing rejected: %v", err)
	}
	if len(grant.ApprovedProducts) != 1 || grant.Budget.MaxQueries != 2 || grant.Budget.PerQueryTimeoutMS != 2000 ||
		grant.Budget.MaxReleaseFacts != request.Manifest.Budget.MaxReleaseFacts ||
		grant.Budget.MaxInfluenceFacts != request.Manifest.Budget.MaxInfluenceFacts ||
		grant.Budget.MaxOutcomeFacts != request.Manifest.Budget.MaxOutcomeFacts ||
		grant.Budget.ExposureProfileVersion != request.Manifest.Budget.ExposureProfileVersion ||
		!grant.ExpiresAt.Equal(issuedAt.Add(10*time.Minute)) {
		t.Fatalf("unexpected narrowed grant: %+v", grant)
	}

	form.Set("max_queries", "11")
	httpRequest = httptest.NewRequest(http.MethodPost, "/decision", strings.NewReader(form.Encode()))
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = httpRequest.ParseForm()
	if _, err := narrowedGrantFromForm(draft, httpRequest, issuedAt); err == nil {
		t.Fatal("budget expansion was accepted")
	}
}

func testDraftRequest(t *testing.T) DraftRequest {
	t.Helper()
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: "task-1",
		HumanSubject: "alice", AgentID: "agent:expense-research",
		DeclaredObjective: "summarize travel expenses", Products: []string{"expense_summary"},
		ApprovedColumns: map[string][]string{"expense_summary": {"month", "total_amount"}},
		MandatoryScope:  map[string]any{"department": []string{"销售部", "市场部"}},
		Sensitivity:     domain.SensitivityHigh,
		Budget: approval.AuthorizationBudgetV1{
			MaxQueries: 10, MaxResultRows: 500, MaxDBMS: 30_000,
			PerQueryTimeoutMS: 5_000, TaskTTLMS: 1_800_000,
			MaxReleaseFacts: 100, MaxInfluenceFacts: 500,
			ExposureProfileVersion: exposure.ProfileV2,
		},
		CatalogVersion: "v1", CatalogSHA256: strings.Repeat("a", 64),
		DatasourceID: "taskgate-test-expenses", SchemaDigest: strings.Repeat("b", 64),
		CallbackContext: "callback-1", Nonce: "000102030405060708090a0b0c0d0e0f",
	}
	digest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return DraftRequest{Manifest: manifest, ManifestDigest: digest, ApprovalMode: "manual", Approver: "bob"}
}

func TestValidateDraftRequestRejectsAutomaticApproval(t *testing.T) {
	request := testDraftRequest(t)
	request.ApprovalMode = "auto"
	request.Approver = ""
	if err := (&Server{users: map[string]user{}}).validateDraftRequest(request); err == nil {
		t.Fatal("OA accepted an automatic approval draft")
	}
}
