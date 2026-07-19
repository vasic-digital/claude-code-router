package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// Regression guard for a finding from the security suite.
//
// Both upstream-response reads already used io.LimitReader (64KiB for error
// bodies, 32MiB for completions), but the INBOUND request path was an
// unbounded io.ReadAll. A single client could stream an arbitrarily large body
// and drive the gateway to OOM, killing every other in-flight request with it.
func TestOversizedRequestBodyIsRejectedNotOOM(t *testing.T) {
	s := New(testCfg(), Options{})

	// One byte over the cap, sent as a stream so the test does not itself
	// allocate 32MiB: an io.Reader body means the handler must enforce the
	// limit while reading, which is exactly the property under test.
	oversized := io.MultiReader(
		strings.NewReader(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"`),
		&repeatReader{b: 'a', n: maxRequestBodyBytes + 1},
		strings.NewReader(`"}]}`),
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", oversized)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for an oversized body (body: %s)",
			rec.Code, truncate(rec.Body.String(), 200))
	}

	// The rejection must still be a well-formed Anthropic error, not a bare
	// string — clients parse this.
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("413 body is not valid JSON: %q", truncate(rec.Body.String(), 200))
	}
	if errResp.Type != "error" {
		t.Errorf("response type = %q, want \"error\"", errResp.Type)
	}
	// The message must say the body was too large, not "invalid JSON" — a
	// plain LimitReader would truncate mid-token and produce exactly that
	// misleading diagnosis.
	if !strings.Contains(strings.ToLower(errResp.Error.Message), "limit") &&
		!strings.Contains(strings.ToLower(errResp.Error.Message), "large") {
		t.Errorf("message %q does not explain that the body was too large", errResp.Error.Message)
	}
}

// A body comfortably under the cap must still be served normally, so the guard
// cannot be "passing" by rejecting everything.
func TestNormalSizedRequestBodyStillAccepted(t *testing.T) {
	s := New(testCfg(), Options{})
	s.Upstream = stubUpstream{}

	body := fmt.Sprintf(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("a", 1024))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a 1KiB body was rejected as too large — the cap is misconfigured")
	}
}

// repeatReader streams n copies of a byte without allocating them up front.
type repeatReader struct {
	b byte
	n int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.n {
		n = r.n
	}
	for i := 0; i < n; i++ {
		p[i] = r.b
	}
	r.n -= n
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stubUpstream returns a minimal valid OpenAI completion so the handler can
// reach a success path without any network.
type stubUpstream struct{}

func (stubUpstream) Do(_ context.Context, _ config.Provider, _ []byte) (*http.Response, error) {
	const body = `{"id":"c1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}
