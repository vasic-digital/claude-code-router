package gateway

// End-to-end tests for the OpenAI-compatible inbound facade
// (POST /v1/chat/completions). recordingUpstream / the shared helpers live in
// anthropic_passthrough_test.go (same package).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func openaiCfg(protocol string) *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name: "oai", APIBaseURL: "https://up.example/v1/chat/completions",
			APIKey: "k", Models: []string{"m"}, Protocol: protocol,
		}},
		Router: config.Route{Default: "oai,routed-model"},
	}
}

func TestOpenAIInboundPassthroughNonStreaming(t *testing.T) {
	const upstreamOpenAIResponse = `{"id":"chatcmpl-1","object":"chat.completion",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello from provider"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":4}}`

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamOpenAIResponse)),
	}}
	s := New(openaiCfg(""), Options{}) // no protocol -> openai via inference
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","temperature":0.5,` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// ---- request leg: model overridden, everything else preserved ----
	var sent map[string]any
	if err := json.Unmarshal(up.gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, up.gotBody)
	}
	if sent["model"] != "routed-model" {
		t.Errorf("upstream model = %v, want routed-model", sent["model"])
	}
	for _, k := range []string{"temperature", "tools", "messages"} {
		if _, ok := sent[k]; !ok {
			t.Errorf("field %q dropped from forwarded OpenAI body:\n%s", k, up.gotBody)
		}
	}

	// ---- response leg: OpenAI response relayed verbatim ----
	if got := rec.Body.String(); got != upstreamOpenAIResponse {
		t.Errorf("client body was not relayed verbatim:\n got: %s\nwant: %s", got, upstreamOpenAIResponse)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestOpenAIInboundPassthroughStreaming(t *testing.T) {
	upstreamSSE := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	s := New(openaiCfg(config.ProtocolOpenAI), Options{})
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := rec.Body.String(); got != upstreamSSE {
		t.Errorf("OpenAI SSE not relayed verbatim:\n got: %q\nwant: %q", got, upstreamSSE)
	}
}

// An OpenAI-inbound request routed to an Anthropic-native provider must fail
// with an explicit 501 (OpenAI error envelope naming the provider), NOT send an
// OpenAI body to a Messages API. The upstream must never be called.
func TestOpenAIInboundToAnthropicProviderIs501(t *testing.T) {
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}}
	s := New(openaiCfg(config.ProtocolAnthropic), Options{})
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if up.gotBody != nil {
		t.Errorf("upstream was called for an unsupported bridge; body:\n%s", up.gotBody)
	}
	// OpenAI error envelope, naming the provider.
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an OpenAI error object: %v\n%s", err, rec.Body.String())
	}
	if !strings.Contains(e.Error.Message, "oai") {
		t.Errorf("error message should name provider %q: %s", "oai", e.Error.Message)
	}
}

// An upstream failure on the OpenAI path must be reported in OpenAI error shape,
// preserving the upstream status — not collapsed to a generic 502, and not in
// Anthropic shape.
func TestOpenAIInboundUpstreamErrorIsOpenAIShaped(t *testing.T) {
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusUnauthorized, // Terminal -> forwarded, not retried
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad key","type":"authentication_error"}}`)),
	}}
	s := New(openaiCfg(config.ProtocolOpenAI), Options{})
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (upstream status preserved)", rec.Code)
	}
	// Must be an OpenAI {"error":{...}} object, and must NOT be the Anthropic
	// {"type":"error","error":{...}} envelope.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &probe); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, ok := probe["error"]; !ok {
		t.Errorf("no top-level error object: %s", rec.Body.String())
	}
	if _, isAnthropic := probe["type"]; isAnthropic {
		t.Errorf("response used the Anthropic error envelope, want OpenAI shape: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bad key") {
		t.Errorf("upstream error message not forwarded: %s", rec.Body.String())
	}
}

// The new /v1/chat/completions route is gated by the same RequireAPIKey
// middleware as /v1/messages when APIKeys is configured.
func TestOpenAIInboundRequiresAPIKeyWhenConfigured(t *testing.T) {
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)),
	}}
	s := New(openaiCfg(config.ProtocolOpenAI), Options{APIKeys: []string{"secret"}})
	s.Upstream = up

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`

	// No credential -> 401.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d, want 401", rec.Code)
	}

	// Correct credential -> 200 (a fresh body is needed since the first was consumed).
	up.resp.Body = io.NopCloser(strings.NewReader(`{"id":"c","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-api-key", "secret")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Errorf("with key = %d, want 200 (body: %s)", rec2.Code, rec2.Body.String())
	}
}
