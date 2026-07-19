package gateway

// End-to-end tests for the OpenAI-compatible inbound facade
// (POST /v1/chat/completions). recordingUpstream / the shared helpers live in
// anthropic_passthrough_test.go (same package).

import (
	"bytes"
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

// The OpenAI inbound facade must attribute its upstream call and token usage in
// metrics, exactly like the Anthropic path — a /v1/chat/completions request that
// reaches the upstream was previously invisible to ccr_gen_ai_upstream_requests_total
// and the token counters (only the RED http_requests middleware counted it). This
// pins the fix AND guards that metric parsing never mutates the verbatim-relayed
// body.
func TestOpenAIInboundRecordsUpstreamAndTokenMetrics(t *testing.T) {
	const upstreamOpenAIResponse = `{"id":"chatcmpl-1","object":"chat.completion",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":11}}`

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamOpenAIResponse)),
	}}
	s := New(openaiCfg(""), Options{})
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The body must still be relayed byte-for-byte — recording tokens reads the
	// buffered body but must not alter what the client receives.
	if got := rec.Body.String(); got != upstreamOpenAIResponse {
		t.Errorf("body not relayed verbatim after metric parsing:\n got: %s\nwant: %s", got, upstreamOpenAIResponse)
	}

	var buf bytes.Buffer
	s.Metrics.WriteExposition(&buf)
	out := buf.String()

	// Upstream attributed exactly once (no fallback on this path), to the routed
	// provider+model.
	wantUpstream := `ccr_gen_ai_upstream_requests_total{provider="oai",model="routed-model"} 1`
	if !metricLinePresent(out, wantUpstream) {
		t.Errorf("missing/incorrect upstream metric line %q in:\n%s", wantUpstream, out)
	}
	// Token counters carry the OpenAI usage block's prompt/completion tokens.
	wantInput := `ccr_gen_ai_input_tokens_total{provider="oai",model="routed-model"} 7`
	if !metricLinePresent(out, wantInput) {
		t.Errorf("missing/incorrect input-tokens metric line %q in:\n%s", wantInput, out)
	}
	wantOutput := `ccr_gen_ai_output_tokens_total{provider="oai",model="routed-model"} 11`
	if !metricLinePresent(out, wantOutput) {
		t.Errorf("missing/incorrect output-tokens metric line %q in:\n%s", wantOutput, out)
	}
}

// metricLinePresent reports whether the Prometheus exposition contains a line
// exactly equal to want (trimmed), tolerant of surrounding whitespace but strict
// on the value so a double-count (…} 2) fails.
func metricLinePresent(exposition, want string) bool {
	for _, line := range strings.Split(exposition, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// The OpenAI facade's STREAMING path must also record token usage (from the
// terminal usage chunk) at parity with non-streaming — and must still relay the
// SSE byte-for-byte (the tee observes chunks after they are written).
func TestOpenAIInboundStreamingRecordsTokens(t *testing.T) {
	upstreamSSE := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":9}}` + "\n\n" +
		"data: [DONE]\n\n"

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	s := New(openaiCfg(""), Options{})
	s.Upstream = up

	clientBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Verbatim relay preserved despite the usage tee.
	if got := rec.Body.String(); got != upstreamSSE {
		t.Errorf("streaming SSE not relayed verbatim:\n got: %q\nwant: %q", got, upstreamSSE)
	}

	var buf bytes.Buffer
	s.Metrics.WriteExposition(&buf)
	out := buf.String()
	if !metricLinePresent(out, `ccr_gen_ai_input_tokens_total{provider="oai",model="routed-model"} 17`) {
		t.Errorf("streaming input tokens not recorded (want 17):\n%s", out)
	}
	if !metricLinePresent(out, `ccr_gen_ai_output_tokens_total{provider="oai",model="routed-model"} 9`) {
		t.Errorf("streaming output tokens not recorded (want 9):\n%s", out)
	}
}

// A streaming OpenAI response WITHOUT a usage chunk (client did not request
// stream_options.include_usage) records nothing — a legitimate best-effort miss,
// not a spurious zero-labelled series or an error.
func TestOpenAIInboundStreamingNoUsageRecordsNothing(t *testing.T) {
	upstreamSSE := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	s := New(openaiCfg(""), Options{})
	s.Upstream = up

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var buf bytes.Buffer
	s.Metrics.WriteExposition(&buf)
	// The family's # TYPE header always prints; assert no labelled SERIES exists
	// (a data line carries a `{` — RecordTokens ignores the 0/0 counts, so none
	// is created).
	if strings.Contains(buf.String(), `ccr_gen_ai_input_tokens_total{provider="oai"`) {
		t.Errorf("no-usage stream must not emit a token series:\n%s", buf.String())
	}
}

// Pure scanner tests — no server, exercising the tee's parsing directly.
func TestOpenAIStreamUsageScanner(t *testing.T) {
	var sc openAIStreamUsageScanner
	for _, line := range []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`event: ping`,
		`data: not-json`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":8}}`,
		`data: [DONE]`,
	} {
		sc.observe(line + "\n")
	}
	if in, out := sc.totals(); in != 42 || out != 8 {
		t.Errorf("openAI scanner totals = %d/%d, want 42/8", in, out)
	}

	var empty openAIStreamUsageScanner
	empty.observe(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n")
	if in, out := empty.totals(); in != 0 || out != 0 {
		t.Errorf("no-usage scanner totals = %d/%d, want 0/0", in, out)
	}
}

func TestAnthropicStreamUsageScanner(t *testing.T) {
	var sc anthropicStreamUsageScanner
	for _, line := range []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":31,"output_tokens":0}}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":14}}`,
		`data: {"type":"message_stop"}`,
	} {
		sc.observe(line + "\n")
	}
	// input from message_start; output is the LAST positive delta (cumulative).
	if in, out := sc.totals(); in != 31 || out != 14 {
		t.Errorf("anthropic scanner totals = %d/%d, want 31/14", in, out)
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
