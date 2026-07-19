package challenges

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "unicode-provider-name",
		TestName: "TestChallenge_UnicodeProviderName",
		Hypothesis: "config.SplitRoute and config.ProviderByName do plain Go string " +
			"equality/indexing, which is byte-safe for UTF-8 (Go never assumes ASCII), so a " +
			"provider name containing CJK characters and emoji should validate, route, and " +
			"resolve exactly as reliably as an ASCII name.",
		ExpectedSafeOutcome: "The unicode provider name round-trips through Validate, SplitRoute, " +
			"ProviderByName, and Select without any mangling, truncation, or lookup failure.",
	})
}

func TestChallenge_UnicodeProviderName(t *testing.T) {
	const name = "供应商-🚀-provider"
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       name,
			APIBaseURL: "https://api.example/v1/chat/completions",
			APIKey:     "k",
			Models:     []string{"model-1"},
		}},
		Router: config.Route{Default: name + ",model-1"},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a unicode provider name: %v", err)
	}

	if p := cfg.ProviderByName(name); p == nil {
		t.Fatal("ProviderByName failed to find the unicode-named provider")
	}

	gotName, gotModel, err := config.SplitRoute(name + ",model-1")
	if err != nil {
		t.Fatalf("SplitRoute failed on a unicode provider name: %v", err)
	}
	if gotName != name {
		t.Fatalf("SplitRoute provider = %q, want %q", gotName, name)
	}
	if gotModel != "model-1" {
		t.Fatalf("SplitRoute model = %q, want model-1", gotModel)
	}

	p, model, err := router.Select(cfg, &translate.AnthropicRequest{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("Select() failed: %v", err)
	}
	if p.Name != name || model != "model-1" {
		t.Fatalf("Select() returned provider=%q model=%q, want %q/model-1", p.Name, model, name)
	}
	t.Logf("safe: unicode provider name %q round-tripped through the whole routing pipeline", name)
}
