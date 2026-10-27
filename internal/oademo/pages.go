package oademo

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/go-chi/chi/v5"
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
	"rejected": "已拒绝", "not_granted": "未签发"}

func stateLabel(state string) string {
	if label, ok := stateLabels[state]; ok {
		return label
	}
	return state
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
	for _, decision := range draft.History {
		view.Progress = append(view.Progress, progressRow{Who: s.displayName(decision.Actor), Label: s.approverLabel(draft, decision.Actor),
			Status: "done", Decision: decisionLabel(decision.Decision), When: decision.At.UTC().Format("2006-01-02 15:04") + " UTC"})
	}
	if draft.State == "pending" && draft.Approver != "" {
		view.Progress = append(view.Progress, progressRow{Who: s.displayName(draft.Approver), Label: s.approverLabel(draft, draft.Approver), Status: "current"})
	}
	return view
}

// approverLabel says what an approver is to the draft: its requester
// confirming their own query, or an approver.
func (s *Server) approverLabel(draft Draft, approver string) string {
	if strings.EqualFold(approver, draft.Manifest.HumanSubject) {
		return "本人确认"
	}
	return "审批"
}

// relatesAsRequester reports whether username submitted, or issued, the draft.
func (d Draft) relatesAsRequester(username string) bool {
	return strings.EqualFold(d.Manifest.HumanSubject, username) || (d.IssuedBy != "" && strings.EqualFold(d.IssuedBy, username))
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
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

func draftIDParam(r *http.Request) string { return chi.URLParam(r, "draftID") }

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
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Task-bound OA Demo</title><style>
body{font-family:system-ui,sans-serif;background:#f4f6f8;color:#17202a;margin:0}.wrap{max-width:920px;margin:32px auto;padding:0 20px}.card{background:white;border:1px solid #dfe5eb;border-radius:12px;padding:24px;margin:16px 0;box-shadow:0 2px 10px #17202a0d}input,select,textarea,button{font:inherit;padding:8px 12px;border:1px solid #bcc7d1;border-radius:8px}textarea{box-sizing:border-box;width:100%;min-height:70px}button{background:#1769aa;color:white;border:0;cursor:pointer}.danger{background:#a92828}.muted{color:#617080}.tag{display:inline-block;padding:3px 8px;border-radius:99px;background:#e8f1f8;margin-right:6px}nav{display:flex;justify-content:space-between;align-items:center;gap:12px;flex-wrap:wrap}nav a{margin-right:12px}form.inline{display:inline}.narrow{margin-top:20px;padding-top:16px;border-top:1px solid #dfe5eb}.budget-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px}.budget-grid input{width:90%}dt{font-weight:600;margin-top:12px}dd{margin-left:0}.error{color:#a92828}.note{background:#fff7e0;border:1px solid #f0d58a;border-radius:8px;padding:10px 12px}ol.history{padding-left:20px}a{color:#1769aa;text-decoration:none}.summary{font-size:1.1em;line-height:1.6}.progress{list-style:none;padding:0;margin:8px 0}.progress li{padding:6px 0;border-bottom:1px dashed #e3e8ee}.progress .done::before{content:"✓ ";color:#2e7d32}.progress .current::before{content:"▶ ";color:#1769aa}.choices label{display:inline-block;margin:4px 12px 4px 0}details{margin-top:16px}details summary{cursor:pointer;color:#617080}code{word-break:break-all}table.history{width:100%;border-collapse:collapse}table.history td,table.history th{text-align:left;padding:8px 6px;border-bottom:1px solid #e3e8ee;vertical-align:top}
</style></head><body><div class="wrap">{{end}}
{{define "foot"}}</div></body></html>{{end}}
{{define "login"}}{{template "head" .}}<div class="card"><h1>OA Demo 登录</h1>{{if .PasswordOnly}}<p class="muted">输入口令登录。登录后默认是普通员工 {{.Default}} 的身份，页面右上角可随时切换为任一管理员、主任或分管领导。</p>{{else}}<p class="muted">申请人提交自己的申请；审批人只看到指派给自己或自己审批过的工单。</p>{{end}}{{if .Error}}<p class="error">{{if .PasswordOnly}}口令错误{{else}}用户名或密码错误{{end}}</p>{{end}}<form method="post" action="/login"><input type="hidden" name="csrf" value="{{.CSRF}}">{{if not .PasswordOnly}}<p><label>用户<br><select name="username">{{range .Users}}<option value="{{.ID}}">{{.Name}}（{{.ID}}，{{roleLabel .Role}}）</option>{{end}}</select></label></p>{{end}}<p><label>{{if .PasswordOnly}}口令{{else}}密码{{end}}<br><input type="password" name="password" required autocomplete="current-password"></label></p><button type="submit">登录</button></form></div>{{template "foot" .}}{{end}}
{{define "nav"}}<nav><div><strong>Task-bound OA</strong> · {{if ne .DisplayName .Username}}{{.DisplayName}} · {{end}}{{.Username}} / {{roleLabel .Role}}</div><div><a href="/tasks">待办</a><a href="/history">审批记录</a>{{if .AllowSwitch}}<form class="inline" method="post" action="/switch"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>身份 <select name="username">{{$current := .Username}}{{range .Accounts}}<option value="{{.ID}}"{{if eq .ID $current}} selected{{end}}>{{.Name}}（{{roleLabel .Role}}）</option>{{end}}</select></label> <button type="submit">切换身份</button></form> {{end}}<form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">退出</button></form></div></nav>{{end}}
{{define "tasks"}}{{template "head" .}}{{template "nav" .Session}}<h1>待办</h1><p class="muted">待提交或待审批的工单；已办结的在<a href="/history">审批记录</a>。</p>{{range .Views}}<div class="card"><span class="tag">{{stateLabel .Draft.State}}</span><span class="tag">{{.Kind}}</span><h2><a href="/tasks/{{.Draft.ID}}">{{.Draft.Manifest.DeclaredObjective}}</a></h2><p class="summary">{{.Summary}}</p><p class="muted">{{if .Draft.Approver}}当前审批人 {{.Draft.Approver}} · {{end}}Task {{.Draft.Manifest.TaskID}}</p>{{if .Draft.Note}}<p class="note">{{.Draft.Note}}</p>{{end}}</div>{{else}}<div class="card"><p>暂无待办。</p></div>{{end}}{{template "foot" .}}{{end}}
{{define "history"}}{{template "head" .}}{{template "nav" .Session}}<h1>审批记录</h1><p class="muted">你提交、代开或审批过的全部工单，按最近变动排序。</p>{{if .Views}}<div class="card"><table class="history"><tr><th>工单</th><th>状态</th><th>审批记录</th></tr>{{range .Views}}<tr><td><a href="/tasks/{{.Draft.ID}}">{{.Draft.Manifest.DeclaredObjective}}</a><br><span class="muted">{{.Kind}} · 申请人 {{.Draft.Manifest.HumanSubject}}{{if .Draft.IssuedBy}} · 代开人 {{.Draft.IssuedBy}}{{end}}</span></td><td>{{stateLabel .Draft.State}}</td><td>{{range .Progress}}{{if eq .Status "done"}}{{.Who}} {{.Decision}}（{{.When}}）<br>{{else}}等待 {{.Who}} {{.Label}}<br>{{end}}{{end}}{{if .Draft.Note}}<span class="muted">{{.Draft.Note}}</span>{{end}}</td></tr>{{end}}</table></div>{{else}}<div class="card"><p>暂无记录。</p></div>{{end}}{{template "foot" .}}{{end}}
{{define "task"}}{{template "head" .}}{{template "nav" .Session}}{{$view := .View}}{{$draft := .View.Draft}}{{$envelope := .View.Envelope}}<div class="card"><span class="tag">{{stateLabel $draft.State}}</span><span class="tag">{{$view.Kind}}</span><h1>{{$draft.Manifest.DeclaredObjective}}</h1><p class="summary">{{$view.Summary}}</p>
{{if $view.Progress}}<h2>审批进度</h2><ol class="progress">{{range $view.Progress}}<li class="{{.Status}}">{{if eq .Status "done"}}{{.Who}} {{.Decision}} · {{.When}}{{else}}等待 {{.Who}}{{.Label}}{{end}}</li>{{end}}</ol>{{end}}{{if $draft.Note}}<p class="note">{{$draft.Note}}</p>{{end}}
<h2>{{if $envelope.Granted}}上一级批准的范围{{else}}申请的范围{{end}}</h2>{{if $envelope.Granted}}<p class="muted">本级在此基础上批准或缩小，不能扩大。</p>{{end}}<dl>{{range $view.Scopes}}<dt>{{.Label}}</dt><dd>{{if .Enum}}{{range $i, $v := .Values}}{{if $i}}、{{end}}{{$v.Label}}{{end}}{{else}}{{.From}} 至 {{.To}}{{end}}</dd>{{end}}<dt>数据产品与字段</dt><dd>{{range $view.Products}}<strong>{{.Label}}</strong>：{{range $i, $c := .Columns}}{{if $i}}、{{end}}{{$c}}{{end}}<br>{{end}}</dd><dt>预算与有效期</dt><dd>最多 {{$envelope.Budget.MaxQueries}} 次查询，每次最多 {{$envelope.Budget.MaxResultRows}} 行；有效期 {{humanDuration $envelope.Budget.TaskTTLMS}}{{if $envelope.Granted}}（从最后一级批准时起算）{{end}}</dd></dl>
{{if .CanSubmit}}<form method="post" action="/tasks/{{$draft.ID}}/submit"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button type="submit">提交申请</button></form>{{end}}
{{if .CanDecide}}<form class="inline" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="approved"><button type="submit">{{if .SelfConfirm}}确认{{else}}按原申请批准{{end}}</button></form> <form class="inline" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="rejected"><button class="danger" type="submit">拒绝</button></form>
<form class="narrow" method="post" action="/tasks/{{$draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="narrow"><h2>缩小后批准</h2><p class="muted">去掉不该给的项即可，只能缩小，不能扩大。</p>{{range $view.Scopes}}<p><strong>{{.Label}}</strong><br><span class="choices">{{if .Enum}}{{$scope := .Name}}{{range .Values}}<label><input type="checkbox" name="scope:{{$scope}}" value="{{.Value}}" checked> {{.Label}}</label>{{end}}{{else}}<label>从 <input type="date" name="scope_from:{{.Name}}" value="{{.From}}" min="{{.From}}" max="{{.To}}"></label> <label>至 <input type="date" name="scope_to:{{.Name}}" value="{{.To}}" min="{{.From}}" max="{{.To}}"></label>{{end}}</span></p>{{end}}<p><strong>数据产品与字段</strong></p>{{range $view.Products}}<p><label><input type="checkbox" name="approved_products" value="{{.Name}}" checked> <strong>{{.Label}}</strong></label><br><span class="choices">{{$product := .Name}}{{range .Columns}}<label><input type="checkbox" name="column:{{$product}}" value="{{.}}" checked> {{.}}</label>{{end}}</span></p>{{end}}<div class="budget-grid"><label>最大查询次数<input name="max_queries" type="number" min="1" max="{{$envelope.Budget.MaxQueries}}" value="{{$envelope.Budget.MaxQueries}}" required></label><label>最大返回行数<input name="max_result_rows" type="number" min="1" max="{{$envelope.Budget.MaxResultRows}}" value="{{$envelope.Budget.MaxResultRows}}" required></label><label>最大数据库时间 (ms)<input name="max_db_ms" type="number" min="1" max="{{$envelope.Budget.MaxDBMS}}" value="{{$envelope.Budget.MaxDBMS}}" required></label><label>单次超时 (ms)<input name="per_query_timeout_ms" type="number" min="1" max="{{$envelope.Budget.PerQueryTimeoutMS}}" value="{{$envelope.Budget.PerQueryTimeoutMS}}" required></label><label>任务 TTL (ms)<input name="task_ttl_ms" type="number" min="1" max="{{$envelope.Budget.TaskTTLMS}}" value="{{$envelope.Budget.TaskTTLMS}}" required></label></div><p><button type="submit">缩小后批准</button></p></form>{{end}}
<details><summary>技术细节</summary><dl><dt>Task ID</dt><dd>{{$draft.Manifest.TaskID}}</dd>{{if $draft.Manifest.ParentTaskID}}<dt>父任务</dt><dd>{{$draft.Manifest.ParentTaskID}}</dd>{{end}}<dt>人类主体</dt><dd>{{$draft.Manifest.HumanSubject}}</dd>{{if $draft.IssuedBy}}<dt>代开人</dt><dd>{{$draft.IssuedBy}}</dd>{{end}}<dt>Agent 身份</dt><dd>{{$draft.Manifest.AgentID}}</dd><dt>数据产品</dt><dd>{{range $draft.Manifest.Products}}{{.}} {{end}}</dd><dt>敏感级别上限</dt><dd>{{$draft.Manifest.Sensitivity}}</dd><dt>审批方式</dt><dd>{{$draft.ApprovalMode}}{{if $draft.Approver}} / 当前审批人 {{$draft.Approver}}{{end}}</dd><dt>全部预算（毫秒）</dt><dd>最大查询次数 {{$draft.Manifest.Budget.MaxQueries}}；最大返回行数 {{$draft.Manifest.Budget.MaxResultRows}}；最大数据库时间 {{$draft.Manifest.Budget.MaxDBMS}} ms；单次查询超时 {{$draft.Manifest.Budget.PerQueryTimeoutMS}} ms；任务有效期 TTL {{$draft.Manifest.Budget.TaskTTLMS}} ms</dd><dt>目录版本 / SHA-256</dt><dd>{{$draft.Manifest.CatalogVersion}} / <code>{{$draft.Manifest.CatalogSHA256}}</code></dd><dt>Manifest SHA-256</dt><dd><code>{{$draft.ManifestDigest}}</code></dd>{{if $draft.History}}<dt>审批记录</dt><dd><ol class="history">{{range $draft.History}}<li>{{.At.UTC.Format "2006-01-02 15:04:05"}} UTC · {{.Actor}} {{decisionLabel .Decision}} · receipt {{.ReceiptID}}</li>{{end}}</ol></dd>{{end}}</dl></details></div><p><a href="/tasks">← 返回待办</a></p>{{template "foot" .}}{{end}}
`
