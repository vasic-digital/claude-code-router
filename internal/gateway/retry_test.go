package gateway

// Tests for the retry loop in messages.go (doUpstreamWithRetry): the
// gateway drives internal/router's Retryable/Terminal classifiers from a
// real retry loop instead of making exactly one upstream attempt and
// forwarding whatever it got. See messages.go's doc comment on
// doUpstreamWithRetry for the full contract.
//
// None of these tests sleep for real router backoff (>=1s floor): they stub
// the package-level retryDelayAfterStatus / retryDelayAfterNetworkError
// indirections via stubFastRetryDelays (or a custom stub, for the tests that
// need to inspect what those functions were called with, or need a
// deliberately LONG delay to prove cancellation cuts it short). Because
// those are package-level vars, none of these tests run in parallel with
// each other or with anything else that might mutate them.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// stubFastRetryDelays swaps the retry-delay indirections for near-zero
// stand-ins so a test that exercises a genuine retry does not have to wait
// out router's real (>=1s) backoff floor. The returned func restores the
// originals; callers must defer it immediately.
func stubFastRetryDelays(t *testing.T) func() {
	t.Helper()
	origStatus, origNet := retryDelayAfterStatus, retryDelayAfterNetworkError
	retryDelayAfterStatus = func(int, string) time.Duration { return time.Millisecond }
	retryDelayAfterNetworkError = func(int) time.Duration { return time.Millisecond }
	return func() {
		retryDelayAfterStatus = origStatus
		retryDelayAfterNetworkError = origNet
	}
}

// countingUpstream is a minimal Upstream fake that counts calls and
// delegates each one to do, giving tests full control over what each
// successive attempt returns without a real network round trip.
type countingUpstream struct {
	calls int32
	do    func(call int32) (*http.Response, error)
}

func (u *countingUpstream) Do(_ context.Context, _ config.Provider, _ []byte) (*http.Response, error) {
	n := atomic.AddInt32(&u.calls, 1)
	return u.do(n)
}

// onceThenErrReader yields data on its first Read, then fails every
// subsequent Read — simulating an upstream connection that delivers one
// chunk and then dies mid-stream.
type onceThenErrReader struct {
	data []byte
	sent bool
}

func (r *onceThenErrReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, errors.New("simulated mid-stream connection drop")
}

func (r *onceThenErrReader) Close() error { return nil }

// --- retry-on-429-then-success ---

func TestRetryOn429ThenSucceeds(t *testing.T) {
	defer stubFastRetryDelays(t)()

	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"id": "chatcmpl-retry-ok",
			"choices": [{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":1,"completion_tokens":1}
		}`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past the 429; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Errorf("recovered content missing from response: %s", rec.Body.String())
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("upstream saw %d calls, want exactly 2 (the failed 429, then the recovery)", got)
	}
}

// --- no-retry-on-401 (Terminal) ---

func TestNoRetryOnTerminalStatus(t *testing.T) {
	defer stubFastRetryDelays(t)()

	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key","type":"authentication_error"}}`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 preserved from upstream", rec.Code)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("upstream saw %d calls, want exactly 1 — a 401 is Terminal and must never be retried (a retry only burns quota)", got)
	}
}

// --- attempt cap respected ---

func TestRetryAttemptCapRespected(t *testing.T) {
	defer stubFastRetryDelays(t)()

	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"still down","type":"api_error"}}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "fake", APIBaseURL: upstream.URL, APIKey: "sk-test", Models: []string{"fake-model"},
		}},
		Router: config.Route{Default: "fake,fake-model"},
	}
	s := New(cfg, Options{MaxAttempts: 3})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 preserved after exhausting retries", rec.Code)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("upstream saw %d calls, want exactly 3 (Options.MaxAttempts), neither fewer nor more", got)
	}
}

// A MaxAttempts of 1 must behave exactly like no retry loop at all: a single
// attempt, no backoff wait.
func TestRetryAttemptCapOfOneMeansNoRetries(t *testing.T) {
	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "fake", APIBaseURL: upstream.URL, APIKey: "sk-test", Models: []string{"fake-model"},
		}},
		Router: config.Route{Default: "fake,fake-model"},
	}
	s := New(cfg, Options{MaxAttempts: 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 preserved from the single attempt", rec.Code)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("upstream saw %d calls, want exactly 1 (MaxAttempts=1 disables retrying entirely)", got)
	}
}

// --- no retry after bytes written (streaming) ---

func TestNoRetryAfterStreamingBytesWritten(t *testing.T) {
	fake := &countingUpstream{do: func(int32) (*http.Response, error) {
		body := &onceThenErrReader{
			data: []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n"),
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
	}}

	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = fake

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(true)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the SSE header is committed before the mid-stream failure, so this must stay 200 with a cleanly terminated stream, not flip to an error status", rec.Code)
	}
	if got := atomic.LoadInt32(&fake.calls); got != 1 {
		t.Fatalf("upstream saw %d calls, want exactly 1 — once SSE bytes were flushed to the client, retrying would re-send a partially-delivered stream and corrupt the conversation", got)
	}

	starts := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: message_start") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("message_start appeared %d times, want exactly 1 (a retry would have produced a second one)", starts)
	}
}

// --- client cancellation aborts promptly ---

func TestClientCancellationAbortsRetryPromptly(t *testing.T) {
	origStatus, origNet := retryDelayAfterStatus, retryDelayAfterNetworkError
	// Deliberately much longer than the context deadline below: if
	// cancellation were not honoured mid-wait, this test would block for the
	// full 5s instead of returning within tens of milliseconds.
	retryDelayAfterStatus = func(int, string) time.Duration { return 5 * time.Second }
	retryDelayAfterNetworkError = func(int) time.Duration { return 5 * time.Second }
	defer func() {
		retryDelayAfterStatus = origStatus
		retryDelayAfterNetworkError = origNet
	}()

	fake := &countingUpstream{do: func(int32) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"down"}}`)),
		}, nil
	}}

	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = fake

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return within 2s — context cancellation is not aborting the retry wait promptly")
	}

	if rec.Code == http.StatusOK {
		t.Fatalf("cancelled request reported 200 OK")
	}
	if got := atomic.LoadInt32(&fake.calls); got != 1 {
		t.Fatalf("upstream saw %d calls, want exactly 1 — the context ended during the retry backoff and must abort before a second attempt is made", got)
	}
}

// --- Retry-After honoured ---

func TestRetryAfterHeaderHonoured(t *testing.T) {
	origStatus, origNet := retryDelayAfterStatus, retryDelayAfterNetworkError
	gotAttempt := -1
	var gotHeader string
	retryDelayAfterStatus = func(attempt int, retryAfterHeader string) time.Duration {
		gotAttempt = attempt
		gotHeader = retryAfterHeader
		return time.Millisecond
	}
	retryDelayAfterNetworkError = func(int) time.Duration { return time.Millisecond }
	defer func() {
		retryDelayAfterStatus = origStatus
		retryDelayAfterNetworkError = origNet
	}()

	var calls int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry (gated on the honoured Retry-After delay) succeeded", rec.Code)
	}
	if gotAttempt != 0 {
		t.Errorf("retry delay computed for attempt index %d, want 0 (the first, failed attempt)", gotAttempt)
	}
	if gotHeader != "7" {
		t.Errorf("Retry-After header threaded through as %q, want %q (the upstream's exact value)", gotHeader, "7")
	}
}

// A transport-level (no HTTP response at all) retryable error must also
// retry and eventually succeed, exercising retryDelayAfterNetworkError
// rather than retryDelayAfterStatus.
func TestRetryOnTransportErrorThenSucceeds(t *testing.T) {
	defer stubFastRetryDelays(t)()

	fake := &countingUpstream{do: func(call int32) (*http.Response, error) {
		if call == 1 {
			// net.Error with Timeout()==true classifies as Retryable — see
			// router.ClassifyTransportError.
			return nil, timeoutError{}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"ok","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
			)),
		}, nil
	}}

	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = fake

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retrying past a transient transport error; body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&fake.calls); got != 2 {
		t.Fatalf("upstream saw %d calls, want exactly 2 (the timeout, then the recovery)", got)
	}
}

// timeoutError is a minimal net.Error whose Timeout() is true, matching
// router.ClassifyTransportError's Retryable case for a transport-level
// failure with no HTTP response.
type timeoutError struct{}

func (timeoutError) Error() string   { return "simulated timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
