// Package livetls is a LIVE transport test for the gateway's TLS-gated
// transports. Where test/live drives `ccr serve` over PLAIN HTTP, this suite
// proves the transports the gateway ADVERTISES but the plain-HTTP suite never
// exercises: HTTP/2 over TLS (ALPN h2), the Alt-Svc h3 advertisement, and
// HTTP/3 over QUIC.
//
// # Start path: the REAL `ccr serve` SUBPROCESS, driven by CLI flags
//
// The `ccr serve` CLI now exposes the full TLS/HTTP3 surface: --tls-cert /
// --tls-key switch the gateway to HTTPS (HTTP/2 over TLS via ALPN) and --http3
// advertises + serves QUIC alongside it (with the CCR_TLS_CERT / CCR_TLS_KEY /
// CCR_HTTP3 env equivalents). So this suite starts the SAME binary an operator
// runs — `ccr serve --tls-cert … --tls-key … --http3` as an os/exec subprocess
// on free loopback ports — and drives it with a real net/http (h1/h2) and a real
// quic-go http3.Transport (h3) over genuine loopback sockets. Every cert is
// generated at runtime into t.TempDir(); there are no fixtures. This closes the
// prior gap where TLS/HTTP3 were reachable only through the in-process
// gateway.Options struct and never through the shipped CLI.
package livetls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// ---------- Built binary (TestMain) ----------

var (
	ccrBin   string
	buildErr error
	buildOut string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-livetls-bin-")
		if err != nil {
			buildErr = fmt.Errorf("mktemp for binary: %w", err)
			return m.Run()
		}
		defer os.RemoveAll(dir)

		bin := filepath.Join(dir, "ccr")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			buildErr = fmt.Errorf("resolve repo root: %w", err)
			return m.Run()
		}
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/ccr")
		cmd.Dir = root
		out, berr := cmd.CombinedOutput()
		buildOut = string(out)
		if berr != nil {
			buildErr = fmt.Errorf("go build ./cmd/ccr failed: %w", berr)
			return m.Run()
		}
		ccrBin = bin
		return m.Run()
	}())
}

// requireBinary fails loudly (never a silent skip) if the binary could not be
// built, surfacing the captured build output.
func requireBinary(t *testing.T) {
	t.Helper()
	if buildErr != nil || ccrBin == "" {
		t.Fatalf("ccr binary was not built: %v\n--- build output ---\n%s", buildErr, buildOut)
	}
}

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

// minimalConfigJSON is enough for the /health handler (it only reports
// len(Providers)); no upstream is ever contacted by these transport tests. The
// top-level keys are capitalised to match the config schema (Providers/Router).
const minimalConfigJSON = `{
  "Providers": [
    {"name": "p", "api_base_url": "https://upstream.invalid/chat/completions", "models": ["m"]}
  ],
  "Router": {"default": "p,m"}
}`

// ---------- Concurrency-safe output buffer ----------

// syncBuffer is safe for the exec copier writes and concurrent test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------- Serve subprocess lifecycle (TLS + HTTP/3) ----------

type serveInstance struct {
	t      *testing.T
	cmd    *exec.Cmd
	out    *syncBuffer
	pool   *x509.CertPool
	gwPort int

	exitCh   chan struct{} // closed once the process has been reaped
	stopOnce sync.Once
}

// startServeTLS generates a fresh cert, writes config.json under a temp HOME,
// and starts the REAL `ccr serve` with --tls-cert/--tls-key/--http3 on free
// loopback ports. It blocks (bounded) until the HTTPS /health answers 200. Any
// failure is a hard t.Fatalf carrying the subprocess output — never a silent
// skip.
func startServeTLS(t *testing.T) *serveInstance {
	t.Helper()
	requireBinary(t)

	certFile, keyFile, pool := genSelfSignedCert(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(minimalConfigJSON), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	si := &serveInstance{
		t:      t,
		out:    &syncBuffer{},
		pool:   pool,
		gwPort: freePort(t),
		exitCh: make(chan struct{}),
	}
	mgmtPort := freePort(t)

	si.cmd = exec.Command(ccrBin, "serve",
		"--no-open",
		"--gateway-host", "127.0.0.1",
		"--gateway-port", strconv.Itoa(si.gwPort),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(mgmtPort),
		"--tls-cert", certFile,
		"--tls-key", keyFile,
		"--http3",
	)
	si.cmd.Stdout = si.out
	si.cmd.Stderr = si.out
	si.cmd.Env = envWith(os.Environ(), map[string]string{
		"HOME":          home,
		"CCR_LOG_LEVEL": "error",
	})

	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve (TLS): %v", err)
	}
	go func() {
		_ = si.cmd.Wait()
		close(si.exitCh)
	}()
	t.Cleanup(si.stop)

	si.waitTLSHealthy(15 * time.Second)
	return si
}

func envWith(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		keep := true
		for k := range overrides {
			if strings.HasPrefix(kv, k+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func (si *serveInstance) gwURL(path string) string {
	return fmt.Sprintf("https://127.0.0.1:%d%s", si.gwPort, path)
}

// exited reports whether the subprocess has already been reaped.
func (si *serveInstance) exited() bool {
	select {
	case <-si.exitCh:
		return true
	default:
		return false
	}
}

// h2Client builds an HTTP/2-capable client that trusts the self-signed cert and
// verifies the server by the 127.0.0.1 IP SAN. ForceAttemptHTTP2 makes net/http
// offer "h2" in the ALPN list so the TLS listener negotiates HTTP/2.
func (si *serveInstance) h2Client() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: si.pool, ServerName: "127.0.0.1"},
		},
	}
}

// waitTLSHealthy polls HTTPS /health until 200 or a bounded deadline. A
// never-ok listener — or an early process exit — is a hard failure (never a
// hanging sleep, never a silent pass).
func (si *serveInstance) waitTLSHealthy(within time.Duration) {
	si.t.Helper()
	client := si.h2Client()
	url := si.gwURL("/health")
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		if si.exited() {
			si.t.Fatalf("ccr serve exited before HTTPS /health became ready\n--- output ---\n%s", si.out.String())
		}
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
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
	si.t.Fatalf("HTTPS /health on port %d never became ready within %s: %v\n--- output ---\n%s",
		si.gwPort, within, lastErr, si.out.String())
}

// stop signals SIGTERM, then kills if the process does not exit in time. Runs
// via t.Cleanup so no ccr process is ever leaked, and is idempotent.
func (si *serveInstance) stop() {
	si.stopOnce.Do(func() {
		if si.cmd.Process == nil {
			return
		}
		_ = si.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-si.exitCh:
		case <-time.After(10 * time.Second):
			_ = si.cmd.Process.Kill()
			<-si.exitCh
		}
	})
}

// ---------- 1+3+4: real HTTPS over HTTP/2, Alt-Svc h3 advertisement ----------

// TestHTTP2OverTLS drives a REAL HTTPS request to GET /health over HTTP/2
// against the `ccr serve --tls-cert … --tls-key … --http3` subprocess and
// asserts the response is 200, was served over TLS (resp.TLS != nil), and the
// negotiated protocol is HTTP/2.0 — and that the same response advertises h3 on
// the listener's port via Alt-Svc (proving altSvcMiddleware fired because
// --http3 is set). These are the DEFINITIVE assertions the suite requires to
// always pass.
func TestHTTP2OverTLS(t *testing.T) {
	si := startServeTLS(t)

	client := si.h2Client()
	url := si.gwURL("/health")
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("HTTPS GET %s: %v\n--- output ---\n%s", url, err, si.out.String())
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
	wantAltSvc := fmt.Sprintf(`h3=":%d"; ma=86400`, si.gwPort)
	if got := resp.Header.Get("Alt-Svc"); got != wantAltSvc {
		t.Fatalf("Alt-Svc = %q, want %q", got, wantAltSvc)
	}
	t.Logf("Alt-Svc OK: %q", resp.Header.Get("Alt-Svc"))
}

// ---------- 5: real HTTP/3 over QUIC (best-effort) ----------

// TestHTTP3OverQUIC makes a REAL HTTP/3 request to /health through quic-go's
// http3.Transport against the subprocess and asserts 200 with
// resp.Proto == "HTTP/3.0". The QUIC handshake over a loopback UDP socket can
// be flaky in constrained CI/sandbox environments (no UDP, blocked, or the h3
// listener's UDP bind losing the close/re-bind port race), so on a
// handshake/dial error this test SKIPS with an explicit, stated reason rather
// than passing silently — the h2 path and the Alt-Svc advertisement in
// TestHTTP2OverTLS carry the definitive proof.
func TestHTTP3OverQUIC(t *testing.T) {
	si := startServeTLS(t)

	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{RootCAs: si.pool, ServerName: "127.0.0.1"},
	}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	url := si.gwURL("/health")

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

// ---------- 6: `ccr serve --http3` without TLS is a clean CLI error ----------

// TestServeHTTP3WithoutTLSRejected proves the SHIPPED CLI refuses to silently
// downgrade: `ccr serve --http3` with no --tls-cert/--tls-key must exit
// non-zero with an explicit "requires TLS" message on stderr, never a quietly
// plain-HTTP listener. The flag parser rejects it before any listener binds, so
// the process exits fast; a bounded wait guards against a hang.
func TestServeHTTP3WithoutTLSRejected(t *testing.T) {
	requireBinary(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(minimalConfigJSON), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ccrBin, "serve", "--no-open", "--http3")
	cmd.Env = envWith(os.Environ(), map[string]string{"HOME": home})
	out, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("`ccr serve --http3` (no TLS) hung instead of erroring; output:\n%s", out)
	}
	if err == nil {
		t.Fatalf("`ccr serve --http3` (no TLS) exited 0; want a non-zero usage error. output:\n%s", out)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		if code := exit.ExitCode(); code != 2 {
			t.Errorf("exit code = %d, want 2 (usage error). output:\n%s", code, out)
		}
	} else {
		t.Fatalf("run ccr serve: unexpected error type %T: %v", err, err)
	}
	if !strings.Contains(string(out), "--http3 requires TLS") {
		t.Fatalf("stderr did not explain the TLS requirement; got:\n%s", out)
	}
	t.Logf("CLI correctly rejected --http3 without TLS:\n%s", strings.TrimSpace(string(out)))
}
