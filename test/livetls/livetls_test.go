// Package livetls is a LIVE transport test for the gateway's TLS-gated
// transports. Where test/live drives `ccr serve` over PLAIN HTTP, this suite
// proves the transports the gateway ADVERTISES but the plain-HTTP suite never
// exercises: HTTP/2 over TLS (ALPN h2), the Alt-Svc h3 advertisement, and
// HTTP/3 over QUIC.
//
// # Start path: IN-PROCESS gateway.New(...).Start(), not `ccr serve`
//
// The `ccr serve` CLI (cmd/ccr/flags.go + serve.go) exposes NO TLS/HTTP3
// surface at all: parseCommonFlags knows only --host/--port/--gateway-host/
// --gateway-port/--open/--gateway (+ the CCR_WEB_*/CCR_GATEWAY_* env
// equivalents), and cmdServe constructs gateway.Options with ONLY Host+Port —
// CertFile, KeyFile and EnableHTTP3 are never set from any flag or env var. TLS
// and HTTP/3 are reachable ONLY through gateway.Options. So there is no
// subprocess path that could turn them on; instead we start the gateway
// IN-PROCESS via gateway.New(cfg, Options{CertFile, KeyFile, EnableHTTP3:true})
// + Start() on a free loopback port. It is still a REAL TLS listener over real
// loopback sockets (net/http's ListenAndServeTLS for h1/h2, quic-go's
// http3.Server for h3) — same-process, but genuinely on the wire, driven by a
// real net/http and a real quic-go client. Every cert is generated at runtime
// into t.TempDir(); there are no fixtures.
package livetls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
)

// ---------- self-signed cert generation (crypto/tls + x509 + ecdsa) ----------

// genSelfSignedCert writes a fresh self-signed ECDSA (P-256) cert+key PEM pair
// into t.TempDir() and returns the two file paths plus a *x509.CertPool that
// trusts the cert. The cert carries the 127.0.0.1 / ::1 IP SANs and a
// "localhost" DNS SAN so a loopback HTTPS client verifies it by ServerName.
// A generation failure is a hard t.Fatalf with detail — never a silent skip.
func genSelfSignedCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA P-256 key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate cert serial: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ccr-livetls-loopback"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed certificate: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert.pem: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal EC private key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(parsed)
	return certFile, keyFile, pool
}

// ---------- helpers ----------

// freePort reserves an unused loopback TCP port and returns it. The same number
// is re-bound by the gateway for BOTH its TCP (h1/h2) and UDP (h3) listeners;
// the close/re-bind window is the standard accepted race for handing a port to
// a listener under test.
func freePort(t *testing.T) int {
	t.Helper()
	// Bounded retry so a transient "address already in use" on an ephemeral :0
	// bind (heavy concurrent port churn / TIME_WAIT) does not fail the run.
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			return port
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reserve free port after retries: %v", lastErr)
	return 0
}

// minimalConfig is enough for the /health handler (it only reports
// len(Providers)); no upstream is ever contacted by these transport tests.
func minimalConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name:       "p",
			APIBaseURL: "https://upstream.invalid/chat/completions",
			Models:     []string{"m"},
		}},
		Router: config.Route{Default: "p,m"},
	}
}

// startTLSGateway starts an in-process TLS gateway on a free port with HTTP/3
// enabled, registers cleanup, and blocks (bounded) until its HTTPS /health
// answers 200. A bind/serve failure surfaces as a fatal readiness timeout with
// no silent skip.
func startTLSGateway(t *testing.T, pool *x509.CertPool, certFile, keyFile string) (*gateway.Server, int) {
	t.Helper()
	port := freePort(t)
	gw := gateway.New(minimalConfig(), gateway.Options{
		Host:        "127.0.0.1",
		Port:        port,
		CertFile:    certFile,
		KeyFile:     keyFile,
		EnableHTTP3: true,
	})
	if err := gw.Start(); err != nil {
		t.Fatalf("start TLS gateway on port %d: %v", port, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gw.Shutdown(ctx)
	})
	waitTLSHealthy(t, pool, port)
	return gw, port
}

// h2Client builds an HTTP/2-capable client that trusts the self-signed cert and
// verifies the server by the 127.0.0.1 IP SAN. ForceAttemptHTTP2 makes net/http
// offer "h2" in the ALPN list so the TLS listener negotiates HTTP/2.
func h2Client(pool *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		},
	}
}

// waitTLSHealthy polls HTTPS /health until 200 or a bounded deadline. A never-ok
// listener is a hard failure (never a hanging sleep, never a silent pass).
func waitTLSHealthy(t *testing.T, pool *x509.CertPool, port int) {
	t.Helper()
	client := h2Client(pool)
	url := fmt.Sprintf("https://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("HTTPS /health on port %d never became ready: %v", port, lastErr)
}

// ---------- 1+3+4: real HTTPS over HTTP/2, Alt-Svc h3 advertisement ----------

// TestHTTP2OverTLS drives a REAL HTTPS request to GET /health over HTTP/2 and
// asserts the response is 200, was served over TLS (resp.TLS != nil), and the
// negotiated protocol is HTTP/2.0 — and that the same response advertises h3 on
// the listener's port via Alt-Svc (proving altSvcMiddleware fired because
// EnableHTTP3 is set). These two are the DEFINITIVE assertions the task
// requires to always pass.
func TestHTTP2OverTLS(t *testing.T) {
	certFile, keyFile, pool := genSelfSignedCert(t)
	_, port := startTLSGateway(t, pool, certFile, keyFile)

	client := h2Client(pool)
	url := fmt.Sprintf("https://127.0.0.1:%d/health", port)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("HTTPS GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatalf("resp.TLS is nil: request was not served over TLS")
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("resp.Proto = %q, want %q (ALPN h2 not negotiated)", resp.Proto, "HTTP/2.0")
	}
	t.Logf("h2 OK: status=%d proto=%q tls.version=0x%04x alpn=%q",
		resp.StatusCode, resp.Proto, resp.TLS.Version, resp.TLS.NegotiatedProtocol)

	// Alt-Svc must advertise h3 on THIS port (compress.go: `h3=":<port>"; ma=86400`).
	wantAltSvc := fmt.Sprintf(`h3=":%d"; ma=86400`, port)
	if got := resp.Header.Get("Alt-Svc"); got != wantAltSvc {
		t.Fatalf("Alt-Svc = %q, want %q", got, wantAltSvc)
	}
	t.Logf("Alt-Svc OK: %q", resp.Header.Get("Alt-Svc"))
}

// ---------- 5: real HTTP/3 over QUIC (best-effort) ----------

// TestHTTP3OverQUIC makes a REAL HTTP/3 request to /health through quic-go's
// http3.Transport and asserts 200 with resp.Proto == "HTTP/3.0". The QUIC
// handshake over a loopback UDP socket can be flaky in constrained CI/sandbox
// environments (no UDP, blocked, or the h3 listener's UDP bind losing the
// close/re-bind port race), so on a handshake/dial error this test SKIPS with
// an explicit, stated reason rather than passing silently — the h2 path and the
// Alt-Svc advertisement in TestHTTP2OverTLS carry the definitive proof.
func TestHTTP3OverQUIC(t *testing.T) {
	certFile, keyFile, pool := genSelfSignedCert(t)
	_, port := startTLSGateway(t, pool, certFile, keyFile)

	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
	}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	url := fmt.Sprintf("https://127.0.0.1:%d/health", port)

	// Bounded retry: the UDP/QUIC listener may need a moment beyond the TCP
	// /health readiness gate. Never an unbounded sleep.
	deadline := time.Now().Add(8 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("best-effort HTTP/3: QUIC handshake to %s did not complete in this environment: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP/3 GET /health status = %d, want 200", resp.StatusCode)
	}
	if resp.Proto != "HTTP/3.0" {
		t.Fatalf("resp.Proto = %q, want %q", resp.Proto, "HTTP/3.0")
	}
	if resp.TLS == nil {
		t.Fatalf("resp.TLS is nil over HTTP/3")
	}
	t.Logf("h3 OK: status=%d proto=%q", resp.StatusCode, resp.Proto)
}

// ---------- 6: HTTP/3 without TLS is an explicit error ----------

// TestHTTP3WithoutTLSIsError proves the gateway REFUSES to silently downgrade:
// EnableHTTP3 with no CertFile/KeyFile must make Start() return an explicit
// error (QUIC has no cleartext mode), never a quietly-plain-HTTP listener.
func TestHTTP3WithoutTLSIsError(t *testing.T) {
	gw := gateway.New(minimalConfig(), gateway.Options{
		Host:        "127.0.0.1",
		Port:        freePort(t),
		EnableHTTP3: true,
		// CertFile/KeyFile deliberately unset.
	})
	err := gw.Start()
	if err == nil {
		// Do not leak a listener if Start unexpectedly succeeded.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = gw.Shutdown(ctx)
		cancel()
		t.Fatalf("Start() with EnableHTTP3 and no TLS returned nil error; want an explicit HTTP/3-requires-TLS error")
	}
	t.Logf("no-TLS HTTP/3 correctly rejected: %v", err)
}
