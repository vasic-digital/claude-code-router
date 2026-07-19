package router

// Ports a slice of two upstream Node CCR suites that both assume a client
// can steer routing per-request, not just via server-side config:
//
//   - test/unit/gateway/routing-architecture.test.mjs
//     ("model registry canonicalizes provider models and rejects ambiguous
//     bare models" — ModelRegistry.resolve("Primary/alpha") and
//     resolve("alpha") both canonicalize to "Primary/alpha", while a model
//     name that exists under more than one provider ("shared") is
//     correctly refused rather than guessed)
//   - test/unit/gateway/router-builtins.test.mjs
//     ("explicit provider selectors without capability routing strip
//     provider prefix upstream" — a request body's `model` field of
//     "NebulaCoder/nebulacoder-cot-v8.0" is routed to provider
//     "NebulaCoder" with the outbound model rewritten to
//     "nebulacoder-cot-v8.0"; "model-chain fallback model selectors must
//     not keep stale target provider headers" — an x-target-provider header
//     naming one provider is overridden when the body's own selector names
//     a different one)
//
// Node CCR's client-visible model id IS the routing signal: whatever the
// request body's `model` field says (a bare model, a "Provider/model"
// selector, or a legacy "Provider,model" selector) resolves to a concrete
// provider via ModelRegistry, with an explicit body selector taking
// precedence over any stale x-target-provider header.
//
// GAP CLOSED (TestSelectIgnoresExplicitProviderModelSelector_GAP below,
// renamed to TestSelectHonoursExplicitProviderModelSelector): router.Select
// now detects an explicit "Provider/model" or "Provider,model" selector in
// req.Model (see selector.go's resolveExplicitSelector) and routes to
// exactly the named provider/model, taking precedence over
// Router.Default/Background — matching upstream's "explicit body selector
// wins" behaviour cited above. An unknown provider name or a model the
// named provider does not list is a named error, per router-builtins'
// expectation that a bad explicit selector fails loudly rather than
// silently falling back to Default (which would route to an upstream the
// caller never asked for).
//
// GAP CLOSED SAFELY (was TestModelRegistryAmbiguousBareModelRejection_GAP;
// now the real tests TestSelectResolvesUnambiguousBareModelWhenNoDefault,
// TestSelectRejectsAmbiguousBareModelWhenNoDefault,
// TestSelectDefaultWinsOverBareModelResolution and
// TestSelectExplicitSelectorDisambiguatesAndWins below): upstream's
// ModelRegistry.resolve treats EVERY request's bare (non-prefixed) model id
// as a lookup key across ALL configured providers, erroring when more than
// one provider lists it. A FAITHFUL port would search every provider's
// Models list on every request and, for an unambiguous match, route directly
// to that provider — bypassing Router.Default even when the operator set one.
// That would change "default routing" for ordinary Claude Code requests and
// is exactly the silent-Default-bypass this port must not introduce.
//
// So the ambiguity rule is ported as a SUBORDINATE resolution path instead of
// a supreme one (see selector.go's resolveBareModel, wired into router.Select
// rule 3): bare-model lookup runs ONLY in the no-route window — when neither
// Router.Default nor (for haiku) Router.Background is set, i.e. exactly where
// Select previously did a blind first-provider guess. There it (a) resolves a
// bare model that EXACTLY ONE provider lists to that provider, and (b) refuses
// an ambiguous bare model with a loud named error rather than guessing —
// upstream's core safety property. A configured Router.Default always wins
// over both, so "default routing" for real Claude Code traffic is unchanged
// and no request can silently bypass an explicit Default. The unambiguous
// pass is a strict improvement on the old blind first-model fallback for the
// no-route case (it now honours the provider that actually serves the model);
// the terminal first-provider fallback is preserved for the no-match case.
// (upstream: test/unit/gateway/routing-architecture.test.mjs)

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// explicitSelectorCfg is the two-provider fixture the GAP test shipped
// with: "Primary" and "Secondary" each serve a "shared" model plus one
// model unique to them, with Router.Default pinned to Primary,alpha so a
// test can prove an explicit selector overrides it.
func explicitSelectorCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "Primary", APIBaseURL: "https://primary/x", Models: []string{"shared", "alpha"}},
			{Name: "Secondary", APIBaseURL: "https://secondary/x", Models: []string{"shared", "beta"}},
		},
		Router: config.Route{Default: "Primary,alpha"},
	}
}

// TestSelectHonoursExplicitProviderModelSelector proves the concrete,
// observable fix for the divergence TestSelectIgnoresExplicitProviderModelSelector_GAP
// used to document: a request naming a provider/model pair that is NOT
// cfg.Router.Default is now routed to exactly that pair, honouring the
// client's explicit choice instead of silently overriding it with Default.
func TestSelectHonoursExplicitProviderModelSelector(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantProv string
		wantMod  string
	}{
		// The slash form, the shape Node CCR's ModelRegistry.resolve
		// canonicalizes to and the form the GAP test itself used.
		{"slash selector picks non-default provider", "Secondary/beta", "Secondary", "beta"},
		// The comma form, matching the on-disk Router.Default/Background
		// route syntax (config.SplitRoute) — a caller can copy a route
		// string verbatim into the model field.
		{"comma selector picks non-default provider", "Secondary,beta", "Secondary", "beta"},
		// An explicit selector for the SAME provider/model Default already
		// names must still resolve via the explicit path (not merely
		// "happen to agree with Default"): this is what proves precedence
		// rather than coincidence, exercised properly by the "different
		// provider" cases above already changing the outcome from Default.
		{"explicit selector matching Default resolves normally", "Primary/alpha", "Primary", "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := explicitSelectorCfg()
			req := &translate.AnthropicRequest{Model: tc.model}

			got, gotModel, err := Select(cfg, req)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if got.Name != tc.wantProv || gotModel != tc.wantMod {
				t.Fatalf("Select(%q) = (%q,%q), want (%q,%q)", tc.model, got.Name, gotModel, tc.wantProv, tc.wantMod)
			}
		})
	}
}

// TestSelectExplicitSelectorOverridesHaikuTier proves precedence is total:
// an explicit selector wins even over the haiku->Background heuristic,
// because a caller that named a provider by hand made a strictly more
// specific choice than any tier-based default could.
func TestSelectExplicitSelectorOverridesHaikuTier(t *testing.T) {
	cfg := explicitSelectorCfg()
	cfg.Router.Background = "Primary,alpha"
	req := &translate.AnthropicRequest{Model: "Secondary/beta-haiku"}
	cfg.Providers[1].Models = append(cfg.Providers[1].Models, "beta-haiku")

	got, gotModel, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name != "Secondary" || gotModel != "beta-haiku" {
		t.Fatalf("Select = (%q,%q), want (Secondary,beta-haiku); explicit selector must win over the haiku heuristic", got.Name, gotModel)
	}
}

// TestSelectExplicitSelectorErrors table-drives the two named-error paths
// task item 1 requires: an unknown provider, and a provider that exists but
// does not serve the requested model. Both must fail loudly rather than
// silently falling back to Router.Default.
func TestSelectExplicitSelectorErrors(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		wantErrText string
	}{
		{"unknown provider, slash form", "Ghost/whatever", "Ghost"},
		{"unknown provider, comma form", "Ghost,whatever", "Ghost"},
		{"known provider, unlisted model", "Secondary/not-a-real-model", "not-a-real-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := explicitSelectorCfg()
			req := &translate.AnthropicRequest{Model: tc.model}

			_, _, err := Select(cfg, req)
			if err == nil {
				t.Fatalf("Select(%q): expected an error", tc.model)
			}
			if !strings.Contains(err.Error(), tc.wantErrText) {
				t.Errorf("error should mention %q, got: %v", tc.wantErrText, err)
			}
		})
	}
}

// TestSelectBareModelStillUsesDefaultRouting is the backward-compatibility
// half of this GAP fix: a request whose model contains neither "/" nor ","
// — i.e. every real Claude Code request today — must be completely
// unaffected by the new explicit-selector code path and continue resolving
// via Default/Background exactly as router_test.go's pre-existing suite
// already proves in more detail. This test exists specifically alongside
// the new selector tests so the "must not change" requirement has explicit,
// local coverage rather than relying on distant assertions.
func TestSelectBareModelStillUsesDefaultRouting(t *testing.T) {
	cfg := explicitSelectorCfg()
	req := &translate.AnthropicRequest{Model: "claude-3-7-sonnet-20250219"}

	got, gotModel, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Name != "Primary" || gotModel != "alpha" {
		t.Fatalf("Select = (%q,%q), want (Primary,alpha) via Router.Default", got.Name, gotModel)
	}
}

// bareModelCfg is the fixture for bare-model resolution: two providers that
// share a common "shared" model and each own one unique model, with NO
// Router.Default. The absence of a Default is the whole point — bare-model
// resolution runs only in that no-route window (see resolveBareModel), so
// these tests exercise the resolution path itself; the sibling
// TestSelectDefaultWinsOverBareModelResolution proves a set Default preempts
// it. Primary's first model is "alpha" so the terminal first-provider
// fallback (first provider, first model) is unambiguous in assertions.
func bareModelCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "Primary", APIBaseURL: "https://primary/x", Models: []string{"alpha", "shared"}},
			{Name: "Secondary", APIBaseURL: "https://secondary/x", Models: []string{"beta", "shared"}},
		},
		// Router intentionally left zero-valued (no Default, no Background).
	}
}

// TestSelectResolvesUnambiguousBareModelWhenNoDefault ports the upstream
// "canonicalizes a bare model to its single owning provider" assertion,
// scoped to the no-route window: with no Router.Default configured, a bare
// model that exactly one provider serves resolves to that provider, and a
// bare model no provider serves falls through to the pre-existing
// first-provider fallback unchanged.
func TestSelectResolvesUnambiguousBareModelWhenNoDefault(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		wantProv string
		wantMod  string
	}{
		{"bare model unique to Primary resolves to Primary", "alpha", "Primary", "alpha"},
		{"bare model unique to Secondary resolves to Secondary", "beta", "Secondary", "beta"},
		// No provider lists this ordinary Claude Code id: the terminal
		// first-provider fallback (first provider, first model) still applies,
		// proving bare resolution is additive, not a replacement.
		{"unmatched bare model falls back to first provider+model", "claude-3-7-sonnet-20250219", "Primary", "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bareModelCfg()
			got, gotModel, err := Select(cfg, &translate.AnthropicRequest{Model: tc.model})
			if err != nil {
				t.Fatalf("Select(%q): %v", tc.model, err)
			}
			if got.Name != tc.wantProv || gotModel != tc.wantMod {
				t.Fatalf("Select(%q) = (%q,%q), want (%q,%q)", tc.model, got.Name, gotModel, tc.wantProv, tc.wantMod)
			}
		})
	}
}

// TestSelectRejectsAmbiguousBareModelWhenNoDefault is the direct port of
// upstream's core safety assertion: a bare model present under more than one
// provider must be REFUSED, not silently resolved to whichever provider is
// listed first. Here "shared" is served by both providers and no Default is
// set, so Select must return a loud, named ambiguity error — the exact
// "silent arbitrary pick" this whole gap exists to forbid.
func TestSelectRejectsAmbiguousBareModelWhenNoDefault(t *testing.T) {
	cfg := bareModelCfg()
	got, gotModel, err := Select(cfg, &translate.AnthropicRequest{Model: "shared"})
	if err == nil {
		t.Fatalf("Select(%q) = (%q,%q), want an ambiguity error; a silent arbitrary pick is exactly what must not happen",
			"shared", got.Name, gotModel)
	}
	for _, want := range []string{"Primary", "Secondary", "shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error should mention %q, got: %v", want, err)
		}
	}
}

// TestSelectDefaultWinsOverBareModelResolution is the load-bearing safety
// test for the way this gap was closed: bare-model resolution must NEVER
// override an explicitly-configured Router.Default. With Default pinned to
// "Primary,alpha", every bare model — ambiguous, unambiguous, or unmatched —
// must resolve to Default, never erroring and never picking a provider bare
// resolution alone would have chosen.
func TestSelectDefaultWinsOverBareModelResolution(t *testing.T) {
	cases := []struct {
		name  string
		model string
	}{
		// "shared" is ambiguous across providers; WITHOUT a Default it errors
		// (previous test). WITH a Default it must resolve to Default instead —
		// "fall through to Default", never an arbitrary pick, never an error.
		{"ambiguous bare model yields to Default", "shared"},
		// "beta" is UNAMBIGUOUS (only Secondary lists it); bare resolution
		// alone would route to Secondary. Default must still win — the
		// strongest proof bare resolution never bypasses a configured Default.
		{"unambiguous bare model still yields to Default", "beta"},
		// An ordinary id no provider lists: Default wins as it always has.
		{"unmatched bare model yields to Default", "claude-3-7-sonnet-20250219"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := explicitSelectorCfg() // Router.Default = "Primary,alpha"
			got, gotModel, err := Select(cfg, &translate.AnthropicRequest{Model: tc.model})
			if err != nil {
				t.Fatalf("Select(%q): %v", tc.model, err)
			}
			if got.Name != "Primary" || gotModel != "alpha" {
				t.Fatalf("Select(%q) = (%q,%q), want (Primary,alpha) via Router.Default; Default must win over bare-model resolution",
					tc.model, got.Name, gotModel)
			}
		})
	}
}

// TestSelectExplicitSelectorDisambiguatesAndWins proves the explicit
// "provider/model" / "provider,model" selector remains the top-precedence
// path: it disambiguates a model that a BARE lookup would reject as
// ambiguous, and it still wins over a configured Router.Default.
func TestSelectExplicitSelectorDisambiguatesAndWins(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *config.Config
		model    string
		wantProv string
		wantMod  string
	}{
		// A bare "shared" is an ambiguity error (no Default); an explicit
		// selector for the same model pins one provider deterministically.
		{"slash selector disambiguates shared (no Default)", bareModelCfg(), "Secondary/shared", "Secondary", "shared"},
		{"comma selector disambiguates shared (no Default)", bareModelCfg(), "Primary,shared", "Primary", "shared"},
		// An explicit selector still overrides a configured Default (the
		// sibling gap this file already closed, re-pinned here in-table).
		{"explicit selector beats Default", explicitSelectorCfg(), "Secondary/beta", "Secondary", "beta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotModel, err := Select(tc.cfg, &translate.AnthropicRequest{Model: tc.model})
			if err != nil {
				t.Fatalf("Select(%q): %v", tc.model, err)
			}
			if got.Name != tc.wantProv || gotModel != tc.wantMod {
				t.Fatalf("Select(%q) = (%q,%q), want (%q,%q)", tc.model, got.Name, gotModel, tc.wantProv, tc.wantMod)
			}
		})
	}
}
