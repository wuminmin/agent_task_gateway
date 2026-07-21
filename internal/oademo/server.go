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
)

type Config struct {
	ServiceToken   string
	CallbackSecret string
	SessionSecret  string
	CallbackURL    string
	PublicBaseURL  string
	AlicePassword  string
	BobPassword    string
	HTTPClient     *http.Client
	Logger         *slog.Logger
	Clock          func() time.Time
}

type Server struct {
	config    Config
	users     map[string]user
	mu        sync.RWMutex
	drafts    map[string]*Draft
	templates *template.Template
}

type user struct {
	Name         string
	Role         string
	PasswordHash []byte
}

type DraftRequest struct {
	TaskID          string   `json:"task_id"`
	Requester       string   `json:"requester"`
	Objective       string   `json:"objective"`
	DataProducts    []string `json:"data_products"`
	Sensitivity     string   `json:"sensitivity"`
	ApprovalMode    string   `json:"approval_mode"`
	Approver        string   `json:"approver,omitempty"`
	CatalogVersion  string   `json:"catalog_version"`
	CallbackContext string   `json:"callback_context,omitempty"`
}

type Draft struct {
	ID              string    `json:"id"`
	TaskID          string    `json:"task_id"`
	Requester       string    `json:"requester"`
	Objective       string    `json:"objective"`
	DataProducts    []string  `json:"data_products"`
	Sensitivity     string    `json:"sensitivity"`
	ApprovalMode    string    `json:"approval_mode"`
	Approver        string    `json:"approver,omitempty"`
	CatalogVersion  string    `json:"catalog_version"`
	CallbackContext string    `json:"callback_context,omitempty"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type callbackEvent struct {
	EventID         string    `json:"event_id"`
	TaskID          string    `json:"task_id"`
	DraftID         string    `json:"draft_id"`
	Status          string    `json:"status"`
	Actor           string    `json:"actor"`
	OccurredAt      time.Time `json:"occurred_at"`
	CatalogVersion  string    `json:"catalog_version"`
	CallbackContext string    `json:"callback_context,omitempty"`
	ApprovalReceipt string    `json:"approval_receipt,omitempty"`
}

func New(config Config) (*Server, error) {
	if config.ServiceToken == "" || config.CallbackSecret == "" || config.SessionSecret == "" || config.CallbackURL == "" {
		return nil, errors.New("OA service token, callback secret, session secret, and callback URL are required")
	}
	if config.AlicePassword == "" || config.BobPassword == "" {
		return nil, errors.New("OA demo passwords are required")
	}
	if config.PublicBaseURL == "" {
		config.PublicBaseURL = "http://127.0.0.1:8090"
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
	aliceHash, err := bcrypt.GenerateFromPassword([]byte(config.AlicePassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	bobHash, err := bcrypt.GenerateFromPassword([]byte(config.BobPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	parsedTemplates, err := template.New("layout").Parse(pageTemplates)
	if err != nil {
		return nil, err
	}
	config.AlicePassword = ""
	config.BobPassword = ""
	return &Server{
		config: config,
		users: map[string]user{
			"alice": {Name: "Alice", Role: "requester", PasswordHash: aliceHash},
			"bob":   {Name: "Bob", Role: "approver", PasswordHash: bobHash},
		},
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
	if err := validateDraftRequest(request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	now := s.config.Clock().UTC()
	draft := &Draft{
		ID: randomID("oa"), TaskID: request.TaskID, Requester: strings.ToLower(request.Requester),
		Objective: request.Objective, DataProducts: append([]string(nil), request.DataProducts...),
		Sensitivity: request.Sensitivity, ApprovalMode: request.ApprovalMode, Approver: strings.ToLower(request.Approver),
		CatalogVersion: request.CatalogVersion, CallbackContext: request.CallbackContext,
		State: "draft", CreatedAt: now, UpdatedAt: now,
	}
	s.mu.Lock()
	s.drafts[draft.ID] = draft
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"draft_id": draft.ID,
		"state":    draft.State,
		"url":      strings.TrimRight(s.config.PublicBaseURL, "/") + "/tasks/" + draft.ID,
	})
}

func validateDraftRequest(request DraftRequest) error {
	if request.TaskID == "" || strings.ToLower(request.Requester) != "alice" || strings.TrimSpace(request.Objective) == "" || len(request.DataProducts) == 0 || request.CatalogVersion == "" {
		return errors.New("task_id, Alice requester, objective, data_products, and catalog_version are required")
	}
	switch request.ApprovalMode {
	case "auto":
		if request.Approver != "" {
			return errors.New("auto approval cannot specify an approver")
		}
	case "manual":
		if strings.ToLower(request.Approver) != "bob" {
			return errors.New("manual demo approval requires Bob")
		}
	default:
		return errors.New("approval_mode must be auto or manual")
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
	s.render(w, "login", map[string]any{"CSRF": csrf, "Error": r.URL.Query().Get("error")})
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
	Username string
	Role     string
	CSRF     string
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		account := s.users[username]
		data := sessionData{Username: username, Role: account.Role, CSRF: s.csrf(r)}
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
		if data.Role == "requester" && draft.Requester != data.Username {
			continue
		}
		if data.Role == "approver" && draft.ApprovalMode != "manual" {
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
	draft, ok := s.authorizedDraft(chi.URLParam(r, "draftID"), data)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, "task", map[string]any{"Session": data, "Draft": draft})
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
	if !ok || draft.Requester != data.Username || draft.State != "draft" {
		s.mu.Unlock()
		http.Error(w, "draft cannot be submitted", http.StatusConflict)
		return
	}
	draft.State = "pending"
	draft.UpdatedAt = s.config.Clock().UTC()
	snapshot := cloneDraft(draft)
	if draft.ApprovalMode == "auto" {
		draft.State = "approved"
		draft.UpdatedAt = s.config.Clock().UTC()
		snapshot = cloneDraft(draft)
	}
	s.mu.Unlock()

	s.dispatch(snapshot, "submitted", data.Username)
	if snapshot.State == "approved" {
		s.dispatch(snapshot, "approved", "oa-auto")
	}
	http.Redirect(w, r, "/tasks/"+draftID, http.StatusSeeOther)
}

func (s *Server) decideDraft(w http.ResponseWriter, r *http.Request) {
	data := r.Context().Value(sessionContextKey{}).(sessionData)
	if data.Role != "approver" {
		http.Error(w, "approver only", http.StatusForbidden)
		return
	}
	decision := strings.ToLower(r.Form.Get("decision"))
	if decision != "approved" && decision != "rejected" {
		http.Error(w, "invalid decision", http.StatusBadRequest)
		return
	}
	draftID := chi.URLParam(r, "draftID")
	s.mu.Lock()
	draft, ok := s.drafts[draftID]
	if !ok || draft.ApprovalMode != "manual" || draft.Approver != data.Username || draft.State != "pending" {
		s.mu.Unlock()
		http.Error(w, "draft cannot be decided", http.StatusConflict)
		return
	}
	draft.State = decision
	draft.UpdatedAt = s.config.Clock().UTC()
	snapshot := cloneDraft(draft)
	s.mu.Unlock()
	s.dispatch(snapshot, decision, data.Username)
	http.Redirect(w, r, "/tasks/"+draftID, http.StatusSeeOther)
}

func (s *Server) dispatch(draft Draft, status, actor string) {
	event := callbackEvent{
		EventID: randomID("evt"), TaskID: draft.TaskID, DraftID: draft.ID, Status: status,
		Actor: actor, OccurredAt: s.config.Clock().UTC(), CatalogVersion: draft.CatalogVersion,
		CallbackContext: draft.CallbackContext,
	}
	if status == "approved" || status == "rejected" {
		event.ApprovalReceipt = randomID("receipt")
	}
	go func() {
		for attempt := 1; attempt <= 3; attempt++ {
			if err := s.sendCallback(context.Background(), event); err == nil {
				return
			} else {
				s.config.Logger.Warn("OA callback failed", "event_id", event.EventID, "attempt", attempt, "error", err)
			}
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}()
}

func (s *Server) sendCallback(ctx context.Context, event callbackEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(s.config.Clock().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OA-Event-ID", event.EventID)
	req.Header.Set("X-OA-Timestamp", timestamp)
	req.Header.Set("X-OA-Signature", approval.Sign([]byte(s.config.CallbackSecret), timestamp, body))
	resp, err := s.config.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback status %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) authorizedDraft(id string, data sessionData) (Draft, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	draft, ok := s.drafts[id]
	if !ok || (data.Role == "requester" && draft.Requester != data.Username) || (data.Role == "approver" && (draft.ApprovalMode != "manual" || draft.Approver != data.Username)) {
		return Draft{}, false
	}
	return cloneDraft(draft), true
}

func cloneDraft(draft *Draft) Draft {
	copy := *draft
	copy.DataProducts = append([]string(nil), draft.DataProducts...)
	return copy
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

const pageTemplates = `
{{define "head"}}<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Task-bound OA Demo</title><style>
body{font-family:system-ui,sans-serif;background:#f4f6f8;color:#17202a;margin:0}.wrap{max-width:880px;margin:48px auto;padding:0 20px}.card{background:white;border:1px solid #dfe5eb;border-radius:12px;padding:24px;margin:16px 0;box-shadow:0 2px 10px #17202a0d}input,select,button{font:inherit;padding:10px 12px;border:1px solid #bcc7d1;border-radius:8px}button{background:#1769aa;color:white;border:0;cursor:pointer}.danger{background:#a92828}.muted{color:#617080}.tag{display:inline-block;padding:3px 8px;border-radius:99px;background:#e8f1f8}nav{display:flex;justify-content:space-between;align-items:center}form.inline{display:inline}dt{font-weight:600;margin-top:12px}dd{margin-left:0}.error{color:#a92828}a{color:#1769aa;text-decoration:none}</style></head><body><div class="wrap">{{end}}
{{define "foot"}}</div></body></html>{{end}}
{{define "login"}}{{template "head" .}}<div class="card"><h1>OA Demo 登录</h1><p class="muted">Alice 提交申请；Bob 审批明细申请。</p>{{if .Error}}<p class="error">用户名或密码错误</p>{{end}}<form method="post" action="/login"><input type="hidden" name="csrf" value="{{.CSRF}}"><p><label>用户<br><select name="username"><option value="alice">Alice</option><option value="bob">Bob</option></select></label></p><p><label>密码<br><input type="password" name="password" required autocomplete="current-password"></label></p><button type="submit">登录</button></form></div>{{template "foot" .}}{{end}}
{{define "nav"}}<nav><div><strong>Task-bound OA</strong> · {{.Username}} / {{.Role}}</div><form class="inline" method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit">退出</button></form></nav>{{end}}
{{define "tasks"}}{{template "head" .}}{{template "nav" .Session}}<h1>审批任务</h1>{{range .Drafts}}<div class="card"><span class="tag">{{.State}}</span><h2><a href="/tasks/{{.ID}}">{{.Objective}}</a></h2><p class="muted">{{.TaskID}} · {{.Sensitivity}} · {{.ApprovalMode}}</p></div>{{else}}<div class="card"><p>暂无任务。</p></div>{{end}}{{template "foot" .}}{{end}}
{{define "task"}}{{template "head" .}}{{template "nav" .Session}}<div class="card"><span class="tag">{{.Draft.State}}</span><h1>{{.Draft.Objective}}</h1><dl><dt>Task ID</dt><dd>{{.Draft.TaskID}}</dd><dt>数据产品</dt><dd>{{range .Draft.DataProducts}}{{.}} {{end}}</dd><dt>敏感级别</dt><dd>{{.Draft.Sensitivity}}</dd><dt>审批方式</dt><dd>{{.Draft.ApprovalMode}}</dd><dt>目录版本</dt><dd>{{.Draft.CatalogVersion}}</dd></dl>{{if and (eq .Session.Role "requester") (eq .Draft.State "draft")}}<form method="post" action="/tasks/{{.Draft.ID}}/submit"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><button type="submit">提交申请</button></form>{{end}}{{if and (eq .Session.Role "approver") (eq .Draft.State "pending")}}<form class="inline" method="post" action="/tasks/{{.Draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="approved"><button type="submit">批准</button></form> <form class="inline" method="post" action="/tasks/{{.Draft.ID}}/decision"><input type="hidden" name="csrf" value="{{.Session.CSRF}}"><input type="hidden" name="decision" value="rejected"><button class="danger" type="submit">拒绝</button></form>{{end}}</div><p><a href="/tasks">← 返回任务列表</a></p>{{template "foot" .}}{{end}}
`
