// Command bps-server runs the managed multi-account bridge: a client-facing
// Responses API with API keys and account scheduling, plus an admin Web UI.
//
// It reads configuration from environment variables, keeps account metadata and
// usage statistics in SQLite under BPS_DATA_DIR, and stores OAuth credentials
// as isolated files in BPS_DATA_DIR/accounts. Credential tokens never enter the
// database, logs or admin responses.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/accounts"
	"github.com/sunboyzoey/GPT-BPS/internal/apikeys"
	"github.com/sunboyzoey/GPT-BPS/internal/admin"
	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
	"github.com/sunboyzoey/GPT-BPS/internal/gateway"
	"github.com/sunboyzoey/GPT-BPS/internal/proxysettings"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

type config struct {
	DataDir          string
	APIAddr          string
	AdminAddr        string
	ProxyURL         string
	Transport        string
	WSIdleTimeout    time.Duration
	AdminPassword    string
	CookieSecure     bool
	SessionTTL       time.Duration
	UsageRetention   time.Duration
	// PublicBaseURL is the HTTPS origin the upstream can reach to fetch staged
	// images. Empty disables inline base64 image support.
	PublicBaseURL string
}

var version = "0.1.0-managed-dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("[bps-server] ")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "bps.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	sched := gateway.NewScheduler()
	gw := gateway.NewGateway(db, sched)

	proxySvc, err := proxysettings.NewService(db, cfg.ProxyURL)
	if err != nil {
		log.Fatalf("proxy settings: %v", err)
	}
	if active := proxySvc.Get(); active != "" {
		log.Printf("outbound proxy: %s", proxysettings.Redacted(active))
	}

	// Stage inline base64 images so the upstream can fetch them as HTTPS URLs.
	// Disabled unless a public origin is configured, because the upstream must
	// be able to reach it.
	var relay *basispoints.ImageRelay
	if cfg.PublicBaseURL != "" {
		relay, err = basispoints.NewImageRelay(cfg.PublicBaseURL, filepath.Join(cfg.DataDir, "image-relay"))
		if err != nil {
			log.Fatalf("image relay: %v", err)
		}
		defer func() { _ = relay.Close() }()
		log.Printf("inline image relay enabled: %s%s", cfg.PublicBaseURL, basispoints.ImageRelayPath)
		// The gateway serves the staged URLs; account runtimes rewrite the
		// inline data URLs into them.
		gw.SetImageRelay(relay)
	}

	manager := &runtimeManager{sched: sched, db: db, proxy: proxySvc, cfg: runtimeConfig(cfg, proxySvc, relay, db)}
	svc, err := accounts.NewService(db, filepath.Join(cfg.DataDir, "accounts"), proxySvc.Get, manager, nil)
	if err != nil {
		log.Fatalf("accounts: %v", err)
	}

	// Load previously authorized accounts. Failures are recorded, not fatal:
	// a temporarily unreadable credential file must not take the server down.
	if err := loadExistingAccounts(db, svc, manager, sched); err != nil {
		log.Printf("startup account load: %v", err)
	}

	// The sealer keeps API keys recoverable for the console without storing
	// plaintext; its secret lives beside the data, never in the database.
	sealer, err := apikeys.OpenSealer(cfg.DataDir)
	if err != nil {
		log.Fatalf("api key sealer: %v", err)
	}

	adminHandler := admin.NewHandler(admin.Deps{
		DB: db, Scheduler: sched, Accounts: svc, Proxy: proxySvc, Runtime: manager, Sealer: sealer,
		Images: relay,
	}, admin.NewSessionManager(cfg.AdminPassword, cfg.SessionTTL, cfg.CookieSecure), admin.WebFS())

	apiSrv := &http.Server{Addr: cfg.APIAddr, Handler: gw.APIHandler(), ReadHeaderTimeout: 30 * time.Second}
	adminSrv := &http.Server{Addr: cfg.AdminAddr, Handler: adminHandler, ReadHeaderTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// Stay well inside the docker stop window (default 10s): the two
		// HTTP servers shut down under one 4s budget, and auth-task
		// cancellation waits in parallel.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = adminSrv.Shutdown(shutdownCtx)
		_ = apiSrv.Shutdown(shutdownCtx)
		svc.Shutdown()
	}()

	// Usage retention cleanup: once at startup, then daily.
	cutoff := func() int64 {
		return time.Now().Add(-cfg.UsageRetention).UnixMilli()
	}
	if n, err := db.DeleteUsageBefore(cutoff()); err == nil && n > 0 {
		log.Printf("usage retention: removed %d records", n)
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := db.DeleteUsageBefore(cutoff()); err != nil {
					log.Printf("usage retention: %v", err)
				} else if n > 0 {
					log.Printf("usage retention: removed %d records", n)
				}
			}
		}
	}()

	errCh := make(chan error, 2)
	go func() {
		log.Printf("admin console listening on http://%s", cfg.AdminAddr)
		// ErrServerClosed is expected on graceful shutdown and also lands
		// here; main counts both returns before exiting so the process
		// terminates promptly instead of blocking forever on a filtered
		// channel read.
		errCh <- adminSrv.ListenAndServe()
	}()
	log.Printf("responses API listening on http://%s (api key required)", cfg.APIAddr)
	log.Printf("bps-server %s, data dir %s", version, cfg.DataDir)
	go func() {
		errCh <- apiSrv.ListenAndServe()
	}()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("%v", err)
		}
	}
	log.Printf("bps-server stopped")
}

// runtimeConfig adapts main config into gateway.RuntimeConfig.
func runtimeConfig(cfg config, proxy gateway.ProxySource, relay *basispoints.ImageRelay, db *store.DB) gateway.RuntimeConfig {
	return gateway.RuntimeConfig{
		Upstream:      basispoints.ResponsesURL,
		Proxy:         proxy,
		Transport:     cfg.Transport,
		WSIdleTimeout: cfg.WSIdleTimeout,
		ImageRelay:    relay,
		// Model aliases are read per request so console edits apply without a
		// restart. A missing mapping passes the model through untouched.
		ModelMapper: func(source string) (string, error) {
			target, err := db.GetModelMapping(source)
			if errors.Is(err, store.ErrNotFound) {
				return "", nil
			}
			return target, err
		},
	}
}

// runtimeManager implements accounts.RuntimeManager by building proxy runtimes
// and swapping them into the scheduler with in-flight drain. It also owns the
// proxy hot-swap: a saved proxy rebuilds every runtime so the next request uses
// the new route.
type runtimeManager struct {
	sched *gateway.Scheduler
	db    *store.DB
	proxy *proxysettings.Service
	cfg   gateway.RuntimeConfig
}

func (m *runtimeManager) EnsureRuntime(a store.Account, st *auth.Store) error {
	rt, err := gateway.BuildRuntime(a, st, m.cfg)
	if err != nil {
		return err
	}
	if old := m.sched.Get(a.ID); old != nil {
		m.sched.Upsert(rt)
		m.drain(old)
	} else {
		m.sched.Upsert(rt)
	}
	return nil
}

func (m *runtimeManager) HasRuntime(id string) bool { return m.sched.HasRuntime(id) }

// RemoveRuntime stops scheduling the account immediately and releases its
// transport once in-flight requests finish.
func (m *runtimeManager) RemoveRuntime(id string) {
	rt := m.sched.Get(id)
	if rt == nil {
		return
	}
	m.sched.Remove(id)
	go m.drain(rt)
}

// ApplyProxy persists a new proxy, then rebuilds every loaded account runtime
// so subsequent upstream requests (and device authorizations) use it at once.
// In-flight requests keep draining on the old transport.
func (m *runtimeManager) ApplyProxy(raw string) error {
	if err := m.proxy.Set(raw); err != nil {
		return err
	}
	if raw == "" {
		log.Printf("outbound proxy cleared: upstream requests now connect directly")
	} else {
		log.Printf("outbound proxy updated: %s", proxysettings.Redacted(raw))
	}
	accounts, err := m.db.ListAccounts()
	if err != nil {
		return err
	}
	var rebuilt, failed int
	for _, a := range accounts {
		if a.UpstreamAccountID == "" {
			continue
		}
		old := m.sched.Get(a.ID)
		if old == nil {
			continue // not loaded; will pick up the new proxy on next load
		}
		st, err := auth.Load(a.CredentialPath)
		if err != nil {
			failed++
			log.Printf("proxy rebuild: account %s credential load failed: %v", a.ID, err)
			continue
		}
		if err := m.EnsureRuntime(*a, st); err != nil {
			failed++
			log.Printf("proxy rebuild: account %s runtime build failed: %v", a.ID, err)
			continue
		}
		rebuilt++
	}
	log.Printf("proxy applied: rebuilt %d account runtime(s), %d failed", rebuilt, failed)
	return nil
}

// drain waits for in-flight requests then releases the old transport.
func (m *runtimeManager) drain(old *gateway.Runtime) {
	deadline := time.Now().Add(5 * time.Minute)
	go func() {
		for old.Active.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		old.CloseIdle()
	}()
}

func loadExistingAccounts(db *store.DB, svc *accounts.Service, manager *runtimeManager, sched *gateway.Scheduler) error {
	rows, err := db.ListAccounts()
	if err != nil {
		return err
	}
	for _, a := range rows {
		if a.UpstreamAccountID == "" {
			continue
		}
		st, err := auth.Load(a.CredentialPath)
		if err != nil {
			_ = db.SetAccountError(a.ID, "credential_load_failed", 0)
			log.Printf("account %s (%s): credential load failed: %v", a.Name, a.ID, err)
			continue
		}
		if err := manager.EnsureRuntime(*a, st); err != nil {
			_ = db.SetAccountError(a.ID, "runtime_build_failed", 0)
			log.Printf("account %s (%s): runtime build failed: %v", a.Name, a.ID, err)
			continue
		}
		log.Printf("account %s (%s) loaded, enabled=%t", a.Name, a.ID, a.Enabled)
	}
	return nil
}

func loadConfig() (config, error) {
	cfg := config{
		DataDir:        env("BPS_DATA_DIR", "/data"),
		APIAddr:        env("BPS_API_ADDR", "0.0.0.0:8790"),
		AdminAddr:      env("BPS_ADMIN_ADDR", "0.0.0.0:8791"),
		ProxyURL:       strings.TrimSpace(os.Getenv("BPS_LOCAL_PROXY")),
		Transport:      env("BPS_UPSTREAM_TRANSPORT", "ws"),
		CookieSecure:   env("BPS_COOKIE_SECURE", "false") == "true",
		PublicBaseURL:  strings.TrimSpace(os.Getenv("BPS_PUBLIC_BASE_URL")),
	}
	if cfg.Transport != "ws" && cfg.Transport != "sse" {
		return cfg, fmt.Errorf("BPS_UPSTREAM_TRANSPORT must be ws or sse, got %q", cfg.Transport)
	}
	wsIdle := env("BPS_WS_IDLE_TIMEOUT", "2m")
	d, err := time.ParseDuration(wsIdle)
	if err != nil || d <= 0 {
		return cfg, fmt.Errorf("BPS_WS_IDLE_TIMEOUT must be a positive duration, got %q", wsIdle)
	}
	cfg.WSIdleTimeout = d

	sessionTTL := env("BPS_SESSION_TTL", "12h")
	d, err = time.ParseDuration(sessionTTL)
	if err != nil || d <= 0 || d > 7*24*time.Hour {
		return cfg, fmt.Errorf("BPS_SESSION_TTL must be a positive duration up to 7d, got %q", sessionTTL)
	}
	cfg.SessionTTL = d

	retention := env("BPS_USAGE_RETENTION_DAYS", "30")
	days, err := strconv.Atoi(retention)
	if err != nil || days < 1 || days > 3650 {
		return cfg, fmt.Errorf("BPS_USAGE_RETENTION_DAYS must be an integer between 1 and 3650, got %q", retention)
	}
	cfg.UsageRetention = time.Duration(days) * 24 * time.Hour

	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil || u.Host == "" {
			return cfg, fmt.Errorf("BPS_LOCAL_PROXY is not a valid URL: %q", cfg.ProxyURL)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return cfg, fmt.Errorf("BPS_LOCAL_PROXY must use http, https, socks5 or socks5h, got %q", u.Scheme)
		}
	}

	for _, addr := range []string{cfg.APIAddr, cfg.AdminAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return cfg, fmt.Errorf("listen address %q is invalid: %v", addr, err)
		}
	}

	passwordPath := env("BPS_ADMIN_PASSWORD_FILE", "/run/secrets/admin_password")
	raw, err := os.ReadFile(passwordPath)
	if err != nil {
		return cfg, fmt.Errorf("read admin password file %s: %v", passwordPath, err)
	}
	cfg.AdminPassword = strings.TrimSpace(string(raw))
	if cfg.AdminPassword == "" {
		return cfg, fmt.Errorf("admin password file %s is empty", passwordPath)
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
