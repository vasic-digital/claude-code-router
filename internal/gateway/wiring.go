package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// This file wires the real internal/router and internal/proxy packages into
// the gateway's Router and Upstream seams.
//
// messages.go deliberately declares those seams as narrow local interfaces and
// ships minimal in-package defaults, so the gateway compiles and serves before
// the richer packages exist. Those defaults are intentionally dumb: the
// built-in router only ever resolves Router.default, so haiku-tier requests
// would be sent to the expensive model rather than the configured background
// one — correct-but-wasteful behaviour that must not reach production.
//
// The two adapters below exist because the packages' signatures differ
// slightly from the seams (pointer vs value receivers, and proxy.Do takes an
// explicit stream flag). Adapting here keeps both packages' own APIs idiomatic
// instead of bending one to the other.

// routerAdapter turns router.Select into the gateway's Router seam.
type routerAdapter struct{ cfg *config.Config }

// Route delegates to router.Select, which implements the full policy:
// haiku-tier -> Router.background, otherwise Router.default, falling back to
// the first provider, and returning a named error rather than guessing an
// upstream when nothing is routable.
func (r routerAdapter) Route(req *translate.AnthropicRequest) (config.Provider, string, error) {
	p, model, err := router.Select(r.cfg, req)
	if err != nil {
		return config.Provider{}, "", err
	}
	// Select returns a pointer into the config; copy it so a handler can never
	// mutate shared configuration state through the returned value.
	return *p, model, nil
}

// upstreamAdapter turns *proxy.Client into the gateway's Upstream seam.
type upstreamAdapter struct{ client *proxy.Client }

// Do forwards to proxy.Client.Do.
//
// The stream flag is derived from the request body rather than threaded
// through the seam: proxy.Do only uses it to set Accept: text/event-stream,
// and sending that header on a non-streaming request is harmless, whereas
// omitting it on a streaming one can make some upstreams buffer the whole
// response. Passing true unconditionally is therefore the safe default, and
// avoids widening the Upstream interface for a single header.
func (u upstreamAdapter) Do(ctx context.Context, p config.Provider, body []byte) (*http.Response, error) {
	return u.client.Do(ctx, &p, body, true)
}

// WireDefaults installs the production router and upstream on s.
//
// Call this after New. It is a separate step, not part of New, so tests can
// keep using the minimal in-package defaults or inject their own fakes.
func (s *Server) WireDefaults(timeout time.Duration) {
	if timeout <= 0 {
		timeout = s.opt.UpstreamTimeout
	}
	s.Router = routerAdapter{cfg: s.cfg}
	s.Upstream = upstreamAdapter{client: proxy.New(timeout)}
}

// TransformerOptionsFor exposes the provider's configured transformers
// (cleancache, streamoptions) for the request-translation step.
func TransformerOptionsFor(p *config.Provider) translate.Options {
	return router.TransformerOptions(p)
}
