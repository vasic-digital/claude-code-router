package challenges

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "contradictory-config",
		TestName: "TestChallenge_ContradictoryConfig",
		Hypothesis: "A config where Router.default names a model that is NOT present in that " +
			"provider's declared Models list is contradictory (the provider claims to only " +
			"serve certain models, then the route asks for one it never declared).",
		ExpectedSafeOutcome: "config.Load/Validate must not panic, and router.Select must return a " +
			"deterministic result (either a clean error, or the requested pair with no crash) -- " +
			"it must never silently route to a zero-value/garbage provider.",
	})
}

// TestChallenge_ContradictoryConfig proves the router survives a config
// that is internally contradictory: the provider's Models list is treated
// as informational (used only by the zero-route fallback and, presumably,
// by discovery UIs), not as an allowlist enforced by Select. This is
// current, deliberate-looking permissiveness -- documented here rather than
// assumed -- so a config with Router.default pointing at an undeclared
// model is accepted, not rejected, and Select returns exactly that pair.
func TestChallenge_ContradictoryConfig(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{
				Name:       "acme",
				APIBaseURL: "https://api.acme.example/v1/chat/completions",
				APIKey:     "k",
				Models:     []string{"acme-small"}, // declared: only acme-small
			},
		},
		Router: config.Route{
			Default: "acme,acme-XL-not-declared-anywhere", // contradicts Models above
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpectedly rejected a contradictory-but-structurally-valid config: %v", err)
	}

	req := &translate.AnthropicRequest{Model: "claude-sonnet-4-5"}
	p, model, err := router.Select(cfg, req)
	if err != nil {
		t.Fatalf("Select() returned an error for a structurally valid (if contradictory) route: %v", err)
	}
	if p == nil {
		t.Fatal("Select() returned a nil provider with a nil error -- must never happen")
	}
	if p.Name != "acme" {
		t.Errorf("provider = %q, want acme", p.Name)
	}
	if model != "acme-XL-not-declared-anywhere" {
		t.Errorf("model = %q, want the exact (undeclared) routed model, no silent substitution", model)
	}
	t.Logf("safe: config with an undeclared routed model is accepted; Models[] is informational, not an enforced allowlist here")
}
