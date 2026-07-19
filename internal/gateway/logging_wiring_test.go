package gateway

// Tests that the per-request access-logging middleware (logging_middleware.go)
// is actually MOUNTED on the route table built by New/routes() — not merely
// unit-tested in isolation against a throwaway gin engine (that is
// logging_middleware_test.go's job). These drive the REAL *Server through
// Handler(), capture the logger's actual output in a bytes.Buffer via the real
// internal/logging API, and prove the security invariant end to end:
//
//   - one http_request line per request, carrying method/path/status;
//   - a configured provider api_key, an inbound Authorization Bearer token,
//     and an inbound x-api-key value NEVER appear anywhere in the output;
//   - request and response BODY content never appears either;
//   - the redactor backing the logger scrubs secret-shaped values to the fixed
//     [REDACTED] marker (defence in depth);
//   - the access log honours the logger's level (CCR_LOG_LEVEL) — it is logged
//     at Info, so a Warn/Error logger suppresses it.
//
// logLines (defined in logging_middleware_test.go, same package) parses the
// JSON log buffer for us.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/logging"
)

// httpRequestLines returns just the access-log entries (msg == "http_request")
// parsed from buf.
func httpRequestLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range logLines(t, buf) {
		if line["msg"] == "http_request" {
			out = append(out, line)
		}
	}
	return out
}

// The distinctive secrets/markers threaded through the full-gateway test. Each
// is long and unique so a substring search for it in the log output is a
// reliable leak detector.
const (
	provProviderKey  = "PROVIDER_APIKEY_sk-upstreamsecret0123456789abcdefLEAK"
	provInboundBear  = "sk-inboundbearersecret0123456789abcdefLEAK"
	provInboundXKey  = "INBOUND_XAPIKEY_clientsecret0123456789abcdefLEAK"
	provPromptMarker = "PROMPT_BODY_MARKER_a1b2c3d4e5"
	provComplMarker  = "COMPLETION_BODY_MARKER_f6e7d8c9"
)

// fullGatewayWithBufferLogger builds a real Server whose access log is captured
// in buf, wired to a fake OpenAI-shaped upstream that echoes provComplMarker.
// The provider is configured with provProviderKey as its api_key.
func fullGatewayWithBufferLogger(t *testing.T, buf *bytes.Buffer, apiKeys []string) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-logtest",
			"choices": [{"index":0,"message":{"role":"assistant","content":"` + provComplMarker + `"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":1,"completion_tokens":1}
		}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "fake",
			APIBaseURL: upstream.URL,
			APIKey:     provProviderKey,
			Models:     []string{"fake-model"},
		}},
		Router: config.Route{Default: "fake,fake-model"},
	}
	logger := logging.NewWithOptions(buf, slog.LevelInfo, logging.FormatJSON)
	return New(cfg, Options{APIKeys: apiKeys, Logger: logger})
}

func promptBody() []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"stream":     false,
		"messages": []map[string]any{
			{"role": "user", "content": provPromptMarker},
		},
	})
	return b
}

// A request driven through the real gateway is logged exactly once with
// method/path/status, and NONE of the provider api_key, the inbound
// Authorization/x-api-key credentials, or the request/response bodies leak into
// the captured log output.
func TestGatewayAccessLogEmitsLineWithoutLeakingSecrets(t *testing.T) {
	var buf bytes.Buffer
	// APIKeys empty -> auth disabled, so the request reaches the upstream and
	// returns 200; the inbound credentials below are still present on the
	// request object the logging middleware sees, and must still never be
	// logged.
	s := fullGatewayWithBufferLogger(t, &buf, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(promptBody()))
	req.Header.Set("Authorization", "Bearer "+provInboundBear)
	req.Header.Set("x-api-key", provInboundXKey)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	// Sanity: the response body really did carry the completion marker, so the
	// "marker absent from logs" assertion below is meaningful and not vacuous.
	if !strings.Contains(rec.Body.String(), provComplMarker) {
		t.Fatalf("test setup broken: response body missing its completion marker: %s", rec.Body.String())
	}

	lines := httpRequestLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d http_request log lines, want exactly 1: %v", len(lines), lines)
	}
	line := lines[0]
	if line["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", line["method"])
	}
	if line["path"] != "/v1/messages" {
		t.Errorf("path = %v, want /v1/messages", line["path"])
	}
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", line["status"])
	}

	out := buf.String()
	for _, secret := range []struct{ what, val string }{
		{"provider api_key", provProviderKey},
		{"inbound Bearer token", provInboundBear},
		{"inbound x-api-key", provInboundXKey},
		{"request body (prompt)", provPromptMarker},
		{"response body (completion)", provComplMarker},
	} {
		if strings.Contains(out, secret.val) {
			t.Fatalf("%s leaked into access log output:\n%s", secret.what, out)
		}
	}
}

// Even a request REJECTED by RequireAPIKey (401) is logged — and the wrong key
// the client presented is not written to the log.
func TestGatewayAccessLogOnAuthRejectionDoesNotLeakPresentedKey(t *testing.T) {
	var buf bytes.Buffer
	s := fullGatewayWithBufferLogger(t, &buf, []string{"the-correct-key"})

	const wrongKey = "WRONG_PRESENTED_KEY_sk-attacker0123456789abcdef"
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(promptBody()))
	req.Header.Set("x-api-key", wrongKey)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	lines := httpRequestLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d http_request log lines, want exactly 1 (a rejected request is still logged): %v", len(lines), lines)
	}
	if lines[0]["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("status = %v, want 401", lines[0]["status"])
	}
	if strings.Contains(buf.String(), wrongKey) {
		t.Fatalf("the presented (wrong) key leaked into access log output:\n%s", buf.String())
	}
}

// The redactor backing the gateway's logger scrubs secret-shaped values to the
// FIXED [REDACTED] marker — never a truncated prefix of the secret. This
// exercises the exact logging.NewWithOptions construction the gateway uses, so
// it is the defence-in-depth guarantee the access log relies on, proven
// directly. This is where the literal marker is shown to appear in place of a
// secret.
func TestGatewayLoggerRedactsSecretShapedValuesToMarker(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.NewWithOptions(&buf, slog.LevelInfo, logging.FormatJSON)

	const secret = "sk-superSecretProviderKey0123456789abcdefZZ"
	logger.Info("http_request",
		"api_key", secret, // sensitive KEY -> whole value redacted
		"note", "Authorization: Bearer "+secret, // secret-shaped VALUE -> redacted in place
	)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret survived redaction into log output:\n%s", out)
	}
	if !strings.Contains(out, logging.RedactedMarker) {
		t.Fatalf("expected the fixed %q marker in output, got:\n%s", logging.RedactedMarker, out)
	}
	// The marker must be the WHOLE replacement, never carrying a prefix of the
	// real key alongside it.
	if strings.Contains(out, "sk-super") {
		t.Fatalf("a prefix of the secret leaked alongside the marker:\n%s", out)
	}
}

// The access log is emitted at Info level, so the logger's configured level
// (which internal/logging derives from CCR_LOG_LEVEL) gates whether it appears.
// Driving the real gateway with loggers pinned at each level proves the
// filtering end to end.
func TestGatewayAccessLogRespectsLevelFiltering(t *testing.T) {
	cases := []struct {
		name       string
		level      slog.Level
		wantLogged bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := logging.NewWithOptions(&buf, tc.level, logging.FormatJSON)
			s := New(testCfg(), Options{Logger: logger})

			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/health status = %d, want 200", rec.Code)
			}

			got := len(httpRequestLines(t, &buf))
			if tc.wantLogged && got == 0 {
				t.Errorf("level %v: expected an http_request line, got none", tc.level)
			}
			if !tc.wantLogged && got != 0 {
				t.Errorf("level %v: expected the Info-level access log to be filtered out, got %d line(s): %s",
					tc.level, got, buf.String())
			}
		})
	}
}
