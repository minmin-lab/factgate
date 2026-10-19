package oademo

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// A second approver (dana) is configured by password; each approver sees and
// decides only the drafts that name them, so bob can neither see nor decide a
// draft the Catalog routed to dana.

func TestApproversAreConfiguredAndOnlyTheNamedOneDecides(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer callback.Close()
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, AlicePassword: "alice-pass", BobPassword: "bob-pass", DanaPassword: "dana-pass"})
	if err != nil {
		t.Fatal(err)
	}
	for approver, admitted := range map[string]bool{"bob": true, "dana": true, "alice": false, "erin": false} {
		request := testDraftRequest(t)
		request.Approver = approver
		if err := server.validateDraftRequest(request); (err == nil) != admitted {
			t.Fatalf("approver %q admitted=%v, want %v (%v)", approver, err == nil, admitted, err)
		}
	}
	withoutDana, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, AlicePassword: "alice-pass", BobPassword: "bob-pass"})
	if err != nil {
		t.Fatal(err)
	}
	request := testDraftRequest(t)
	request.Approver = "dana"
	if err := withoutDana.validateDraftRequest(request); err == nil {
		t.Fatal("an OA without dana accepted a draft naming dana")
	}

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	create := func(approver string) string {
		t.Helper()
		request := testDraftRequest(t)
		request.Approver = approver
		body, _ := json.Marshal(request)
		post, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(body)))
		post.Header.Set("Authorization", "Bearer service")
		response, err := http.DefaultClient.Do(post)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var created map[string]any
		if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&created) != nil {
			t.Fatalf("create draft for %s: status %d", approver, response.StatusCode)
		}
		return created["draft_id"].(string)
	}
	bobDraft, danaDraft := create("bob"), create("dana")
	alice := login(t, httpServer.URL, "alice", "alice-pass")
	for _, draft := range []string{bobDraft, danaDraft} {
		if status := act(t, alice, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
			t.Fatalf("alice submit %s: status %d", draft, status)
		}
	}
	bob := login(t, httpServer.URL, "bob", "bob-pass")
	list := get(t, bob, httpServer.URL+"/tasks")
	if !strings.Contains(list, bobDraft) || strings.Contains(list, danaDraft) {
		t.Fatal("bob's task list does not show exactly the drafts naming bob")
	}
	if status := decide(t, bob, httpServer.URL, danaDraft, server); status == http.StatusOK {
		t.Fatal("bob decided a draft routed to dana")
	}
	dana := login(t, httpServer.URL, "dana", "dana-pass")
	if status := decide(t, dana, httpServer.URL, danaDraft, server); status != http.StatusOK {
		t.Fatalf("dana could not decide her draft: status %d", status)
	}
	server.mu.RLock()
	state := server.drafts[danaDraft].State
	server.mu.RUnlock()
	if state != "approved" {
		t.Fatalf("dana's draft state %q, want approved", state)
	}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func login(t *testing.T, base, username, password string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	page := get(t, client, base+"/login")
	match := csrfField.FindStringSubmatch(page)
	if match == nil {
		t.Fatal("login page has no CSRF token")
	}
	response, err := client.PostForm(base+"/login", url.Values{"csrf": {match[1]}, "username": {username}, "password": {password}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if strings.HasSuffix(response.Request.URL.Path, "/login") {
		t.Fatalf("login as %s failed", username)
	}
	return client
}

func get(t *testing.T, client *http.Client, target string) string {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body)
}

// act posts a draft action with the CSRF token of the user's task list page
// (the session token), returning the final status.
func act(t *testing.T, client *http.Client, base, draft, action string, values url.Values) int {
	t.Helper()
	match := csrfField.FindStringSubmatch(get(t, client, base+"/tasks"))
	if match == nil {
		t.Fatal("task list has no CSRF token")
	}
	form := url.Values{"csrf": {match[1]}}
	for key, value := range values {
		form[key] = value
	}
	response, err := client.PostForm(base+"/tasks/"+draft+"/"+action, form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}

func decide(t *testing.T, client *http.Client, base, draft string, _ *Server) int {
	t.Helper()
	return act(t, client, base, draft, "decision", url.Values{"decision": {"approved"}})
}
