package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type recordKeyType struct{}

var upstreamStatusPattern = regexp.MustCompile(`HTTP (\d{3})`)

const (
	maxSSEParseBuffer = 1 << 20 // stop parsing if no event boundary in 1 MiB
	maxErrorBody      = 64 << 10
)

// usageRecord accumulates one request's outcome. It is written once at request
// end; per-delta database writes are deliberately avoided.
type usageRecord struct {
	mu sync.Mutex

	ID       string
	APIKeyID string

	model   string
	effort  string
	reportedModel string

	httpStatus     int
	retryAfterSec  int
	errorCode      string
	upstreamStatus int

	terminal string
	inputTokens, outputTokens, totalTokens, cachedTokens, reasoningTokens *int64

	startedAt time.Time

	// error body capture for non-SSE JSON failures
	errorBody []byte
	isSSE     bool
	tooBig    bool
	buf       []byte
}

func recordFromContext(ctx context.Context) *usageRecord {
	rec, _ := ctx.Value(recordKeyType{}).(*usageRecord)
	return rec
}

func (u *usageRecord) setRequestMeta(meta proxyRequestMetadata) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.model, u.effort = meta.Model, meta.ReasoningEffort
}

func (u *usageRecord) setHTTPStatus(code int, retryAfter, contentType string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.httpStatus = code
	if retryAfter != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n > 0 {
			u.retryAfterSec = n
		}
	}
	u.isSSE = strings.HasPrefix(contentType, "text/event-stream")
}

// trackingWriter passes every byte through unchanged while observing terminal
// SSE events and JSON error bodies for usage statistics.
type trackingWriter struct {
	http.ResponseWriter
	rec         *usageRecord
	wroteHeader bool
}

func newTrackingWriter(w http.ResponseWriter, rec *usageRecord) *trackingWriter {
	return &trackingWriter{ResponseWriter: w, rec: rec}
}

func (t *trackingWriter) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.wroteHeader = true
	t.rec.setHTTPStatus(code, t.Header().Get("Retry-After"), t.Header().Get("Content-Type"))
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	t.rec.observe(p)
	return t.ResponseWriter.Write(p)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (u *usageRecord) observe(p []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.terminal != "" || u.tooBig {
		return
	}
	if !u.isSSE {
		// Non-SSE: capture the JSON error body (bounded).
		if len(u.buf) < maxErrorBody {
			u.buf = append(u.buf, p...)
		}
		return
	}
	u.buf = append(u.buf, p...)
	if len(u.buf) > maxSSEParseBuffer {
		u.tooBig = true
		u.buf = nil
		return
	}
	for {
		idx := bytes.Index(u.buf, []byte("\n\n"))
		if idx < 0 {
			return
		}
		event := u.buf[:idx]
		u.buf = u.buf[idx+2:]
		u.handleSSEEvent(event)
		if u.terminal != "" {
			u.buf = nil
			return
		}
	}
}

type sseUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type sseEvent struct {
	Type string `json:"type"`
	Code string `json:"code"`
	Response *struct {
		Model string     `json:"model"`
		Usage *sseUsage  `json:"usage"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"response"`
}

func (u *usageRecord) handleSSEEvent(raw []byte) {
	eventType := ""
	var data strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line[len("data:"):], " "), " "))
			data.WriteString("\n")
		}
	}
	payload := strings.TrimSuffix(data.String(), "\n")
	if payload == "" || payload == "[DONE]" {
		return
	}
	var ev sseEvent
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Type != "" {
		eventType = ev.Type
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "error":
	default:
		return
	}
	u.terminal = eventType
	if ev.Response != nil {
		if ev.Response.Model != "" {
			u.reportedModel = ev.Response.Model
		}
		if ev.Response.Error != nil && ev.Response.Error.Code != "" {
			u.errorCode = ev.Response.Error.Code
		}
		if us := ev.Response.Usage; us != nil {
			u.inputTokens, u.outputTokens, u.totalTokens = us.InputTokens, us.OutputTokens, us.TotalTokens
			if us.InputTokensDetails != nil {
				u.cachedTokens = us.InputTokensDetails.CachedTokens
			}
			if us.OutputTokensDetails != nil {
				u.reasoningTokens = us.OutputTokensDetails.ReasoningTokens
			}
		}
	}
	if ev.Code != "" && u.errorCode == "" {
		u.errorCode = ev.Code
	}
}

// finishJSONError parses a non-SSE JSON error body once at request end.
func (u *usageRecord) finishJSONError() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.isSSE || len(u.buf) == 0 {
		u.buf = nil
		return
	}
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(u.buf, &doc) == nil {
		u.errorCode = doc.Error.Code
		if m := upstreamStatusPattern.FindStringSubmatch(doc.Error.Message); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				u.upstreamStatus = n
			}
		}
	}
	u.buf = nil
}

// proxyRequestMetadata mirrors proxy.RequestMetadata to avoid an import cycle.
type proxyRequestMetadata struct {
	Model           string
	ReasoningEffort string
}
