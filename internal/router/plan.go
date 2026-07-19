package router

// Cross-provider execution planning.
//
// router.Select answers "which single (provider, model) should serve THIS
// request" (see router.go). The fallback primitives in fallback.go answer, in
// isolation, "given an ordered de-duplicated list of selector strings, build a
// plan (BuildExecutionPlan) and, after one entry fails, which entry comes next
// (NextFallbackProvider)". What was missing — and what this file adds — is the
// glue that turns Select's concrete primary (*config.Provider, model) into that
// ordered selector plan, discovering the CROSS-PROVIDER fallbacks the config
// already implies: a single model id served by more than one configured
// provider (e.g. "gpt-4o" listed by both an "openai" and an "azure" provider).
//
// This is deliberately ADDITIVE. Select's signature and behaviour are
// unchanged; nothing here is wired into the gateway yet. See the "Gateway
// seam" section on BuildProviderPlan for exactly how a future
// doUpstreamWithRetry would consume it.

import (
	"fmt"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// providerSelector renders a (provider, model) pair as the canonical
// "provider,model" selector string the rest of this package speaks —
// the same on-disk form config.SplitRoute parses and
// parseExplicitSelector accepts. Emitting one canonical form for every
// plan entry is what makes BuildExecutionPlan's string-keyed de-duplication
// correct: two entries naming the same (provider, model) collapse only if
// they serialise identically, so every producer here must agree on the
// separator. Comma (not slash) is chosen because a model id may itself
// contain a slash (catalog ids like "anthropic/claude-3"), and
// parseExplicitSelector splits on the LEFTMOST separator — with a comma the
// provider/model boundary is unambiguous even then.
func providerSelector(providerName, model string) string {
	return providerName + "," + model
}

// BuildProviderPlan produces the ordered, de-duplicated cross-provider
// fallback plan for a request whose PRIMARY (provider, model) has already been
// chosen by Select.
//
// The plan, in order, is:
//
//  1. the primary attempt — always index 0, always first, byte-for-byte the
//     (provider, model) Select returned;
//  2. each entry of explicitFallbacks, in the given order — the seam through
//     which a future config-expressed fallback chain feeds in (the current
//     config.Route has no such field, so callers pass nil today; see the note
//     below). Entries that already use "provider,model"/"provider/model"
//     selector syntax are normalised to the canonical comma form so they
//     de-duplicate against the auto-discovered entries; anything else is kept
//     verbatim so a misconfigured chain still surfaces loudly at
//     NextFallbackProvider time rather than being silently dropped here;
//  3. every OTHER configured provider whose Models list also contains
//     primaryModel — the "a model served by multiple providers" case —
//     in config declaration order, which is the plan's stable, documented
//     tie-break.
//
// Exact-duplicate (provider, model) attempts are removed by BuildExecutionPlan
// (an explicit fallback that merely repeats the primary or an auto-discovered
// entry collapses to a single attempt), so no upstream is ever double-charged
// within one plan. A single-provider config, or a model only one provider
// serves, yields a one-element plan — consuming it is byte-identical to
// today's single-attempt behaviour.
//
// The return type is the same []Attempt fallback.go already defines, whose
// .Model field is a canonical selector string directly consumable by
// NextFallbackProvider. A nil primary (a caller bug — Select never returns a
// nil provider without an error) yields a nil plan rather than panicking.
//
// # Gateway seam
//
// This is the function a future internal/gateway doUpstreamWithRetry would
// call to gain cross-provider fallback. Today that loop retries a single fixed
// provider (internal/gateway/messages.go, doUpstreamWithRetry(c, ctx,
// provider config.Provider, body)). The wired-up shape, using ONLY this
// package's existing exports, is:
//
//	primary, primaryModel, err := router.Select(cfg, req)      // unchanged
//	plan := router.BuildProviderPlan(cfg, primary, primaryModel, nil)
//	for i := 0; i < maxAttempts && i < len(plan); {
//	    prov, model, err := router.ResolveAttempt(cfg, plan[i]) // concrete (provider, model)
//	    if err != nil { /* misconfigured plan entry: surface it */ }
//	    resp, transportErr := s.Upstream.Do(ctx, *prov, bodyFor(model))
//	    class := router.ClassifyStatus(resp.StatusCode) // or ClassifyTransportError(transportErr)
//	    if resp ok { return resp }
//	    _, _, ok, ferr := router.NextFallbackProvider(cfg, plan, plan[i].Model, class)
//	    if ferr != nil { /* surface */ }
//	    if !ok { break } // Terminal, or plan exhausted: stop
//	    sleep(router.FallbackRetryDelayAfterStatus(i, resp.Header.Get("Retry-After")))
//	    i++
//	}
//
// The gateway needs no new selector plumbing: it reads each failed attempt's
// selector straight off plan[i].Model (a public field) and lets
// NextFallbackProvider gate advancement on the Terminal/Retryable
// classification. Wiring that loop — and re-transforming the request body for
// each new model id — is an internal/gateway change, intentionally left undone
// here.
func BuildProviderPlan(cfg *config.Config, primary *config.Provider, primaryModel string, explicitFallbacks []string) []Attempt {
	if primary == nil {
		return nil
	}

	fallbacks := make([]string, 0, len(explicitFallbacks))

	// (2) Explicit, config-expressed chain first, normalised so it
	// de-duplicates against everything else.
	for _, f := range explicitFallbacks {
		if pn, m, ok := parseExplicitSelector(f); ok {
			fallbacks = append(fallbacks, providerSelector(pn, m))
		} else {
			fallbacks = append(fallbacks, f)
		}
	}

	// (3) Auto-discovered cross-provider fallbacks: any OTHER provider that
	// also serves primaryModel, in config order.
	if cfg != nil {
		for i := range cfg.Providers {
			p := &cfg.Providers[i]
			if p.Name == primary.Name {
				continue
			}
			if containsString(p.Models, primaryModel) {
				fallbacks = append(fallbacks, providerSelector(p.Name, primaryModel))
			}
		}
	}

	return BuildExecutionPlan(providerSelector(primary.Name, primaryModel), fallbacks)
}

// ResolveAttempt maps one plan Attempt back to the concrete configured provider
// and model it names, so the gateway can resolve EVERY attempt (including the
// primary at index 0) through one uniform call rather than special-casing
// Select's output.
//
// It mirrors NextFallbackProvider's resolution contract exactly: the attempt's
// .Model must be a "provider,model"/"provider/model" selector naming a
// configured provider, and both failure modes (not a selector, or an unknown
// provider) are returned as errors rather than silently skipped — a plan entry
// that cannot be resolved is an operator/caller mistake that must surface, not
// be swallowed. Unlike resolveExplicitSelector it does NOT re-verify that the
// provider lists the model: plans built by BuildProviderPlan already guarantee
// that for their auto-discovered entries, and an explicit chain is validated at
// NextFallbackProvider time, so re-checking here would only reject valid plans
// on a technicality.
func ResolveAttempt(cfg *config.Config, a Attempt) (*config.Provider, string, error) {
	providerName, modelName, matched := parseExplicitSelector(a.Model)
	if !matched {
		return nil, "", fmt.Errorf("router: plan attempt %q is not a \"provider,model\" or \"provider/model\" selector", a.Model)
	}
	p := cfg.ProviderByName(providerName)
	if p == nil {
		return nil, "", fmt.Errorf("router: plan attempt %q references unknown provider %q", a.Model, providerName)
	}
	return p, modelName, nil
}
