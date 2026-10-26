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
	ExtraUsers    []ExtraUser
	HTTPClient    *http.Client
	Logger        *slog.Logger
	Clock         func() time.Time
	ReceiptSigner approval.ReceiptSigner
}

// ExtraUser is one configured demo account.
type ExtraUser struct {
	Name     string
	Role     string // requester or approver
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
	Role         string
	PasswordHash []byte
}

type DraftRequest = approval.DraftRequest

type Draft struct {
	ID              string                           `json:"id"`
	Manifest        approval.AuthorizationManifestV1 `json:"authorization_manifest"`
	ManifestDigest  string                           `json:"manifest_digest"`
	ApprovalMode    string                           `json:"approval_mode"`
	Approver        string                           `json:"approver,omitempty"`
	ApprovedGrant   *approval.TaskGrantCoreV1        `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1      `json:"approval_receipt,omitempty"`
	State           string                           `json:"state"`
	// History lists the decisions on the draft, oldest first. In an approval
	// chain (KB1-LOOP-141) TaskGate answers each approval with the next
	// approver; the draft is then reassigned to them and Approver is always
	// the one who decides now.
	History []DraftDecision `json:"history,omitempty"`
	// Note is what TaskGate answered the last callback with when it did not
	// simply take it: the next approver, why no grant was issued, or why the
	// callback was refused.
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	if config.AlicePassword == "" || config.BobPassword == "" {
		return nil, errors.New("OA demo passwords are required")
	}
	if config.PublicBaseURL == "" {
		config.PublicBaseURL = "http://127.0.0.1:8092"
	}
	if _, err := url.ParseRequestURI(config.PublicBaseURL); err != nil {
		return nil, fmt.Errorf("invalid public base URL: %w", err)
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
	aliceHash, err := bcrypt.GenerateFromPassword([]byte(config.AlicePassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	bobHash, err := bcrypt.GenerateFromPassword([]byte(config.BobPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	parsedTemplates, err := template.New("layout").Funcs(template.FuncMap{"json": templateJSON, "decisionLabel": decisionLabel, "roleLabel": roleLabel}).Parse(pageTemplates)
	if err != nil {
		return nil, err
	}
	users := map[string]user{
		"alice": {Name: "Alice", Role: "requester", PasswordHash: aliceHash},
		"bob":   {Name: "Bob", Role: "approver", PasswordHash: bobHash},
	}
	if config.DanaPassword != "" {
		danaHash, err := bcrypt.GenerateFromPassword([]byte(config.DanaPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		users["dana"] = user{Name: "Dana", Role: "approver", PasswordHash: danaHash}
	}
	for _, extra := range config.ExtraUsers {
		name := strings.ToLower(strings.TrimSpace(extra.Name))
		if _, exists := users[name]; exists || name == "" || extra.Password == "" || (extra.Role != "requester" && extra.Role != "approver") {
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
		users[name] = user{Name: display, Role: extra.Role, PasswordHash: hash}
	}
	config.ExtraUsers = nil
	config.AlicePassword = ""
	config.BobPassword = ""
	config.DanaPassword = ""
	return &Server{
		config:    config,
		users:     users,
		drafts:    make(map[string]*Draft),
		templates: parsedTemplates,
	}, nil
}

func (s *Server) Handler() http.Handler {
	router := chi.NewRouter()
	router.Get("/health/live", emptyHealth)
	router.Get("/health/ready", emptyHealth)
	router.Get("/login", s.loginPage)
	router.Post("/login", s.login)
	router.With(s.requireSession, s.requireCSRF).Post("/logout", s.logout)
	router.Post("/api/drafts", s.createDraft)
	router.Group(func(protected chi.Router) {
		protected.Use(s.requireSession)
		protected.Get("/", s.listDrafts)
		protected.Get("/tasks", s.listDrafts)
		protected.Get("/tasks/{draftID}", s.viewDraft)
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
		ApprovalMode: request.ApprovalMode, Approver: strings.ToLower(request.Approver),
		State: "draft", CreatedAt: now, UpdatedAt: now,
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

func (s *Server) validateDraftRequest(request DraftRequest) error {
	if err := approval.ValidateAuthorizationSnapshot(request); err != nil {
		return fmt.Errorf("invalid authorization manifest: %w", err)
	}
	switch request.ApprovalMode {
	case "manual":
		if approver, ok := s.users[request.Approver]; !ok || approver.Role != "approver" {
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
	s.render(w, "login", map[string]any{"CSRF": csrf, "Error": r.URL.Query().Get("error"), "Users": s.loginOptions()})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !validCookieForm(r, "oa_login_csrf", r.Form.Get("csrf")) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.Form.Get("username")))
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
	CSRF        string
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		account := s.users[username]
		data := sessionData{Username: username, DisplayName: account.Name, Role: account.Role, CSRF: s.csrf(r)}
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
	s.mu.RLock()
	drafts := make([]Draft, 0, len(s.drafts))
	for _, draft := range s.drafts {
		if data.Role == "requester" && !strings.EqualFold(draft.Manifest.HumanSubject, data.Username) {
			continue
		}
		if data.Role == "approver" && (draft.ApprovalMode != "manual" || (draft.Approver != data.Username && !draft.decidedBy(data.Username))) {
			continue
		}
		drafts = append(drafts, cloneDraft(draft))
	}
	s.mu.RUnlock()
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].CreatedAt.After(drafts[j].CreatedAt) })
	s.render(w, "tasks", map[string]any{"Session": data, "Drafts": drafts})
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
	if !refuseDraftAccess(w, r, status, message) {
		s.render(w, "task", map[string]any{"Session": data, "Draft": draft})
	}
}

// draftAccess decides whether the logged-in user may open a draft: a
// requester their own, an approver the drafts naming them now or that they
// decided before (an earlier step of an approval chain). A draft that is
// someone else's is 403 with the reason, not 404, so a user can tell "not
// mine" from "gone" (draft ids are random, so existence is not a secret worth
// keeping from a logged-in user).
func draftAccess(draft *Draft, data sessionData) (int, string) {
	switch {
	case data.Role == "requester" && !strings.EqualFold(draft.Manifest.HumanSubject, data.Username):
		return http.StatusForbidden, "此工单不属于你（" + data.Username + "），只有其申请人本人可以打开和提交"
	case data.Role == "approver" && draft.ApprovalMode != "manual":
		return http.StatusForbidden, "此工单不需要人工审批"
	case data.Role == "approver" && draft.Approver != data.Username && !draft.decidedBy(data.Username):
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
	if data.Role != "requester" {
		http.Error(w, "requester only", http.StatusForbidden)
		return
	}
	draftID := chi.URLParam(r, "draftID")
	s.mu.Lock()
	draft, ok := s.drafts[draftID]
	status, message := http.StatusNotFound, ""
	if ok {
		status, message = draftAccess(draft, data)
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
	if data.Role != "approver" {
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
	RemainingSteps int    `json:"remaining_steps"`
	TerminalReason string `json:"terminal_reason"`
}

var callbackStatusLabels = map[string]string{"submitted": "提交", "approved": "批准", "narrowed": "缩小后批准", "rejected": "拒绝"}

var approvalStepLabels = map[string]string{
	"requester_director":  "申请人所在部门主任",
	"department_director": "被申请部门主任",
	"leader":              "分管领导",
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
			s.displayName(previous), callbackStatusLabels[status], s.displayName(next), approvalStepLabels[answer.NextRole], answer.RemainingSteps)
		if account, ok := s.users[next]; !ok || account.Role != "approver" {
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
	timestamp := strconv.FormatInt(s.config.Clock().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OA-Event-ID", event.EventID)
	req.Header.Set("X-OA-Timestamp", timestamp)
	req.Header.Set("X-OA-Signature", approval.Sign([]byte(s.config.CallbackSecret), timestamp, body))
	resp, err := s.config.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The Gateway explains a rejection only in its response body; keep an
		// excerpt so the retry log says why activation did not happen.
		excerpt := strings.TrimSpace(string(responseBody))
		if len(excerpt) > 512 {
			excerpt = excerpt[:512]
		}
		return nil, fmt.Errorf("callback status %d: %s", resp.StatusCode, excerpt)
	}
	return responseBody, nil
}

func cloneDraft(draft *Draft) Draft {
	copy := *draft
	copy.Manifest = cloneManifest(draft.Manifest)
	copy.ApprovedGrant = cloneGrantPointer(draft.ApprovedGrant)
	copy.ApprovalReceipt = cloneReceiptPointer(draft.ApprovalReceipt)
	copy.History = append([]DraftDecision(nil), draft.History...)
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
	var products []string
	if strings.HasPrefix(productsRaw, "[") {
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
	if err := decodeStrictJSON(r.Form.Get("approved_columns"), &columns); err != nil {
		return approval.TaskGrantCoreV1{}, fmt.Errorf("approved_columns: %w", err)
	}
	for product := range columns {
		sort.Strings(columns[product])
	}
	candidate.ApprovedColumns = columns

	var scope map[string]any
	if err := decodeStrictJSON(r.Form.Get("mandatory_scope"), &scope); err != nil {
		return approval.TaskGrantCoreV1{}, fmt.Errorf("mandatory_scope: %w", err)
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

const pageTemplates = `
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Task-bound OA Demo</title><style>
body{font-family:system-ui,sans-serif;background:#f4f6f8;color:#17202a;margin:0}.wrap{max-width:880px;margin:48px auto;padding:0 20px}.card{background:white;border:1px solid #dfe5eb;border-radius:12px;padding:24px;margin:16px 0;box-shadow:0 2px 10px #17202a0d}input,select,textarea,button{font:inherit;padding:10px 12px;border:1px solid #bcc7d1;border-radius:8px}textarea{box-sizing:border-box;width:100%;min-height:70px}button{background:#1769aa;color:white;border:0;cursor:pointer}.danger{background:#a92828}.muted{color:#617080}.tag{display:inline-block;padding:3px 8px;border-radius:99px;background:#e8f1f8}nav{display:flex;justify-content:space-between;align-items:center}form.inline{display:inline}.narrow{margin-top:20px;padding-top:16px;border-top:1px solid #dfe5eb}.budget-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px}.budget-grid input{width:90%}dt{font-weight:600;margin-top:12px}dd{margin-left:0}.error{color:#a92828}.note{background:#fff7e0;border:1px solid #f0d58a;border-radius:8px;padding:10px 12px}ol.history{padding-left:20px}a{color:#1769aa;text-decoration:none}</style></head><body><div class="wrap">{{end}}
{{define "foot"}}</div></body></html>{{end}}
{{define "login"}}{{template "head" .}}<div class="card"><h1>OA Demo 登录</h1><p class="muted">申请人提交自己的申请；审批人只看到指派给自己或自己审批过的工单。</p>{{if .Error}}<p class="error">用户名或密码错误</p>{{end}}<form method="post" action="/login"><input type="hidden" name="csrf" value="{{.CSRF}}"><p><label>用户<br><select name="username">{{range .Users}}<option value="{{.ID}}">{{.Name}}（{{.ID}}，{{roleLabel .Role}}）</option>{{end}}</select></label></p><p><label>密码<br><input type="password" name="password" required autocomplete="current-password"></label></p><button type="submit">登录</button></form></div>{{template "foot" .}}{{end}}
{{define "nav"}}<nav><div><strong>Task-bound OA</strong> · {{if ne .DisplayName .Username}}{{.DisplayName}} · {{end}}{{.Username}} / {{.Role}}</div><form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">退出</button></form></nav>{{end}}
{{define "tasks"}}{{template "head" .}}{{template "nav" .Session}}<h1>审批任务</h1>{{range .Drafts}}<div class="card"><span class="tag">{{.State}}</span><h2><a href="/tasks/{{.ID}}">{{.Manifest.DeclaredObjective}}</a></h2><p class="muted">{{.Manifest.TaskID}} · Agent {{.Manifest.AgentID}} · {{.Manifest.Sensitivity}} · {{.ApprovalMode}}{{if .Approver}} · 当前审批人 {{.Approver}}{{end}}</p>{{if .Note}}<p class="note">{{.Note}}</p>{{end}}</div>{{else}}<div class="card"><p>暂无任务。</p></div>{{end}}{{template "foot" .}}{{end}}
{{define "task"}}{{template "head" .}}{{template "nav" .Session}}{{$envelope := .Draft.Envelope}}<div class="card"><span class="tag">{{.Draft.State}}</span><h1>{{.Draft.Manifest.DeclaredObjective}}</h1><dl><dt>Task ID</dt><dd>{{.Draft.Manifest.TaskID}}</dd><dt>人类主体</dt><dd>{{.Draft.Manifest.HumanSubject}}</dd><dt>Agent 身份</dt><dd>{{.Draft.Manifest.AgentID}}</dd><dt>申请数据产品</dt><dd>{{range .Draft.Manifest.Products}}{{.}} {{end}}</dd><dt>申请字段</dt><dd>{{range $product, $columns := .Draft.Manifest.ApprovedColumns}}<strong>{{$product}}</strong>: {{range $columns}}{{.}} {{end}}<br>{{end}}</dd><dt>强制数据范围</dt><dd>{{range $name, $value := .Draft.Manifest.MandatoryScope}}<strong>{{$name}}</strong>: {{$value}}<br>{{end}}</dd><dt>敏感级别上限</dt><dd>{{.Draft.Manifest.Sensitivity}}</dd><dt>全部预算与有效期</dt><dd>最大查询次数 {{.Draft.Manifest.Budget.MaxQueries}}；最大返回行数 {{.Draft.Manifest.Budget.MaxResultRows}}；最大数据库时间 {{.Draft.Manifest.Budget.MaxDBMS}} ms；单次查询超时 {{.Draft.Manifest.Budget.PerQueryTimeoutMS}} ms；任务有效期 TTL {{.Draft.Manifest.Budget.TaskTTLMS}} ms</dd><dt>审批方式</dt><dd>{{.Draft.ApprovalMode}}{{if .Draft.Approver}} / 当前审批人 {{.Draft.Approver}}{{end}}</dd><dt>目录版本 / SHA-256</dt><dd>{{.Draft.Manifest.CatalogVersion}} / <code>{{.Draft.Manifest.CatalogSHA256}}</code></dd><dt>Manifest SHA-256</dt><dd><code>{{.Draft.ManifestDigest}}</code></dd></dl>{{if .Draft.Note}}<p class="note">{{.Draft.Note}}</p>{{end}}{{if .Draft.History}}<h2>审批记录</h2><ol class="history">{{range .Draft.History}}<li>{{.At.UTC.Format "2006-01-02 15:04:05"}} UTC · {{.Actor}} {{decisionLabel .Decision}}</li>{{end}}</ol>{{end}}{{if $envelope.Granted}}<h2>上一级批准的范围</h2><p class="muted">本级在此基础上批准或缩小，不能扩大。</p><dl><dt>数据产品</dt><dd>{{range $envelope.Products}}{{.}} {{end}}</dd><dt>字段</dt><dd>{{range $product, $columns := $envelope.Columns}}<strong>{{$product}}</strong>: {{range $columns}}{{.}} {{end}}<br>{{end}}</dd><dt>强制数据范围</dt><dd>{{range $name, $value := $envelope.Scope}}<strong>{{$name}}</strong>: {{$value}}<br>{{end}}</dd><dt>预算与有效期</dt><dd>最大查询次数 {{$envelope.Budget.MaxQueries}}；最大返回行数 {{$envelope.Budget.MaxResultRows}}；最大数据库时间 {{$envelope.Budget.MaxDBMS}} ms；单次查询超时 {{$envelope.Budget.PerQueryTimeoutMS}} ms；任务有效期 TTL {{$envelope.Budget.TaskTTLMS}} ms（从最后一级批准时起算）</dd></dl>{{end}}{{if and (eq .Session.Role "requester") (eq .Draft.State "draft")}}<form method="post" action="/tasks/{{.Draft.ID}}/submit"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button type="submit">提交申请</button></form>{{end}}{{if and (eq .Session.Role "approver") (eq .Draft.State "pending") (eq .Draft.Approver .Session.Username)}}<form class="inline" method="post" action="/tasks/{{.Draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="approved"><button type="submit">按原申请批准</button></form> <form class="inline" method="post" action="/tasks/{{.Draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="rejected"><button class="danger" type="submit">拒绝</button></form><form class="narrow" method="post" action="/tasks/{{.Draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="narrow"><h2>缩小后批准</h2><p><label>产品（JSON 数组或逗号分隔）<br><textarea name="approved_products" required>{{json $envelope.Products}}</textarea></label></p><p><label>字段（JSON 对象）<br><textarea name="approved_columns" required>{{json $envelope.Columns}}</textarea></label></p><p><label>强制范围（枚举可取子集，日期区间可收紧）<br><textarea name="mandatory_scope" required>{{json $envelope.Scope}}</textarea></label></p><div class="budget-grid"><label>最大查询次数<input name="max_queries" type="number" min="1" max="{{$envelope.Budget.MaxQueries}}" value="{{$envelope.Budget.MaxQueries}}" required></label><label>最大返回行数<input name="max_result_rows" type="number" min="1" max="{{$envelope.Budget.MaxResultRows}}" value="{{$envelope.Budget.MaxResultRows}}" required></label><label>最大数据库时间 (ms)<input name="max_db_ms" type="number" min="1" max="{{$envelope.Budget.MaxDBMS}}" value="{{$envelope.Budget.MaxDBMS}}" required></label><label>单次超时 (ms)<input name="per_query_timeout_ms" type="number" min="1" max="{{$envelope.Budget.PerQueryTimeoutMS}}" value="{{$envelope.Budget.PerQueryTimeoutMS}}" required></label><label>任务 TTL (ms)<input name="task_ttl_ms" type="number" min="1" max="{{$envelope.Budget.TaskTTLMS}}" value="{{$envelope.Budget.TaskTTLMS}}" required></label></div><p><button type="submit">缩小后批准</button></p></form>{{end}}</div><p><a href="/tasks">← 返回任务列表</a></p>{{template "foot" .}}{{end}}
`

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
	sort.Slice(options, func(i, j int) bool {
		if options[i].Role != options[j].Role {
			return options[i].Role == "requester"
		}
		return options[i].ID < options[j].ID
	})
	return options
}
