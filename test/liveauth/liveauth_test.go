// Package liveauth is a LIVE end-to-end test that proves the `ccr serve
// --api-key <key>` inbound-authentication flag actually gates the REAL running
// gateway subprocess — not the in-process gin handler, but the shipped binary an
// operator runs, driven over genuine loopback HTTP.
//
// # What it proves
//
// internal/gateway/auth.go's RequireAPIKey is mounted (gateway.go) ONLY on the
// four completion routes (/v1/messages, /proxy/v1/messages,
// /v1/chat/completions, /proxy/v1/chat/completions) and NEVER on /health or
// /ready. A non-empty accepted-key list (via --api-key, repeatable, or the
// comma-separated CCR_API_KEYS env) turns enforcement ON; an empty list leaves
// every request unauthenticated for backward compatibility. This suite starts
// the SAME binary with `ccr serve … --api-key testkey-canary-123`, points it at
// a fake OpenAI-shaped upstream, and drives real HTTP to assert:
//
//   - no auth header ⇒ 401 Anthropic error envelope, upstream NOT reached;
//   - wrong key (Bearer / x-api-key) ⇒ 401;
//   - correct key via Authorization: Bearer ⇒ NOT 401, upstream WAS reached;
//   - correct key via x-api-key ⇒ NOT 401 (both header schemes work);
//   - GET /health and /ready are never gated (reachable with no auth);
//   - POST /v1/chat/completions with no key ⇒ 401 (the facade route is gated);
//   - the accepted key never appears in ANY captured 401 body (no leak);
//   - a SECOND instance started with NO --api-key leaves POST /v1/messages
//     UNauthenticated (backward-compat: the flag is what enables gating).
//
// The subprocess/build/free-port/serve-lifecycle scaffolding mirrors
// test/livetls and test/live: `go build ./cmd/ccr` once in TestMain, a
// bounded-retry freePort, a serveInstance that writes config.json under a temp
// HOME and waits for /health, and a t.Cleanup that SIGTERMs the child.
package liveauth

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

// The single accepted key under test. It is a canary: it is deliberately
// distinctive so the secret-safety assertion can grep every captured 401 body
// for it and prove the middleware never echoes it back.
const canaryKey = "testkey-canary-123"

// ---------- Built binary (TestMain) ----------

var (
	ccrBin   string
	buildErr error
	buildOut string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-liveauth-bin-")
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
		// Build the real CLI once. -race is intentionally NOT applied: the
		// subprocess is driven over the network as a black box and a -race binary
		// only slows it without exercising anything here.
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

// ---------- Free-port helper (bounded retry) ----------

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

// ---------- Fake OpenAI-shaped upstream ----------

// fakeUpstream is one httptest.Server that returns a canned non-streaming
// OpenAI chat-completion and counts every hit. If the auth gate lets a request
// through, the gateway translates the inbound Anthropic/OpenAI request and POSTs
// here, bumping hits; if the gate rejects at the edge, hits never moves — which
// is exactly the "upstream not reached" proof.
type fakeUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"chatcmpl-canary","object":"chat.completion","choices":`+
			`[{"index":0,"message":{"role":"assistant","content":"Hello from the fake upstream."},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// url is the complete endpoint a provider config should use: the gateway treats
// api_base_url as the full URL and does not append a path.
func (f *fakeUpstream) url() string { return f.srv.URL + "/v1/chat/completions" }

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

// ---------- Concurrency-safe subprocess output buffer ----------

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

// ---------- Serve subprocess lifecycle ----------

type serveInstance struct {
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	gwPort   int
	mgmtPort int
}

// configJSON points the gateway's single provider at the fake upstream and wires
// a default route so /v1/messages and /v1/chat/completions dispatch to it.
func configJSON(upstreamURL string) string {
	return fmt.Sprintf(`{"Providers":[{"name":"main","api_base_url":%q,"api_key":"sk-upstream-not-secret",`+
		`"models":["main-model"]}],"Router":{"default":"main,main-model"}}`, upstreamURL)
}

// startServe writes config.json under a fresh temp HOME and starts the REAL
// `ccr serve` on free loopback ports. Each apiKey in apiKeys is passed as a
// repeated --api-key flag; passing none leaves inbound auth disabled. It blocks
// (bounded) until /health answers 200 — /health is never gated, so this readiness
// probe works with or without auth configured. Every failure is a hard t.Fatalf
// carrying the captured subprocess log; nothing here silently skips.
func startServe(t *testing.T, upstreamURL string, apiKeys ...string) *serveInstance {
	t.Helper()
	requireBinary(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(configJSON(upstreamURL)), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	si := &serveInstance{
		t:        t,
		out:      &syncBuffer{},
		gwPort:   freePort(t),
		mgmtPort: freePort(t),
	}

	args := []string{"serve",
		"--no-open",
		"--gateway-host", "127.0.0.1",
		"--gateway-port", strconv.Itoa(si.gwPort),
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(si.mgmtPort),
	}
	for _, k := range apiKeys {
		args = append(args, "--api-key", k)
	}

	si.cmd = exec.Command(ccrBin, args...)
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

// envWith returns base with the given key=value overrides applied, replacing any
// existing occurrence so the child sees exactly one of each. It also strips the
// CCR_API_KEYS env entirely so a stray value in the test runner's environment can
// never silently enable auth on an instance meant to be unauthenticated.
func envWith(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		keep := true
		if strings.HasPrefix(kv, "CCR_API_KEYS=") {
			keep = false
		}
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

// waitHealthy polls the gateway's /health until 200 or a bounded deadline. A dead
// subprocess or a never-ready listener is fatal and prints the subprocess log —
// never an unbounded sleep, never a silent pass.
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

// stop SIGTERMs the child, then kills it if it lingers. Runs via t.Cleanup so no
// ccr process is ever leaked; temp HOME is removed by t.TempDir's own cleanup.
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

// ---------- HTTP drive helpers ----------

type httpResult struct {
	status int
	body   string
}

// doPost sends a POST with optional headers and reads the full body.
// Accept-Encoding: identity is forced so a negotiated brotli/gzip body never
// leaves us grepping an undecoded blob for the canary.
func doPost(t *testing.T, url, body string, headers map[string]string) httpResult {
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
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response %s: %v", url, err)
	}
	return httpResult{status: resp.StatusCode, body: string(raw)}
}

func doGet(t *testing.T, url string) httpResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(raw)}
}

// anthropicBody / openAIBody build the two inbound request shapes.
func anthropicBody() string {
	return `{"model":"claude-canary","max_tokens":256,"messages":[{"role":"user","content":"hello there"}]}`
}
func openAIBody() string {
	return `{"model":"main-model","messages":[{"role":"user","content":"hello there"}]}`
}

// The exact Anthropic 401 envelope RequireAPIKey emits (auth.go writeUnauthorized).
const wantAuthErrType = `"type":"authentication_error"`
const wantAuthErrMsg = `"message":"invalid or missing API key"`

// ---------- The gated suite ----------

// TestInboundAuthGatesRunningGateway starts ONE `ccr serve … --api-key
// testkey-canary-123` subprocess and drives every gate assertion against it as
// ordered subtests, then a final secret-safety subtest that greps every 401 body
// captured along the way for the canary key. A shared, mutex-guarded collector of
// 401 bodies is threaded through the subtests so the leak check sees them all.
func TestInboundAuthGatesRunningGateway(t *testing.T) {
	requireBinary(t)

	fake := newFakeUpstream(t)
	si := startServe(t, fake.url(), canaryKey)

	// On ANY failure, dump the subprocess log so a break is diagnosable.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- ccr serve (auth-enabled) output ---\n%s", si.out.String())
		}
	})

	var (
		mu           sync.Mutex
		captured401s []string // every 401 body seen, for the leak grep
	)
	record401 := func(body string) {
		mu.Lock()
		captured401s = append(captured401s, body)
		mu.Unlock()
	}

	assertAnthropicEnvelope := func(t *testing.T, r httpResult) {
		t.Helper()
		if !strings.Contains(r.body, wantAuthErrType) || !strings.Contains(r.body, wantAuthErrMsg) {
			t.Fatalf("401 body is not the Anthropic auth-error envelope:\n%s", r.body)
		}
	}

	// 1. No auth header ⇒ 401 + Anthropic envelope + upstream NOT reached.
	t.Run("messages_no_key_rejected_upstream_untouched", func(t *testing.T) {
		before := fake.count()
		r := doPost(t, si.gwURL("/v1/messages"), anthropicBody(), nil)
		if r.status != http.StatusUnauthorized {
			t.Fatalf("POST /v1/messages (no key): status = %d, want 401\nbody:\n%s", r.status, r.body)
		}
		record401(r.body)
		assertAnthropicEnvelope(t, r)
		if after := fake.count(); after != before {
			t.Fatalf("upstream was reached on an unauthenticated request: hits %d -> %d", before, after)
		}
	})

	// 2. Wrong key via BOTH header schemes ⇒ 401.
	t.Run("messages_wrong_key_rejected", func(t *testing.T) {
		before := fake.count()
		rBearer := doPost(t, si.gwURL("/v1/messages"), anthropicBody(),
			map[string]string{"Authorization": "Bearer nope"})
		if rBearer.status != http.StatusUnauthorized {
			t.Fatalf("POST /v1/messages (wrong Bearer): status = %d, want 401\nbody:\n%s", rBearer.status, rBearer.body)
		}
		record401(rBearer.body)
		assertAnthropicEnvelope(t, rBearer)

		rXKey := doPost(t, si.gwURL("/v1/messages"), anthropicBody(),
			map[string]string{"x-api-key": "nope"})
		if rXKey.status != http.StatusUnauthorized {
			t.Fatalf("POST /v1/messages (wrong x-api-key): status = %d, want 401\nbody:\n%s", rXKey.status, rXKey.body)
		}
		record401(rXKey.body)
		assertAnthropicEnvelope(t, rXKey)

		if after := fake.count(); after != before {
			t.Fatalf("upstream was reached on a wrong-key request: hits %d -> %d", before, after)
		}
	})

	// 3. Correct key via Authorization: Bearer ⇒ NOT 401, upstream WAS reached.
	t.Run("messages_correct_bearer_accepted", func(t *testing.T) {
		before := fake.count()
		r := doPost(t, si.gwURL("/v1/messages"), anthropicBody(),
			map[string]string{"Authorization": "Bearer " + canaryKey})
		if r.status == http.StatusUnauthorized {
			record401(r.body)
			t.Fatalf("POST /v1/messages (correct Bearer): got 401, want the auth gate to ADMIT it\nbody:\n%s", r.body)
		}
		if after := fake.count(); after != before+1 {
			t.Fatalf("correct-key request did not reach upstream exactly once: hits %d -> %d", before, after)
		}
		t.Logf("accepted via Bearer: status=%d (upstream hit)", r.status)
	})

	// 4. Correct key via x-api-key ⇒ NOT 401, upstream WAS reached.
	t.Run("messages_correct_xapikey_accepted", func(t *testing.T) {
		before := fake.count()
		r := doPost(t, si.gwURL("/v1/messages"), anthropicBody(),
			map[string]string{"x-api-key": canaryKey})
		if r.status == http.StatusUnauthorized {
			record401(r.body)
			t.Fatalf("POST /v1/messages (correct x-api-key): got 401, want the auth gate to ADMIT it\nbody:\n%s", r.body)
		}
		if after := fake.count(); after != before+1 {
			t.Fatalf("correct x-api-key request did not reach upstream exactly once: hits %d -> %d", before, after)
		}
		t.Logf("accepted via x-api-key: status=%d (upstream hit)", r.status)
	})

	// 5. /health and /ready are NEVER gated.
	t.Run("health_and_ready_never_gated", func(t *testing.T) {
		h := doGet(t, si.gwURL("/health"))
		if h.status != http.StatusOK {
			t.Fatalf("GET /health (no auth): status = %d, want 200\nbody:\n%s", h.status, h.body)
		}
		rd := doGet(t, si.gwURL("/ready"))
		if rd.status == http.StatusUnauthorized {
			record401(rd.body)
			t.Fatalf("GET /ready (no auth): got 401 — readiness must never be gated\nbody:\n%s", rd.body)
		}
		t.Logf("health=%d ready=%d (both reachable without auth)", h.status, rd.status)
	})

	// 6. The OpenAI facade route is gated too: POST /v1/chat/completions, no key ⇒ 401.
	t.Run("chat_completions_no_key_rejected", func(t *testing.T) {
		before := fake.count()
		r := doPost(t, si.gwURL("/v1/chat/completions"), openAIBody(), nil)
		if r.status != http.StatusUnauthorized {
			t.Fatalf("POST /v1/chat/completions (no key): status = %d, want 401\nbody:\n%s", r.status, r.body)
		}
		record401(r.body)
		assertAnthropicEnvelope(t, r)
		if after := fake.count(); after != before {
			t.Fatalf("upstream was reached on an unauthenticated facade request: hits %d -> %d", before, after)
		}
	})

	// 7. SECRET-SAFETY: the accepted key must never appear in ANY 401 body.
	// This runs last so it sees every 401 recorded by the subtests above.
	t.Run("secret_safety_no_canary_leak", func(t *testing.T) {
		mu.Lock()
		bodies := append([]string(nil), captured401s...)
		mu.Unlock()
		if len(bodies) == 0 {
			t.Fatalf("no 401 bodies were captured; the leak check would be vacuous")
		}
		leaks := 0
		for i, b := range bodies {
			if strings.Contains(b, canaryKey) {
				leaks++
				t.Errorf("401 body #%d LEAKS the accepted key %q:\n%s", i, canaryKey, b)
			}
		}
		t.Logf("scanned %d captured 401 bodies; canary occurrences = %d", len(bodies), leaks)
	})
}

// ---------- The backward-compat (auth-disabled) proof ----------

// TestNoAPIKeyLeavesGatewayUnauthenticated starts a SECOND `ccr serve` with NO
// --api-key at all and asserts POST /v1/messages with no auth header is NOT 401
// (it reaches the gateway and the upstream). This is the control that proves the
// flag itself is what enables gating: same binary, same routes, same request —
// only the presence of --api-key differs between this and the gated suite.
func TestNoAPIKeyLeavesGatewayUnauthenticated(t *testing.T) {
	requireBinary(t)

	fake := newFakeUpstream(t)
	si := startServe(t, fake.url()) // no keys => auth disabled

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- ccr serve (auth-disabled) output ---\n%s", si.out.String())
		}
	})

	before := fake.count()
	r := doPost(t, si.gwURL("/v1/messages"), anthropicBody(), nil)
	if r.status == http.StatusUnauthorized {
		t.Fatalf("POST /v1/messages with NO --api-key returned 401; default must be UNauthenticated\nbody:\n%s", r.body)
	}
	if after := fake.count(); after != before+1 {
		t.Fatalf("unauthenticated default did not pass the request through to upstream: hits %d -> %d", before, after)
	}
	t.Logf("auth-disabled default: POST /v1/messages status=%d (not gated, upstream hit)", r.status)
}
