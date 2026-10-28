package oademo

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// The navigation's agent card (KB1-LOOP-151): beside the OA account's card,
// the OA shows one agent label, the configured one or else TaskGate's first,
// with TaskGate's candidate principals grouped by department, and asks
// TaskGate to switch that label, signed like its callbacks. The fake below
// implements the endpoint from the contract, independently of the OA's own
// types.

type fakeAgentGateway struct {
	mu         sync.Mutex
	identities []map[string]string
	candidates []map[string]string
	requests   []map[string]string
	rejected   int // requests whose headers, signature or body broke the contract
	refusal    *fakeAnswer
}

func newFakeAgentGateway() *fakeAgentGateway {
	return &fakeAgentGateway{
		identities: []map[string]string{
			{"label": "lzj", "principal_id": "principal_finance_admin", "subject": "finance-admin", "bound_by": "procurement-admin", "bound_at": "2026-10-28T02:12:00Z"},
			{"label": "wmm", "principal_id": "principal_staff", "subject": "staff", "bound_by": "", "bound_at": ""},
		},
		candidates: []map[string]string{
			{"principal_id": "principal_finance_director", "subject": "finance-director", "department": "finance", "job_family": "director", "center": ""},
			{"principal_id": "principal_procurement_admin", "subject": "procurement-admin", "department": "procurement", "job_family": "dept_admin", "center": ""},
			{"principal_id": "principal_finance_admin", "subject": "finance-admin", "department": "finance", "job_family": "dept_admin", "center": ""},
			{"principal_id": "principal_vp_admin", "subject": "vp-admin", "department": "finance", "job_family": "vice_president", "center": ""},
			{"principal_id": "principal_hr_staff", "subject": "hr-staff", "department": "hr", "job_family": "staff", "center": "hr-a"},
			{"principal_id": "principal_finance_a_head", "subject": "finance-a-head", "department": "finance", "job_family": "center_head", "center": "finance-a"},
			{"principal_id": "principal_staff", "subject": "staff", "department": "finance", "job_family": "staff", "center": "finance-a"},
			{"principal_id": "principal_gm", "subject": "gm", "department": "", "job_family": "general_manager", "center": ""},
		},
	}
}

func (g *fakeAgentGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	eventID := r.Header.Get("X-OA-Event-ID")
	var request map[string]string
	decoder := json.NewDecoder(bytes.NewReader(body))
	if eventID == "" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
		approval.Verify([]byte("callback"), r.Header.Get("X-OA-Timestamp"), r.Header.Get("X-OA-Signature"), body, time.Now(), 5*time.Minute) != nil ||
		decoder.Decode(&request) != nil || request["event_id"] != eventID || !contractKeys(request) {
		g.mu.Lock()
		g.rejected++
		g.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "contract violated"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, request)
	switch request["action"] {
	case "list":
		writeJSON(w, http.StatusOK, map[string]any{"identities": g.identities, "candidates": g.candidates})
	case "switch":
		if g.refusal != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(g.refusal.status)
			_, _ = w.Write([]byte(g.refusal.body))
			return
		}
		for _, identity := range g.identities {
			if identity["label"] != request["label"] {
				continue
			}
			identity["principal_id"], identity["bound_by"] = request["principal_id"], request["actor"]
			identity["bound_at"] = time.Date(2026, 10, 28, 3, 40, 0, 0, time.UTC).Format(time.RFC3339)
			for _, candidate := range g.candidates {
				if candidate["principal_id"] == request["principal_id"] {
					identity["subject"] = candidate["subject"]
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"identity": identity})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown label"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action"})
	}
}

// contractKeys checks a request carries exactly the contract's fields.
func contractKeys(request map[string]string) bool {
	keys := make([]string, 0, len(request))
	for key := range request {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	switch request["action"] {
	case "list":
		return strings.Join(keys, ",") == "action,event_id"
	case "switch":
		return strings.Join(keys, ",") == "action,actor,event_id,label,principal_id" && request["label"] != "" && request["principal_id"] != "" && request["actor"] != ""
	}
	return false
}

func (g *fakeAgentGateway) seen(action string) []map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var matched []map[string]string
	for _, request := range g.requests {
		if request["action"] == action {
			matched = append(matched, request)
		}
	}
	return matched
}

func agentIdentityOA(t *testing.T, allowSwitch bool, identityURL, label string) *httptest.Server {
	t.Helper()
	gateway := &fakeGateway{answers: map[string]fakeAnswer{}}
	callback := httptest.NewServer(gateway)
	t.Cleanup(callback.Close)
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, OmitBuiltinUsers: true, AllowSwitch: allowSwitch, AgentIdentityURL: identityURL, AgentIdentityLabel: label,
		Labels: Labels{Values: map[string]map[string]string{
			"kb_department": {"finance": "财务部", "procurement": "采购部"},
			"center":        {"finance-a": "财务部第一中心"}}},
		ExtraUsers: []ExtraUser{
			{Name: "staff", Role: "both", Password: "shared", DisplayName: "普通员工"},
			{Name: "finance-admin", Role: "approver", Password: "shared", DisplayName: "财务部管理员"},
			{Name: "finance-a-head", Role: "both", Password: "shared", DisplayName: "财务部第一中心主任"},
			{Name: "procurement-admin", Role: "approver", Password: "shared", DisplayName: "采购部管理员"},
			{Name: "vp-admin", Role: "approver", Password: "shared", DisplayName: "管理线副总经理"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

// postAgentIdentity posts the agent form without following the redirect.
func postAgentIdentity(t *testing.T, client *http.Client, base string, values url.Values) (*http.Response, string) {
	t.Helper()
	match := csrfField.FindStringSubmatch(get(t, client, base+"/tasks"))
	if match == nil {
		t.Fatal("no CSRF token")
	}
	form := url.Values{"csrf": {match[1]}}
	for key, value := range values {
		form[key] = value
	}
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := noFollow.PostForm(base+"/agent-identity", form)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response, string(body)
}

// The navigation's two identity cards: the OA account's, and the agent's
// when the control is on.
const (
	oaCardStart    = `<div class="who" id="who-oa">`
	agentCardStart = `<div class="who" id="who-agent">`
)

// agentCard is the navigation's agent card of a page, up to the end of the
// navigation ("" without one).
func agentCard(page string) string {
	start, end := strings.Index(page, agentCardStart), strings.Index(page, "</nav>")
	if start < 0 || end < start {
		return ""
	}
	return page[start:end]
}

// oaCard is the navigation's OA account card of a page ("" without one).
func oaCard(page string) string {
	start := strings.Index(page, oaCardStart)
	if start < 0 {
		return ""
	}
	card := page[start:]
	if end := strings.Index(card, agentCardStart); end >= 0 {
		return card[:end]
	}
	if end := strings.Index(card, "</div></div>"); end >= 0 {
		return card[:end+len("</div>")]
	}
	return ""
}

func TestAgentIdentityIsOffWithoutTheURLOrTheSwitcher(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	normalize := regexp.MustCompile(`[0-9a-f]{64}`)
	pages := map[bool][]string{}
	for _, setup := range []struct {
		allowSwitch bool
		url, label  string
	}{{true, "", ""}, {true, "", "wmm"}, {false, fake.URL, ""}, {false, fake.URL, "wmm"}, {false, "", ""}} {
		httpServer := agentIdentityOA(t, setup.allowSwitch, setup.url, setup.label)
		staff := login(t, httpServer.URL, "staff", "shared")
		page := get(t, staff, httpServer.URL+"/tasks")
		if strings.Contains(page, "/agent-identity") || strings.Contains(page, "who-agent") || strings.Contains(page, "我的 agent") {
			t.Fatalf("%+v: the agent card is offered: %s", setup, page)
		}
		// The OA account card stands alone, with its switcher in the
		// shared-password mode only.
		if oa := oaCard(page); strings.Count(page, `class="who"`) != 1 || !strings.Contains(oa, `<div class="who-name">普通员工</div><div class="who-note">staff · 申请人兼审批人</div>`) ||
			strings.Contains(oa, `action="/switch"`) != setup.allowSwitch || strings.Contains(page, `src="/switch.js"`) != setup.allowSwitch {
			t.Fatalf("%+v: the OA account card: %s", setup, page)
		}
		if response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {"wmm"}, "principal_id": {"principal_staff"}}); response.StatusCode != http.StatusNotFound {
			t.Fatalf("%+v: POST /agent-identity = %d, want 404", setup, response.StatusCode)
		}
		pages[setup.allowSwitch] = append(pages[setup.allowSwitch], normalize.ReplaceAllString(page, "X"))
	}
	// A label without the URL, or a URL or label without the switcher,
	// changes nothing on the page, and TaskGate is never asked.
	for allowSwitch, same := range pages {
		for _, page := range same[1:] {
			if page != same[0] {
				t.Fatalf("switch %v: a URL or label changed the page:\n%s\n%s", allowSwitch, same[0], page)
			}
		}
	}
	if len(pages[true]) != 2 || len(pages[false]) != 3 {
		t.Fatalf("compared %d and %d pages", len(pages[true]), len(pages[false]))
	}
	if len(gateway.seen("list")) != 0 || len(gateway.seen("switch")) != 0 || gateway.rejected != 0 {
		t.Fatalf("the agent endpoint was called while off: %v", gateway.requests)
	}
}

func TestAgentIdentityNavigationHasTwoCards(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "lzj")
	staff := login(t, httpServer.URL, "staff", "shared")
	page := get(t, staff, httpServer.URL+"/tasks")
	nav := page[strings.Index(page, "<nav>"):strings.Index(page, "</nav>")]
	// The top bar holds the product, the two pages and the logout only; the
	// two cards follow, the OA account's first.
	if !strings.HasPrefix(nav, `<nav><div class="bar"><strong>Task-bound OA</strong><div><a href="/tasks">待办</a><a href="/history">审批记录</a>`+
		`<form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="`) ||
		!strings.Contains(nav, `<button type="submit">退出</button></form></div></div><div class="whos">`+oaCardStart) ||
		strings.Count(nav, `class="who"`) != 2 || strings.Index(nav, oaCardStart) > strings.Index(nav, agentCardStart) {
		t.Fatalf("navigation: %s", nav)
	}
	oa, agent := oaCard(page), agentCard(page)
	if !strings.HasPrefix(oa, oaCardStart+`<div class="who-title">我在 OA 里是谁（审批用）</div><div class="who-name">普通员工</div>`+
		`<div class="who-note">staff · 申请人兼审批人</div><form class="who-pick" method="post" action="/switch"><input type="hidden" name="csrf" value="`) ||
		!strings.Contains(oa, `"><label>换一个身份：<select name="username">`) || !strings.Contains(oa, `<option value="staff" selected>`) ||
		!strings.HasSuffix(oa, `</select></label><button type="submit">切换身份</button></form></div>`) {
		t.Fatalf("OA account card: %s", oa)
	}
	if !strings.HasPrefix(agent, agentCardStart+`<div class="who-title">我的 agent 是谁（申请与查询用）</div>`+
		`<div class="who-name">财务部管理员（finance-admin） · 部门管理员</div>`+
		`<div class="who-note">共用令牌 lzj · 2026-10-28 02:12 UTC 由 采购部管理员（procurement-admin） 切换</div>`+
		`<div class="who-note">切换立即生效；切完对 agent 说一句「你被切换了，调 whoami 确认」。</div>`+
		`<form class="who-pick" method="post" action="/agent-identity"><input type="hidden" name="csrf" value="`) ||
		!strings.Contains(agent, `"><input type="hidden" name="next" value="/tasks"><input type="hidden" name="label" value="lzj"><label>换一个身份：<select name="principal_id">`) ||
		!strings.HasSuffix(agent, `</select></label><button type="submit">切换身份</button></form></div></div><script src="/switch.js"></script>`) {
		t.Fatalf("agent card: %s", agent)
	}
	// One label, in the second line and a hidden field: no label to choose,
	// and no "current" word, the large line being the current principal.
	if strings.Contains(nav, `<select name="label"`) || strings.Contains(nav, `name="action"`) || strings.Contains(agent, "wmm") || strings.Contains(nav, "当前") {
		t.Fatalf("label: %s", agent)
	}
	// The cards' style is in the page's head.
	head := page[:strings.Index(page, "</head>")]
	for _, rule := range []string{`.whos{display:flex;flex-wrap:wrap;gap:16px;`, `.who{flex:1 1 320px;background:white;border:1px solid #dfe5eb;border-radius:8px;padding:12px 16px}`,
		`.who-title{font-size:12px;color:#617080}`, `.who-name{font-size:17px;font-weight:700;`, `.who-pick select{box-sizing:border-box;width:100%;`} {
		if !strings.Contains(head, rule) {
			t.Fatalf("no %s in the head", rule)
		}
	}
	if script := get(t, http.DefaultClient, httpServer.URL+"/switch.js"); !strings.Contains(script, `nav form[action="/agent-identity"]`) ||
		!strings.Contains(script, `form.querySelector('select[name="principal_id"]').addEventListener("change", function () { form.submit(); });`) ||
		strings.Contains(script, `select[name="label"]`) || strings.Contains(script, "action.value") || !strings.Contains(script, `nav form[action="/switch"]`) {
		t.Fatalf("switch script: %s", script)
	}
	// The other pages carry the cards too.
	if history := get(t, staff, httpServer.URL+"/history"); !strings.Contains(agentCard(history), `<input type="hidden" name="next" value="/history">`) || oaCard(history) == "" {
		t.Fatalf("history page: %s", history)
	}
	if len(gateway.seen("switch")) != 0 || gateway.rejected != 0 {
		t.Fatalf("rendering switched or broke the contract: %v rejected %d", gateway.requests, gateway.rejected)
	}
}

func TestAgentIdentityNavigationGroupsCandidatesByDepartment(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "lzj")
	staff := login(t, httpServer.URL, "staff", "shared")
	card := agentCard(get(t, staff, httpServer.URL+"/tasks"))
	// The company's leaders first, then the departments by code, each
	// labelled from the labels file or by its code.
	principals := card[strings.Index(card, `name="principal_id"`):]
	order := []string{`<optgroup label="公司领导">`, `<optgroup label="财务部">`, `<optgroup label="hr">`, `<optgroup label="采购部">`}
	at := -1
	for _, group := range order {
		next := strings.Index(principals, group)
		if next <= at {
			t.Fatalf("group %s out of order in %s", group, principals)
		}
		at = next
	}
	if !strings.HasPrefix(principals[strings.Index(principals, "<optgroup"):], `<optgroup label="公司领导"><option value="principal_gm">gm · 总经理</option><option value="principal_vp_admin">管理线副总经理（vp-admin） · 副总经理</option></optgroup>`) {
		t.Fatalf("leaders group: %s", principals)
	}
	finance := principals[strings.Index(principals, `<optgroup label="财务部">`):]
	finance = finance[:strings.Index(finance, "</optgroup>")]
	want := `<optgroup label="财务部"><option value="principal_staff">普通员工（staff） · 员工</option>` +
		`<option value="principal_finance_a_head">财务部第一中心主任（finance-a-head） · 中心主任（财务部第一中心）</option>` +
		`<option value="principal_finance_admin" selected>财务部管理员（finance-admin） · 部门管理员</option>` +
		`<option value="principal_finance_director">finance-director · 部门主任</option>`
	if finance != want {
		t.Fatalf("finance group\n got %s\nwant %s", finance, want)
	}
	if !strings.Contains(card, `<optgroup label="hr"><option value="principal_hr_staff">hr-staff · 员工</option></optgroup>`) {
		t.Fatalf("a staff member's center is not shown, and an unlabelled department shows its code: %s", card)
	}
}

func TestAgentIdentityConfiguredLabelIsTheFormsLabel(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "wmm")
	staff := login(t, httpServer.URL, "staff", "shared")
	card := agentCard(get(t, staff, httpServer.URL+"/tasks"))
	// wmm was never switched: no binding sentence after the label.
	if !strings.Contains(card, `<div class="who-name">普通员工（staff） · 员工</div><div class="who-note">共用令牌 wmm</div>`) ||
		!strings.Contains(card, `<input type="hidden" name="label" value="wmm"><label>换一个身份：<select name="principal_id">`) ||
		!strings.Contains(card, `<option value="principal_staff" selected>`) || strings.Contains(card, `<option value="principal_finance_admin" selected>`) ||
		strings.Contains(card, "lzj") {
		t.Fatalf("with OA_AGENT_LABEL=wmm: %s", card)
	}
	if len(gateway.seen("switch")) != 0 || gateway.rejected != 0 {
		t.Fatalf("rendering switched or broke the contract: %v rejected %d", gateway.requests, gateway.rejected)
	}
}

func TestAgentIdentityUnregisteredLabelShowsNoForm(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "tester")
	staff := login(t, httpServer.URL, "staff", "shared")
	page := get(t, staff, httpServer.URL+"/tasks")
	card := agentCard(page)
	if !strings.HasPrefix(card, agentCardStart+`<div class="who-title">我的 agent 是谁（申请与查询用）</div><div class="who-name error">网关未登记标签 tester</div></div></div>`) ||
		strings.Contains(page, `action="/agent-identity"`) || strings.Contains(card, "<select") || strings.Contains(card, "切换立即生效") || !strings.Contains(page, `action="/switch"`) {
		t.Fatalf("with an unregistered OA_AGENT_LABEL: %s", page)
	}
	// Neither the configured label nor a listed one may be switched.
	for _, label := range []string{"tester", "lzj"} {
		if response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {label}, "principal_id": {"principal_staff"}}); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("switching %s = %d, want 400", label, response.StatusCode)
		}
	}
	if len(gateway.seen("switch")) != 0 || gateway.rejected != 0 {
		t.Fatalf("a switch reached TaskGate: %v rejected %d", gateway.requests, gateway.rejected)
	}
}

func TestAgentIdentityWithoutALabelShowsTheFirstInOrder(t *testing.T) {
	gateway := newFakeAgentGateway()
	// TaskGate's order is not the label order.
	gateway.identities[0], gateway.identities[1] = gateway.identities[1], gateway.identities[0]
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "")
	staff := login(t, httpServer.URL, "staff", "shared")
	card := agentCard(get(t, staff, httpServer.URL+"/tasks"))
	if !strings.Contains(card, `<div class="who-note">共用令牌 lzj · `) || !strings.Contains(card, `<input type="hidden" name="label" value="lzj">`) ||
		!strings.Contains(card, `<option value="principal_finance_admin" selected>`) || strings.Contains(card, "wmm") {
		t.Fatalf("without OA_AGENT_LABEL: %s", card)
	}
	// TaskGate lists no agent at all.
	gateway.mu.Lock()
	gateway.identities = nil
	gateway.mu.Unlock()
	if card := agentCard(get(t, staff, httpServer.URL+"/tasks")); !strings.Contains(card, `<div class="who-name muted">网关没有登记任何 agent</div></div></div>`) ||
		strings.Contains(card, "<form") {
		t.Fatalf("with no agent listed: %s", card)
	}
}

func TestAgentIdentitySwitchOfAnotherLabelIsRefused(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	for _, setup := range []struct{ configured, shown, other string }{{"wmm", "wmm", "lzj"}, {"", "lzj", "wmm"}} {
		httpServer := agentIdentityOA(t, true, fake.URL, setup.configured)
		staff := login(t, httpServer.URL, "staff", "shared")
		for _, label := range []string{setup.other, "nobody"} {
			response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {label}, "principal_id": {"principal_staff"}, "next": {"/tasks"}})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("OA_AGENT_LABEL %q: switching %s = %d, want 400", setup.configured, label, response.StatusCode)
			}
		}
		if len(gateway.seen("switch")) != 0 {
			t.Fatalf("OA_AGENT_LABEL %q: another label reached TaskGate: %v", setup.configured, gateway.requests)
		}
		// The shown label switches, and the answer keeps no agent state in
		// the browser.
		response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {setup.shown}, "principal_id": {"principal_staff"}, "next": {"/tasks"}})
		if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 0 {
			t.Fatalf("OA_AGENT_LABEL %q: switching %s = %d, cookies %v", setup.configured, setup.shown, response.StatusCode, response.Cookies())
		}
		switches := gateway.seen("switch")
		if len(switches) != 1 || switches[0]["label"] != setup.shown || gateway.rejected != 0 {
			t.Fatalf("OA_AGENT_LABEL %q: switches %v rejected %d", setup.configured, switches, gateway.rejected)
		}
		gateway.mu.Lock()
		gateway.requests = nil
		gateway.mu.Unlock()
	}
}

func TestAgentIdentitySwitchIsSignedAndNamesTheSessionAccount(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "wmm")
	staff := login(t, httpServer.URL, "staff", "shared")
	response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {"wmm"},
		"principal_id": {"principal_finance_a_head"}, "next": {"/tasks/oa_0123abcd"}})
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/tasks/oa_0123abcd" {
		t.Fatalf("switch = %d to %q", response.StatusCode, response.Header.Get("Location"))
	}
	switches := gateway.seen("switch")
	if len(switches) != 1 || switches[0]["label"] != "wmm" || switches[0]["principal_id"] != "principal_finance_a_head" ||
		switches[0]["actor"] != "staff" || gateway.rejected != 0 {
		t.Fatalf("switch requests %v rejected %d", switches, gateway.rejected)
	}
	// The switched label is shown, bound by the session's account.
	card := agentCard(get(t, staff, httpServer.URL+"/tasks"))
	if !strings.Contains(card, `<input type="hidden" name="label" value="wmm">`) || !strings.Contains(card, `<option value="principal_finance_a_head" selected>`) ||
		!strings.Contains(card, `<div class="who-name">财务部第一中心主任（finance-a-head） · 中心主任（财务部第一中心）</div>`+
			`<div class="who-note">共用令牌 wmm · 2026-10-28 03:40 UTC 由 普通员工（staff） 切换</div>`) {
		t.Fatalf("after the switch: %s", card)
	}
	// After an OA account switch the actor is the new account; a next outside
	// the to-do list and draft pages lands on the to-do list.
	match := csrfField.FindStringSubmatch(get(t, staff, httpServer.URL+"/tasks"))
	switched, err := staff.PostForm(httpServer.URL+"/switch", url.Values{"csrf": {match[1]}, "username": {"procurement-admin"}})
	if err != nil {
		t.Fatal(err)
	}
	switched.Body.Close()
	for _, next := range []string{"https://evil.example/tasks", "//evil.example/tasks", "/history", "/tasks/../login", ""} {
		response, _ := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {"wmm"}, "principal_id": {"principal_vp_admin"}, "next": {next}})
		if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/tasks" {
			t.Fatalf("next %q: %d to %q", next, response.StatusCode, response.Header.Get("Location"))
		}
	}
	switches = gateway.seen("switch")
	if last := switches[len(switches)-1]; last["actor"] != "procurement-admin" || last["label"] != "wmm" || gateway.rejected != 0 {
		t.Fatalf("switch after the account switch: %v rejected %d", last, gateway.rejected)
	}
	// Missing fields are refused before TaskGate is asked.
	before := len(gateway.seen("switch"))
	for _, values := range []url.Values{{"label": {"wmm"}}, {"principal_id": {"principal_staff"}}} {
		if response, _ := postAgentIdentity(t, staff, httpServer.URL, values); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%v = %d, want 400", values, response.StatusCode)
		}
	}
	if len(gateway.seen("switch")) != before {
		t.Fatal("an incomplete switch reached TaskGate")
	}
	// Without the CSRF token the switch is refused.
	noToken, err := staff.PostForm(httpServer.URL+"/agent-identity", url.Values{"label": {"wmm"}, "principal_id": {"principal_staff"}})
	if err != nil {
		t.Fatal(err)
	}
	noToken.Body.Close()
	if noToken.StatusCode != http.StatusForbidden {
		t.Fatalf("switch without CSRF = %d", noToken.StatusCode)
	}
}

func TestAgentIdentityRefusalIsABadGatewayWithTheGatewaysReason(t *testing.T) {
	gateway := newFakeAgentGateway()
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := agentIdentityOA(t, true, fake.URL, "")
	staff := login(t, httpServer.URL, "staff", "shared")
	for _, refusal := range []struct {
		answer fakeAnswer
		want   string
	}{
		{fakeAnswer{http.StatusBadRequest, `{"error":"principal_hr_staff is not a candidate for lzj"}`}, "HTTP 400：principal_hr_staff is not a candidate for lzj"},
		{fakeAnswer{http.StatusNotFound, `404 page not found`}, "网关未开启 agent 身份切换（HTTP 404）"},
		{fakeAnswer{http.StatusBadRequest, `{"error":"` + strings.Repeat("长", 400) + `"}`}, "HTTP 400：长长"},
	} {
		gateway.mu.Lock()
		answer := refusal.answer
		gateway.refusal = &answer
		gateway.mu.Unlock()
		response, body := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {"lzj"}, "principal_id": {"principal_hr_staff"}, "next": {"/tasks"}})
		if response.StatusCode != http.StatusBadGateway || !strings.Contains(body, refusal.want) {
			t.Fatalf("refused switch = %d: %s", response.StatusCode, body)
		}
		if len(strings.TrimSpace(body)) > 512 || !utf8.ValidString(body) || strings.Contains(body, fake.URL) || strings.Contains(body, "v1=") || !strings.HasPrefix(body, "agent 身份切换失败") {
			t.Fatalf("the refusal leaks or overflows (%d bytes): %s", len(body), body)
		}
	}
}

func TestAgentIdentityUnreachableGatewayLeavesThePageUsable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	address := closed.URL
	closed.Close()
	httpServer := agentIdentityOA(t, true, address, "lzj")
	staff := login(t, httpServer.URL, "staff", "shared")
	status, page := getStatus(t, staff, httpServer.URL+"/tasks")
	if status != http.StatusOK || !strings.Contains(agentCard(page), `<div class="who-name error">网关暂不可用（无法连接）</div></div></div>`) || !strings.Contains(page, "暂无待办") ||
		!strings.Contains(page, `action="/switch"`) || strings.Contains(page, address) || strings.Contains(page, `action="/agent-identity"`) {
		t.Fatalf("page with the gateway down (%d): %s", status, page)
	}
	response, body := postAgentIdentity(t, staff, httpServer.URL, url.Values{"label": {"lzj"}, "principal_id": {"principal_staff"}})
	if response.StatusCode != http.StatusBadGateway || strings.Contains(body, address) || !strings.Contains(body, "无法连接") {
		t.Fatalf("switch with the gateway down = %d: %s", response.StatusCode, body)
	}
}

func TestAgentIdentityURLMustBeAURL(t *testing.T) {
	if _, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session", CallbackURL: "http://127.0.0.1:1/cb",
		AlicePassword: "a", BobPassword: "b", AgentIdentityURL: "not a url"}); err == nil {
		t.Fatal("an invalid agent identity URL was accepted")
	}
}
