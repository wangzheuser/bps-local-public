package proxy

import (
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
)

// pngDataURL renders a tiny valid PNG as an inline data URL.
func pngDataURL(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func inlineImageBody(t *testing.T, dataURL string) []byte {
	t.Helper()
	body := map[string]any{
		"model": "gpt-5.6-luna",
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "what is this"},
				map[string]any{"type": "input_image", "image_url": dataURL},
			},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// stagedLink extracts the relay URL the upstream received. The bridge prepends
// a developer message, so the image lives in whichever user item carries it.
func stagedLink(t *testing.T, received []byte) string {
	t.Helper()
	var doc struct {
		Input []struct {
			Content []struct {
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(received, &doc); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	for _, item := range doc.Input {
		for _, part := range item.Content {
			if part.ImageURL != "" {
				return part.ImageURL
			}
		}
	}
	t.Fatalf("no image_url in upstream body: %s", received)
	return ""
}

func TestInlineImageRewrittenAndServed(t *testing.T) {
	relay, err := basispoints.NewImageRelay("https://relay.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	var received []byte
	srv, up := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer up.Close()
	srv.SetImageRelay(relay)

	body := inlineImageBody(t, pngDataURL(t))
	rec := postResponses(t, srv.Handler(), string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	link := stagedLink(t, received)
	if !strings.HasPrefix(link, "https://relay.example"+basispoints.ImageRelayPath) {
		t.Fatalf("image not rewritten to relay URL: %q", link)
	}

	// The staged URL must be fetchable without an API key: the upstream has none.
	path := strings.TrimPrefix(link, "https://relay.example")
	get := httptest.NewRequest(http.MethodGet, path, nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("relay fetch status %d: %s", getRec.Code, getRec.Body.String())
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if getRec.Header().Get("Cache-Control") == "" {
		t.Fatal("relay response must set Cache-Control")
	}
}

func TestRelayEndpointAbsentWithoutRelay(t *testing.T) {
	srv, up := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	})
	defer up.Close()

	// Without a relay, inline images stay rejected and no image route exists.
	rec := postResponses(t, srv.Handler(), string(inlineImageBody(t, pngDataURL(t))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for inline image without relay", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not accept data:image") {
		t.Fatalf("body = %s", rec.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, basispoints.ImageRelayPath+"abc", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, get)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("relay route must not exist, got %d", getRec.Code)
	}
}

