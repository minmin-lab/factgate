package oademo

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/domain"
)

func clonePredicateFootprint(input *domain.PredicateFootprintLimitsV1) *domain.PredicateFootprintLimitsV1 {
	if input == nil {
		return nil
	}
	copy := *input
	return &copy
}

type Config struct {
	ServiceToken   string
	CallbackSecret string
	SessionSecret  string
	CallbackURL    string
	PublicBaseURL  string
	AlicePassword  string
	BobPassword    string
	// DanaPassword, when set, adds dana as a second approver (for example the
	// approver a Catalog routes leadership-classified requests to). Each
	// approver sees and decides only the drafts that name them.
	DanaPassword string
	// ExtraUsers adds further demo accounts (for example a colleague's own
	// requester account, or the directors and leaders of an approval chain);
	// names must not collide with the built-in users.
	ExtraUsers []ExtraUser
	// OmitBuiltinUsers leaves out alice, bob and dana, so the OA offers only
	// ExtraUsers (the demo stack's role accounts, KB1-LOOP-144); they must then
	// include a requester and an approver. The formal round keeps the built-in
	// users.
	OmitBuiltinUsers bool
	// AllowSwitch offers a logged-in user a switch to any other configured
	// account without a password (the shared-password demo mode only).
	AllowSwitch bool
	// DefaultAccount is the identity a password-only login starts as; empty
	// means the first requester in switcher order.
	DefaultAccount string
	// AgentIdentityURL is TaskGate's agent-identity endpoint (KB1-LOOP-151,
	// agent_identity.go). With AllowSwitch the navigation gains a second,
	// independent card that binds the testers' shared agent token, by label,
	// to a principal. Empty leaves the card out.
	AgentIdentityURL string
	// AgentIdentityLabel is the label of that shared token. Empty means the
	// first, in label order, that TaskGate lists; a label TaskGate does not
	// list is said so on the page, with no control. It does nothing without
	// the URL.
	AgentIdentityLabel string
	// Labels are display names for Catalog identifiers (pages.go).
	Labels        Labels
	HTTPClient    *http.Client
	Logger        *slog.Logger
	Clock         func() time.Time
	ReceiptSigner approval.ReceiptSigner
}

// ExtraUser is one configured demo account.
type ExtraUser struct {
	Name     string
	Role     string // requester, approver, or both
	Password string
	// DisplayName, when set, is shown instead of the name (for example
	// "财务部主任"), so a tester sees who holds which authority.
	DisplayName string
}

type Server struct {
	config    Config
	users     map[string]user
	mu        sync.RWMutex
	drafts    map[string]*Draft
	templates *template.Template
}

type user struct {
	Name         string // display name
	Role         string // requester, approver, or both
	CanRequest   bool
	CanApprove   bool
	PasswordHash []byte
}

func capabilities(role string) (canRequest, canApprove, ok bool) {
	switch role {
	case "requester":
		return true, false, true
	case "approver":
		return false, true, true
	case "both":
		return true, true, true
	}
	return false, false, false
}

type DraftRequest = approval.DraftRequest

type Draft struct {
	ID             string                           `json:"id"`
	Manifest       approval.AuthorizationManifestV1 `json:"authorization_manifest"`
	ManifestDigest string                           `json:"manifest_digest"`
	ApprovalMode   string                           `json:"approval_mode"`
	Approver       string                           `json:"approver,omitempty"`
	// IssuedBy is the administrator who issued the task for its human
	// subject (KB1-LOOP-147); they may submit the draft in their place.
	IssuedBy string `json:"issued_by,omitempty"`
	// MemberConfirm marks a child task delegated to a project member
	// (KB1-LOOP-148): Approver is that member, who submits and confirms it;
	// the manifest's human subject stays the project's owner.
	MemberConfirm bool `json:"member_confirm,omitempty"`
	// ParentExpiresAt bounds a delegated task's grant: a decision later than
	// the request shortens the TTL to what the parent has left.
	ParentExpiresAt *time.Time `json:"parent_expires_at,omitempty"`
	// AgentSubmitted marks a draft the agent submitted for its human subject
	// (KB1-LOOP-149; DraftRequest.Submitted): it is created pending, offers
	// no submission, and no submitted callback is sent for it.
	AgentSubmitted  bool                        `json:"agent_submitted,omitempty"`
	ApprovedGrant   *approval.TaskGrantCoreV1   `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt,omitempty"`
	State           string                      `json:"state"`
	// History lists the decisions on the draft, oldest first. In an approval
	// chain (KB1-LOOP-141) TaskGate answers each approval with the next
	// approver; the draft is then reassigned to them and Approver is always
	// the one who decides now.
	History []DraftDecision `json:"history,omitempty"`
	// Note is what TaskGate answered the last callback with when it did not
	// simply take it: the next approver, why no grant was issued, or why the
	// callback was refused.
	Note string `json:"note,omitempty"`
	// Withdrawal is TaskGate's notice that it archived the task while the
	// draft still waited (KB1-LOOP-137): revoked, or expired while pending.
	// The draft is then "withdrawn" and nobody may submit or decide it.
	Withdrawal *approval.DraftWithdrawal `json:"withdrawal,omitempty"`
	CreatedAt  time.Time                 `json:"created_at"`
	UpdatedAt  time.Time                 `json:"updated_at"`
}

// DraftDecision is one approver's decision on a draft.
type DraftDecision struct {
	Actor     string    `json:"actor"`
	Decision  string    `json:"decision"`
	At        time.Time `json:"at"`
	ReceiptID string    `json:"receipt_id"`
}

// decidedBy reports whether username has decided the draft before.
func (d Draft) decidedBy(username string) bool {
	for _, decision := range d.History {
		if decision.Actor == username {
			return true
		}
	}
	return false
}

// draftEnvelope is what the current approver decides on: the request as made,
// or in an approval chain what the previous approver granted.
type draftEnvelope struct {
	Products []string
	Columns  map[string][]string
	Scope    map[string]any
	Budget   approval.AuthorizationBudgetV1
	// Granted is true when a previous approver's grant is the base.
	Granted bool
}

// Envelope is the draft's current envelope (the task page shows it and
// prefills the narrow form with it).
func (d Draft) Envelope() draftEnvelope {
	if grant := d.ApprovedGrant; grant != nil {
		return draftEnvelope{Products: grant.ApprovedProducts, Columns: grant.ApprovedColumns, Scope: grant.MandatoryScope,
			Budget: grant.Budget, Granted: true}
	}
	return draftEnvelope{Products: d.Manifest.Products, Columns: d.Manifest.ApprovedColumns, Scope: d.Manifest.MandatoryScope,
		Budget: d.Manifest.Budget}
}

type callbackEvent struct {
	EventID         string                      `json:"event_id"`
	TaskID          string                      `json:"task_id"`
	DraftID         string                      `json:"draft_id"`
	Status          string                      `json:"status"`
	Actor           string                      `json:"actor"`
	OccurredAt      time.Time                   `json:"occurred_at"`
	CatalogVersion  string                      `json:"catalog_version"`
	CallbackContext string                      `json:"callback_context,omitempty"`
	ManifestDigest  string                      `json:"manifest_digest"`
	ApprovedGrant   *approval.TaskGrantCoreV1   `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt,omitempty"`
}

func New(config Config) (*Server, error) {
	if config.ServiceToken == "" || config.CallbackSecret == "" || config.SessionSecret == "" || config.CallbackURL == "" {
		return nil, errors.New("OA service token, callback secret, session secret, and callback URL are required")
	}
	if !config.OmitBuiltinUsers && (config.AlicePassword == "" || config.BobPassword == "") {
		return nil, errors.New("OA demo passwords are required")
	}
	if config.PublicBaseURL == "" {
		config.PublicBaseURL = "http://127.0.0.1:8092"
	}
	if _, err := url.ParseRequestURI(config.PublicBaseURL); err != nil {
		return nil, fmt.Errorf("invalid public base URL: %w", err)
	}
	if config.AgentIdentityURL != "" {
		if _, err := url.ParseRequestURI(config.AgentIdentityURL); err != nil {
			return nil, fmt.Errorf("invalid agent identity URL: %w", err)
		}
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.ReceiptSigner == nil {
		config.ReceiptSigner = approval.DemoReceiptSigner([]byte(config.CallbackSecret))
	}
	users := map[string]user{}
	if !config.OmitBuiltinUsers {
		builtin := []struct{ id, name, role, password string }{
			{"alice", "Alice", "requester", config.AlicePassword},
			{"bob", "Bob", "approver", config.BobPassword},
			{"dana", "Dana", "approver", config.DanaPassword},
		}
		for _, account := range builtin {
			if account.password == "" {
				continue // dana is optional
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(account.password), bcrypt.DefaultCost)
			if err != nil {
				return nil, err
			}
			canRequest, canApprove, _ := capabilities(account.role)
			users[account.id] = user{Name: account.name, Role: account.role, CanRequest: canRequest, CanApprove: canApprove, PasswordHash: hash}
		}
	}
	for _, extra := range config.ExtraUsers {
		name := strings.ToLower(strings.TrimSpace(extra.Name))
		canRequest, canApprove, validRole := capabilities(extra.Role)
		if _, exists := users[name]; exists || name == "" || extra.Password == "" || !validRole {
			return nil, fmt.Errorf("invalid or duplicate extra OA user %q", extra.Name)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(extra.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		display := strings.TrimSpace(extra.DisplayName)
		if display == "" {
			display = extra.Name
		}
		users[name] = user{Name: display, Role: extra.Role, CanRequest: canRequest, CanApprove: canApprove, PasswordHash: hash}
	}
	anyRequester, anyApprover := false, false
	for _, account := range users {
		anyRequester, anyApprover = anyRequester || account.CanRequest, anyApprover || account.CanApprove
	}
	if !anyRequester || !anyApprover {
		return nil, errors.New("the OA demo needs at least one requester and one approver")
	}
	config.ExtraUsers = nil
	config.AlicePassword = ""
	config.BobPassword = ""
	config.DanaPassword = ""
	server := &Server{config: config, users: users, drafts: make(map[string]*Draft)}
	parsedTemplates, err := template.New("layout").Funcs(template.FuncMap{"json": templateJSON, "decisionLabel": decisionLabel,
		"roleLabel": roleLabel, "stateLabel": stateLabel, "draftStateLabel": draftStateLabel, "humanDuration": humanDuration}).Parse(pageTemplates)
	if err != nil {
		return nil, err
	}
	server.templates = parsedTemplates
	return server, nil
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Get("/health/live", emptyHealth)
	router.Get("/health/ready", emptyHealth)
	router.Get("/login", s.loginPage)
	router.Post("/login", s.login)
	router.With(s.requireSession, s.requireCSRF).Post("/logout", s.logout)
	router.With(s.requireSession, s.requireCSRF).Post("/switch", s.switchAccount)
	router.Get("/switch.js", switchScript)
	router.Get("/poll.js", pollScript)
	router.With(s.requireSession, s.requireCSRF).Post("/agent-identity", s.agentIdentitySwitch)
	router.With(s.requireSession).Get("/history", s.history)
	router.Post("/api/drafts", s.createDraft)
	router.Post("/api/drafts/{draftID}/withdraw", s.withdrawDraft)
	router.Group(func(protected chi.Router) {
		protected.Use(s.requireSession)
		protected.Get("/", s.listDrafts)
		protected.Get("/tasks", s.listDrafts)
		protected.Get("/tasks/{draftID}", s.viewDraft)
		protected.Get("/tasks/{draftID}/state", s.draftState)
		protected.With(s.requireCSRF).Post("/tasks/{draftID}/submit", s.submitDraft)
		protected.With(s.requireCSRF).Post("/tasks/{draftID}/decision", s.decideDraft)
	})
	return securityHeaders(router)
}

func emptyHealth(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func (s *Server) createDraft(w http.ResponseWriter, r *http.Request) {
	if !constantToken(r.Header.Get("Authorization"), "Bearer "+s.config.ServiceToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var request DraftRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid draft request"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid draft request"})
		return
	}
	if err := s.validateDraftRequest(request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	now := s.config.Clock().UTC()
	draft := &Draft{
		ID: randomID("oa"), Manifest: cloneManifest(request.Manifest), ManifestDigest: request.ManifestDigest,
		ApprovalMode: request.ApprovalMode, Approver: strings.ToLower(request.Approver), IssuedBy: strings.ToLower(request.IssuedBy),
		MemberConfirm:   request.MemberConfirm,
		ParentExpiresAt: cloneTime(request.ParentExpiresAt),
		State:           "draft", CreatedAt: now, UpdatedAt: now,
	}
	if request.Submitted {
		// TaskGate already holds the task as submitted: the draft waits for its
		// approver from the start, and nobody submits it again.
		draft.State, draft.AgentSubmitted = "pending", true
		draft.Note = s.agentSubmittedNote(draft)
	}
	s.mu.Lock()
	s.drafts[draft.ID] = draft
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"draft_id": draft.ID,
		"state":    draft.State,
		"url":      approval.DraftPageURL(s.config.PublicBaseURL, draft.ID),
	})
}

// withdrawDraft takes TaskGate's notice that a task was archived while its
// draft still waited (KB1-LOOP-137): an unsubmitted or pending draft becomes
// "withdrawn", leaves the to-do lists and offers no action; the history page
// shows it revoked or expired. It is authenticated like createDraft. Repeating
// it is idempotent, and a draft already decided (approved, narrowed, rejected,
// not granted) is left as it is and answered unchanged.
func (s *Server) withdrawDraft(w http.ResponseWriter, r *http.Request) {
	if !constantToken(r.Header.Get("Authorization"), "Bearer "+s.config.ServiceToken) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	var notice approval.DraftWithdrawal
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&notice); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid withdrawal"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid withdrawal"})
		return
	}
	draftID := chi.URLParam(r, "draftID")
	if notice.DraftID != draftID || notice.TaskID == "" || !approval.ValidWithdrawalReason(notice.Reason) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid withdrawal"})
		return
	}
	now := s.config.Clock().UTC()
	if notice.OccurredAt.IsZero() {
		notice.OccurredAt = now
	}
	if notice.Actor == "" {
		notice.Actor = "system"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown draft"})
		return
	}
	if draft.Manifest.TaskID != notice.TaskID {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft belongs to another task"})
		return
	}
	if !draft.open() {
		// Withdrawn already, or decided: nothing changes.
		writeJSON(w, http.StatusOK, map[string]any{"draft_id": draft.ID, "state": draft.State, "unchanged": true})
		return
	}
	withdrawal := notice
	draft.State, draft.Withdrawal, draft.UpdatedAt = "withdrawn", &withdrawal, now
	draft.Note = s.withdrawalNote(withdrawal)
	writeJSON(w, http.StatusOK, map[string]any{"draft_id": draft.ID, "state": draft.State, "unchanged": false})
}

// withdrawalNote is what a withdrawn draft says instead of its actions.
func (s *Server) withdrawalNote(withdrawal approval.DraftWithdrawal) string {
	at := pageTime(withdrawal.OccurredAt)
	if withdrawal.Reason == approval.WithdrawalExpired {
		return "TaskGate 中此任务等待提交或审批超过时限，已于 " + at + " 过期作废；工单已撤回，无需再提交或审批。需要时请重新申请"
	}
	return "TaskGate 中此任务已由 " + s.displayName(withdrawal.Actor) + " 于 " + at + " 撤销；工单已撤回，无需再提交或审批"
}

// agentSubmittedNote is what an agent-submitted draft says until its first
// decision: who it was submitted for and who acts on it now.
func (s *Server) agentSubmittedNote(draft *Draft) string {
	note := "agent 已代 " + s.displayName(draft.Manifest.HumanSubject) + " 提交，"
	switch {
	case strings.EqualFold(draft.Approver, draft.Manifest.HumanSubject):
		return note + "等待本人确认"
	case draft.MemberConfirm:
		return note + "等待项目成员 " + s.displayName(draft.Approver) + " 本人确认"
	}
	return note + "等待 " + s.displayName(draft.Approver) + " 审批"
}

func (s *Server) validateDraftRequest(request DraftRequest) error {
	if err := approval.ValidateAuthorizationSnapshot(request); err != nil {
		return fmt.Errorf("invalid authorization manifest: %w", err)
	}
	switch request.ApprovalMode {
	case "manual":
		if approver, ok := s.users[strings.ToLower(request.Approver)]; !ok || !approver.CanApprove {
			return errors.New("manual demo approval requires a configured approver")
		}
	default:
		return errors.New("approval_mode must be manual; automatic task approval is disabled")
	}
	return nil
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}
	csrf := randomID("login")
	http.SetCookie(w, &http.Cookie{Name: "oa_login_csrf", Value: csrf, Path: "/login", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, MaxAge: 600})
	s.render(w, "login", map[string]any{"CSRF": csrf, "Error": r.URL.Query().Get("error"), "Users": s.loginOptions(),
		"PasswordOnly": s.config.AllowSwitch, "Default": s.defaultAccount()})
}

// defaultAccount is the identity a password-only login lands on in the
// shared-password mode: the first requester (the tester's own account).
func (s *Server) defaultAccount() string {
	if name := strings.ToLower(strings.TrimSpace(s.config.DefaultAccount)); name != "" {
		if _, ok := s.users[name]; ok {
			return name
		}
	}
	options := s.loginOptions()
	if len(options) == 0 {
		return ""
	}
	return options[0].ID
}

// passwordOnlyLogin resolves the account of a login that named none (the
// shared-password mode): the default account, or any account the password
// opens, so a mode with distinct passwords still logs the right person in.
func (s *Server) passwordOnlyLogin(password string) (string, bool) {
	if name := s.defaultAccount(); name != "" && bcrypt.CompareHashAndPassword(s.users[name].PasswordHash, []byte(password)) == nil {
		return name, true
	}
	for _, option := range s.loginOptions() {
		if bcrypt.CompareHashAndPassword(s.users[option.ID].PasswordHash, []byte(password)) == nil {
			return option.ID, true
		}
	}
	return "", false
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !validCookieForm(r, "oa_login_csrf", r.Form.Get("csrf")) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.Form.Get("username")))
	if username == "" && s.config.AllowSwitch {
		// The shared-password mode: one password, the identity is chosen after.
		name, valid := s.passwordOnlyLogin(r.Form.Get("password"))
		if !valid {
			http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
			return
		}
		username = name
	}
	account, ok := s.users[username]
	passwordValid := false
	if ok {
		passwordValid = bcrypt.CompareHashAndPassword(account.PasswordHash, []byte(r.Form.Get("password"))) == nil
	} else {
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$w4vB.oMUSv1xCuYibXVPqO6CkXiLgO22tcP9w2Z.2bKtMCZaITkBy"), []byte(r.Form.Get("password")))
	}
	if !ok || !passwordValid {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}
	expires := s.config.Clock().Add(8 * time.Hour)
	value := s.signSession(username, expires)
	http.SetCookie(w, &http.Cookie{Name: "oa_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, Expires: expires, MaxAge: int(expires.Sub(s.config.Clock()).Seconds())})
	http.SetCookie(w, &http.Cookie{Name: "oa_login_csrf", Value: "", Path: "/login", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

type sessionContextKey struct{}

type sessionData struct {
	Username    string
	DisplayName string
	Role        string
	CanRequest  bool
	CanApprove  bool
	CSRF        string
	// AllowSwitch and AccountGroups feed the account switcher in the navigation.
	AllowSwitch   bool
	AccountGroups []accountGroupView
	// AgentIdentity is the navigation's agent line (agent_identity.go); the
	// pages that show the navigation set it, and it stays nil when off.
	AgentIdentity *agentNav
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		account := s.users[username]
		data := sessionData{Username: username, DisplayName: account.Name, Role: account.Role,
			CanRequest: account.CanRequest, CanApprove: account.CanApprove, CSRF: s.csrf(r), AllowSwitch: s.config.AllowSwitch}
		if s.config.AllowSwitch {
			data.AccountGroups = s.accountGroups()
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, data)))
	})
}

func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		data, _ := r.Context().Value(sessionContextKey{}).(sessionData)
		if !constantToken(r.Form.Get("csrf"), data.CSRF) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "oa_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) listDrafts(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	data.AgentIdentity = s.agentNav(r)
	s.mu.RLock()
	drafts := make([]Draft, 0, len(s.drafts))
	// In the shared-password mode the page also lists what waits for the
	// other accounts (otherTodos); the formal round's OA scans only its own.
	var open []Draft
	for _, draft := range s.drafts {
		if draft.open() && visible(draft, data) {
			drafts = append(drafts, cloneDraft(draft))
		}
		if s.config.AllowSwitch && draft.open() {
			open = append(open, cloneDraft(draft))
		}
	}
	s.mu.RUnlock()
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].CreatedAt.After(drafts[j].CreatedAt) })
	views := make([]draftView, 0, len(drafts))
	for _, draft := range drafts {
		views = append(views, s.view(draft))
	}
	s.render(w, "tasks", map[string]any{"Session": data, "Views": views, "Others": s.otherTodos(data, open)})
}

func (s *Server) viewDraft(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	s.mu.RLock()
	stored, found := s.drafts[chi.URLParam(r, "draftID")]
	var draft Draft
	status, message := http.StatusNotFound, ""
	if found {
		status, message = draftAccess(stored, data)
		draft = cloneDraft(stored)
	}
	s.mu.RUnlock()
	if refuseDraftAccess(w, r, status, message) {
		return
	}
	data.AgentIdentity = s.agentNav(r)
	s.render(w, "task", s.taskPage(data, draft))
}

// draftState answers the task page's poll (pollScript) with the session's
// page version (pageVersion), under the page's own access rules: 404 for an
// unknown draft, 403 for someone else's.
func (s *Server) draftState(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	s.mu.RLock()
	stored, found := s.drafts[chi.URLParam(r, "draftID")]
	var version string
	status, message := http.StatusNotFound, ""
	if found {
		status, message = draftAccess(stored, data)
		version = pageVersion(*stored, s.awaitsChange(*stored, decidesNow(*stored, data)))
	}
	s.mu.RUnlock()
	if refuseDraftAccess(w, r, status, message) {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
}

// draftVersion changes whenever the task page would show something new:
// another state or approver, another decision, TaskGate's note, the grant
// or the receipt.
func draftVersion(draft Draft) string {
	return strings.Join([]string{draft.State, draft.Approver, strconv.Itoa(len(draft.History)),
		strconv.FormatBool(draft.Note != ""), strconv.FormatBool(draft.ApprovedGrant != nil),
		strconv.FormatBool(draft.ApprovalReceipt != nil)}, "|")
}

// pageVersion is the version a session's task page is rendered from and
// polls against: draftVersion and whether the page awaits change
// (awaitsChange), so a page rendered inside the answer window also reloads
// once when the window closes and its waiting notice no longer applies. A
// waiting page reloads once draftState answers a different one.
func pageVersion(draft Draft, awaits bool) string {
	return draftVersion(draft) + "|" + strconv.FormatBool(awaits)
}

// decidesNow reports whether the session may decide the draft now: it is
// pending and assigned to the session's account, which can approve. The
// task page and its poll (draftState) both use it.
func decidesNow(draft Draft, data sessionData) bool {
	return draft.State == "pending" && data.CanApprove && strings.EqualFold(draft.Approver, data.Username)
}

// taskPage is what the task page renders: the view, whether the session
// may submit or decide the draft now, the switch to whoever acts on it
// instead (shared-password mode), whether the page polls for changes, and
// the draft's version it polls against.
func (s *Server) taskPage(data sessionData, draft Draft) map[string]any {
	canDecide := decidesNow(draft, data)
	awaits := s.awaitsChange(draft, canDecide)
	return map[string]any{"Session": data, "View": s.view(draft),
		"CanSubmit":   draft.State == "draft" && draft.relatesAsRequester(data.Username),
		"CanDecide":   canDecide,
		"SelfConfirm": strings.EqualFold(draft.Approver, draft.Manifest.HumanSubject) || draft.MemberConfirm,
		"SwitchTo":    s.switchOffer(data, draft),
		"Refresh":     awaits,
		"Version":     pageVersion(draft, awaits)}
}

// draftAccess decides whether the logged-in user may open a draft: a
// requester their own, an approver the drafts naming them now or that they
// decided before (an earlier step of an approval chain). A draft that is
// someone else's is 403 with the reason, not 404, so a user can tell "not
// mine" from "gone" (draft ids are random, so existence is not a secret worth
// keeping from a logged-in user).
func draftAccess(draft *Draft, data sessionData) (int, string) {
	switch {
	case draft.relatesAsRequester(data.Username):
		return http.StatusOK, ""
	case !data.CanApprove:
		return http.StatusForbidden, "此工单不属于你（" + data.Username + "），只有其申请人、代开人或确认成员可以打开和提交"
	case draft.ApprovalMode != "manual":
		return http.StatusForbidden, "此工单不需要人工审批"
	case !draft.relatesAsApprover(data.Username):
		return http.StatusForbidden, "此工单指派给审批人 " + draft.Approver + "，你（" + data.Username + "）不能查看或审批"
	}
	return http.StatusOK, ""
}

// refuseDraftAccess writes a refusal and reports whether it did.
func refuseDraftAccess(w http.ResponseWriter, r *http.Request, status int, message string) bool {
	switch status {
	case http.StatusOK:
		return false
	case http.StatusNotFound:
		http.NotFound(w, r)
	default:
		http.Error(w, message, status)
	}
	return true
}

func (s *Server) submitDraft(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	draftID := chi.URLParam(r, "draftID")
	s.mu.Lock()
	draft, ok := s.drafts[draftID]
	status, message := http.StatusNotFound, ""
	if ok {
		status, message = draftAccess(draft, data)
	}
	if status == http.StatusOK && !draft.relatesAsRequester(data.Username) {
		status, message = http.StatusForbidden, "只有申请人本人、代开人或确认成员可以提交"
	}
	if status == http.StatusOK && draft.State != "draft" {
		status, message = http.StatusConflict, "draft cannot be submitted"
	}
	if status != http.StatusOK {
		s.mu.Unlock()
		refuseDraftAccess(w, r, status, message)
		return
	}
	if err := validateStoredDraft(draft); err != nil {
		s.mu.Unlock()
		s.config.Logger.Error("authorization manifest changed before submission", "draft_id", draftID, "error", err)
		http.Error(w, "draft authorization manifest is invalid", http.StatusConflict)
		return
	}
	draft.State = "pending"
	draft.UpdatedAt = s.config.Clock().UTC()
	submittedSnapshot := cloneDraft(draft)
	s.mu.Unlock()

	s.dispatch(submittedSnapshot, "submitted", data.Username)
	http.Redirect(w, r, "/tasks/"+draftID, http.StatusSeeOther)
}

func (s *Server) decideDraft(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	if !data.CanApprove {
		http.Error(w, "approver only", http.StatusForbidden)
		return
	}
	decision := strings.ToLower(r.Form.Get("decision"))
	if decision == "narrowed" {
		decision = "narrow"
	}
	if decision != "approved" && decision != "rejected" && decision != "narrow" {
		http.Error(w, "invalid decision", http.StatusBadRequest)
		return
	}
	draftID := chi.URLParam(r, "draftID")
	s.mu.Lock()
	draft, ok := s.drafts[draftID]
	status, message := http.StatusNotFound, ""
	if ok {
		status, message = draftAccess(draft, data)
	}
	if status == http.StatusOK && draft.Approver != data.Username {
		status, message = http.StatusForbidden, "你（"+data.Username+"）已审批过此工单，现在等待 "+draft.Approver+" 审批；你只能查看"
	}
	if status == http.StatusOK && draft.State != "pending" {
		status, message = http.StatusConflict, "draft cannot be decided"
	}
	if status != http.StatusOK {
		s.mu.Unlock()
		refuseDraftAccess(w, r, status, message)
		return
	}
	if err := validateStoredDraft(draft); err != nil {
		s.mu.Unlock()
		s.config.Logger.Error("authorization manifest changed before decision", "draft_id", draftID, "error", err)
		http.Error(w, "draft authorization manifest is invalid", http.StatusConflict)
		return
	}
	issuedAt := s.config.Clock().UTC()
	parent, err := decisionBase(draft, issuedAt)
	if err != nil {
		s.mu.Unlock()
		http.Error(w, "cannot construct approved grant", http.StatusInternalServerError)
		return
	}
	var candidate *approval.TaskGrantCoreV1
	protocolDecision := approval.ApprovalDecisionReject
	state := "rejected"
	switch decision {
	case "approved":
		candidate = &parent
		protocolDecision = approval.ApprovalDecisionApprove
		state = "approved"
	case "narrow":
		grant, err := narrowedGrantFromForm(parent, r, issuedAt)
		if err != nil {
			s.mu.Unlock()
			http.Error(w, "invalid narrowing: "+err.Error(), http.StatusBadRequest)
			return
		}
		candidate = &grant
		protocolDecision = approval.ApprovalDecisionNarrow
		state = "narrowed"
	}
	if candidate != nil && draft.ParentExpiresAt != nil {
		// A delegated task may not outlive its parent: a grant is re-issued
		// from now with its TTL, so the TTL is shortened to what the parent
		// has left, which makes an approve-as-made a narrowing.
		allowed := draft.ParentExpiresAt.Sub(issuedAt).Truncate(time.Millisecond).Milliseconds()
		if allowed <= 0 {
			s.mu.Unlock()
			http.Error(w, "the parent authorization has expired", http.StatusConflict)
			return
		}
		if candidate.Budget.TaskTTLMS > allowed {
			candidate.Budget.TaskTTLMS = allowed
			candidate.ExpiresAt = issuedAt.UTC().Add(time.Duration(allowed) * time.Millisecond)
			protocolDecision, state = approval.ApprovalDecisionNarrow, "narrowed"
		}
	}
	if err := s.issueDecision(draft, protocolDecision, data.Username, issuedAt, candidate); err != nil {
		s.mu.Unlock()
		s.config.Logger.Error("sign approval decision", "draft_id", draftID, "error", err)
		http.Error(w, "cannot issue approval receipt", http.StatusInternalServerError)
		return
	}
	draft.State = state
	draft.UpdatedAt = issuedAt
	draft.Note = ""
	draft.History = append(draft.History, DraftDecision{Actor: data.Username, Decision: state, At: issuedAt,
		ReceiptID: draft.ApprovalReceipt.ReceiptID})
	snapshot := cloneDraft(draft)
	s.mu.Unlock()
	s.dispatch(snapshot, state, data.Username)
	http.Redirect(w, r, "/tasks/"+draftID, http.StatusSeeOther)
}

func (s *Server) dispatch(draft Draft, status, actor string) {
	occurredAt := s.config.Clock().UTC()
	if draft.ApprovalReceipt != nil {
		occurredAt = draft.ApprovalReceipt.IssuedAt
	}
	event := callbackEvent{
		EventID: randomID("evt"), TaskID: draft.Manifest.TaskID, DraftID: draft.ID, Status: status,
		Actor: actor, OccurredAt: occurredAt, CatalogVersion: draft.Manifest.CatalogVersion,
		CallbackContext: draft.Manifest.CallbackContext, ManifestDigest: draft.ManifestDigest,
		ApprovedGrant: cloneGrantPointer(draft.ApprovedGrant), ApprovalReceipt: cloneReceiptPointer(draft.ApprovalReceipt),
	}
	receiptID := ""
	if draft.ApprovalReceipt != nil {
		receiptID = draft.ApprovalReceipt.ReceiptID
	}
	go func() {
		var err error
		for attempt := 1; attempt <= 3; attempt++ {
			var answer []byte
			if answer, err = s.sendCallback(context.Background(), event); err == nil {
				s.applyAnswer(draft.ID, status, receiptID, answer)
				return
			}
			s.config.Logger.Warn("OA callback failed", "event_id", event.EventID, "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		s.noteOnDraft(draft.ID, status, receiptID, "TaskGate 未接受这次"+callbackStatusLabels[status]+"（已重试 3 次）："+err.Error())
	}()
}

// callbackAnswer is what TaskGate answers an accepted callback with. An
// approval-chain step that leaves steps open names the next approver; an
// approval TaskGate took without issuing a grant names the terminal reason.
type callbackAnswer struct {
	Pending        bool   `json:"pending"`
	NextApprover   string `json:"next_approver"`
	NextRole       string `json:"next_role"`
	NextRoleLabel  string `json:"next_role_label"`
	RemainingSteps int    `json:"remaining_steps"`
	TerminalReason string `json:"terminal_reason"`
}

var callbackStatusLabels = map[string]string{"submitted": "提交", "approved": "批准", "narrowed": "缩小后批准", "rejected": "拒绝"}

// approvalStepLabels is the fallback when TaskGate's answer carries no
// next_role_label (a gateway older than KB1-LOOP-147).
var approvalStepLabels = map[string]string{
	"requester_director":  "申请人所在部门审批人",
	"department_director": "被申请部门审批人",
	"leader":              "分管领导",
}

func (answer callbackAnswer) nextRoleLabel() string {
	if answer.NextRoleLabel != "" {
		return answer.NextRoleLabel
	}
	if label, ok := approvalStepLabels[answer.NextRole]; ok {
		return label
	}
	return answer.NextRole
}

var terminalReasonLabels = map[string]string{
	"approver_not_authorized": "审批人对所批范围无权，任务已归档",
}

// applyAnswer acts on TaskGate's answer to a decision: the next approver of
// an approval chain gets the draft, or the draft records that no grant was
// issued. It applies only while the draft still holds the decision answered.
func (s *Server) applyAnswer(draftID, status, receiptID string, body []byte) {
	if status == "submitted" || receiptID == "" {
		return
	}
	var answer callbackAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, ok := s.drafts[draftID]
	if !ok || draft.State != status || draft.ApprovalReceipt == nil || draft.ApprovalReceipt.ReceiptID != receiptID {
		return
	}
	switch {
	case answer.Pending && answer.NextApprover != "":
		next := strings.ToLower(strings.TrimSpace(answer.NextApprover))
		previous := draft.Approver
		draft.Approver, draft.State = next, "pending"
		draft.Note = fmt.Sprintf("%s 已%s；TaskGate 按组织架构要求下一级 %s（%s）审批，还剩 %d 级",
			s.displayName(previous), callbackStatusLabels[status], s.displayName(next), answer.nextRoleLabel(), answer.RemainingSteps)
		if account, ok := s.users[next]; !ok || !account.CanApprove {
			draft.Note += "；本 OA 没有该审批人账号，无法继续"
			s.config.Logger.Error("approval chain names an approver the OA does not have", "draft_id", draftID, "approver", next)
		}
	case answer.TerminalReason != "":
		draft.State = "not_granted"
		reason := terminalReasonLabels[answer.TerminalReason]
		if reason == "" {
			reason = answer.TerminalReason
		}
		draft.Note = "TaskGate 未签发授权：" + reason
	default:
		return
	}
	draft.UpdatedAt = s.config.Clock().UTC()
}

// noteOnDraft records why a callback failed while the draft still holds it.
func (s *Server) noteOnDraft(draftID, status, receiptID, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A refused submission leaves the draft pending; a refused decision, in
	// the state the decision set.
	held := status
	if status == "submitted" {
		held = "pending"
	}
	draft, ok := s.drafts[draftID]
	if !ok || draft.State != held {
		return
	}
	if receiptID != "" && (draft.ApprovalReceipt == nil || draft.ApprovalReceipt.ReceiptID != receiptID) {
		return
	}
	draft.Note = note
}

// displayName is the account's display name with its login name.
func (s *Server) displayName(username string) string {
	if account, ok := s.users[username]; ok && account.Name != username {
		return account.Name + "（" + username + "）"
	}
	return username
}

func (s *Server) sendCallback(ctx context.Context, event callbackEvent) ([]byte, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	status, responseBody, err := s.signedPost(ctx, s.config.CallbackURL, event.EventID, body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		// The Gateway explains a rejection only in its response body; keep an
		// excerpt so the retry log says why activation did not happen.
		return nil, fmt.Errorf("callback status %d: %s", status, excerpt(string(responseBody)))
	}
	return responseBody, nil
}

// signedPost posts a JSON body to the Gateway the way it authenticates the
// OA: X-OA-Event-ID, X-OA-Timestamp and X-OA-Signature, an HMAC of the
// timestamp and the raw body under the callback secret. It returns the status
// and at most 64 KiB of the answer.
func (s *Server) signedPost(ctx context.Context, target, eventID string, body []byte) (int, []byte, error) {
	timestamp := strconv.FormatInt(s.config.Clock().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OA-Event-ID", eventID)
	req.Header.Set("X-OA-Timestamp", timestamp)
	req.Header.Set("X-OA-Signature", approval.Sign([]byte(s.config.CallbackSecret), timestamp, body))
	resp, err := s.config.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, responseBody, nil
}

// excerpt trims text to at most 512 bytes, cut at a character boundary.
func excerpt(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= 512 {
		return text
	}
	cut := 512
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func cloneDraft(draft *Draft) Draft {
	copy := *draft
	copy.Manifest = cloneManifest(draft.Manifest)
	copy.ApprovedGrant = cloneGrantPointer(draft.ApprovedGrant)
	copy.ApprovalReceipt = cloneReceiptPointer(draft.ApprovalReceipt)
	copy.History = append([]DraftDecision(nil), draft.History...)
	if draft.Withdrawal != nil {
		withdrawal := *draft.Withdrawal
		copy.Withdrawal = &withdrawal
	}
	return copy
}

func validateStoredDraft(draft *Draft) error {
	return approval.ValidateAuthorizationSnapshot(approval.DraftRequest{
		Manifest: draft.Manifest, ManifestDigest: draft.ManifestDigest,
		ApprovalMode: draft.ApprovalMode, Approver: draft.Approver,
	})
}

func cloneManifest(source approval.AuthorizationManifestV1) approval.AuthorizationManifestV1 {
	copy := source
	copy.Products = append([]string(nil), source.Products...)
	copy.ApprovedColumns = cloneColumns(source.ApprovedColumns)
	copy.MandatoryScope = cloneMandatoryScope(source.MandatoryScope)
	return copy
}

func cloneColumns(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for product, columns := range source {
		result[product] = append([]string(nil), columns...)
	}
	return result
}

func cloneMandatoryScope(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for name, value := range source {
		switch typed := value.(type) {
		case []string:
			result[name] = append([]string(nil), typed...)
		case []any:
			result[name] = append([]any(nil), typed...)
		case map[string]string:
			copy := make(map[string]string, len(typed))
			for key, item := range typed {
				copy[key] = item
			}
			result[name] = copy
		case map[string]any:
			copy := make(map[string]any, len(typed))
			for key, item := range typed {
				copy[key] = item
			}
			result[name] = copy
		default:
			result[name] = typed
		}
	}
	return result
}

func domainCoreForDraft(draft *Draft, issuedAt time.Time) (approval.TaskGrantCoreV1, error) {
	return domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, issuedAt)
}

// decisionBase is what an approver approves or narrows: the manifest's core,
// or in an approval chain the previous approver's grant re-issued now, which
// is what TaskGate checks the decision against.
func decisionBase(draft *Draft, issuedAt time.Time) (approval.TaskGrantCoreV1, error) {
	if draft.ApprovedGrant != nil {
		return draft.ApprovedGrant.Reissued(issuedAt), nil
	}
	return domainCoreForDraft(draft, issuedAt)
}

func (s *Server) issueDecision(draft *Draft, decision approval.ApprovalDecision, actor string, issuedAt time.Time, grant *approval.TaskGrantCoreV1) error {
	grantDigest := ""
	if grant != nil {
		computed, err := approval.GrantCoreDigest(*grant)
		if err != nil {
			return err
		}
		grantDigest = computed
	}
	receipt, err := s.config.ReceiptSigner.SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: randomID("receipt"),
		TaskID: draft.Manifest.TaskID, Decision: decision, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: grantDigest, ApproverID: actor, IssuedAt: issuedAt.UTC(),
	})
	if err != nil {
		return err
	}
	draft.ApprovedGrant = cloneGrantPointer(grant)
	draft.ApprovalReceipt = cloneReceiptPointer(&receipt)
	return nil
}

func narrowedGrantFromForm(parent approval.TaskGrantCoreV1, r *http.Request, issuedAt time.Time) (approval.TaskGrantCoreV1, error) {
	candidate := parent

	productsRaw := strings.TrimSpace(r.Form.Get("approved_products"))
	if productsRaw == "" {
		return approval.TaskGrantCoreV1{}, errors.New("approved_products is required")
	}
	products := checkedProducts(r.Form)
	if len(products) > 0 {
		// the checkbox form: one value per kept product
	} else if strings.HasPrefix(productsRaw, "[") {
		if err := decodeStrictJSON(productsRaw, &products); err != nil {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("approved_products: %w", err)
		}
	} else {
		for _, product := range strings.Split(productsRaw, ",") {
			if product = strings.TrimSpace(product); product != "" {
				products = append(products, product)
			}
		}
	}
	if len(products) == 0 {
		return approval.TaskGrantCoreV1{}, errors.New("approved_products cannot be empty")
	}
	sort.Strings(products)
	candidate.ApprovedProducts = products

	var columns map[string][]string
	if raw := strings.TrimSpace(r.Form.Get("approved_columns")); raw != "" {
		if err := decodeStrictJSON(raw, &columns); err != nil {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("approved_columns: %w", err)
		}
	} else {
		checked, err := checkedColumns(r.Form, products)
		if err != nil {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("approved_columns: %w", err)
		}
		columns = checked
	}
	for product := range columns {
		sort.Strings(columns[product])
	}
	candidate.ApprovedColumns = columns

	var scope map[string]any
	if raw := strings.TrimSpace(r.Form.Get("mandatory_scope")); raw != "" {
		if err := decodeStrictJSON(raw, &scope); err != nil {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("mandatory_scope: %w", err)
		}
	} else {
		checked, err := checkedScope(r.Form, cloneMandatoryScope(parent.MandatoryScope))
		if err != nil {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("mandatory_scope: %w", err)
		}
		scope = checked
	}
	for name, value := range scope {
		if values, ok := value.([]any); ok {
			sort.Slice(values, func(i, j int) bool {
				left, _ := values[i].(string)
				right, _ := values[j].(string)
				return left < right
			})
			scope[name] = values
		}
	}
	candidate.MandatoryScope = scope

	// The form narrows resource limits, but exposure limits and their semantic
	// profile are not editable dimensions in the demo UI. Preserve those
	// signed ceilings instead of accidentally disabling exposure accounting.
	candidate.Budget = approval.AuthorizationBudgetV1{
		MaxReleaseFacts:        parent.Budget.MaxReleaseFacts,
		MaxInfluenceFacts:      parent.Budget.MaxInfluenceFacts,
		MaxOutcomeFacts:        parent.Budget.MaxOutcomeFacts,
		ExposureProfileVersion: parent.Budget.ExposureProfileVersion,
		PredicateFootprint:     clonePredicateFootprint(parent.Budget.PredicateFootprint),
	}
	budgetFields := []struct {
		name   string
		target *int64
	}{
		{"max_queries", &candidate.Budget.MaxQueries},
		{"max_result_rows", &candidate.Budget.MaxResultRows},
		{"max_db_ms", &candidate.Budget.MaxDBMS},
		{"per_query_timeout_ms", &candidate.Budget.PerQueryTimeoutMS},
		{"task_ttl_ms", &candidate.Budget.TaskTTLMS},
	}
	for _, field := range budgetFields {
		value, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get(field.name)), 10, 64)
		if err != nil || value <= 0 {
			return approval.TaskGrantCoreV1{}, fmt.Errorf("%s must be a positive integer", field.name)
		}
		*field.target = value
	}
	candidate.ExpiresAt = issuedAt.UTC().Add(time.Duration(candidate.Budget.TaskTTLMS) * time.Millisecond)
	if err := parent.CheckNarrowing(candidate); err != nil {
		return approval.TaskGrantCoreV1{}, err
	}
	parentDigest, err := approval.GrantCoreDigest(parent)
	if err != nil {
		return approval.TaskGrantCoreV1{}, err
	}
	candidateDigest, err := approval.GrantCoreDigest(candidate)
	if err != nil {
		return approval.TaskGrantCoreV1{}, err
	}
	if candidateDigest == parentDigest {
		return approval.TaskGrantCoreV1{}, errors.New("narrow must reduce at least one authorization dimension")
	}
	return candidate, nil
}

func decodeStrictJSON(raw string, target any) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("value is required")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON is forbidden")
	}
	return nil
}

func cloneTime(source *time.Time) *time.Time {
	if source == nil {
		return nil
	}
	value := source.UTC()
	return &value
}

func cloneGrantPointer(source *approval.TaskGrantCoreV1) *approval.TaskGrantCoreV1 {
	if source == nil {
		return nil
	}
	copy := *source
	copy.ApprovedProducts = append([]string(nil), source.ApprovedProducts...)
	copy.ApprovedColumns = cloneColumns(source.ApprovedColumns)
	copy.MandatoryScope = cloneMandatoryScope(source.MandatoryScope)
	return &copy
}

func cloneReceiptPointer(source *approval.ApprovalReceiptV1) *approval.ApprovalReceiptV1 {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}

func (s *Server) session(r *http.Request) (string, bool) {
	cookie, err := r.Cookie("oa_session")
	if err != nil {
		return "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	provided, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(s.config.SessionSecret))
	_, _ = mac.Write(payload)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", false
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 3 {
		return "", false
	}
	expires, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || !s.config.Clock().Before(time.Unix(expires, 0)) {
		return "", false
	}
	_, ok := s.users[fields[0]]
	return fields[0], ok
}

func (s *Server) signSession(username string, expires time.Time) string {
	payload := []byte(username + "|" + strconv.FormatInt(expires.Unix(), 10) + "|" + randomID("s"))
	mac := hmac.New(sha256.New, []byte(s.config.SessionSecret))
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) csrf(r *http.Request) string {
	cookie, err := r.Cookie("oa_session")
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(s.config.SessionSecret))
	_, _ = mac.Write([]byte("csrf." + cookie.Value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		s.config.Logger.Error("render template", "template", name, "error", err)
	}
}

func validCookieForm(r *http.Request, cookieName, formValue string) bool {
	cookie, err := r.Cookie(cookieName)
	return err == nil && constantToken(cookie.Value, formValue)
}

func constantToken(actual, expected string) bool {
	if len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func randomID(prefix string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func templateJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decisionLabel(decision string) string {
	if label, ok := callbackStatusLabels[decision]; ok {
		return label
	}
	return decision
}

func roleLabel(role string) string {
	switch role {
	case "requester":
		return "申请人"
	case "approver":
		return "审批人"
	case "both":
		return "申请人兼审批人"
	}
	return role
}

// loginOption is one account offered on the login page.
type loginOption struct{ ID, Name, Role string }

// loginOptions lists the configured accounts, requesters first, then by name.
func (s *Server) loginOptions() []loginOption {
	options := make([]loginOption, 0, len(s.users))
	for id, account := range s.users {
		options = append(options, loginOption{ID: id, Name: account.Name, Role: account.Role})
	}
	rank := map[string]int{"requester": 0, "both": 1, "approver": 2}
	sort.Slice(options, func(i, j int) bool {
		if rank[options[i].Role] != rank[options[j].Role] {
			return rank[options[i].Role] < rank[options[j].Role]
		}
		return options[i].ID < options[j].ID
	})
	return options
}
