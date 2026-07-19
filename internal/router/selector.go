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
	"github.com/vasic-digital/claude-code-router/internal/translate"
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

// DefaultLongContextThreshold is the estimated prompt token count above which a
// request is treated as "long context" and routed to Router.LongContext (when
// configured — see chooseRoute). It matches the Node implementation's default
// trigger of 60000 tokens (1000*60). The Node router exposes this as a
// configurable Router.longContextThreshold; config.Route here carries no such
// field, so the threshold is a fixed constant — change it here if a different
// cutoff is wanted.
const DefaultLongContextThreshold = 60000

// charsPerToken is the coarse characters-per-token divisor used to turn a byte
// length into an approximate token count. Real BPE tokenisers (tiktoken and the
// like) average close to 4 characters per token across English prose and code,
// which is all the precision a "is this prompt huge?" gate needs. This is
// deliberately an estimate rather than a real tokeniser call, so routing stays a
// pure, dependency-free, fast function of the request.
const charsPerToken = 4

// estimateTokenCount approximates the prompt token footprint of req.
//
// It sums the byte lengths of the raw payloads that actually carry prompt text
// — every message's content, the system block(s), and each tool's name,
// description and input schema — and divides by charsPerToken. The polymorphic
// fields (System and message Content are json.RawMessage) are measured by their
// raw byte length rather than re-decoded; the small amount of structural JSON
// punctuation that adds only biases the long-context gate very slightly more
// eager, never less, which is the safe direction. max_tokens (the requested
// OUTPUT budget, not prompt input) is intentionally excluded.
//
// This reads only fields the router already receives on translate.AnthropicRequest,
// so — unlike think-routing — long-context routing fires in production today
// with no caller-side change.
func estimateTokenCount(req *translate.AnthropicRequest) int {
	if req == nil {
		return 0
	}
	chars := len(req.System)
	for _, m := range req.Messages {
		chars += len(m.Content)
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return chars / charsPerToken
}

// requestWantsThinking reports whether req asked for extended reasoning
// ("thinking"), which — when Router.Think is configured — routes the request to
// the think model (see chooseRoute).
//
// HONEST LIMITATION: the typed translate.AnthropicRequest this router receives
// does NOT model Anthropic's `thinking` request field (see its definition in
// internal/translate/anthropic.go — Model, MaxTokens, Messages, System, Tools,
// Temperature, TopP, StopSequences, Stream, and nothing else). There is
// therefore nothing here to inspect: this returns false for every request the
// gateway builds today, so think-routing is inert in production.
//
// Making it fire requires a caller-side change OUTSIDE this package's ownership:
// add a
//
//	Thinking json.RawMessage `json:"thinking,omitempty"`
//
// field to translate.AnthropicRequest so the incoming `thinking` block survives
// decoding, then have this return true when it is present and non-null (e.g.
// len(req.Thinking) > 0 && !bytes.Equal(req.Thinking, []byte("null"))). No proxy
// signal (max_tokens, stop sequences, …) is substituted, because inferring
// "thinking" from an unrelated field would misroute ordinary requests — exactly
// the kind of silent misrouting the rest of this package is careful to avoid.
//
// The routing LOGIC that consumes this signal is real and is pinned directly by
// unit tests that set routeSignals.thinking (see the chooseRoute tests).
func requestWantsThinking(req *translate.AnthropicRequest) bool {
	return false
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

// resolveBareModel searches every configured provider for one whose Models
// list contains model verbatim. It is a LAST-RESORT resolution path: Select
// calls it only when no Router route applies at all (neither Router.Default
// nor, for a haiku request, Router.Background is set — see Select), so it can
// never override an explicitly-configured route. Within that no-route window
// it deliberately refuses to guess, porting the safe half of upstream's
// ModelRegistry.resolve ambiguity rule (see
// explicit_provider_selector_port_test.go):
//
//   - exactly one provider lists model  -> matched=true, that provider, no error.
//     A bare model id served by a single provider resolves to it instead of the
//     blind first-provider guess firstProviderFallback would otherwise make.
//   - two or more providers list model   -> matched=true, a named ambiguity
//     error. An ambiguous bare id must fail loudly, never resolve to whichever
//     provider happens to be listed first — the exact "guessing" upstream
//     forbids, and the billing/correctness hazard router.Select's doc comment
//     already warns about for the wrong-account case.
//   - no provider lists model            -> matched=false, no error, and Select
//     falls through to its existing first-provider fallback unchanged.
//
// Crucially this runs only in the no-route branch, so a configured
// Router.Default always wins over it: an ambiguous OR unambiguous bare model
// under a set Default is resolved by Default and never reaches this function.
func resolveBareModel(cfg *config.Config, model string) (p *config.Provider, matched bool, err error) {
	if model == "" {
		return nil, false, nil
	}

	var found []*config.Provider
	for i := range cfg.Providers {
		if containsString(cfg.Providers[i].Models, model) {
			found = append(found, &cfg.Providers[i])
		}
	}

	switch len(found) {
	case 0:
		return nil, false, nil
	case 1:
		return found[0], true, nil
	default:
		names := make([]string, 0, len(found))
		for _, pr := range found {
			names = append(names, pr.Name)
		}
		return nil, true, fmt.Errorf(
			"router: bare model %q is ambiguous: served by %d providers (%s); "+
				"pin one with an explicit \"provider/model\" selector or configure Router.Default",
			model, len(found), strings.Join(names, ", "))
	}
}
