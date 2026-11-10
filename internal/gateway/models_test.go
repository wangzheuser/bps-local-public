package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const modelCatalogJSON = `{
  "allowed": true,
  "plan_type": "prolite",
  "auth_mode": "chatgpt",
  "model_catalog": {
    "models": [
      {"id":"gpt-5.6-luna","label":"GPT-5.6 Luna","default_effort":"medium",
       "efforts":[{"value":"low","label":"Light"},{"value":"medium","label":"Standard"}]},
      {"id":"gpt-5.6-sol","label":"GPT-5.6 Sol","default_effort":"medium","efforts":[]}
    ],
    "default_model": "gpt-5.6-sol",
    "source": "chatgpt",
    "restricted_models": []
  }
}`

// modelUpstream serves both the access catalog and a generation stream.
func modelUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(modelCatalogJSON))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-sol\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1,\"total_tokens\":8}}}\n\n"))
	}))
}

func TestModelsEndpointReturnsUpstreamCatalog(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, _, key := buildTestGateway(t, up.URL, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Object   string `json:"object"`
			OwnedBy  string `json:"owned_by"`
			Label    string `json:"label"`
			Efforts  []struct {
				Value string `json:"value"`
			} `json:"efforts"`
		} `json:"data"`
		DefaultModel string `json:"default_model"`
		PlanType     string `json:"plan_type"`
		Allowed      bool   `json:"allowed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Object != "list" || len(doc.Data) != 2 {
		t.Fatalf("unexpected payload: %s", rec.Body.String())
	}
	if doc.Data[0].ID != "gpt-5.6-luna" || doc.Data[0].Object != "model" || doc.Data[0].OwnedBy != "basispoints" {
		t.Fatalf("bad entry: %+v", doc.Data[0])
	}
	if len(doc.Data[0].Efforts) != 2 {
		t.Fatalf("efforts not exposed: %+v", doc.Data[0])
	}
	if doc.DefaultModel != "gpt-5.6-sol" || doc.PlanType != "prolite" || !doc.Allowed {
		t.Fatalf("metadata missing: %+v", doc)
	}
}

func TestModelsEndpointRequiresKey(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, _, _ := buildTestGateway(t, up.URL, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestModelsEndpointNoAccount(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "")
	sched.Get("acct_A").Enabled.Store(false)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_account_available") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRuntimeGenerateReturnsUpstreamText(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, sched, _ := buildTestGateway(t, up.URL, "")
	_ = gw
	rt := sched.Get("acct_A")

	res, err := rt.Generate(context.Background(), "gpt-5.6-sol", "ping", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Text != "pong" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.ReportedModel != "gpt-5.6-sol" {
		t.Fatalf("reported model = %q", res.ReportedModel)
	}
	if res.Terminal != "response.completed" {
		t.Fatalf("terminal = %q", res.Terminal)
	}
	if res.Usage == nil || res.Usage["input_tokens"] != float64(7) {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestRuntimeGenerateReportsFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"basispoints_ws_timeout\"}}}\n\n"))
	}))
	defer up.Close()
	_, sched, _ := buildTestGateway(t, up.URL, "")

	_, err := sched.Get("acct_A").Generate(context.Background(), "m", "hi", "")
	if err == nil || !strings.Contains(err.Error(), "response.failed") {
		t.Fatalf("expected failure, got %v", err)
	}
}
