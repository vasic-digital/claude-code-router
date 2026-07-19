package gateway

// End-to-end proof that a provider with the anthropic protocol is routed
// through the gateway UNTRANSLATED on BOTH legs: the upstream receives the
// Anthropic body unchanged (only the router's model override applied), and the
// upstream's Anthropic-shaped response is relayed back to the client verbatim.
//
// This is the wired counterpart to internal/config's ResolvedProtocol tests and
// internal/translate's AnthropicPassthrough tests — those prove the pieces; this
// proves handleMessages actually uses them.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// recordingUpstream captures the exact bytes handleMessages sends upstream and
// returns a canned response, so a test can assert on BOTH what went out and what
// came back.
type recordingUpstream struct {
	gotBody []byte
	resp    *http.Response
}

func (u *recordingUpstream) Do(_ context.Context, _ config.Provider, body []byte) (*http.Response, error) {
	u.gotBody = append([]byte(nil), body...)
	return u.resp, nil
}

func anthropicCfg(protocol string) *config.Config {
	return &config.Config{
		Providers: []config.Provider{{
			Name: "native", APIBaseURL: "https://example/v1/messages",
			APIKey: "k", Models: []string{"claude-x"}, Protocol: protocol,
		}},
		Router: config.Route{Default: "native,routed-model"},
	}
}

func TestAnthropicNativePassthroughNonStreaming(t *testing.T) {
	const upstreamAnthropicResponse = `{"id":"msg_native","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"from the native upstream"}],` +
		`"model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			// An upstream-only header that must NOT reach the client — the relay
			// path rebuilds the response from the body alone and copies no
			// upstream header, exactly like the OpenAI path.
			"X-Upstream-Internal-Secret": []string{"must-not-leak"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamAnthropicResponse)),
	}}

	s := New(anthropicCfg(config.ProtocolAnthropic), Options{})
	s.Upstream = up

	// A request carrying top_k — a field the OpenAI translator drops entirely.
	// Its survival in gotBody is the proof the request was NOT translated.
	clientBody := `{"model":"claude-sent","max_tokens":16,"top_k":40,` +
		`"messages":[{"role":"user","content":"hello native"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// ---- request leg: sent unchanged, only model overridden ----
	var sent map[string]any
	if err := json.Unmarshal(up.gotBody, &sent); err != nil {
		t.Fatalf("upstream received non-JSON body: %v\n%s", err, up.gotBody)
	}
	if sent["model"] != "routed-model" {
		t.Errorf("upstream model = %v, want routed-model (router override)", sent["model"])
	}
	if _, ok := sent["top_k"]; !ok {
		t.Errorf("top_k was dropped — request was translated, not passed through:\n%s", up.gotBody)
	}
	// A translated request would carry OpenAI's leading system message / string
	// content shape; a passthrough keeps Anthropic's "messages" with a bare
	// string content. Assert the content is still the original string.
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want the original single message", sent["messages"])
	}
	if m0, _ := msgs[0].(map[string]any); m0["content"] != "hello native" {
		t.Errorf("message content = %v, want the original string (untranslated)", msgs[0])
	}

	// ---- response leg: relayed byte-for-byte ----
	if got := rec.Body.String(); got != upstreamAnthropicResponse {
		t.Errorf("client body was re-encoded, not relayed verbatim:\n got: %s\nwant: %s", got, upstreamAnthropicResponse)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if v := rec.Header().Get("X-Upstream-Internal-Secret"); v != "" {
		t.Errorf("relay leaked an upstream-only header: X-Upstream-Internal-Secret=%q", v)
	}
}

func TestAnthropicNativePassthroughStreaming(t *testing.T) {
	// A real Anthropic SSE stream. It must reach the client unchanged — NOT be
	// parsed as OpenAI chunks (which would silently produce an empty stream,
	// since these events have no OpenAI "choices[].delta").
	upstreamSSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}

	s := New(anthropicCfg(config.ProtocolAnthropic), Options{})
	s.Upstream = up

	clientBody := `{"model":"claude-sent","max_tokens":16,"stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := rec.Body.String(); got != upstreamSSE {
		t.Errorf("SSE stream was not relayed verbatim:\n got: %q\nwant: %q", got, upstreamSSE)
	}

	// The outgoing request was also a passthrough (stream flag preserved).
	if !bytes.Contains(up.gotBody, []byte(`"stream":true`)) {
		t.Errorf("upstream request lost stream flag:\n%s", up.gotBody)
	}
}

// The Anthropic-native streaming relay must record token usage from the stream
// itself: input_tokens from message_start, output_tokens from the (cumulative)
// message_delta — while still relaying the SSE byte-for-byte.
func TestAnthropicNativeStreamingRecordsTokens(t *testing.T) {
	upstreamSSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-x","usage":{"input_tokens":23,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	s := New(anthropicCfg(config.ProtocolAnthropic), Options{})
	s.Upstream = up

	clientBody := `{"model":"claude-sent","max_tokens":16,"stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != upstreamSSE {
		t.Errorf("SSE not relayed verbatim under the usage tee:\n got: %q\nwant: %q", got, upstreamSSE)
	}

	var buf bytes.Buffer
	s.Metrics.WriteExposition(&buf)
	out := buf.String()
	// anthropicCfg routes to provider "anthropic" model "routed-model" (see the
	// non-streaming passthrough test); assert on the substrings to stay robust to
	// the exact resolved names.
	if !strings.Contains(out, "ccr_gen_ai_input_tokens_total{") || !strings.Contains(out, "} 23") {
		t.Errorf("anthropic-native streaming input tokens not recorded (want 23):\n%s", out)
	}
	if !strings.Contains(out, "ccr_gen_ai_output_tokens_total{") || !strings.Contains(out, "} 12") {
		t.Errorf("anthropic-native streaming output tokens not recorded (want 12):\n%s", out)
	}
}

// A provider whose api_base_url is the canonical Anthropic endpoint but which
// sets NO explicit protocol must ALSO take the passthrough path, via inference.
// This proves the gateway consults ResolvedProtocol (inference included), not a
// bare Protocol string comparison.
func TestAnthropicPassthroughViaInference(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "native", APIBaseURL: "https://api.anthropic.com/v1/messages",
			APIKey: "k", Models: []string{"claude-x"}, // no Protocol field
		}},
		Router: config.Route{Default: "native,routed-model"},
	}
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"m","type":"message","role":"assistant","content":[],"model":"claude-x"}`)),
	}}
	s := New(cfg, Options{})
	s.Upstream = up

	clientBody := `{"model":"claude-sent","max_tokens":8,"top_k":7,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// top_k survives ⇒ inference selected the passthrough path.
	if !bytes.Contains(up.gotBody, []byte(`"top_k"`)) {
		t.Errorf("inference did not select passthrough — top_k was translated away:\n%s", up.gotBody)
	}
}

// The default (OpenAI) path must be entirely unaffected: a provider with no
// protocol and an ordinary chat-completions URL is still translated. This is the
// regression guard that the new branch did not change existing behaviour.
func TestOpenAIProviderStillTranslated(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "oai", APIBaseURL: "https://api.deepseek.com/chat/completions",
			APIKey: "k", Models: []string{"m"},
		}},
		Router: config.Route{Default: "oai,routed-model"},
	}
	up := &recordingUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"x","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)),
	}}
	s := New(cfg, Options{})
	s.Upstream = up

	clientBody := `{"model":"claude-sent","max_tokens":8,"system":"be terse","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Translation turns the top-level Anthropic "system" into a leading OpenAI
	// system message and drops the top-level "system" key.
	var sent map[string]any
	if err := json.Unmarshal(up.gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if _, ok := sent["system"]; ok {
		t.Errorf("top-level system survived — provider was NOT translated:\n%s", up.gotBody)
	}
	msgs, _ := sent["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages in translated body:\n%s", up.gotBody)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first message role = %v, want system (translation should have prepended it)", first["role"])
	}
	// And the client sees a re-encoded Anthropic message (id from the OpenAI id).
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body not JSON: %v", err)
	}
	if out["type"] != "message" {
		t.Errorf("client response was not re-encoded to an Anthropic message: %s", rec.Body.String())
	}
}
