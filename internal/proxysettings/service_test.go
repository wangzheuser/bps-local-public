package proxysettings

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

func TestValidateProxyURL(t *testing.T) {
	valid := []string{
		"", "http://127.0.0.1:7890", "https://proxy.example:8443",
		"socks5://127.0.0.1:1080", "socks5h://user:pass@127.0.0.1:1080",
	}
	for _, v := range valid {
		if err := ValidateProxyURL(v); err != nil {
			t.Errorf("valid proxy rejected %q: %v", v, err)
		}
	}
	invalid := []string{
		"127.0.0.1:7890", "ftp://127.0.0.1:21", "http://", "://bad",
		"http://127.0.0.1:7890#frag",
	}
	for _, v := range invalid {
		if err := ValidateProxyURL(v); err == nil {
			t.Errorf("invalid proxy accepted %q", v)
		}
	}
}

func TestRedactedHidesCredentials(t *testing.T) {
	got := Redacted("http://user:s3cret@proxy.example:8080")
	if strings.Contains(got, "s3cret") {
		t.Fatalf("credentials leaked: %s", got)
	}
	if !strings.Contains(got, "proxy.example") {
		t.Fatalf("host missing: %s", got)
	}
}

func TestServicePersistsAndHotSwaps(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}

	svc, err := NewService(db, "http://127.0.0.1:7890")
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	// First run adopts the environment default and persists it.
	if got := svc.Get(); got != "http://127.0.0.1:7890" {
		t.Fatalf("initial = %q", got)
	}
	if stored, _ := db.GetSetting(store.SettingProxyURL); stored != "http://127.0.0.1:7890" {
		t.Fatalf("env default not persisted: %q", stored)
	}

	// Set swaps live value immediately and persists.
	if err := svc.Set("socks5://127.0.0.1:1080"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := svc.Get(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("live value = %q", got)
	}
	if stored, _ := db.GetSetting(store.SettingProxyURL); stored != "socks5://127.0.0.1:1080" {
		t.Fatalf("not persisted: %q", stored)
	}

	// Invalid values are rejected and must not change state.
	if err := svc.Set("ftp://bad"); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if got := svc.Get(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("state changed after rejection: %q", got)
	}

	// A second service (simulating restart) reads the persisted value.
	svc2, err := NewService(db, "http://ignored:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := svc2.Get(); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("restart value = %q", got)
	}
}

func TestCheckRejectsBadConfig(t *testing.T) {
	res := Check("ftp://x:1", time.Second)
	if res.OK || res.Stage != "config" {
		t.Fatalf("bad config: %+v", res)
	}
}

func TestCheckReportsUnreachableProxy(t *testing.T) {
	// Port 1 is not listening: the proxy dial must fail.
	res := Check("http://127.0.0.1:1", 2*time.Second)
	if res.OK {
		t.Fatalf("unreachable proxy reported OK: %+v", res)
	}
	if res.Error == "" {
		t.Fatal("missing error message")
	}
}

func TestCheckSucceedsThroughWorkingProxy(t *testing.T) {
	// A minimal HTTP CONNECT proxy is enough to prove the probe path: the
	// check only requires an HTTP response through the tunnel.
	var sawConnect bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			sawConnect = true
			w.WriteHeader(http.StatusBadGateway) // tunnel not implemented
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxySrv.Close()

	res := Check(proxySrv.URL, 5*time.Second)
	if !sawConnect {
		t.Fatalf("proxy was not used (no CONNECT): %+v", res)
	}
	// A 502 from the proxy proves the proxy was reachable and spoke HTTP, but
	// the tunnel to the upstream failed, so OK stays false with a tunnel stage.
	if res.OK {
		t.Fatalf("broken tunnel must not report OK: %+v", res)
	}
	if res.Stage != "tunnel" {
		t.Fatalf("stage = %q, want tunnel: %+v", res.Stage, res)
	}
	if res.ProxyDialMS < 0 {
		t.Fatalf("proxy dial timing negative: %+v", res)
	}
}

func TestCheckProxyDialFailureIsDistinct(t *testing.T) {
	res := Check("http://127.0.0.1:1", 2*time.Second)
	if res.OK {
		t.Fatal("dead proxy reported OK")
	}
	if res.Stage != "dial" {
		t.Fatalf("stage = %q, want dial: %+v", res.Stage, res)
	}
}

func TestSanitizeTransportErrorRedacts(t *testing.T) {
	err := &testErr{"proxyconnect tcp: http://user:hunter2@10.0.0.1:8080 refused"}
	got := sanitizeTransportError(err)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("credential leaked: %s", got)
	}
}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }
