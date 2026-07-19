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
// router.Select in this repository does not read req.Model for provider
// selection AT ALL, aside from a single substring check ("does the model
// name contain \"haiku\"") used only to choose between two fixed,
// server-configured routes (cfg.Router.Default / cfg.Router.Background —
// see router.go's isHaikuTier). A client cannot address a specific
// provider or a specific non-default/non-background model by name; every
// non-haiku request goes to cfg.Router.Default regardless of what model
// string it names. This is the router package's most consequential
// architectural gap relative to upstream.

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// TestSelectIgnoresExplicitProviderModelSelector_GAP shows the concrete,
// observable divergence: a request naming a provider/model pair that is
// NOT cfg.Router.Default is still routed to cfg.Router.Default, silently
// overriding the client's explicit choice rather than honouring it or
// rejecting it.
func TestSelectIgnoresExplicitProviderModelSelector_GAP(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "Primary", APIBaseURL: "https://primary/x", Models: []string{"shared", "alpha"}},
			{Name: "Secondary", APIBaseURL: "https://secondary/x", Models: []string{"shared", "beta"}},
		},
		Router: config.Route{Default: "Primary,alpha"},
	}
	// The client explicitly asks for Secondary's model, e.g. via a
	// "Secondary/beta" selector embedded in the model field — the shape
	// Node CCR's ModelRegistry.resolve would canonicalize and route on.
	req := &translate.AnthropicRequest{Model: "Secondary/beta"}

	got, gotModel, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Documents CURRENT behaviour (Default wins unconditionally), which is
	// exactly what upstream's routing model does NOT do.
	if got.Name != "Primary" || gotModel != "alpha" {
		t.Fatalf("Select unexpectedly honoured the explicit selector: got (%q,%q)", got.Name, gotModel)
	}
	t.Skip("GAP: router.Select never reads an explicit \"Provider/model\" (or " +
		"\"Provider,model\") selector out of the request body; only cfg.Router.Default / " +
		"Background are ever chosen (haiku substring aside). Upstream's ModelRegistry " +
		"resolves and routes on a client-supplied selector, with an ambiguous bare model " +
		"name (present under multiple providers) explicitly rejected rather than guessed. " +
		"(upstream: test/unit/gateway/routing-architecture.test.mjs, " +
		"test/unit/gateway/router-builtins.test.mjs)")
}

// TestModelRegistryAmbiguousBareModelRejection_GAP documents the sibling
// upstream assertion that a bare model name present under more than one
// provider must be refused rather than silently resolved to whichever
// provider happens to be listed first. router.Select has no code path that
// even attempts bare-model-to-provider resolution (it only ever consults
// cfg.Router.Default/Background/the first provider fallback), so there is
// no ambiguity check to test.
func TestModelRegistryAmbiguousBareModelRejection_GAP(t *testing.T) {
	t.Skip("GAP: router.Select has no bare-model-to-provider resolution at all (see " +
		"TestSelectIgnoresExplicitProviderModelSelector_GAP), so there is no ambiguous-" +
		"model rejection path to port. (upstream: " +
		"test/unit/gateway/routing-architecture.test.mjs)")
}
