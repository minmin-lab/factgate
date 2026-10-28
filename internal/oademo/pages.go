package oademo

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// Pages (KB1-LOOP-147, the demo's usability pass): one login with an account
// switcher in the shared-password mode, a to-do list and a history page, and
// an approval page a person can read -- a one-line summary, the chain's
// progress, Chinese labels from an optional labels file, durations instead of
// milliseconds, and a narrowing form made of checkboxes. The technical
// identifiers stay on the page, folded away, where the scripts and tests that
// scrape them (Task ID, the budget inputs, the CSRF field) still find them.

// Labels maps Catalog identifiers to display names: scope names, scope
// values per scope, and products. Missing entries show the identifier.
type Labels struct {
	Scopes   map[string]string            `yaml:"scopes" json:"scopes"`
	Values   map[string]map[string]string `yaml:"values" json:"values"`
	Products map[string]string            `yaml:"products" json:"products"`
	// Groups arrange the account switcher: each group (a business line under
	// its leader) lists its accounts in display order; accounts in no group
	// come first, ungrouped.
	Groups []AccountGroup `yaml:"groups" json:"groups"`
}

// AccountGroup is one <optgroup> of the account switcher.
type AccountGroup struct {
	Label   string   `yaml:"label" json:"label"`
	Members []string `yaml:"members" json:"members"`
}

// accountGroups arranges the configured accounts by Labels.Groups: the
// ungrouped ones first (label empty), then each group with the members it
// actually has, in the group's order.
func (s *Server) accountGroups() []accountGroupView {
	options := s.loginOptions()
	byID := make(map[string]loginOption, len(options))
	grouped := map[string]bool{}
	for _, option := range options {
		byID[option.ID] = option
	}
	for _, group := range s.config.Labels.Groups {
		for _, member := range group.Members {
			grouped[strings.ToLower(member)] = true
		}
	}
	var views []accountGroupView
	var loose []loginOption
	for _, option := range options {
		if !grouped[option.ID] {
			loose = append(loose, option)
		}
	}
	if len(loose) > 0 {
		views = append(views, accountGroupView{Accounts: loose})
	}
	for _, group := range s.config.Labels.Groups {
		view := accountGroupView{Label: group.Label}
		for _, member := range group.Members {
			if option, ok := byID[strings.ToLower(member)]; ok {
				view.Accounts = append(view.Accounts, option)
			}
		}
		if len(view.Accounts) > 0 {
			views = append(views, view)
		}
	}
	return views
}

type accountGroupView struct {
	Label    string
	Accounts []loginOption
}

// LoadLabels reads a labels YAML file; an empty path yields no labels.
func LoadLabels(path string) (Labels, error) {
	var labels Labels
	if path == "" {
		return labels, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return labels, err
	}
	if err := yaml.Unmarshal(raw, &labels); err != nil {
		return labels, fmt.Errorf("labels file %s: %w", path, err)
	}
	return labels, nil
}

func (l Labels) scope(name string) string {
	if label := l.Scopes[name]; label != "" {
		return label
	}
	return name
}

func (l Labels) value(scope, value string) string {
	if label := l.Values[scope][value]; label != "" {
		return label
	}
	return value
}

func (l Labels) product(name string) string {
	if label := l.Products[name]; label != "" {
		return label
	}
	return name
}

// humanDuration says a millisecond count the way people do: in the largest
// unit the duration reaches, rounded to one decimal (a parent's remaining
// time is rarely a whole number of anything).
func humanDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	units := []struct {
		unit time.Duration
		name string
	}{{365 * 24 * time.Hour, "年"}, {24 * time.Hour, "天"}, {time.Hour, "小时"}, {time.Minute, "分钟"}, {time.Second, "秒"}}
	for _, candidate := range units {
		if d < candidate.unit {
			continue
		}
		value := math.Round(float64(d)/float64(candidate.unit)*10) / 10
		if value == math.Trunc(value) {
			return fmt.Sprintf("%d %s", int64(value), candidate.name)
		}
		return fmt.Sprintf("%.1f %s", value, candidate.name)
	}
	return fmt.Sprintf("%d 毫秒", ms)
}

// pageTime is how the pages show a moment: to the minute, in UTC.
func pageTime(at time.Time) string { return at.UTC().Format("2006-01-02 15:04") + " UTC" }

const longLivedTTL = 30 * 24 * time.Hour

// kindLabel names what a draft is: a long-lived authorization (a project), a
// query inside one, or an ordinary task.
func kindLabel(draft Draft) string {
	switch {
	case draft.Manifest.ParentTaskID != "":
		return "项目内查询"
	case time.Duration(draft.Manifest.Budget.TaskTTLMS)*time.Millisecond >= longLivedTTL:
		return "项目（长期授权）"
	}
	return "任务"
}

var stateLabels = map[string]string{"draft": "待提交", "pending": "待审批", "approved": "已批准", "narrowed": "缩小后批准",
	"rejected": "已拒绝", "not_granted": "未签发", "withdrawn": "已撤回"}

func stateLabel(state string) string {
	if label, ok := stateLabels[state]; ok {
		return label
	}
	return state
}

// withdrawalLabels name a withdrawn draft by why TaskGate archived its task.
var withdrawalLabels = map[string]string{approval.WithdrawalRevoked: "已撤销", approval.WithdrawalExpired: "已过期"}

// draftStateLabel is a draft's state as the pages show it: a withdrawn draft
// by its reason (revoked or expired), any other by stateLabel.
func draftStateLabel(draft Draft) string {
	if draft.State == "withdrawn" && draft.Withdrawal != nil {
		if label, ok := withdrawalLabels[draft.Withdrawal.Reason]; ok {
			return label
		}
	}
	return stateLabel(draft.State)
}

// View models the task page renders.

type valueRow struct{ Value, Label string }

type scopeRow struct {
	Name, Label string
	Enum        bool
	Values      []valueRow
	From, To    string
}

type productRow struct {
	Name, Label string
	Columns     []string
}

type progressRow struct {
	Who, Label, Status, Decision, When string
}

type draftView struct {
	Draft    Draft
	Kind     string
	Summary  string
	Scopes   []scopeRow // the envelope the current approver decides on
	Products []productRow
	Progress []progressRow
	Envelope draftEnvelope
}

func (s *Server) scopeRows(scope map[string]any) []scopeRow {
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]scopeRow, 0, len(names))
	for _, name := range names {
		row := scopeRow{Name: name, Label: s.config.Labels.scope(name)}
		switch typed := scope[name].(type) {
		case []string:
			row.Enum = true
			for _, value := range typed {
				row.Values = append(row.Values, valueRow{Value: value, Label: s.config.Labels.value(name, value)})
			}
		case []any:
			row.Enum = true
			for _, item := range typed {
				value, _ := item.(string)
				row.Values = append(row.Values, valueRow{Value: value, Label: s.config.Labels.value(name, value)})
			}
		case map[string]any:
			row.From, _ = typed["from"].(string)
			row.To, _ = typed["to"].(string)
		case map[string]string:
			row.From, row.To = typed["from"], typed["to"]
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) productRows(products []string, columns map[string][]string) []productRow {
	rows := make([]productRow, 0, len(products))
	for _, product := range products {
		rows = append(rows, productRow{Name: product, Label: s.config.Labels.product(product), Columns: append([]string(nil), columns[product]...)})
	}
	return rows
}

// view builds what the task page shows for a draft.
func (s *Server) view(draft Draft) draftView {
	envelope := draft.Envelope()
	if draft.ParentExpiresAt != nil {
		// What a decision now could grant at most: the parent's remaining time.
		if allowed := draft.ParentExpiresAt.Sub(s.config.Clock().UTC()).Truncate(time.Millisecond).Milliseconds(); allowed > 0 && allowed < envelope.Budget.TaskTTLMS {
			envelope.Budget.TaskTTLMS = allowed
		}
	}
	view := draftView{Draft: draft, Kind: kindLabel(draft), Envelope: envelope,
		Scopes: s.scopeRows(envelope.Scope), Products: s.productRows(envelope.Products, envelope.Columns)}
	var parts []string
	for _, row := range view.Scopes {
		if row.Enum {
			labels := make([]string, 0, len(row.Values))
			for _, value := range row.Values {
				labels = append(labels, value.Label)
			}
			parts = append(parts, row.Label+" "+strings.Join(labels, "、"))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s 至 %s", row.Label, row.From, row.To))
		}
	}
	products := make([]string, 0, len(view.Products))
	for _, row := range view.Products {
		products = append(products, row.Label)
	}
	view.Summary = fmt.Sprintf("%s 申请 %s 的资料（%s），最多 %d 次查询，有效期 %s", s.displayName(draft.Manifest.HumanSubject),
		strings.Join(parts, "；"), strings.Join(products, "、"), envelope.Budget.MaxQueries, humanDuration(envelope.Budget.TaskTTLMS))
	if draft.IssuedBy != "" {
		view.Summary += "；由 " + s.displayName(draft.IssuedBy) + " 代开"
	}
	if draft.MemberConfirm && draft.Approver != "" {
		view.Summary += "；由项目成员 " + s.displayName(draft.Approver) + " 本人确认"
	}
	for _, decision := range draft.History {
		view.Progress = append(view.Progress, progressRow{Who: s.displayName(decision.Actor), Label: s.approverLabel(draft, decision.Actor),
			Status: "done", Decision: decisionLabel(decision.Decision), When: pageTime(decision.At)})
	}
	if draft.State == "pending" && draft.Approver != "" {
		view.Progress = append(view.Progress, progressRow{Who: s.displayName(draft.Approver), Label: s.approverLabel(draft, draft.Approver), Status: "current"})
	}
	return view
}

// approverLabel says what an approver is to the draft: its requester
// confirming their own query, a project member confirming the query
// delegated to them, or an approver.
func (s *Server) approverLabel(draft Draft, approver string) string {
	switch {
	case strings.EqualFold(approver, draft.Manifest.HumanSubject):
		return "本人确认"
	case draft.MemberConfirm && strings.EqualFold(approver, draft.Approver):
		return "成员确认"
	}
	return "审批"
}

// relatesAsRequester reports whether username submitted, or issued, the
// draft, or is the member who confirms a member-confirmed child; each of them
// may submit it.
func (d Draft) relatesAsRequester(username string) bool {
	return strings.EqualFold(d.Manifest.HumanSubject, username) || (d.IssuedBy != "" && strings.EqualFold(d.IssuedBy, username)) ||
		d.isMember(username)
}

// isMember reports whether username is the member who confirms a
// member-confirmed child.
func (d Draft) isMember(username string) bool {
	return d.MemberConfirm && d.Approver != "" && strings.EqualFold(d.Approver, username)
}

// submitters lists who is expected to submit the draft, in the order a
// switch offers them: a member-confirmed child's member alone (its owner may
// still submit, but the member confirms it next), otherwise the requester and
// the issuer.
func (d Draft) submitters() []string {
	if d.MemberConfirm && d.Approver != "" {
		return []string{d.Approver}
	}
	return []string{d.Manifest.HumanSubject, d.IssuedBy}
}

func (d Draft) expectsSubmissionFrom(username string) bool {
	for _, submitter := range d.submitters() {
		if submitter != "" && strings.EqualFold(submitter, username) {
			return true
		}
	}
	return false
}

// relatesAsApprover reports whether username decides, or decided, the draft.
func (d Draft) relatesAsApprover(username string) bool {
	return d.ApprovalMode == "manual" && (strings.EqualFold(d.Approver, username) || d.decidedBy(username))
}

func (d Draft) open() bool { return d.State == "draft" || d.State == "pending" }

// visible reports whether a session may see a draft.
func visible(draft *Draft, data sessionData) bool {
	return draft.relatesAsRequester(data.Username) || (data.CanApprove && draft.relatesAsApprover(data.Username))
}

// history lists every draft the session relates to, newest change first.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	data.AgentIdentity = s.agentNav(r)
	s.mu.RLock()
	drafts := make([]Draft, 0, len(s.drafts))
	for _, draft := range s.drafts {
		if visible(draft, data) {
			drafts = append(drafts, cloneDraft(draft))
		}
	}
	s.mu.RUnlock()
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].UpdatedAt.After(drafts[j].UpdatedAt) })
	views := make([]draftView, 0, len(drafts))
	for _, draft := range drafts {
		views = append(views, s.view(draft))
	}
	s.render(w, "history", map[string]any{"Session": data, "Views": views})
}

// switchAccount re-signs the session for another configured account. It is
// offered only in the shared-password mode (Config.AllowSwitch): the accounts
// then share one password, so switching reveals nothing a login would not.
// It lands on next when that is the to-do list or a draft's page, otherwise
// on the to-do list.
func (s *Server) switchAccount(w http.ResponseWriter, r *http.Request) {
	if !s.config.AllowSwitch {
		http.NotFound(w, r)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.Form.Get("username")))
	if _, ok := s.users[username]; !ok {
		http.Error(w, "unknown account", http.StatusBadRequest)
		return
	}
	expires := s.config.Clock().Add(8 * time.Hour)
	http.SetCookie(w, &http.Cookie{Name: "oa_session", Value: s.signSession(username, expires), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, Expires: expires})
	next := "/tasks"
	if candidate := r.Form.Get("next"); switchNext.MatchString(candidate) {
		next = candidate
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// switchNext is where a switch may land: the to-do list or one draft's page.
// Anything else (another site, "//host", "..", a query) falls back to /tasks.
var switchNext = regexp.MustCompile(`^/tasks(/[A-Za-z0-9_-]+)?$`)

// canAct reports whether username may act on the draft now: submit it (its
// requester, issuer or confirming member, while unsubmitted) or decide it (its current
// approver, while pending) -- the checks submitDraft and decideDraft make.
func (s *Server) canAct(draft Draft, username string) bool {
	account, ok := s.users[strings.ToLower(username)]
	if !ok {
		return false
	}
	switch draft.State {
	case "draft":
		return draft.relatesAsRequester(username)
	case "pending":
		return account.CanApprove && draft.ApprovalMode == "manual" && strings.EqualFold(draft.Approver, username)
	}
	return false
}

// todoAccount is one other account's to-dos on the to-do page.
type todoAccount struct {
	ID, Name string
	Drafts   []todoRow
}

type todoRow struct{ ID, State, Objective, Subject string }

// otherTodos is the shared-password mode's overview of what waits for the
// other accounts: in the switcher's order, each account that may act on an
// open draft now, with those drafts newest first.
func (s *Server) otherTodos(data sessionData, open []Draft) []todoAccount {
	if !data.AllowSwitch || len(open) == 0 {
		return nil
	}
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt.After(open[j].CreatedAt) })
	seen := map[string]bool{data.Username: true}
	var accounts []todoAccount
	for _, group := range data.AccountGroups {
		for _, option := range group.Accounts {
			if seen[option.ID] {
				continue
			}
			seen[option.ID] = true
			account := todoAccount{ID: option.ID, Name: option.Name}
			for _, draft := range open {
				if s.canAct(draft, option.ID) && (draft.State != "draft" || draft.expectsSubmissionFrom(option.ID)) {
					account.Drafts = append(account.Drafts, todoRow{ID: draft.ID, State: draft.State,
						Objective: draft.Manifest.DeclaredObjective, Subject: s.displayName(draft.Manifest.HumanSubject)})
				}
			}
			if len(account.Drafts) > 0 {
				accounts = append(accounts, account)
			}
		}
	}
	return accounts
}

// switchOffer is the task page's one-click switch to whoever acts on the
// draft now, when that is not the session (the shared-password mode only).
type switchOffer struct{ ID, Name, Action string }

func (s *Server) switchOffer(data sessionData, draft Draft) *switchOffer {
	if !data.AllowSwitch {
		return nil
	}
	var candidates []string
	action := ""
	switch {
	case draft.State == "pending" && !strings.EqualFold(draft.Approver, data.Username):
		candidates, action = []string{draft.Approver}, "审批"
	case draft.State == "draft" && !draft.expectsSubmissionFrom(data.Username):
		candidates, action = draft.submitters(), "提交"
	}
	for _, candidate := range candidates {
		if name := strings.ToLower(candidate); name != "" && s.canAct(draft, name) {
			return &switchOffer{ID: name, Name: s.users[name].Name, Action: action}
		}
	}
	return nil
}

// answerWait is how long a decided draft's page keeps polling for changes
// while TaskGate's answer (the next approver, or why no grant was issued)
// has not arrived; a final approval TaskGate simply takes leaves no note, so
// a page rendered after the window no longer polls.
const answerWait = 10 * time.Second

// awaitsChange reports whether the task page should poll for changes
// (pollScript) and reload when one comes: the draft waits for someone
// else's decision (the approver's own page does not poll, so it keeps a
// narrowing form being filled in), or for TaskGate's answer to a decision
// just made.
func (s *Server) awaitsChange(draft Draft, canDecide bool) bool {
	switch draft.State {
	case "pending":
		return !canDecide
	case "approved", "narrowed":
		if draft.Note != "" || len(draft.History) == 0 {
			return false
		}
		return s.config.Clock().Sub(draft.History[len(draft.History)-1].At) < answerWait
	}
	return false
}

func draftIDParam(r *http.Request) string { return chi.URLParam(r, "draftID") }

// switchScript submits the account card's switcher as soon as an identity
// is chosen, and likewise the agent card (agent_identity.go): choosing a
// principal switches the shown label to it.
// The page's Content-Security-Policy allows scripts from the OA itself only,
// so it is served here rather than written inline; without scripts the two
// "切换身份" buttons stay.
func switchScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(`document.addEventListener("DOMContentLoaded", function () {
  var form = document.querySelector('nav form[action="/switch"]');
  if (!form) { return; }
  var select = form.querySelector('select[name="username"]');
  var button = form.querySelector('button[type="submit"]');
  if (button) { button.hidden = true; }
  select.addEventListener("change", function () { form.submit(); });
});
document.addEventListener("DOMContentLoaded", function () {
  var form = document.querySelector('nav form[action="/agent-identity"]');
  if (!form) { return; }
  var button = form.querySelector('button[type="submit"]');
  if (button) { button.hidden = true; }
  form.querySelector('select[name="principal_id"]').addEventListener("change", function () { form.submit(); });
});
`))
}

// pollScript keeps a waiting task page current without interrupting whoever
// reads it; it replaced a meta refresh that reloaded the page every 3 s and
// so closed an open menu and lost the scroll position and the selection.
// It asks /tasks/{id}/state (draftState) for the draft's version every 3 s,
// after two minutes every 10 s, and once the version differs from the one
// the page was rendered from, reloads the page as soon as the user is idle:
// no field, menu or button focused, no pointer button held, no text
// selected, and no input, key, pointer or scroll in the last 2 s. It stops
// after 3 failed polls in a row (an error status, a redirect such as to the
// login page, or an answer without a version). Served from the OA itself
// for the Content-Security-Policy, like switchScript.
func pollScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(`(function () {
  var meta = document.querySelector('meta[name="draft-version"]');
  if (!meta || !window.fetch) { return; }
  var shown = meta.getAttribute("content");
  var stateURL = location.pathname.replace(/\/+$/, "") + "/state";
  var started = new Date().getTime();
  var lastActivity = 0;
  var pointerHeld = false;
  var pending = false;
  var failures = 0;
  function now() { return new Date().getTime(); }
  function touched() { lastActivity = now(); }
  document.addEventListener("input", touched, true);
  document.addEventListener("keydown", touched, true);
  document.addEventListener("scroll", touched, true);
  document.addEventListener("pointerdown", function () { pointerHeld = true; touched(); }, true);
  document.addEventListener("pointerup", function () { pointerHeld = false; touched(); }, true);
  document.addEventListener("pointercancel", function () { pointerHeld = false; }, true);
  function idle() {
    var active = document.activeElement;
    var tag = active && active.tagName ? active.tagName.toUpperCase() : "";
    if (tag === "SELECT" || tag === "INPUT" || tag === "TEXTAREA" || tag === "BUTTON") { return false; }
    if (pointerHeld) { return false; }
    var selection = window.getSelection ? window.getSelection() : null;
    if (selection && String(selection) !== "") { return false; }
    return now() - lastActivity >= 2000;
  }
  function schedule() {
    setTimeout(tick, now() - started < 120000 ? 3000 : 10000);
  }
  function settle() {
    if (idle()) { location.reload(); return; }
    schedule();
  }
  function tick() {
    if (pending) { settle(); return; }
    fetch(stateURL, { credentials: "same-origin", cache: "no-store", headers: { "Accept": "application/json" } }).then(function (response) {
      if (!response.ok || response.redirected) { throw new Error("state answered " + response.status); }
      return response.json();
    }).then(function (state) {
      if (!state || typeof state.version !== "string") { throw new Error("state has no version"); }
      failures = 0;
      if (state.version !== shown) { pending = true; settle(); return; }
      schedule();
    }).catch(function () {
      failures += 1;
      if (failures < 3) { schedule(); }
    });
  }
  schedule();
})();
`))
}

// formNarrowing reads the checkbox form of the narrowing section when the
// JSON fields (kept for scripts) are absent.

func checkedProducts(form map[string][]string) []string {
	values := form["approved_products"]
	if len(values) == 1 {
		return nil // one value: the JSON or comma form, parsed by the caller
	}
	return append([]string(nil), values...)
}

func checkedColumns(form map[string][]string, products []string) (map[string][]string, error) {
	columns := make(map[string][]string, len(products))
	for _, product := range products {
		selected := form["column:"+product]
		if len(selected) == 0 {
			return nil, fmt.Errorf("product %s keeps no column", product)
		}
		columns[product] = append([]string(nil), selected...)
	}
	return columns, nil
}

func checkedScope(values map[string][]string, parent map[string]any) (map[string]any, error) {
	form := formValues(values)
	scope := make(map[string]any, len(parent))
	for name, value := range parent {
		switch typed := value.(type) {
		case []any, []string:
			selected := form["scope:"+name]
			if len(selected) == 0 {
				return nil, fmt.Errorf("scope %s keeps no value", name)
			}
			values := make([]any, 0, len(selected))
			for _, item := range selected {
				values = append(values, item)
			}
			scope[name] = values
			_ = typed
		case map[string]any:
			from, to := strings.TrimSpace(form.get("scope_from:"+name)), strings.TrimSpace(form.get("scope_to:"+name))
			narrowed := map[string]any{}
			for key, item := range typed {
				narrowed[key] = item
			}
			if from != "" {
				narrowed["from"] = from
			}
			if to != "" {
				narrowed["to"] = to
			}
			scope[name] = narrowed
		default:
			scope[name] = value
		}
	}
	return scope, nil
}

type formValues map[string][]string

func (f formValues) get(key string) string {
	if values := f[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

const pageTemplates = `
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">{{if .Refresh}}<meta name="draft-version" content="{{.Version}}"><script src="/poll.js" defer></script>{{end}}<title>Task-bound OA Demo</title><style>
body{font-family:system-ui,sans-serif;background:#f4f6f8;color:#17202a;margin:0}.wrap{max-width:920px;margin:32px auto;padding:0 20px}.card{background:white;border:1px solid #dfe5eb;border-radius:12px;padding:24px;margin:16px 0;box-shadow:0 2px 10px #17202a0d}input,select,textarea,button{font:inherit;padding:8px 12px;border:1px solid #bcc7d1;border-radius:8px}textarea{box-sizing:border-box;width:100%;min-height:70px}button{background:#1769aa;color:white;border:0;cursor:pointer}.danger{background:#a92828}.muted{color:#617080}.tag{display:inline-block;padding:3px 8px;border-radius:99px;background:#e8f1f8;margin-right:6px}nav .bar{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap}nav a{margin-right:12px}.whos{display:flex;flex-wrap:wrap;gap:16px;margin:16px 0}.who{flex:1 1 320px;background:white;border:1px solid #dfe5eb;border-left:4px solid #1769aa;border-radius:8px;padding:12px 16px}.who-title{font-size:15px;font-weight:700;color:#1769aa}#who-agent{border-left-color:#2e7d32}#who-agent .who-title{color:#2e7d32}.who-name{font-size:17px;font-weight:700;margin:4px 0 2px}.who-note{font-size:13px;color:#617080}.who-pick{display:flex;align-items:center;gap:8px;margin-top:8px}.who-pick label{display:flex;align-items:center;flex:1 1 auto;min-width:0;white-space:nowrap}.who-pick select{box-sizing:border-box;width:100%;min-width:0}form.inline{display:inline}.narrow{margin-top:20px;padding-top:16px;border-top:1px solid #dfe5eb}.budget-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px}.budget-grid input{width:90%}dt{font-weight:600;margin-top:12px}dd{margin-left:0}.error{color:#a92828}.note{background:#fff7e0;border:1px solid #f0d58a;border-radius:8px;padding:10px 12px}ol.history{padding-left:20px}a{color:#1769aa;text-decoration:none}.summary{font-size:1.1em;line-height:1.6}.progress{list-style:none;padding:0;margin:8px 0}.progress li{padding:6px 0;border-bottom:1px dashed #e3e8ee}.progress .done::before{content:"✓ ";color:#2e7d32}.progress .current::before{content:"▶ ";color:#1769aa}.choices label{display:inline-block;margin:4px 12px 4px 0}details{margin-top:16px}details summary{cursor:pointer;color:#617080}code{word-break:break-all}table.history{width:100%;border-collapse:collapse}table.history td,table.history th{text-align:left;padding:8px 6px;border-bottom:1px solid #e3e8ee;vertical-align:top}ul.todos{list-style:none;padding:0;margin:8px 0}ul.todos li{padding:8px 0;border-bottom:1px dashed #e3e8ee}
</style></head><body><div class="wrap">{{end}}
{{define "foot"}}</div></body></html>{{end}}
{{define "login"}}{{template "head" .}}<div class="card"><h1>OA Demo 登录</h1>{{if .PasswordOnly}}<p class="muted">输入口令登录。登录后默认是普通员工 {{.Default}} 的身份，页面顶部「OA 角色切换」一栏可随时切换为任一管理员、主任或分管领导。</p>{{else}}<p class="muted">申请人提交自己的申请；审批人只看到指派给自己或自己审批过的工单。</p>{{end}}{{if .Error}}<p class="error">{{if .PasswordOnly}}口令错误{{else}}用户名或密码错误{{end}}</p>{{end}}<form method="post" action="/login"><input type="hidden" name="csrf" value="{{.CSRF}}">{{if not .PasswordOnly}}<p><label>用户<br><select name="username">{{range .Users}}<option value="{{.ID}}">{{.Name}}（{{.ID}}，{{roleLabel .Role}}）</option>{{end}}</select></label></p>{{end}}<p><label>{{if .PasswordOnly}}口令{{else}}密码{{end}}<br><input type="password" name="password" required autocomplete="current-password"></label></p><button type="submit">登录</button></form></div>{{template "foot" .}}{{end}}
{{define "nav"}}<nav><div class="bar"><strong>Task-bound OA</strong><div><a href="/tasks">待办</a><a href="/history">审批记录</a><form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">退出</button></form></div></div><div class="whos"><div class="who" id="who-oa"><div class="who-title">OA 角色切换（审批用）</div><div class="who-name">{{.DisplayName}}</div><div class="who-note">{{.Username}} · {{roleLabel .Role}}</div>{{if .AllowSwitch}}<form class="who-pick" method="post" action="/switch"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>换一个身份：<select name="username">{{$current := .Username}}{{range .AccountGroups}}{{if .Label}}<optgroup label="{{.Label}}">{{end}}{{range .Accounts}}<option value="{{.ID}}"{{if eq .ID $current}} selected{{end}}>{{.Name}}</option>{{end}}{{if .Label}}</optgroup>{{end}}{{end}}</select></label><button type="submit">切换身份</button></form>{{end}}</div>{{with .AgentIdentity}}<div class="who" id="who-agent"><div class="who-title">MCP 角色切换（agent 申请与查询用）</div>{{if .Unavailable}}<div class="who-name error">网关暂不可用（{{.Unavailable}}）</div>{{else if .Shown}}<div class="who-name">{{.Principal}}</div><div class="who-note">共用令牌 {{.Label}}{{if .Binding}} · {{.Binding}}{{end}}</div><div class="who-note">切换立即生效；切完对 agent 说一句「你被切换了，调 whoami 确认」。</div><form class="who-pick" method="post" action="/agent-identity"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="next" value="{{.Next}}"><input type="hidden" name="label" value="{{.Label}}"><label>换一个身份：<select name="principal_id">{{range .Groups}}<optgroup label="{{.Label}}">{{range .Options}}<option value="{{.PrincipalID}}"{{if .Selected}} selected{{end}}>{{.Text}}</option>{{end}}</optgroup>{{end}}</select></label><button type="submit">切换身份</button></form>{{else if .Unregistered}}<div class="who-name error">网关未登记标签 {{.Unregistered}}</div>{{else}}<div class="who-name muted">网关没有登记任何 agent</div>{{end}}</div>{{end}}</div>{{if .AllowSwitch}}<script src="/switch.js"></script>{{end}}</nav>{{end}}
{{define "tasks"}}{{template "head" .}}{{template "nav" .Session}}<h1>待办</h1><p class="muted">待提交或待审批的工单；已办结的在<a href="/history">审批记录</a>。</p>{{range .Views}}<div class="card"><span class="tag">{{draftStateLabel .Draft}}</span><span class="tag">{{.Kind}}</span><h2><a href="/tasks/{{.Draft.ID}}">{{.Draft.Manifest.DeclaredObjective}}</a></h2><p class="summary">{{.Summary}}</p><p class="muted">{{if .Draft.Approver}}当前审批人 {{.Draft.Approver}} · {{end}}Task {{.Draft.Manifest.TaskID}}</p>{{if .Draft.Note}}<p class="note">{{.Draft.Note}}</p>{{end}}</div>{{else}}<div class="card"><p>暂无待办。</p></div>{{end}}{{if .Others}}<h2>其他身份的待办</h2><p class="muted">以下工单正等待其他身份处理；点“切换并打开”即切换到该身份并打开工单。</p>{{range .Others}}{{$account := .ID}}<div class="card"><h3>{{.Name}}（{{len .Drafts}} 张）</h3><ul class="todos">{{range .Drafts}}<li><span class="tag">{{stateLabel .State}}</span>{{.Objective}} <span class="muted">· 申请人 {{.Subject}}</span> <form class="inline" method="post" action="/switch"><input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="username" value="{{$account}}"><input type="hidden" name="next" value="/tasks/{{.ID}}"><button type="submit">切换并打开</button></form></li>{{end}}</ul></div>{{end}}{{end}}{{template "foot" .}}{{end}}
{{define "history"}}{{template "head" .}}{{template "nav" .Session}}<h1>审批记录</h1><p class="muted">你提交、代开或审批过的全部工单，按最近变动排序。</p>{{if .Views}}<div class="card"><table class="history"><tr><th>工单</th><th>状态</th><th>审批记录</th></tr>{{range .Views}}<tr><td><a href="/tasks/{{.Draft.ID}}">{{.Draft.Manifest.DeclaredObjective}}</a><br><span class="muted">{{.Kind}} · 申请人 {{.Draft.Manifest.HumanSubject}}{{if .Draft.IssuedBy}} · 代开人 {{.Draft.IssuedBy}}{{end}}{{if .Draft.MemberConfirm}} · 确认成员 {{.Draft.Approver}}{{end}}{{if .Draft.AgentSubmitted}} · agent 代为提交{{end}}</span></td><td>{{draftStateLabel .Draft}}</td><td>{{range .Progress}}{{if eq .Status "done"}}{{.Who}} {{.Decision}}（{{.When}}）<br>{{else}}等待 {{.Who}} {{.Label}}<br>{{end}}{{end}}{{if .Draft.Note}}<span class="muted">{{.Draft.Note}}</span>{{end}}</td></tr>{{end}}</table></div>{{else}}<div class="card"><p>暂无记录。</p></div>{{end}}{{template "foot" .}}{{end}}
{{define "task"}}{{template "head" .}}{{template "nav" .Session}}{{$view := .View}}{{$draft := .View.Draft}}{{$envelope := .View.Envelope}}<div class="card"><span class="tag">{{draftStateLabel $draft}}</span><span class="tag">{{$view.Kind}}</span><h1>{{$draft.Manifest.DeclaredObjective}}</h1><p class="summary">{{$view.Summary}}</p>
{{if $view.Progress}}<h2>审批进度</h2><ol class="progress">{{range $view.Progress}}<li class="{{.Status}}">{{if eq .Status "done"}}{{.Who}} {{.Decision}} · {{.When}}{{else}}等待 {{.Who}}{{.Label}}{{end}}</li>{{end}}</ol>{{end}}{{if $draft.Note}}<p class="note">{{$draft.Note}}</p>{{end}}{{if .Refresh}}<p class="muted">等待处理结果，有新进展时本页自动更新；你在页面上操作时不会被打断。</p>{{end}}
<h2>{{if $envelope.Granted}}上一级批准的范围{{else}}申请的范围{{end}}</h2>{{if $envelope.Granted}}<p class="muted">本级在此基础上批准或缩小，不能扩大。</p>{{end}}<dl>{{range $view.Scopes}}<dt>{{.Label}}</dt><dd>{{if .Enum}}{{range $i, $v := .Values}}{{if $i}}、{{end}}{{$v.Label}}{{end}}{{else}}{{.From}} 至 {{.To}}{{end}}</dd>{{end}}<dt>数据产品与字段</dt><dd>{{range $view.Products}}<strong>{{.Label}}</strong>：{{range $i, $c := .Columns}}{{if $i}}、{{end}}{{$c}}{{end}}<br>{{end}}</dd><dt>预算与有效期</dt><dd>最多 {{$envelope.Budget.MaxQueries}} 次查询，每次最多 {{$envelope.Budget.MaxResultRows}} 行；有效期 {{humanDuration $envelope.Budget.TaskTTLMS}}{{if $envelope.Granted}}（从最后一级批准时起算）{{end}}</dd></dl>
{{with .SwitchTo}}<form method="post" action="/switch"><input type="hidden" name="csrf" value="{{$.Session.CSRF}}"><input type="hidden" name="username" value="{{.ID}}"><input type="hidden" name="next" value="/tasks/{{$draft.ID}}"><button type="submit">切换到 {{.Name}} 并{{.Action}}</button></form>{{end}}
{{if .CanSubmit}}<form method="post" action="/tasks/{{$draft.ID}}/submit"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button type="submit">提交申请</button></form>{{end}}
{{if .CanDecide}}<form class="inline" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="approved"><button type="submit">{{if .SelfConfirm}}确认{{else}}按原申请批准{{end}}</button></form> <form class="inline" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="rejected"><button class="danger" type="submit">拒绝</button></form>
<form class="narrow" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="narrow"><h2>缩小后批准</h2><p class="muted">去掉不该给的项即可，只能缩小，不能扩大。</p>{{range $view.Scopes}}<p><strong>{{.Label}}</strong><br><span class="choices">{{if .Enum}}{{$scope := .Name}}{{range .Values}}<label><input type="checkbox" name="scope:{{$scope}}" value="{{.Value}}" checked> {{.Label}}</label>{{end}}{{else}}<label>从 <input type="date" name="scope_from:{{.Name}}" value="{{.From}}" min="{{.From}}" max="{{.To}}"></label> <label>至 <input type="date" name="scope_to:{{.Name}}" value="{{.To}}" min="{{.From}}" max="{{.To}}"></label>{{end}}</span></p>{{end}}<p><strong>数据产品与字段</strong></p>{{range $view.Products}}<p><label><input type="checkbox" name="approved_products" value="{{.Name}}" checked> <strong>{{.Label}}</strong></label><br><span class="choices">{{$product := .Name}}{{range .Columns}}<label><input type="checkbox" name="column:{{$product}}" value="{{.}}" checked> {{.}}</label>{{end}}</span></p>{{end}}<div class="budget-grid"><label>最大查询次数<input name="max_queries" type="number" min="1" max="{{$envelope.Budget.MaxQueries}}" value="{{$envelope.Budget.MaxQueries}}" required></label><label>最大返回行数<input name="max_result_rows" type="number" min="1" max="{{$envelope.Budget.MaxResultRows}}" value="{{$envelope.Budget.MaxResultRows}}" required></label><label>最大数据库时间 (ms)<input name="max_db_ms" type="number" min="1" max="{{$envelope.Budget.MaxDBMS}}" value="{{$envelope.Budget.MaxDBMS}}" required></label><label>单次超时 (ms)<input name="per_query_timeout_ms" type="number" min="1" max="{{$envelope.Budget.PerQueryTimeoutMS}}" value="{{$envelope.Budget.PerQueryTimeoutMS}}" required></label><label>任务 TTL (ms)<input name="task_ttl_ms" type="number" min="1" max="{{$envelope.Budget.TaskTTLMS}}" value="{{$envelope.Budget.TaskTTLMS}}" required></label></div><p><button type="submit">缩小后批准</button></p></form>{{end}}
<details><summary>技术细节</summary><dl><dt>Task ID</dt><dd>{{$draft.Manifest.TaskID}}</dd>{{if $draft.Manifest.ParentTaskID}}<dt>父任务</dt><dd>{{$draft.Manifest.ParentTaskID}}</dd>{{end}}<dt>人类主体</dt><dd>{{$draft.Manifest.HumanSubject}}</dd>{{if $draft.IssuedBy}}<dt>代开人</dt><dd>{{$draft.IssuedBy}}</dd>{{end}}{{if $draft.MemberConfirm}}<dt>确认成员</dt><dd>{{$draft.Approver}}</dd>{{end}}{{if $draft.AgentSubmitted}}<dt>提交方式</dt><dd>agent 代申请人提交（创建即待审批）</dd>{{end}}<dt>Agent 身份</dt><dd>{{$draft.Manifest.AgentID}}</dd><dt>数据产品</dt><dd>{{range $draft.Manifest.Products}}{{.}} {{end}}</dd><dt>敏感级别上限</dt><dd>{{$draft.Manifest.Sensitivity}}</dd><dt>审批方式</dt><dd>{{$draft.ApprovalMode}}{{if $draft.Approver}} / 当前审批人 {{$draft.Approver}}{{end}}</dd><dt>全部预算（毫秒）</dt><dd>最大查询次数 {{$draft.Manifest.Budget.MaxQueries}}；最大返回行数 {{$draft.Manifest.Budget.MaxResultRows}}；最大数据库时间 {{$draft.Manifest.Budget.MaxDBMS}} ms；单次查询超时 {{$draft.Manifest.Budget.PerQueryTimeoutMS}} ms；任务有效期 TTL {{$draft.Manifest.Budget.TaskTTLMS}} ms</dd><dt>目录版本 / SHA-256</dt><dd>{{$draft.Manifest.CatalogVersion}} / <code>{{$draft.Manifest.CatalogSHA256}}</code></dd><dt>Manifest SHA-256</dt><dd><code>{{$draft.ManifestDigest}}</code></dd>{{if $draft.History}}<dt>审批记录</dt><dd><ol class="history">{{range $draft.History}}<li>{{.At.UTC.Format "2006-01-02 15:04:05"}} UTC · {{.Actor}} {{decisionLabel .Decision}} · receipt {{.ReceiptID}}</li>{{end}}</ol></dd>{{end}}</dl></details></div><p><a href="/tasks">← 返回待办</a></p>{{template "foot" .}}{{end}}
`
