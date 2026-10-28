package oademo

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// The delegated-authorization demo's OA (KB1-LOOP-147): role accounts that
// both request and approve, an issuer who submits the draft they issued, an
// account switcher in the shared-password mode, a history page, Chinese
// labels and a checkbox narrowing form.

func delegatedOA(t *testing.T, allowSwitch bool) (*Server, *httptest.Server, *fakeGateway) {
	t.Helper()
	gateway := &fakeGateway{answers: map[string]fakeAnswer{}}
	callback := httptest.NewServer(gateway)
	t.Cleanup(callback.Close)
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, OmitBuiltinUsers: true, AllowSwitch: allowSwitch,
		Labels: Labels{Scopes: map[string]string{"department": "部门"}, Values: map[string]map[string]string{"department": {"销售部": "销售部（Sales）"}},
			Products: map[string]string{"expense_summary": "费用汇总"},
			Groups:   []AccountGroup{{Label: "管理线", Members: []string{"leader-admin", "finance-admin"}}, {Label: "业务线", Members: []string{"sales-admin"}}}},
		ExtraUsers: []ExtraUser{
			{Name: "staff", Role: "both", Password: "shared", DisplayName: "普通员工"},
			{Name: "finance-admin", Role: "approver", Password: "shared", DisplayName: "财务部管理员"},
			{Name: "sales-admin", Role: "approver", Password: "shared", DisplayName: "销售部管理员"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return server, httpServer, gateway
}

// staffDraft is a draft whose human subject is staff; approver and issuer as given.
func staffDraft(t *testing.T, approver, issuedBy string, ttl time.Duration) DraftRequest {
	t.Helper()
	request := testDraftRequest(t)
	request.Manifest.HumanSubject = "staff"
	request.Manifest.Budget.TaskTTLMS = ttl.Milliseconds()
	digest, err := approval.ManifestDigest(request.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	request.ManifestDigest, request.Approver, request.IssuedBy = digest, approver, issuedBy
	return request
}

func TestABothAccountSubmitsAndConfirmsItsOwnDraft(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, false)
	draft := createDraft(t, httpServer.URL, staffDraft(t, "staff", "", 24*time.Hour))
	staff := login(t, httpServer.URL, "staff", "shared")
	list := get(t, staff, httpServer.URL+"/tasks")
	if !strings.Contains(list, draft) || !strings.Contains(list, "普通员工（staff） 申请 部门 销售部（Sales）、市场部 的资料（费用汇总）") ||
		!strings.Contains(list, "有效期 1 天") || !strings.Contains(list, "待提交") {
		t.Fatalf("staff's to-do list: %s", list)
	}
	page := get(t, staff, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, "提交申请") || strings.Contains(page, `name="decision" value="approved"`) {
		t.Fatalf("an unsubmitted draft offers the wrong actions: %s", page)
	}
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("staff submit: %d", status)
	}
	page = get(t, staff, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, ">确认<") || !strings.Contains(page, "等待 普通员工（staff）本人确认") {
		t.Fatalf("a self-confirmed draft does not read as one: %s", page)
	}
	if status := act(t, staff, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("staff confirm: %d", status)
	}
	settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	if event := gateway.event(t, draft, "staff", "approved"); event.ApprovalReceipt == nil || event.ApprovalReceipt.ApproverID != "staff" {
		t.Fatalf("confirmation receipt %+v", event.ApprovalReceipt)
	}
	// Settled drafts leave the to-do list and appear in the history.
	if list := get(t, staff, httpServer.URL+"/tasks"); strings.Contains(list, draft) {
		t.Fatal("a settled draft is still a to-do")
	}
	if history := get(t, staff, httpServer.URL+"/history"); !strings.Contains(history, draft) || !strings.Contains(history, "已批准") || !strings.Contains(history, "普通员工（staff） 批准（") {
		t.Fatalf("history: %s", history)
	}
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, settled.ApprovalReceipt.ReceiptID) {
		t.Fatal("the settled draft's page does not carry its receipt id")
	}
	// An approver-only account that the draft never named sees nothing of it.
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	if status, _ := getStatus(t, admin, httpServer.URL+"/tasks/"+draft); status != http.StatusForbidden {
		t.Fatalf("a stranger opening the draft = %d", status)
	}
	if history := get(t, admin, httpServer.URL+"/history"); strings.Contains(history, draft) {
		t.Fatal("a stranger's history lists the draft")
	}
}

func TestTheIssuerSubmitsAndApprovesTheDraftTheyIssued(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, false)
	draft := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "finance-admin", 365*24*time.Hour))
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	list := get(t, admin, httpServer.URL+"/tasks")
	if !strings.Contains(list, draft) || !strings.Contains(list, "项目（长期授权）") || !strings.Contains(list, "有效期 1 年") || !strings.Contains(list, "由 财务部管理员（finance-admin） 代开") {
		t.Fatalf("the issuer's to-do list: %s", list)
	}
	if status := act(t, admin, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("the issuer submitting: %d", status)
	}
	if event := gateway.event(t, draft, "finance-admin", "submitted"); event.Status != "submitted" {
		t.Fatalf("submit event %+v", event)
	}
	if status := act(t, admin, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("the issuer approving: %d", status)
	}
	awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	// The employee sees their issued authorization in the history; another
	// administrator does not.
	staff := login(t, httpServer.URL, "staff", "shared")
	if history := get(t, staff, httpServer.URL+"/history"); !strings.Contains(history, draft) || !strings.Contains(history, "代开人 finance-admin") {
		t.Fatalf("the employee's history: %s", history)
	}
	other := login(t, httpServer.URL, "sales-admin", "shared")
	if status, _ := getStatus(t, other, httpServer.URL+"/tasks/"+draft); status != http.StatusForbidden {
		t.Fatalf("another administrator opening the draft = %d", status)
	}
	// An ordinary draft cannot be submitted by an approver who did not issue it.
	plain := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	if status := act(t, admin, httpServer.URL, plain, "submit", nil); status != http.StatusForbidden {
		t.Fatalf("an approver submitting a draft they did not issue = %d", status)
	}
}

func TestAccountSwitchIsOfferedOnlyInTheSharedPasswordMode(t *testing.T) {
	_, httpServer, _ := delegatedOA(t, true)
	staff := login(t, httpServer.URL, "staff", "shared")
	page := get(t, staff, httpServer.URL+"/tasks")
	if !strings.Contains(page, `action="/switch"`) || !strings.Contains(page, `<option value="staff" selected>`) || !strings.Contains(page, `<script src="/switch.js">`) {
		t.Fatalf("no account switcher: %s", page)
	}
	if status, script := getStatus(t, http.DefaultClient, httpServer.URL+"/switch.js"); status != http.StatusOK || !strings.Contains(script, `form.submit()`) {
		t.Fatalf("switch script %d: %s", status, script)
	}
	// The switcher is grouped by business line: the ungrouped tester first,
	// then each line in the labels file's order with the members it has.
	staffAt, adminGroup, salesGroup := strings.Index(page, `<option value="staff"`), strings.Index(page, `<optgroup label="管理线">`), strings.Index(page, `<optgroup label="业务线">`)
	if staffAt < 0 || adminGroup < staffAt || salesGroup < adminGroup || !strings.Contains(page, `<optgroup label="管理线"><option value="finance-admin">`) ||
		!strings.Contains(page, `<optgroup label="业务线"><option value="sales-admin">`) {
		t.Fatalf("the switcher is not grouped: %s", page)
	}
	match := csrfField.FindStringSubmatch(page)
	response, err := staff.PostForm(httpServer.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"finance-admin"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	page = get(t, staff, httpServer.URL+"/tasks")
	if !strings.Contains(page, "finance-admin · 审批人") || !strings.Contains(page, `<option value="finance-admin" selected>`) {
		t.Fatalf("after the switch: %s", page)
	}
	match = csrfField.FindStringSubmatch(page) // the session, and so its token, changed
	response, err = staff.PostForm(httpServer.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"nobody"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("switching to an unknown account = %d", response.StatusCode)
	}

	_, fixed, _ := delegatedOA(t, false)
	staff = login(t, fixed.URL, "staff", "shared")
	page = get(t, staff, fixed.URL+"/tasks")
	if strings.Contains(page, `action="/switch"`) {
		t.Fatal("the switcher is offered outside the shared-password mode")
	}
	match = csrfField.FindStringSubmatch(page)
	response, err = staff.PostForm(fixed.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"finance-admin"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("switch outside the shared-password mode = %d", response.StatusCode)
	}
}

func TestCheckboxNarrowingMatchesTheJSONForm(t *testing.T) {
	request := testDraftRequest(t)
	draft := &Draft{Manifest: request.Manifest, ManifestDigest: request.ManifestDigest, ApprovalMode: "manual", Approver: "bob"}
	issuedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	parent, err := decisionBase(draft, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	budgets := url.Values{"max_queries": {"5"}, "max_result_rows": {"100"}, "max_db_ms": {"10000"}, "per_query_timeout_ms": {"2000"}, "task_ttl_ms": {"600000"}}
	boxes := url.Values{"approved_products": {"expense_summary"}, "column:expense_summary": {"total_amount"}, "scope:department": {"销售部"}}
	jsonForm := url.Values{"approved_products": {`["expense_summary"]`}, "approved_columns": {`{"expense_summary":["total_amount"]}`}, "mandatory_scope": {`{"department":["销售部"]}`}}
	grants := make([]approval.TaskGrantCoreV1, 0, 2)
	for _, form := range []url.Values{boxes, jsonForm} {
		for key, values := range budgets {
			form[key] = values
		}
		r := httptest.NewRequest(http.MethodPost, "/tasks/x/decision", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		grant, err := narrowedGrantFromForm(parent, r, issuedAt)
		if err != nil {
			t.Fatalf("narrowing from %v: %v", form, err)
		}
		grants = append(grants, grant)
	}
	left, _ := approval.GrantCoreDigest(grants[0])
	right, _ := approval.GrantCoreDigest(grants[1])
	if left != right || len(grants[0].ApprovedColumns["expense_summary"]) != 1 || grants[0].Budget.MaxQueries != 5 {
		t.Fatalf("checkbox grant %+v differs from the JSON grant %+v", grants[0], grants[1])
	}
	// Keeping a product without any column, or a scope without any value, is refused.
	for name, form := range map[string]url.Values{
		"no column": {"approved_products": {"expense_summary"}, "scope:department": {"销售部"}},
		"no value":  {"approved_products": {"expense_summary"}, "column:expense_summary": {"total_amount"}},
	} {
		for key, values := range budgets {
			form[key] = values
		}
		r := httptest.NewRequest(http.MethodPost, "/tasks/x/decision", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_ = r.ParseForm()
		if _, err := narrowedGrantFromForm(parent, r, issuedAt); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestHumanDurationAndKinds(t *testing.T) {
	for ms, want := range map[int64]string{1_800_000: "30 分钟", 7_200_000: "2 小时", 86_400_000: "1 天", 31_536_000_000: "1 年", 1_500: "1.5 秒",
		599_998: "10 分钟", 129_600_000: "1.5 天", 500: "500 毫秒"} {
		if got := humanDuration(ms); got != want {
			t.Fatalf("humanDuration(%d) = %q, want %q", ms, got, want)
		}
	}
	draft := Draft{Manifest: testDraftRequest(t).Manifest}
	if kindLabel(draft) != "任务" {
		t.Fatalf("a 30 minute task is %q", kindLabel(draft))
	}
	draft.Manifest.Budget.TaskTTLMS = (365 * 24 * time.Hour).Milliseconds()
	if kindLabel(draft) != "项目（长期授权）" {
		t.Fatalf("a one-year task is %q", kindLabel(draft))
	}
	draft.Manifest.ParentTaskID = "root"
	if kindLabel(draft) != "项目内查询" {
		t.Fatalf("a child task is %q", kindLabel(draft))
	}
}

// A delegated task's decision may not outlive its parent: the OA shortens
// the TTL to what the parent has left and decides "narrowed".
func TestADelegatedDecisionIsShortenedToTheParentsRemainingTime(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, false)
	request := staffDraft(t, "staff", "", 30*time.Minute)
	parentExpiry := server.config.Clock().UTC().Add(10 * time.Minute)
	request.ParentExpiresAt = &parentExpiry
	draft := createDraft(t, httpServer.URL, request)
	staff := login(t, httpServer.URL, "staff", "shared")
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, "有效期 10 分钟") {
		t.Fatalf("the page does not show the parent's bound: %s", page)
	}
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if status := act(t, staff, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "narrowed" })
	event := gateway.event(t, draft, "staff", "narrowed")
	if event.ApprovedGrant == nil || event.ApprovedGrant.Budget.TaskTTLMS > (10*time.Minute).Milliseconds() ||
		event.ApprovedGrant.ExpiresAt.After(parentExpiry) || settled.ApprovalReceipt.Decision != approval.ApprovalDecisionNarrow {
		t.Fatalf("shortened grant %+v receipt %+v", event.ApprovedGrant, settled.ApprovalReceipt)
	}
	// A parent that already expired refuses the decision.
	expired := staffDraft(t, "staff", "", 30*time.Minute)
	past := server.config.Clock().UTC().Add(-time.Minute)
	expired.ParentExpiresAt = &past
	gone := createDraft(t, httpServer.URL, expired)
	if status := act(t, staff, httpServer.URL, gone, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if status := act(t, staff, httpServer.URL, gone, "decision", url.Values{"decision": {"approved"}}); status != http.StatusConflict {
		t.Fatalf("deciding under an expired parent = %d, want 409", status)
	}
}

// In the shared-password mode the login page asks for the password only;
// the session starts as the default (first requester) account, and the
// identity is switched afterwards. A login that names an account still works
// (the scripts do that).
func TestAConfiguredDefaultAccountWinsOverSwitcherOrder(t *testing.T) {
	gateway := &fakeGateway{answers: map[string]fakeAnswer{}}
	callback := httptest.NewServer(gateway)
	t.Cleanup(callback.Close)
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session", CallbackURL: callback.URL,
		OmitBuiltinUsers: true, AllowSwitch: true, DefaultAccount: "staff",
		ExtraUsers: []ExtraUser{{Name: "staff", Role: "both", Password: "shared"}, {Name: "engineering-a-head", Role: "both", Password: "shared"},
			{Name: "finance-admin", Role: "approver", Password: "shared"}}})
	if err != nil {
		t.Fatal(err)
	}
	if server.defaultAccount() != "staff" {
		t.Fatalf("default account %q, want the configured staff over engineering-a-head", server.defaultAccount())
	}
	server.config.DefaultAccount = "nobody"
	if server.defaultAccount() != "engineering-a-head" {
		t.Fatalf("an unknown configured default did not fall back to switcher order: %q", server.defaultAccount())
	}
}

func TestPasswordOnlyLoginLandsOnTheDefaultAccount(t *testing.T) {
	_, httpServer, _ := delegatedOA(t, true)
	page := get(t, http.DefaultClient, httpServer.URL+"/login")
	if strings.Contains(page, `name="username"`) || !strings.Contains(page, "默认是普通员工 staff") {
		t.Fatalf("the shared-password login page: %s", page)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	match := csrfField.FindStringSubmatch(get(t, client, httpServer.URL+"/login"))
	response, err := client.PostForm(httpServer.URL+"/login", url.Values{"csrf": {match[1]}, "password": {"shared"}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if tasks := get(t, client, httpServer.URL+"/tasks"); !strings.Contains(tasks, "staff · 申请人兼审批人") || !strings.Contains(tasks, "切换身份") {
		t.Fatalf("after a password-only login: %s", tasks)
	}
	if response.Header.Get("Cache-Control") != "" && response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control %q", response.Header.Get("Cache-Control"))
	}
	freshJar, _ := cookiejar.New(nil)
	fresh := &http.Client{Jar: freshJar}
	match = csrfField.FindStringSubmatch(get(t, fresh, httpServer.URL+"/login"))
	wrong, err := fresh.PostForm(httpServer.URL+"/login", url.Values{"csrf": {match[1]}, "password": {"wrong"}})
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()
	if !strings.Contains(wrong.Request.URL.String(), "error=invalid") {
		t.Fatalf("a wrong password landed on %s", wrong.Request.URL)
	}
	// Naming the account still works, as the scripts do.
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	if tasks := get(t, admin, httpServer.URL+"/tasks"); !strings.Contains(tasks, "finance-admin · 审批人") {
		t.Fatalf("a named login: %s", tasks)
	}
	// Outside the shared-password mode the page still asks for the account.
	_, fixed, _ := delegatedOA(t, false)
	if page := get(t, http.DefaultClient, fixed.URL+"/login"); !strings.Contains(page, `name="username"`) {
		t.Fatal("the account chooser is gone outside the shared-password mode")
	}
}

// In the shared-password mode the to-do page also lists what waits for the
// other accounts, each draft with a "切换并打开" button; the switch lands on
// the draft's page when next names one, and on the to-do list otherwise.
func TestTheToDoPageListsOtherAccountsTodosAndSwitchesIntoThem(t *testing.T) {
	_, httpServer, _ := delegatedOA(t, true)
	draft := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	staff := login(t, httpServer.URL, "staff", "shared")
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	list := get(t, staff, httpServer.URL+"/tasks")
	at := strings.Index(list, "其他身份的待办")
	if at < 0 {
		t.Fatalf("no overview of the other accounts' to-dos: %s", list)
	}
	others := list[at:]
	if !strings.Contains(others, "<h3>财务部管理员（1 张）</h3>") || !strings.Contains(others, ">切换并打开<") ||
		!strings.Contains(others, `name="username" value="finance-admin"`) || !strings.Contains(others, `name="next" value="/tasks/`+draft+`"`) {
		t.Fatalf("finance-admin's waiting draft is not offered: %s", others)
	}
	if strings.Contains(others, `name="username" value="staff"`) || strings.Contains(others, "销售部管理员（") {
		t.Fatalf("the overview lists the session itself or an account with nothing waiting: %s", others)
	}

	// The switch with next lands on the draft's page as finance-admin.
	match := csrfField.FindStringSubmatch(list)
	response, err := staff.PostForm(httpServer.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"finance-admin"}, "next": {"/tasks/" + draft}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.Request.URL.Path != "/tasks/"+draft || !strings.Contains(string(body), "finance-admin · 审批人") ||
		!strings.Contains(string(body), `name="decision" value="approved"`) {
		t.Fatalf("the switch landed on %s: %s", response.Request.URL, body)
	}

	// Any other next (another site, another page, a path trick) lands on /tasks.
	noFollow := &http.Client{Jar: staff.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, next := range []string{"https://evil.example/tasks", "//evil.example/tasks", "/history", "/login", "/tasks/../login",
		"/tasksx", "/tasks/" + draft + "?x=1", `/tasks\evil`, "javascript:alert(1)", ""} {
		match := csrfField.FindStringSubmatch(get(t, staff, httpServer.URL+"/tasks"))
		response, err := noFollow.PostForm(httpServer.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"staff"}, "next": {next}})
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/tasks" {
			t.Fatalf("switch with next %q: status %d location %q, want 303 /tasks", next, response.StatusCode, response.Header.Get("Location"))
		}
	}
	if page := get(t, staff, httpServer.URL+"/tasks"); !strings.Contains(page, "staff · 申请人兼审批人") {
		t.Fatal("the switches did not land on staff")
	}

	// Outside the shared-password mode the overview is not offered.
	_, fixed, _ := delegatedOA(t, false)
	pending := createDraft(t, fixed.URL, staffDraft(t, "finance-admin", "", time.Hour))
	staff = login(t, fixed.URL, "staff", "shared")
	if status := act(t, staff, fixed.URL, pending, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if page := get(t, staff, fixed.URL+"/tasks"); strings.Contains(page, "其他身份的待办") || strings.Contains(page, "切换并打开") {
		t.Fatal("the overview is offered outside the shared-password mode")
	}
}

// The draft page offers a switch to whoever acts on the draft now: the
// approver while it is pending, the requester while it is unsubmitted.
func TestTheDraftPageOffersTheSwitchToWhoeverActsNow(t *testing.T) {
	_, httpServer, _ := delegatedOA(t, true)
	draft := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	page := get(t, admin, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, ">切换到 普通员工 并提交<") || !strings.Contains(page, `name="username" value="staff"`) ||
		!strings.Contains(page, `name="next" value="/tasks/`+draft+`"`) {
		t.Fatalf("the approver is not offered the switch to the requester: %s", page)
	}
	if list := get(t, admin, httpServer.URL+"/tasks"); !strings.Contains(list, "<h3>普通员工（1 张）</h3>") {
		t.Fatalf("the approver's overview does not list the unsubmitted draft under staff: %s", list)
	}
	staff := login(t, httpServer.URL, "staff", "shared")
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); strings.Contains(page, "切换到 ") {
		t.Fatal("the requester is offered a switch for the draft they submit themselves")
	}
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	page = get(t, staff, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, ">切换到 财务部管理员 并审批<") || !strings.Contains(page, `name="username" value="finance-admin"`) ||
		!strings.Contains(page, `name="next" value="/tasks/`+draft+`"`) || strings.Contains(page, `name="decision"`) {
		t.Fatalf("the requester is not offered the switch to the approver: %s", page)
	}
	if page := get(t, admin, httpServer.URL+"/tasks/"+draft); strings.Contains(page, "切换到 ") || !strings.Contains(page, `name="decision" value="approved"`) {
		t.Fatal("the approver's own page offers a switch, or lost its decision forms")
	}

	_, fixed, _ := delegatedOA(t, false)
	pending := createDraft(t, fixed.URL, staffDraft(t, "finance-admin", "", time.Hour))
	staff = login(t, fixed.URL, "staff", "shared")
	if status := act(t, staff, fixed.URL, pending, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if page := get(t, staff, fixed.URL+"/tasks/"+pending); strings.Contains(page, "切换到 ") {
		t.Fatal("a switch is offered outside the shared-password mode")
	}
}

// The draft page updates itself while it waits for someone else's
// decision, or for TaskGate's answer to a decision just made: it polls the
// draft's version (poll.js) and reloads only on a change, never on a timer;
// the approver's own page (a narrowing form being filled in) and settled
// pages do not poll.
func TestTheDraftPageRefreshesWhileItWaits(t *testing.T) {
	const (
		pollTag     = `<script src="/poll.js"`
		versionMeta = `<meta name="draft-version"`
		waiting     = "等待处理结果，有新进展时本页自动更新；你在页面上操作时不会被打断。"
	)
	polls := func(page string) bool { return strings.Contains(page, pollTag) && strings.Contains(page, versionMeta) }
	pollsAtAll := func(page string) bool { return strings.Contains(page, pollTag) || strings.Contains(page, versionMeta) }
	server, httpServer, gateway := delegatedOA(t, true)
	draft := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	staff := login(t, httpServer.URL, "staff", "shared")
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); pollsAtAll(page) {
		t.Fatal("an unsubmitted draft's page polls")
	}
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !polls(page) || !strings.Contains(page, waiting) || strings.Contains(page, "http-equiv") {
		t.Fatalf("a pending draft's page does not poll for the requester, or reloads on a timer: %s", page)
	}
	if page := get(t, admin, httpServer.URL+"/tasks/"+draft); pollsAtAll(page) {
		t.Fatal("the approver's own page polls and would reset the narrowing form")
	}
	if status := act(t, admin, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	gateway.event(t, draft, "finance-admin", "approved")
	if settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" }); settled.Note != "" {
		t.Fatalf("a plain approval left a note: %q", settled.Note)
	}
	if page := get(t, admin, httpServer.URL+"/tasks/"+draft); !polls(page) || !strings.Contains(page, waiting) {
		t.Fatal("the page does not poll while TaskGate's answer may still come")
	}
	// The window closes 10 seconds after the decision.
	server.mu.Lock()
	decided := server.drafts[draft]
	decided.History[len(decided.History)-1].At = server.config.Clock().UTC().Add(-11 * time.Second)
	server.mu.Unlock()
	if page := get(t, admin, httpServer.URL+"/tasks/"+draft); pollsAtAll(page) || strings.Contains(page, waiting) {
		t.Fatal("an approved draft's page still polls after the answer window")
	}

	// An approval whose answer arrived (here: the callback failed) carries a
	// note and does not poll.
	failing := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	gateway.answer(failing, "finance-admin", "approved", http.StatusConflict, `{"error":"callback actor is not allowed"}`)
	if status := act(t, staff, httpServer.URL, failing, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	if status := act(t, admin, httpServer.URL, failing, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	awaitDraft(t, server, failing, func(d Draft) bool { return d.State == "approved" && d.Note != "" })
	if page := get(t, admin, httpServer.URL+"/tasks/"+failing); pollsAtAll(page) || !strings.Contains(page, "callback actor is not allowed") {
		t.Fatalf("an approved draft with a note: %s", page)
	}
}

// The waiting page's poll: /tasks/{id}/state answers the session's page
// version under the page's own access rules; the version changes with the
// decision, and once more when the answer window closes, so a page that
// waited for TaskGate's answer drops its waiting notice. poll.js is served
// without a session, like switch.js.
func TestTheDraftStateEndpointReportsChanges(t *testing.T) {
	const waiting = "等待处理结果，有新进展时本页自动更新"
	server, httpServer, gateway := delegatedOA(t, true)
	// The test clock, set before the first request: moving it forward
	// closes the answer window.
	var skew atomic.Int64
	server.config.Clock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	draft := createDraft(t, httpServer.URL, staffDraft(t, "finance-admin", "", time.Hour))
	staff := login(t, httpServer.URL, "staff", "shared")
	admin := login(t, httpServer.URL, "finance-admin", "shared")
	version := func(client *http.Client) string {
		t.Helper()
		response, err := client.Get(httpServer.URL + "/tasks/" + draft + "/state")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var state map[string]string
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			json.NewDecoder(response.Body).Decode(&state) != nil || state["version"] == "" {
			t.Fatalf("state: status %d, content type %q, body %v", response.StatusCode, response.Header.Get("Content-Type"), state)
		}
		return state["version"]
	}
	unsubmitted := version(staff)
	if status := act(t, staff, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("submit: %d", status)
	}
	pending := version(staff)
	stored := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "pending" })
	// The requester's page waits; the approver's own page does not.
	if pending == unsubmitted || version(staff) != pending || pending != pageVersion(stored, true) || version(admin) != pageVersion(stored, false) {
		t.Fatalf("versions: unsubmitted %q, pending %q, the approver's %q", unsubmitted, pending, version(admin))
	}
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, `<meta name="draft-version" content="`+pending+`">`) {
		t.Fatalf("the waiting page does not carry the version it polls against (%q): %s", pending, page)
	}

	if status := act(t, admin, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("approve: %d", status)
	}
	gateway.event(t, draft, "finance-admin", "approved")
	settled := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	if settled.Note != "" {
		t.Fatalf("a plain final approval left a note: %q", settled.Note)
	}
	decided := version(staff)
	if decided == pending || decided != pageVersion(settled, true) {
		t.Fatalf("the version did not change with the decision, or the page does not wait for the answer: %q", decided)
	}
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, `<meta name="draft-version" content="`+decided+`">`) ||
		!strings.Contains(page, `<script src="/poll.js"`) || !strings.Contains(page, waiting) {
		t.Fatalf("inside the answer window the page does not wait with version %q: %s", decided, page)
	}
	// No answer comes (TaskGate took the final approval): when the window
	// closes the version changes once more, and the reloaded page no longer
	// waits or polls.
	skew.Store(int64(answerWait + time.Second))
	if closed := version(staff); closed == decided || closed != pageVersion(settled, false) {
		t.Fatalf("the version did not change when the answer window closed: %q", closed)
	}
	if page := get(t, staff, httpServer.URL+"/tasks/"+draft); strings.Contains(page, waiting) || strings.Contains(page, "/poll.js") ||
		strings.Contains(page, `<meta name="draft-version"`) {
		t.Fatalf("after the answer window the page still waits: %s", page)
	}

	stranger := login(t, httpServer.URL, "sales-admin", "shared")
	if status, body := getStatus(t, stranger, httpServer.URL+"/tasks/"+draft+"/state"); status != http.StatusForbidden || strings.Contains(body, "version") {
		t.Fatalf("someone else's draft state = %d %s, want 403", status, body)
	}
	if status, _ := getStatus(t, staff, httpServer.URL+"/tasks/draft_unknown/state"); status != http.StatusNotFound {
		t.Fatalf("an unknown draft's state = %d, want 404", status)
	}
	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if status, body := getStatus(t, anonymous, httpServer.URL+"/tasks/"+draft+"/state"); status != http.StatusSeeOther || strings.Contains(body, "version") {
		t.Fatalf("the state without a session = %d %s, want a redirect to the login", status, body)
	}

	response, err := http.Get(httpServer.URL + "/poll.js")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	script, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(string(script), `"/state"`) || !strings.Contains(string(script), `meta[name="draft-version"]`) ||
		!strings.Contains(string(script), `credentials: "same-origin"`) {
		t.Fatalf("poll.js: status %d, content type %q: %s", response.StatusCode, response.Header.Get("Content-Type"), script)
	}
}

// A member-confirmed child (KB1-LOOP-148): the project's owner stays the
// human subject, the member is the approver, submits and confirms; the
// overview and the switch point to the member; a stranger may do neither.
func TestTheMemberSubmitsAndConfirmsAMemberConfirmedChild(t *testing.T) {
	server, httpServer, gateway := delegatedOA(t, true)
	request := staffDraft(t, "staff", "", time.Hour)
	request.Manifest.HumanSubject = "finance-admin"
	digest, err := approval.ManifestDigest(request.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	request.ManifestDigest, request.MemberConfirm = digest, true
	draft := createDraft(t, httpServer.URL, request)
	if stored := awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "draft" }); !stored.MemberConfirm || stored.Approver != "staff" {
		t.Fatalf("the draft did not keep member_confirm: %+v", stored.MemberConfirm)
	}

	// The owner's page and a stranger's overview point to the member.
	owner := login(t, httpServer.URL, "finance-admin", "shared")
	if page := get(t, owner, httpServer.URL+"/tasks/"+draft); !strings.Contains(page, ">切换到 普通员工 并提交<") ||
		!strings.Contains(page, "由项目成员 普通员工（staff） 本人确认") {
		t.Fatalf("the owner is not sent to the member: %s", page)
	}
	stranger := login(t, httpServer.URL, "sales-admin", "shared")
	list := get(t, stranger, httpServer.URL+"/tasks")
	at := strings.Index(list, "其他身份的待办")
	if at < 0 || !strings.Contains(list[at:], "<h3>普通员工（1 张）</h3>") || strings.Contains(list[at:], "<h3>财务部管理员（") {
		t.Fatalf("the overview does not point to the member: %s", list)
	}
	if status := act(t, stranger, httpServer.URL, draft, "submit", nil); status != http.StatusForbidden {
		t.Fatalf("a stranger submitting = %d, want 403", status)
	}

	member := login(t, httpServer.URL, "staff", "shared")
	page := get(t, member, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, "提交申请") || strings.Contains(page, "切换到 ") {
		t.Fatalf("the member's page: %s", page)
	}
	if status := act(t, member, httpServer.URL, draft, "submit", nil); status != http.StatusOK {
		t.Fatalf("the member submitting = %d", status)
	}
	if event := gateway.event(t, draft, "staff", "submitted"); event.Status != "submitted" {
		t.Fatalf("submit event %+v", event)
	}
	page = get(t, member, httpServer.URL+"/tasks/"+draft)
	if !strings.Contains(page, ">确认<") || !strings.Contains(page, "成员确认") {
		t.Fatalf("a member-confirmed draft does not read as one: %s", page)
	}
	if status := act(t, stranger, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusForbidden {
		t.Fatalf("a stranger deciding = %d, want 403", status)
	}
	if status := act(t, member, httpServer.URL, draft, "decision", url.Values{"decision": {"approved"}}); status != http.StatusOK {
		t.Fatalf("the member confirming = %d", status)
	}
	awaitDraft(t, server, draft, func(d Draft) bool { return d.State == "approved" })
	if event := gateway.event(t, draft, "staff", "approved"); event.ApprovalReceipt == nil || event.ApprovalReceipt.ApproverID != "staff" {
		t.Fatalf("confirmation receipt %+v", event.ApprovalReceipt)
	}
	if history := get(t, member, httpServer.URL+"/history"); !strings.Contains(history, draft) || !strings.Contains(history, "确认成员 staff") {
		t.Fatalf("the member's history: %s", history)
	}
}
