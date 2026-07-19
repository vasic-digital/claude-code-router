// Package livetimeout is a genuine end-to-end test that proves the
// `ccr serve --upstream-timeout <dur>` CLI flag actually BOUNDS a single
// non-streaming upstream call on the REAL running gateway.
//
// Unlike the in-process handler tests under internal/gateway, every scenario
// here:
//
//  1. builds the real ./cmd/ccr binary (TestMain, once);
//  2. stands up an httptest "upstream" whose handler SLEEPS a controllable
//     duration before responding with a canned OpenAI completion;
//  3. writes a ~/.claude-code-router/config.json (HOME pinned to a t.TempDir())
//     pointing the gateway's single provider at that fake upstream;
//  4. starts `ccr serve --upstream-timeout <dur> --max-attempts 1` as an
//     os/exec SUBPROCESS on a free loopback port;
//  5. waits for the gateway's real /health listener;
//  6. drives a real POST /v1/messages through the gateway's real listener and
//     MEASURES the wall-clock time to the client's response;
//  7. asserts the timeout FIRED (a non-200 well under the upstream sleep) or,
//     for the control scenarios, that a short timeout does not break a fast
//     call and that the absence of the flag lets a slow-but-bounded call
//     succeed;
//  8. tears the subprocess down cleanly in t.Cleanup.
//
// The client's own timeout is set far LONGER than every assertion window, so a
// genuine hang (the timeout NOT firing) FAILS the test on the timing bound
// rather than being masked by the client giving up. This file is fully
// self-contained: it imports nothing from internal/gateway and drives the
// gateway strictly over the loopback network, as a black box.
package livetimeout

import (
	"bytes"
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
	ccrBin   string // absolute path to the freshly-built ccr binary
	buildErr error  // non-nil if `go build` failed
	buildOut string // captured combined build output (for a useful failure message)
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-livetimeout-bin-")
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

		// Build the real CLI. -race is intentionally NOT applied: the subprocess
		// is driven over the network and a -race binary would only slow it down
		// (and could skew the wall-clock timing assertions) without exercising
		// anything the harness cares about.
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

// freePort asks the kernel for an unused TCP port, closes the listener, and
// returns the port for the subprocess to re-bind. The close/re-bind window is
// the standard, accepted race for allocating a port to a child process. Under
// concurrent port churn even an ephemeral :0 bind can transiently fail, so this
// retries ~50x at 20ms before giving up.
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

// newSleepingUpstream returns an httptest.Server whose handler waits `sleep`
// (or until the request context is cancelled, whichever comes first) and then
// writes a canned non-streaming OpenAI chat-completion. When the gateway's
// upstream-timeout fires, it cancels the request context and this handler
// returns promptly WITHOUT writing — so the wall-clock the client observes is
// driven by the gateway's timeout, not by the handler finishing its sleep.
func newSleepingUpstream(t *testing.T, sleep time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(sleep):
			// Full sleep elapsed without cancellation: answer normally.
			writeOpenAICompletion(w, "chatcmpl-slow", "Hello from the (eventually) slow upstream.", 11, 7)
		case <-r.Context().Done():
			// The gateway cancelled the in-flight call (its upstream-timeout
			// fired). The client has already been sent the gateway's error;
			// nothing left to write here.
			return
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeOpenAICompletion writes a canned non-streaming OpenAI chat-completion.
func writeOpenAICompletion(w http.ResponseWriter, id, content string, promptTok, completionTok int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"id":%q,"object":"chat.completion","choices":[`+
		`{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, content, promptTok, completionTok, promptTok+completionTok)
}

// ---------- config-JSON builder ----------

// singleProviderConfig renders a config.json with exactly one OpenAI-shaped
// provider whose api_base_url is the fake upstream, routed as the default.
func singleProviderConfig(upstreamURL string) string {
	return fmt.Sprintf(
		`{"Providers":[{"name":"slow","api_base_url":%q,"api_key":"sk-timeout-test","models":["m"]}],`+
			`"Router":{"default":"slow,m"}}`, upstreamURL)
}

// anthropicBody builds a non-streaming POST /v1/messages request body.
func anthropicBody(content string) string {
	return fmt.Sprintf(`{"model":"probe","max_tokens":256,"messages":[{"role":"user","content":%q}]}`, content)
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

// serveInstance is one running `ccr serve` subprocess.
type serveInstance struct {
	t      *testing.T
	cmd    *exec.Cmd
	out    *syncBuffer
	home   string
	gwPort int
}

// startServe writes cfgJSON to $HOME/.claude-code-router/config.json under a
// fresh temp HOME, starts `ccr serve` on free loopback ports with extraArgs
// appended, waits for the gateway's /health, and registers cleanup. A build
// failure, a config-write failure, or a readiness timeout is a hard t.Fatalf
// carrying the captured subprocess output — never a silent skip that could hide
// a real break.
func startServe(t *testing.T, cfgJSON string, extraArgs ...string) *serveInstance {
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
		t:      t,
		out:    &syncBuffer{},
		home:   home,
		gwPort: freePort(t),
	}
	mgmtPort := freePort(t)

	args := []string{"serve",
		"--no-open",
		"--gateway-host", "127.0.0.1",
		"--gateway-port", strconv.Itoa(si.gwPort),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(mgmtPort),
	}
	args = append(args, extraArgs...)

	si.cmd = exec.Command(ccrBin, args...)
	si.cmd.Stdout = si.out
	si.cmd.Stderr = si.out
	si.cmd.Env = envWith(os.Environ(), map[string]string{
		"HOME":          home,
		"CCR_LOG_LEVEL": "error", // keep the child's access log quiet
	})

	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve: %v", err)
	}
	t.Cleanup(si.stop)

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

// waitHealthy polls the gateway's real /health listener until it answers 200 or
// the deadline passes. A timeout is fatal and prints the subprocess output.
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

// stop signals the subprocess to shut down gracefully, then kills it if it does
// not exit in time. Always runs (t.Cleanup) so no ccr process is ever leaked.
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

// ---------- HTTP client helper ----------

// timedResult is a fully-read HTTP response plus the wall-clock elapsed to
// obtain it.
type timedResult struct {
	status  int
	body    string
	elapsed time.Duration
}

// postTimed sends a non-streaming POST /v1/messages and MEASURES the wall-clock
// to the fully-read response. The client's own timeout is deliberately far
// longer than any assertion window so a genuine hang FAILS the timing bound
// rather than being masked by the client aborting. Accept-Encoding: identity
// keeps the gateway from handing back a compressed body.
func postTimed(t *testing.T, url, body string, clientTimeout time.Duration) timedResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")

	client := &http.Client{Timeout: clientTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// A client-side timeout here means the gateway hung PAST clientTimeout —
		// a real defect worth surfacing loudly with the elapsed time.
		t.Fatalf("POST %s failed after %s (a hang past the client timeout indicates the "+
			"upstream-timeout did NOT bound the call): %v", url, time.Since(start), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read response %s: %v", url, err)
	}
	return timedResult{status: resp.StatusCode, body: string(raw), elapsed: elapsed}
}

// ---------- The live timeout suite ----------

// TestUpstreamTimeoutBoundsSlowCall proves the --upstream-timeout flag bounds a
// single non-streaming upstream call on the real running gateway, with two
// controls proving the bound is specifically the flag's doing.
func TestUpstreamTimeoutBoundsSlowCall(t *testing.T) {
	requireBinary(t)

	// SCENARIO 1 — slow_upstream_times_out.
	//
	// Upstream sleeps 3s; the gateway is launched with --upstream-timeout 500ms
	// and --max-attempts 1 (so no retry budget inflates the wall-clock). The
	// client must receive a NON-200 error in well under the 3s sleep, proving the
	// timeout FIRED rather than the upstream finishing.
	t.Run("slow_upstream_times_out", func(t *testing.T) {
		up := newSleepingUpstream(t, 3*time.Second)
		si := startServe(t, singleProviderConfig(up.URL),
			"--upstream-timeout", "500ms", "--max-attempts", "1")

		res := postTimed(t, si.gwURL("/v1/messages"), anthropicBody("please answer slowly"), 30*time.Second)
		t.Logf("slow_upstream_times_out: status=%d elapsed=%s", res.status, res.elapsed)

		if res.status == http.StatusOK {
			t.Fatalf("DEFECT: expected a NON-200 (timeout should have fired) but got 200 after %s; body:\n%s",
				res.elapsed, res.body)
		}
		if res.status < 500 {
			t.Fatalf("expected a 5xx timeout/gateway error, got status %d; body:\n%s", res.status, res.body)
		}
		// Anthropic error envelope, not a hang and not a success body.
		if !strings.Contains(res.body, `"type":"error"`) || !strings.Contains(res.body, `"error"`) {
			t.Fatalf("expected an Anthropic error envelope, got:\n%s", res.body)
		}
		// The load-bearing proof: bounded by the ~500ms timeout (+1 attempt),
		// NOT by the 3s upstream sleep. A generous 2.5s ceiling absorbs
		// scheduling/subprocess jitter while still being far below 3s.
		if res.elapsed >= 2500*time.Millisecond {
			t.Fatalf("DEFECT: the timeout did not bound the call — response took %s "+
				"(want < 2.5s for a 500ms timeout with 1 attempt; the upstream sleeps 3s)", res.elapsed)
		}
	})

	// SCENARIO 2 — fast_upstream_succeeds (control).
	//
	// The SAME short 500ms upstream-timeout, but an upstream that answers
	// immediately: the request must succeed (200) with the translated body,
	// proving the short timeout does not break normal fast calls.
	t.Run("fast_upstream_succeeds", func(t *testing.T) {
		up := newSleepingUpstream(t, 0) // no sleep: respond immediately
		si := startServe(t, singleProviderConfig(up.URL),
			"--upstream-timeout", "500ms", "--max-attempts", "1")

		res := postTimed(t, si.gwURL("/v1/messages"), anthropicBody("answer fast please"), 30*time.Second)
		t.Logf("fast_upstream_succeeds: status=%d elapsed=%s", res.status, res.elapsed)

		if res.status != http.StatusOK {
			t.Fatalf("control: a fast upstream under a 500ms timeout must return 200, got %d; body:\n%s",
				res.status, res.body)
		}
		// The translated Anthropic message shape reached the client.
		if !strings.Contains(res.body, `"type":"message"`) ||
			!strings.Contains(res.body, "Hello from the (eventually) slow upstream.") {
			t.Fatalf("control: expected a translated Anthropic message body, got:\n%s", res.body)
		}
		if res.elapsed >= 2*time.Second {
			t.Fatalf("control: a fast call should return in well under 2s, took %s", res.elapsed)
		}
	})

	// SCENARIO 3 — default_no_timeout_slow_upstream_succeeds (control).
	//
	// NO --upstream-timeout flag: the gateway uses its 10m default, which does
	// not trip on a ~1s upstream. The request must SUCCEED (200), proving the
	// flag — not some ambient bound — is what introduces the deadline.
	t.Run("default_no_timeout_slow_upstream_succeeds", func(t *testing.T) {
		up := newSleepingUpstream(t, 1*time.Second)
		si := startServe(t, singleProviderConfig(up.URL),
			"--max-attempts", "1") // no --upstream-timeout: default 10m applies

		res := postTimed(t, si.gwURL("/v1/messages"), anthropicBody("answer after a second"), 30*time.Second)
		t.Logf("default_no_timeout_slow_upstream_succeeds: status=%d elapsed=%s", res.status, res.elapsed)

		if res.status != http.StatusOK {
			t.Fatalf("control: with no --upstream-timeout, a ~1s upstream must succeed under the 10m default, "+
				"got %d; body:\n%s", res.status, res.body)
		}
		if !strings.Contains(res.body, `"type":"message"`) ||
			!strings.Contains(res.body, "Hello from the (eventually) slow upstream.") {
			t.Fatalf("control: expected a translated Anthropic message body, got:\n%s", res.body)
		}
		// It genuinely waited for the ~1s upstream (proving it was NOT bounded
		// short) yet still completed.
		if res.elapsed < 900*time.Millisecond {
			t.Fatalf("control: expected the call to wait for the ~1s upstream, but it returned in %s "+
				"(did the upstream really sleep?)", res.elapsed)
		}
	})
}
