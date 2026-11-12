package gateway

import (
	"context"
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

// A response.failed event carries the upstream's own code, message and backoff
// hint. Losing them leaves an operator with "upstream reported error" and no way
// to tell overload from an auth problem.
func TestRuntimeGenerateSurfacesUpstreamErrorDetail(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`event: response.failed` + "\n" +
				`data: {"type":"response.failed","response":{"status":"failed",` +
				`"error":{"type":"service_unavailable_error","code":"server_is_overloaded",` +
				`"message":"Our servers are currently overloaded. Please try again later.",` +
				`"param":null,"headers":{"retry-after":"6","retry-after-ms":"6000"}}}}` + "\n\n"))
	}))
	defer up.Close()
	_, sched, _ := buildTestGateway(t, up.URL, "")

	_, err := sched.Get("acct_A").Generate(context.Background(), "m", "hi", "")
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{"server_is_overloaded", "overloaded", "retry-after 6s"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// The bridge relays the upstream's overload signal as a bare "error" event with
// the error object at the top level (not nested under response). This is the
// shape observed against production, and the one that used to lose its detail.
func TestRuntimeGenerateSurfacesTopLevelErrorEvent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`event: error` + "\n" +
				`data: {"error":{"code":"server_is_overloaded","headers":{"retry-after":"7",` +
				`"retry-after-ms":"7000","x-retry-metadata":"NO_MORE_RETRY"},` +
				`"message":"Our servers are currently overloaded. Please try again later.",` +
				`"param":null,"type":"service_unavailable_error"},` +
				`"response_id":"resp_x","sequence_number":2,"type":"error"}` + "\n\n"))
	}))
	defer up.Close()
	_, sched, _ := buildTestGateway(t, up.URL, "")

	_, err := sched.Get("acct_A").Generate(context.Background(), "m", "hi", "")
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{"server_is_overloaded", "retry-after 7s"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}


