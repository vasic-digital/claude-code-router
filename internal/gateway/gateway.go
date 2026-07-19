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
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quic-go/quic-go/http3"

	"github.com/vasic-digital/claude-code-router/internal/cache"
	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/metrics"
	"github.com/vasic-digital/claude-code-router/internal/translate"
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
	// accepts on ALL FOUR completion routes (/v1/messages, /v1/chat/completions,
	// and their /proxy aliases), via "Authorization: Bearer <key>" or
	// "x-api-key: <key>". /health and /ready are never gated by this — a
	// supervisor must always be able to probe liveness/readiness regardless
	// of auth configuration. An EMPTY (the default, zero-value) list leaves
	// those routes unauthenticated too: see auth.go's package doc for why
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
	//
	// Its type is the local ResponseCache interface, NOT cache.Cache, so the
	// gateway can be handed either the exact-only store (wrapped in
	// exactCacheAdapter) or a *cache.SemanticCache without the request path
	// caring which — both expose the same request-aware Lookup/Store.
	Cache ResponseCache
	// CacheAllowToolResponses mirrors Config.Cache.AllowToolResponses: it is the
	// argument handed to cache.ResponseCacheable on the store side, so a
	// tool-call response is only ever cached when the operator opted in. It is
	// inert when Cache is nil.
	CacheAllowToolResponses bool

	// Metrics records the RED HTTP triple plus the GenAI token/upstream/cache
	// counters (see internal/metrics). New sets a non-nil default Recorder so a
	// standalone gateway (and every test) records without any wiring; serve.go
	// overrides it with the ONE process-wide Recorder it also mounts on the
	// management server's /metrics. The recording seams (the routes() middleware
	// and the messages.go hooks) tolerate a nil value, so a caller that
	// deliberately clears it keeps the request path byte-identical.
	Metrics *metrics.Recorder
}

// ResponseCache is the gateway's view of the response cache. It is REQUEST-aware
// (every method takes the routed *translate.AnthropicRequest alongside the
// fingerprint key) so the same call site drives either the exact-only tier or
// the semantic tier without branching:
//
//   - exactCacheAdapter wraps a plain cache.Cache (fingerprint-only); it ignores
//     req and always reports HitExact / HitNone.
//   - *cache.SemanticCache satisfies this interface directly: it consults the
//     exact tier first, then (on an exact miss) the request's salient text.
//
// Keeping this an EXPORTED interface lets serve.go type BuildCache's result and
// gw.Cache uniformly, and lets tests inject either shape.
type ResponseCache interface {
	Lookup(key string, req *translate.AnthropicRequest) (*cache.Entry, cache.HitKind, bool)
	Store(key string, req *translate.AnthropicRequest, e *cache.Entry) error
	Close() error
}

// exactCacheAdapter adapts a fingerprint-only cache.Cache to the request-aware
// ResponseCache interface. It discards req (the exact tier keys purely on the
// fingerprint) and reports every hit as HitExact — there is no similarity tier
// behind it.
type exactCacheAdapter struct{ c cache.Cache }

func (a exactCacheAdapter) Lookup(key string, _ *translate.AnthropicRequest) (*cache.Entry, cache.HitKind, bool) {
	if e, ok := a.c.Lookup(key); ok {
		return e, cache.HitExact, true
	}
	return nil, cache.HitNone, false
}

func (a exactCacheAdapter) Store(key string, _ *translate.AnthropicRequest, e *cache.Entry) error {
	return a.c.Store(key, e)
}

func (a exactCacheAdapter) Close() error { return a.c.Close() }

// defaultCacheMaxEntries bounds the in-memory LRU when Config.Cache.MaxEntries
// is left at 0 ("use a sane default").
const defaultCacheMaxEntries = 1024

// defaultSemanticThreshold is the cosine floor a near-duplicate must clear when
// Config.Cache.Semantic is on but SemanticThreshold is left at 0. ~0.85 is the
// documented near-duplicate band for the local lexical embedder (a re-ask, a
// retry, or a one-word edit clears it; unrelated text does not).
const defaultSemanticThreshold = 0.85

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
//
// The result is a ResponseCache: the exact store (memory or sqlite) is built as
// before, then EITHER wrapped in exactCacheAdapter (byte-identical, exact-only
// behaviour — the default) OR, when c.Semantic is set, wrapped in a
// cache.SemanticCache driven by the local lexical embedder and c's threshold
// (or defaultSemanticThreshold when the threshold is left at 0).
func BuildCache(c *config.CacheConfig) (ResponseCache, error) {
	if c == nil || !c.Enabled {
		return nil, nil
	}
	ttl := time.Duration(c.TTLSeconds) * time.Second
	var exact cache.Cache
	switch c.Backend {
	case "", "memory":
		maxEntries := c.MaxEntries
		if maxEntries <= 0 {
			maxEntries = defaultCacheMaxEntries
		}
		exact = cache.NewMemoryLRU(cache.MemoryOptions{MaxEntries: maxEntries, TTL: ttl})
	case "sqlite":
		if c.Path == "" {
			return nil, fmt.Errorf("cache: sqlite backend requires a path")
		}
		s, err := cache.NewSQLiteCache(c.Path, ttl)
		if err != nil {
			return nil, err
		}
		exact = s
	default:
		return nil, fmt.Errorf("cache: unknown backend %q", c.Backend)
	}

	if c.Semantic {
		threshold := c.SemanticThreshold
		if threshold == 0 {
			threshold = defaultSemanticThreshold
		}
		return cache.NewSemanticCache(exact, cache.NewLocalEmbedder(0), threshold), nil
	}
	return exactCacheAdapter{c: exact}, nil
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
	// A non-nil default Recorder so a standalone gateway (and every test)
	// records metrics without any wiring. serve.go overrides this with the ONE
	// process-wide Recorder it also exposes on the management /metrics endpoint.
	s.Metrics = metrics.New()
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

	// Metrics is mounted alongside logging and, like it, OUTSIDE compression and
	// panic recovery so it observes the FINAL response status (a recovered 500
	// included) after the whole chain has run. It records the RED HTTP triple for
	// EVERY request — /health and /ready included — and, exactly like
	// LoggingMiddleware, it never gates: it always calls c.Next(). The route
	// TEMPLATE (c.FullPath(), e.g. "/v1/messages"), never the raw path, is the
	// label, so cardinality stays bounded and secret-free; an unmatched path
	// (404) collapses to a single low-cardinality "/(unmatched)" bucket.
	s.eng.Use(s.metricsMiddleware())

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

// metricsMiddleware records the RED HTTP triple for every request. It bumps the
// in-flight gauge on entry, defers the decrement, times the whole chain, and —
// after c.Next() has run so the status and byte count are final — records the
// request against its route TEMPLATE. It always calls c.Next() and never gates,
// so /health and /ready keep answering exactly as before; they are merely
// counted. A nil s.Metrics (a caller that deliberately cleared the default)
// makes this a transparent pass-through.
func (s *Server) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rec := s.Metrics
		if rec == nil {
			c.Next()
			return
		}
		rec.IncInFlight()
		defer rec.DecInFlight()
		start := time.Now()
		c.Next()
		routeTemplate := c.FullPath()
		if routeTemplate == "" {
			// An unmatched path (404) has no template; collapse every such
			// request to one bounded bucket rather than leaking the raw URL as a
			// high-cardinality (and potentially secret-bearing) label.
			routeTemplate = "/(unmatched)"
		}
		rec.RecordRequest(c.Request.Method, routeTemplate, c.Writer.Status(), time.Since(start))
	}
}

// Start binds and serves. It returns ONLY once every listener has actually
// bound (and, for TLS, the certificate has loaded) — so a returned nil is a
// genuine promise that the gateway is serving, and any bind/cert failure is
// returned as an error rather than swallowed in a background goroutine after the
// caller already reported "listening". Serving then continues in the background
// until Shutdown.
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

	// Load the certificate up front so a bad --tls-cert/--tls-key path or an
	// unparseable PEM is reported synchronously (the single most common operator
	// error for this feature) instead of surfacing later from a goroutine while
	// the caller has already printed "listening on https://…".
	var cert tls.Certificate
	if tlsEnabled {
		loaded, err := tls.LoadX509KeyPair(s.opt.CertFile, s.opt.KeyFile)
		if err != nil {
			return fmt.Errorf("load TLS cert/key: %w", err)
		}
		cert = loaded
	}

	// Bind the TCP listener synchronously so "address already in use" is a
	// returned error, not a swallowed goroutine failure.
	ln, err := net.Listen("tcp", s.Addr())
	if err != nil {
		return fmt.Errorf("bind gateway %s: %w", s.Addr(), err)
	}

	if s.opt.EnableHTTP3 {
		// Bind the QUIC UDP socket synchronously too, on the same address, so an
		// h3 bind failure is returned rather than swallowed by the old
		// `_ = s.h3.ListenAndServeTLS(...)`. The loaded cert is reused — no
		// second read of the key material.
		udpAddr, uerr := net.ResolveUDPAddr("udp", s.Addr())
		if uerr != nil {
			_ = ln.Close()
			return fmt.Errorf("resolve gateway h3 udp addr %s: %w", s.Addr(), uerr)
		}
		udpConn, uerr := net.ListenUDP("udp", udpAddr)
		if uerr != nil {
			_ = ln.Close()
			return fmt.Errorf("bind gateway h3 udp %s: %w", s.Addr(), uerr)
		}
		s.h3 = &http3.Server{
			Addr:      s.Addr(),
			Handler:   s.eng,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		}
		go func() {
			if serr := s.h3.Serve(udpConn); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				fmt.Printf("gateway: h3 listener stopped: %v\n", serr)
			}
		}()
	}

	go func() {
		var serr error
		if tlsEnabled {
			// The listener is already bound; ServeTLS re-reads the (already
			// validated) cert files and configures HTTP/2 exactly as
			// ListenAndServeTLS did.
			serr = s.h1h2.ServeTLS(ln, s.opt.CertFile, s.opt.KeyFile)
		} else {
			serr = s.h1h2.Serve(ln)
		}
		if serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			// Surface rather than swallow; the supervisor decides.
			fmt.Printf("gateway: listener stopped: %v\n", serr)
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
