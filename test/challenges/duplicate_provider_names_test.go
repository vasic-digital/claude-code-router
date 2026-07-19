package challenges

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "duplicate-provider-names",
		TestName: "TestChallenge_DuplicateProviderNames",
		Hypothesis: "Two providers configured with the exact same Name would make " +
			"config.ProviderByName / router.Select ambiguous (which one does a route actually " +
			"select?), so this must be rejected outright at config-validation time rather than " +
			"silently picking whichever one happens to be first (or last) in the slice.",
		ExpectedSafeOutcome: "Config.Validate returns a clear, specific error naming the duplicate " +
			"provider, and does so BEFORE any routing decision is ever made.",
	})
}

func TestChallenge_DuplicateProviderNames(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "acme", APIBaseURL: "https://api.acme.example/v1/a", APIKey: "k1", Models: []string{"m1"}},
			{Name: "acme", APIBaseURL: "https://api.acme.example/v1/b", APIKey: "k2", Models: []string{"m2"}},
		},
		Router: config.Route{Default: "acme,m1"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted two providers with the same name -- routing would be ambiguous")
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "acme") {
		t.Errorf("error = %q, want it to clearly name the duplicate provider", err.Error())
	}
	t.Logf("safe: clean, specific rejection: %v", err)
}
