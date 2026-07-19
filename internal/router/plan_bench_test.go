package router

// Benchmarks for the cross-provider execution-plan API. BuildProviderPlan runs
// once per request on the routing hot path (see the gateway seam in plan.go), so
// its cost as the configured provider count grows — and the cost of walking the
// resulting fallback chain — is worth tracking. Sizes span a tiny config, a
// mid-size fleet, and a deliberately large one to surface any super-linear
// behaviour.

import (
	"fmt"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// benchSink absorbs benchmark results so the compiler cannot elide the work
// under measurement.
var benchSink int

// benchSharedModel is the one model every provider in a benchmark config serves,
// so the built plan's length equals the provider count (the worst case: every
// provider is a cross-provider fallback for it).
const benchSharedModel = "shared-model"

// benchConfig builds an n-provider config in which every provider serves
// benchSharedModel, and returns it alongside the first provider as the primary.
func benchConfig(n int) (*config.Config, *config.Provider) {
	providers := make([]config.Provider, n)
	for i := range providers {
		name := fmt.Sprintf("p%d", i)
		providers[i] = config.Provider{
			Name:       name,
			APIBaseURL: "https://" + name + "/v1/chat/completions",
			Models:     []string{benchSharedModel},
		}
	}
	cfg := &config.Config{Providers: providers}
	return cfg, &cfg.Providers[0]
}

var benchSizes = []struct {
	name string
	n    int
}{
	{"small", 3},
	{"medium", 25},
	{"large", 200},
}

// BenchmarkBuildProviderPlan measures plan construction across small/medium/large
// provider counts (plan length == provider count for benchSharedModel).
func BenchmarkBuildProviderPlan(b *testing.B) {
	for _, sz := range benchSizes {
		cfg, primary := benchConfig(sz.n)
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				plan := BuildProviderPlan(cfg, primary, benchSharedModel, nil)
				benchSink += len(plan)
			}
		})
	}
}

// BenchmarkNextFallbackProvider measures walking a full fallback chain: from the
// head, advancing on Retryable until the plan is exhausted (chain length ==
// provider count).
func BenchmarkNextFallbackProvider(b *testing.B) {
	for _, sz := range benchSizes {
		cfg, primary := benchConfig(sz.n)
		plan := BuildProviderPlan(cfg, primary, benchSharedModel, nil)
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cur := plan[0]
				for {
					p, _, ok, err := NextFallbackProvider(cfg, plan, cur.Model, Retryable)
					if err != nil {
						b.Fatalf("NextFallbackProvider(%q): %v", cur.Model, err)
					}
					if !ok {
						break
					}
					benchSink += len(p.Name)
					cur = plan[cur.Index+1]
				}
			}
		})
	}
}
