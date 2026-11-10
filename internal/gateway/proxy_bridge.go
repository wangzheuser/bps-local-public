package gateway

import (
	"context"

	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/proxy"
)

// newProxyServer builds the single-account bridge. Kept as a variable so tests
// can observe construction without the network.
var newProxyServer = func(cfg proxy.Config, st *auth.Store) (*proxy.Server, error) {
	return proxy.New(cfg, st)
}

func proxyConfig(cfg RuntimeConfig) proxy.Config {
	proxyURL := ""
	if cfg.Proxy != nil {
		proxyURL = cfg.Proxy.Get()
	}
	return proxy.Config{
		Upstream:          cfg.Upstream,
		ProxyURL:          proxyURL,
		UpstreamTransport: cfg.Transport,
		WSIdleTimeout:     cfg.WSIdleTimeout,
		Verbose:           cfg.Verbose,
		ImageRelay:        cfg.ImageRelay,
		RequestMetadataSink: func(ctx context.Context, meta proxy.RequestMetadata) {
			if rec := recordFromContext(ctx); rec != nil {
				rec.setRequestMeta(proxyRequestMetadata{Model: meta.Model, ReasoningEffort: meta.ReasoningEffort})
			}
		},
	}
}
