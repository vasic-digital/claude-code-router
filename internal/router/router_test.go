package router

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func twoProviderCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{
				Name: "main-prov", APIBaseURL: "https://main/v1/chat/completions", APIKey: "k1",
				Models:      []string{"main-model-a", "main-model-b"},
				Transformer: &config.Transformer{Use: []string{"cleancache"}},
			},
			{
				Name: "bg-prov", APIBaseURL: "https://bg/v1/chat/completions", APIKey: "k2",
				Models: []string{"bg-model-a"},
			},
		},
		Router: config.Route{
			Default:    "main-prov,main-model-a",
			Background: "bg-prov,bg-model-a",
		},
	}
}

func TestSelectDefaultRouteForOrdinaryModel(t *testing.T) {
	cfg := twoProviderCfg()
	req := &translate.AnthropicRequest{Model: "claude-3-7-sonnet-20250219"}

	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" {
		t.Errorf("provider = %q, want main-prov", p.Name)
	}
	if model != "main-model-a" {
		t.Errorf("model = %q, want main-model-a", model)
	}
}

func TestSelectBackgroundRouteForHaikuModel(t *testing.T) {
	cfg := twoProviderCfg()
	cases := []string{
		"haiku",
		"claude-3-5-haiku-20241022",
		"HAIKU-UPPER-CASE",
	}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			req := &translate.AnthropicRequest{Model: model}
			p, gotModel, err := Select(cfg, req)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if p.Name != "bg-prov" {
				t.Errorf("provider = %q, want bg-prov", p.Name)
			}
			if gotModel != "bg-model-a" {
				t.Errorf("model = %q, want bg-model-a", gotModel)
			}
		})
	}
}

// When Background is unset, even a haiku request must still fall back to
// Default rather than erroring — a single-route config is common and valid.
func TestSelectHaikuFallsBackToDefaultWhenBackgroundUnset(t *testing.T) {
	cfg := twoProviderCfg()
	cfg.Router.Background = ""
	req := &translate.AnthropicRequest{Model: "claude-3-5-haiku-20241022"}

	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// A nil request (e.g. a health probe path building no Anthropic body) must
// not panic and must resolve via Default.
func TestSelectNilRequestUsesDefault(t *testing.T) {
	cfg := twoProviderCfg()
	p, model, err := Select(cfg, nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// No Router block at all: the first provider and its first model must be
// used, so a minimal single-provider config works without extra ceremony.
func TestSelectFallsBackToFirstProviderWhenRouteEmpty(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "only", APIBaseURL: "https://only/v1/chat/completions", Models: []string{"m1", "m2"}},
		},
	}
	p, model, err := Select(cfg, &translate.AnthropicRequest{Model: "whatever"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "only" || model != "m1" {
		t.Errorf("got (%q,%q), want (only, m1)", p.Name, model)
	}
}

// Fallback also applies on the haiku path when Background is unset AND
// Default is unset: still must not error out with providers present.
func TestSelectFallsBackForHaikuWithNoRoutesConfigured(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "only", APIBaseURL: "https://only/v1/chat/completions", Models: []string{"m1"}},
		},
	}
	p, model, err := Select(cfg, &translate.AnthropicRequest{Model: "haiku"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "only" || model != "m1" {
		t.Errorf("got (%q,%q), want (only, m1)", p.Name, model)
	}
}

func TestSelectErrorsWithNoProvidersAtAll(t *testing.T) {
	_, _, err := Select(&config.Config{}, &translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected an error with zero providers configured")
	}
}

func TestSelectErrorsWhenFallbackProviderHasNoModels(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "empty", APIBaseURL: "https://x/y"}},
	}
	_, _, err := Select(cfg, &translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected an error when the fallback provider has no models")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should name the provider, got: %v", err)
	}
}

// A route referencing a provider that does not exist must fail loudly and
// name the offending provider — this is the case Config.Validate() would
// normally catch at load time, but Select must not trust that callers always
// go through Load/Validate first.
func TestSelectErrorsOnRouteToUnknownProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "real", APIBaseURL: "https://x/y", Models: []string{"m"}}},
		Router:    config.Route{Default: "ghost,some-model"},
	}
	_, _, err := Select(cfg, &translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected an error for a route to an unknown provider")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the unknown provider %q, got: %v", "ghost", err)
	}
}

// A malformed route string (no comma) must surface as an error rather than
// panicking or silently misrouting.
func TestSelectErrorsOnMalformedRoute(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.Provider{{Name: "real", APIBaseURL: "https://x/y", Models: []string{"m"}}},
		Router:    config.Route{Default: "not-a-valid-route"},
	}
	_, _, err := Select(cfg, &translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected an error for a malformed route string")
	}
}

func TestSelectNilConfigErrors(t *testing.T) {
	_, _, err := Select(nil, &translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected an error for a nil config")
	}
}

func TestTransformerOptionsMapsProviderFlags(t *testing.T) {
	cases := []struct {
		name string
		p    *config.Provider
		want translate.Options
	}{
		{
			name: "both set",
			p:    &config.Provider{Transformer: &config.Transformer{Use: []string{"cleancache", "streamoptions"}}},
			want: translate.Options{CleanCache: true, StreamOptions: true},
		},
		{
			name: "cleancache only",
			p:    &config.Provider{Transformer: &config.Transformer{Use: []string{"cleancache"}}},
			want: translate.Options{CleanCache: true, StreamOptions: false},
		},
		{
			name: "none set",
			p:    &config.Provider{},
			want: translate.Options{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TransformerOptions(tc.p)
			if got.CleanCache != tc.want.CleanCache || got.StreamOptions != tc.want.StreamOptions {
				t.Errorf("TransformerOptions = %+v, want %+v", got, tc.want)
			}
		})
	}
}
