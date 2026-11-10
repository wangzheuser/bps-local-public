package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExcelClientID identifies the Excel application; it is not a per-login secret.
const ExcelClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
const deviceRedirect = "https://auth.openai.com/basispoints/deviceauth/callback"

// DeviceClient implements the device flow observed in the user's Excel capture.
// Refresh-token rotation is deliberately separate and is not performed here.
type DeviceClient struct {
	HTTP *http.Client
	base string
	wait func(context.Context, time.Duration) error
}

type DevicePrompt struct{ URL, UserCode string }
type DeviceCredentials struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token,omitempty"`
	IDToken           string          `json:"id_token,omitempty"`
	TokenType         string          `json:"token_type"`
	ExpiresIn         int64           `json:"expires_in"`
	Scope             string          `json:"scope"`
	ObtainedAt        string          `json:"obtained_at"`
	ClientID          string          `json:"client_id"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at,omitempty"`
}

func NewDeviceClient(proxyAddress string) (*DeviceClient, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxyAddress != "" {
		u, e := url.Parse(proxyAddress)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return nil, errors.New("invalid OAuth proxy URL")
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &DeviceClient{HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: "https://auth.openai.com", wait: waitDevice}, nil
}
func waitDevice(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *DeviceClient) post(ctx context.Context, path, kind string, data io.Reader, out any) (int, string, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, data)
	if e != nil {
		return 0, "", errors.New("cannot construct OAuth request")
	}
	req.Header.Set("Content-Type", kind)
	req.Header.Set("Accept", "application/json")
	resp, e := c.HTTP.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		return 0, "", errors.New("OAuth transport failed (check proxy and connectivity)")
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return resp.StatusCode, "", errors.New("cannot read OAuth response")
	}
	if resp.StatusCode != 200 {
		var v struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(raw, &v)
		var code string
		if json.Unmarshal(v.Error, &code) != nil {
			var detail struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(v.Error, &detail)
			code = detail.Code
		}
		// Only known protocol codes may leave this boundary; descriptions and
		// arbitrary identifiers can contain credential material.
		switch code {
		case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "access_denied", "authorization_pending", "deviceauth_authorization_pending", "slow_down", "expired_token", "server_error", "temporarily_unavailable":
		default:
			code = ""
		}
		return resp.StatusCode, code, nil
	}
	if json.Unmarshal(raw, out) != nil {
		return 200, "", errors.New("invalid OAuth JSON response")
	}
	return 200, "", nil
}
func jsonBody(v any) io.Reader { b, _ := json.Marshal(v); return strings.NewReader(string(b)) }

// Login requires manual browser approval. No browser credentials are read.
func (c *DeviceClient) Login(ctx context.Context, show func(DevicePrompt) error) (*DeviceCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var start struct {
		ID       string          `json:"device_auth_id"`
		Code     string          `json:"user_code"`
		Interval json.RawMessage `json:"interval"`
	}
	status, code, e := c.post(ctx, "/api/accounts/deviceauth/usercode", "application/json", jsonBody(map[string]string{"client_id": ExcelClientID}), &start)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, fmt.Errorf("device-code request failed: HTTP %d (%s)", status, code)
	}
	if start.ID == "" || start.Code == "" {
		return nil, errors.New("device-code response missing required fields")
	}
	seconds := 5
	if len(start.Interval) > 0 {
		n, err := strconv.Atoi(strings.Trim(string(start.Interval), `"`))
		if err != nil || n < 1 || n > 300 {
			return nil, errors.New("invalid device polling interval")
		}
		seconds = n
	}
	// The authorize page requires the user_code in the query string. Without it
	// the endpoint redirects into the regular OAuth authorization flow, which
	// this client is not registered for and answers with deviceauth_disabled.
	authorizeURL := c.base + "/api/accounts/deviceauth/authorize?client_id=" +
		url.QueryEscape(ExcelClientID) + "&user_code=" + url.QueryEscape(start.Code)
	if e = show(DevicePrompt{URL: authorizeURL, UserCode: start.Code}); e != nil {
		return nil, e
	}
	var grant struct {
		Code      string `json:"authorization_code"`
		Verifier  string `json:"code_verifier"`
		Challenge string `json:"code_challenge"`
	}
	for {
		if e = c.wait(ctx, time.Duration(seconds)*time.Second); e != nil {
			return nil, e
		}
		status, code, e = c.post(ctx, "/api/accounts/deviceauth/token", "application/json", jsonBody(map[string]string{"client_id": ExcelClientID, "device_auth_id": start.ID, "user_code": start.Code}), &grant)
		if e != nil {
			return nil, e
		}
		if status == 200 {
			break
		}
		if status == 403 && code == "deviceauth_authorization_pending" {
			continue
		}
		return nil, fmt.Errorf("device authorization failed: HTTP %d (%s)", status, code)
	}
	if grant.Code == "" || len(grant.Verifier) < 43 || len(grant.Verifier) > 128 || grant.Challenge == "" {
		return nil, errors.New("device authorization missing valid code/PKCE fields")
	}
	digest := sha256.Sum256([]byte(grant.Verifier))
	if base64.RawURLEncoding.EncodeToString(digest[:]) != grant.Challenge {
		return nil, errors.New("device authorization PKCE mismatch")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {grant.Code}, "client_id": {ExcelClientID}, "redirect_uri": {deviceRedirect}, "code_verifier": {grant.Verifier}}
	var creds DeviceCredentials
	status, code, e = c.post(ctx, "/oauth/token?unified=true", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), &creds)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, fmt.Errorf("OAuth token exchange failed: HTTP %d (%s)", status, code)
	}
	if creds.AccessToken == "" || !strings.EqualFold(creds.TokenType, "Bearer") || creds.ExpiresIn <= 0 || creds.ExpiresIn > 31536000 {
		return nil, errors.New("invalid OAuth token response")
	}
	creds.ObtainedAt = time.Now().UTC().Format(time.RFC3339)
	creds.ClientID = ExcelClientID
	return &creds, nil
}

// SaveDeviceCredentials atomically publishes a new 0600 file, never replacing
// an existing credential file or symlink (including a concurrent writer).
func SaveDeviceCredentials(path string, creds *DeviceCredentials) error {
	if path == "" {
		return errors.New("empty OAuth destination")
	}
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".bps-oauth-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = json.NewEncoder(f).Encode(creds); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Link(f.Name(), path); e != nil {
		return fmt.Errorf("cannot save OAuth file (existing files are never overwritten): %w", e)
	}
	return nil
}
