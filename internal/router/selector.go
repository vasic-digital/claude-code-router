package router

// Explicit provider selectors.
//
// Claude Code's own request bodies never use this syntax (see router.go's
// package comment: the model field is always one of Claude Code's own tier
// ids). This exists for callers — the claude_toolkit provider-alias wrapper,
// a debugging client, a future multi-model chain — that want to bypass the
// server-configured Router.Default/Background routes for a single request
// and pin an exact upstream by name, the way Node CCR's ModelRegistry lets a
// request body's `model` field name "Provider/model" or "Provider,model"
// directly (see explicit_provider_selector_port_test.go for the ported
// upstream expectations this implements).

import (
	"fmt"
	"strings"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// parseExplicitSelector splits a client-supplied model string into a
// provider name and the remaining model id, if it uses explicit-selector
// syntax at all.
//
// Two separators are recognised because two different conventions exist in
// the wild: a comma matches the on-disk "provider,model" route syntax this
// package already parses for cfg.Router.Default/Background (config.SplitRoute),
// so a caller can literally copy a route string into the model field; a
// slash matches Node CCR's own "Provider/model" wire format and common
// "org/model" catalog ids. Whichever separator appears FIRST (leftmost) in
// the string decides the split, so a model id can never be parsed two
// different ways.
//
// ok is false when neither separator is present, or when either half would
// be empty — both signal "this is an ordinary bare model id, not a
// selector", so the caller falls through to normal Default/Background
// routing instead of misreading e.g. a leading/trailing comma as a selector.
func parseExplicitSelector(model string) (provider, rest string, ok bool) {
	ci := strings.Index(model, ",")
	si := strings.Index(model, "/")

	idx := -1
	switch {
	case ci < 0 && si < 0:
		return "", "", false
	case ci < 0:
		idx = si
	case si < 0:
		idx = ci
	case ci < si:
		idx = ci
	default:
		idx = si
	}

	provider = strings.TrimSpace(model[:idx])
	rest = strings.TrimSpace(model[idx+1:])
	if provider == "" || rest == "" {
		return "", "", false
	}
	return provider, rest, true
}

// resolveExplicitSelector interprets model as a client-pinned selector and
// resolves it against cfg.
//
// matched reports whether model used explicit-selector syntax at all (so
// Select knows whether to fall through to Default/Background). When matched
// is true, err is non-nil for exactly the two failure modes the caller asked
// for: the named provider does not exist, or it exists but does not list the
// requested model — both are the caller asking for something that is not
// actually configured, which must fail loudly rather than silently
// redirecting to whatever Default happens to be (that would send a request
// to an upstream/account the caller never chose, a billing and correctness
// hazard identical to the one router.Select's doc comment already calls out
// for the non-explicit path).
func resolveExplicitSelector(cfg *config.Config, model string) (p *config.Provider, resolvedModel string, matched bool, err error) {
	providerName, resolvedModel, matched := parseExplicitSelector(model)
	if !matched {
		return nil, "", false, nil
	}

	p = cfg.ProviderByName(providerName)
	if p == nil {
		return nil, "", true, fmt.Errorf("router: explicit selector %q references unknown provider %q", model, providerName)
	}
	if !containsString(p.Models, resolvedModel) {
		return nil, "", true, fmt.Errorf("router: explicit selector %q: provider %q does not serve model %q", model, providerName, resolvedModel)
	}
	return p, resolvedModel, true, nil
}

// containsString reports whether s appears verbatim in list. Extracted
// rather than reusing a generic slices helper so this package stays on
// exactly the stdlib surface already imported elsewhere in it.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
