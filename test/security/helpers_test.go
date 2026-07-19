// Package security probes the gateway and its proxy layer for the specific
// hazards a request router sits directly on top of: secret leakage on error
// paths, header-injection via attacker- or misconfiguration-controlled
// strings landing in real HTTP headers, SSRF-shaped upstream configuration,
// and unbounded resource consumption from untrusted input sizes.
//
// Every secret used anywhere in this package is a clearly-fake placeholder
// (never a real credential), and no test writes any secret-shaped value to
// disk — failures here are asserted purely against in-memory HTTP responses
// and Go errors.
package security

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
)

const defaultBound = 5 * time.Second

// runBounded executes fn in a goroutine and fails the test if it does not
// return within timeout, so a regression that turns a clean error path into
// a hang fails this test quickly instead of wedging the suite.
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
		t.Fatalf("scenario did not complete within %s", timeout)
	}
}

// gwServer builds a *gateway.Server with a single provider pointed at
// upstreamURL and carrying apiKey, so error paths that might (incorrectly)
// echo request material back have something concrete to leak.
func gwServer(upstreamURL, apiKey string) *gateway.Server {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "sec-provider",
			APIBaseURL: upstreamURL,
			APIKey:     apiKey,
			Models:     []string{"sec-model"},
		}},
		Router: config.Route{Default: "sec-provider,sec-model"},
	}
	return gateway.New(cfg, gateway.Options{UpstreamTimeout: 2 * time.Second})
}

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
// handler in-process.
func postMessages(s *gateway.Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicBody(false)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}
