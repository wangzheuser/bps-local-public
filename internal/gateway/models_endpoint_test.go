package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// catalogPayload is the OpenAI-compatible shape the console persists.
const catalogPayload = `{"object":"list","data":[
  {"id":"gpt-5.6-luna","object":"model","created":1,"owned_by":"basispoints",
   "label":"GPT-5.6 Luna","default_effort":"medium",
   "efforts":[{"value":"low","label":"Light"},{"value":"medium","label":"Standard"}]},
  {"id":"gpt-5.6-sol","object":"model","created":1,"owned_by":"basispoints",
   "label":"GPT-5.6 Sol","default_effort":"medium","efforts":[]}
],"default_model":"gpt-5.6-sol","plan_type":"prolite","source":"chatgpt",
"allowed":true,"denial_reason":"","restricted_models":[]}`

func getModels(t *testing.T, gw *Gateway, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)
	return rec
}

// /v1/models serves the persisted snapshot: no account is scheduled and no
// upstream call happens.
func TestModelsEndpointServesPersistedCatalog(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "")

	// Disable every account: a live upstream read would now fail, proving the
	// endpoint no longer depends on the scheduler.
	sched.Get("acct_A").Enabled.Store(false)

	if err := gw.db.SaveModelCatalog([]byte(catalogPayload), "acct_A"); err != nil {
		t.Fatal(err)
	}

	rec := getModels(t, gw, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Object       string `json:"object"`
		SyncedAt     int64  `json:"synced_at"`
		DefaultModel string `json:"default_model"`
		PlanType     string `json:"plan_type"`
		Allowed      bool   `json:"allowed"`
		Data         []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
			Efforts []struct {
				Value string `json:"value"`
			} `json:"efforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Object != "list" || len(doc.Data) != 2 {
		t.Fatalf("payload = %s", rec.Body.String())
	}
	if doc.Data[0].ID != "gpt-5.6-luna" || doc.Data[0].OwnedBy != "basispoints" {
		t.Fatalf("entry = %+v", doc.Data[0])
	}
	if len(doc.Data[0].Efforts) != 2 {
		t.Fatalf("efforts dropped: %+v", doc.Data[0])
	}
	if doc.DefaultModel != "gpt-5.6-sol" || doc.PlanType != "prolite" || !doc.Allowed {
		t.Fatalf("metadata lost: %+v", doc)
	}
	if doc.SyncedAt == 0 {
		t.Fatal("synced_at must be exposed so clients can judge freshness")
	}
}

// Before the first sync the endpoint returns an actionable error.
func TestModelsEndpointBeforeSync(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, _, key := buildTestGateway(t, up.URL, "")

	rec := getModels(t, gw, key)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_catalog_unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestModelsEndpointRequiresKey(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, _, _ := buildTestGateway(t, up.URL, "")

	if rec := getModels(t, gw, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A corrupt snapshot must not be served as if it were valid.
func TestModelsEndpointRejectsCorruptCatalog(t *testing.T) {
	up := modelUpstream(t)
	defer up.Close()
	gw, _, key := buildTestGateway(t, up.URL, "")

	if err := gw.db.SaveModelCatalog([]byte("{not json"), "acct_A"); err != nil {
		t.Fatal(err)
	}
	rec := getModels(t, gw, key)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_catalog_corrupt") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
