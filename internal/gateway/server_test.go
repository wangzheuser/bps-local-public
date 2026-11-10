package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sunboyzoey/GPT-BPS/internal/apikeys"
	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

const sseCompleted = "event: response.completed\ndata: {\"type\":\"response.completed\"," +
	"\"response\":{\"model\":\"gpt-6-astra\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

// fakeSSEUpstream serves the basispoints SSE transport shape. When rejectToken
// is set and the bearer matches, it returns that HTTP status instead.
func fakeSSEUpstream(t *testing.T, body string, rejectToken string, rejectStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectToken != "" && r.Header.Get("Authorization") == "Bearer "+rejectToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rejectStatus)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":"basispoints_upstream_error","message":"upstream returned HTTP %d"}}`, rejectStatus)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
}

// buildTestGateway wires a full gateway with one or two accounts against the
// given upstream. Returns the gateway, plaintext API key and cleanup.
func buildTestGateway(t *testing.T, upstreamURL string, accountBToken string) (*Gateway, *Scheduler, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sched := NewScheduler()
	gw := NewGateway(db, sched)

	keyGen, err := apikeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	keyRec := &store.APIKey{ID: "key_test", Name: "test", Prefix: keyGen.DisplayPrefix, KeyHash: keyGen.Hash, Enabled: true}
	if err := db.CreateAPIKey(keyRec); err != nil {
		t.Fatal(err)
	}

	stA, err := auth.Load(fakeAuthFile(t, "token-A", "acct-up-A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := addAccount(db, sched, "acct_A", stA, upstreamURL); err != nil {
		t.Fatal(err)
	}
	if accountBToken != "" {
		stB, err := auth.Load(fakeAuthFile(t, accountBToken, "acct-up-B"))
		if err != nil {
			t.Fatal(err)
		}
		if err := addAccount(db, sched, "acct_B", stB, upstreamURL); err != nil {
			t.Fatal(err)
		}
	}
	return gw, sched, keyGen.Plaintext
}

func addAccount(db *store.DB, sched *Scheduler, id string, st *auth.Store, upstream string) error {
	a := store.Account{ID: id, Name: "acc-" + id, CredentialPath: "/tmp/" + id + ".json",
		UpstreamAccountID: id + "-up", Enabled: true, Weight: 1}
	if err := db.CreateAccount(&a); err != nil {
		return err
	}
	rt, err := BuildRuntime(a, st, RuntimeConfig{Upstream: upstream, Transport: "sse"})
	if err != nil {
		return err
	}
	sched.Upsert(rt)
	return nil
}

func postResponsesWithKey(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const basicBody = `{"model":"gpt-6-astra","stream":true,"store":false,` +
	`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
	`"reasoning":{"effort":"low"}}`

func TestGatewayRejectsMissingAndInvalidKeys(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, _, key := buildTestGateway(t, up.URL, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(basicBody))
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status %d", rec.Code)
	}

	badKey := "sk-bps-" + strings.Repeat("A", 43) // correct shape, wrong value
	rec = postResponsesWithKey(t, gw.APIHandler(), badKey, basicBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key: status %d", rec.Code)
	}
	_ = key
}

func TestGatewayRecordsCompletedUsage(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, _, key := buildTestGateway(t, up.URL, "")

	rec := postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	rows, total, err := gw.db.ListUsageRequests(store.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("usage rows total = %d", total)
	}
	u := rows[0]
	if u.Status != "completed" || u.TerminalEvent != "response.completed" {
		t.Fatalf("status=%s terminal=%s", u.Status, u.TerminalEvent)
	}
	if u.InputTokens.Int64 != 10 || u.OutputTokens.Int64 != 5 || !u.InputTokens.Valid {
		t.Fatalf("tokens not recorded: %+v", u)
	}
	if u.APIKeyID == nil || *u.APIKeyID != "key_test" {
		t.Fatalf("api key not recorded: %+v", u.APIKeyID)
	}
	if u.AccountID == nil {
		t.Fatal("account not recorded")
	}
	if u.DurationMS < 0 {
		t.Fatalf("duration = %d", u.DurationMS)
	}
}

func TestGateway503WhenNoEligibleAccount(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "")
	sched.Get("acct_A").Enabled.Store(false)

	rec := postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_account_available") {
		t.Fatalf("body %s", rec.Body.String())
	}
	rows, _, _ := gw.db.ListUsageRequests(store.UsageFilters{})
	if len(rows) != 1 || rows[0].Status != "http_error" || rows[0].ErrorCode != "no_account_available" {
		t.Fatalf("usage row mismatch: %+v", rows)
	}
}

func TestGatewayAuthFailureExcludesAccount(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "token-B", 401)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "token-B")

	// First request may hit either account; drive requests until B is excluded.
	// With both idle, the tie-break alternates; a request to B returns 401.
	postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
	rtB := sched.Get("acct_B")
	rtA := sched.Get("acct_A")
	if rtB.AuthRequired.Load() {
		// B hit the 401; next request must go to A.
		rec2 := postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
		if rec2.Code != http.StatusOK {
			t.Fatalf("second request status %d body %s", rec2.Code, rec2.Body.String())
		}
		if !rtA.Enabled.Load() || rtA.AuthRequired.Load() {
			t.Fatal("account A must remain eligible")
		}
	} else {
		// A served the first request; B is still eligible. Force one more
		// request with A disabled so B is picked and rejected.
		rtA.Enabled.Store(false)
		rec2 := postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 from B, got %d", rec2.Code)
		}
		if !rtB.AuthRequired.Load() {
			t.Fatal("B must be marked auth_required after 401")
		}
		rtA.Enabled.Store(true)
	}
	// Now B is auth-required in both paths; re-enable flow works.
	sched.ResetAuth("acct_B")
	if _, err := sched.Pick(""); err != nil {
		t.Fatalf("reset auth failed: %v", err)
	}
}

func TestGatewayRecordsUpstreamJSONError(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "token-B", 401)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "token-B")
	sched.Get("acct_A").Enabled.Store(false) // force B

	rec := postResponsesWithKey(t, gw.APIHandler(), key, basicBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	rows, _, _ := gw.db.ListUsageRequests(store.UsageFilters{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	u := rows[0]
	if u.Status != "http_error" || u.HTTPStatus != 401 {
		t.Fatalf("status=%s http=%d", u.Status, u.HTTPStatus)
	}
	if u.ErrorCode != "basispoints_upstream_error" {
		t.Fatalf("code = %q", u.ErrorCode)
	}
}

func TestGatewayRateLimitCooldown(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "token-B", 429)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "token-B")
	sched.Get("acct_A").Enabled.Store(false)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(basicBody))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	rt := sched.Get("acct_B")
	if rt.CooldownUntil.Load() <= 0 {
		t.Fatal("429 must set a cooldown")
	}
}

func TestGatewayHealthz(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, _, _ := buildTestGateway(t, up.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc["ok"] != true {
		t.Fatalf("healthz body %s", rec.Body.String())
	}
	// Without a relay the field is absent, not zeroed.
	if _, ok := doc["image_relay"]; ok {
		t.Fatalf("image_relay must be omitted when disabled: %s", rec.Body.String())
	}
}

func TestGatewayHealthzReportsImageRelay(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, _, _ := buildTestGateway(t, up.URL, "")

	relay, err := basispoints.NewImageRelay("https://relay.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()
	gw.SetImageRelay(relay)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status %d", rec.Code)
	}
	var doc struct {
		OK    bool `json:"ok"`
		Relay struct {
			Images    int   `json:"images"`
			Bytes     int64 `json:"bytes"`
			MaxBytes  int64 `json:"max_bytes"`
			MaxImages int   `json:"max_images"`
			TTL       int   `json:"ttl_seconds"`
		} `json:"image_relay"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !doc.OK || doc.Relay.MaxBytes <= 0 || doc.Relay.MaxImages <= 0 || doc.Relay.TTL <= 0 {
		t.Fatalf("relay stats missing: %s", rec.Body.String())
	}
}

func TestGatewayDisableAccountMidTest(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "token-B")

	// A request succeeds, then A is disabled: the next request must not go to A.
	if rec := postResponsesWithKey(t, gw.APIHandler(), key, basicBody); rec.Code != http.StatusOK {
		t.Fatalf("first request failed: %d", rec.Code)
	}
	sched.Get("acct_A").Enabled.Store(false)
	if rec := postResponsesWithKey(t, gw.APIHandler(), key, basicBody); rec.Code != http.StatusOK {
		t.Fatalf("second request failed: %d", rec.Code)
	}
	// Both requests completed; B must have served the second one.
	rows, total, _ := gw.db.ListUsageRequests(store.UsageFilters{})
	if total != 2 || rows[0].Status != "completed" || rows[1].Status != "completed" {
		t.Fatalf("usage mismatch: %+v", rows)
	}
	if rows[0].AccountID == nil || rows[1].AccountID == nil {
		t.Fatal("account attribution missing")
	}
	if *rows[1].AccountID != "acct_B" {
		t.Fatalf("second request served by %s, want acct_B", *rows[1].AccountID)
	}
}
