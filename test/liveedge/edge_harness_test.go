// Package liveedge is a LIVE adversarial / edge-case robustness harness for the
// ccr gateway. Like the sibling test/live suite it builds the real ./cmd/ccr
// binary, stands up a fake OpenAI/Anthropic upstream, boots `ccr serve` as an
// os/exec SUBPROCESS on free loopback ports, and drives REAL HTTP through the
// gateway's real listener. Unlike test/live (which asserts the happy path), this
// suite feeds the running gateway malformed and HOSTILE input — oversized bodies,
// garbage JSON, misbehaving upstreams, secret-bearing configs — and proves the
// process stays UP, SAFE (no api_key leak), and never panics.
//
// This file owns the harness (binary build, bounded free-port helper, fake
// upstream, serve lifecycle, HTTP + raw-socket + Prometheus helpers). The
// adversarial scenarios live in edge_test.go. Nothing here imports
// internal/gateway — the gateway is a black box driven over loopback.
package liveedge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------- Built binary (TestMain) ----------

var (
	ccrBin   string
	buildErr error
	buildOut string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-liveedge-bin-")
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
		// Plain build — no -race on the driven subprocess (per suite policy).
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

func requireBinary(t *testing.T) {
	t.Helper()
	if buildErr != nil || ccrBin == "" {
		t.Fatalf("ccr binary was not built: %v\n--- build output ---\n%s", buildErr, buildOut)
	}
}

// ---------- Bounded free-port helper ----------

// freePort asks the kernel for an unused loopback TCP port, closes the listener,
// and returns the port for the subprocess to re-bind. Bounded retry (~50x @20ms)
// survives transient "address already in use" under concurrent port churn rather
// than failing the whole run on a spurious bind error.
func freePort(t *testing.T) int {
	t.Helper()
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

// ---------- Fake upstream ----------

// fakeUpstream multiplexes many provider scenarios by URL path; a provider's
// api_base_url is <server>/<key>, and each request is served by the handler
// registered for that first path segment.
type fakeUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	counts   map[string]int
	handlers map[string]http.HandlerFunc
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{counts: map[string]int{}, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) handle(key string, fn http.HandlerFunc) {
	f.mu.Lock()
	f.handlers[key] = fn
	f.mu.Unlock()
}

func (f *fakeUpstream) url(key string) string { return f.srv.URL + "/" + key }

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(r.URL.Path, "/")
	if i := strings.IndexByte(key, '/'); i >= 0 {
		key = key[:i]
	}
	f.mu.Lock()
	f.counts[key]++
	fn := f.handlers[key]
	f.mu.Unlock()
	if fn == nil {
		http.Error(w, "no handler for key "+key, http.StatusInternalServerError)
		return
	}
	fn(w, r)
}

// writeOpenAICompletion writes a canned non-streaming OpenAI chat-completion.
// content is JSON-encoded (not %q) so control chars / NUL / unicode round-trip
// as valid JSON rather than Go's invalid-for-JSON \x00 escape.
func writeOpenAICompletion(w http.ResponseWriter, id, content string, promptTok, completionTok int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	cb, _ := json.Marshal(content)
	fmt.Fprintf(w, `{"id":%q,"object":"chat.completion","choices":[`+
		`{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, cb, promptTok, completionTok, promptTok+completionTok)
}

// ---------- Serve subprocess lifecycle ----------

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

type serveInstance struct {
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	home     string
	gwPort   int
	mgmtPort int
}

func (si *serveInstance) gwURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.gwPort, path)
}
func (si *serveInstance) gwHostPort() string { return fmt.Sprintf("127.0.0.1:%d", si.gwPort) }
func (si *serveInstance) mgmtURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.mgmtPort, path)
}

// startServe writes cfgJSON under a fresh temp HOME, starts `ccr serve` on free
// ports, waits for /health, and registers a Cleanup that kills the subprocess.
func startServe(t *testing.T, cfgJSON string) *serveInstance {
	t.Helper()
	requireBinary(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	si := &serveInstance{
		t:        t,
		out:      &syncBuffer{},
		home:     home,
		gwPort:   freePort(t),
		mgmtPort: freePort(t),
	}
	si.cmd = exec.Command(ccrBin, "serve",
		"--no-open",
		"--gateway-host", "127.0.0.1",
		"--gateway-port", strconv.Itoa(si.gwPort),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(si.mgmtPort),
	)
	si.cmd.Stdout = si.out
	si.cmd.Stderr = si.out
	si.cmd.Env = envWith(os.Environ(), map[string]string{
		"HOME":          home,
		"CCR_LOG_LEVEL": "error",
	})
	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve: %v", err)
	}
	t.Cleanup(si.stop)

	si.waitHealthy(15 * time.Second)
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

func (si *serveInstance) waitHealthy(within time.Duration) {
	si.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if si.cmd.ProcessState != nil && si.cmd.ProcessState.Exited() {
			si.t.Fatalf("ccr serve exited before becoming healthy\n--- output ---\n%s", si.out.String())
		}
		resp, err := client.Get(si.gwURL("/health"))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(75 * time.Millisecond)
	}
	si.t.Fatalf("gateway /health not ready within %s\n--- output ---\n%s", within, si.out.String())
}

// alive reports whether the subprocess is still running (has not exited).
func (si *serveInstance) alive() bool {
	return si.cmd.ProcessState == nil || !si.cmd.ProcessState.Exited()
}

func (si *serveInstance) stop() {
	if si.cmd.Process == nil {
		return
	}
	_ = si.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = si.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = si.cmd.Process.Kill()
		<-done
	}
}

// ---------- HTTP client helpers ----------

type httpResult struct {
	status      int
	body        string
	contentType string
}

// post sends a POST with the given body/headers and reads the full response.
// Accept-Encoding: identity so the gateway's negotiated compression never leaves
// an undecoded body. A generous but BOUNDED timeout means a gateway hang FAILS
// the test (t.Fatalf) instead of blocking forever.
func post(t *testing.T, url, body string, headers map[string]string) httpResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response %s: %v", url, err)
	}
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// get sends a GET and reads the full response (bounded timeout).
func get(t *testing.T, url string) httpResult {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// request sends an arbitrary method with no body and reads the full response.
func request(t *testing.T, method, url string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// postRawOversized sends a request whose body is larger than the gateway's
// inbound cap, over a RAW socket. The body is written from a background goroutine
// while the response is read on the main goroutine with a read deadline. This is
// deliberate: with net/http's Client, a server that answers 413 and closes the
// connection mid-upload can surface to the caller as a "broken pipe" WRITE error
// that masks the 413 it actually sent. Reading the response independently of the
// (best-effort, error-ignored) write guarantees we observe the real status the
// gateway returned, and the read deadline guarantees a hang FAILS rather than
// blocks forever.
func postRawOversized(t *testing.T, hostPort, path, body string) httpResult {
	t.Helper()
	conn, err := net.DialTimeout("tcp", hostPort, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", hostPort, err)
	}
	defer conn.Close()

	header := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n", path, hostPort, len(body))
	if _, err := io.WriteString(conn, header); err != nil {
		t.Fatalf("write request header: %v", err)
	}
	// Best-effort body write; the server may 413 and close before we finish.
	go func() {
		_, _ = io.WriteString(conn, body)
	}()

	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read oversized response (a hang or reset here is a real robustness failure): %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// ---------- Prometheus scrape ----------

func scrapeMetrics(t *testing.T, si *serveInstance) string {
	t.Helper()
	var last httpResult
	for i := 0; i < 40; i++ {
		last = get(t, si.mgmtURL("/metrics"))
		if last.status == http.StatusOK {
			return last.body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("management /metrics never returned 200 (last status %d)\n--- output ---\n%s", last.status, si.out.String())
	return ""
}

// ---------- assertion helpers ----------

func mustEqualInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", what, got, want)
	}
}

func mustContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected to contain %q, got:\n%s", what, needle, truncate(haystack, 2000))
	}
}

func mustNotContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected NOT to contain %q, got:\n%s", what, needle, truncate(haystack, 2000))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...(+%d bytes)", len(s)-n)
}

// assertServerUp proves the gateway is still serving after an adversarial hit:
// the subprocess has not exited and /health answers 200.
func assertServerUp(t *testing.T, si *serveInstance) {
	t.Helper()
	if !si.alive() {
		t.Fatalf("gateway subprocess exited after adversarial input\n--- output ---\n%s", si.out.String())
	}
	h := get(t, si.gwURL("/health"))
	mustEqualInt(t, "post-adversarial GET /health status", h.status, 200)
	mustContain(t, "post-adversarial GET /health body", h.body, `"status":"ok"`)
}
