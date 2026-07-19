package challenges

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "zero-models-provider",
		TestName: "TestChallenge_ZeroModelsProvider",
		Hypothesis: "A provider declared with an empty Models list is a plausible operator mistake " +
			"(copy-pasted a provider block and forgot to fill in Models).",
		ExpectedSafeOutcome: "When a route explicitly names a model, the empty Models list must not " +
			"block it (no panic, no bogus error). When there is NO route at all and the router must " +
			"fall back to \"the first provider's first model\", a zero-model provider must fail with a " +
			"clear, specific error instead of an index-out-of-range panic.",
	})
}

// TestChallenge_ZeroModelsProvider exercises both paths that touch
// Provider.Models: the normal explicit-route path (which never reads
// Models at all) and the zero-route fallback path (firstProviderFallback),
// which explicitly guards len(p.Models) == 0.
func TestChallenge_ZeroModelsProvider(t *testing.T) {
	provider := config.Provider{
		Name:       "empty-catalogue",
		APIBaseURL: "https://api.example/v1/chat/completions",
		APIKey:     "k",
		Models:     nil, // zero models declared
	}

	t.Run("explicit_route_is_unaffected_by_empty_Models", func(t *testing.T) {
		cfg := &config.Config{
			Providers: []config.Provider{provider},
			Router:    config.Route{Default: "empty-catalogue,some-model"},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected a provider with zero Models despite an explicit route: %v", err)
		}
		p, model, err := router.Select(cfg, &translate.AnthropicRequest{Model: "claude-sonnet-4-5"})
		if err != nil {
			t.Fatalf("Select() failed for an explicit route to a zero-Models provider: %v", err)
		}
		if p.Name != "empty-catalogue" || model != "some-model" {
			t.Errorf("got provider=%q model=%q, want empty-catalogue/some-model", p.Name, model)
		}
	})

	t.Run("fallback_path_with_no_route_fails_cleanly_not_a_panic", func(t *testing.T) {
		cfg := &config.Config{
			Providers: []config.Provider{provider},
			Router:    config.Route{}, // no route configured at all -> triggers firstProviderFallback
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected an otherwise-valid zero-Models provider: %v", err)
		}

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Select() panicked instead of failing cleanly: %v", r)
			}
		}()
		_, _, err := router.Select(cfg, &translate.AnthropicRequest{Model: "claude-sonnet-4-5"})
		if err == nil {
			t.Fatal("expected a clear error when the only provider has zero models and no route is configured")
		}
		if !strings.Contains(err.Error(), "no models") {
			t.Errorf("error = %q, want it to explain the provider has no models", err.Error())
		}
		t.Logf("safe: clean error, no panic: %v", err)
	})
}
