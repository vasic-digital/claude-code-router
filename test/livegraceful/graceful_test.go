// Package livegraceful is a LIVE, black-box test that the ccr gateway shuts down
// GRACEFULLY under load — a production-critical property.
//
// Unlike the in-process handler tests under internal/gateway (which mount
// gateway.Handler() into httptest and never bind a socket), this suite:
//
//  1. builds the real ./cmd/ccr binary once (TestMain);
//  2. stands up a fake upstream whose handler DELAYS before responding, so a
//     request that has reached the gateway is genuinely in-flight for a
//     measurable window;
//  3. writes a ~/.claude-code-router/config.json under a pinned temp HOME whose
//     one provider points at that fake upstream;
//  4. starts `ccr serve` as an os/exec SUBPROCESS on free loopback ports and
//     waits for the gateway's real /health listener;
//  5. fires a batch of concurrent POST /v1/messages, confirms (best-effort, via
//     the management server's ccr_http_inflight_requests gauge) that requests
//     are mid-flight, then sends SIGTERM;
//  6. asserts the process exits cleanly (code 0) inside a bounded grace window,
//     the serve log shows the shutdown line and NO panic / goroutine dump /
//     fatal error, every COMPLETED response is a well-formed Anthropic message
//     (never a truncated/garbage body), and the management server is gone too.
//
// The whole file owns its harness (binary build, bounded-retry free ports, fake
// delayed upstream, serve lifecycle, HTTP helpers). It imports nothing from
// internal/gateway — the gateway is driven strictly over the loopback network,
// as a black box. The subprocess is deliberately built WITHOUT -race: it is
// driven over the network, so a -race binary would only slow it without
// exercising anything this test asserts.
package livegraceful

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
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
	ccrBin   string // absolute path to the freshly-built ccr binary
	buildErr error  // non-nil if `go build` failed
	buildOut string // captured combined build output (for a useful failure message)
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-livegraceful-bin-")
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

		// Build the real CLI. -race is intentionally NOT applied to this
		// subprocess build: it is driven over the network, so a -race binary
		// would only slow the run without exercising the harness.
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

// requireBinary fails the calling test loudly (never a silent skip) if the
// binary could not be built, surfacing the captured build output.
func requireBinary(t *testing.T) {
	t.Helper()
	if buildErr != nil || ccrBin == "" {
		t.Fatalf("ccr binary was not built: %v\n--- build output ---\n%s", buildErr, buildOut)
	}
}

// ---------- Free-port helper (bounded retry) ----------

// freePort asks the kernel for an unused loopback TCP port, closes the listener,
// and returns the port for the subprocess to re-bind. Under heavy concurrent
// port churn a :0 bind can transiently fail; retry with a short backoff rather
// than fail the whole run on one spurious bind error.
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

// ---------- Fake DELAYING upstream ----------

// delayedUpstream is one httptest.Server whose handler sleeps a random duration
// in [minDelay,maxDelay) before writing a canned OpenAI chat-completion, so a
// request that has reached the gateway is genuinely in-flight for a measurable
// window — which is what lets us send SIGTERM while work is in progress.
type delayedUpstream struct {
	srv      *httptest.Server
	minDelay time.Duration
	maxDelay time.Duration

	mu       sync.Mutex
	rnd      *rand.Rand
	started  int // requests whose handler began
	finished int // requests whose handler wrote a full response
}

func newDelayedUpstream(t *testing.T, minDelay, maxDelay time.Duration) *delayedUpstream {
	u := &delayedUpstream{
		minDelay: minDelay,
		maxDelay: maxDelay,
		rnd:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *delayedUpstream) url() string { return u.srv.URL }

func (u *delayedUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.started++
	span := u.maxDelay - u.minDelay
	d := u.minDelay
	if span > 0 {
		d += time.Duration(u.rnd.Int63n(int64(span)))
	}
	u.mu.Unlock()

	// Genuinely hold the request open so it is in-flight across a SIGTERM.
	select {
	case <-time.After(d):
	case <-r.Context().Done():
		// The client/gateway gave up (connection closed). Do not write; the
		// gateway will surface a clean error rather than a truncated body.
		return
	}

	// A complete, well-formed OpenAI non-streaming completion. The gateway
	// translates this into an Anthropic message before the client sees it.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-graceful","object":"chat.completion","choices":[`+
		`{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		"Hello from the delayed upstream.")

	u.mu.Lock()
	u.finished++
	u.mu.Unlock()
}

// ---------- Serve subprocess lifecycle ----------

// syncBuffer is a mutex-guarded buffer safe for the two os/exec copier
// goroutines (stdout + stderr) to write to concurrently.
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

// serveInstance is one running `ccr serve` subprocess. A single background
// goroutine owns exec.Cmd.Wait so the exit code can be read by both the test and
// the cleanup without ever calling Wait twice.
type serveInstance struct {
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	home     string
	gwPort   int
	mgmtPort int

	waitCh  chan struct{} // closed when the subprocess has been reaped
	mu      sync.Mutex
	waitErr error
	exited  bool
}

// startServe writes cfgJSON under a fresh temp HOME, starts `ccr serve` on free
// ports, waits for the gateway's /health, and registers a force-kill cleanup. A
// build/config-write/readiness failure is a hard t.Fatalf carrying the captured
// subprocess output — never a silent skip that could hide a real break.
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
		waitCh:   make(chan struct{}),
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
		"CCR_LOG_LEVEL": "error", // keep the child's access log quiet
	})

	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve: %v", err)
	}

	// One owner of Wait. Everything else observes the result via waitCh.
	go func() {
		err := si.cmd.Wait()
		si.mu.Lock()
		si.waitErr = err
		si.exited = true
		si.mu.Unlock()
		close(si.waitCh)
	}()

	t.Cleanup(func() {
		select {
		case <-si.waitCh:
			return // already exited
		default:
		}
		_ = si.cmd.Process.Kill()
		<-si.waitCh
	})

	si.waitHealthy(15 * time.Second)
	return si
}

// envWith returns base with the given key=value overrides applied (replacing any
// existing occurrence of each key so the child sees exactly one).
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
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.gwPort, path)
}

func (si *serveInstance) mgmtURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.mgmtPort, path)
}

func (si *serveInstance) hasExited() bool {
	select {
	case <-si.waitCh:
		return true
	default:
		return false
	}
}

// waitHealthy polls the gateway's /health until it answers 200 or the deadline
// passes. A dead subprocess or a timeout is fatal and prints the captured output.
func (si *serveInstance) waitHealthy(within time.Duration) {
	si.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if si.hasExited() {
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

// signal sends sig to the subprocess (best-effort; a race with exit is benign).
func (si *serveInstance) signal(sig os.Signal) {
	if si.cmd.Process != nil {
		_ = si.cmd.Process.Signal(sig)
	}
}

// waitForExit blocks until the subprocess has been reaped or within elapses. It
// returns (exitCode, true) on exit and (0, false) on timeout. A normal (nil)
// Wait is exit code 0; an *exec.ExitError yields its code; any other Wait error
// is reported as -1 so the caller can surface it.
func (si *serveInstance) waitForExit(within time.Duration) (int, bool) {
	select {
	case <-si.waitCh:
	case <-time.After(within):
		return 0, false
	}
	si.mu.Lock()
	defer si.mu.Unlock()
	if si.waitErr == nil {
		return 0, true
	}
	if ee, ok := si.waitErr.(*exec.ExitError); ok {
		return ee.ExitCode(), true
	}
	return -1, true
}

// ---------- HTTP + metrics helpers ----------

// messageResult is the outcome of one concurrent /v1/messages call.
type messageResult struct {
	status int
	body   string
	err    error // transport error (e.g. connection refused after shutdown began)
}

// postMessages fires n concurrent POST /v1/messages and returns a channel that
// yields exactly n results. Each request forces Accept-Encoding: identity so a
// negotiated brotli/gzip body never masquerades as truncated garbage.
func postMessages(si *serveInstance, n int, body string) <-chan messageResult {
	results := make(chan messageResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, si.gwURL("/v1/messages"), strings.NewReader(body))
			if err != nil {
				results <- messageResult{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "identity")
			client := &http.Client{Timeout: 20 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				results <- messageResult{err: err}
				return
			}
			raw, rerr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if rerr != nil {
				results <- messageResult{status: resp.StatusCode, err: rerr}
				return
			}
			results <- messageResult{status: resp.StatusCode, body: string(raw)}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	return results
}

// scrapeInflight reads the ccr_http_inflight_requests gauge from the management
// server's /metrics, returning (value, true) on a successful scrape. The gauge
// is label-free, so its sample line is `ccr_http_inflight_requests <value>`.
func scrapeInflight(si *serveInstance) (float64, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(si.mgmtURL("/metrics"))
	if err != nil {
		return 0, false
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	const name = "ccr_http_inflight_requests"
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		rest := strings.TrimSpace(line[len(name):])
		// Guard against a longer metric name sharing this prefix.
		if rest == "" || strings.HasPrefix(rest, "_") || strings.HasPrefix(rest, "{") {
			continue
		}
		if f, err := strconv.ParseFloat(rest, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ---------- Config / body builders ----------

// singleProviderConfig points one provider at the delayed upstream and routes
// the default tier to it. The gateway treats api_base_url as the complete
// endpoint (it appends no path), so this IS the URL it POSTs to.
func singleProviderConfig(upstreamURL string) string {
	return fmt.Sprintf(
		`{"Providers":[{"name":"main","api_base_url":%q,"api_key":"sk-graceful","models":["main-model"]}],`+
			`"Router":{"default":"main,main-model"}}`,
		upstreamURL)
}

func anthropicBody(model, content string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"messages":[{"role":"user","content":%q}]}`, model, content)
}

// ---------- Log-sanity assertions ----------

// crashMarkers are substrings a clean shutdown must never emit. A Go panic
// prints "panic:" plus a "goroutine N [running]:" stack; a runtime crash prints
// "fatal error:". Their presence in the captured stdout+stderr is proof the
// shutdown was NOT graceful.
var crashMarkers = []string{"panic:", "goroutine ", "fatal error:", "runtime error:", "leaked"}

func assertNoCrash(t *testing.T, what, log string) {
	t.Helper()
	for _, marker := range crashMarkers {
		if strings.Contains(log, marker) {
			t.Fatalf("%s: serve log contains crash/leak marker %q\n--- output ---\n%s", what, marker, log)
		}
	}
}

// ---------- Subtest 1: graceful shutdown UNDER LOAD ----------

// TestGracefulShutdownUnderLoad proves the gateway drains genuinely in-flight
// requests on SIGTERM:
//   - clean exit: the process exits with code 0 inside a bounded grace window;
//   - no-panic: the serve log shows "shutting down..." and NO panic / goroutine
//     dump / fatal error / leak marker;
//   - no-corruption: every COMPLETED response is either a well-formed Anthropic
//     message (200, parses, type=message + role=assistant) or a clean JSON error
//     — never a truncated/garbage body; requests refused after shutdown began
//     (connection error) are acceptable and counted separately;
//   - management-gone: the management /metrics listener stops accepting too.
func TestGracefulShutdownUnderLoad(t *testing.T) {
	requireBinary(t)

	// A comfortable in-flight window: long enough that we reliably observe the
	// inflight gauge > 0 and send SIGTERM while requests are mid-handler, short
	// enough that draining them stays well under the 10s serve grace.
	upstream := newDelayedUpstream(t, 200*time.Millisecond, 450*time.Millisecond)
	si := startServe(t, singleProviderConfig(upstream.url()))

	const batch = 16
	results := postMessages(si, batch, anthropicBody("claude-sonnet-graceful", "hello there"))

	// Best-effort: wait until the gateway reports work in flight, then SIGTERM
	// while it is genuinely mid-drain. If (very unlikely) we never observe it,
	// send SIGTERM anyway rather than hang — the no-corruption assertions below
	// are the real guarantee.
	observedInflight := false
	inflightDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(inflightDeadline) {
		if v, ok := scrapeInflight(si); ok && v > 0 {
			observedInflight = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	sigAt := time.Now()
	si.signal(syscall.SIGTERM)

	// Clean exit inside a bounded grace window. shutdownGrace in serve.go is 10s;
	// a sane cap of 12s leaves slack for a slow CI host without ever being open-ended.
	const graceWindow = 12 * time.Second
	code, exited := si.waitForExit(graceWindow)
	if !exited {
		t.Fatalf("ccr serve did not exit within %s of SIGTERM\n--- output ---\n%s", graceWindow, si.out.String())
	}
	shutdownDur := time.Since(sigAt)
	if code != 0 {
		t.Fatalf("ccr serve exited with code %d after SIGTERM (want 0)\n--- output ---\n%s", code, si.out.String())
	}

	// Collect every request outcome (the channel closes after all n arrive). A
	// bounded guard: the process has already exited, so all clients have either
	// completed or errored; this must not hang.
	var completed, refused, otherErr int
	collectDeadline := time.After(graceWindow)
	got := 0
collect:
	for got < batch {
		select {
		case r, ok := <-results:
			if !ok {
				break collect
			}
			got++
			switch {
			case r.err != nil:
				// A request that arrived after shutdown began is legitimately
				// refused (connection reset/refused). That is acceptable.
				refused++
			case r.status == http.StatusOK:
				// A COMPLETED 200 must be a well-formed Anthropic message — parse
				// it and check the translated shape. A truncated/garbage body
				// fails json.Unmarshal or lacks the shape, which is a hard fail.
				var msg map[string]any
				if jerr := json.Unmarshal([]byte(r.body), &msg); jerr != nil {
					t.Fatalf("no-corruption: completed 200 body is not valid JSON: %v\nbody=%q\n--- output ---\n%s",
						jerr, r.body, si.out.String())
				}
				if msg["type"] != "message" || msg["role"] != "assistant" {
					t.Fatalf("no-corruption: completed 200 is not a well-formed Anthropic message (type=%v role=%v)\nbody=%q",
						msg["type"], msg["role"], r.body)
				}
				completed++
			default:
				// A non-200 that still returned a body must be a CLEAN error:
				// valid JSON, not a truncated fragment.
				var anyJSON any
				if jerr := json.Unmarshal([]byte(r.body), &anyJSON); jerr != nil {
					t.Fatalf("no-corruption: completed non-200 (status %d) body is not clean JSON: %v\nbody=%q",
						r.status, jerr, r.body)
				}
				otherErr++
			}
		case <-collectDeadline:
			t.Fatalf("timed out collecting request results (got %d/%d) after process exit\n--- output ---\n%s",
				got, batch, si.out.String())
		}
	}

	// Graceful drain means the requests that were mid-flight when SIGTERM landed
	// completed successfully rather than being cut off. Since we SIGTERM only
	// after observing work in flight, at least one must have completed cleanly.
	if observedInflight && completed == 0 {
		t.Fatalf("graceful drain: observed in-flight requests but NONE completed cleanly "+
			"(completed=%d refused=%d otherErr=%d)\n--- output ---\n%s",
			completed, refused, otherErr, si.out.String())
	}

	log := si.out.String()

	// no-panic + shutdown line.
	assertNoCrash(t, "graceful-under-load", log)
	if !strings.Contains(log, "shutting down...") {
		t.Fatalf("serve log missing the shutdown line %q\n--- output ---\n%s", "shutting down...", log)
	}
	// The gateway and management listeners must not report a shutdown error.
	if strings.Contains(log, "gateway shutdown:") {
		t.Fatalf("serve log reports a gateway shutdown error\n--- output ---\n%s", log)
	}
	if strings.Contains(log, "management shutdown:") {
		t.Fatalf("serve log reports a management shutdown error\n--- output ---\n%s", log)
	}

	// management-gone: the /metrics listener no longer accepts connections.
	if _, ok := scrapeInflight(si); ok {
		t.Fatalf("management /metrics still answered after shutdown; the management server did not stop")
	}

	t.Logf("graceful under load: exit=0 in %s, observedInflight=%v, completed=%d refused=%d otherErr=%d (batch=%d), upstream started=%d finished=%d",
		shutdownDur.Round(time.Millisecond), observedInflight, completed, refused, otherErr, batch, upstream.started, upstream.finished)
}

// ---------- Subtest 2: idle shutdown ----------

// TestGracefulShutdownIdle proves an IDLE server (no requests in flight) exits 0
// promptly on SIGTERM and logs the shutdown line with no panic.
func TestGracefulShutdownIdle(t *testing.T) {
	requireBinary(t)

	upstream := newDelayedUpstream(t, 200*time.Millisecond, 450*time.Millisecond)
	si := startServe(t, singleProviderConfig(upstream.url()))

	sigAt := time.Now()
	si.signal(syscall.SIGTERM)

	// With nothing to drain, shutdown is near-instant; a 5s cap is generous.
	const idleWindow = 5 * time.Second
	code, exited := si.waitForExit(idleWindow)
	if !exited {
		t.Fatalf("idle ccr serve did not exit within %s of SIGTERM\n--- output ---\n%s", idleWindow, si.out.String())
	}
	if code != 0 {
		t.Fatalf("idle ccr serve exited with code %d after SIGTERM (want 0)\n--- output ---\n%s", code, si.out.String())
	}

	log := si.out.String()
	assertNoCrash(t, "idle-shutdown", log)
	if !strings.Contains(log, "shutting down...") {
		t.Fatalf("idle serve log missing the shutdown line %q\n--- output ---\n%s", "shutting down...", log)
	}

	t.Logf("idle shutdown: exit=0 in %s", time.Since(sigAt).Round(time.Millisecond))
}
