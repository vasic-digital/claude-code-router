package gateway

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"

	"github.com/vasic-digital/claude-code-router/internal/config"
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

func TestDefaultsMatchNodeImplementation(t *testing.T) {
	s := New(testCfg(), Options{})
	// 3456 is the gateway port the Node implementation and every existing
	// toolkit config expect; changing it would break installed setups.
	if s.Addr() != "127.0.0.1:3456" {
		t.Errorf("Addr = %q, want 127.0.0.1:3456", s.Addr())
	}
}
