package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/logging"
)

func testCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name: "p1", APIBaseURL: "https://up.example/v1/chat/completions",
			APIKey: "k", Models: []string{"m1", "m2"},
		}},
		Router: config.Route{Default: "p1,m1"},
	}
}

func TestNegotiatePrefersBrotliThenGzip(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"identity":             "",
		"gzip":                 "gzip",
		"br":                   "br",
		"gzip, br":             "br", // brotli wins even when listed second
		"br;q=0.1, gzip;q=0.9": "br", // preference is by capability, not q
		"gzip, deflate":        "gzip",
		"deflate":              "",
		"*":                    "",     // wildcard is not a concrete encoding
		"br;q=0, gzip":         "gzip", // q=0 means "not acceptable"
		"gzip;q=0":             "",
		"GZIP":                 "gzip", // header tokens are case-insensitive
	}
	for header, want := range cases {
		if got := negotiate(header); got != want {
			t.Errorf("negotiate(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestHealthEndpointUncompressedWhenNotRequested(t *testing.T) {
	s := New(testCfg(), Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty when not requested", enc)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
}

// The compressed body must actually decode — asserting only on the header
// would pass even if we emitted plaintext under a Content-Encoding claim.
func TestBrotliResponseActuallyDecodes(t *testing.T) {
	s := New(testCfg(), Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to include Accept-Encoding", v)
	}
	dec, err := io.ReadAll(brotli.NewReader(rec.Body))
	if err != nil {
		t.Fatalf("brotli decode failed — body was not valid brotli: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(dec, &body); err != nil {
		t.Fatalf("decoded body is not JSON: %q", dec)
	}
	if body["status"] != "ok" {
		t.Errorf("decoded status = %v", body["status"])
	}
}

func TestGzipResponseActuallyDecodes(t *testing.T) {
	s := New(testCfg(), Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: body was not valid gzip: %v", err)
	}
	dec, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip decode: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(dec, &body); err != nil {
		t.Fatalf("decoded body is not JSON: %q", dec)
	}
}

// A compressed response must not carry a stale Content-Length: the compressed
// body length differs, and clients would truncate or hang.
func TestCompressedResponseDropsContentLength(t *testing.T) {
	s := New(testCfg(), Options{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q on a compressed response, want it removed", cl)
	}
}

func TestReadyReflectsRoutability(t *testing.T) {
	// Configured and routable -> 200.
	s := New(testCfg(), Options{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("ready with providers = %d, want 200", rec.Code)
	}

	// No providers -> 503, not a misleading 200.
	empty := New(&config.Config{}, Options{})
	rec2 := httptest.NewRecorder()
	empty.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Errorf("ready with no providers = %d, want 503", rec2.Code)
	}

	// Providers but no default route -> still not ready.
	noRoute := New(&config.Config{
		Providers: []config.Provider{{Name: "p1", APIBaseURL: "https://x/y"}},
	}, Options{})
	rec3 := httptest.NewRecorder()
	noRoute.Handler().ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec3.Code != http.StatusServiceUnavailable {
		t.Errorf("ready with no default route = %d, want 503", rec3.Code)
	}
}

// Asking for HTTP/3 without TLS must fail loudly. QUIC has no cleartext mode,
// so silently downgrading would misreport the transport actually in use.
func TestHTTP3WithoutTLSIsAnExplicitError(t *testing.T) {
	s := New(testCfg(), Options{Port: 0, EnableHTTP3: true})
	err := s.Start()
	if err == nil {
		t.Fatal("EnableHTTP3 without TLS must return an error")
	}
	if !strings.Contains(err.Error(), "TLS") {
		t.Errorf("error should explain the TLS requirement, got: %v", err)
	}
}

// Start must load the certificate SYNCHRONOUSLY: a bad --tls-cert/--tls-key path
// is the single most common operator error for the TLS feature, and it must be
// a returned error — never a nil return followed by the caller printing
// "listening on https://…" while the bind silently failed in a goroutine.
func TestStartBadCertPathReturnsError(t *testing.T) {
	s := New(testCfg(), Options{Host: "127.0.0.1", Port: 0,
		CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"})
	err := s.Start()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		t.Fatal("Start() with a nonexistent cert path must return an error, not report listening")
	}
	if !strings.Contains(err.Error(), "cert") {
		t.Errorf("error should mention the cert load failure, got: %v", err)
	}
}

// Start must bind the TCP listener SYNCHRONOUSLY: an already-in-use port must be
// a returned error, not a swallowed goroutine failure after Start() already
// returned nil.
func TestStartPortInUseReturnsError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port to occupy: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	s := New(testCfg(), Options{Host: "127.0.0.1", Port: port})
	if serr := s.Start(); serr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
		t.Fatalf("Start() on already-bound port %d must return an error", port)
	}
}

func TestAltSvcAdvertisedOnlyWithHTTP3(t *testing.T) {
	// Without HTTP/3 there must be no Alt-Svc promise we cannot keep.
	s := New(testCfg(), Options{Port: 3456})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if v := rec.Header().Get("Alt-Svc"); v != "" {
		t.Errorf("Alt-Svc = %q without HTTP/3 enabled, want empty", v)
	}

	// With HTTP/3 the advertisement must name the right port.
	s3 := New(testCfg(), Options{Port: 4443, EnableHTTP3: true})
	rec3 := httptest.NewRecorder()
	s3.Handler().ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/health", nil))
	if v := rec3.Header().Get("Alt-Svc"); !strings.Contains(v, `h3=":4443"`) {
		t.Errorf("Alt-Svc = %q, want it to advertise h3 on 4443", v)
	}
}

// A panicking handler must be BOTH recovered (clean 500, no crashed process)
// AND still access-logged. gin.Recovery is mounted inside LoggingMiddleware
// (see routes()); if that ordering ever regresses to Recovery-outermost, the
// panic unwinds past LoggingMiddleware's post-c.Next() log call and the request
// escapes the access log — which this test exists to catch. The redaction
// guarantee must survive the panic path too: no secret leaks into the log.
func TestPanicRequestIsRecoveredAndStillLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.NewWithOptions(&buf, slog.LevelInfo, logging.FormatJSON)
	s := New(testCfg(), Options{Logger: logger})

	const secret = "sk-panic-secret-value-0123456789abcdef"
	// Register a handler that panics only after the middleware chain has been
	// entered, so the panic must unwind through LoggingMiddleware before
	// gin.Recovery catches it — the exact path the fix protects.
	s.eng.GET("/boom", func(c *gin.Context) {
		panic("handler exploded")
	})

	// gin.Recovery dumps the panic + stack to gin.DefaultErrorWriter (separate
	// from our access logger). Silence it so the test's own output stays clean.
	oldWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = io.Discard
	defer func() { gin.DefaultErrorWriter = oldWriter }()

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	// Recovery still converts the panic into a clean 500.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (panic must be recovered, not propagated)", rec.Code)
	}

	// The recovered 500 must STILL be logged exactly once — the whole point.
	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines for a panicking request, want exactly 1: %v", len(lines), lines)
	}
	line := lines[0]
	if line["status"] != float64(http.StatusInternalServerError) {
		t.Errorf("logged status = %v, want 500", line["status"])
	}
	if line["path"] != "/boom" {
		t.Errorf("logged path = %v, want /boom", line["path"])
	}
	if line["method"] != http.MethodGet {
		t.Errorf("logged method = %v, want GET", line["method"])
	}

	// Redaction guarantee: the Authorization secret must never reach the log.
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("panic-path access log leaked the Authorization secret: %s", buf.String())
	}
}

func TestDefaultsMatchNodeImplementation(t *testing.T) {
	s := New(testCfg(), Options{})
	// 3456 is the gateway port the Node implementation and every existing
	// toolkit config expect; changing it would break installed setups.
	if s.Addr() != "127.0.0.1:3456" {
		t.Errorf("Addr = %q, want 127.0.0.1:3456", s.Addr())
	}
}
