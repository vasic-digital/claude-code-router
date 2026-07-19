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
// request path via a reusable requestProtocolForPath classifier, and
// separately decides whether that request is eligible for model-based
// routing at all via shouldApplyGatewayRouting (POST + a small path
// allowlist; sub-resource paths like ".../interaction-1/cancel" are
// explicitly excluded even though they share a path prefix with a routable
// one, because the model for an in-flight interaction is fixed by the
// interaction record, not the request body).
//
// gateway.go now registers a real POST /v1/messages route (see
// internal/gateway/messages.go, added after this test-porting task began —
// it is off-limits to edit here), so the single-protocol subset of this
// contract (Anthropic Messages, POST-only) is real and PORTED below.
// Everything beyond that single hardcoded route is still GAP:
//   - there is no REUSABLE path->protocol classifier function anywhere —
//     "POST /v1/messages routes, everything else 404s" is an artifact of
//     gin's route table (routes() in gateway.go), not a callable function
//     upstream's requestProtocolForPath equivalent could be ported as;
//   - none of OpenAI chat-completions ("/chat/completions"), OpenAI
//     Responses ("/responses"), Gemini generateContent, Gemini
//     Interactions, or the "/proxy/v1/*" path aliases are recognised at
//     all — Claude Code is this gateway's only supported client shape.

import (
	"net/http"
	"net/http/httptest"
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
// upstream's "gateway routing applies only to POST model-selection
// endpoints": for the one endpoint this gateway actually implements
// (/v1/messages), only POST is routing-eligible — every other method 404s
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

// TestRequestProtocolForPath_GAP documents upstream's requestProtocolForPath
// contract (protocol-endpoints.test.mjs, "request protocol detection covers
// every supported public endpoint shape") in full. Only the first row
// ("/messages" family -> anthropic_messages, via the literal POST
// /v1/messages route) has any counterpart in this repository; there is no
// reusable classifier function, and none of the other protocol families are
// recognised at any path.
func TestRequestProtocolForPath_GAP(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/messages", "anthropic_messages"},
		{"/proxy/v1/messages", "anthropic_messages"},
		{"/chat/completions", "openai_chat_completions"},
		{"/proxy/v1/chat/completions", "openai_chat_completions"},
		{"/responses", "openai_responses"},
		{"/proxy/v1/responses", "openai_responses"},
		{"/v1/models/gemini-2.5-pro:generateContent", "gemini_generate_content"},
		{"/v1beta/models/gemini-2.5-pro:streamGenerateContent", "gemini_generate_content"},
		{"/v1/interactions", "gemini_interactions"},
		{"/v1beta/interactions/interaction-1", "gemini_interactions"},
		{"/v1beta/interactions/interaction-1/cancel", "gemini_interactions"},
		// Unrecognised shapes must resolve to "no protocol", not a guess.
		{"/v1/completions", ""},
		{"/v1beta/models/gemini-2.5-pro:countTokens", ""},
	}
	_ = cases
	t.Skip("GAP: no requestProtocolForPath (or equivalent) exists in internal/gateway; " +
		"routing to /v1/messages is a single hardcoded gin route (see gateway.go's " +
		"routes()), not a reusable path->protocol classifier, and no other protocol " +
		"family (OpenAI chat-completions, OpenAI Responses, Gemini generateContent, " +
		"Gemini Interactions, or any \"/proxy/v1/*\" alias) is recognised at any path. " +
		"(upstream: test/unit/routing/protocol-endpoints.test.mjs)")
}

// TestShouldApplyGatewayRouting_GAP documents upstream's full
// shouldApplyGatewayRouting contract across every protocol family. The
// POST-vs-other-methods slice of it, restricted to /v1/messages, is real —
// see TestOnlyPOSTMessagesIsRoutingEligible — but the path-allowlist and
// sub-resource-exclusion behaviour (Gemini Interactions "get status" /
// "cancel" sharing a prefix with a routable path yet being excluded from
// routing) has no counterpart, because none of those paths exist here at
// all.
func TestShouldApplyGatewayRouting_GAP(t *testing.T) {
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
	}
	_ = cases
	t.Skip("GAP: no shouldApplyGatewayRouting (or equivalent) exists as a callable function " +
		"anywhere in this repository, and only one of the paths in upstream's table " +
		"(/v1/messages) is implemented at all — see TestOnlyPOSTMessagesIsRoutingEligible " +
		"for the real subset of this contract that is PORTED. (upstream: " +
		"test/unit/routing/protocol-endpoints.test.mjs, " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}
