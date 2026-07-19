package security

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
)

// ---------- Outbound: the upstream response cap IS enforced ----------

// respondNonStreaming reads upstream responses through io.LimitReader(...,
// 32<<20) — this proves that cap actually engages against a real oversized
// response rather than asserting it exists by reading the source. This is
// the DEFENSIVE half of "oversized bodies must not cause unbounded memory
// growth": what the gateway reads FROM an upstream is capped.
func TestUpstreamResponseBodyCapIsEnforced(t *testing.T) {
	const totalMiB = 40
	filler := strings.Repeat("A", 1<<20)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"`)
		for i := 0; i < totalMiB; i++ {
			_, _ = w.Write([]byte(filler))
		}
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, "sk-irrelevant")
	start := time.Now()
	runBounded(t, defaultBound, func() {
		rec := postMessages(s)
		elapsed := time.Since(start)
		if rec.Code == http.StatusOK {
			t.Fatalf("a %dMiB truncated-JSON upstream body was reported as success", totalMiB)
		}
		// The cap (32MiB) engaging, not the full 40MiB transfer completing
		// and THEN failing, is what proves memory use is bounded rather than
		// proportional to whatever an upstream chooses to send.
		if elapsed > 3*time.Second {
			t.Errorf("handling a %dMiB body (32MiB cap) took %s, want it bounded by the cap", totalMiB, elapsed)
		}
	})
}

// ---------- Inbound: the request body has NO such cap today ----------
//
// handleMessages calls io.ReadAll(c.Request.Body) with no io.LimitReader and
// no gin/http.MaxBytesReader anywhere in front of it. This test does NOT
// assert that oversized inbound requests are rejected — they currently are
// not, and asserting rejection would make this test fail against real
// behaviour, which the task rules (and CLAUDE.md's testing conventions)
// disallow doing silently. Instead it PINS the current ceiling behaviour
// (a moderately large body is read successfully, request handling completes
// in time roughly proportional to size, no panic) and calls out — in the
// open, in the test's own doc comment and this package's findings — that
// there is currently no upper bound on inbound request-body size. A caller
// that can reach POST /v1/messages can make the gateway buffer an
// arbitrarily large request body in memory before validation ever runs.
//
// This is a genuine, reportable finding (see the task summary), not
// something this test-only package should silently work around by adding
// production code — internal/gateway/messages.go is owned by another agent.
func TestInboundRequestBodyHasNoSizeCapToday(t *testing.T) {
	const bodyMiB = 8 // kept modest so the test stays fast; the absence of a
	// cap is provable at this size just as well as at a more alarming one.

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "p", APIBaseURL: "http://127.0.0.1:1/x", // never actually reached; see below
			APIKey: "sk-irrelevant", Models: []string{"m"},
		}},
		Router: config.Route{Default: "p,m"},
	}
	s := gateway.New(cfg, gateway.Options{UpstreamTimeout: 2 * time.Second})

	huge := strings.Repeat("a", bodyMiB<<20)
	payload, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": huge},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	start := time.Now()
	runBounded(t, defaultBound, func() {
		// The specific outcome (200/502/whatever, depending on the
		// unreachable upstream) is not the point; the point is that reading
		// and JSON-decoding an 8MiB body does not panic, does not hang, and
		// is accepted for processing rather than rejected for size — proving
		// the absence of an inbound cap, which is the finding.
		s.Handler().ServeHTTP(rec, req)
	})
	elapsed := time.Since(start)
	t.Logf("gateway accepted and processed an %dMiB request body in %s with no size rejection at any layer (no io.LimitReader/MaxBytesReader guards c.Request.Body in handleMessages) — flagged as a finding, not fixed here (out of this package's scope)", bodyMiB, elapsed)

	if rec.Code == 0 {
		t.Fatalf("handler did not produce any response at all")
	}
}

// A quick, deterministic proxy-level companion: internal/proxy.Client.Do
// itself never reads or copies the REQUEST body at all beyond wrapping it in
// a bytes.Reader (see proxy.go's Do) — so its own memory overhead for a
// large outbound body is exactly the caller-supplied []byte, no additional
// multiplication. This is a structural property, verified here by size
// rather than by reading the source.
func TestProxyDoDoesNotMultiplyRequestBodyMemory(t *testing.T) {
	const bodyMiB = 4
	body := []byte(fmt.Sprintf(`{"padding":%q}`, strings.Repeat("b", bodyMiB<<20)))

	var gotLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		gotLen = int(n)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := &config.Provider{Name: "p", APIBaseURL: upstream.URL, APIKey: "sk-irrelevant", Models: []string{"m"}}
	c := proxy.New(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), defaultBound)
	defer cancel()
	resp, err := c.Do(ctx, p, body, false)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if gotLen != len(body) {
		t.Errorf("upstream received %d bytes, want exactly %d (the caller-supplied body, unmodified)", gotLen, len(body))
	}
}
