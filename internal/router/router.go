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
// operator's existing config.json continues to route the same way:
//
//  1. If req's model is literally "haiku" or contains "haiku" (Claude Code's
//     cheap/background tier — ids like "claude-3-5-haiku-20241022" wrap the
//     tier name rather than equalling it, hence a substring match), prefer
//     cfg.Router.Background when it is set.
//  2. Otherwise, and whenever Background is unset, use cfg.Router.Default.
//  3. If the resulting route string is empty (operator configured providers
//     but never wrote a Router block), fall back to the first provider and
//     the first model in its Models list, so a minimal single-provider
//     config still works.
//
// Every failure to resolve a concrete provider is returned as an error
// rather than silently picking something arbitrary — routing a request to
// the wrong account/model is a billing and correctness hazard, not just a
// cosmetic one.
func Select(cfg *config.Config, req *translate.AnthropicRequest) (*config.Provider, string, error) {
	if cfg == nil {
		return nil, "", fmt.Errorf("router: nil config")
	}

	route := cfg.Router.Default
	if req != nil && isHaikuTier(req.Model) && cfg.Router.Background != "" {
		route = cfg.Router.Background
	}

	if route == "" {
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
