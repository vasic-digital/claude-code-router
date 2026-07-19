// Package liveload is a LIVE load / soak harness for the ccr binary.
//
// It mirrors the black-box pattern of test/live/harness_test.go — build the real
// ./cmd/ccr binary once (TestMain), stand up an httptest fake upstream returning
// canned OpenAI completions with a FIXED usage block, write a config.json under a
// pinned HOME pointing the gateway at the fake upstream, start `ccr serve` as an
// os/exec subprocess on free loopback ports, wait for /health, then drive real
// HTTP through the gateway's real listener and scrape the management server's
// real /metrics — but here the traffic is CONCURRENT and SUSTAINED, and the
// assertions prove the metrics add up EXACTLY under load.
//
// This file owns the harness (binary build, free ports, fake upstream, serve
// lifecycle, GOROUTINE-SAFE HTTP + Prometheus-scrape helpers). The load
// scenarios live in load_test.go. Nothing here imports internal/gateway,
// internal/cache or internal/metrics — the gateway is a black box driven over
// loopback. -race is intentionally NOT applied to this subprocess binary.
package liveload

import (
	"bufio"
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
		dir, err := os.MkdirTemp("", "ccr-liveload-bin-")
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
		// subprocess build — it is driven over the network as a black box and a
		// -race binary would only slow the load test without exercising more.
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

// requireBinary fails loudly (never a silent skip) if the binary was not built.
func requireBinary(t *testing.T) {
	t.Helper()
	if buildErr != nil || ccrBin == "" {
		t.Fatalf("ccr binary was not built: %v\n--- build output ---\n%s", buildErr, buildOut)
	}
}

// ---------- Free-port helper ----------

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

// ---------- Fake upstream ----------

// fakeUpstream multiplexes provider scenarios by URL path. A provider's
// api_base_url is <server>/<key>; every request increments counts[key] (under a
// mutex, safe for the concurrent load) and is served by the handler for key.
type fakeUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	counts   map[string]int
	handlers map[string]http.HandlerFunc
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{
		counts:   map[string]int{},
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

// url returns the full endpoint a provider config should use for key. The
// gateway treats api_base_url as the complete endpoint (no path appended).
func (f *fakeUpstream) url(key string) string { return f.srv.URL + "/" + key }

func (f *fakeUpstream) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

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

// ---------- Canned upstream responses (FIXED usage block) ----------

// writeOpenAICompletion writes a canned non-streaming OpenAI chat-completion
// with a fixed usage block so per-response token accounting is exact.
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
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	home     string
	gwPort   int
	mgmtPort int
}

// startServe writes cfgJSON under a fresh temp HOME, starts `ccr serve` on free
// ports, waits for /health, and registers cleanup (kills the server + rm temp).
// Every failure is a hard t.Fatalf carrying the captured subprocess output.
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
		"CCR_LOG_LEVEL": "error", // keep the child's access log quiet under load
	})

	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve: %v", err)
	}
	t.Cleanup(si.stop)

	si.waitHealthy(20 * time.Second)
	return si
}

// envWith returns base with the given key=value overrides applied.
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

// waitHealthy polls /health until 200 or the deadline. A timeout is fatal and
// prints the subprocess output.
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

// stop signals SIGTERM, then kills if the process does not exit in time. Always
// runs (t.Cleanup) so no ccr process is ever leaked.
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

// ---------- Goroutine-safe HTTP client ----------

// loadClient is tuned for many concurrent connections to one host so the load
// generator, not connection pooling, is the bottleneck.
var loadClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		MaxConnsPerHost:     0, // unlimited concurrent connections
		IdleConnTimeout:     30 * time.Second,
	},
}

// httpResult is a fully-read HTTP response.
type httpResult struct {
	status      int
	body        string
	contentType string
}

// rawPost sends a POST and reads the full response. It NEVER touches *testing.T,
// so it is safe to call from many goroutines concurrently (unlike a t.Fatalf
// helper, whose FailNow must run on the test goroutine). Accept-Encoding:
// identity is forced so a negotiated brotli/gzip body never arrives undecoded.
func rawPost(url, body string, headers map[string]string) (httpResult, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return httpResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := loadClient.Do(req)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}, nil
}

// rawGet sends a GET and reads the full response. Goroutine-safe.
func rawGet(url string) (httpResult, error) {
	resp, err := loadClient.Get(url)
	if err != nil {
		return httpResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpResult{}, err
	}
	return httpResult{status: resp.StatusCode, body: string(raw), contentType: resp.Header.Get("Content-Type")}, nil
}

// get is the t.Fatalf convenience wrapper for use on the test goroutine only.
func get(t *testing.T, url string) httpResult {
	t.Helper()
	res, err := rawGet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return res
}

// ---------- Concurrency driver ----------

// taskResult is one request's outcome, gathered off the test goroutine.
type taskResult struct {
	idx    int
	res    httpResult
	err    error
	elapse time.Duration
}

// runConcurrent fires n tasks across w worker goroutines, invoking fn(i) for
// indices 0..n-1, and returns every result. Bounded: it drains a closed job
// channel and every worker exits when the channel is empty — no open sleeps, no
// leaked goroutines. Panics inside fn are recovered into an error so one bad
// request cannot crash the whole test binary.
func runConcurrent(n, w int, fn func(i int) (httpResult, error)) []taskResult {
	jobs := make(chan int)
	results := make([]taskResult, n)
	var wg sync.WaitGroup
	for g := 0; g < w; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				start := time.Now()
				res, err := func() (r httpResult, e error) {
					defer func() {
						if p := recover(); p != nil {
							e = fmt.Errorf("panic in request goroutine: %v", p)
						}
					}()
					return fn(i)
				}()
				results[i] = taskResult{idx: i, res: res, err: err, elapse: time.Since(start)}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

// ---------- Prometheus scrape helpers ----------

// scrapeMetrics fetches /metrics, retrying briefly since the management listener
// comes up just after the gateway's /health.
func scrapeMetrics(t *testing.T, si *serveInstance) string {
	t.Helper()
	var last httpResult
	var lastErr error
	for i := 0; i < 40; i++ {
		last, lastErr = rawGet(si.mgmtURL("/metrics"))
		if lastErr == nil && last.status == http.StatusOK {
			return last.body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("management /metrics never returned 200 (last status %d, err %v)\n--- output ---\n%s",
		last.status, lastErr, si.out.String())
	return ""
}

// metricValue returns the value of the sample of metricName whose label set is a
// superset of want, or 0 if none (an absent counter reads as 0 — exactly right
// for before/after deltas).
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

// providerJSON renders one Providers[] entry.
func providerJSON(name, url, key, model string) string {
	return fmt.Sprintf(`{"name":%q,"api_base_url":%q,"api_key":%q,"models":[%q]}`,
		name, url, key, model)
}

// anthropicBody builds a POST /v1/messages request body. extra is appended raw
// (e.g. `,"stream":true`).
func anthropicBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}
