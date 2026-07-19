package router

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// planModels flattens a plan to its selector strings, so tests assert on
// ordering/de-duplication with a single readable slice compare.
func planModels(plan []Attempt) []string {
	out := make([]string, len(plan))
	for i, a := range plan {
		out[i] = a.Model
	}
	return out
}

// assertPlan checks both the selector order AND that Index is a stable, gap-free
// 0..n-1 sequence — the contract callers key backoff and logging off.
func assertPlan(t *testing.T, plan []Attempt, want []string) {
	t.Helper()
	if got := planModels(plan); !equalStrings(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for i, a := range plan {
		if a.Index != i {
			t.Errorf("plan[%d].Index = %d, want %d", i, a.Index, i)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// multiProviderCfg: "shared-model" is served by three providers (p-openai,
// p-azure, p-bedrock) in that config order; "solo-model" only by p-openai.
func multiProviderCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "p-openai", APIBaseURL: "https://openai/v1/chat/completions", Models: []string{"shared-model", "solo-model"}},
			{Name: "p-azure", APIBaseURL: "https://azure/v1/chat/completions", Models: []string{"shared-model"}},
			{Name: "p-bedrock", APIBaseURL: "https://bedrock/v1/chat/completions", Models: []string{"shared-model", "other"}},
		},
	}
}

// A single provider (or a model only one provider serves) must yield a
// one-element plan — the byte-identical-to-today's-single-attempt case.
func TestBuildProviderPlanSingleProviderOneElement(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "only", APIBaseURL: "https://only/v1/chat/completions", Models: []string{"m1"}},
		},
	}
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "m1", nil)
	assertPlan(t, plan, []string{"only,m1"})
}

// A model served by only one provider (even though other providers exist)
// still yields a one-element plan: no other provider serves it, so there is
// nothing to fall back to.
func TestBuildProviderPlanModelServedByOneProviderOnly(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "solo-model", nil)
	assertPlan(t, plan, []string{"p-openai,solo-model"})
}

// The headline case: a model served by multiple providers yields the primary
// first, then the OTHER providers serving it, in config order, deduped.
func TestBuildProviderPlanCrossProviderFallbacks(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "shared-model", nil)
	assertPlan(t, plan, []string{
		"p-openai,shared-model",  // primary, always first
		"p-azure,shared-model",   // config order
		"p-bedrock,shared-model", // config order
	})
}

// Primary is always first even when it is NOT the first provider in config
// order — ordering is "primary, then the remaining serving providers in config
// order", not raw config order.
func TestBuildProviderPlanPrimaryIsAlwaysFirst(t *testing.T) {
	cfg := multiProviderCfg()
	// Primary is p-bedrock (the LAST provider in config order).
	plan := BuildProviderPlan(cfg, &cfg.Providers[2], "shared-model", nil)
	assertPlan(t, plan, []string{
		"p-bedrock,shared-model", // primary first despite being config-last
		"p-openai,shared-model",  // remaining, in config order
		"p-azure,shared-model",
	})
}

// An explicit fallback chain (the seam a future config field feeds) is honoured
// in order, placed before the auto-discovered cross-provider entries, and
// de-duplicated against them. Here "p-azure,shared-model" appears BOTH as an
// explicit entry and as an auto-discovered one; it must appear exactly once, at
// its explicit position.
func TestBuildProviderPlanExplicitChainOrderedAndDeduped(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "shared-model",
		[]string{"p-azure,shared-model"})
	assertPlan(t, plan, []string{
		"p-openai,shared-model",  // primary
		"p-azure,shared-model",   // explicit chain, wins its position...
		"p-bedrock,shared-model", // ...so auto-discovery does not re-add p-azure
	})
}

// An explicit fallback written with SLASH selector syntax must normalise to the
// canonical comma form so it de-duplicates against a comma-form auto-discovered
// entry (a cross-syntax duplicate would otherwise slip through and double-charge
// the same upstream).
func TestBuildProviderPlanExplicitChainNormalisesSeparator(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "shared-model",
		[]string{"p-azure/shared-model"}) // slash form of an auto-discovered entry
	assertPlan(t, plan, []string{
		"p-openai,shared-model",
		"p-azure,shared-model", // normalised + deduped, NOT a second p-azure entry
		"p-bedrock,shared-model",
	})
}

// An explicit fallback that repeats the PRIMARY collapses away entirely — the
// primary is already index 0.
func TestBuildProviderPlanExplicitRepeatOfPrimaryCollapses(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "solo-model",
		[]string{"p-openai,solo-model"})
	assertPlan(t, plan, []string{"p-openai,solo-model"})
}

// A model id that itself contains a slash must round-trip: the comma separator
// keeps the provider/model boundary unambiguous, and parseExplicitSelector
// (used by ResolveAttempt/NextFallbackProvider) recovers the full model id.
func TestBuildProviderPlanModelWithSlashRoundTrips(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "prov-a", APIBaseURL: "https://a/v1/chat/completions", Models: []string{"anthropic/claude-3"}},
			{Name: "prov-b", APIBaseURL: "https://b/v1/chat/completions", Models: []string{"anthropic/claude-3"}},
		},
	}
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "anthropic/claude-3", nil)
	assertPlan(t, plan, []string{"prov-a,anthropic/claude-3", "prov-b,anthropic/claude-3"})

	// The fallback entry must resolve back to (prov-b, anthropic/claude-3).
	p, model, err := ResolveAttempt(cfg, plan[1])
	if err != nil {
		t.Fatalf("ResolveAttempt: %v", err)
	}
	if p.Name != "prov-b" || model != "anthropic/claude-3" {
		t.Errorf("ResolveAttempt = (%q,%q), want (prov-b, anthropic/claude-3)", p.Name, model)
	}
}

// A nil primary (a caller bug) yields a nil plan rather than panicking.
func TestBuildProviderPlanNilPrimary(t *testing.T) {
	cfg := multiProviderCfg()
	if plan := BuildProviderPlan(cfg, nil, "shared-model", nil); plan != nil {
		t.Fatalf("BuildProviderPlan(nil primary) = %v, want nil", plan)
	}
}

// A nil config must not panic: the plan is just the primary (no config means no
// cross-provider fallbacks to discover).
func TestBuildProviderPlanNilConfig(t *testing.T) {
	primary := &config.Provider{Name: "solo", APIBaseURL: "https://x/y", Models: []string{"m"}}
	plan := BuildProviderPlan(nil, primary, "m", nil)
	assertPlan(t, plan, []string{"solo,m"})
}

// The plan composes end-to-end with the existing fallback primitives: every
// non-final entry advances to the next via NextFallbackProvider on a Retryable
// class, the chain terminates cleanly (ok=false, no error) at the end, and a
// Terminal class never advances. This is the exact loop the gateway seam runs.
func TestBuildProviderPlanComposesWithNextFallbackProvider(t *testing.T) {
	cfg := multiProviderCfg()
	plan := BuildProviderPlan(cfg, &cfg.Providers[0], "shared-model", nil)

	// Walk the whole chain the way the gateway would, asserting no infinite
	// loop: it must terminate in exactly len(plan) steps.
	steps := 0
	cur := plan[0]
	for {
		steps++
		if steps > len(plan)+1 {
			t.Fatalf("NextFallbackProvider did not terminate after %d steps (plan len %d)", steps, len(plan))
		}
		p, model, ok, err := NextFallbackProvider(cfg, plan, cur.Model, Retryable)
		if err != nil {
			t.Fatalf("NextFallbackProvider(%q): %v", cur.Model, err)
		}
		if !ok {
			break // chain exhausted
		}
		// The advanced-to (provider, model) must equal the next plan entry
		// resolved.
		wantP, wantModel, rerr := ResolveAttempt(cfg, plan[cur.Index+1])
		if rerr != nil {
			t.Fatalf("ResolveAttempt(plan[%d]): %v", cur.Index+1, rerr)
		}
		if p.Name != wantP.Name || model != wantModel {
			t.Errorf("advanced to (%q,%q), want (%q,%q)", p.Name, model, wantP.Name, wantModel)
		}
		cur = plan[cur.Index+1]
	}
	if steps != len(plan) {
		t.Errorf("walked %d steps, want %d (one terminating check per entry)", steps, len(plan))
	}

	// A Terminal class on the primary must NOT advance, even though fallbacks
	// exist.
	_, _, ok, err := NextFallbackProvider(cfg, plan, plan[0].Model, Terminal)
	if err != nil {
		t.Fatalf("NextFallbackProvider (Terminal): unexpected error %v", err)
	}
	if ok {
		t.Fatal("Terminal class advanced the plan; it must never")
	}
}

// ResolveAttempt surfaces a misconfigured plan entry loudly (matching
// NextFallbackProvider's contract) rather than returning a nil provider.
func TestResolveAttemptErrors(t *testing.T) {
	cfg := multiProviderCfg()

	t.Run("not a selector", func(t *testing.T) {
		_, _, err := ResolveAttempt(cfg, Attempt{Index: 0, Model: "bare-model-id"})
		if err == nil {
			t.Fatal("expected an error for a non-selector attempt")
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		_, _, err := ResolveAttempt(cfg, Attempt{Index: 0, Model: "ghost,shared-model"})
		if err == nil || !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("expected an error naming the unknown provider, got: %v", err)
		}
	})
}

// End-to-end with Select: the (provider, model) Select returns is exactly the
// primary of the plan, resolved. This pins the "primary is Select's output"
// contract the gateway relies on.
func TestBuildProviderPlanPrimaryMatchesSelect(t *testing.T) {
	cfg := multiProviderCfg()
	cfg.Router = config.Route{Default: "p-openai,shared-model"}

	primary, model, err := Select(cfg, &translate.AnthropicRequest{Model: "claude-sonnet"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	plan := BuildProviderPlan(cfg, primary, model, nil)

	gotP, gotModel, err := ResolveAttempt(cfg, plan[0])
	if err != nil {
		t.Fatalf("ResolveAttempt(primary): %v", err)
	}
	if gotP.Name != primary.Name || gotModel != model {
		t.Errorf("plan primary = (%q,%q), want Select's (%q,%q)", gotP.Name, gotModel, primary.Name, model)
	}
}
