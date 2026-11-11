// Package gateway fronts the managed server: API key authentication, weighted
// account scheduling, stream usage instrumentation and usage persistence. Each
// account keeps its own proxy runtime so credentials and replay caches never
// mix.
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/basispoints"
	"github.com/sunboyzoey/GPT-BPS/internal/proxy"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// ProxySource supplies the currently active outbound proxy ("" = direct). It is
// read on every runtime build so an admin change applies to the next rebuild.
type ProxySource interface {
	Get() string
}

// RuntimeConfig carries the shared upstream settings used to build runtimes.
type RuntimeConfig struct {
	Upstream      string
	Proxy         ProxySource
	Transport     string
	WSIdleTimeout time.Duration
	Verbose       bool
	// ImageRelay stages inline base64 images so the upstream can fetch them as
	// HTTPS URLs. Nil disables inline image support.
	ImageRelay *basispoints.ImageRelay
	// ModelMapper aliases the requested model before it reaches the upstream.
	// Nil leaves every model unchanged.
	ModelMapper proxy.ModelMapper
}

// Runtime is one schedulable BPS account. All mutable scheduling state is
// guarded by the scheduler mutex except the atomics below.
type Runtime struct {
	ID      string
	Name    string
	Store   *auth.Store
	Handler http.Handler
	// Models reads this account's upstream model catalog.
	Models func(ctx context.Context) (*proxy.ModelCatalog, error)

	// Serves tracks which models this account really provides, as measured by
	// fingerprint attribution. A nil map means "no capability data", which
	// routing treats as unconstrained rather than unsupported.
	servesMu sync.RWMutex
	serves   map[string]bool

	close func()

	Active        atomic.Int64
	Enabled       atomic.Bool
	AuthRequired  atomic.Bool
	CooldownUntil atomic.Int64

	// Guarded by Scheduler.mu.
	weight              int
	consecutiveFailures int
	lastErrorCode       string
}

// RuntimeInfo is the admin-facing snapshot of one runtime.
type RuntimeInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	Weight         int    `json:"weight"`
	ActiveRequests int64  `json:"active_requests"`
	AuthRequired   bool   `json:"auth_required"`
	CooldownUntil  int64  `json:"cooldown_until"`
	LastErrorCode  string `json:"last_error_code"`
}

// BuildRuntime creates a proxy server for one account. The metadata sink feeds
// usage instrumentation through the request context without touching prompt
// content.
func BuildRuntime(a store.Account, st *auth.Store, cfg RuntimeConfig) (*Runtime, error) {
	pcfg := proxyConfig(cfg)
	srv, err := newProxyServer(pcfg, st)
	if err != nil {
		return nil, fmt.Errorf("build proxy runtime for %s: %w", a.ID, err)
	}
	rt := &Runtime{
		ID:      a.ID,
		Name:    a.Name,
		Store:   st,
		Handler: srv.Handler(),
		Models:  srv.Models,
		close:   srv.CloseIdleConnections,
		weight:  a.Weight,
	}
	rt.Enabled.Store(a.Enabled)
	return rt, nil
}

// CloseIdle releases the runtime's upstream transport.
func (r *Runtime) CloseIdle() {
	if r.close != nil {
		r.close()
	}
}


// SetServes installs the measured capability set. Passing nil clears it, which
// makes routing treat this account as unconstrained.
func (r *Runtime) SetServes(models map[string]bool) {
	r.servesMu.Lock()
	defer r.servesMu.Unlock()
	r.serves = models
}

// Serves reports whether this account was measured as really providing model.
// The second result is false when no capability data is installed, letting the
// scheduler skip the constraint instead of excluding the account.
func (r *Runtime) Serves(model string) (serves bool, hasData bool) {
	r.servesMu.RLock()
	defer r.servesMu.RUnlock()
	if r.serves == nil {
		return false, false
	}
	return r.serves[model], true
}

// ServesSnapshot returns a copy of the capability set for the console.
func (r *Runtime) ServesSnapshot() map[string]bool {
	r.servesMu.RLock()
	defer r.servesMu.RUnlock()
	if r.serves == nil {
		return nil
	}
	out := make(map[string]bool, len(r.serves))
	for k, v := range r.serves {
		out[k] = v
	}
	return out
}
