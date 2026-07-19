package challenges

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "conflicting-transformers",
		TestName: "TestChallenge_ConflictingTransformers",
		Hypothesis: "config.Validate does not whitelist Transformer.Use values at all, so a " +
			"provider config listing duplicate transformer names and/or a misspelled/unknown " +
			"transformer name alongside real ones is accepted. Provider.Has must still resolve " +
			"each KNOWN name correctly regardless of duplicates or neighbours, and router." +
			"TransformerOptions must reflect exactly that -- with an unknown name being a silent " +
			"no-op, never a crash.",
		ExpectedSafeOutcome: "Validate() succeeds; Has(\"cleancache\") and Has(\"streamoptions\") both " +
			"report true; the unknown/misspelled entry has zero effect and causes no error.",
	})
}

func TestChallenge_ConflictingTransformers(t *testing.T) {
	provider := config.Provider{
		Name:       "acme",
		APIBaseURL: "https://api.acme.example/v1/chat/completions",
		APIKey:     "k",
		Models:     []string{"m"},
		Transformer: &config.Transformer{
			Use: []string{
				"cleancache",
				"cleancache", // duplicate
				"streamoptions",
				"cleancach",           // a plausible typo of "cleancache" -- must NOT accidentally match
				"unknown_transformer", // forward/unknown name
			},
		},
	}
	cfg := &config.Config{
		Providers: []config.Provider{provider},
		Router:    config.Route{Default: "acme,m"},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a provider with duplicate/unknown transformer names: %v", err)
	}

	if !provider.Has("cleancache") {
		t.Error(`Has("cleancache") = false, want true (present, even if duplicated)`)
	}
	if !provider.Has("streamoptions") {
		t.Error(`Has("streamoptions") = false, want true`)
	}
	if provider.Has("cleancach") == false {
		// This assertion is deliberately the OPPOSITE of what it looks like:
		// "cleancach" (the typo) IS itself a distinct, present entry, so
		// Has("cleancach") must be true for THAT exact string. The point of
		// this challenge is that it must NOT be conflated with "cleancache".
		t.Error(`Has("cleancach") = false, want true -- it was explicitly added as its own (typo) entry`)
	}
	if provider.Has("unknown_transformer") == false {
		t.Error(`Has("unknown_transformer") = false, want true -- Has is a raw lookup, unaware of any whitelist`)
	}
	if provider.Has("streamopt") {
		t.Error(`Has("streamopt") = true, want false -- must be an exact match, not a prefix match`)
	}

	opts := router.TransformerOptions(&provider)
	if !opts.CleanCache {
		t.Error("TransformerOptions().CleanCache = false, want true")
	}
	if !opts.StreamOptions {
		t.Error("TransformerOptions().StreamOptions = false, want true")
	}
	t.Log("safe: duplicate and unknown transformer names coexist without error; only recognised names drive behaviour")
	t.Log("finding (not a crash, a DX gap): a misspelled transformer name like \"cleancach\" is accepted " +
		"by Validate() with zero warning -- an operator's typo silently disables the intended fixup. " +
		"See README.md Findings section.")
}
