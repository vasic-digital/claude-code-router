package router

// Fuzz tests for the selector-parsing surface the plan API resolves through.
//
// ResolveAttempt and parseExplicitSelector both consume free-form strings (a
// plan entry's .Model, or a client-supplied model id). The invariant under any
// input — including malformed, empty, separator-only, or adversarial strings —
// is that they never panic (no nil-deref, no out-of-range slice) and always
// report a non-selector or an unknown provider as an ERROR rather than silently
// returning a nil provider a caller would then deref.

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// fuzzResolveCfg is a fixed, valid config the fuzzer resolves attempts against.
func fuzzResolveCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "p-openai", APIBaseURL: "https://openai/v1/chat/completions", Models: []string{"shared-model", "anthropic/claude-3"}},
			{Name: "p-azure", APIBaseURL: "https://azure/v1/chat/completions", Models: []string{"shared-model"}},
		},
	}
}

// FuzzResolveAttempt asserts ResolveAttempt never panics and honours its
// error contract: on success the provider is non-nil, configured, and the
// selector round-trips; on failure the provider is nil.
func FuzzResolveAttempt(f *testing.F) {
	for _, s := range []string{
		"", ",", "/", "a,", ",b", "a/", "/b",
		"p-openai,shared-model", "p-openai/shared-model",
		"p-openai,anthropic/claude-3", "ghost,shared-model",
		"a,b,c", "a//b", "  ,  ", "p-openai , shared-model",
	} {
		f.Add(s)
	}

	cfg := fuzzResolveCfg()
	f.Fuzz(func(t *testing.T, model string) {
		p, m, err := ResolveAttempt(cfg, Attempt{Index: 0, Model: model})
		if err != nil {
			if p != nil {
				t.Fatalf("ResolveAttempt(%q): err=%v but provider=%v (must be nil on error)", model, err, p)
			}
			return
		}
		// Success: provider must be non-nil and actually configured, and the
		// resolved (name, model) must reconstruct the selector.
		if p == nil {
			t.Fatalf("ResolveAttempt(%q): nil error but nil provider", model)
		}
		if cfg.ProviderByName(p.Name) == nil {
			t.Fatalf("ResolveAttempt(%q): resolved to unconfigured provider %q", model, p.Name)
		}
		// The resolved (name, model) must form a canonical selector that
		// re-parses to the same pair (input spacing/slash-form is normalised
		// away, so this is checked against the canonical form, not the raw
		// input).
		canon := providerSelector(p.Name, m)
		if pn, pm, ok := parseExplicitSelector(canon); !ok || pn != p.Name || pm != m {
			t.Fatalf("ResolveAttempt(%q): canonical selector %q does not re-parse to (%q,%q)", model, canon, p.Name, m)
		}
	})
}

// FuzzParseExplicitSelector asserts parseExplicitSelector never panics and that
// a positive match always yields non-empty, space-trimmed halves — the
// precondition ResolveAttempt/NextFallbackProvider rely on to safely index a
// provider name and model out of the string.
func FuzzParseExplicitSelector(f *testing.F) {
	for _, s := range []string{
		"", ",", "/", "a,b", "a/b", "a,", ",b", "a,b,c", "a//b",
		" a , b ", "anthropic/claude-3", "p,anthropic/claude-3",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, model string) {
		provider, rest, ok := parseExplicitSelector(model)
		if !ok {
			if provider != "" || rest != "" {
				t.Fatalf("parseExplicitSelector(%q): ok=false but returned (%q,%q)", model, provider, rest)
			}
			return
		}
		if provider == "" || rest == "" {
			t.Fatalf("parseExplicitSelector(%q): ok=true with an empty half (%q,%q)", model, provider, rest)
		}
		if provider != strings.TrimSpace(provider) || rest != strings.TrimSpace(rest) {
			t.Fatalf("parseExplicitSelector(%q): halves not trimmed (%q,%q)", model, provider, rest)
		}
	})
}
