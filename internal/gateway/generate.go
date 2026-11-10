package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"
)

// GenerationResult is the outcome of one account test generation.
type GenerationResult struct {
	Text          string         `json:"text"`
	Reasoning     string         `json:"reasoning,omitempty"`
	ReportedModel string         `json:"reported_model"`
	Usage         map[string]any `json:"usage,omitempty"`
	LatencyMS     int64          `json:"latency_ms"`
	Terminal      string         `json:"terminal"`
}

// Generate runs one real generation through this account's bridge and returns
// the text the upstream produced. It reuses the account handler, so request
// conversion, tool sanitising and error mapping behave exactly as they do for
// client traffic.
//
// The caller supplies the deadline; a test that runs long is the caller's
// problem to bound.
func (r *Runtime) Generate(ctx context.Context, model, prompt, effort string) (*GenerationResult, error) {
	if r.Handler == nil {
		return nil, fmt.Errorf("account runtime is not loaded")
	}
	payload := map[string]any{
		"model":   model,
		"input":   prompt,
		"stream":  true,
		"store":   false,
	}
	if effort != "" {
		payload["reasoning"] = map[string]any{"effort": effort}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode test request: %w", err)
	}

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	started := time.Now()
	r.Handler.ServeHTTP(rec, req)
	latency := time.Since(started).Milliseconds()

	if rec.Code != 200 {
		msg := strings.TrimSpace(rec.Body.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, fmt.Errorf("upstream rejected the test request (HTTP %d): %s", rec.Code, msg)
	}

	res := &GenerationResult{LatencyMS: latency}
	if err := parseSSEInto(rec.Body.String(), res); err != nil {
		return nil, err
	}
	if res.Terminal == "" {
		return nil, fmt.Errorf("upstream stream ended without a terminal event")
	}
	if res.Terminal != "response.completed" && res.Terminal != "response.incomplete" {
		return nil, fmt.Errorf("upstream reported %s", res.Terminal)
	}
	return res, nil
}

// parseSSEInto collects text deltas and the terminal metadata from an SSE body.
func parseSSEInto(raw string, out *GenerationResult) error {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	var data strings.Builder
	flush := func() {
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return
		}
		var ev struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Model string         `json:"model"`
				Usage map[string]any `json:"usage"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return
		}
		switch ev.Type {
		case "response.output_text.delta":
			out.Text += ev.Delta
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			out.Reasoning += ev.Delta
		case "response.completed", "response.incomplete", "response.failed", "error":
			out.Terminal = ev.Type
			if ev.Response.Model != "" {
				out.ReportedModel = ev.Response.Model
			}
			if ev.Response.Usage != nil {
				out.Usage = ev.Response.Usage
			}
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
	}
	flush()
	return scanner.Err()
}
