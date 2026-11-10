// Package proxysettings stores the single outbound proxy used for every
// upstream call and lets the running server switch it without a restart.
package proxysettings

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// AllowedSchemes are the proxy schemes the bridge can dial through.
var AllowedSchemes = []string{"http", "https", "socks5", "socks5h"}

// ValidateProxyURL accepts an empty string (direct connection) or a proxy URL
// with a supported scheme, host and no fragment.
func ValidateProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("代理地址无法解析")
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("代理地址必须包含协议与主机，例如 http://127.0.0.1:7890")
	}
	ok := false
	for _, s := range AllowedSchemes {
		if u.Scheme == s {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("代理协议必须是 http、https、socks5 或 socks5h")
	}
	if u.Fragment != "" {
		return fmt.Errorf("代理地址不能包含 #fragment")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("代理地址缺少主机名")
	}
	return nil
}

// Redacted returns a display form that hides proxy credentials.
func Redacted(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid)"
	}
	return u.Redacted()
}

// Holder keeps the active proxy URL. Reads are lock-free so every request can
// cheaply ask for the current value; writes swap atomically.
type Holder struct {
	value atomic.Value // string
}

// NewHolder builds a holder with an initial value.
func NewHolder(initial string) *Holder {
	h := &Holder{}
	h.value.Store(strings.TrimSpace(initial))
	return h
}

// Get returns the active proxy URL ("" means direct).
func (h *Holder) Get() string {
	if h == nil {
		return ""
	}
	v, _ := h.value.Load().(string)
	return v
}

// Set swaps the active proxy URL.
func (h *Holder) Set(raw string) { h.value.Store(strings.TrimSpace(raw)) }

// Service owns persistence plus the live holder.
type Service struct {
	db     *store.DB
	holder *Holder
}

// NewService loads the stored proxy (falling back to the environment default)
// and returns a service that writes through to both the database and holder.
func NewService(db *store.DB, envDefault string) (*Service, error) {
	stored, err := db.GetSetting(store.SettingProxyURL)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	active := stored
	if err != nil && errors.Is(err, store.ErrNotFound) {
		// First run: adopt the environment value so existing deployments keep
		// working, and persist it so the console shows the truth.
		active = strings.TrimSpace(envDefault)
		if err := db.SetSetting(store.SettingProxyURL, active); err != nil {
			return nil, err
		}
	}
	if err := ValidateProxyURL(active); err != nil {
		return nil, fmt.Errorf("stored proxy is invalid: %w", err)
	}
	return &Service{db: db, holder: NewHolder(active)}, nil
}

// Get returns the active proxy URL.
func (s *Service) Get() string { return s.holder.Get() }

// Set validates and persists a new proxy URL, then publishes it to the holder
// so subsequent upstream calls use it immediately.
func (s *Service) Set(raw string) error {
	raw = strings.TrimSpace(raw)
	if err := ValidateProxyURL(raw); err != nil {
		return err
	}
	if err := s.db.SetSetting(store.SettingProxyURL, raw); err != nil {
		return err
	}
	s.holder.Set(raw)
	return nil
}

// CheckResult reports one connectivity probe outcome.
type CheckResult struct {
	OK           bool   `json:"ok"`
	ProxyURL     string `json:"proxy_url"`
	Target       string `json:"target"`
	StatusCode   int    `json:"status_code,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	ProxyDialMS  int64  `json:"proxy_dial_ms,omitempty"`
	Error        string `json:"error,omitempty"`
	Stage        string `json:"stage,omitempty"` // config | dial | tunnel | tls | http
}

// CheckTarget is the URL probed when checking a proxy. It is the real upstream
// origin so a passing check implies the proxy can actually reach BPS.
const CheckTarget = "https://bps.openai.com/basispoints/api/responses/access"

// Check probes the given proxy (empty = direct) by fetching CheckTarget through
// it. It reports the proxy dial separately from the end-to-end fetch so the
// console can distinguish "proxy dead" from "proxy alive but upstream blocked".
func Check(raw string, timeout time.Duration) CheckResult {
	raw = strings.TrimSpace(raw)
	res := CheckResult{ProxyURL: Redacted(raw), Target: CheckTarget}
	if err := ValidateProxyURL(raw); err != nil {
		res.Error = err.Error()
		res.Stage = "config"
		return res
	}
	var proxyURL *url.URL
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			res.Error = "代理地址无法解析"
			res.Stage = "config"
			return res
		}
		proxyURL = u

		// Stage 1: prove the proxy host accepts TCP connections. This is the
		// part the admin actually configures; the end-to-end fetch below can
		// still fail because of the upstream or the proxy's own egress.
		addr := u.Host
		if u.Port() == "" {
			switch u.Scheme {
			case "https":
				addr = net.JoinHostPort(u.Hostname(), "443")
			case "socks5", "socks5h":
				addr = net.JoinHostPort(u.Hostname(), "1080")
			default:
				addr = net.JoinHostPort(u.Hostname(), "80")
			}
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		res.ProxyDialMS = time.Since(start).Milliseconds()
		if err != nil {
			res.Error = fmt.Sprintf("无法连接代理 %s：%v", addr, err)
			res.Stage = "dial"
			return res
		}
		_ = conn.Close()
	}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	transport.Proxy = nil
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// A redirect from the probe target still proves connectivity.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, CheckTarget, nil)
	if err != nil {
		res.Error = "无法构造检测请求"
		res.Stage = "config"
		return res
	}
	req.Header.Set("User-Agent", "bps-server-proxy-check/1")
	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		if proxyURL != nil {
			// The proxy accepted TCP but the tunneled request failed. That is a
			// proxy-side or upstream-side problem, not a dead proxy.
			res.Stage = "tunnel"
			res.Error = "代理可连接，但经代理访问上游失败：" + sanitizeTransportError(err)
			return res
		}
		res.Stage = classifyError(err)
		res.Error = sanitizeTransportError(err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	res.StatusCode = resp.StatusCode
	// Any HTTP status means the TCP/TLS path and proxy tunnel worked. 401 is
	// expected here because no credentials are attached.
	res.OK = true
	return res
}

func classifyError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "proxyconnect"), strings.Contains(msg, "proxyconnect tcp"):
		return "dial"
	case strings.Contains(msg, "socks"):
		return "dial"
	case strings.Contains(msg, "tls"), strings.Contains(msg, "x509"):
		return "tls"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "dial"
	default:
		return "http"
	}
}

// sanitizeTransportError keeps proxy credentials out of the admin response.
func sanitizeTransportError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "://"); i >= 0 {
		if j := strings.IndexAny(msg[i+3:], " /"); j >= 0 {
			head := msg[:i+3]
			tail := msg[i+3+j:]
			// Replace userinfo-bearing authority with a placeholder.
			return head + "[redacted]" + tail
		}
	}
	return msg
}
