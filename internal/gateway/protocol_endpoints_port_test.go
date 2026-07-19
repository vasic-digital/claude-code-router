package gateway

// Ports the behavioural intent of two upstream Node CCR suites:
//
//   - test/unit/routing/protocol-endpoints.test.mjs
//     (requestProtocolForPath, shouldApplyGatewayRouting)
//   - test/unit/gateway/routing-architecture.test.mjs
//     (the "gateway routing runs for body-model protocols independent of
//     agent user-agent" and "body-model protocols do not require route input
//     adaptation" cases, which duplicate the protocol-endpoints contract)
//
// Upstream Node CCR resolves, for every inbound HTTP request, which wire
// protocol it speaks (Anthropic Messages, OpenAI chat-completions, OpenAI
// Responses, Gemini generateContent, Gemini "Interactions") purely from the
// request path via a reusable requestProtocolForPath classifier, and separately
// decides whether that request is eligible for model-based routing via
// shouldApplyGatewayRouting (POST + a path allowlist; interaction sub-resources
// like ".../interaction-1/cancel" are excluded even though they share a prefix
// with a routable one, because the model for an in-flight interaction is fixed
// by the interaction record, not the request body).
//
// GAP CLOSED. Both classifiers are now REAL, callable functions (see
// internal/gateway/protocol.go) and BOTH are load-bearing: handleInbound
// (openai_inbound.go) dispatches every inbound request via
// requestProtocolForPath, and routes() registers the routable POST paths. The
// two previous t.Skip placeholders — which asserted the classifiers did not
// exist — are replaced by real tests running upstream's full path tables.
//
// Coverage note (honest scope): the classifier recognises all five families as
// a faithful port of upstream's pure function, but the gateway currently SERVES
// two of them — Anthropic Messages (/v1/messages) and OpenAI chat-completions
// (/v1/chat/completions, see openai_inbound.go). OpenAI Responses and the two
// Gemini families are classified but not yet given live handlers; they are
// documented as recognised-not-served rather than silently mishandled.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func portTestCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{{Name: "p1", APIBaseURL: "https://up.example/v1/chat/completions", APIKey: "k", Models: []string{"m1"}}},
		Router:    config.Route{Default: "p1,m1"},
	}
}

// TestOnlyPOSTMessagesIsRoutingEligible is the real, narrow analogue of
// upstream's "gateway routing applies only to POST model-selection endpoints":
// for /v1/messages, only POST is routing-eligible — every other method 404s
// rather than being silently accepted or misrouted.
func TestOnlyPOSTMessagesIsRoutingEligible(t *testing.T) {
	s := New(portTestCfg(), Options{})
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/v1/messages", nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s /v1/messages = 200, want routing NOT to apply to a non-POST method", method)
		}
	}
}

// TestRequestProtocolForPath ports upstream's requestProtocolForPath contract in
// full: "request protocol detection covers every supported public endpoint
// shape", including the "/proxy/v1/*" alias and the strict edges where an
// unrecognised shape must resolve to "" (no protocol) rather than a guess.
func TestRequestProtocolForPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/messages", protoAnthropicMessages},
		{"/proxy/v1/messages", protoAnthropicMessages},
		{"/chat/completions", protoOpenAIChatCompletions},
		{"/proxy/v1/chat/completions", protoOpenAIChatCompletions},
		{"/responses", protoOpenAIResponses},
		{"/proxy/v1/responses", protoOpenAIResponses},
		{"/v1/models/gemini-2.5-pro:generateContent", protoGeminiGenerateContent},
		{"/v1beta/models/gemini-2.5-pro:streamGenerateContent", protoGeminiGenerateContent},
		{"/v1/interactions", protoGeminiInteractions},
		{"/v1beta/interactions/interaction-1", protoGeminiInteractions},
		{"/v1beta/interactions/interaction-1/cancel", protoGeminiInteractions},
		// Unrecognised shapes must resolve to "no protocol", not a guess.
		{"/v1/completions", ""},                           // bare completions, not chat
		{"/v1beta/models/gemini-2.5-pro:countTokens", ""}, // not a generate call
		{"/v1/embeddings", ""},                            // unrelated OpenAI endpoint
		{"/", ""},                                         // root
	}
	for _, tc := range cases {
		if got := requestProtocolForPath(tc.path); got != tc.want {
			t.Errorf("requestProtocolForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestShouldApplyGatewayRouting ports upstream's shouldApplyGatewayRouting
// contract across every protocol family, including the POST-only rule and the
// interaction sub-resource exclusion (a specific interaction / its cancel share
// a prefix with the routable collection yet are NOT routing-eligible).
func TestShouldApplyGatewayRouting(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"POST", "/v1/messages", true},
		{"POST", "/v1/chat/completions", true},
		{"POST", "/v1/responses", true},
		{"POST", "/v1beta/interactions", true},
		{"POST", "/v1beta/interactions/interaction-123", false},
		{"POST", "/v1beta/interactions/interaction-123/cancel", false},
		{"POST", "/v1beta/models/gemini:generateContent", true},
		{"GET", "/v1/messages", false},
		{"DELETE", "/v1beta/interactions", false},
		{"POST", "/v1/completions", false}, // unrecognised path is never routable
	}
	for _, tc := range cases {
		if got := shouldApplyGatewayRouting(tc.method, tc.path); got != tc.want {
			t.Errorf("shouldApplyGatewayRouting(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestProxyAliasReachesMessagesHandler proves the "/proxy/v1/*" alias is not
// merely classified but actually WIRED: a POST to the alias reaches the same
// Anthropic handler as the canonical path (200 via a stub upstream), so the
// classifier's alias handling is load-bearing end-to-end.
func TestProxyAliasReachesMessagesHandler(t *testing.T) {
	s := New(testCfg(), Options{})
	s.Upstream = authOKUpstream{} // canned OpenAI completion -> 200

	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/proxy/v1/messages = %d, want 200 (alias must reach the messages handler)", rec.Code)
	}
}
