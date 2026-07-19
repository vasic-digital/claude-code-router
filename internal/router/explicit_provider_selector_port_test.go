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
// GAP REMAINS OPEN (TestModelRegistryAmbiguousBareModelRejection_GAP
// below): upstream's ModelRegistry.resolve treats EVERY request's bare
// (non-prefixed) model id as a lookup key across ALL configured providers,
// erroring when more than one provider lists it. Porting that would mean
// router.Select no longer only ever resolving a bare, non-haiku model via
// Router.Default — it would first have to search every provider's Models
// list on every request, and (for the common case where exactly one
// provider happens to list a match) route directly to that provider,
// bypassing Default entirely. That is a strictly larger behaviour change
// than "detect an explicit selector": it changes what "default routing"
// means for ordinary Claude Code requests, which this port's own
// requirement — "existing Select() behaviour (haiku->background, default,
// first-provider fallback) must not change" — rules out. See that test for
// the full reasoning.

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

// TestModelRegistryAmbiguousBareModelRejection_GAP documents the sibling
// upstream assertion that a bare model name present under more than one
// provider must be refused rather than silently resolved to whichever
// provider happens to be listed first.
//
// This GAP remains open. Closing TestSelectIgnoresExplicitProviderModelSelector_GAP
// (see TestSelectHonoursExplicitProviderModelSelector above) only taught
// Select to recognise EXPLICIT "Provider/model" / "Provider,model"
// selectors — a new, additive code path that a bare model id never enters.
// Porting upstream's ambiguity check faithfully would require Select to
// search every provider's Models list for a plain, non-prefixed model on
// EVERY request (not just when an explicit selector is present), and, for
// the unambiguous case, route directly to whichever single provider
// matched — bypassing Router.Default even when the caller never asked for
// that. That would change "default" routing for ordinary Claude Code
// requests (whose model ids sometimes coincide with a configured Models
// entry purely by chance), which conflicts directly with this task's
// explicit backward-compatibility requirement: "existing Select() behaviour
// (haiku->background, default, first-provider fallback) must not change."
// Implementing it correctly and safely — e.g. behind an opt-in config flag
// — is a config.Config surface change outside internal/router/, which this
// task is scoped not to touch. (upstream: test/unit/gateway/routing-architecture.test.mjs)
func TestModelRegistryAmbiguousBareModelRejection_GAP(t *testing.T) {
	t.Skip("GAP: closing this would require router.Select to search every " +
		"provider's Models list for a bare (non-prefixed) model on EVERY " +
		"request and route directly to an unambiguous single match, bypassing " +
		"Router.Default — a change to \"default\" routing behaviour this task's " +
		"backward-compatibility requirement explicitly rules out. The additive " +
		"explicit-selector path (TestSelectHonoursExplicitProviderModelSelector) " +
		"does not touch bare models at all, so this remains genuinely unresolved " +
		"within internal/router/'s existing Select surface. (upstream: " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}
