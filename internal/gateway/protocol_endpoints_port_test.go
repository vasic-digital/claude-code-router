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
// request path, and separately decides whether that request is eligible for
// model-based routing at all (POST + a small path allowlist; sub-resource
// paths like ".../interaction-1/cancel" are explicitly excluded even though
// they share a path prefix with a routable one).
//
// This repository's gateway currently only registers /health and /ready
// (see gateway.go's routes()); the Anthropic Messages endpoint itself
// (internal/gateway/messages.go) does not exist yet in this snapshot and is
// being built by another agent concurrently, so there is no
// requestProtocolForPath/shouldApplyGatewayRouting equivalent anywhere in
// this package to call. Every case below is GAP, not PORTED: the tests
// document the exact contract upstream enforces so the eventual endpoint
// wiring in messages.go has a ready-made acceptance table.

import "testing"

// TestRequestProtocolForPath_GAP documents upstream's requestProtocolForPath
// contract (protocol-endpoints.test.mjs, "request protocol detection covers
// every supported public endpoint shape"). Our gateway has no function that
// maps an inbound path to a wire protocol at all — every request this
// package currently serves is a fixed health/readiness probe, not a
// model-routed call.
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
		"this package registers only /health and /ready today. Once the /v1/messages " +
		"handler (internal/gateway/messages.go) lands, it should expose a path->protocol " +
		"classifier with the table encoded above (upstream: " +
		"test/unit/routing/protocol-endpoints.test.mjs).")
}

// TestShouldApplyGatewayRouting_GAP documents upstream's shouldApplyGatewayRouting
// contract: routing applies only to POST requests on a path allowlist, and
// sub-resource paths under an otherwise-routable prefix (Gemini Interactions
// "get status" / "cancel") are explicitly excluded even though they share
// the prefix, because the model is fixed by the interaction record — not by
// the request body — so a second routing decision would be meaningless.
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
	t.Skip("GAP: no shouldApplyGatewayRouting (or equivalent) exists anywhere in this " +
		"repository — routing today is entirely internal/router.Select, which is never " +
		"invoked from an HTTP path/method pair because no request-handling route besides " +
		"/health and /ready is registered yet. (upstream: " +
		"test/unit/routing/protocol-endpoints.test.mjs, " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}
