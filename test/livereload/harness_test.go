// Package livereload is a genuine end-to-end test harness that proves config
// HOT-RELOAD works on the real ccr binary — INCLUDING its honest boundary.
//
// The Watcher (internal/config/watch.go), wired through cmd/ccr/reload.go +
// serve.go, polls ~/.claude-code-router/config.json. A VALIDATED change is
// detected, swapped into the watcher's Current(), and LOGGED by serve's
// onReload callback; an INVALID change is REJECTED (onReject) and the previous
// good config is kept. Crucially, the RUNNING gateway captured its
// *config.Config at construction and is NOT swapped in place — its /health
// keeps reporting the STARTUP provider count until the process is restarted.
// These tests assert all three, and deliberately do NOT claim the live gateway
// swaps.
//
// Like test/live, every test here builds the real ./cmd/ccr binary once
// (TestMain), writes a config.json under a pinned $HOME=t.TempDir(), starts
// `ccr serve --no-open ...` as an os/exec subprocess on free loopback ports,
// captures its combined stdout+stderr into a mutex-guarded buffer, and waits
// for the gateway's real /health listener. Nothing imports internal/*; the
// gateway and its reloader are driven strictly as a black box.
//
// POLL INTERVAL ACCOUNTED FOR: serve.go passes config.DefaultPollInterval
// (2s) as the interval for BOTH the file watcher and the Current()-pointer
// detector. They are stacked, so an accepted reload becomes observable in at
// most ~2s (watcher tick) + ~2s (detector tick) ≈ 4s. Every bounded log poll
// below uses reloadDeadline = 30s, comfortably exceeding 2× that worst case
// even on a loaded host, and fails with the full captured log on timeout
// (never a silent skip).
package livereload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
)

// reloadDeadline bounds every log poll for a reload accept/reject line. It must
// exceed ~2× the stacked (watcher + detector) 2s poll interval; 30s does so
// with wide margin for a busy CI host.
const reloadDeadline = 30 * time.Second

// logPollStep is how often the bounded poll re-reads the captured log.
const logPollStep = 100 * time.Millisecond

// ---------- Built binary (TestMain) ----------

var (
	ccrBin   string
	buildErr error
	buildOut string
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "ccr-livereload-bin-")
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

// ---------- Serve subprocess lifecycle ----------

type serveInstance struct {
	t        *testing.T
	cmd      *exec.Cmd
	out      *syncBuffer
	home     string
	cfgPath  string
	gwPort   int
	mgmtPort int

	exitCh   chan struct{} // closed once the process has been reaped
	stopOnce sync.Once
}

// startServe writes cfgJSON to $HOME/.claude-code-router/config.json under a
// fresh temp HOME, starts `ccr serve` on free ports, waits for /health, and
// registers cleanup. Any failure is a hard t.Fatalf carrying the subprocess
// output — never a silent skip.
func startServe(t *testing.T, cfgJSON string) *serveInstance {
	t.Helper()
	requireBinary(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".claude-code-router")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	si := &serveInstance{
		t:        t,
		out:      &syncBuffer{},
		home:     home,
		cfgPath:  cfgPath,
		gwPort:   freePort(t),
		mgmtPort: freePort(t),
		exitCh:   make(chan struct{}),
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
		"CCR_LOG_LEVEL": "error", // quiet the access log; reload lines are printed directly
	})

	if err := si.cmd.Start(); err != nil {
		t.Fatalf("start ccr serve: %v", err)
	}
	// Reap the process exactly once, from one goroutine, so ExitCode() and
	// exit detection are race-free for every waiter.
	go func() {
		_ = si.cmd.Wait()
		close(si.exitCh)
	}()
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

func (si *serveInstance) gwURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", si.gwPort, path)
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

// waitHealthy polls the gateway's real /health until it answers 200 or the
// deadline passes. A timeout — or an early exit — is fatal with the output.
func (si *serveInstance) waitHealthy(within time.Duration) {
	si.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if si.exited() {
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

// healthProviders GETs /health and returns its HTTP status and reported
// provider count. The count reflects the gateway's STARTUP config (s.cfg), so
// it is the load-bearing signal that the live gateway is NOT swapped in place.
func (si *serveInstance) healthProviders(t *testing.T) (int, int) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(si.gwURL("/health"))
	if err != nil {
		t.Fatalf("GET /health: %v\n--- output ---\n%s", err, si.out.String())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, -1
	}
	var body struct {
		Status    string `json:"status"`
		Providers int    `json:"providers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse /health body %q: %v", string(raw), err)
	}
	return resp.StatusCode, body.Providers
}

// rewriteConfig atomically-ish replaces config.json with newJSON. Distinct
// byte length from the previous content is the caller's job (see the reload
// tests) so the watcher's mtime+SIZE detection has an extra signal to trip on.
func (si *serveInstance) rewriteConfig(t *testing.T, newJSON string) {
	t.Helper()
	if err := os.WriteFile(si.cfgPath, []byte(newJSON), 0o644); err != nil {
		t.Fatalf("rewrite config.json: %v", err)
	}
}

// pollLogFor waits until the captured output contains want, or reloadDeadline
// elapses. On timeout it is a hard t.Fatalf carrying the full captured log —
// never a silent skip. Returns the log snapshot that first satisfied the match
// (useful as evidence).
func (si *serveInstance) pollLogFor(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(reloadDeadline)
	for time.Now().Before(deadline) {
		s := si.out.String()
		if strings.Contains(s, want) {
			return s
		}
		if si.exited() {
			// The process died; one last read then fail — a crashed reloader
			// must not masquerade as "still waiting".
			s = si.out.String()
			if strings.Contains(s, want) {
				return s
			}
			t.Fatalf("ccr serve exited before log contained %q\n--- output ---\n%s", want, s)
		}
		time.Sleep(logPollStep)
	}
	t.Fatalf("timed out after %s waiting for log to contain %q\n--- output ---\n%s",
		reloadDeadline, want, si.out.String())
	return ""
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
