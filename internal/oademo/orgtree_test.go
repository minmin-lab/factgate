package oademo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The organization tree (OA_ORG_FILE): both identity cards pick from one
// company -> business line -> department -> center -> people tree instead of
// a flat <select>. The sample tree is testdata/org-tree.yaml.

const sampleOrgTree = "testdata/org-tree.yaml"

func loadSampleTree(t *testing.T) *OrgTree {
	t.Helper()
	tree, err := LoadOrgTree(sampleOrgTree)
	if err != nil || tree == nil {
		t.Fatalf("load %s: %v", sampleOrgTree, err)
	}
	return tree
}

// orgParents maps each person of the tree to the label of the group that
// holds them.
func orgParents(tree *OrgTree) map[string]string {
	parents := map[string]string{}
	var walk func(node OrgNode, parent string)
	walk = func(node OrgNode, parent string) {
		if node.Kind == "person" {
			parents[node.Subject] = parent
			return
		}
		for _, child := range node.Children {
			walk(child, node.Label)
		}
	}
	walk(tree.Root, "")
	return parents
}

func TestLoadOrgTreeParsesTheSampleFile(t *testing.T) {
	tree := loadSampleTree(t)
	if tree.Version != OrgTreeVersion || tree.Company != "大米集团" || tree.Root.Label != "大米集团" || tree.Root.Code != "company" {
		t.Fatalf("tree header %q %q %q %q", tree.Version, tree.Company, tree.Root.Label, tree.Root.Code)
	}
	// Two lines, three departments, two centers each, twenty people.
	groupsAt := map[int][]string{}
	var walk func(node OrgNode, depth int)
	walk = func(node OrgNode, depth int) {
		if node.Kind == "group" {
			groupsAt[depth] = append(groupsAt[depth], node.Label)
			for _, child := range node.Children {
				walk(child, depth+1)
			}
		}
	}
	walk(tree.Root, 0)
	if len(groupsAt[1]) != 2 || len(groupsAt[2]) != 3 || len(groupsAt[3]) != 6 || len(groupsAt[4]) != 0 {
		t.Fatalf("groups by depth %v", groupsAt)
	}
	people := tree.people()
	if len(people) != 20 {
		t.Fatalf("%d people", len(people))
	}
	// The big line: 姓名 · 岗位 · 部门 中心; the role label when there is no
	// title; no department or center for the leaders above them.
	for subject, want := range map[string]string{
		"zhouyan":     "周燕 · 会计 · 财务部 会计核算中心",
		"sunhao":      "孙浩 · 中心主任 · 财务部 会计核算中心",
		"zhaomin":     "赵敏 · 财务部主任 · 财务部",
		"wangjianguo": "王建国 · 副总经理",
		"liuguodong":  "刘国栋 · 总经理",
	} {
		if got := people[subject].line(); got != want {
			t.Fatalf("%s line %q, want %q", subject, got, want)
		}
	}
	if search := people["zhouyan"].search(); search != "周燕 zhouyan 会计 员工 大米集团 管理线（副总经理 王建国） 财务部 会计核算中心" {
		t.Fatalf("zhouyan search text %q", search)
	}
	// No path, no tree; a path that does not exist is an error.
	if none, err := LoadOrgTree(""); none != nil || err != nil {
		t.Fatalf("empty path: %v %v", none, err)
	}
	if _, err := LoadOrgTree(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing org tree file was accepted")
	}
}

func TestLoadOrgTreeRefusesABrokenTree(t *testing.T) {
	const head = "version: oa-org-tree-v1\ncompany: 大米集团\nroot:\n  kind: group\n  label: 大米集团\n  children:\n"
	person := func(subject string) string {
		return "  - {kind: person, subject: " + subject + ", name: 某人, title: 员工}\n"
	}
	for name, test := range map[string]struct{ body, want string }{
		"a person twice": {head + person("liuguodong") + "  - kind: group\n    label: 财务部\n    children:\n    " + person("liuguodong"), "appears twice"},
		"upper case":     {head + person("Liuguodong"), "must match"},
		"a hyphen":       {head + person("liu-guodong"), "must match"},
		"a digit first":  {head + person("1liu"), "must match"},
		"no subject":     {head + "  - {kind: person, name: 某人}\n", "must match"},
		"no name":        {head + "  - {kind: person, subject: liu}\n", "no name"},
		"wrong version":  {strings.Replace(head, "oa-org-tree-v1", "oa-org-tree-v2", 1) + person("liu"), "version"},
		"a person root":  {"version: oa-org-tree-v1\nroot: {kind: person, subject: liu, name: 刘}\n", "root must be a group"},
		"unknown kind":   {head + "  - {kind: team, label: 小组}\n", "not group or person"},
		"group no label": {head + "  - {kind: group, children: []}\n", "no label"},
		"person children": {head + "  - kind: person\n    subject: liu\n    name: 刘\n    children:\n    " + person("wang"),
			"has children"},
		"not yaml": {"version: [", "org tree file"},
	} {
		path := filepath.Join(t.TempDir(), "org.yaml")
		if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
			t.Fatal(err)
		}
		tree, err := LoadOrgTree(path)
		if err == nil || tree != nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: tree %v error %v, want an error with %q", name, tree, err, test.want)
		}
	}
	// A tree handed to New directly is checked as well.
	broken := loadSampleTree(t)
	broken.Root.Children = append(broken.Root.Children, OrgNode{Kind: "person", Subject: "zhouyan", Name: "周燕"})
	if _, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session", CallbackURL: "http://127.0.0.1:1/cb",
		AlicePassword: "a", BobPassword: "b", OrgTree: broken}); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Fatalf("New with a duplicate person: %v", err)
	}
}

// orgMissing are the sample tree's persons who have no OA account in
// orgTreeOA; tester is the account the tree does not name.
var orgMissing = map[string]bool{"weilan": true, "youna": true}

// orgTreeOA is an OA with the sample tree: every person of the tree an
// account (named "账户 <subject>", so the tree's names can be told apart)
// except orgMissing, plus tester.
func orgTreeOA(t *testing.T, tree *OrgTree, allowSwitch bool, identityURL, label string) *httptest.Server {
	t.Helper()
	gateway := &fakeGateway{answers: map[string]fakeAnswer{}}
	callback := httptest.NewServer(gateway)
	t.Cleanup(callback.Close)
	users := []ExtraUser{{Name: "tester", Role: "both", Password: "shared", DisplayName: "测试员"}}
	for subject := range tree.people() {
		if !orgMissing[subject] {
			users = append(users, ExtraUser{Name: subject, Role: "both", Password: "shared", DisplayName: "账户 " + subject})
		}
	}
	server, err := New(Config{ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, OmitBuiltinUsers: true, AllowSwitch: allowSwitch, AgentIdentityURL: identityURL, AgentIdentityLabel: label,
		OrgTree: tree, ExtraUsers: users})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

// treeLeaf is one person of a rendered tree.
type treeLeaf struct {
	Group           string // the <summary> of the group holding it
	Field, Value    string // its button's name and value ("" when missing)
	Text, Search    string
	Current         bool
	Missing         bool
	Disabled        bool
	ButtonIsPick    bool
	ButtonIsSubmits bool
}

func htmlAttr(node *html.Node, key string) (string, bool) {
	for _, attribute := range node.Attr {
		if attribute.Key == key {
			return attribute.Val, true
		}
	}
	return "", false
}

func htmlClass(node *html.Node, class string) bool {
	value, _ := htmlAttr(node, "class")
	for _, field := range strings.Fields(value) {
		if field == class {
			return true
		}
	}
	return false
}

func htmlText(node *html.Node) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return text.String()
}

func htmlFind(node *html.Node, match func(*html.Node) bool) *html.Node {
	if node.Type == html.ElementNode && match(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := htmlFind(child, match); found != nil {
			return found
		}
	}
	return nil
}

func htmlElement(name string) func(*html.Node) bool {
	return func(n *html.Node) bool { return n.Data == name }
}

// parseTree reads the navigation's tree form posting to action: its leaves
// and its groups (label -> open by default).
func parseTree(t *testing.T, page, action string) ([]treeLeaf, map[string]bool) {
	t.Helper()
	document, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	nav := htmlFind(document, htmlElement("nav"))
	if nav == nil {
		t.Fatalf("no navigation: %s", page)
	}
	form := htmlFind(nav, func(n *html.Node) bool {
		value, _ := htmlAttr(n, "action")
		return n.Data == "form" && value == action && htmlClass(n, "who-tree")
	})
	if form == nil {
		t.Fatalf("no tree form posting to %s: %s", action, page)
	}
	var leaves []treeLeaf
	groups := map[string]bool{}
	var visit func(n *html.Node, group string)
	visit = func(n *html.Node, group string) {
		if n.Type == html.ElementNode && n.Data == "li" {
			switch {
			case htmlClass(n, "node"):
				details := htmlFind(n, htmlElement("details"))
				summary := htmlFind(details, htmlElement("summary"))
				label := htmlText(summary)
				_, open := htmlAttr(details, "open")
				groups[label] = open
				for child := details.FirstChild; child != nil; child = child.NextSibling {
					visit(child, label)
				}
				return
			case htmlClass(n, "leaf"):
				search, _ := htmlAttr(n, "data-search")
				leaf := treeLeaf{Group: group, Text: htmlText(n), Search: search, Current: htmlClass(n, "current"), Missing: htmlClass(n, "missing")}
				if button := htmlFind(n, htmlElement("button")); button != nil {
					leaf.Field, _ = htmlAttr(button, "name")
					leaf.Value, _ = htmlAttr(button, "value")
					_, leaf.Disabled = htmlAttr(button, "disabled")
					kind, _ := htmlAttr(button, "type")
					leaf.ButtonIsPick, leaf.ButtonIsSubmits = htmlClass(button, "pick"), kind == "submit"
				}
				leaves = append(leaves, leaf)
				return
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child, group)
		}
	}
	visit(form, "")
	return leaves, groups
}

// leafOf finds the leaf whose search text names subject as a word.
func leafOf(t *testing.T, leaves []treeLeaf, subject string) treeLeaf {
	t.Helper()
	for _, leaf := range leaves {
		for _, word := range strings.Fields(leaf.Search) {
			if word == subject {
				return leaf
			}
		}
	}
	t.Fatalf("no leaf for %s in %+v", subject, leaves)
	return treeLeaf{}
}

// navOf is the page's navigation.
func navOf(page string) string {
	start, end := strings.Index(page, "<nav>"), strings.Index(page, "</nav>")
	if start < 0 || end < start {
		return ""
	}
	return page[start:end]
}

func switchTo(t *testing.T, client *http.Client, base, username string) string {
	t.Helper()
	match := csrfField.FindStringSubmatch(get(t, client, base+"/tasks"))
	if match == nil {
		t.Fatal("no CSRF token")
	}
	response, err := client.PostForm(base+"/switch", url.Values{"csrf": {match[1]}, "username": {username}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || response.Request.URL.Path != "/tasks" {
		t.Fatalf("switch to %s: %d at %s", username, response.StatusCode, response.Request.URL)
	}
	return string(body)
}

func TestOrgTreeNavigationPicksOAAccountsFromTheTree(t *testing.T) {
	tree := loadSampleTree(t)
	parents := orgParents(tree)
	httpServer := orgTreeOA(t, tree, true, "", "")
	client := login(t, httpServer.URL, "zhouyan", "shared")
	page := get(t, client, httpServer.URL+"/tasks")
	nav := navOf(page)
	// The card's big line is the tree's person; the note keeps the account.
	if !strings.Contains(nav, `<div class="who-name">周燕 · 会计 · 财务部 会计核算中心</div><div class="who-note">zhouyan · 申请人兼审批人</div>`) {
		t.Fatalf("OA card big line: %s", nav)
	}
	// The tree replaces the <select>; its script replaces switch.js.
	if strings.Contains(nav, "<select") || strings.Contains(nav, `src="/switch.js"`) || !strings.Contains(nav, `<script src="/tree.js"></script>`) ||
		!strings.Contains(nav, `<form class="who-tree" method="post" action="/switch"><input type="hidden" name="csrf" value="`) ||
		!strings.Contains(nav, `"><details class="tree"><summary>换一个身份</summary><input type="search" placeholder="按姓名、拼音或部门筛选"`) {
		t.Fatalf("OA card switcher: %s", nav)
	}
	leaves, groups := parseTree(t, page, "/switch")
	if len(leaves) != len(parents)+1 {
		t.Fatalf("%d leaves for %d people and tester", len(leaves), len(parents))
	}
	for subject, parent := range parents {
		leaf := leafOf(t, leaves, subject)
		if leaf.Group != parent {
			t.Fatalf("%s sits under %q, want %q", subject, leaf.Group, parent)
		}
		if orgMissing[subject] {
			if !leaf.Missing || leaf.Value != "" || leaf.Field != "" || leaf.Current {
				t.Fatalf("%s has no account but is offered: %+v", subject, leaf)
			}
			continue
		}
		if leaf.Missing || leaf.Field != "username" || leaf.Value != subject || !leaf.ButtonIsPick || !leaf.ButtonIsSubmits ||
			leaf.Current != (subject == "zhouyan") || leaf.Disabled != (subject == "zhouyan") {
			t.Fatalf("%s: %+v", subject, leaf)
		}
	}
	// Persons show the tree's name and post, not the account's display name.
	if !strings.Contains(nav, `<button type="submit" name="username" value="sunhao" class="pick">孙浩 <span class="t">中心主任</span></button></li>`) ||
		!strings.Contains(nav, `<button type="submit" name="username" value="zhouyan" class="pick" disabled>周燕 <span class="t">会计</span></button></li>`) ||
		!strings.Contains(nav, `"><span>尤娜 <span class="t">销售代表</span></span></li>`) || strings.Contains(nav, "账户 sunhao") {
		t.Fatalf("leaf markup: %s", nav)
	}
	if !strings.Contains(nav, `<li class="leaf current" data-search="周燕 zhouyan 会计 员工 大米集团 管理线（副总经理 王建国） 财务部 会计核算中心">`) ||
		!strings.Contains(nav, `<li class="leaf missing" data-search="尤娜 youna`) {
		t.Fatalf("leaf classes: %s", nav)
	}
	// The account the tree does not name stays reachable, under 其他.
	if other := leafOf(t, leaves, "tester"); other.Group != "其他" || other.Value != "tester" || other.Field != "username" || other.Missing ||
		other.Current || strings.TrimSpace(other.Text) != "测试员 申请人兼审批人" {
		t.Fatalf("tester: %+v", other)
	}
	// The company and its lines are open; departments and centers start folded.
	for label, open := range map[string]bool{"大米集团": true, "管理线（副总经理 王建国）": true, "业务线（副总经理 李明）": true, "财务部": false,
		"人力资源部": false, "销售部": false, "会计核算中心": false, "资金管理中心": false, "招聘中心": false, "薪酬中心": false,
		"华东销售中心": false, "华南销售中心": false, "其他": true} {
		if got, ok := groups[label]; !ok || got != open {
			t.Fatalf("group %s open=%v (present %v), want %v", label, got, ok, open)
		}
	}
	if len(groups) != 13 || !strings.Contains(nav, `<li class="node"><details><summary>会计核算中心</summary><ul><li class="leaf"`) {
		t.Fatalf("groups %v: %s", groups, nav)
	}
	// The tree's style is in the head.
	head := page[:strings.Index(page, "</head>")]
	for _, rule := range []string{`.tree ul{list-style:none;margin:0;padding-left:14px}`, `.tree ul.org{max-height:360px;overflow:auto;`,
		`.tree li.node>details>summary{font-weight:700;`, `.tree button.pick{background:white;color:#17202a;`, `.tree button.pick:hover{background:#e8f1f8}`,
		`.tree .t{color:#617080;font-size:12px}`, `.tree li.current .pick{font-weight:700;color:#1769aa;`, `.tree li.current .pick::after{content:"（当前）"}`,
		`.tree li.missing{color:#9aa5b1;`} {
		if !strings.Contains(head, rule) {
			t.Fatalf("no %s in the head", rule)
		}
	}

	// A pick switches like the <select> did; the big line follows.
	page = switchTo(t, client, httpServer.URL, "sunhao")
	if !strings.Contains(navOf(page), `<div class="who-name">孙浩 · 中心主任 · 财务部 会计核算中心</div><div class="who-note">sunhao · 申请人兼审批人</div>`) {
		t.Fatalf("after switching to sunhao: %s", navOf(page))
	}
	leaves, _ = parseTree(t, page, "/switch")
	if picked, former := leafOf(t, leaves, "sunhao"), leafOf(t, leaves, "zhouyan"); !picked.Current || !picked.Disabled || former.Current || former.Disabled {
		t.Fatalf("after the switch: sunhao %+v zhouyan %+v", picked, former)
	}
	// An account the tree does not name keeps its display name.
	page = switchTo(t, client, httpServer.URL, "tester")
	if !strings.Contains(navOf(page), `<div class="who-name">测试员</div><div class="who-note">tester · 申请人兼审批人</div>`) {
		t.Fatalf("tester's big line: %s", navOf(page))
	}
	leaves, _ = parseTree(t, page, "/switch")
	if other := leafOf(t, leaves, "tester"); !other.Current || !other.Disabled {
		t.Fatalf("tester is not current: %+v", other)
	}

	// /tree.js is served to anyone, as a script.
	response, err := http.Get(httpServer.URL + "/tree.js")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(string(script), `nav details.tree`) || !strings.Contains(string(script), `data-search`) ||
		!strings.Contains(string(script), `"Enter"`) || !strings.Contains(string(script), `button.click()`) || !strings.Contains(string(script), `input.hidden = false`) {
		t.Fatalf("tree.js: %d %q %s", response.StatusCode, response.Header.Get("Content-Type"), script)
	}
	// switch.js leaves a tree's form alone.
	if switchJS := get(t, http.DefaultClient, httpServer.URL+"/switch.js"); !strings.Contains(switchJS, "if (!select) { return; }") ||
		!strings.Contains(switchJS, `if (!form || !form.querySelector('select[name="principal_id"]')) { return; }`) {
		t.Fatalf("switch.js: %s", switchJS)
	}

	// Outside the shared-password mode there is no switcher, but the big
	// line still comes from the tree.
	fixed := orgTreeOA(t, tree, false, "", "")
	page = get(t, login(t, fixed.URL, "zhouyan", "shared"), fixed.URL+"/tasks")
	if nav := navOf(page); strings.Contains(nav, "<form class=\"who-tree\"") || strings.Contains(nav, "<script") ||
		!strings.Contains(nav, `<div class="who-name">周燕 · 会计 · 财务部 会计核算中心</div>`) {
		t.Fatalf("without the switcher: %s", nav)
	}
}

func TestOrgTreeAgentCardJoinsCandidatesToTheTree(t *testing.T) {
	tree := loadSampleTree(t)
	parents := orgParents(tree)
	gateway := newFakeAgentGateway()
	gateway.identities = []map[string]string{
		{"label": "lzj", "principal_id": "principal_zhouyan", "subject": "zhouyan", "bound_by": "sunhao", "bound_at": "2026-10-28T02:12:00Z"},
	}
	// Every person of the tree but liuguodong is a candidate; zhuli has a
	// second principal, and ghost is a candidate the tree does not name.
	gateway.candidates = nil
	for subject := range parents {
		if subject != "liuguodong" {
			gateway.candidates = append(gateway.candidates, map[string]string{"principal_id": "principal_" + subject, "subject": subject,
				"department": "finance", "job_family": "staff", "center": ""})
		}
	}
	gateway.candidates = append(gateway.candidates,
		map[string]string{"principal_id": "principal_zhuli_2", "subject": "zhuli", "department": "sales", "job_family": "staff", "center": ""},
		map[string]string{"principal_id": "principal_ghost", "subject": "ghost", "department": "", "job_family": "center_head", "center": ""})
	fake := httptest.NewServer(gateway)
	t.Cleanup(fake.Close)
	httpServer := orgTreeOA(t, tree, true, fake.URL, "lzj")
	client := login(t, httpServer.URL, "zhouyan", "shared")
	page := get(t, client, httpServer.URL+"/tasks")
	card := agentCard(page)
	if !strings.Contains(card, `<div class="who-name">周燕 · 会计 · 财务部 会计核算中心</div><div class="who-note">共用令牌 lzj · `) ||
		strings.Contains(card, "<select") ||
		!strings.Contains(card, `<form class="who-tree" method="post" action="/agent-identity"><input type="hidden" name="csrf" value="`) ||
		!strings.Contains(card, `"><input type="hidden" name="next" value="/tasks"><input type="hidden" name="label" value="lzj"><details class="tree"><summary>换一个身份</summary>`) {
		t.Fatalf("agent card: %s", card)
	}
	leaves, groups := parseTree(t, page, "/agent-identity")
	if len(leaves) != len(parents)+2 || !groups["其他"] {
		t.Fatalf("%d leaves for %d people, zhuli's second principal and ghost; groups %v", len(leaves), len(parents), groups)
	}
	for subject, parent := range parents {
		leaf := leafOf(t, leaves, subject)
		if leaf.Group != parent {
			t.Fatalf("%s sits under %q, want %q", subject, leaf.Group, parent)
		}
		if subject == "liuguodong" {
			if !leaf.Missing || leaf.Value != "" || leaf.Current {
				t.Fatalf("liuguodong is no candidate but is offered: %+v", leaf)
			}
			continue
		}
		// weilan and youna have no OA account but are candidates: the agent
		// card offers them.
		if leaf.Missing || leaf.Field != "principal_id" || leaf.Value != "principal_"+subject || !leaf.ButtonIsPick ||
			leaf.Current != (subject == "zhouyan") || leaf.Disabled != (subject == "zhouyan") {
			t.Fatalf("%s: %+v", subject, leaf)
		}
	}
	// What the tree does not take stays reachable under 其他.
	var others []treeLeaf
	for _, leaf := range leaves {
		if leaf.Group == "其他" {
			others = append(others, leaf)
		}
	}
	if len(others) != 2 || others[0].Value != "principal_ghost" || strings.TrimSpace(others[0].Text) != "ghost 中心主任" ||
		others[1].Value != "principal_zhuli_2" || strings.TrimSpace(others[1].Text) != "账户 zhuli 员工" || others[0].Missing || others[1].Missing {
		t.Fatalf("其他: %+v", others)
	}

	// A pick posts the shown label and the principal; the card follows.
	response, _ := postAgentIdentity(t, client, httpServer.URL, url.Values{"label": {"lzj"}, "principal_id": {"principal_yangfan"}, "next": {"/tasks"}})
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("switch = %d", response.StatusCode)
	}
	if switches := gateway.seen("switch"); len(switches) != 1 || switches[0]["principal_id"] != "principal_yangfan" || switches[0]["label"] != "lzj" ||
		switches[0]["actor"] != "zhouyan" || gateway.rejected != 0 {
		t.Fatalf("switches %v rejected %d", switches, gateway.rejected)
	}
	page = get(t, client, httpServer.URL+"/tasks")
	if card := agentCard(page); !strings.Contains(card, `<div class="who-name">杨帆 · 中心主任 · 销售部 华东销售中心</div>`) {
		t.Fatalf("after the switch: %s", card)
	}
	leaves, _ = parseTree(t, page, "/agent-identity")
	if picked, former := leafOf(t, leaves, "yangfan"), leafOf(t, leaves, "zhouyan"); !picked.Current || !picked.Disabled || former.Current {
		t.Fatalf("after the switch: yangfan %+v zhouyan %+v", picked, former)
	}
	// The OA card is unchanged by the agent switch.
	if !strings.Contains(oaCard(page), `<div class="who-name">周燕 · 会计 · 财务部 会计核算中心</div>`) {
		t.Fatalf("OA card after the agent switch: %s", oaCard(page))
	}
}
