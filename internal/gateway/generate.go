package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
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
	// Error is the upstream error object from a failed terminal event. The
	// bridge relays those bytes verbatim to API clients, so only this admin
	// path needs to surface them; without it a failure reads as a bare
	// "upstream reported error" with no code, message or retry hint.
	Error *UpstreamError `json:"error,omitempty"`
	// RawTerminal keeps the terminal event exactly as the bridge emitted it.
	// Upstream error shapes vary (response.error, top-level error, nested
	// payloads), so the raw bytes are the only reliable diagnostic.
	RawTerminal string `json:"raw_terminal,omitempty"`
}

// UpstreamError mirrors the error object the upstream attaches to a failed
// terminal event.
type UpstreamError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   any    `json:"param,omitempty"`
	// RetryAfter is the upstream's own backoff hint in seconds, carried inside
	// the event body rather than in an HTTP header.
	RetryAfter int `json:"retry_after,omitempty"`
}

// describe renders the upstream error for callers that only have an error string.
func (e *UpstreamError) describe() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.RetryAfter > 0 {
		parts = append(parts, "retry-after "+strconv.Itoa(e.RetryAfter)+"s")
	}
	if len(parts) == 0 {
		if e.Type != "" {
			return e.Type
		}
		return "upstream failure without details"
	}
	return strings.Join(parts, ": ")
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
		// Surface what the upstream actually said. Overload, rate limiting and
		// auth expiry all arrive as a failed terminal event, and a bare terminal
		// name gives an operator nothing to act on. The partial result travels
		// with the error so callers can read RawTerminal and Error.
		if detail := res.Error.describe(); detail != "" {
			return res, fmt.Errorf("upstream reported %s (%s)", res.Terminal, detail)
		}
		return res, fmt.Errorf("upstream reported %s", res.Terminal)
	}
	return res, nil
}

// eventError is the error object attached to a terminal SSE event.
type eventError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   any    `json:"param"`
	Headers struct {
		RetryAfter string `json:"retry-after"`
	} `json:"headers"`
}

// captureError records the upstream error from a terminal event. A bare error
// event carries it at the top level; response.failed nests it under
// response.error. Both shapes are observed in production.
func (r *GenerationResult) captureError(
	respErr, topErr *eventError,
	topCode, topMessage string,
) {
	// Prefer the nested shape, then the top-level object, then bare fields.
	pick := respErr
	if pick == nil {
		pick = topErr
	}
	up := &UpstreamError{}
	if pick != nil {
		up.Type = pick.Type
		up.Code = pick.Code
		up.Message = pick.Message
		up.Param = pick.Param
		if n, err := strconv.Atoi(strings.TrimSpace(pick.Headers.RetryAfter)); err == nil && n > 0 {
			up.RetryAfter = n
		}
	}
	// A bare error event keeps its fields at the top level.
	if up.Code == "" && topCode != "" {
		up.Code = topCode
	}
	if up.Message == "" && topMessage != "" {
		up.Message = topMessage
	}
	if up.Code == "" && up.Message == "" && up.Type == "" {
		return
	}
	r.Error = up
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
			// A bare error event carries the error object at the top level, while
			// response.failed nests it under response.error. Both shapes occur.
			Error   *eventError `json:"error"`
			Code    string      `json:"code"`
			Message string      `json:"message"`
			Response struct {
				Model string         `json:"model"`
				Usage map[string]any `json:"usage"`
				Error *eventError    `json:"error"`
			} `json:"response"`
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
			out.captureError(ev.Response.Error, ev.Error, ev.Code, ev.Message)
			if out.Terminal != "response.completed" && out.Terminal != "response.incomplete" {
				out.RawTerminal = payload
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
