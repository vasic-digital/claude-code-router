package config

// Closes a gap surfaced by two upstream suites that both assume a provider
// carries an explicit wire-protocol/type:
//
//   - test/unit/providers/provider-url.test.mjs
//     (providerBaseUrlForProtocol derives a different base per protocol)
//   - test/unit/gateway/gateway-claude-code-oauth.test.mjs
//     (provider records carry `type: "anthropic_messages"`; anthropic-beta
//     header merging is applied only to providers of that type)
//
// Both rely on a provider record knowing which protocol its upstream speaks.
// config.Provider now carries that: an optional `protocol` field ("openai" |
// "anthropic"), a conservative inference from api_base_url when it is absent,
// and validation that rejects anything else. internal/translate exposes an
// Anthropic-native passthrough for the "anthropic" case, and the gateway routes
// to it — so a provider whose api_base_url is a real Anthropic Messages
// endpoint can now be proxied correctly instead of being forced through the
// OpenAI translation that a native endpoint rejects.
//
// GAP CLOSED. This file previously held a single t.Skip asserting the field did
// not exist; that is no longer true, so the skip is replaced by real tests. A
// skip asserting a closed gap is a false record of coverage.

import "testing"

// The canonical anthropic-native URL from the original gap description. It must
// resolve to the anthropic protocol WITHOUT an explicit field — otherwise the
// most obvious "point a provider at the real Anthropic API" case would still be
// silently mistranslated.
const anthropicNativeURL = "https://api.anthropic.com/v1/messages"

func TestResolvedProtocol(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		want     string
	}{
		{
			// The regression guard that matters most: an ABSENT protocol on an
			// ordinary OpenAI-shaped provider must behave exactly as before this
			// field existed. Every config on disk today omits the field.
			name:     "absent field on openai-shaped url defaults to openai",
			provider: Provider{Name: "p", APIBaseURL: "https://api.deepseek.com/chat/completions"},
			want:     ProtocolOpenAI,
		},
		{
			name:     "explicit anthropic wins",
			provider: Provider{Name: "p", APIBaseURL: "https://api.deepseek.com/chat/completions", Protocol: ProtocolAnthropic},
			want:     ProtocolAnthropic,
		},
		{
			name:     "explicit openai wins even over an anthropic-looking url",
			provider: Provider{Name: "p", APIBaseURL: anthropicNativeURL, Protocol: ProtocolOpenAI},
			want:     ProtocolOpenAI,
		},
		{
			name:     "inferred anthropic from api.anthropic.com host",
			provider: Provider{Name: "p", APIBaseURL: anthropicNativeURL},
			want:     ProtocolAnthropic,
		},
		{
			name:     "inferred anthropic from a subdomain of anthropic.com",
			provider: Provider{Name: "p", APIBaseURL: "https://api.eu.anthropic.com/v1/messages"},
			want:     ProtocolAnthropic,
		},
		{
			name:     "inferred anthropic from an /anthropic path segment (toolkit proxy convention)",
			provider: Provider{Name: "p", APIBaseURL: "https://proxy.example.com/anthropic/v1/messages"},
			want:     ProtocolAnthropic,
		},
		{
			// A host that merely CONTAINS "anthropic" but is not *.anthropic.com
			// must NOT be reclassified — that would silently break a real
			// OpenAI-shaped provider, the exact failure mode inference must avoid.
			name:     "lookalike host is not misclassified",
			provider: Provider{Name: "p", APIBaseURL: "https://api.anthropic-proxy.example.com/v1/chat/completions"},
			want:     ProtocolOpenAI,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.provider.ResolvedProtocol(); got != tc.want {
				t.Errorf("ResolvedProtocol() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateProtocol(t *testing.T) {
	valid := []string{"", ProtocolOpenAI, ProtocolAnthropic}
	for _, p := range valid {
		c := &Config{Providers: []Provider{{Name: "p", APIBaseURL: "https://x/y", Protocol: p}}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate rejected valid protocol %q: %v", p, err)
		}
	}

	// An unrecognised protocol must be a NAMED error, not a silent fallback.
	c := &Config{Providers: []Provider{{Name: "typo-prov", APIBaseURL: "https://x/y", Protocol: "anthropic "}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid protocol")
	}
	for _, want := range []string{"typo-prov", "anthropic "} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err.Error(), want)
		}
	}
}

// The live config the toolkit writes must keep loading and validating with the
// new field present in the struct — and every one of its providers must resolve
// to openai, since none of them names an anthropic endpoint. This is the
// end-to-end proof that adding the field changed nothing for real deployments.
func TestLiveConfigStillOpenAI(t *testing.T) {
	c, err := Load(Path())
	if err != nil {
		t.Skipf("no loadable live config (%v) — skipping cleanly, this is not a failure", err)
	}
	if len(c.Providers) == 0 {
		t.Skip("live config has no providers")
	}
	for _, p := range c.Providers {
		if got := p.ResolvedProtocol(); got != ProtocolOpenAI {
			t.Errorf("live provider %q resolved to %q, want openai (adding the field must not reclassify a real provider)", p.Name, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
