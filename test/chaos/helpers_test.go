// Package chaos drives the real gateway HTTP handler (internal/gateway) and
// the standalone upstream proxy client (internal/proxy) against deliberately
// misbehaving upstream servers.
//
// The property under test everywhere in this package is the same: whatever
// an upstream does — truncates, lies about its own framing, hangs, refuses to
// resolve — the router must degrade CLEANLY. That means exactly one of:
//
//   - a Go error is returned to the immediate caller (proxy-level tests), or
//   - a well-formed Anthropic-shaped {"type":"error",...} JSON body is written
//     with a sensible HTTP status (gateway-level tests).
//
// It must NEVER panic, NEVER hang past an explicit bound, and NEVER report
// success (200 OK with a body Claude Code would try to parse as real content)
// for a response that was actually truncated, malformed, or absent.
//
// Every scenario runs through runBounded so a regression that reintroduces a
// hang fails the specific test quickly instead of wedging the suite or CI.
package chaos

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
)

// defaultBound is the hard wall-clock ceiling for a single chaos scenario.
// Every scenario here is designed to resolve in well under a second (small
// configured timeouts, sub-300ms synthetic delays); this bound exists purely
// as a backstop so a real regression fails fast and visibly instead of
// hanging the test binary.
const defaultBound = 5 * time.Second

// runBounded executes fn in a goroutine and fails the test if it does not
// return within timeout. Go has no way to forcibly cancel a genuinely wedged
// goroutine, so a regression that reintroduces a hang still leaks one
// goroutine here — but the TEST ITSELF, and the suite as a whole, completes
// promptly and reports the failure, which is what "no test may hang" means
// in practice for black-box HTTP-handler tests like these.
func runBounded(t *testing.T, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("scenario did not complete within %s — the gateway/proxy appears to have hung", timeout)
	}
}

// gwServer builds a *gateway.Server wired to a single provider pointing at
// upstreamURL, with the given per-call upstream timeout (0 keeps the
// package's own generous default).
func gwServer(upstreamURL string, upstreamTimeout time.Duration) *gateway.Server {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "chaos-provider",
			APIBaseURL: upstreamURL,
			APIKey:     "sk-chaos-test-not-a-real-key",
			Models:     []string{"chaos-model"},
		}},
		Router: config.Route{Default: "chaos-provider,chaos-model"},
	}
	return gateway.New(cfg, gateway.Options{UpstreamTimeout: upstreamTimeout})
}

// anthropicBody builds a minimal valid POST /v1/messages body.
func anthropicBody(stream bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"stream":     stream,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	return b
}

// postMessages drives one POST /v1/messages through the gateway's real gin
// handler, in-process (no socket), and returns the recorded response.
func postMessages(s *gateway.Server, stream bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicBody(stream)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// assertAnthropicErrorShape asserts body decodes as the Anthropic
// {"type":"error","error":{"type":...,"message":...}} envelope, with both
// type and message non-empty. This is the "well-formed error response" half
// of the chaos contract: a caller (Claude Code) must always be able to parse
// whatever the gateway sends back on a bad path.
func assertAnthropicErrorShape(t *testing.T, status int, body []byte) (errType, message string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("status %d body is not valid JSON, want a well-formed Anthropic error envelope: %v\nbody: %s", status, err, body)
	}
	if e.Type != "error" {
		t.Errorf("top-level type = %q, want \"error\"", e.Type)
	}
	if e.Error.Type == "" {
		t.Errorf("error.type is empty")
	}
	if e.Error.Message == "" {
		t.Errorf("error.message is empty")
	}
	if status == http.StatusOK {
		t.Errorf("status = 200 alongside an error envelope — a caller could mistake this for success")
	}
	return e.Error.Type, e.Error.Message
}

// sseEvents extracts the ordered "event: X" names from a raw SSE response
// body.
func sseEvents(body []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := cutPrefix(line, "event: "); ok {
			out = append(out, v)
		}
	}
	return out
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// ---------- raw TCP chaos upstream ----------
//
// httptest.Server always writes conformant HTTP through Go's own server
// stack, which makes it impossible to lie about framing (a Content-Length
// that doesn't match the actual bytes sent), send an oversized header, or
// close mid-write on demand — exactly the failure modes several scenarios
// below need. rawServer hands each accepted connection to a handler that
// controls the literal bytes on the wire.
func rawServer(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by t.Cleanup
			}
			go func(c net.Conn) {
				defer c.Close()
				handle(c)
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

// drainRequestHead reads and discards the request line and headers (up to
// the blank line that ends them) so the handler behaves like a real, if
// chaotic, HTTP server rather than one that ignores its input entirely. It
// deliberately does not read the request body: none of the scenarios here
// need to inspect it, and the client (a bounded http.Client) never blocks
// waiting for us to do so.
func drainRequestHead(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if line == "\r\n" || line == "\n" {
			return
		}
	}
}
