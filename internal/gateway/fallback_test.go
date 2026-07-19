package gateway

// Cross-provider fallback (Router.crossProviderFallback). MaxAttempts:1 removes
// same-provider retries so each provider is tried exactly once and a Retryable
// primary advances immediately (no backoff), isolating the fallback behaviour.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// fallbackUpstream returns a per-provider status + body and records calls/bodies
// per provider name, so a test can prove which providers were tried and what
// each received.
type fallbackUpstream struct {
	mu       sync.Mutex
	calls    map[string]int
	bodies   map[string][]byte
	status   map[string]int
	respBody map[string]string
	errs     map[string]error // per-provider transport error (returned instead of a response)
}

func newFallbackUpstream() *fallbackUpstream {
	return &fallbackUpstream{calls: map[string]int{}, bodies: map[string][]byte{}, status: map[string]int{}, respBody: map[string]string{}, errs: map[string]error{}}
}

func (u *fallbackUpstream) Do(_ context.Context, p config.Provider, body []byte) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls[p.Name]++
	u.bodies[p.Name] = append([]byte(nil), body...)
	if e := u.errs[p.Name]; e != nil {
		return nil, e
	}
	st := u.status[p.Name]
	if st == 0 {
		st = http.StatusOK
	}
	rb := u.respBody[p.Name]
	if rb == "" {
		rb = `{"error":{"message":"upstream error"}}`
	}
	return &http.Response{
		StatusCode: st,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(rb)),
	}, nil
}

func (u *fallbackUpstream) callsFor(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[name]
}

// two providers, both serving model "m"; primary is the routed default.
func fallbackCfg(cross bool) *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "primary", APIBaseURL: "https://p/v1/chat/completions", APIKey: "k", Models: []string{"m"}},
			{Name: "secondary", APIBaseURL: "https://s/v1/chat/completions", APIKey: "k", Models: []string{"m"}},
		},
		Router: config.Route{Default: "primary,m", CrossProviderFallback: cross},
	}
}

func postMessages(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

const secondaryAnswer = `{"id":"sec","choices":[{"index":0,"message":{"role":"assistant","content":"from secondary"},"finish_reason":"stop"}]}`

// A RETRYABLE primary failure advances to the next provider serving the model,
// which succeeds — and the winning provider received a properly re-translated
// body (model "m").
func TestFallbackAdvancesOnRetryableFailure(t *testing.T) {
	up := newFallbackUpstream()
	up.status["primary"] = http.StatusServiceUnavailable // 503, Retryable
	up.status["secondary"] = http.StatusOK
	up.respBody["secondary"] = secondaryAnswer

	s := New(fallbackCfg(true), Options{MaxAttempts: 1})
	s.Upstream = up
	rec := postMessages(t, s)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback should have succeeded); body: %s", rec.Code, rec.Body.String())
	}
	if up.callsFor("primary") != 1 {
		t.Errorf("primary calls = %d, want 1", up.callsFor("primary"))
	}
	if up.callsFor("secondary") != 1 {
		t.Errorf("secondary calls = %d, want 1 (fallback must advance on 503)", up.callsFor("secondary"))
	}
	// The winning provider got a re-translated OpenAI body with model "m".
	var sent map[string]any
	if err := json.Unmarshal(up.bodies["secondary"], &sent); err != nil {
		t.Fatalf("secondary body not JSON: %v", err)
	}
	if sent["model"] != "m" {
		t.Errorf("secondary model = %v, want m", sent["model"])
	}
	// The client received the secondary's answer, translated to Anthropic shape.
	if !strings.Contains(rec.Body.String(), "from secondary") {
		t.Errorf("client did not receive secondary's answer: %s", rec.Body.String())
	}
}

// A RETRYABLE TRANSPORT error on the primary (a timeout — see
// router.ClassifyTransportError) advances to the next provider, exactly like a
// retryable status. This covers the transport half of the tryNext contract.
func TestFallbackAdvancesOnTransportError(t *testing.T) {
	up := newFallbackUpstream()
	up.errs["primary"] = timeoutError{} // net.Error, Timeout()==true => Retryable
	up.status["secondary"] = http.StatusOK
	up.respBody["secondary"] = secondaryAnswer

	s := New(fallbackCfg(true), Options{MaxAttempts: 1})
	s.Upstream = up
	rec := postMessages(t, s)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a retryable transport error must fall back); body: %s", rec.Code, rec.Body.String())
	}
	if up.callsFor("primary") != 1 || up.callsFor("secondary") != 1 {
		t.Errorf("calls primary=%d secondary=%d, want 1 and 1", up.callsFor("primary"), up.callsFor("secondary"))
	}
	if !strings.Contains(rec.Body.String(), "from secondary") {
		t.Errorf("client did not receive secondary's answer: %s", rec.Body.String())
	}
}

// A TERMINAL primary failure (401) must NOT fall back — a bad credential fails
// the request; it is not a signal that the primary is unhealthy.
func TestFallbackNotTriggeredOnTerminalFailure(t *testing.T) {
	up := newFallbackUpstream()
	up.status["primary"] = http.StatusUnauthorized // 401, Terminal
	up.respBody["primary"] = `{"error":{"message":"bad key","type":"authentication_error"}}`
	up.status["secondary"] = http.StatusOK
	up.respBody["secondary"] = secondaryAnswer

	s := New(fallbackCfg(true), Options{MaxAttempts: 1})
	s.Upstream = up
	rec := postMessages(t, s)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if up.callsFor("secondary") != 0 {
		t.Errorf("secondary calls = %d, want 0 — a Terminal failure must not fall back", up.callsFor("secondary"))
	}
}

// Every provider RETRYABLE-fails: all are tried, and the LAST provider's error
// is what reaches the client.
func TestFallbackExhaustedForwardsLastError(t *testing.T) {
	up := newFallbackUpstream()
	up.status["primary"] = http.StatusServiceUnavailable // 503
	up.status["secondary"] = http.StatusBadGateway       // 502
	up.respBody["secondary"] = `{"error":{"message":"secondary down"}}`

	s := New(fallbackCfg(true), Options{MaxAttempts: 1})
	s.Upstream = up
	rec := postMessages(t, s)

	if up.callsFor("primary") != 1 || up.callsFor("secondary") != 1 {
		t.Fatalf("calls primary=%d secondary=%d, want 1 and 1", up.callsFor("primary"), up.callsFor("secondary"))
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (last provider's error forwarded)", rec.Code)
	}
}

// With fallback OFF (the default), a Retryable primary failure is forwarded and
// no other provider is tried — byte-identical to before the feature.
func TestFallbackDisabledStaysSingleProvider(t *testing.T) {
	up := newFallbackUpstream()
	up.status["primary"] = http.StatusServiceUnavailable // 503
	up.status["secondary"] = http.StatusOK
	up.respBody["secondary"] = secondaryAnswer

	s := New(fallbackCfg(false), Options{MaxAttempts: 1})
	s.Upstream = up
	rec := postMessages(t, s)

	if up.callsFor("secondary") != 0 {
		t.Errorf("secondary calls = %d, want 0 with fallback disabled", up.callsFor("secondary"))
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (forwarded, no fallback)", rec.Code)
	}
}
