package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestTrackingWriterParsesTerminalUsage(t *testing.T) {
	rec := &usageRecord{}
	tw := newTrackingWriter(httptest.NewRecorder(), rec)
	// Content-Type not set before WriteHeader -> defaults OK; SSE detection
	// needs the header, so set it the way proxy does.
	tw.Header().Set("Content-Type", "text/event-stream")

	chunks := []string{
		"event: response.crea",
		"ted\ndata: {\"type\":\"response.created\"}\n\nevent: response.out",
		"put_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
		`event: response.completed` + "\n" +
			`data: {"type":"response.completed","response":{"model":"gpt-6-astra","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}` + "\n\n",
	}
	for _, c := range chunks {
		if _, err := tw.Write([]byte(c)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.terminal != "response.completed" {
		t.Fatalf("terminal = %q", rec.terminal)
	}
	if rec.reportedModel != "gpt-6-astra" {
		t.Fatalf("model = %q", rec.reportedModel)
	}
	checkInt(t, rec.inputTokens, 10, "input")
	checkInt(t, rec.outputTokens, 5, "output")
	checkInt(t, rec.totalTokens, 15, "total")
	checkInt(t, rec.cachedTokens, 3, "cached")
	checkInt(t, rec.reasoningTokens, 2, "reasoning")
}

func checkInt(t *testing.T, p *int64, want int64, name string) {
	t.Helper()
	if p == nil || *p != want {
		t.Fatalf("%s tokens = %v, want %d", name, p, want)
	}
}

func TestTrackingWriterParsesFailedEvent(t *testing.T) {
	rec := &usageRecord{}
	tw := newTrackingWriter(httptest.NewRecorder(), rec)
	tw.Header().Set("Content-Type", "text/event-stream")
	body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"basispoints_ws_timeout\"}}}\n\n"
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.terminal != "response.failed" || rec.errorCode != "basispoints_ws_timeout" {
		t.Fatalf("terminal=%q code=%q", rec.terminal, rec.errorCode)
	}
	if rec.inputTokens != nil {
		t.Fatal("failed event must not report tokens")
	}
}

func TestTrackingWriterCapturesJSONError(t *testing.T) {
	rec := &usageRecord{}
	tw := newTrackingWriter(httptest.NewRecorder(), rec)
	tw.Header().Set("Content-Type", "application/json")
	tw.WriteHeader(401)
	if _, err := tw.Write([]byte(`{"error":{"type":"invalid_request_error","code":"basispoints_upstream_error","message":"upstream returned HTTP 401"}}`)); err != nil {
		t.Fatal(err)
	}
	tw.rec.finishJSONError()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.errorCode != "basispoints_upstream_error" {
		t.Fatalf("code = %q", rec.errorCode)
	}
	if rec.upstreamStatus != 401 {
		t.Fatalf("upstream status = %d", rec.upstreamStatus)
	}
	if rec.isSSE {
		t.Fatal("json body must not be classified as SSE")
	}
}

func TestTrackingWriterMissingUsageStaysNil(t *testing.T) {
	rec := &usageRecord{}
	tw := newTrackingWriter(httptest.NewRecorder(), rec)
	tw.Header().Set("Content-Type", "text/event-stream")
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\"}}\n\n"
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.inputTokens != nil || rec.outputTokens != nil || rec.totalTokens != nil {
		t.Fatal("missing usage must stay nil")
	}
}
