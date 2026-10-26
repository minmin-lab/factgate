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

	"taskbound.local/agent-data-gateway/internal/approval"
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
	if status := decide(t, bob, httpServer.URL, danaDraft, server); status != http.StatusForbidden {
		t.Fatalf("bob deciding a draft routed to dana: status %d, want 403", status)
	}
	if status, body := getStatus(t, bob, httpServer.URL+"/tasks/"+danaDraft); status != http.StatusForbidden || !strings.Contains(body, "dana") {
		t.Fatalf("bob opening dana's draft: status %d body %q, want 403 naming dana", status, body)
	}
	if status, _ := getStatus(t, bob, httpServer.URL+"/tasks/no-such-draft"); status != http.StatusNotFound {
		t.Fatalf("a missing draft: status %d, want 404", status)
	}
	dana := login(t, httpServer.URL, "dana", "dana-pass")
	if status := decide(t, dana, httpServer.URL, danaDraft, server); status != http.StatusOK {
		t.Fatalf("dana could not decide her draft: status %d", status)
	}
	if status := decide(t, dana, httpServer.URL, danaDraft, server); status != http.StatusConflict {
		t.Fatalf("deciding an already decided draft: status %d, want 409", status)
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

// getStatus is get with the final status.
func getStatus(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
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

// An extra requester (a colleague's own account) logs in and submits only the
// drafts whose human subject they are; malformed or colliding extra users are
// refused at start.
func TestExtraRequesterSubmitsOwnDrafts(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer callback.Close()
	base := Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session", CallbackURL: callback.URL,
		AlicePassword: "alice-pass", BobPassword: "bob-pass"}
	for _, bad := range [][]ExtraUser{{{Name: "bob", Role: "requester", Password: "x"}}, {{Name: "lzj", Role: "admin", Password: "x"}}, {{Name: "lzj", Role: "requester"}}} {
		config := base
		config.ExtraUsers = bad
		if _, err := New(config); err == nil {
			t.Fatalf("accepted extra users %+v", bad)
		}
	}
	config := base
	config.ExtraUsers = []ExtraUser{{Name: "lzj", Role: "requester", Password: "lzj-pass"}}
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	if page := get(t, http.DefaultClient, httpServer.URL+"/login"); !strings.Contains(page, `value="lzj"`) {
		t.Fatal("the login page does not offer lzj")
	}
	request := testDraftRequest(t)
	request.Manifest.HumanSubject = "lzj"
	digest, err := approval.ManifestDigest(request.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	request.ManifestDigest = digest
	body, _ := json.Marshal(request)
	post, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(body)))
	post.Header.Set("Authorization", "Bearer service")
	response, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&created) != nil {
		t.Fatalf("create lzj draft: %d", response.StatusCode)
	}
	response.Body.Close()
	draft := created["draft_id"].(string)
	alice := login(t, httpServer.URL, "alice", "alice-pass")
	if status := act(t, alice, httpServer.URL, draft, "submit", nil); status != http.StatusForbidden {
		t.Fatalf("alice submitting lzj's draft: status %d, want 403", status)
	}
	lzj := login(t, httpServer.URL, "lzj", "lzj-pass")
	if status := act(t, lzj, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("lzj could not submit their draft: %d", status)
	}
}

// The demo stack offers only its role accounts (KB1-LOOP-144): without the
// built-in users, alice, bob and dana are neither offered nor able to log in,
// and the OA refuses to start without both a requester and an approver.
func TestOAWithoutBuiltinUsersOffersOnlyItsRoleAccounts(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer callback.Close()
	base := Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session", CallbackURL: callback.URL,
		OmitBuiltinUsers: true}
	for _, lacking := range [][]ExtraUser{
		{{Name: "staff", Role: "requester", Password: "shared"}},
		{{Name: "finance-director", Role: "approver", Password: "shared"}},
	} {
		config := base
		config.ExtraUsers = lacking
		if _, err := New(config); err == nil {
			t.Fatalf("an OA with only %+v started", lacking)
		}
	}
	config := base
	config.ExtraUsers = []ExtraUser{{Name: "staff", Role: "requester", Password: "shared", DisplayName: "普通员工（财务部）"},
		{Name: "finance-director", Role: "approver", Password: "shared", DisplayName: "财务部主任"}}
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	page := get(t, http.DefaultClient, httpServer.URL+"/login")
	for _, gone := range []string{`value="alice"`, `value="bob"`, `value="dana"`} {
		if strings.Contains(page, gone) {
			t.Fatalf("the login page still offers %s", gone)
		}
	}
	if !strings.Contains(page, `value="staff"`) || !strings.Contains(page, "财务部主任") {
		t.Fatal("the login page does not offer the role accounts")
	}
	login(t, httpServer.URL, "staff", "shared")
	login(t, httpServer.URL, "finance-director", "shared")
	jarless := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	match := csrfField.FindStringSubmatch(page)
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/login",
		strings.NewReader(url.Values{"csrf": {match[1]}, "username": {"alice"}, "password": {"alice-pass"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "oa_login_csrf", Value: match[1]})
	response, err := jarless.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if location := response.Header.Get("Location"); !strings.Contains(location, "/login?error=invalid") {
		t.Fatalf("alice logging in to an OA without built-in users: redirected to %q", location)
	}
}
