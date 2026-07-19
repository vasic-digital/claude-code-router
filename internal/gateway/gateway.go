// Package gateway serves the Anthropic-compatible endpoint that Claude Code
// talks to, translating each request to the routed upstream provider.
//
// # Transport strategy
//
// The listener offers, in descending preference:
//
//   - HTTP/3 over QUIC, advertised via the Alt-Svc header, when TLS is enabled.
//   - HTTP/2 over TLS (ALPN "h2").
//   - HTTP/1.1, always, as the universal fallback.
//
// HTTP/3 requires TLS: QUIC has no cleartext mode. The default local bind is
// plain HTTP on 127.0.0.1 because that is what Claude Code and the existing
// toolkit expect, so HTTP/3 is opt-in via TLS configuration rather than the
// default. This is a deliberate compatibility choice, not an oversight.
//
// Content encoding negotiates brotli first (best ratio for JSON/SSE), then
// gzip, then identity — strictly honouring the client's Accept-Encoding.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quic-go/quic-go/http3"

	"github.com/vasic-digital/claude-code-router/internal/cache"
	"github.com/vasic-digital/claude-code-router/internal/config"
)

// Options configures a Server.
type Options struct {
	Host string
	Port int
	// CertFile/KeyFile enable TLS. HTTP/3 is only served when both are set,
	// because QUIC mandates TLS.
	CertFile string
	KeyFile  string
	// EnableHTTP3 additionally serves QUIC and advertises Alt-Svc.
	EnableHTTP3 bool
	// UpstreamTimeout bounds a single upstream call. Streaming responses are
	// exempt: an SSE session legitimately outlives any fixed deadline.
	UpstreamTimeout time.Duration
	// MaxAttempts caps the number of upstream attempts a single /v1/messages
	// request will make: one initial try plus up to MaxAttempts-1 automatic
	// retries, gated by internal/router's Retryable/Terminal classification
	// (see messages.go's doUpstreamWithRetry). Zero or negative means
	// "unset" and falls back to defaultMaxAttempts; tests lower this to
	// bound how many upstream calls a retry scenario needs to arrange.
	MaxAttempts int
	// APIKeys, when non-empty, are the client-presented keys RequireAPIKey
	// accepts on POST /v1/messages (via "Authorization: Bearer <key>" or
	// "x-api-key: <key>"). /health and /ready are never gated by this — a
	// supervisor must always be able to probe liveness/readiness regardless
	// of auth configuration. An EMPTY (the default, zero-value) list leaves
	// /v1/messages unauthenticated too: see auth.go's package doc for why
	// that default is deliberate — the toolkit that drives this gateway
	// today sends no client key at all, on a loopback-only listener.
	APIKeys []string
	// Logger is the structured logger the per-request access-logging
	// middleware writes to (see logging_middleware.go, mounted in routes()).
	// A nil Logger (the zero-value default) means "use an env-configured
	// redacting logger to os.Stderr" — LoggingMiddleware itself does that
	// fallback via internal/logging.New, so CCR_LOG_LEVEL / CCR_LOG_FORMAT are
	// honoured out of the box without a caller wiring anything. It is a
	// *slog.Logger from internal/logging (New / NewWithOptions), never a raw
	// slog logger: every logger this package accepts must carry the redaction
	// guarantee. Tests point this at a bytes.Buffer to capture output.
	Logger *slog.Logger
}

// defaultMaxAttempts is the number of upstream attempts a request gets when
// Options.MaxAttempts is left unset (see doUpstreamWithRetry in
// messages.go): one initial try plus up to two automatic retries.
const defaultMaxAttempts = 3

// Server is the gateway listener.
type Server struct {
	opt   Options
	cfg   *config.Config
	eng   *gin.Engine
	h1h2  *http.Server
	h3    *http3.Server
	ready chan struct{}

	// Router and Upstream are overridable seams for the full routing and
	// upstream-proxy implementations (internal/router, internal/proxy), which
	// this package deliberately does not import. New wires in minimal working
	// defaults (defaultRouter, defaultUpstream — see messages.go) so the
	// gateway is functional standalone; a caller that owns the fuller
	// implementations may replace either field after construction, before
	// Start.
	Router   Router
	Upstream Upstream

	// Cache is the optional response cache (see internal/cache). It is a seam
	// like Router/Upstream: New leaves it NIL, in which case the request path
	// is byte-identical to a build with no cache — no lookup, no store. A
	// caller that wants caching builds a store with BuildCache and assigns it
	// (serve.go does this from Config.Cache), before Start.
	Cache cache.Cache
	// CacheAllowToolResponses mirrors Config.Cache.AllowToolResponses: it is the
	// argument handed to cache.ResponseCacheable on the store side, so a
	// tool-call response is only ever cached when the operator opted in. It is
	// inert when Cache is nil.
	CacheAllowToolResponses bool
}

// defaultCacheMaxEntries bounds the in-memory LRU when Config.Cache.MaxEntries
// is left at 0 ("use a sane default").
const defaultCacheMaxEntries = 1024

// BuildCache constructs the Cache described by c, or (nil, nil) when caching is
// disabled (c == nil or c.Enabled == false). It is the single place that turns
// a validated *config.CacheConfig into a live store:
//
//   - "" / "memory": an in-process LRU (cache.NewMemoryLRU), bounded by
//     MaxEntries (defaulting to defaultCacheMaxEntries) and TTLSeconds.
//   - "sqlite": a persistent store (cache.NewSQLiteCache) at Path.
//
// New never calls this (it must not fail, and must not touch the filesystem);
// the caller does, so a sqlite open error is surfaced to a place that can log
// it and fall back to caching-disabled rather than crash the process.
func BuildCache(c *config.CacheConfig) (cache.Cache, error) {
	if c == nil || !c.Enabled {
		return nil, nil
	}
	ttl := time.Duration(c.TTLSeconds) * time.Second
	switch c.Backend {
	case "", "memory":
		maxEntries := c.MaxEntries
		if maxEntries <= 0 {
			maxEntries = defaultCacheMaxEntries
		}
		return cache.NewMemoryLRU(cache.MemoryOptions{MaxEntries: maxEntries, TTL: ttl}), nil
	case "sqlite":
		if c.Path == "" {
			return nil, fmt.Errorf("cache: sqlite backend requires a path")
		}
		return cache.NewSQLiteCache(c.Path, ttl)
	default:
		return nil, fmt.Errorf("cache: unknown backend %q", c.Backend)
	}
}

// New builds a Server. It does not listen until Start is called.
func New(cfg *config.Config, opt Options) *Server {
	if opt.Host == "" {
		opt.Host = "127.0.0.1"
	}
	if opt.Port == 0 {
		opt.Port = 3456
	}
	if opt.UpstreamTimeout == 0 {
		opt.UpstreamTimeout = 10 * time.Minute
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = defaultMaxAttempts
	}
	gin.SetMode(gin.ReleaseMode)
	// gin.New() (not gin.Default()) so NO middleware is pre-installed here:
	// panic recovery is mounted in routes() INSIDE LoggingMiddleware, so that a
	// recovered 500 is still access-logged. Mounting gin.Recovery() here would
	// make it the outermost middleware, and a panicking handler would unwind
	// past LoggingMiddleware's post-c.Next() log call before Recovery caught it —
	// leaving the panic request unlogged. See routes() for the ordering.
	eng := gin.New()

	s := &Server{opt: opt, cfg: cfg, eng: eng, ready: make(chan struct{})}
	s.Router = defaultRouter{cfg: cfg}
	s.Upstream = &defaultUpstream{}
	s.routes()
	return s
}

// Addr is the host:port the server binds.
func (s *Server) Addr() string { return fmt.Sprintf("%s:%d", s.opt.Host, s.opt.Port) }

// Handler exposes the router for testing without binding a socket.
func (s *Server) Handler() http.Handler { return s.eng }

func (s *Server) routes() {
	// Access logging is mounted FIRST, so it is the OUTERMOST middleware:
	//
	//   - It logs EVERY inbound request exactly once — /health, /ready, the
	//     inbound completion endpoints, and requests RequireAPIKey rejects with
	//     401 alike — because it wraps the whole chain via s.eng.Use rather
	//     than being route-scoped. It never gates anything (it always calls
	//     c.Next()), so unlike RequireAPIKey it is safe as a global middleware:
	//     /health and /ready keep answering unchanged, they are merely logged.
	//   - Sitting OUTSIDE compressionMiddleware means it observes the final
	//     response status and the actual (post-compression) byte count after
	//     the entire chain has run, and it sets the X-Request-Id response
	//     header before any handler writes — an inner position would capture a
	//     not-yet-flushed byte count.
	//   - It logs only request/response METADATA (method, path, status,
	//     duration, bytes, request id). It never reads either body and never
	//     logs any header value, so an inbound Authorization/x-api-key
	//     credential and all prompt/completion content are structurally absent
	//     from the log; the internal/logging redactor backing the logger is a
	//     second line of defence, not the primary guarantee.
	s.eng.Use(LoggingMiddleware(s.opt.Logger))

	// Panic recovery is mounted INSIDE LoggingMiddleware (registered after it,
	// so it wraps the handlers but is itself wrapped by logging). gin.Recovery
	// recovers a panicking handler and writes a 500, then returns NORMALLY to
	// its caller — which is LoggingMiddleware, whose post-c.Next() code then
	// runs and logs that recovered 500 (correct status and byte count included).
	// The reverse order — Recovery outermost — would let the panic unwind past
	// LoggingMiddleware's log call, so panic requests would escape the access
	// log entirely.
	s.eng.Use(gin.Recovery())
	s.eng.Use(compressionMiddleware())
	if s.opt.EnableHTTP3 {
		s.eng.Use(altSvcMiddleware(s.opt.Port))
	}

	// Liveness. Deliberately unauthenticated and dependency-free so a
	// supervisor can distinguish "process up" from "upstream reachable".
	s.eng.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"providers": len(s.cfg.Providers),
		})
	})

	// Readiness: green only when the router can actually pick an upstream.
	s.eng.GET("/ready", func(c *gin.Context) {
		if len(s.cfg.Providers) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "no providers configured",
			})
			return
		}
		if s.cfg.Router.Default == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "no default route configured",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// The inbound completion endpoints. Every routable POST path is dispatched
	// through the SAME classifier-driven entrypoint (handleInbound, which calls
	// requestProtocolForPath), so the ported protocol classifier is genuinely
	// load-bearing rather than dead code:
	//   - Anthropic Messages (/v1/messages)      — the endpoint Claude Code talks to;
	//   - OpenAI chat-completions (/v1/chat/completions) — the OpenAI-compatible
	//     inbound facade (see openai_inbound.go), so an OpenAI-SDK client can
	//     reach any OpenAI-shaped provider.
	// Both are also exposed under the "/proxy/v1/..." alias upstream uses.
	//
	// RequireAPIKey is mounted HERE ONLY, as route-scoped middleware — NOT via
	// s.eng.Use, which would also gate /health and /ready above and break
	// liveness/readiness probing the moment APIKeys is configured. When
	// s.opt.APIKeys is empty (the zero-value default), RequireAPIKey itself
	// disables auth entirely (see auth.go's package doc): the toolkit that
	// drives this gateway today sends no client key at all, and must keep
	// working unchanged.
	inbound := RequireAPIKey(s.opt.APIKeys)
	for _, p := range []string{
		"/v1/messages", "/proxy/v1/messages",
		"/v1/chat/completions", "/proxy/v1/chat/completions",
	} {
		s.eng.POST(p, inbound, s.handleInbound)
	}
}

// Start binds and serves. It returns once the listener is up; serving
// continues in the background until Shutdown.
func (s *Server) Start() error {
	s.h1h2 = &http.Server{
		Addr:              s.Addr(),
		Handler:           s.eng,
		ReadHeaderTimeout: 10 * time.Second,
	}

	tlsEnabled := s.opt.CertFile != "" && s.opt.KeyFile != ""
	if s.opt.EnableHTTP3 && !tlsEnabled {
		// Fail loudly: silently downgrading to HTTP/1.1 after the operator
		// explicitly asked for HTTP/3 would be a lie about the transport.
		return errors.New("HTTP/3 requires TLS: set both CertFile and KeyFile (QUIC has no cleartext mode)")
	}

	if s.opt.EnableHTTP3 {
		s.h3 = &http3.Server{Addr: s.Addr(), Handler: s.eng}
		go func() { _ = s.h3.ListenAndServeTLS(s.opt.CertFile, s.opt.KeyFile) }()
	}

	go func() {
		var err error
		if tlsEnabled {
			err = s.h1h2.ListenAndServeTLS(s.opt.CertFile, s.opt.KeyFile)
		} else {
			err = s.h1h2.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Surface rather than swallow; the supervisor decides.
			fmt.Printf("gateway: listener stopped: %v\n", err)
		}
	}()
	close(s.ready)
	return nil
}

// Shutdown gracefully stops every listener.
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error
	if s.h3 != nil {
		if err := s.h3.Close(); err != nil {
			firstErr = err
		}
	}
	if s.h1h2 != nil {
		if err := s.h1h2.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
