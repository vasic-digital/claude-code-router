package router

// Property-based tests for the cross-provider execution-plan API
// (BuildProviderPlan / ResolveAttempt / NextFallbackProvider).
//
// The example-based tests in plan_test.go pin specific, hand-written configs.
// These tests instead assert the plan's INVARIANTS hold across many randomly
// generated configs and primary selections, so a regression that only shows up
// on an unusual provider/model shape is caught. The generator is seeded with a
// fixed value inside each test body (never math/rand at package init), so every
// run explores the identical sequence of inputs and a failure is reproducible.

import (
	"math/rand"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// propIters is how many generated (config, primary) pairs each property is
// checked against. High enough to explore overlapping model sets, duplicate
// provider names, slash-bearing model ids and empty model lists; low enough to
// stay fast under -race.
const propIters = 800

// propNamePool is intentionally small so generated configs frequently reuse a
// name (exercising duplicate-name handling) and frequently have several
// providers serving the same model (the cross-provider fallback case). Names
// contain neither the comma nor slash that providerSelector/parseExplicitSelector
// treat as separators, so every generated selector round-trips.
var propNamePool = []string{"po", "az", "bd", "gc", "aw", "hf"}

// propModelPool mixes plain ids with slash-bearing catalog ids ("org/model")
// and a multi-slash id, so the comma-separator round-trip is exercised against
// exactly the ids the doc comment on providerSelector calls out. No id contains
// a comma.
var propModelPool = []string{"m-alpha", "m-beta", "shared", "anthropic/claude-3", "a/b/c", "gpt-4o"}

// genRouterConfig builds a random config: 1..8 providers, each with a
// pool-drawn (possibly duplicate) name and a random, possibly-empty subset of
// the model pool.
func genRouterConfig(rng *rand.Rand) *config.Config {
	n := 1 + rng.Intn(8)
	providers := make([]config.Provider, n)
	for i := range providers {
		name := propNamePool[rng.Intn(len(propNamePool))]
		var models []string
		for _, m := range propModelPool {
			if rng.Intn(2) == 0 {
				models = append(models, m)
			}
		}
		providers[i] = config.Provider{
			Name:       name,
			APIBaseURL: "https://" + name + "/v1/chat/completions",
			Models:     models,
		}
	}
	return &config.Config{Providers: providers}
}

// genEligiblePrimary picks a random provider that actually serves at least one
// model, plus one of its models, to use as the plan's primary. Returns ok=false
// when no provider in cfg lists any model (the caller then skips that iteration
// — BuildProviderPlan's primary contract assumes Select produced a real
// (provider, model)).
func genEligiblePrimary(rng *rand.Rand, cfg *config.Config) (primary *config.Provider, model string, ok bool) {
	var eligible []int
	for i := range cfg.Providers {
		if len(cfg.Providers[i].Models) > 0 {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return nil, "", false
	}
	idx := eligible[rng.Intn(len(eligible))]
	p := &cfg.Providers[idx]
	return p, p.Models[rng.Intn(len(p.Models))], true
}

// TestPropBuildProviderPlanInvariants asserts, over many generated inputs, the
// core structural invariants of a plan built with no explicit fallbacks (so
// every entry is either the primary or auto-discovered and MUST resolve):
//
//	(a) the primary is byte-for-byte at index 0;
//	(b) no two attempts name the same (provider, model);
//	(c) every attempt resolves via ResolveAttempt to a configured provider,
//	    and its selector reconstructs from the resolved (name, model);
//	    Index is a gap-free 0..n-1 sequence.
func TestPropBuildProviderPlanInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE))
	checked := 0
	for iter := 0; iter < propIters; iter++ {
		cfg := genRouterConfig(rng)
		primary, model, ok := genEligiblePrimary(rng, cfg)
		if !ok {
			continue
		}
		checked++
		plan := BuildProviderPlan(cfg, primary, model, nil)

		// (a) primary at index 0, exactly Select's (provider, model).
		if len(plan) == 0 {
			t.Fatalf("iter %d: empty plan for primary (%q,%q)", iter, primary.Name, model)
		}
		wantPrimary := providerSelector(primary.Name, model)
		if plan[0].Model != wantPrimary || plan[0].Index != 0 {
			t.Fatalf("iter %d: plan[0] = {%d,%q}, want {0,%q}", iter, plan[0].Index, plan[0].Model, wantPrimary)
		}

		// (b) no duplicate (provider, model) attempt.
		seen := make(map[string]bool, len(plan))
		for _, a := range plan {
			if seen[a.Model] {
				t.Fatalf("iter %d: duplicate attempt %q in plan %v", iter, a.Model, planModels(plan))
			}
			seen[a.Model] = true
		}

		// (c) every attempt resolves; Index gap-free; selector round-trips.
		for i, a := range plan {
			if a.Index != i {
				t.Fatalf("iter %d: plan[%d].Index = %d, want %d", iter, i, a.Index, i)
			}
			p, m, err := ResolveAttempt(cfg, a)
			if err != nil || p == nil {
				t.Fatalf("iter %d: ResolveAttempt(%q) = (%v,%q,%v), want a configured provider", iter, a.Model, p, m, err)
			}
			if got := providerSelector(p.Name, m); got != a.Model {
				t.Fatalf("iter %d: selector round-trip: ResolveAttempt(%q) -> providerSelector = %q", iter, a.Model, got)
			}
		}
	}
	if checked == 0 {
		t.Fatal("generator produced no eligible primary in any iteration")
	}
}

// TestPropBuildProviderPlanSingleProviderYieldsOneElement asserts invariant (d):
// a config with exactly ONE provider always yields a one-element plan (just the
// primary), no matter how many models that provider lists — there is no other
// provider to fall back to.
func TestPropBuildProviderPlanSingleProviderYieldsOneElement(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5106713))
	for iter := 0; iter < propIters; iter++ {
		name := propNamePool[rng.Intn(len(propNamePool))]
		var models []string
		for _, m := range propModelPool {
			if rng.Intn(2) == 0 {
				models = append(models, m)
			}
		}
		if len(models) == 0 {
			models = []string{propModelPool[rng.Intn(len(propModelPool))]}
		}
		cfg := &config.Config{Providers: []config.Provider{
			{Name: name, APIBaseURL: "https://" + name + "/v1/chat/completions", Models: models},
		}}
		model := models[rng.Intn(len(models))]
		plan := BuildProviderPlan(cfg, &cfg.Providers[0], model, nil)
		if len(plan) != 1 {
			t.Fatalf("iter %d: single-provider plan len = %d (%v), want 1", iter, len(plan), planModels(plan))
		}
		if plan[0].Model != providerSelector(name, model) || plan[0].Index != 0 {
			t.Fatalf("iter %d: plan[0] = {%d,%q}, want {0,%q}", iter, plan[0].Index, plan[0].Model, providerSelector(name, model))
		}
	}
}

// TestPropBuildProviderPlanDeterministic asserts invariant (e): the same input
// always produces the same plan (identical selectors AND identical indices).
// Determinism is what lets a caller cache/log a plan and trust a re-derivation
// matches it.
func TestPropBuildProviderPlanDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(0xD37E4))
	for iter := 0; iter < propIters; iter++ {
		cfg := genRouterConfig(rng)
		primary, model, ok := genEligiblePrimary(rng, cfg)
		if !ok {
			continue
		}
		a := BuildProviderPlan(cfg, primary, model, nil)
		b := BuildProviderPlan(cfg, primary, model, nil)
		if !equalStrings(planModels(a), planModels(b)) {
			t.Fatalf("iter %d: non-deterministic selectors: %v vs %v", iter, planModels(a), planModels(b))
		}
		if len(a) != len(b) {
			t.Fatalf("iter %d: non-deterministic length: %d vs %d", iter, len(a), len(b))
		}
		for i := range a {
			if a[i].Index != b[i].Index {
				t.Fatalf("iter %d: attempt %d index differs: %d vs %d", iter, i, a[i].Index, b[i].Index)
			}
		}
	}
}

// TestPropNextFallbackProviderWalkTerminates asserts the walk invariant: driving
// a plan with NextFallbackProvider from index 0 while always classifying
// Retryable visits EVERY entry and terminates in EXACTLY len(plan) steps — no
// infinite loop, no skipped entry — and each advance lands on the next plan
// entry. It also asserts a Terminal classification never advances.
func TestPropNextFallbackProviderWalkTerminates(t *testing.T) {
	rng := rand.New(rand.NewSource(0xFA11BAC))
	checked := 0
	for iter := 0; iter < propIters; iter++ {
		cfg := genRouterConfig(rng)
		primary, model, ok := genEligiblePrimary(rng, cfg)
		if !ok {
			continue
		}
		checked++
		plan := BuildProviderPlan(cfg, primary, model, nil)

		// Retryable walk from the head must terminate in exactly len(plan)
		// steps (one classify per entry, the last returning ok=false).
		steps := 0
		cur := plan[0]
		for {
			steps++
			if steps > len(plan)+1 {
				t.Fatalf("iter %d: walk did not terminate after %d steps (plan len %d, %v)", iter, steps, len(plan), planModels(plan))
			}
			p, m, advanced, err := NextFallbackProvider(cfg, plan, cur.Model, Retryable)
			if err != nil {
				t.Fatalf("iter %d: NextFallbackProvider(%q): %v", iter, cur.Model, err)
			}
			if !advanced {
				break
			}
			next := plan[cur.Index+1]
			wantP, wantM, rerr := ResolveAttempt(cfg, next)
			if rerr != nil {
				t.Fatalf("iter %d: ResolveAttempt(%q): %v", iter, next.Model, rerr)
			}
			if p.Name != wantP.Name || m != wantM {
				t.Fatalf("iter %d: advanced to (%q,%q), want (%q,%q)", iter, p.Name, m, wantP.Name, wantM)
			}
			cur = next
		}
		if steps != len(plan) {
			t.Fatalf("iter %d: walked %d steps, want %d", iter, steps, len(plan))
		}

		// Terminal on the head never advances.
		if _, _, advanced, err := NextFallbackProvider(cfg, plan, plan[0].Model, Terminal); advanced || err != nil {
			t.Fatalf("iter %d: Terminal advanced=%v err=%v, want advanced=false err=nil", iter, advanced, err)
		}
	}
	if checked == 0 {
		t.Fatal("generator produced no eligible primary in any iteration")
	}
}
