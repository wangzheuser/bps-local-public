package admin

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/accounts"
	"github.com/sunboyzoey/GPT-BPS/internal/apikeys"
	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
	"github.com/sunboyzoey/GPT-BPS/internal/modelmap"
	"github.com/sunboyzoey/GPT-BPS/internal/gateway"
	"github.com/sunboyzoey/GPT-BPS/internal/proxysettings"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

func staticFS() fs.FS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")},
		"app.js":     &fstest.MapFile{Data: []byte("//js")},
		"style.css":  &fstest.MapFile{Data: []byte("body{}")},
	}
}

type testEnv struct {
	t       *testing.T
	handler *Handler
	db      *store.DB
	sched   *gateway.Scheduler
	svc     *accounts.Service
	proxy   *proxysettings.Service
	csrf    string
	cookie  string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	sched := gateway.NewScheduler()
	svc, err := accounts.NewService(db, filepath.Join(t.TempDir(), "accounts"), func() string { return "" }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessionManager("test-password", time.Hour, false)
	sealer, err := apikeys.OpenSealer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Deps{DB: db, Scheduler: sched, Accounts: svc, Sealer: sealer, Now: time.Now}, sessions, staticFS())
	return &testEnv{t: t, handler: handler, db: db, sched: sched, svc: svc}
}

func (e *testEnv) login() {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"password":"test-password"}`))
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login status %d body %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		e.t.Fatal(err)
	}
	e.csrf = doc.CSRF
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			e.t.Helper()
			e.cookie = c.Value
		}
	}
}

func (e *testEnv) request(method, path, body string, withAuth bool) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withAuth && e.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: e.cookie})
	}
	if withAuth && e.csrf != "" && method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", e.csrf)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestLoginLogoutFlow(t *testing.T) {
	e := newTestEnv(t)
	rec := e.request(http.MethodPost, "/admin/api/login", `{"password":"wrong"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status %d", rec.Code)
	}
	e.login()
	rec = e.request(http.MethodGet, "/admin/api/overview", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status %d", rec.Code)
	}
	rec = e.request(http.MethodPost, "/admin/api/logout", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/admin/api/overview", "", true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatal("session must be gone after logout")
	}
}

func TestCSRFRequiredForMutations(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	// Drop the CSRF header for a mutation: must fail.
	saved := e.csrf
	e.csrf = ""
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"x"}`, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("mutation without csrf status %d", rec.Code)
	}
	e.csrf = saved
}

func TestInvalidSessionRejected(t *testing.T) {
	e := newTestEnv(t)
	e.cookie = "bogus"
	e.csrf = "bogus"
	rec := e.request(http.MethodGet, "/admin/api/overview", "", true)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus session status %d", rec.Code)
	}
}

func TestAPIKeyCreateShownOnce(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"codex"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status %d body %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Key     string         `json:"key"`
		KeyView map[string]any `json:"key_view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.Key, apikeys.Prefix) || len(doc.Key) != len(apikeys.Prefix)+43 {
		t.Fatalf("plaintext key format: %q", doc.Key[:12])
	}
	if doc.KeyView["key_hash"] != nil {
		t.Fatal("hash must not be exposed")
	}
	// The key list must not contain the plaintext.
	rec = e.request(http.MethodGet, "/admin/api/api-keys", "", true)
	if strings.Contains(rec.Body.String(), doc.Key) {
		t.Fatal("plaintext key leaked in list")
	}
}

func TestAPIKeyRevokeBlocksReenable(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"k"}`, true)
	var doc struct {
		KeyView struct {
			ID string `json:"id"`
		} `json:"key_view"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	rec = e.request(http.MethodPost, "/admin/api/api-keys/"+doc.KeyView.ID+"/revoke", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status %d", rec.Code)
	}
	rec = e.request(http.MethodPost, "/admin/api/api-keys/"+doc.KeyView.ID+"/enabled", `{"enabled":true}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-enable revoked key status %d", rec.Code)
	}
}

func TestAccountLifecycleEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/accounts", `{"name":"账号 A"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create account status %d", rec.Code)
	}
	var doc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)

	rec = e.request(http.MethodGet, "/admin/api/accounts", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pending_authorization") {
		t.Fatalf("accounts list: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), doc.ID+`-v1.json`) {
		t.Fatal("credential path must not be exposed")
	}

	// Enable before authorization must fail with a clear message.
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+doc.ID+"/enabled", `{"enabled":true}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("enable unauthorized status %d", rec.Code)
	}

	// Weight bounds.
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+doc.ID+"/weight", `{"weight":0}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad weight status %d", rec.Code)
	}
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+doc.ID+"/weight", `{"weight":7}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid weight status %d", rec.Code)
	}
}

func TestUsageEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	acct := &store.Account{ID: "acct_u", Name: "U", CredentialPath: "/tmp/u.json", Weight: 1}
	if err := e.db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}
	acctID := "acct_u"
	u := &store.UsageRequest{
		ID: "req_u", AccountID: &acctID, RequestedModel: "gpt-6-astra",
		Status: "completed", HTTPStatus: 200, TerminalEvent: "response.completed",
		StartedAt: time.Now().Add(-time.Minute).UnixMilli(), FinishedAt: time.Now().UnixMilli(),
		DurationMS: 500,
	}
	u.InputTokens.Valid, u.InputTokens.Int64 = true, 10
	if err := e.db.InsertUsageRequest(u); err != nil {
		t.Fatal(err)
	}

	rec := e.request(http.MethodGet, "/admin/api/usage/summary?group_by=model", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gpt-6-astra") {
		t.Fatalf("summary: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.request(http.MethodGet, "/admin/api/usage/summary?group_by=bogus", "", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus group status %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/admin/api/usage/requests?limit=10", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"total":1`) {
		t.Fatalf("requests: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEnableSetsSchedulerRuntimeEnabled(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	// Seed an authorized account with a loaded runtime.
	acct := &store.Account{
		ID: "acct_en", Name: "EN", CredentialPath: "/tmp/en.json",
		UpstreamAccountID: "up-en", Weight: 1,
	}
	if err := e.db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}
	rt := &gateway.Runtime{ID: acct.ID, Name: acct.Name, Store: testAuthStore(t)}
	e.sched.Upsert(rt)

	rec := e.request(http.MethodPost, "/admin/api/accounts/"+acct.ID+"/enabled", `{"enabled":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	// The runtime flag is what actually decides scheduling; forgetting it made
	// enabled accounts unreachable.
	if !rt.Enabled.Load() {
		t.Fatal("runtime Enabled flag not set: account would never be scheduled")
	}
	if e.sched.EligibleCount() != 1 {
		t.Fatalf("eligible = %d, want 1", e.sched.EligibleCount())
	}
}

func TestAuthorizationRejectsAlreadyAuthorizedAccount(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	acct := &store.Account{
		ID: "acct_auth", Name: "AUTH", CredentialPath: "/tmp/auth.json",
		UpstreamAccountID: "up-auth", Weight: 1,
	}
	if err := e.db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}
	rec := e.request(http.MethodPost, "/admin/api/accounts/"+acct.ID+"/authorization", "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("authorize authorized account = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already_authorized") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestDeleteAccountEndpoint(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/accounts", `{"name":"待删除账号"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	var doc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)

	rec = e.request(http.MethodDelete, "/admin/api/accounts/"+doc.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.request(http.MethodGet, "/admin/api/accounts", "", true)
	if strings.Contains(rec.Body.String(), doc.ID) {
		t.Fatal("deleted account still listed")
	}
	// Deleting again returns 404.
	rec = e.request(http.MethodDelete, "/admin/api/accounts/"+doc.ID, "", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
	// Deletion requires CSRF.
	e.csrf = ""
	rec = e.request(http.MethodDelete, "/admin/api/accounts/"+doc.ID, "", true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete without csrf = %d", rec.Code)
	}
}

func TestDeleteAPIKeyEndpoint(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"待删除"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	var doc struct {
		KeyView struct {
			ID string `json:"id"`
		} `json:"key_view"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)

	rec = e.request(http.MethodDelete, "/admin/api/api-keys/"+doc.KeyView.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.request(http.MethodGet, "/admin/api/api-keys", "", true)
	if strings.Contains(rec.Body.String(), doc.KeyView.ID) {
		t.Fatal("deleted key still listed")
	}
	rec = e.request(http.MethodDelete, "/admin/api/api-keys/"+doc.KeyView.ID, "", true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}
}

func TestDeletedAPIKeyCannotAuthenticate(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"临时"}`, true)
	var doc struct {
		Key     string `json:"key"`
		KeyView struct {
			ID string `json:"id"`
		} `json:"key_view"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)

	// A fresh gateway on the same DB must reject the key after deletion.
	gw := gateway.NewGateway(e.db, e.sched)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+doc.Key)
	rr := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		// Key never worked? It should work before deletion (503 no account).
		t.Fatalf("key rejected before deletion: %d %s", rr.Code, rr.Body.String())
	}

	e.request(http.MethodDelete, "/admin/api/api-keys/"+doc.KeyView.ID, "", true)

	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+doc.Key)
	rr = httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted key still authenticates: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAccountModelsAndTestEndpoints(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	acct := &store.Account{
		ID: "acct_t", Name: "T", CredentialPath: "/tmp/t.json",
		UpstreamAccountID: "up-t", Weight: 1,
	}
	if err := e.db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}

	// No runtime yet: both endpoints must explain the state instead of hanging.
	rec := e.request(http.MethodGet, "/admin/api/accounts/"+acct.ID+"/models", "", true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("models without runtime = %d, want 409", rec.Code)
	}
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+acct.ID+"/test",
		`{"model":"m","prompt":"hi"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("test without runtime = %d, want 409", rec.Code)
	}

	// Wire a fake runtime: catalog plus a canned generation.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"allowed":true,"plan_type":"prolite","model_catalog":{"models":[{"id":"gpt-5.6-sol","label":"Sol","default_effort":"medium","efforts":[]}],"default_model":"gpt-5.6-sol","source":"chatgpt","restricted_models":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello from upstream\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-sol\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n\n"))
	}))
	defer up.Close()

	rt, err := gateway.BuildRuntime(*acct, testAuthStore(t), gateway.RuntimeConfig{
		Upstream: up.URL, Transport: "sse",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.sched.Upsert(rt)

	rec = e.request(http.MethodGet, "/admin/api/accounts/"+acct.ID+"/models", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d %s", rec.Code, rec.Body.String())
	}
	var models struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		DefaultModel string `json:"default_model"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &models)
	if len(models.Models) != 1 || models.Models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("models payload = %s", rec.Body.String())
	}

	// Empty prompt falls back to "hi" and returns the real upstream text.
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+acct.ID+"/test",
		`{"model":"gpt-5.6-sol","prompt":""}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	var res struct {
		OK            bool   `json:"ok"`
		Text          string `json:"text"`
		ReportedModel string `json:"reported_model"`
		Requested     string `json:"requested_model"`
		Terminal      string `json:"terminal"`
		LatencyMS     int64  `json:"latency_ms"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.OK || res.Text != "hello from upstream" {
		t.Fatalf("test payload = %s", rec.Body.String())
	}
	if res.ReportedModel != "gpt-5.6-sol" || res.Requested != "gpt-5.6-sol" {
		t.Fatalf("model fields = %+v", res)
	}
	if res.Terminal != "response.completed" || res.LatencyMS < 0 {
		t.Fatalf("terminal/latency = %+v", res)
	}

	// Missing model is rejected.
	rec = e.request(http.MethodPost, "/admin/api/accounts/"+acct.ID+"/test",
		`{"prompt":"hi"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model = %d, want 400", rec.Code)
	}
}

func TestRevealKeyRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	// Create through the API so the sealed copy is produced the real way.
	rec := e.request(http.MethodPost, "/admin/api/api-keys", `{"name":"reveal-me"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key     string `json:"key"`
		KeyView struct {
			ID         string `json:"id"`
			Prefix     string `json:"prefix"`
			Revealable bool   `json:"revealable"`
		} `json:"key_view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" {
		t.Fatal("create must return the plaintext once")
	}

	// The list must stay masked and must not carry the plaintext.
	listRec := e.request(http.MethodGet, "/admin/api/api-keys", "", true)
	if strings.Contains(listRec.Body.String(), created.Key) {
		t.Fatal("list response leaked the plaintext key")
	}
	var listed struct {
		Keys []struct {
			ID         string `json:"id"`
			Prefix     string `json:"prefix"`
			Revealable bool   `json:"revealable"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listed)
	if len(listed.Keys) != 1 || !listed.Keys[0].Revealable {
		t.Fatalf("list payload = %s", listRec.Body.String())
	}
	if strings.Contains(listed.Keys[0].Prefix, created.Key[len(created.Key)-10:]) {
		t.Fatal("prefix must be masked, not the full key")
	}

	// Reveal returns the same plaintext and forbids caching.
	rev := e.request(http.MethodPost, "/admin/api/api-keys/"+created.KeyView.ID+"/reveal", "", true)
	if rev.Code != http.StatusOK {
		t.Fatalf("reveal = %d %s", rev.Code, rev.Body.String())
	}
	if cc := rev.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var revealed struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rev.Body.Bytes(), &revealed)
	if revealed.Key != created.Key {
		t.Fatal("reveal returned a different key")
	}

	// GET must not work: a link or prefetch cannot pull a key.
	if got := e.request(http.MethodGet, "/admin/api/api-keys/"+created.KeyView.ID+"/reveal", "", true); got.Code == http.StatusOK {
		t.Fatal("GET reveal must not succeed")
	}
}

func TestRevealKeyWithoutCiphertext(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	// A key created before encrypted storage has no sealed copy.
	rec := &store.APIKey{
		ID: "key_legacy", Name: "legacy", Prefix: "sk-bps-legacy12",
		KeyHash: apikeys.Hash("sk-bps-legacy"), Enabled: true,
	}
	if err := e.db.CreateAPIKey(rec); err != nil {
		t.Fatal(err)
	}
	got := e.request(http.MethodPost, "/admin/api/api-keys/key_legacy/reveal", "", true)
	if got.Code != http.StatusConflict {
		t.Fatalf("legacy reveal = %d, want 409: %s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "reveal_unavailable") {
		t.Fatalf("body = %s", got.Body.String())
	}
}

func TestOverviewReportsImageRelay(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	// Disabled: the field is null so the console hides the panel.
	rec := e.request(http.MethodGet, "/admin/api/overview", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}
	var off struct {
		Relay *struct {
			MaxBytes int64 `json:"max_bytes"`
		} `json:"image_relay"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &off)
	if off.Relay != nil {
		t.Fatalf("image_relay must be null when disabled: %s", rec.Body.String())
	}

	// Enabled: capacity fields are present.
	relay, err := basispoints.NewImageRelay("https://relay.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	e.handler.deps.Images = relay

	rec = e.request(http.MethodGet, "/admin/api/overview", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d %s", rec.Code, rec.Body.String())
	}
	var on struct {
		Relay struct {
			Images    int   `json:"images"`
			Bytes     int64 `json:"bytes"`
			MaxBytes  int64 `json:"max_bytes"`
			MaxImages int   `json:"max_images"`
			TTL       int   `json:"ttl_seconds"`
		} `json:"image_relay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &on); err != nil {
		t.Fatal(err)
	}
	if on.Relay.MaxBytes <= 0 || on.Relay.MaxImages <= 0 || on.Relay.TTL <= 0 {
		t.Fatalf("relay stats missing: %s", rec.Body.String())
	}
	if on.Relay.Images != 0 || on.Relay.Bytes != 0 {
		t.Fatalf("fresh relay should be empty: %s", rec.Body.String())
	}
}

func TestModelsSyncAndMappings(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	// Never synced: the console gets catalog: null rather than an error.
	rec := e.request(http.MethodGet, "/admin/api/models", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get models = %d %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Catalog json.RawMessage `json:"catalog"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &empty)
	if len(empty.Catalog) != 0 && string(empty.Catalog) != "null" {
		t.Fatalf("expected null catalog, got %s", rec.Body.String())
	}

	// Sync without an eligible account fails and leaves the catalog empty.
	rec = e.request(http.MethodPost, "/admin/api/models/sync", "", true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("sync without account = %d, want 503: %s", rec.Code, rec.Body.String())
	}

	// Wire an account whose upstream serves a catalog, then sync.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":true,"plan_type":"prolite","model_catalog":{"models":[
			{"id":"gpt-5.6-sol","label":"Sol","default_effort":"medium","efforts":[{"value":"low","label":"Light"}]},
			{"id":"gpt-6-astra","label":"Astra","default_effort":"medium","efforts":[]}
		],"default_model":"gpt-5.6-sol","source":"chatgpt","restricted_models":[]}}`))
	}))
	defer up.Close()

	acct := &store.Account{ID: "acct_m", Name: "M", CredentialPath: "/tmp/m.json", UpstreamAccountID: "up-m", Weight: 1, Enabled: true}
	if err := e.db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}
	rt, err := gateway.BuildRuntime(*acct, testAuthStore(t), gateway.RuntimeConfig{Upstream: up.URL, Transport: "sse"})
	if err != nil {
		t.Fatal(err)
	}
	rt.Enabled.Store(true)
	e.sched.Upsert(rt)

	rec = e.request(http.MethodPost, "/admin/api/models/sync", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	var synced struct {
		Catalog       json.RawMessage `json:"catalog"`
		SyncedAt      int64           `json:"synced_at"`
		SourceAccount string          `json:"source_account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &synced); err != nil {
		t.Fatal(err)
	}
	if synced.SyncedAt == 0 || synced.SourceAccount != "M" {
		t.Fatalf("sync metadata = %+v", synced)
	}
	var cat struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(synced.Catalog, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Data) != 2 || cat.Data[0].ID != "gpt-5.6-sol" {
		t.Fatalf("catalog = %s", synced.Catalog)
	}

	// Mappings: replace, then reject bad input.
	rec = e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"gpt-6-astra","target":"gpt-5.6-sol"}]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("put mappings = %d %s", rec.Code, rec.Body.String())
	}
	target, err := e.db.GetModelMapping("gpt-6-astra")
	if err != nil || target != "gpt-5.6-sol" {
		t.Fatalf("stored mapping = %q, %v", target, err)
	}

	rec = e.request(http.MethodGet, "/admin/api/models/mappings", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gpt-6-astra") {
		t.Fatalf("get mappings = %d %s", rec.Code, rec.Body.String())
	}

	// Duplicate source is rejected before anything is written.
	rec = e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"a","target":"b"},{"source":"a","target":"c"}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate source = %d, want 400", rec.Code)
	}
	if target, _ := e.db.GetModelMapping("gpt-6-astra"); target != "gpt-5.6-sol" {
		t.Fatal("a rejected save must not touch stored mappings")
	}

	// Empty fields are rejected too.
	rec = e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"x","target":""}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty target = %d, want 400", rec.Code)
	}
}

// Wildcards are allowed on the source but never on the target.
func TestMappingWildcardRules(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	// A wildcard source is accepted.
	rec := e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"gpt-6*","target":"gpt-5.6-sol"},{"source":"gpt-?.?-sol","target":"gpt-5.6-luna"}]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("wildcard source = %d %s", rec.Code, rec.Body.String())
	}
	if got, err := e.db.GetModelMapping("gpt-6*"); err != nil || got != "gpt-5.6-sol" {
		t.Fatalf("stored wildcard = %q, %v", got, err)
	}

	// A wildcard target is rejected: it could never resolve upstream.
	rec = e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"gpt-6*","target":"gpt-5*"}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wildcard target = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "通配符") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	// The rejected save must not disturb the stored set.
	if got, _ := e.db.GetModelMapping("gpt-6*"); got != "gpt-5.6-sol" {
		t.Fatalf("stored mapping changed: %q", got)
	}

	// '?' is also a wildcard and equally rejected in the target.
	rec = e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"x","target":"gpt-?.6"}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("? in target = %d, want 400", rec.Code)
	}
}

// Saving mappings must invalidate the resolver cache so edits apply at once.
func TestMappingSaveInvalidatesCache(t *testing.T) {
	e := newTestEnv(t)
	e.login()

	cache := modelmap.NewCache(func() ([]modelmap.Rule, error) {
		rows, err := e.db.ListModelMappings()
		if err != nil {
			return nil, err
		}
		rules := make([]modelmap.Rule, 0, len(rows))
		for _, m := range rows {
			rules = append(rules, modelmap.Rule{Pattern: m.SourceModel, Target: m.TargetModel})
		}
		return rules, nil
	})
	e.handler.deps.Models = cache

	// Prime the cache with an empty rule set.
	if got, err := cache.Resolve("gpt-6-astra"); err != nil || got != "" {
		t.Fatalf("initial resolve = %q, %v", got, err)
	}

	rec := e.request(http.MethodPut, "/admin/api/models/mappings",
		`{"mappings":[{"source":"gpt-6*","target":"gpt-5.6-sol"}]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}

	// The new rule must be visible without a restart.
	got, err := cache.Resolve("gpt-6-astra")
	if err != nil || got != "gpt-5.6-sol" {
		t.Fatalf("after save resolve = %q, %v", got, err)
	}
}

func TestStaticAssets(t *testing.T) {
	e := newTestEnv(t)
	e.login()
	rec := e.request(http.MethodGet, "/admin/", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("index status %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/admin/app.js", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("app.js status %d", rec.Code)
	}
	rec = e.request(http.MethodGet, "/admin/../secret", "", true)
	// The mux normalizes the path and redirects; the secret must not be served.
	if rec.Code == http.StatusOK {
		t.Fatalf("traversal served content: %s", rec.Body.String())
	}
}

// proxyEnv adds a proxy service and a stub runtime to the base env.
func (e *testEnv) withProxy(t *testing.T) {
	t.Helper()
	svc, err := proxysettings.NewService(e.db, "http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	e.proxy = svc
	sealer, err := apikeys.OpenSealer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.handler = NewHandler(Deps{
		DB: e.db, Scheduler: e.sched, Accounts: e.svc, Proxy: svc, Runtime: &stubRuntime{svc: svc},
		Sealer: sealer, Now: time.Now,
	}, NewSessionManager("test-password", time.Hour, false), staticFS())
}

type stubRuntime struct{ svc *proxysettings.Service }

// testAuthStore builds a credential store that never expires, for runtimes that
// only need eligibility checks.
func testAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	doc := `{"auth_mode":"chatgpt","tokens":{"access_token":"tok","account_id":"up"}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := auth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *stubRuntime) ApplyProxy(raw string) error { return s.svc.Set(raw) }

func TestGetAndPutProxy(t *testing.T) {
	e := newTestEnv(t)
	e.withProxy(t)
	e.login()

	rec := e.request(http.MethodGet, "/admin/api/settings/proxy", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "7890") {
		t.Fatalf("get proxy: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.request(http.MethodPut, "/admin/api/settings/proxy", `{"proxy_url":"socks5://127.0.0.1:1080"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("put proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.proxy.Get(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("live value = %q", got)
	}

	// Invalid scheme is rejected with 400 and does not change state.
	rec = e.request(http.MethodPut, "/admin/api/settings/proxy", `{"proxy_url":"ftp://x:1"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy status = %d", rec.Code)
	}
	if got := e.proxy.Get(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("state changed on rejection: %q", got)
	}

	// Empty value clears the proxy (direct connection).
	rec = e.request(http.MethodPut, "/admin/api/settings/proxy", `{"proxy_url":""}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear proxy: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.proxy.Get(); got != "" {
		t.Fatalf("proxy not cleared: %q", got)
	}
}

func TestProxySettingsRequireAuthAndCSRF(t *testing.T) {
	e := newTestEnv(t)
	e.withProxy(t)

	rec := e.request(http.MethodGet, "/admin/api/settings/proxy", "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated get status = %d", rec.Code)
	}

	e.login()
	saved := e.csrf
	e.csrf = ""
	rec = e.request(http.MethodPut, "/admin/api/settings/proxy", `{"proxy_url":""}`, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("put without csrf status = %d", rec.Code)
	}
	e.csrf = saved
}

func TestCheckProxyEndpoint(t *testing.T) {
	e := newTestEnv(t)
	e.withProxy(t)
	e.login()

	// Unreachable proxy: 200 with ok=false and a dial stage.
	rec := e.request(http.MethodPost, "/admin/api/settings/proxy/check",
		`{"proxy_url":"http://127.0.0.1:1"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("check status = %d", rec.Code)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Stage != "dial" {
		t.Fatalf("unreachable check = %+v", res)
	}

	// Invalid config is reported, not 500.
	rec = e.request(http.MethodPost, "/admin/api/settings/proxy/check", `{"proxy_url":"ftp://x"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid check status = %d", rec.Code)
	}
	var bad struct {
		OK    bool   `json:"ok"`
		Stage string `json:"stage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if bad.OK || bad.Stage != "config" {
		t.Fatalf("invalid check = %+v", bad)
	}
}
