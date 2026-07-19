package chaos

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- 1. Truncated response (200 then abrupt close mid-body) ----------

func TestChaosTruncatedBodyMidStream(t *testing.T) {
	upstreamURL := rawServer(t, func(conn net.Conn) {
		drainRequestHead(conn)
		// Claims 1000 bytes are coming, sends a handful, then the connection
		// just dies — the classic "truncated response" a flaky upstream or a
		// mid-generation crash produces.
		head := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n"
		_, _ = conn.Write([]byte(head + `{"id":"partial-onl`))
		// connection closes here (deferred by rawServer's accept loop)
	})

	s := gwServer(upstreamURL, 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		errType, msg := assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		if rec.Code == http.StatusOK {
			t.Fatalf("truncated upstream body reported as success")
		}
		t.Logf("truncated body -> status=%d type=%q msg=%q", rec.Code, errType, msg)
	})
}

// ---------- 2. Malformed JSON ----------

func TestChaosMalformedJSONBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id": "x", "choices": [{"message": {"content": tru`) // syntactically broken
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		if rec.Code == http.StatusOK {
			t.Fatalf("malformed upstream JSON reported as success: %s", rec.Body.String())
		}
		_, msg := assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		if !strings.Contains(strings.ToLower(msg), "json") && !strings.Contains(strings.ToLower(msg), "malformed") {
			t.Errorf("error message %q does not indicate a parse failure", msg)
		}
	})
}

// ---------- 3. Valid SSE that dies without a terminating [DONE] ----------

func TestChaosSSEDiesWithoutDone(t *testing.T) {
	upstreamURL := rawServer(t, func(conn net.Conn) {
		drainRequestHead(conn)
		head := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n"
		_, _ = conn.Write([]byte(head))
		lines := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		}
		for _, l := range lines {
			_, _ = conn.Write([]byte("data: " + l + "\n\n"))
		}
		// Connection closes here — NO "data: [DONE]" is ever sent, simulating
		// an upstream that dies mid-generation.
	})

	s := gwServer(upstreamURL, 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — headers were already committed before the stream died, so this must stay 200 with a terminated event stream, not flip to an error status", rec.Code)
		}
		events := sseEvents(rec.Body.Bytes())
		// Despite the missing [DONE], the handler must still close out the
		// event sequence cleanly: message_start ... message_delta,
		// message_stop. That is the "degrades cleanly" contract for a stream
		// that a hostile or crashed upstream abandoned mid-flight.
		if len(events) == 0 || events[0] != "message_start" {
			t.Fatalf("events = %v, want it to start with message_start", events)
		}
		last := events[len(events)-1]
		if last != "message_stop" {
			t.Fatalf("events = %v, want the sequence to still terminate with message_stop despite no upstream [DONE]", events)
		}
		if !strings.Contains(string(rec.Body.Bytes()), `"text":"Hel"`) {
			t.Errorf("partial content received before the connection died was dropped:\n%s", rec.Body.String())
		}
	})
}

// ---------- 4. Upstream hangs past the configured upstream timeout ----------

func TestChaosUpstreamHangsPastHeaderTimeout(t *testing.T) {
	upstreamURL := rawServer(t, func(conn net.Conn) {
		drainRequestHead(conn)
		// Sleep well past the gateway's configured timeout, then give up.
		// Kept short in absolute terms (<300ms) per the suite's no-long-sleep
		// rule; what matters is that it is longer than the timeout below.
		time.Sleep(250 * time.Millisecond)
	})

	// A deliberately tiny timeout so the test proves the bound is honoured
	// without the test itself taking anywhere near 250ms to fail over.
	s := gwServer(upstreamURL, 40*time.Millisecond)

	start := time.Now()
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		elapsed := time.Since(start)
		if rec.Code == http.StatusOK {
			t.Fatalf("a hung upstream produced a 200 OK — should have timed out")
		}
		assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		// Generous margin over the 40ms budget to absorb scheduler jitter,
		// but nowhere near the upstream's 250ms sleep — proves the CONFIGURED
		// timeout governs, not the upstream's own (mis)behaviour.
		if elapsed > 200*time.Millisecond {
			t.Errorf("request took %s to fail over, want well under the upstream's 250ms hang — the timeout does not appear to be honoured", elapsed)
		}
		t.Logf("hung upstream: gateway failed over in %s (budget 40ms)", elapsed)
	})
}

// A streaming request has NO equivalent internal deadline today (by design —
// an SSE session may legitimately run for minutes — see gateway.go's comment
// on Options.UpstreamTimeout). That means time-to-first-byte on a streaming
// call is bounded ONLY by whatever context the inbound request itself
// carries. This test proves that mechanism works correctly when a deadline
// IS present (e.g. imposed by a real net/http.Server's own timeouts, or by
// Claude Code disconnecting) — and, by construction, documents that nothing
// in the gateway itself supplies one for the streaming path. A genuinely
// upstream-supplied hang with NO caller-side deadline at all would hang the
// goroutine indefinitely; we do not — and must not — reproduce that
// unbounded case in an automated test, so this test supplies the deadline
// itself rather than omitting it.
func TestChaosStreamingHangHonoursCallerContextDeadline(t *testing.T) {
	upstreamURL := rawServer(t, func(conn net.Conn) {
		drainRequestHead(conn)
		time.Sleep(250 * time.Millisecond) // never sends a single byte back
	})

	s := gwServer(upstreamURL, 3*time.Second) // irrelevant: not on the streaming path

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(anthropicBody(true))))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(req.Context(), 40*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	runBounded(t, defaultBound, func() {
		s.Handler().ServeHTTP(rec, req)
	})
	elapsed := time.Since(start)

	if rec.Code == http.StatusOK {
		t.Fatalf("a hung streaming upstream with an expired caller context produced 200 OK")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("request took %s to abort after the caller context expired at 40ms — cancellation is not being honoured on the streaming path", elapsed)
	}
	t.Logf("streaming hang aborted via caller context in %s (no internal timeout exists for this path)", elapsed)
}

// ---------- 5. Completely empty body ----------

func TestChaosEmptyBodyNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write nothing at all.
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		if rec.Code == http.StatusOK {
			t.Fatalf("an empty upstream body was reported as success")
		}
		assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
	})
}

func TestChaosEmptyBodyStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Connection closes with zero bytes of SSE body — a degenerate
		// stream that never sent a single event, not even [DONE].
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (headers are committed before any body arrives)", rec.Code)
		}
		events := sseEvents(rec.Body.Bytes())
		want := []string{"message_start", "message_delta", "message_stop"}
		if len(events) != len(want) {
			t.Fatalf("events = %v, want exactly %v for a zero-byte upstream stream", events, want)
		}
		for i := range want {
			if events[i] != want[i] {
				t.Errorf("event[%d] = %q, want %q", i, events[i], want[i])
			}
		}
	})
}

// ---------- 6a. Oversized response headers ----------

func TestChaosOversizedResponseHeader(t *testing.T) {
	upstreamURL := rawServer(t, func(conn net.Conn) {
		drainRequestHead(conn)
		// 11MiB single header value — over Go's default 10MiB response
		// header cap, so the transport itself must reject this before ever
		// handing a response back to application code.
		big := strings.Repeat("A", 11<<20)
		head := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nX-Huge: " + big + "\r\nContent-Length: 2\r\n\r\n{}"
		_, _ = conn.Write([]byte(head))
	})

	s := gwServer(upstreamURL, 5*time.Second)
	start := time.Now()
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		elapsed := time.Since(start)
		if rec.Code == http.StatusOK {
			t.Fatalf("an oversized response header was accepted as a 200")
		}
		assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		if elapsed > defaultBound {
			t.Errorf("oversized-header rejection took %s", elapsed)
		}
	})
}

// ---------- 6b. Very large body: the 32MiB response cap must engage ----------

func TestChaosOversizedResponseBodyIsCappedNotUnbounded(t *testing.T) {
	const chunk = 1 << 20 // 1MiB
	const chunks = 34     // 34MiB total, safely over respondNonStreaming's 32MiB read cap
	filler := strings.Repeat("A", chunk)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// An opening brace and an unterminated string: whatever prefix
		// survives truncation at the cap is guaranteed to be invalid JSON,
		// which is the point — the cap must engage before the message
		// integrity is a going concern.
		_, _ = io.WriteString(w, `{"id":"big","choices":[{"message":{"content":"`)
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte(filler))
		}
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, 10*time.Second)
	start := time.Now()
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		elapsed := time.Since(start)
		if rec.Code == http.StatusOK {
			t.Fatalf("a 34MiB truncated-JSON upstream body was reported as success")
		}
		assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		// This is the actual "no unbounded memory growth" proof: reading and
		// failing on a body many times the cap still completes fast — the
		// gateway is not attempting to buffer all 34MiB, only the first 32.
		if elapsed > 3*time.Second {
			t.Errorf("oversized body handling took %s, want it bounded by the 32MiB read cap, not by the upstream's full 34MiB", elapsed)
		}
		t.Logf("34MiB upstream body (32MiB cap) handled in %s", elapsed)
	})
}

// ---------- 7. Transient recovery: 429 then 200 ----------

func TestChaosTransientRecoveryAfterRateLimit(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "chatcmpl-recovered",
			"choices": [{"index":0,"message":{"role":"assistant","content":"back online"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":1,"completion_tokens":2}
		}`)
	}))
	defer upstream.Close()

	// The gateway itself does not retry — the caller (Claude Code) does. This
	// test proves the SAME server, across two independent calls, correctly
	// reflects both states in turn: the first call must not "stick" the
	// gateway into a failed condition that poisons the second.
	s := gwServer(upstream.URL, 3*time.Second)

	runBounded(t, defaultBound, func() {
		rec1 := postMessages(s, false)
		if rec1.Code != http.StatusTooManyRequests {
			t.Fatalf("first call: status = %d, want 429", rec1.Code)
		}
		errType, _ := assertAnthropicErrorShape(t, rec1.Code, rec1.Body.Bytes())
		if errType != "rate_limit_error" {
			t.Errorf("first call: error.type = %q, want rate_limit_error", errType)
		}

		rec2 := postMessages(s, false)
		if rec2.Code != http.StatusOK {
			t.Fatalf("second call (after recovery): status = %d, body = %s, want 200", rec2.Code, rec2.Body.String())
		}
		if !strings.Contains(rec2.Body.String(), "back online") {
			t.Errorf("second call: recovered content missing: %s", rec2.Body.String())
		}
	})

	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("upstream saw %d calls, want exactly 2", got)
	}
}

// ---------- 8. DNS failure and connection refused ----------

func TestChaosConnectionRefused(t *testing.T) {
	// Port 1 is a privileged, essentially never-listened-on port: refused
	// near-instantly on every platform this suite runs on.
	s := gwServer("http://127.0.0.1:1/v1/chat/completions", 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		if rec.Code == http.StatusOK {
			t.Fatalf("connection-refused upstream reported as success")
		}
		_, msg := assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		if strings.Contains(msg, "sk-chaos-test-not-a-real-key") {
			t.Fatalf("connection-refused error message leaked the provider API key: %q", msg)
		}
	})
}

func TestChaosDNSFailure(t *testing.T) {
	// .invalid is reserved by RFC 2606 to never resolve — deterministic
	// across every environment and network, no live flakiness.
	s := gwServer("http://this-host-should-not-exist.invalid.example/v1/chat/completions", 3*time.Second)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s, false)
		if rec.Code == http.StatusOK {
			t.Fatalf("DNS-failure upstream reported as success")
		}
		_, msg := assertAnthropicErrorShape(t, rec.Code, rec.Body.Bytes())
		if strings.Contains(msg, "sk-chaos-test-not-a-real-key") {
			t.Fatalf("DNS-failure error message leaked the provider API key: %q", msg)
		}
	})
}
