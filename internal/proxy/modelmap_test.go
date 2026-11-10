package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mapperOf builds a lookup from a fixed table.
func mapperOf(table map[string]string) ModelMapper {
	return func(source string) (string, error) {
		if target, ok := table[source]; ok {
			return target, nil
		}
		return "", nil
	}
}

func bodyWithModel(model string, extra map[string]any) []byte {
	doc := map[string]any{"model": model, "input": "hi", "stream": true}
	for k, v := range extra {
		doc[k] = v
	}
	raw, _ := json.Marshal(doc)
	return raw
}

func modelIn(t *testing.T, raw []byte) string {
	t.Helper()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc.Model
}

func TestRewriteModelAppliesMapping(t *testing.T) {
	raw := bodyWithModel("gpt-6-astra", map[string]any{"store": false})
	out, used, err := rewriteModel(raw, "gpt-6-astra", mapperOf(map[string]string{
		"gpt-6-astra": "gpt-5.6-luna",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if used != "gpt-5.6-luna" {
		t.Fatalf("used = %q", used)
	}
	if got := modelIn(t, out); got != "gpt-5.6-luna" {
		t.Fatalf("body model = %q", got)
	}
	// Other fields must survive untouched.
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["store"] != false || doc["input"] != "hi" || doc["stream"] != true {
		t.Fatalf("unrelated fields changed: %v", doc)
	}
}

func TestRewriteModelPassthroughWhenUnmapped(t *testing.T) {
	raw := bodyWithModel("gpt-5.6-sol", nil)
	out, used, err := rewriteModel(raw, "gpt-5.6-sol", mapperOf(map[string]string{
		"gpt-6-astra": "gpt-5.6-luna",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if used != "gpt-5.6-sol" || string(out) != string(raw) {
		t.Fatalf("unmapped model must pass through unchanged")
	}
}

func TestRewriteModelIgnoresNilMapper(t *testing.T) {
	raw := bodyWithModel("gpt-6-astra", nil)
	out, used, err := rewriteModel(raw, "gpt-6-astra", nil)
	if err != nil || used != "gpt-6-astra" || string(out) != string(raw) {
		t.Fatalf("nil mapper must be a no-op")
	}
}

func TestRewriteModelIgnoresLookupFailure(t *testing.T) {
	raw := bodyWithModel("gpt-6-astra", nil)
	failing := func(string) (string, error) { return "", errors.New("db down") }
	out, used, err := rewriteModel(raw, "gpt-6-astra", failing)
	if err != nil {
		t.Fatalf("a lookup failure must not fail the request: %v", err)
	}
	if used != "gpt-6-astra" || string(out) != string(raw) {
		t.Fatal("lookup failure must fall back to passthrough")
	}
}

func TestRewriteModelIdentityMappingIsNoop(t *testing.T) {
	raw := bodyWithModel("gpt-5.6-sol", nil)
	out, used, err := rewriteModel(raw, "gpt-5.6-sol", mapperOf(map[string]string{
		"gpt-5.6-sol": "gpt-5.6-sol",
	}))
	if err != nil || used != "gpt-5.6-sol" || string(out) != string(raw) {
		t.Fatal("identity mapping must not rewrite the body")
	}
}

func TestRewriteModelSurvivesInvalidJSON(t *testing.T) {
	raw := []byte("not json at all")
	out, used, err := rewriteModel(raw, "gpt-6-astra", mapperOf(map[string]string{
		"gpt-6-astra": "gpt-5.6-luna",
	}))
	if err != nil {
		t.Fatalf("invalid JSON must not error here: %v", err)
	}
	// The body is left for the normal validation path to reject.
	if string(out) != string(raw) {
		t.Fatal("invalid body must be left untouched")
	}
	if used != "gpt-6-astra" {
		t.Fatalf("used = %q", used)
	}
}

func TestModelFromRawTrimsWhitespace(t *testing.T) {
	if got := modelFromRaw([]byte(`{"model":"  gpt-5.6-sol  "}`)); got != "gpt-5.6-sol" {
		t.Fatalf("got %q", got)
	}
	if got := modelFromRaw([]byte(`{}`)); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := modelFromRaw([]byte(`bad`)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteModelMappedTargetIsSentUpstream(t *testing.T) {
	// End-to-end through the handler: the upstream must receive the alias while
	// the client keeps asking for the original name.
	var received []byte
	srv, up := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	})
	defer up.Close()
	srv.cfg.ModelMapper = mapperOf(map[string]string{"gpt-6-astra": "gpt-5.6-luna"})

	rec := postResponses(t, srv.Handler(), string(bodyWithModel("gpt-6-astra", nil)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(received), "gpt-5.6-luna") {
		t.Fatalf("upstream body missing mapped model: %s", received)
	}
}

