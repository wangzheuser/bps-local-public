package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/accounts"
	"github.com/sunboyzoey/GPT-BPS/internal/apikeys"
	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
	"github.com/sunboyzoey/GPT-BPS/internal/modelmap"
	"github.com/sunboyzoey/GPT-BPS/internal/gateway"
	"github.com/sunboyzoey/GPT-BPS/internal/modeltrace"
	"github.com/sunboyzoey/GPT-BPS/internal/probe"
	"github.com/sunboyzoey/GPT-BPS/internal/proxysettings"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// ProxyRuntime is the server-side hook that applies a new proxy immediately.
type ProxyRuntime interface {
	// ApplyProxy persists a validated proxy and rebuilds every account runtime.
	ApplyProxy(raw string) error
}

// Deps wires the admin handler to the rest of the server.
type Deps struct {
	DB        *store.DB
	Scheduler *gateway.Scheduler
	Accounts  *accounts.Service
	Proxy     *proxysettings.Service
	Runtime   ProxyRuntime
	Sealer    *apikeys.Sealer
	// Images reports inline-image staging usage; nil hides the panel.
	Images *basispoints.ImageRelay
	// Models caches model aliases; saving mappings invalidates it so edits take
	// effect on the next request. Nil disables invalidation.
	Models *modelmap.Cache
	// Probe runs model attribution; nil disables the capability panel.
	Probe *probe.Service
	Now   func() time.Time
}

// Handler serves /admin/* including the embedded Web UI.
type Handler struct {
	deps     Deps
	sessions *SessionManager
	mux      *http.ServeMux
	static   fs.FS
	now      func() time.Time
}

// NewHandler builds the admin handler.
func NewHandler(deps Deps, sessions *SessionManager, static fs.FS) *Handler {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	h := &Handler{deps: deps, sessions: sessions, static: static, now: now}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/api/login", h.handleLogin)
	mux.HandleFunc("POST /admin/api/logout", h.requireSession(h.handleLogout))
	mux.HandleFunc("GET /admin/api/session", h.handleSessionState)
	mux.HandleFunc("GET /admin/api/overview", h.requireSession(h.handleOverview))

	mux.HandleFunc("GET /admin/api/accounts", h.requireSession(h.handleListAccounts))
	mux.HandleFunc("POST /admin/api/accounts", h.requireSession(h.handleCreateAccount))
	mux.HandleFunc("POST /admin/api/accounts/{id}/authorization", h.requireSession(h.handleStartAuthorization))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}/authorization", h.requireSession(h.handleCancelAuthorization))
	mux.HandleFunc("POST /admin/api/accounts/{id}/enabled", h.requireSession(h.handleAccountEnabled))
	mux.HandleFunc("POST /admin/api/accounts/{id}/weight", h.requireSession(h.handleAccountWeight))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", h.requireSession(h.handleDeleteAccount))
	mux.HandleFunc("GET /admin/api/accounts/{id}/models", h.requireSession(h.handleAccountModels))
	mux.HandleFunc("POST /admin/api/accounts/{id}/test", h.requireSession(h.handleAccountTest))

	mux.HandleFunc("GET /admin/api/api-keys", h.requireSession(h.handleListKeys))
	mux.HandleFunc("POST /admin/api/api-keys", h.requireSession(h.handleCreateKey))
	mux.HandleFunc("POST /admin/api/api-keys/{id}/enabled", h.requireSession(h.handleKeyEnabled))
	mux.HandleFunc("POST /admin/api/api-keys/{id}/revoke", h.requireSession(h.handleKeyRevoke))
	mux.HandleFunc("POST /admin/api/api-keys/{id}/reveal", h.requireSession(h.handleRevealKey))
	mux.HandleFunc("DELETE /admin/api/api-keys/{id}", h.requireSession(h.handleDeleteKey))

	mux.HandleFunc("GET /admin/api/usage/summary", h.requireSession(h.handleUsageSummary))
	mux.HandleFunc("GET /admin/api/usage/requests", h.requireSession(h.handleUsageRequests))

	mux.HandleFunc("GET /admin/api/settings/proxy", h.requireSession(h.handleGetProxy))
	mux.HandleFunc("PUT /admin/api/settings/proxy", h.requireSession(h.handlePutProxy))
	mux.HandleFunc("POST /admin/api/settings/proxy/check", h.requireSession(h.handleCheckProxy))
	mux.HandleFunc("GET /admin/api/models", h.requireSession(h.handleGetModels))
	mux.HandleFunc("POST /admin/api/models/sync", h.requireSession(h.handleSyncModels))
	mux.HandleFunc("GET /admin/api/models/mappings", h.requireSession(h.handleGetMappings))
	mux.HandleFunc("PUT /admin/api/models/mappings", h.requireSession(h.handlePutMappings))
	mux.HandleFunc("GET /admin/api/capabilities", h.requireSession(h.handleGetCapabilities))
	mux.HandleFunc("POST /admin/api/capabilities/detect", h.requireSession(h.handleDetectCapabilities))
	mux.HandleFunc("POST /admin/api/capabilities/detect-one", h.requireSession(h.handleDetectOneCapability))
	mux.HandleFunc("PUT /admin/api/capabilities/schedule", h.requireSession(h.handleScheduleCapabilities))

	mux.HandleFunc("GET /admin/{$}", h.handleIndex)
	mux.HandleFunc("GET /admin/", h.serveStatic)
	h.mux = mux
	return h
}

// ServeHTTP applies session-independent routing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// requireSession enforces cookie + CSRF for API mutations.
func (h *Handler) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.sessions.Validate(r)
		if !ok {
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "管理员会话无效或已过期")
			return
		}
		if !h.sessions.CSRF(r, s) {
			writeAdminError(w, http.StatusForbidden, "csrf_invalid", "CSRF 校验失败")
			return
		}
		next(w, r)
	}
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	token, csrf, err := h.sessions.Login(body.Password, h.remoteIP(r))
	if err != nil {
		if errors.Is(err, errLocked) {
			writeAdminError(w, http.StatusTooManyRequests, "login_locked", err.Error())
			return
		}
		writeAdminError(w, http.StatusUnauthorized, "invalid_password", "密码错误")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   h.sessions.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": csrf})
}

func (h *Handler) handleSessionState(w http.ResponseWriter, r *http.Request) {
	s, ok := h.sessions.Validate(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": s.csrf})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.Logout(r)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/admin", HttpOnly: true,
		Secure: h.sessions.cookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	usage, err := h.deps.DB.UsageOverviewToday(dayStart)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "usage_query_failed", "统计查询失败")
		return
	}
	snapshot := h.deps.Scheduler.Snapshot()
	accountsTotal := len(snapshot)
	enabled, active, cooldown, authReq := 0, int64(0), 0, 0
	for _, rt := range snapshot {
		if rt.Enabled {
			enabled++
		}
		active += rt.ActiveRequests
		if rt.CooldownUntil > now.UnixMilli() {
			cooldown++
		}
		if rt.AuthRequired {
			authReq++
		}
	}
	keys, err := h.deps.DB.ListAPIKeys()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	keysEnabled := 0
	for _, k := range keys {
		if k.Enabled && k.RevokedAt == 0 {
			keysEnabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": map[string]any{
			"total": accountsTotal, "enabled": enabled, "active_requests": active,
			"cooldown": cooldown, "auth_required": authReq,
		},
		"api_keys": map[string]any{"total": len(keys), "enabled": keysEnabled},
		"image_relay": h.imageRelayView(),
		"usage_today": map[string]any{
			"requests": usage.Requests, "completed": usage.Completed, "failed": usage.Failed,
			"input_tokens": nullToJSON(usage.InputTokens), "output_tokens": nullToJSON(usage.OutputTokens),
			"total_tokens": nullToJSON(usage.TotalTokens), "avg_duration_ms": nullFloatToJSON(usage.AvgDurationMS),
		},
	})
}

// imageRelayView reports staging usage, or nil when the relay is disabled so
// the console can hide the panel instead of showing zeroes.
func (h *Handler) imageRelayView() any {
	if h.deps.Images == nil {
		return nil
	}
	return h.deps.Images.Stats()
}

// --- accounts ---

type accountView struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	UpstreamAccountID string  `json:"upstream_account_id"`
	PlanType          string  `json:"plan_type"`
	Enabled           bool    `json:"enabled"`
	Weight            int     `json:"weight"`
	Status            string  `json:"status"`
	TokenExpiresAt    int64   `json:"token_expires_at"`
	LastErrorCode     string  `json:"last_error_code"`
	CooldownUntil     int64   `json:"cooldown_until"`
	ActiveRequests    int64   `json:"active_requests"`
	CreatedAt         int64   `json:"created_at"`
	UpdatedAt         int64   `json:"updated_at"`
	Authorization     *accounts.AuthTaskView `json:"authorization,omitempty"`
	LastAuthError     string  `json:"last_auth_error,omitempty"`
}

func (h *Handler) accountViews() ([]accountView, error) {
	rows, err := h.deps.DB.ListAccounts()
	if err != nil {
		return nil, err
	}
	snapshot := map[string]gateway.RuntimeInfo{}
	for _, rt := range h.deps.Scheduler.Snapshot() {
		snapshot[rt.ID] = rt
	}
	now := h.now().UnixMilli()
	out := make([]accountView, 0, len(rows))
	for _, a := range rows {
		v := accountView{
			ID: a.ID, Name: a.Name,
			UpstreamAccountID: maskID(a.UpstreamAccountID),
			PlanType: a.PlanType, Enabled: a.Enabled, Weight: a.Weight,
			TokenExpiresAt: a.TokenExpiresAt, LastErrorCode: a.LastErrorCode,
			CooldownUntil: a.CooldownUntil, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		}
		if rt, ok := snapshot[a.ID]; ok {
			v.ActiveRequests = rt.ActiveRequests
			v.LastErrorCode = firstNonEmpty(rt.LastErrorCode, a.LastErrorCode)
			if rt.CooldownUntil > 0 {
				v.CooldownUntil = rt.CooldownUntil
			}
		}
		v.Status = h.accountStatus(a, snapshot[a.ID], now)
		if task := h.deps.Accounts.AuthorizationStatus(a.ID); task != nil {
			v.Authorization = task
		}
		v.LastAuthError = h.deps.Accounts.LastAuthError(a.ID)
		out = append(out, v)
	}
	return out, nil
}

func (h *Handler) accountStatus(a *store.Account, rt gateway.RuntimeInfo, nowMS int64) string {
	// A running task only means "waiting for the first authorization". Once the
	// account holds credentials, a leftover task must not mask the real state:
	// that is what made a successfully authorized account look stuck.
	if a.UpstreamAccountID == "" {
		if task := h.deps.Accounts.AuthorizationStatus(a.ID); task != nil && task.State == "running" {
			return "authorizing"
		}
	}
	if a.UpstreamAccountID == "" {
		if h.deps.Accounts.LastAuthError(a.ID) != "" {
			return "authorization_failed"
		}
		return "pending_authorization"
	}
	if rt.ID != "" && rt.AuthRequired {
		return "auth_required"
	}
	if rt.ID != "" && rt.CooldownUntil > nowMS {
		return "cooldown"
	}
	if a.Enabled {
		return "ready"
	}
	return "ready_disabled"
}

func maskID(id string) string {
	if id == "" {
		return ""
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	views, err := h.accountViews()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

func (h *Handler) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 100 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "账号名称必须为 1-100 个字符")
		return
	}
	a, err := h.deps.Accounts.Create(name)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "create_failed", "创建账号失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": a.ID, "name": a.Name})
}

func (h *Handler) handleStartAuthorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.deps.DB.GetAccount(id); err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	// An explicit {"reauthorize":true} body replaces existing credentials; a
	// plain call must not start a pointless task on an authorized account.
	var body struct {
		Reauthorize bool `json:"reauthorize"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)

	var task *accounts.AuthTaskView
	var err error
	if body.Reauthorize {
		task, err = h.deps.Accounts.Reauthorize(id)
	} else {
		task, err = h.deps.Accounts.StartAuthorization(id)
	}
	if err != nil {
		if errors.Is(err, accounts.ErrAlreadyAuthorized) {
			writeAdminError(w, http.StatusConflict, "already_authorized",
				"该账号已完成授权，无需重复授权；如需更换凭据请使用重新授权")
			return
		}
		writeAdminError(w, http.StatusInternalServerError, "authorization_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) handleCancelAuthorization(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := h.deps.Accounts.CancelAuthorization(id)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "没有进行中的授权任务")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) handleAccountEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Enabled == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	if err := h.deps.Accounts.SetEnabled(id, *body.Enabled); err != nil {
		writeAdminError(w, http.StatusConflict, "enable_failed", err.Error())
		return
	}
	if *body.Enabled {
		h.deps.Scheduler.SetEnabled(id, true)
		h.deps.Scheduler.ResetAuth(id)
	} else {
		h.deps.Scheduler.SetEnabled(id, false)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleAccountWeight(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Weight *int `json:"weight"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Weight == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	if err := h.deps.Accounts.SetWeight(id, *body.Weight); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_weight", err.Error())
		return
	}
	h.deps.Scheduler.SetWeight(id, *body.Weight)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteAccount permanently removes an account, its credential files and
// its runtime. Historical usage rows are kept without attribution.
func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.deps.Accounts.Delete(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, "not_found", "账号不存在")
			return
		}
		writeAdminError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAccountModels lists the models one specific account can use. Unlike
// /v1/models this is admin-authenticated and never consumes the scheduler.
func (h *Handler) handleAccountModels(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.deps.DB.GetAccount(id); err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	rt := h.deps.Scheduler.Get(id)
	if rt == nil || rt.Models == nil {
		writeAdminError(w, http.StatusConflict, "runtime_unavailable",
			"账号运行时未加载，请先完成授权并启用账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cat, err := rt.Models(ctx)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "models_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":          cat.Models,
		"default_model":   cat.DefaultModel,
		"plan_type":       cat.PlanType,
		"allowed":         cat.Allowed,
		"denial_reason":   cat.DenialReason,
		"restricted_models": cat.RestrictedModels,
	})
}

// handleAccountTest runs one real generation against a specific account and
// returns what the upstream actually produced.
func (h *Handler) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.deps.DB.GetAccount(id); err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Effort string `json:"effort"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请选择模型")
		return
	}
	prompt := body.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = "hi"
	}
	if len(prompt) > 32<<10 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "提示词过长")
		return
	}
	rt := h.deps.Scheduler.Get(id)
	if rt == nil {
		writeAdminError(w, http.StatusConflict, "runtime_unavailable",
			"账号运行时未加载，请先完成授权并启用账号")
		return
	}

	// Generations are slow; bound them but allow real thinking time.
	ctx, cancel := context.WithTimeout(r.Context(), 150*time.Second)
	defer cancel()
	res, err := rt.Generate(ctx, model, prompt, strings.TrimSpace(body.Effort))
	if err != nil {
		// Include the upstream's own terminal event so an operator can see the
		// real code and message instead of only "upstream reported error".
		detail := err.Error()
		if res != nil && res.RawTerminal != "" {
			detail += " | upstream terminal event: " + res.RawTerminal
		}
		writeAdminError(w, http.StatusBadGateway, "test_failed", detail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"requested_model": model,
		"reported_model": res.ReportedModel,
		"text":           res.Text,
		"reasoning":      res.Reasoning,
		"usage":          res.Usage,
		"latency_ms":     res.LatencyMS,
		"terminal":       res.Terminal,
	})
}

// --- models ---

// modelCatalogView is the persisted catalog as the console sees it.
type modelCatalogView struct {
	Catalog       json.RawMessage `json:"catalog"`
	SyncedAt      int64           `json:"synced_at"`
	SourceAccount string          `json:"source_account"`
}

type modelMappingView struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// handleGetModels returns the stored snapshot. A never-synced server reports
// catalog: null so the console can show a "sync first" prompt instead of an
// empty table.
func (h *Handler) handleGetModels(w http.ResponseWriter, r *http.Request) {
	cat, err := h.deps.DB.GetModelCatalog()
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, modelCatalogView{})
		return
	}
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, modelCatalogView{
		Catalog: json.RawMessage(cat.Payload), SyncedAt: cat.SyncedAt, SourceAccount: cat.SourceAccount,
	})
}

// handleSyncModels pulls the catalog from one schedulable account and stores it.
// A failed sync keeps the previous snapshot: losing the list because the
// upstream hiccuped would break /v1/models for every client.
func (h *Handler) handleSyncModels(w http.ResponseWriter, r *http.Request) {
	rt, err := h.deps.Scheduler.Pick("")
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "no_account_available",
			"没有可用的账号，请先启用至少一个已授权账号")
		return
	}
	defer h.deps.Scheduler.Release(rt)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cat, err := rt.Models(ctx)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "sync_failed", err.Error())
		return
	}

	// Store the OpenAI-compatible shape so /v1/models can serve it verbatim.
	now := h.now().Unix()
	data := make([]map[string]any, 0, len(cat.Models))
	for _, m := range cat.Models {
		efforts := make([]map[string]string, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			efforts = append(efforts, map[string]string{"value": e.Value, "label": e.Label})
		}
		data = append(data, map[string]any{
			"id": m.ID, "object": "model", "created": now, "owned_by": "basispoints",
			"label": m.Label, "default_effort": m.DefaultEffort, "efforts": efforts,
		})
	}
	payload, err := json.Marshal(map[string]any{
		"object":            "list",
		"data":              data,
		"default_model":     cat.DefaultModel,
		"plan_type":         cat.PlanType,
		"source":            cat.Source,
		"allowed":           cat.Allowed,
		"denial_reason":     cat.DenialReason,
		"restricted_models": cat.RestrictedModels,
	})
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "encode_failed", "序列化模型目录失败")
		return
	}
	if err := h.deps.DB.SaveModelCatalog(payload, rt.Name); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", "保存模型目录失败")
		return
	}
	stored, err := h.deps.DB.GetModelCatalog()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, modelCatalogView{
		Catalog: json.RawMessage(stored.Payload), SyncedAt: stored.SyncedAt, SourceAccount: stored.SourceAccount,
	})
}

func (h *Handler) handleGetMappings(w http.ResponseWriter, r *http.Request) {
	rows, err := h.deps.DB.ListModelMappings()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	out := make([]modelMappingView, 0, len(rows))
	for _, m := range rows {
		out = append(out, modelMappingView{Source: m.SourceModel, Target: m.TargetModel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": out})
}

// handlePutMappings replaces the whole mapping set. The console edits it as a
// unit, so validation happens here before anything is written.
func (h *Handler) handlePutMappings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mappings []modelMappingView `json:"mappings"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	if len(body.Mappings) > 512 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "映射数量过多")
		return
	}
	seen := make(map[string]struct{}, len(body.Mappings))
	pairs := make([]store.ModelMapping, 0, len(body.Mappings))
	for _, m := range body.Mappings {
		source := strings.TrimSpace(m.Source)
		target := strings.TrimSpace(m.Target)
		if source == "" || target == "" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "映射的源模型与目标模型都必须填写")
			return
		}
		if len(source) > 200 || len(target) > 200 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "模型名称过长")
			return
		}
		// The target is sent upstream verbatim, so a wildcard there could never
		// resolve to a real model. Reject it instead of failing the request later.
		if modelmap.HasWildcard(target) {
			writeAdminError(w, http.StatusBadRequest, "invalid_request",
				"目标模型不能包含通配符，请填写具体的模型名称："+target)
			return
		}
		if source == target {
			// An identity mapping would only add noise; the console filters
			// these too, but a direct API call must not smuggle one in.
			continue
		}
		if _, dup := seen[source]; dup {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "同一个源模型只能配置一条映射："+source)
			return
		}
		seen[source] = struct{}{}
		pairs = append(pairs, store.ModelMapping{SourceModel: source, TargetModel: target})
	}
	if err := h.deps.DB.ReplaceModelMappings(pairs); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "save_failed", "保存映射失败")
		return
	}
	// Make the new rules visible to the very next request.
	h.deps.Models.Invalidate()
	out := make([]modelMappingView, 0, len(pairs))
	for _, m := range pairs {
		out = append(out, modelMappingView{Source: m.SourceModel, Target: m.TargetModel})
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": out})
}

// --- capabilities ---

type capabilityCell struct {
	Account string  `json:"account"`
	Model   string  `json:"model"`
	Top     string  `json:"top"`
	SelfProb float64 `json:"self_prob"`
	Verified bool   `json:"verified"`
	CheckedAt int64 `json:"checked_at"`
	Error   string  `json:"error,omitempty"`
}

// handleGetCapabilities returns the measured model/account matrix plus probe
// scheduling state. When probing is unavailable the payload says why so the
// console can show setup guidance instead of an empty grid.
func (h *Handler) handleGetCapabilities(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"available": false, "cells": []capabilityCell{}}
	if h.deps.Probe != nil {
		ok, err := h.deps.Probe.Available()
		resp["available"] = ok
		if !ok && err != nil {
			resp["unavailable_reason"] = err.Error()
		}
		resp["status"] = h.deps.Probe.Status()
	} else {
		resp["unavailable_reason"] = "attribution service is not configured"
	}

	rows, err := h.deps.DB.ListAccountModels()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	names := map[string]string{}
	if accounts, err := h.deps.DB.ListAccounts(); err == nil {
		for _, a := range accounts {
			names[a.ID] = a.Name
		}
	}
	cells := make([]capabilityCell, 0, len(rows))
	for _, row := range rows {
		name := names[row.AccountID]
		if name == "" {
			name = row.AccountID
		}
		cells = append(cells, capabilityCell{
			Account: name, Model: row.Model, Top: row.TopModel,
			SelfProb: row.SelfProb, Verified: row.Verified,
			CheckedAt: row.CheckedAt, Error: row.LastError,
		})
	}
	resp["cells"] = cells

	interval, lastRun, challenges, err := h.deps.DB.GetProbeSchedule()
	if err == nil {
		resp["interval_minutes"] = interval
		resp["last_run"] = lastRun
		resp["challenges"] = challenges
		resp["max_challenges"] = modeltrace.MaxChallenges
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDetectCapabilities starts a full probe run in the background.
func (h *Handler) handleDetectCapabilities(w http.ResponseWriter, r *http.Request) {
	if h.deps.Probe == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "probe_unavailable",
			"归因检测未配置，请设置 BPS_MODELTRACE_BANK 指向指纹库")
		return
	}
	status, err := h.deps.Probe.Start()
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "probe_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleDetectOneCapability re-measures a single account/model pair. The request
// is synchronous because one pair takes tens of seconds, which the console shows
// as a disabled per-row button rather than as a background run.
func (h *Handler) handleDetectOneCapability(w http.ResponseWriter, r *http.Request) {
	if h.deps.Probe == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "probe_unavailable",
			"归因检测未配置，请设置 BPS_MODELTRACE_BANK 指向指纹库")
		return
	}
	var body struct {
		AccountID string `json:"account_id"`
		Account   string `json:"account"`
		Model     string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	accountID := strings.TrimSpace(body.AccountID)
	// The console knows the account name, not its id, so accept either.
	if accountID == "" {
		name := strings.TrimSpace(body.Account)
		if name == "" {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "请指定账号")
			return
		}
		accounts, err := h.deps.DB.ListAccounts()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
			return
		}
		for _, a := range accounts {
			if a.Name == name {
				accountID = a.ID
				break
			}
		}
		if accountID == "" {
			writeAdminError(w, http.StatusNotFound, "not_found", "账号不存在")
			return
		}
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请指定模型")
		return
	}

	// Bound the probe: one pair with retries can legitimately run a few minutes.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	row, err := h.deps.Probe.ProbeOne(ctx, accountID, model)
	if err != nil {
		writeAdminError(w, http.StatusBadGateway, "probe_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, capabilityCell{
		Account: accountID, Model: row.Model, Top: row.TopModel,
		SelfProb: row.SelfProb, Verified: row.Verified,
		CheckedAt: row.CheckedAt, Error: row.LastError,
	})
}

// handleScheduleCapabilities persists the scheduled probing interval and how
// many challenges each model gets.
func (h *Handler) handleScheduleCapabilities(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IntervalMinutes *int `json:"interval_minutes"`
		Challenges      *int `json:"challenges"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	if body.IntervalMinutes == nil && body.Challenges == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "没有需要保存的字段")
		return
	}
	out := map[string]any{}
	if body.IntervalMinutes != nil {
		minutes := *body.IntervalMinutes
		if minutes < 0 || minutes > 7*24*60 {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "间隔必须在 0 到 10080 分钟之间")
			return
		}
		if err := h.deps.DB.SetProbeInterval(minutes); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "save_failed", "保存失败")
			return
		}
		out["interval_minutes"] = minutes
	}
	if body.Challenges != nil {
		n := *body.Challenges
		// Below 1 there is nothing to attribute; above 3 buys nothing because
		// the bank has no calibration beyond three probes.
		if n < 1 || n > modeltrace.MaxChallenges {
			writeAdminError(w, http.StatusBadRequest, "invalid_request",
				"挑战次数必须在 1 到 3 之间")
			return
		}
		if err := h.deps.DB.SetProbeChallenges(n); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "save_failed", "保存失败")
			return
		}
		out["challenges"] = n
	}
	writeJSON(w, http.StatusOK, out)
}

// --- api keys ---

type apiKeyView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Enabled    bool   `json:"enabled"`
	Revoked    bool   `json:"revoked"`
	Expired    bool   `json:"expired"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt int64  `json:"last_used_at"`
	CreatedAt  int64  `json:"created_at"`
	// Revealable tells the console whether a copy button can work. It leaks
	// only the presence of ciphertext, never the key itself.
	Revealable bool `json:"revealable"`
}

func keyViews(rows []*store.APIKey, now time.Time) []apiKeyView {
	out := make([]apiKeyView, 0, len(rows))
	for _, k := range rows {
		out = append(out, apiKeyView{
			ID: k.ID, Name: k.Name, Prefix: k.Prefix,
			Enabled: k.Enabled, Revoked: k.RevokedAt != 0,
			Expired: k.ExpiresAt != 0 && k.ExpiresAt <= now.UnixMilli(),
			ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt,
			Revealable: k.KeySealed != "",
		})
	}
	return out
}

// handleRevealKey returns the plaintext of one saved key. It is POST-only so a
// link, prefetch or cross-site navigation cannot pull a key out of the console.
func (h *Handler) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	k, err := h.deps.DB.GetAPIKey(id)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	plain, err := h.deps.Sealer.Open(k.KeySealed)
	if err != nil {
		writeAdminError(w, http.StatusConflict, "reveal_unavailable",
			"该 Key 创建于加密存储启用之前，无法恢复明文；请重新创建并保存")
		return
	}
	// Never let a browser or proxy cache key material.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"id": k.ID, "key": plain})
}

func (h *Handler) handleListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.deps.DB.ListAPIKeys()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keyViews(rows, h.now())})
}

func (h *Handler) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 100 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Key 名称必须为 1-100 个字符")
		return
	}
	gen, err := apikeys.Generate()
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "generate_failed", "生成 Key 失败")
		return
	}
	id, err := apikeys.NewID("key_")
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "generate_failed", "生成 Key 失败")
		return
	}
	if body.ExpiresAt != 0 && body.ExpiresAt < h.now().UnixMilli() {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "过期时间必须晚于当前时间")
		return
	}
	rec := &store.APIKey{
		ID: id, Name: name, Prefix: gen.DisplayPrefix, KeyHash: gen.Hash,
		Enabled: true, ExpiresAt: body.ExpiresAt,
	}
	// Keep an encrypted copy so the console can reveal the key later; the
	// database alone never holds the plaintext. A sealing failure is surfaced
	// instead of silently producing a key that can never be copied again.
	sealed, err := h.deps.Sealer.Seal(gen.Plaintext)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "seal_failed",
			"无法加密保存 Key，请检查数据目录权限")
		return
	}
	rec.KeySealed = sealed
	if err := h.deps.DB.CreateAPIKey(rec); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "create_failed", "保存 Key 失败")
		return
	}
	// The plaintext appears exactly once, in this response.
	writeJSON(w, http.StatusOK, map[string]any{
		"key":      gen.Plaintext,
		"key_view": keyViews([]*store.APIKey{rec}, h.now())[0],
	})
}

func (h *Handler) handleKeyEnabled(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Enabled == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	k, err := h.deps.DB.GetAPIKey(id)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	if k.RevokedAt != 0 {
		writeAdminError(w, http.StatusConflict, "revoked", "已撤销的 Key 不能重新启用")
		return
	}
	if err := h.deps.DB.SetAPIKeyEnabled(id, *body.Enabled); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "update_failed", "更新失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.deps.DB.GetAPIKey(id); err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	if err := h.deps.DB.RevokeAPIKey(id); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "update_failed", "更新失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteKey removes a key record permanently. Historical usage rows keep
// their aggregates but lose the key attribution.
func (h *Handler) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.deps.DB.DeleteAPIKey(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminError(w, http.StatusNotFound, "not_found", "Key 不存在")
			return
		}
		writeAdminError(w, http.StatusInternalServerError, "delete_failed", "删除失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- usage ---

func parseTimeParam(value string, now time.Time) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, true
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n, true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func (h *Handler) usageFilters(r *http.Request) (store.UsageFilters, string, bool) {
	q := r.URL.Query()
	now := h.now()
	f := store.UsageFilters{}
	ok := true
	f.From, ok = parseTimeParam(q.Get("from"), now)
	if !ok {
		return f, "", false
	}
	f.To, ok = parseTimeParam(q.Get("to"), now)
	if !ok {
		return f, "", false
	}
	f.AccountID = strings.TrimSpace(q.Get("account_id"))
	f.APIKeyID = strings.TrimSpace(q.Get("api_key_id"))
	f.Model = strings.TrimSpace(q.Get("model"))
	if f.Limit, ok = parsePositiveInt(q.Get("limit"), 50, 200); !ok {
		return f, "", false
	}
	if f.Offset, ok = parseInt(q.Get("offset"), 0); !ok || f.Offset < 0 {
		return f, "", false
	}
	return f, strings.TrimSpace(q.Get("group_by")), true
}

func parsePositiveInt(v string, def, max int) (int, bool) {
	if strings.TrimSpace(v) == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

func parseInt(v string, def int) (int, bool) {
	if strings.TrimSpace(v) == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (h *Handler) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	f, groupBy, ok := h.usageFilters(r)
	if !ok {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "查询参数无效")
		return
	}
	if groupBy == "" {
		groupBy = "day"
	}
	rows, err := h.deps.DB.UsageSummary(f, groupBy)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "group_by 仅支持 day、account、api_key、model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": summaryToJSON(rows)})
}

func (h *Handler) handleUsageRequests(w http.ResponseWriter, r *http.Request) {
	f, _, ok := h.usageFilters(r)
	if !ok {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "查询参数无效")
		return
	}
	rows, total, err := h.deps.DB.ListUsageRequests(f)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "db_query_failed", "查询失败")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, map[string]any{
			"id": u.ID,
			"api_key_id": nullStr(u.APIKeyID), "account_id": nullStr(u.AccountID),
			"requested_model": u.RequestedModel, "reported_model": u.ReportedModel,
			"reasoning_effort": u.ReasoningEffort, "status": u.Status,
			"http_status": u.HTTPStatus, "upstream_status": u.UpstreamStatus,
			"terminal_event": u.TerminalEvent, "error_code": u.ErrorCode,
			"input_tokens": nullToJSON(u.InputTokens), "output_tokens": nullToJSON(u.OutputTokens),
			"total_tokens": nullToJSON(u.TotalTokens), "cached_tokens": nullToJSON(u.CachedTokens),
			"reasoning_tokens": nullToJSON(u.ReasoningTokens),
			"started_at": u.StartedAt, "finished_at": u.FinishedAt, "duration_ms": u.DurationMS,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func summaryToJSON(rows []*store.SummaryRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"label": r.Label, "requests": r.Requests, "completed": r.Completed,
			"incomplete": r.Incomplete, "failed": r.Failed,
			"input_tokens": nullToJSON(r.InputTokens), "output_tokens": nullToJSON(r.OutputTokens),
			"total_tokens": nullToJSON(r.TotalTokens), "avg_duration_ms": nullFloatToJSON(r.AvgDuration),
		})
	}
	return out
}

func nullToJSON(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullFloatToJSON(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

func nullStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- proxy settings ---

func (h *Handler) handleGetProxy(w http.ResponseWriter, r *http.Request) {
	if h.deps.Proxy == nil {
		writeAdminError(w, http.StatusNotImplemented, "proxy_unavailable", "代理设置不可用")
		return
	}
	raw := h.deps.Proxy.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy_url":       raw,
		"proxy_display":   proxysettings.Redacted(raw),
		"has_credentials": strings.Contains(raw, "@"),
	})
}

func (h *Handler) handlePutProxy(w http.ResponseWriter, r *http.Request) {
	if h.deps.Runtime == nil || h.deps.Proxy == nil {
		writeAdminError(w, http.StatusNotImplemented, "proxy_unavailable", "代理设置不可用")
		return
	}
	var body struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "请求体无效")
		return
	}
	raw := strings.TrimSpace(body.ProxyURL)
	if err := proxysettings.ValidateProxyURL(raw); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		return
	}
	if err := h.deps.Runtime.ApplyProxy(raw); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "apply_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"proxy_url":     h.deps.Proxy.Get(),
		"proxy_display": proxysettings.Redacted(h.deps.Proxy.Get()),
	})
}

// handleCheckProxy probes the submitted proxy without saving it. An empty body
// or empty proxy_url checks the currently active value.
func (h *Handler) handleCheckProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProxyURL *string `json:"proxy_url"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	raw := ""
	if body.ProxyURL != nil {
		raw = strings.TrimSpace(*body.ProxyURL)
	} else if h.deps.Proxy != nil {
		raw = h.deps.Proxy.Get()
	}
	result := proxysettings.Check(raw, 20*time.Second)
	writeJSON(w, http.StatusOK, result)
}

// --- static assets ---

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	// The shell page is static and contains no secrets; the embedded JS
	// renders the login view when the session is missing. API endpoints stay
	// session-protected.
	serveEmbedded(w, r, h.static, "index.html")
}

func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
		writeAdminError(w, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	serveEmbedded(w, r, h.static, name)
}

func serveEmbedded(w http.ResponseWriter, r *http.Request, static fs.FS, name string) {
	data, err := fs.ReadFile(static, name)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	switch {
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}



