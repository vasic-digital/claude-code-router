package config

// Ports test/unit/providers/provider-url.test.mjs.
//
// Upstream Node CCR accepts a bare host, a scheme-less URL, or a full URL
// with credentials/query/fragment as a provider's base, then DERIVES a
// protocol-specific endpoint from it: strip endpoint suffixes and unsafe URL
// parts, default a scheme (http for localhost, https otherwise), and
// compute a different effective base per wire protocol (OpenAI
// chat-completions keeps "/v1", Anthropic Messages and Gemini use the root,
// versioned "bypass"/nested-app-path Gemini bases are preserved verbatim).
//
// This repository's config.Provider deliberately does none of that. Per
// config.go's own field doc and proxy.go's package doc:
//
//	// APIBaseURL is the FULL chat-completions URL, not a base to append to.
//	...
//	// p.APIBaseURL is used VERBATIM as the request URL. ... appending any
//	// suffix here would double up the path for every configured provider
//	// and break them all identically, so this function must never do that.
//
// i.e. the Go router requires the operator (in practice, claude_toolkit's
// provider-alias generator) to supply the complete literal endpoint per
// provider, and intentionally has no URL-derivation layer at all — there is
// also no protocol/type field on config.Provider to derive a base FOR (see
// provider_protocol_type_port_test.go). Every upstream assertion here is
// N/A by design, not a gap; the tests below document our actual, opposite,
// intentional behaviour instead of the ported one, for regression coverage.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation is the direct opposite
// of upstream's "provider URL normalization chooses protocol-specific
// bases" (providerBaseUrlForProtocol yields "https://api.example.com/v1"
// for openai_chat_completions but "https://api.example.com" for
// anthropic_messages from the SAME input). Our config has no protocol
// concept and no derivation step, so the exact string given in
// api_base_url is what gets requested — proven end to end via Load.
func TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation(t *testing.T) {
	body := `{"Providers":[{"name":"p","api_base_url":"https://api.example.com/v1/chat/completions?token=secret#frag","models":["m"]}]}`
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Upstream would strip the query/fragment and derive
	// "https://api.example.com/v1" (or ".../"" for anthropic_messages).
	// We must preserve the string byte-for-byte, credentials/query/fragment
	// included, because it is a complete endpoint, not a base to reduce.
	got := c.Providers[0].APIBaseURL
	want := "https://api.example.com/v1/chat/completions?token=secret#frag"
	if got != want {
		t.Errorf("api_base_url = %q, want unmodified %q", got, want)
	}
}

// TestValidateRejectsSchemeLessAPIBaseURL documents the mirror-image of
// upstream's providerUrlWithDefaultScheme, which ACCEPTS a scheme-less base
// like "api.example.com/v1" and defaults it to "https://api.example.com/v1"
// (or "http://" for a bare "127.0.0.1:3456/v1"/"localhost..." host).
// config.Validate() takes the opposite, stricter position: a scheme-less
// api_base_url is a configuration error, because there is no derivation
// layer downstream to paper over an ambiguous scheme — see
// TestValidateRejectsBadConfigs's "non-http scheme" case for the sibling
// assertion (a wrong-but-present scheme, e.g. "ftp://", is likewise
// rejected).
func TestValidateRejectsSchemeLessAPIBaseURL(t *testing.T) {
	body := `{"Providers":[{"name":"p","api_base_url":"api.example.com/v1/chat/completions"}]}`
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected an error for a scheme-less api_base_url; upstream Node CCR " +
			"would instead default it to https:// — this repository rejects it instead")
	}
}
