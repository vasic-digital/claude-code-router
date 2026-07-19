package config

// Generalises a gap surfaced by two upstream suites that both assume a
// provider carries an explicit wire-protocol/type:
//
//   - test/unit/providers/provider-url.test.mjs
//     (providerBaseUrlForProtocol takes a protocol argument —
//     "openai_chat_completions" | "openai_responses" | "anthropic_messages"
//     | "gemini_generate_content" | "gemini_interactions" — and derives a
//     different base per protocol from the same provider URL)
//   - test/unit/gateway/gateway-claude-code-oauth.test.mjs
//     (provider records carry `type: "anthropic_messages"`; OAuth
//     anthropic-beta header merging is applied only to providers of that
//     type, and left untouched for every other provider)
//
// Both rely on a provider record knowing which protocol its upstream
// speaks. config.Provider has no such field: it is name + api_base_url +
// api_key + models + an opt-in transformer list, and
// internal/translate.AnthropicToOpenAI unconditionally assumes every
// upstream speaks OpenAI chat-completions.
//
// Concretely this means: if an operator points a config.Provider's
// api_base_url at a real Anthropic-native endpoint (e.g.
// "https://api.anthropic.com/v1/messages" — exactly the shape Claude Code
// itself would otherwise call directly), this router still converts the
// request to OpenAI shape before sending it, which the real Anthropic API
// will reject. There is no way to configure a provider as "send my
// Anthropic-shaped request through unchanged."

import "testing"

func TestProviderProtocolTypeField_GAP(t *testing.T) {
	p := &Provider{
		Name:       "anthropic-native",
		APIBaseURL: "https://api.anthropic.com/v1/messages",
		APIKey:     "sk-ant-...",
		Models:     []string{"claude-opus-4-6"},
	}
	_ = p
	t.Skip("GAP: config.Provider has no protocol/type field distinguishing an " +
		"OpenAI-chat-completions upstream from an Anthropic-native one (or Gemini, or " +
		"OpenAI Responses); every provider is unconditionally translated to OpenAI shape " +
		"by internal/translate.AnthropicToOpenAI. A provider whose api_base_url is a real " +
		"Anthropic Messages endpoint cannot be proxied to correctly. (upstream: " +
		"test/unit/providers/provider-url.test.mjs, " +
		"test/unit/gateway/gateway-claude-code-oauth.test.mjs)")
}
