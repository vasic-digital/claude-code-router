package gateway

import (
	"net/http"
	"strings"
)

// Wire-protocol family identifiers, matching upstream Node CCR's
// requestProtocolForPath vocabulary. These name the SHAPE of an inbound
// request as determined from its path alone; they are distinct from
// config.Provider.Protocol (which names the UPSTREAM's shape). A request can
// arrive as any of these families and still be routed to a provider of either
// upstream protocol.
const (
	protoAnthropicMessages     = "anthropic_messages"
	protoOpenAIChatCompletions = "openai_chat_completions"
	protoOpenAIResponses       = "openai_responses"
	protoGeminiGenerateContent = "gemini_generate_content"
	protoGeminiInteractions    = "gemini_interactions"
)

// normalizeProxy strips a trailing slash and an optional leading "/proxy"
// alias segment. Upstream exposes every endpoint both directly and under a
// "/proxy/v1/..." alias; both must classify identically.
func normalizeProxy(path string) string {
	return strings.TrimPrefix(strings.TrimSuffix(path, "/"), "/proxy")
}

// stripVersion removes a leading "/v1" or "/v1beta" version segment, so the
// version-independent families (Anthropic Messages, OpenAI chat-completions,
// OpenAI Responses) match whether or not a version prefix is present. The
// Gemini families are matched on the ORIGINAL path instead, because their
// classification is version-aware.
func stripVersion(p string) string {
	for _, v := range []string{"/v1beta", "/v1"} {
		if p == v {
			return ""
		}
		if strings.HasPrefix(p, v+"/") {
			return p[len(v):]
		}
	}
	return p
}

// requestProtocolForPath classifies an inbound request path to the wire
// protocol family it speaks, or "" for an unrecognised shape.
//
// It is a faithful port of upstream's pure classifier: path-only (method is
// irrelevant here — shouldApplyGatewayRouting layers the method rule on top),
// tolerant of the "/proxy/v1/*" alias, and deliberately strict at the edges —
// "/v1/completions" (bare completions, not chat) and a Gemini ":countTokens"
// call resolve to "" rather than being guessed into a neighbouring family.
func requestProtocolForPath(path string) string {
	p := normalizeProxy(path)
	switch stripVersion(p) {
	case "/messages":
		return protoAnthropicMessages
	case "/chat/completions":
		return protoOpenAIChatCompletions
	case "/responses":
		return protoOpenAIResponses
	}
	// Gemini generateContent / streamGenerateContent are addressed as a method
	// suffix on a model resource (".../models/<model>:generateContent").
	if strings.HasSuffix(p, ":generateContent") || strings.HasSuffix(p, ":streamGenerateContent") {
		return protoGeminiGenerateContent
	}
	// Gemini "Interactions": the collection (/interactions) and any
	// sub-resource beneath it (/interactions/<id>[/cancel]).
	if np := stripVersion(p); np == "/interactions" || strings.HasPrefix(np, "/interactions/") {
		return protoGeminiInteractions
	}
	return ""
}

// shouldApplyGatewayRouting reports whether an inbound request is eligible for
// model-based routing (i.e. the gateway should pick a provider from the request
// body's model), porting upstream's predicate:
//
//   - the method must be POST — a GET/DELETE on an otherwise-routable path is
//     a read/lifecycle call, not a completion, and must not be routed;
//   - the path must classify to a known protocol family;
//   - a Gemini Interactions SUB-resource (/interactions/<id> and
//     /interactions/<id>/cancel) is excluded even though it shares a prefix
//     with the routable collection: the model for an in-flight interaction is
//     fixed by the interaction record, not chosen from the request body.
//
// SCOPE (no bluff): this is a faithful, tested port of upstream's predicate,
// kept for the multi-path dispatch upstream performs. In THIS gateway only
// requestProtocolForPath is on the live path (handleInbound dispatches by it),
// and gin's route table already enforces the POST+path predicate for the two
// served endpoints — so shouldApplyGatewayRouting has no production caller yet
// and is exercised only by protocol_endpoints_port_test.go. It becomes
// load-bearing the day a single catch-all inbound route needs to gate routing
// eligibility itself instead of relying on per-path registration.
func shouldApplyGatewayRouting(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch requestProtocolForPath(path) {
	case "":
		return false
	case protoGeminiInteractions:
		// Only the collection endpoint selects a model from the body.
		return stripVersion(normalizeProxy(path)) == "/interactions"
	default:
		return true
	}
}
