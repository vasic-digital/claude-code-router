package challenges

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "model-id-special-chars",
		TestName: "TestChallenge_ModelIDWithCommasQuotesNewlines",
		Hypothesis: "config.SplitRoute splits a \"provider,model\" route string on the FIRST comma " +
			"only, so a model id containing further commas, embedded quotes, or a literal newline " +
			"should still be captured verbatim as a single model id, and must round-trip safely " +
			"through JSON re-encoding for the upstream request.",
		ExpectedSafeOutcome: "No panic anywhere in the pipeline; the odd model id string survives " +
			"router.Select and translate.AnthropicToOpenAI byte-for-byte, and the resulting request " +
			"still marshals to valid JSON.",
	})
}

func TestChallenge_ModelIDWithCommasQuotesNewlines(t *testing.T) {
	const weird = `weird"model,with,commas` + "\nand-a-newline\tand-a-tab"

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "acme",
			APIBaseURL: "https://api.acme.example/v1/chat/completions",
			APIKey:     "k",
			Models:     []string{weird},
		}},
		Router: config.Route{Default: "acme," + weird},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a route whose model id contains commas/quotes/newlines: %v", err)
	}

	p, model, err := router.Select(cfg, &translate.AnthropicRequest{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("Select() failed: %v", err)
	}
	if model != weird {
		t.Fatalf("model = %q, want the exact weird id %q (SplitRoute must split on the FIRST comma only)", model, weird)
	}
	if p.Name != "acme" {
		t.Fatalf("provider = %q, want acme", p.Name)
	}

	// Push the weird id all the way through the translation layer via the
	// router's own model override, and confirm the resulting request is
	// still valid, round-trippable JSON.
	req := &translate.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := translate.AnthropicToOpenAI(req, router.TransformerOptions(p) /* CleanCache/StreamOptions both false here */)
	if err != nil {
		t.Fatalf("AnthropicToOpenAI failed: %v", err)
	}
	out.Model = model // simulate the gateway applying the router's chosen model

	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("re-parsing the marshaled request failed -- the weird model id broke JSON round-tripping: %v\nraw: %s", err, b)
	}
	if got, _ := roundTrip["model"].(string); got != weird {
		t.Errorf("round-tripped model = %q, want %q", got, weird)
	}
	if !strings.Contains(string(b), `\n`) {
		t.Errorf("expected the embedded newline to be JSON-escaped in the wire payload, got: %s", b)
	}
	t.Logf("safe: weird model id survived Select + AnthropicToOpenAI + JSON round-trip intact")
}
