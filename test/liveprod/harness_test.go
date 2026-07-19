// Package liveprod is the broad PRODUCTION-MATRIX live test for the ccr binary.
//
// Where test/live drives one carefully-chosen scenario per behaviour, this
// suite sweeps a MATRIX of (endpoint x config permutation), standing up a fresh
// `ccr serve` subprocess and a fresh fake upstream for each permutation and
// asserting BOTH the HTTP response shape AND the moved Prometheus counters for
// every cell. It deliberately mirrors — rather than imports — test/live's
// harness (a separate package cannot reach test/live's unexported helpers), so
// this file re-establishes the same pattern:
//
//  1. build the real ./cmd/ccr binary once (TestMain);
//  2. per permutation, stand up an httptest fake upstream keyed by URL path
//     with a per-key hit counter, and a $HOME-pinned config.json pointing the
//     providers at it;
//  3. start `ccr serve` as an os/exec subprocess on free loopback ports;
//  4. poll the gateway's real /health, then drive real HTTP through the real
//     listener and scrape the management server's real /metrics;
//  5. assert responses + metric deltas;
//  6. tear every subprocess down in t.Cleanup, capturing stdout+stderr for
//     failure messages.
//
// The matrix itself lives in matrix_test.go.
package liveprod

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
	ccrBin   string // absolute path to the freshly-built ccr binary
	buildErr error  // non-nil if `go build` failed
	buildOut string // captured combined build output (for a useful failure message)
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-liveprod-bin-")
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
		// subprocess build: the subprocess is driven over the network as a black
		// box and a -race binary would only slow it without exercising anything
		// here (and this test must not be run under -race for that reason).
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

// ---------- Free-port helper (bounded retry) ----------

// freePort asks the kernel for an unused loopback TCP port, closes the
// listener, and returns the port for the subprocess to re-bind. Under heavy
// concurrent port churn even an ephemeral :0 bind can transiently fail, so it
// retries up to ~50 times with a 20ms backoff rather than failing the whole run
// on a spurious bind error.
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

// fakeUpstream is one httptest.Server multiplexing many provider scenarios by
// URL path. A provider's api_base_url is set to <server>/<key>; every request
// increments counts[key], records the last request body under bodies[key], and
// is served by the handler registered for key.
type fakeUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	counts   map[string]int
	bodies   map[string]string
	handlers map[string]http.HandlerFunc
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{
		counts:   map[string]int{},
		bodies:   map[string]string{},
		handlers: map[string]http.HandlerFunc{},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) handle(key string, fn http.HandlerFunc) {
	f.mu.Lock()
	f.handlers[key] = fn
	f.mu.Unlock()
}

// url returns the full endpoint URL a provider config should use for key. The
// gateway treats api_base_url as the complete endpoint (it appends no path), so
// this IS the URL the gateway POSTs to.
func (f *fakeUpstream) url(key string) string { return f.srv.URL + "/" + key }

func (f *fakeUpstream) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

// lastBody returns the most recent request body the upstream saw for key.
func (f *fakeUpstream) lastBody(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[key]
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(r.URL.Path, "/")
	if i := strings.IndexByte(key, '/'); i >= 0 {
		key = key[:i] // only the first path segment identifies the scenario
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	// Restore the body so a handler that inspects it (e.g. the stream probe in
	// openAIEcho) still sees the full payload — the capture above must not
	// consume it out from under the handler.
	r.Body = io.NopCloser(bytes.NewReader(raw))
	f.mu.Lock()
	f.counts[key]++
	f.bodies[key] = string(raw)
	fn := f.handlers[key]
	f.mu.Unlock()

	if fn == nil {
		http.Error(w, "no handler for key "+key, http.StatusInternalServerError)
		return
	}
	fn(w, r)
}

// ---------- Canned upstream responses ----------

// writeOpenAICompletion writes a canned non-streaming OpenAI chat-completion.
func writeOpenAICompletion(w http.ResponseWriter, id, content string, promptTok, completionTok int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"id":%q,"object":"chat.completion","choices":[`+
		`{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, content, promptTok, completionTok, promptTok+completionTok)
}

// writeOpenAISSE writes a canned OpenAI chat-completions SSE stream whose text
// deltas concatenate to strings.Join(deltas, "") and which ends with a usage +
// finish_reason chunk then [DONE].
func writeOpenAISSE(w http.ResponseWriter, id string, deltas []string, promptTok, completionTok int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	emit := func(s string) {
		io.WriteString(w, "data: "+s+"\n\n")
		if flush != nil {
			flush.Flush()
		}
	}
	for _, d := range deltas {
		emit(fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, id, d))
	}
	emit(fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		id, promptTok, completionTok, promptTok+completionTok))
	emit("[DONE]")
}

// openAIEcho serves an OpenAI upstream that branches on the request's stream
// flag: a canned SSE stream (13/5 tokens) when streaming, else a canned
// completion (11/7 tokens). It is the shared "main" handler for permutations
// that drive both streaming and non-streaming traffic to one provider.
func openAIEcho(idPrefix, content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(b, &probe)
		if probe.Stream {
			writeOpenAISSE(w, idPrefix+"-stream", []string{"Hello", ", world", "!"}, 13, 5)
			return
		}
		writeOpenAICompletion(w, idPrefix, content, 11, 7)
	}
}

// ---------- Serve subprocess lifecycle ----------

// syncBuffer is a mutex-guarded buffer safe for the two os/exec copier
// goroutines (stdout + stderr) to write concurrently.
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
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	home     string
	gwPort   int
	mgmtPort int
}

// startServe writes cfgJSON to $HOME/.claude-code-router/config.json under a
// fresh temp HOME, starts `ccr serve` on free ports, waits for the gateway's
// /health, and registers cleanup. A build failure, config-write failure, or
// readiness timeout is a hard t.Fatalf carrying the captured subprocess output —
// never a silent skip that could hide a real break.
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

func (si *serveInstance) mgmtURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.mgmtPort, path)
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

// ---------- HTTP client helpers ----------

// httpResult is a fully-read HTTP response.
type httpResult struct {
	status      int
	body        string
	contentType string
}

// post sends a POST with the given body and optional headers, reading the full
// response. Accept-Encoding: identity is forced so the gateway's negotiated
// compression never leaves an undecoded brotli/gzip body.
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
	client := &http.Client{Timeout: 60 * time.Second}
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

// reqMethod sends an arbitrary method with no body (used for the negative
// routability probes) and reads the full response.
func reqMethod(t *testing.T, method, url string) httpResult {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// get sends a GET and reads the full response.
func get(t *testing.T, url string) httpResult {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}
}

// ---------- Prometheus scrape helpers ----------

// scrapeMetrics fetches the management server's /metrics text, retrying briefly
// since the management listener comes up just after the gateway's /health.
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

// metricValue returns the value of the sample of metricName whose label set is a
// superset of want, or 0 if no such sample exists (an absent counter reads as 0,
// exactly right for before/after deltas).
func metricValue(text, metricName string, want map[string]string) float64 {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, metricName) {
			continue
		}
		rest := line[len(metricName):]
		var labels map[string]string
		var valStr string
		switch {
		case strings.HasPrefix(rest, "{"):
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				continue
			}
			labels = parseLabels(rest[1:end])
			valStr = strings.TrimSpace(rest[end+1:])
		case strings.HasPrefix(rest, " "):
			labels = map[string]string{}
			valStr = strings.TrimSpace(rest)
		default:
			// A longer metric name sharing this prefix (e.g. *_bucket / *_sum).
			continue
		}
		if !labelsSuperset(labels, want) {
			continue
		}
		if f, err := strconv.ParseFloat(valStr, 64); err == nil {
			return f
		}
	}
	return 0
}

// metricPresent reports whether at least one sample of metricName carries every
// label in want (regardless of value).
func metricPresent(text, metricName string, want map[string]string) bool {
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, metricName) {
			continue
		}
		rest := line[len(metricName):]
		if !strings.HasPrefix(rest, "{") {
			continue
		}
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			continue
		}
		if labelsSuperset(parseLabels(rest[1:end]), want) {
			return true
		}
	}
	return false
}

func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		k := part[:eq]
		v := strings.Trim(part[eq+1:], `"`)
		out[k] = v
	}
	return out
}

func labelsSuperset(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// ---------- config / body builders ----------

// providerJSON renders one Providers[] entry. protocol is emitted only when
// non-empty (an absent protocol exercises inference).
func providerJSON(name, url, key, model, protocol string) string {
	proto := ""
	if protocol != "" {
		proto = fmt.Sprintf(`,"protocol":%q`, protocol)
	}
	return fmt.Sprintf(`{"name":%q,"api_base_url":%q,"api_key":%q,"models":[%q]%s}`,
		name, url, key, model, proto)
}

// anthropicBody builds a POST /v1/messages request body. extra is appended raw
// (e.g. `,"stream":true` or `,"thinking":{...}`).
func anthropicBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}

// openAIBody builds a POST /v1/chat/completions request body.
func openAIBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}

// ---------- small assertion helpers ----------

func mustContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected to contain %q, got:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected NOT to contain %q, got:\n%s", what, needle, haystack)
	}
}

func mustEqualInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", what, got, want)
	}
}

func mustEqualFloat(t *testing.T, what string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// parseAnthropicSSE collects event names and concatenates every text_delta's
// text from an Anthropic Messages SSE stream.
func parseAnthropicSSE(body string) (events []string, text string) {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if e, ok := strings.CutPrefix(line, "event:"); ok {
			events = append(events, strings.TrimSpace(e))
			continue
		}
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			d = strings.TrimSpace(d)
			if d == "" || d == "[DONE]" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(d), &m) != nil {
				continue
			}
			if m["type"] == "content_block_delta" {
				if delta, ok := m["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
					if tx, ok := delta["text"].(string); ok {
						text += tx
					}
				}
			}
		}
	}
	return events, text
}

func hasEvent(events []string, name string) bool {
	for _, e := range events {
		if e == name {
			return true
		}
	}
	return false
}
