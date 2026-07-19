// Package router decides which configured upstream provider and model
// should serve a given incoming Anthropic request.
//
// Claude Code does not ask the gateway which upstream to use — it just sends
// a Messages API request with whatever model id its own internal tier
// chooser picked (a "main" id for ordinary turns, a "haiku" id for cheap
// background turns such as summarisation or title generation). The router's
// job is to turn that single signal, plus the operator's Router config, into
// a concrete (provider, model) pair, without ever silently proceeding with
// no upstream at all.
package router

import (
	"fmt"
	"strings"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// Select picks the upstream provider and the model id to send it for req.
//
// Rule order, matching the Node implementation's routing behaviour so an
// operator's existing config.json continues to route the same way. The first
// applicable rule wins:
//
//  0. If req's model is an explicit "provider,model" or "provider/model"
//     selector (see resolveExplicitSelector), that pins the exact upstream
//     the caller asked for and wins over every rule below — including the
//     tier/override heuristics, since a caller that names a provider by hand
//     has already made a more specific choice than any tier-based default.
//  1. LongContext: if the request's estimated prompt size exceeds
//     DefaultLongContextThreshold AND cfg.Router.LongContext is set, route
//     there. Checked before the tier rules because an oversized prompt may
//     not physically fit the background/think/default models (see chooseRoute).
//  2. Background: if req's model is literally "haiku" or contains "haiku"
//     (Claude Code's cheap/background tier — ids like
//     "claude-3-5-haiku-20241022" wrap the tier name rather than equalling it,
//     hence a substring match), prefer cfg.Router.Background when it is set.
//  3. Think: if the request asked for extended reasoning AND cfg.Router.Think
//     is set, route there. The signal is the client's own `thinking` field
//     (see requestWantsThinking) — it fires when a request carries one.
//  4. Default: otherwise, and whenever the applicable override is unset, use
//     cfg.Router.Default.
//  5. If the resulting route string is empty (operator configured providers
//     but never wrote a Router block), and req names a bare model that
//     EXACTLY ONE provider lists, route to that provider (resolveBareModel).
//     This is a last-resort resolution that runs ONLY in the no-route window,
//     so a configured Router.Default always wins over it; a bare model served
//     by two or more providers is a loud ambiguity error, never a silent
//     arbitrary pick.
//  6. If still unresolved (no route and no unambiguous bare match), fall back
//     to the first provider and the first model in its Models list, so a
//     minimal single-provider config still works.
//
// Regression guarantee: when neither Router.Think nor Router.LongContext is
// configured, rules 1 and 3 can never fire, so Select behaves byte-identically
// to its previous haiku->Background-else-Default form.
//
// Every failure to resolve a concrete provider is returned as an error
// rather than silently picking something arbitrary — routing a request to
// the wrong account/model is a billing and correctness hazard, not just a
// cosmetic one.
func Select(cfg *config.Config, req *translate.AnthropicRequest) (*config.Provider, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("router: nil config")
	}

	if req != nil {
		if p, model, matched, err := resolveExplicitSelector(cfg, req.Model); matched {
			// matched is true whenever req.Model USED explicit-selector
			// syntax at all, whether or not it resolved cleanly — a
			// malformed or unknown selector must fail loudly rather than
			// silently falling through to Default, which would route the
			// request to an upstream the caller never asked for.
			return p, model, err
		}
	}

	route := chooseRoute(cfg.Router, routeSignalsFor(req))

	if route == "" {
		// No route configured at all. Before the blind first-provider
		// fallback, try to honour a bare model that unambiguously names a
		// single provider. This never overrides a configured route (we only
		// reach here when chooseRoute found nothing applicable); an ambiguous
		// bare model fails loudly rather than being guessed.
		if req != nil {
			if p, matched, err := resolveBareModel(cfg, req.Model); err != nil {
				return nil, "", err
			} else if matched {
				return p, req.Model, nil
			}
		}
		return firstProviderFallback(cfg)
	}

	name, model, err := config.SplitRoute(route)
	if err != nil {
		return nil, "", fmt.Errorf("router: %w", err)
	}
	p := cfg.ProviderByName(name)
	if p == nil {
		return nil, "", fmt.Errorf("router: route %q references unknown provider %q", route, name)
	}
	return p, model, nil
}

// routeSignals are the request-derived facts the route-override precedence
// depends on. They are extracted once, up front, from the incoming request so
// that chooseRoute — where the precedence actually lives — can be exercised in
// isolation without constructing whole request bodies, and so the thinking
// signal has a single, well-documented seam rather than being smeared through
// the selection logic.
type routeSignals struct {
	// haiku marks Claude Code's cheap/background tier (see isHaikuTier).
	haiku bool
	// thinking marks a request that asked for extended reasoning. It is read
	// from the client's own `thinking` field (see requestWantsThinking) and
	// fires when a request carries one.
	thinking bool
	// longContext marks a request whose estimated prompt token footprint
	// exceeds DefaultLongContextThreshold (see estimateTokenCount).
	longContext bool
}

// routeSignalsFor extracts the routing signals from req. A nil req yields the
// zero value (no tier, no thinking, not long) so chooseRoute cleanly resolves
// to Router.Default — the same result the old nil-request path produced.
func routeSignalsFor(req *translate.AnthropicRequest) routeSignals {
	if req == nil {
		return routeSignals{}
	}
	return routeSignals{
		haiku:       isHaikuTier(req.Model),
		thinking:    requestWantsThinking(req),
		longContext: estimateTokenCount(req) > DefaultLongContextThreshold,
	}
}

// chooseRoute applies the route-override precedence to the extracted signals
// and returns the configured route string to use, or "" when none of the
// configured routes apply (Select then falls through to bare-model resolution
// and finally the first-provider fallback).
//
// Precedence, highest first (the explicit "provider,model" selector is handled
// earlier, in Select, and outranks everything here):
//
//  1. LongContext — request estimated to exceed DefaultLongContextThreshold AND
//     Router.LongContext configured. Checked first, matching the Node
//     implementation: an oversized request must go to the wide-context model
//     even when it is also a background/thinking turn, because the other models
//     may not physically fit the prompt.
//  2. Background — haiku-tier request AND Router.Background configured. The
//     pre-existing cheap/background-tier rule, unchanged and kept ahead of
//     Think so a haiku turn still lands on Background exactly as before even
//     when Think is also configured.
//  3. Think — thinking request AND Router.Think configured.
//  4. Default — Router.Default, the base route for every ordinary request.
//
// When neither Router.Think nor Router.LongContext is configured, branches 1
// and 3 can never be taken, so the result is byte-identical to the previous
// haiku->Background-else-Default behaviour (the regression guard).
func chooseRoute(r config.Route, s routeSignals) string {
	if s.longContext && r.LongContext != "" {
		return r.LongContext
	}
	if s.haiku && r.Background != "" {
		return r.Background
	}
	if s.thinking && r.Think != "" {
		return r.Think
	}
	return r.Default
}

// isHaikuTier reports whether model belongs to Claude Code's haiku
// (background/cheap) tier. Substring rather than equality because real
// upstream and Anthropic-native ids embed the tier name inside a longer,
// dated id rather than using it bare.
func isHaikuTier(model string) bool {
	return strings.Contains(strings.ToLower(model), "haiku")
}

// firstProviderFallback is used when no route string is configured at all.
// It returns a clear, specific error rather than a generic "not routable"
// message, since the two failure modes (no providers vs. a provider with an
// empty Models list) call for different operator fixes.
func firstProviderFallback(cfg *config.Config) (*config.Provider, string, error) {
	if len(cfg.Providers) == 0 {
		return nil, "", fmt.Errorf("router: no route configured and no providers available")
	}
	p := &cfg.Providers[0]
	if len(p.Models) == 0 {
		return nil, "", fmt.Errorf("router: no route configured and fallback provider %q has no models", p.Name)
	}
	return p, p.Models[0], nil
}

// TransformerOptions maps a provider's configured transformer names
// (config.Provider.Has) onto the translate.Options fields that actually
// drive conversion behaviour, so gateway code needs to know neither the
// transformer name strings nor which Options field each one corresponds to.
func TransformerOptions(p *config.Provider) translate.Options {
	return translate.Options{
		CleanCache:    p.Has("cleancache"),
		StreamOptions: p.Has("streamoptions"),
	}
}
