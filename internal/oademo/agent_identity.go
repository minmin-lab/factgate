package oademo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The agent's identity (KB1-LOOP-151): the tester runs one agent with one MCP
// token per label, and the person at the OA chooses which principal each
// label acts as. TaskGate keeps the binding; the OA lists the labels and the
// candidate principals and asks for a switch, signed like its callbacks
// (POST Config.AgentIdentityURL, action "list" or "switch"). The control is a
// second line of the navigation, independent of the OA account switcher, and
// is offered only in the shared-password mode with the URL configured.

// agentLabelCookie remembers which label the navigation shows. It is a
// display preference only: a switch names its label in the form, and a label
// TaskGate no longer lists falls back to the first.
const agentLabelCookie = "oa_agent_label"

func (s *Server) agentIdentityEnabled() bool {
	return s.config.AllowSwitch && s.config.AgentIdentityURL != ""
}

// agentIdentityRequest is the body of a call to the agent-identity endpoint.
type agentIdentityRequest struct {
	EventID     string `json:"event_id"`
	Action      string `json:"action"`
	Label       string `json:"label,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	Actor       string `json:"actor,omitempty"`
}

// agentIdentity is one label's current binding.
type agentIdentity struct {
	Label       string `json:"label"`
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
	BoundBy     string `json:"bound_by"`
	BoundAt     string `json:"bound_at"` // RFC 3339
}

// agentCandidate is one principal a label may be bound to.
type agentCandidate struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
	Department  string `json:"department"`
	JobFamily   string `json:"job_family"`
	Center      string `json:"center"`
}

// agentIdentityList is TaskGate's answer to "list".
type agentIdentityList struct {
	Identities []agentIdentity  `json:"identities"`
	Candidates []agentCandidate `json:"candidates"`
}

func (l agentIdentityList) hasLabel(label string) bool {
	for _, identity := range l.Identities {
		if identity.Label == label {
			return true
		}
	}
	return false
}

// agentGatewayError is a failed call to the agent-identity endpoint, said in
// a form a page may show: never the endpoint's address or the signature.
type agentGatewayError struct{ summary string }

func (e *agentGatewayError) Error() string { return e.summary }

// agentFailure is what a page says about a failed call.
func agentFailure(err error) string {
	var gatewayErr *agentGatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr.summary
	}
	return "内部错误"
}

// callAgentIdentity posts a signed request to the agent-identity endpoint and
// decodes a 200 answer into answer (nil skips decoding). The full error goes
// to the log; the returned error is the summary a page may show.
func (s *Server) callAgentIdentity(ctx context.Context, request agentIdentityRequest, answer any) error {
	request.EventID = randomID("evt")
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	status, response, err := s.signedPost(ctx, s.config.AgentIdentityURL, request.EventID, body)
	if err != nil {
		s.config.Logger.Warn("agent identity request failed", "action", request.Action, "event_id", request.EventID, "error", err)
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return &agentGatewayError{"连接超时"}
		}
		return &agentGatewayError{"无法连接"}
	}
	if status != http.StatusOK {
		s.config.Logger.Warn("agent identity request refused", "action", request.Action, "event_id", request.EventID,
			"status", status, "body", excerpt(string(response)))
		return &agentGatewayError{agentRefusal(status, response)}
	}
	if answer != nil {
		if err := json.Unmarshal(response, answer); err != nil {
			s.config.Logger.Warn("agent identity answer unreadable", "action", request.Action, "event_id", request.EventID, "error", err)
			return &agentGatewayError{"网关响应无法解析"}
		}
	}
	return nil
}

// agentRefusal summarizes a non-200 answer: the gateway's error message when
// it gave one (at most 512 bytes), otherwise the status.
func agentRefusal(status int, body []byte) string {
	if status == http.StatusNotFound {
		return "网关未开启 agent 身份切换（HTTP 404）"
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &refusal) == nil && strings.TrimSpace(refusal.Error) != "" {
		return excerpt(fmt.Sprintf("HTTP %d：%s", status, strings.TrimSpace(refusal.Error)))
	}
	if text := excerpt(string(body)); text != "" {
		return excerpt(fmt.Sprintf("HTTP %d：%s", status, text))
	}
	return fmt.Sprintf("HTTP %d", status)
}

func (s *Server) listAgentIdentities(ctx context.Context) (agentIdentityList, error) {
	var list agentIdentityList
	err := s.callAgentIdentity(ctx, agentIdentityRequest{Action: "list"}, &list)
	return list, err
}

// agentIdentitySwitch answers the navigation's agent form. action=select
// shows another label (it sets the label cookie after checking TaskGate
// lists the label; nothing is switched). Anything else asks TaskGate to bind
// the label to principal_id in the name of the session's account. Both land
// on next when switchNext allows it, otherwise on the to-do list.
func (s *Server) agentIdentitySwitch(w http.ResponseWriter, r *http.Request) {
	if !s.agentIdentityEnabled() {
		http.NotFound(w, r)
		return
	}
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	label := r.Form.Get("label")
	if strings.TrimSpace(label) == "" {
		http.Error(w, "label is required", http.StatusBadRequest)
		return
	}
	next := "/tasks"
	if candidate := r.Form.Get("next"); switchNext.MatchString(candidate) {
		next = candidate
	}
	if r.Form.Get("action") == "select" {
		list, err := s.listAgentIdentities(r.Context())
		if err != nil {
			http.Error(w, excerpt("agent 身份列表不可用："+agentFailure(err)), http.StatusBadGateway)
			return
		}
		if !list.hasLabel(label) {
			http.Error(w, "unknown agent label", http.StatusBadRequest)
			return
		}
	} else {
		principal := r.Form.Get("principal_id")
		if strings.TrimSpace(principal) == "" {
			http.Error(w, "principal_id is required", http.StatusBadRequest)
			return
		}
		request := agentIdentityRequest{Action: "switch", Label: label, PrincipalID: principal, Actor: data.Username}
		if err := s.callAgentIdentity(r.Context(), request, nil); err != nil {
			http.Error(w, excerpt("agent 身份切换失败："+agentFailure(err)), http.StatusBadGateway)
			return
		}
	}
	s.setAgentLabel(w, r, label)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// setAgentLabel remembers the label for eight hours, as long as a session.
// The value is base64url, since a label may hold bytes a cookie cannot.
func (s *Server) setAgentLabel(w http.ResponseWriter, r *http.Request, label string) {
	lifetime := 8 * time.Hour
	http.SetCookie(w, &http.Cookie{Name: agentLabelCookie, Value: base64.RawURLEncoding.EncodeToString([]byte(label)), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, Expires: s.config.Clock().Add(lifetime),
		MaxAge: int(lifetime.Seconds())})
}

func agentLabelFromCookie(r *http.Request) string {
	cookie, err := r.Cookie(agentLabelCookie)
	if err != nil {
		return ""
	}
	label, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	return string(label)
}

// agentNav is the navigation's agent line. Unavailable says why TaskGate's
// list could not be read; no Labels means TaskGate lists no agent.
type agentNav struct {
	Unavailable string
	Next        string
	Labels      []agentLabelOption
	Groups      []agentPrincipalGroup
	Current     string
}

type agentLabelOption struct {
	Label    string
	Selected bool
}

type agentPrincipalGroup struct {
	Label   string
	Options []agentPrincipalOption
}

type agentPrincipalOption struct {
	PrincipalID, Text string
	Selected          bool
}

// agentNav reads TaskGate's list for the navigation (once per page; the demo
// is small enough not to cache it). It is nil when the control is off.
func (s *Server) agentNav(r *http.Request) *agentNav {
	if !s.agentIdentityEnabled() {
		return nil
	}
	nav := &agentNav{Next: r.URL.Path}
	list, err := s.listAgentIdentities(r.Context())
	if err != nil {
		nav.Unavailable = agentFailure(err)
		return nav
	}
	if len(list.Identities) == 0 {
		return nav
	}
	current := list.Identities[0]
	if chosen := agentLabelFromCookie(r); chosen != "" {
		for _, identity := range list.Identities {
			if identity.Label == chosen {
				current = identity
				break
			}
		}
	}
	for _, identity := range list.Identities {
		nav.Labels = append(nav.Labels, agentLabelOption{Label: identity.Label, Selected: identity.Label == current.Label})
	}
	nav.Groups = s.agentPrincipalGroups(list.Candidates, current.PrincipalID)
	nav.Current = s.agentCurrent(current, list.Candidates)
	return nav
}

// jobFamilyLabels names the job families of the demo's organization; others
// show as they are.
var jobFamilyLabels = map[string]string{"staff": "员工", "center_head": "中心主任", "dept_admin": "部门管理员",
	"director": "部门主任", "vice_president": "副总经理", "general_manager": "总经理"}

// jobFamilyOrder ranks a department's members; other job families come last.
var jobFamilyOrder = map[string]int{"staff": 0, "center_head": 1, "dept_admin": 2, "director": 3}

func jobFamilyRank(jobFamily string) int {
	if rank, ok := jobFamilyOrder[jobFamily]; ok {
		return rank
	}
	return len(jobFamilyOrder)
}

// leadershipGroup collects the company's leaders ahead of the departments.
const leadershipGroup = "公司领导"

func isLeadership(jobFamily string) bool {
	return jobFamily == "vice_president" || jobFamily == "general_manager"
}

// agentPrincipalGroups arranges the candidates by department: the company's
// leaders first, then each department in the order of its code, labelled by
// Labels.Values["kb_department"]; candidates without a department last.
// Within a group: staff, center heads, department administrators, directors,
// then the rest, each by subject.
func (s *Server) agentPrincipalGroups(candidates []agentCandidate, selected string) []agentPrincipalGroup {
	var leaders, loose []agentCandidate
	departments := map[string][]agentCandidate{}
	for _, candidate := range candidates {
		switch {
		case isLeadership(candidate.JobFamily):
			leaders = append(leaders, candidate)
		case candidate.Department == "":
			loose = append(loose, candidate)
		default:
			departments[candidate.Department] = append(departments[candidate.Department], candidate)
		}
	}
	codes := make([]string, 0, len(departments))
	for code := range departments {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var groups []agentPrincipalGroup
	add := func(label string, members []agentCandidate) {
		if len(members) == 0 {
			return
		}
		sort.SliceStable(members, func(i, j int) bool {
			left, right := members[i], members[j]
			if a, b := jobFamilyRank(left.JobFamily), jobFamilyRank(right.JobFamily); a != b {
				return a < b
			}
			if left.JobFamily != right.JobFamily {
				return left.JobFamily < right.JobFamily
			}
			if left.Subject != right.Subject {
				return left.Subject < right.Subject
			}
			return left.PrincipalID < right.PrincipalID
		})
		group := agentPrincipalGroup{Label: label}
		for _, member := range members {
			group.Options = append(group.Options, agentPrincipalOption{PrincipalID: member.PrincipalID,
				Text: s.agentPrincipalText(member), Selected: member.PrincipalID == selected})
		}
		groups = append(groups, group)
	}
	add(leadershipGroup, leaders)
	for _, code := range codes {
		add(s.config.Labels.value("kb_department", code), departments[code])
	}
	add("未分部门", loose)
	return groups
}

// agentPrincipalText names a principal: the OA account's display name when
// the subject is one (displayName adds the login name), otherwise the
// subject; then the job family, and a center head's center.
func (s *Server) agentPrincipalText(candidate agentCandidate) string {
	text := candidate.Subject
	if account := strings.ToLower(candidate.Subject); account != "" {
		if _, ok := s.users[account]; ok {
			text = s.displayName(account)
		}
	}
	if text == "" {
		text = candidate.PrincipalID
	}
	job := jobFamilyLabels[candidate.JobFamily]
	if job == "" {
		job = candidate.JobFamily
	}
	if job != "" {
		text += " · " + job
	}
	if candidate.JobFamily == "center_head" && candidate.Center != "" {
		text += "（" + s.config.Labels.value("center", candidate.Center) + "）"
	}
	return text
}

// agentCurrent says what the shown label is bound to, when, and by whom.
func (s *Server) agentCurrent(identity agentIdentity, candidates []agentCandidate) string {
	if identity.PrincipalID == "" && identity.Subject == "" {
		return "未绑定"
	}
	principal := agentCandidate{PrincipalID: identity.PrincipalID, Subject: identity.Subject}
	for _, candidate := range candidates {
		if candidate.PrincipalID == identity.PrincipalID {
			principal = candidate
			break
		}
	}
	text := s.agentPrincipalText(principal)
	when := strings.TrimSpace(identity.BoundAt)
	if at, err := time.Parse(time.RFC3339, when); err == nil {
		when = pageTime(at)
	}
	switch by := strings.ToLower(strings.TrimSpace(identity.BoundBy)); {
	case by != "" && when != "":
		text += "，" + when + " 由 " + s.displayName(by) + " 切换"
	case by != "":
		text += "，由 " + s.displayName(by) + " 切换"
	case when != "":
		text += "，" + when + " 绑定"
	}
	return text
}
