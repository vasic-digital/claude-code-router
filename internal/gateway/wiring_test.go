package gateway

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// wiringCfg has DISTINCT default and background routes, so a test can tell
// which one the router actually chose.
func wiringCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{
				Name: "big", APIBaseURL: "https://big.example/v1/chat/completions",
				APIKey: "k1", Models: []string{"expensive-model"},
				Transformer: &config.Transformer{Use: []string{"cleancache", "streamoptions"}},
			},
			{
				Name: "small", APIBaseURL: "https://small.example/v1/chat/completions",
				APIKey: "k2", Models: []string{"cheap-model"},
			},
		},
		Router: config.Route{
			Default:    "big,expensive-model",
			Background: "small,cheap-model",
		},
	}
}

// The whole point of wiring the real router in: the built-in defaultRouter
// always resolves Router.default, so a haiku-tier request would be sent to the
// expensive model. After WireDefaults it must resolve Router.background.
func TestWireDefaultsEnablesBackgroundRouting(t *testing.T) {
	s := New(wiringCfg(), Options{})

	haiku := &translate.AnthropicRequest{
		Model:    "claude-3-5-haiku-20241022",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}

	// Before wiring: the minimal default sends everything to Router.default.
	pBefore, mBefore, err := s.Router.Route(haiku)
	if err != nil {
		t.Fatalf("default router: %v", err)
	}
	if pBefore.Name != "big" || mBefore != "expensive-model" {
		t.Fatalf("built-in default should resolve Router.default, got %s/%s", pBefore.Name, mBefore)
	}

	// After wiring: haiku-tier is recognised and routed to background.
	s.WireDefaults(30 * time.Second)
	pAfter, mAfter, err := s.Router.Route(haiku)
	if err != nil {
		t.Fatalf("wired router: %v", err)
	}
	if pAfter.Name != "small" || mAfter != "cheap-model" {
		t.Errorf("haiku-tier routed to %s/%s, want small/cheap-model (Router.background)",
			pAfter.Name, mAfter)
	}
}

// A non-haiku request must still take Router.default after wiring.
func TestWiredRouterKeepsDefaultForOrdinaryModels(t *testing.T) {
	s := New(wiringCfg(), Options{})
	s.WireDefaults(30 * time.Second)

	req := &translate.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	p, m, err := s.Router.Route(req)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if p.Name != "big" || m != "expensive-model" {
		t.Errorf("ordinary model routed to %s/%s, want big/expensive-model", p.Name, m)
	}
}

// Route returns a COPY: a handler must not be able to corrupt shared config
// state (which every subsequent request reads) by mutating what it received.
func TestRouteReturnsCopyNotSharedConfigPointer(t *testing.T) {
	cfg := wiringCfg()
	s := New(cfg, Options{})
	s.WireDefaults(time.Second)

	req := &translate.AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	p, _, err := s.Router.Route(req)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	p.APIKey = "MUTATED"
	p.APIBaseURL = "https://attacker.example/"

	if cfg.Providers[0].APIKey == "MUTATED" {
		t.Error("mutating the returned provider corrupted the shared config APIKey")
	}
	if cfg.Providers[0].APIBaseURL == "https://attacker.example/" {
		t.Error("mutating the returned provider corrupted the shared config APIBaseURL")
	}
}

// Transformer flags must survive the wiring, or cleancache/streamoptions
// silently stop applying and upstreams start rejecting requests.
func TestTransformerOptionsForReflectsProviderConfig(t *testing.T) {
	cfg := wiringCfg()

	withBoth := TransformerOptionsFor(&cfg.Providers[0])
	if !withBoth.CleanCache {
		t.Error("cleancache not propagated for a provider that declares it")
	}
	if !withBoth.StreamOptions {
		t.Error("streamoptions not propagated for a provider that declares it")
	}

	withNone := TransformerOptionsFor(&cfg.Providers[1])
	if withNone.CleanCache || withNone.StreamOptions {
		t.Errorf("transformers enabled for a provider that declares none: %+v", withNone)
	}
}

// An unroutable config must surface a named error, never a silent fallback to
// an arbitrary upstream.
func TestWiredRouterErrorsWhenNothingIsRoutable(t *testing.T) {
	s := New(&config.Config{}, Options{})
	s.WireDefaults(time.Second)

	_, _, err := s.Router.Route(&translate.AnthropicRequest{Model: "m"})
	if err == nil {
		t.Fatal("routing with no providers must return an error")
	}
}
