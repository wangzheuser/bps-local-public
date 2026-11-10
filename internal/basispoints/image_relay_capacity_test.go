package basispoints

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noisyImage builds a data URL whose bytes are unique per call, so the relay
// cannot de-duplicate it and capacity limits are actually exercised.
func noisyImage(t *testing.T, size int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	pix := make([]byte, size*size*4)
	if _, err := rand.Read(pix); err != nil {
		t.Fatal(err)
	}
	copy(img.Pix, pix)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}

func rewriteBody(t *testing.T, relay *ImageRelay, dataURL string) ([]byte, error) {
	t.Helper()
	body := map[string]any{
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": dataURL},
			},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return relay.Rewrite(raw, "scope")
}

// stagedToken pulls the staged token out of a rewritten body.
func stagedToken(t *testing.T, out []byte) string {
	t.Helper()
	var doc struct {
		Input []struct {
			Content []struct {
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, item := range doc.Input {
		for _, part := range item.Content {
			if part.ImageURL != "" {
				return strings.TrimPrefix(part.ImageURL, "https://images.example"+ImageRelayPath)
			}
		}
	}
	return ""
}

// serve fetches a staged image through the relay handler.
func serve(relay *ImageRelay, token string) int {
	rec := httptest.NewRecorder()
	relay.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ImageRelayPath+token, nil))
	return rec.Code
}

// stage stages one image and returns its token.
func stage(t *testing.T, relay *ImageRelay, size int) string {
	t.Helper()
	out, err := rewriteBody(t, relay, noisyImage(t, size))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return stagedToken(t, out)
}

// A full store evicts the least recently served image to admit a new one, so a
// fresh request still succeeds instead of failing with 503.
func TestImageRelayEvictsLeastRecentlyUsed(t *testing.T) {
	relay, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	// Fill past the entry ceiling; eviction keeps the store inside its limits.
	var tokens []string
	for i := 0; i < imageRelayMaxEntries+16; i++ {
		tokens = append(tokens, stage(t, relay, 96))
	}
	stats := relay.Stats()
	if stats.Images > stats.MaxImages {
		t.Fatalf("entries %d exceed ceiling %d", stats.Images, stats.MaxImages)
	}
	if stats.EvictedTotal == 0 {
		t.Fatal("expected evictions once the ceiling was crossed")
	}

	// Serve an entry so it becomes recently used, then push another image in.
	recent := tokens[len(tokens)-1]
	if code := serve(relay, recent); code != 200 {
		t.Fatalf("serve staged image: %d", code)
	}
	beforeEvicted := relay.Stats().EvictedTotal
	newToken := stage(t, relay, 96)
	if newToken == "" {
		t.Fatal("new image was not staged")
	}
	if relay.Stats().EvictedTotal <= beforeEvicted {
		t.Fatal("staging at capacity must evict something")
	}
	// The just-served entry must survive: it is the most recently used.
	if code := serve(relay, recent); code != 200 {
		t.Fatalf("recently served entry was evicted, got %d", code)
	}
}

// Entries the upstream is downloading right now are never evicted: dropping one
// would break an in-flight request to serve a new one.
func TestImageRelayNeverEvictsActiveReaders(t *testing.T) {
	relay, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	for i := 0; i < imageRelayMaxEntries+8; i++ {
		stage(t, relay, 96)
	}

	// Pin every entry as actively read, as if every upstream fetch is running.
	relay.mu.Lock()
	for _, img := range relay.entries {
		img.readers++
	}
	relay.mu.Unlock()
	before := relay.Stats()

	// Nothing can be evicted, so a new image is refused rather than breaking
	// the in-flight downloads.
	_, err = rewriteBody(t, relay, noisyImage(t, 96))
	if err == nil {
		t.Fatal("expected full store when every entry is being read")
	}
	if !strings.Contains(err.Error(), "temporary image storage is full") {
		t.Fatalf("unexpected error: %v", err)
	}
	after := relay.Stats()
	if after.EvictedTotal != before.EvictedTotal {
		t.Fatalf("active readers must not be evicted: %d -> %d", before.EvictedTotal, after.EvictedTotal)
	}
	if after.Images != before.Images {
		t.Fatalf("entry count changed: %d -> %d", before.Images, after.Images)
	}
}

// Sustained pressure must never exceed either ceiling.
func TestImageRelayStaysWithinBudgetUnderPressure(t *testing.T) {
	relay, err := NewImageRelay("https://images.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Close() }()

	for i := 0; i < imageRelayMaxEntries+32; i++ {
		stage(t, relay, 96)
		s := relay.Stats()
		if s.Bytes > s.MaxBytes {
			t.Fatalf("byte budget exceeded at %d: %d > %d", i, s.Bytes, s.MaxBytes)
		}
		if s.Images > s.MaxImages {
			t.Fatalf("entry ceiling exceeded at %d: %d > %d", i, s.Images, s.MaxImages)
		}
	}
	if relay.Stats().EvictedTotal == 0 {
		t.Fatal("expected evictions under sustained pressure")
	}
}
