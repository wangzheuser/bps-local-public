package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/apikeys"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// Gateway is the managed Responses API front end.
type Gateway struct {
	db   *store.DB
	sched *Scheduler
	newID func(prefix string) (string, error)
	now   func() time.Time
}

// NewGateway builds the gateway.
func NewGateway(db *store.DB, sched *Scheduler) *Gateway {
	return &Gateway{db: db, sched: sched, newID: apikeys.NewID, now: time.Now}
}

// APIHandler assembles the client-facing routes. /healthz is unauthenticated;
// responses routes require an API key.
func (g *Gateway) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.Handle("/v1/responses", g.apiKeyAuth(http.HandlerFunc(g.handleResponses)))
	mux.Handle("/responses", g.apiKeyAuth(http.HandlerFunc(g.handleResponses)))
	mux.Handle("/v1/models", g.apiKeyAuth(http.HandlerFunc(g.handleModels)))
	return mux
}

// handleModels lists the models the selected account may use. The catalog is
// read from upstream, so it reflects the account's real entitlements rather
// than a hardcoded list.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	rt, err := g.sched.Pick(strings.TrimSpace(r.Header.Get("session_id")))
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "no_account_available",
			"no eligible BPS account is available; enable an account or wait for cooldown to end")
		return
	}
	defer g.sched.Release(rt)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cat, err := rt.Models(ctx)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "basispoints_models_unavailable", err.Error())
		return
	}

	now := g.now().Unix()
	data := make([]map[string]any, 0, len(cat.Models))
	for _, m := range cat.Models {
		efforts := make([]map[string]string, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			efforts = append(efforts, map[string]string{"value": e.Value, "label": e.Label})
		}
		data = append(data, map[string]any{
			"id":             m.ID,
			"object":         "model",
			"created":        now,
			"owned_by":       "basispoints",
			"label":          m.Label,
			"default_effort": m.DefaultEffort,
			"efforts":        efforts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":          "list",
		"data":            data,
		"default_model":   cat.DefaultModel,
		"plan_type":       cat.PlanType,
		"source":          cat.Source,
		"allowed":         cat.Allowed,
		"denial_reason":   cat.DenialReason,
		"restricted_models": cat.RestrictedModels,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := g.db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"ok":false}`)
		return
	}
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": "invalid_request_error", "code": code, "message": message},
	})
}

// apiKeyAuth validates the Bearer key and attaches its id to the context.
func (g *Gateway) apiKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
			writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "missing bearer API key")
			return
		}
		key := strings.TrimSpace(header[len("Bearer "):])
		if !apikeys.ValidFormat(key) {
			writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
			return
		}
		rec, err := g.db.GetAPIKeyByHash(apikeys.Hash(key))
		if err != nil || rec == nil || !apikeys.Matches(key, rec.KeyHash) {
			writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid API key")
			return
		}
		now := g.now()
		if !rec.Enabled || rec.RevokedAt != 0 ||
			(rec.ExpiresAt != 0 && rec.ExpiresAt <= now.UnixMilli()) {
			writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "API key is disabled, revoked or expired")
			return
		}
		ctx := context.WithValue(r.Context(), apiKeyIDKeyType{}, rec.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type apiKeyIDKeyType struct{}

func apiKeyIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(apiKeyIDKeyType{}).(string)
	return v
}

// handleResponses schedules an account, proxies the request and records usage.
func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := g.now()
	rec := &usageRecord{ID: newIDOrPanic(g), startedAt: start}
	if keyID := apiKeyIDFromContext(r.Context()); keyID != "" {
		rec.APIKeyID = keyID
	}
	ctx := context.WithValue(r.Context(), recordKeyType{}, rec)
	r = r.WithContext(ctx)

	sessionID := strings.TrimSpace(r.Header.Get("session_id"))
	rt, err := g.sched.Pick(sessionID)
	if err != nil {
		rec.mu.Lock()
		rec.httpStatus = http.StatusServiceUnavailable
		rec.errorCode = "no_account_available"
		rec.mu.Unlock()
		g.finishRequest(rec, rt, r, w)
		writeAPIError(w, http.StatusServiceUnavailable, "no_account_available",
			"no eligible BPS account is available; enable an account or wait for cooldown to end")
		return
	}
	defer g.sched.Release(rt)

	tw := newTrackingWriter(w, rec)
	rt.Handler.ServeHTTP(tw, r)
	g.finishRequest(rec, rt, r, w)
}

func newIDOrPanic(g *Gateway) string {
	id, err := g.newID("req_")
	if err != nil {
		panic(err) // crypto/rand failure terminates per Go contract
	}
	return id
}

// finishRequest classifies the outcome, updates scheduling state and persists
// the usage row. It never alters bytes already delivered to the client.
func (g *Gateway) finishRequest(rec *usageRecord, rt *Runtime, r *http.Request, w http.ResponseWriter) {
	finished := g.now()
	rec.finishJSONError()

	rec.mu.Lock()
	status := rec.httpStatus
	terminal := rec.terminal
	errorCode := rec.errorCode
	retryAfter := rec.retryAfterSec
	inputTokens, outputTokens, totalTokens := rec.inputTokens, rec.outputTokens, rec.totalTokens
	cachedTokens, reasoningTokens := rec.cachedTokens, rec.reasoningTokens
	model, effort, reportedModel := rec.model, rec.effort, rec.reportedModel
	rec.mu.Unlock()

	// Classify.
	outcome := "unknown"
	clientClosed := r.Context().Err() != nil
	switch {
	case terminal == "response.completed":
		outcome = "completed"
	case terminal == "response.incomplete":
		outcome = "incomplete"
	case terminal == "response.failed" || terminal == "error":
		outcome = "failed"
	case status >= 400:
		outcome = "http_error"
	case clientClosed:
		outcome = "client_closed"
	case status == http.StatusOK:
		outcome = "upstream_eof"
	}

	// Scheduling updates.
	if rt != nil {
		switch {
		case outcome == "completed" || outcome == "incomplete":
			if g.sched.MarkSuccess(rt) {
				_ = g.db.ClearAccountError(rt.ID)
			}
		case outcome == "http_error" && status == http.StatusUnauthorized:
			code := errorCode
			if code == "" {
				code = "basispoints_auth_error"
			}
			g.sched.MarkAuthFailure(rt, code)
			_ = g.db.SetAccountError(rt.ID, code, 0)
		case outcome == "http_error" && status == http.StatusTooManyRequests:
			code := errorCode
			if code == "" {
				code = "basispoints_rate_limited"
			}
			wait := time.Duration(retryAfter) * time.Second
			g.sched.MarkCooldown(rt, wait, code)
			_ = g.db.SetAccountError(rt.ID, code, g.now().Add(wait).UnixMilli())
		case outcome == "http_error" || outcome == "upstream_eof":
			code := errorCode
			if code == "" {
				code = "basispoints_transport_error"
			}
			g.sched.MarkNetworkFailure(rt, code)
			_ = g.db.SetAccountError(rt.ID, code, g.now().UnixMilli())
		case outcome == "failed" && isNetworkErrorCode(errorCode):
			g.sched.MarkNetworkFailure(rt, errorCode)
			_ = g.db.SetAccountError(rt.ID, errorCode, g.now().UnixMilli())
		case outcome == "client_closed":
			// No penalty; the upstream may still finish.
		}
	}

	// Persist usage. Failures are logged, never surfaced into the response.
	upstreamStatus := 0
	if outcome == "http_error" && status >= 400 {
		// Recover the upstream status from the error code message captured by
		// the tracking writer when available.
		rec.mu.Lock()
		upstreamStatus = rec.upstreamStatus
		rec.mu.Unlock()
	}
	row := &store.UsageRequest{
		ID:              rec.ID,
		RequestedModel:  model,
		ReportedModel:   reportedModel,
		ReasoningEffort: effort,
		Status:          outcome,
		HTTPStatus:      status,
		UpstreamStatus:  upstreamStatus,
		TerminalEvent:   terminal,
		ErrorCode:       errorCode,
		InputTokens:     toNullInt64(inputTokens),
		OutputTokens:    toNullInt64(outputTokens),
		TotalTokens:     toNullInt64(totalTokens),
		CachedTokens:    toNullInt64(cachedTokens),
		ReasoningTokens: toNullInt64(reasoningTokens),
		StartedAt:       rec.startedAt.UnixMilli(),
		FinishedAt:      finished.UnixMilli(),
		DurationMS:      finished.Sub(rec.startedAt).Milliseconds(),
	}
	if rec.APIKeyID != "" {
		keyID := rec.APIKeyID
		row.APIKeyID = &keyID
	}
	if rt != nil {
		accountID := rt.ID
		row.AccountID = &accountID
	}
	if err := g.db.InsertUsageRequest(row); err != nil {
		log.Printf("[bps-server] usage record failed: %v (request %s outcome %s)", err, rec.ID, outcome)
	}
	if rec.APIKeyID != "" {
		_ = g.db.TouchAPIKey(rec.APIKeyID, finished.UnixMilli())
	}
}

func isNetworkErrorCode(code string) bool {
	switch code {
	case "basispoints_ws_timeout", "basispoints_ws_disconnected", "basispoints_ws_draining", "basispoints_transport_error":
		return true
	}
	return false
}

func toNullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: *p}
}
