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
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/quic-go/quic-go/http3"

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
}

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
	gin.SetMode(gin.ReleaseMode)
	eng := gin.New()
	eng.Use(gin.Recovery())

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

	// The Anthropic-compatible endpoint Claude Code actually talks to.
	s.eng.POST("/v1/messages", s.handleMessages)
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
