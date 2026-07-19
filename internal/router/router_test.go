package router

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// fourRouteCfg has a distinct provider behind each of the four routes so a test
// can tell which route fired purely from the resolved provider name.
func fourRouteCfg() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{Name: "main-prov", APIBaseURL: "https://main/v1/chat/completions", APIKey: "k1", Models: []string{"main-model-a", "main-model-b"}},
			{Name: "bg-prov", APIBaseURL: "https://bg/v1/chat/completions", APIKey: "k2", Models: []string{"bg-model-a"}},
			{Name: "think-prov", APIBaseURL: "https://think/v1/chat/completions", APIKey: "k3", Models: []string{"think-model"}},
			{Name: "lc-prov", APIBaseURL: "https://lc/v1/chat/completions", APIKey: "k4", Models: []string{"lc-model"}},
		},
		Router: config.Route{
			Default:     "main-prov,main-model-a",
			Background:  "bg-prov,bg-model-a",
			Think:       "think-prov,think-model",
			LongContext: "lc-prov,lc-model",
		},
	}
}

// bigContentRequest builds a request whose single message is large enough that
// estimateTokenCount clears DefaultLongContextThreshold. model lets a caller
// also set the tier (e.g. a haiku id) to exercise the precedence interplay.
func bigContentRequest(model string) *translate.AnthropicRequest {
	// A JSON string of this many characters estimates to > threshold tokens.
	body := `"` + strings.Repeat("x", (DefaultLongContextThreshold+1000)*charsPerToken) + `"`
	return &translate.AnthropicRequest{
		Model:    model,
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(body)}},
	}
}

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

// ---------- Think / LongContext override precedence ----------

// TestChooseRoutePrecedence pins the full route-override precedence directly on
// chooseRoute, so the think signal (which no production request carries today —
// see requestWantsThinking) is supplied explicitly rather than through a real
// request body. The rest of the matrix (longContext, haiku, default, and the
// regression case where think/longContext are unset) is covered here too.
func TestChooseRoutePrecedence(t *testing.T) {
	all := config.Route{
		Default:     "main-prov,main-model-a",
		Background:  "bg-prov,bg-model-a",
		Think:       "think-prov,think-model",
		LongContext: "lc-prov,lc-model",
	}
	cases := []struct {
		name  string
		route config.Route
		sig   routeSignals
		want  string
	}{
		{"ordinary -> default", all, routeSignals{}, "main-prov,main-model-a"},
		{"thinking with think set -> think", all, routeSignals{thinking: true}, "think-prov,think-model"},
		{
			"thinking with think unset -> default (unchanged)",
			config.Route{Default: "main-prov,main-model-a"},
			routeSignals{thinking: true},
			"main-prov,main-model-a",
		},
		{"longContext with lc set -> longContext", all, routeSignals{longContext: true}, "lc-prov,lc-model"},
		{
			"longContext with lc unset -> default (unchanged)",
			config.Route{Default: "main-prov,main-model-a", Background: "bg-prov,bg-model-a"},
			routeSignals{longContext: true},
			"main-prov,main-model-a",
		},
		{"haiku -> background", all, routeSignals{haiku: true}, "bg-prov,bg-model-a"},
		{
			"haiku with background unset -> default",
			config.Route{Default: "main-prov,main-model-a"},
			routeSignals{haiku: true},
			"main-prov,main-model-a",
		},
		// Precedence interplay:
		{"longContext beats think", all, routeSignals{longContext: true, thinking: true}, "lc-prov,lc-model"},
		{"longContext beats background (haiku)", all, routeSignals{longContext: true, haiku: true}, "lc-prov,lc-model"},
		{"background beats think (haiku + thinking)", all, routeSignals{haiku: true, thinking: true}, "bg-prov,bg-model-a"},
		{"longContext beats everything", all, routeSignals{longContext: true, haiku: true, thinking: true}, "lc-prov,lc-model"},
		// Regression guard: with Think AND LongContext both unset, every signal
		// combination collapses to the old haiku->Background-else-Default rule.
		{
			"regression: no think/lc, haiku -> background",
			config.Route{Default: "main-prov,main-model-a", Background: "bg-prov,bg-model-a"},
			routeSignals{haiku: true, thinking: true, longContext: true},
			"bg-prov,bg-model-a",
		},
		{
			"regression: no think/lc, ordinary -> default",
			config.Route{Default: "main-prov,main-model-a", Background: "bg-prov,bg-model-a"},
			routeSignals{thinking: true, longContext: true},
			"main-prov,main-model-a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseRoute(tc.route, tc.sig); got != tc.want {
				t.Errorf("chooseRoute(%+v) = %q, want %q", tc.sig, got, tc.want)
			}
		})
	}
}

// TestSelectLongContextRouteFiresFromLargeRequest proves the long-context path
// end-to-end through Select from a real request body — no caller plumbing
// needed, since the size is computed from fields Select already receives.
func TestSelectLongContextRouteFiresFromLargeRequest(t *testing.T) {
	cfg := fourRouteCfg()
	p, model, err := Select(cfg, bigContentRequest("claude-3-7-sonnet-20250219"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "lc-prov" || model != "lc-model" {
		t.Errorf("got (%q,%q), want (lc-prov, lc-model)", p.Name, model)
	}
}

// A large request whose model is also haiku-tier must STILL go to LongContext:
// the oversized prompt outranks the background tier (see chooseRoute).
func TestSelectLongContextBeatsBackgroundForLargeHaikuRequest(t *testing.T) {
	cfg := fourRouteCfg()
	p, model, err := Select(cfg, bigContentRequest("claude-3-5-haiku-20241022"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "lc-prov" || model != "lc-model" {
		t.Errorf("got (%q,%q), want (lc-prov, lc-model)", p.Name, model)
	}
}

// A below-threshold request must NOT trip the long-context route: it resolves
// to Default like any ordinary turn.
func TestSelectBelowThresholdUsesDefault(t *testing.T) {
	cfg := fourRouteCfg()
	req := &translate.AnthropicRequest{
		Model:    "claude-3-7-sonnet-20250219",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hello there"`)}},
	}
	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// Regression: with LongContext UNSET, even an enormous request stays on Default
// — behaviour is identical to before the override existed.
func TestSelectLargeRequestUnchangedWhenLongContextUnset(t *testing.T) {
	cfg := fourRouteCfg()
	cfg.Router.LongContext = ""
	p, model, err := Select(cfg, bigContentRequest("claude-3-7-sonnet-20250219"))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// An explicit "provider/model" selector must win over the think and
// long-context overrides just as it wins over Default/Background — even when the
// request is also huge (would otherwise trip LongContext).
func TestSelectExplicitSelectorBeatsOverrides(t *testing.T) {
	cfg := fourRouteCfg()
	req := bigContentRequest("bg-prov/bg-model-a")
	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "bg-prov" || model != "bg-model-a" {
		t.Errorf("explicit selector ignored: got (%q,%q), want (bg-prov, bg-model-a)", p.Name, model)
	}
}

// thinkingRequest builds a request carrying Anthropic's `thinking` field, the
// signal that activates Router.Think routing. thinking is the raw JSON to place
// in that field (e.g. `null` to prove an explicit null does NOT count).
func thinkingRequest(model, thinking string) *translate.AnthropicRequest {
	return &translate.AnthropicRequest{
		Model:    model,
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"think hard about this"`)}},
		Thinking: json.RawMessage(thinking),
	}
}

// TestRequestWantsThinking pins the seam directly: present-and-non-null fires,
// absent and explicit-null do not, and neither does a nil request.
func TestRequestWantsThinking(t *testing.T) {
	cases := []struct {
		name string
		req  *translate.AnthropicRequest
		want bool
	}{
		{"nil request", nil, false},
		{
			"absent thinking",
			&translate.AnthropicRequest{Model: "m"},
			false,
		},
		{
			"explicit null thinking",
			thinkingRequest("m", `null`),
			false,
		},
		{
			"explicit null with whitespace",
			thinkingRequest("m", " null "),
			false,
		},
		{
			"enabled thinking block",
			thinkingRequest("m", `{"type":"enabled","budget_tokens":1024}`),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestWantsThinking(tc.req); got != tc.want {
				t.Errorf("requestWantsThinking = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSelectThinkRouteFiresFromThinkingRequest proves the think path end-to-end
// through Select: a request that carries a real `thinking` block routes to
// Router.Think when it is configured.
func TestSelectThinkRouteFiresFromThinkingRequest(t *testing.T) {
	cfg := fourRouteCfg()
	req := thinkingRequest("claude-3-7-sonnet-20250219", `{"type":"enabled","budget_tokens":1024}`)
	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "think-prov" || model != "think-model" {
		t.Errorf("think route did not fire: got (%q,%q), want (think-prov, think-model)", p.Name, model)
	}
}

// Regression: with Router.Think UNSET, even a real thinking request stays on
// Default — behaviour is identical to before the override could fire.
func TestSelectThinkingRequestUnchangedWhenThinkUnset(t *testing.T) {
	cfg := fourRouteCfg()
	cfg.Router.Think = ""
	req := thinkingRequest("claude-3-7-sonnet-20250219", `{"type":"enabled","budget_tokens":1024}`)
	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("think must not fire when Router.Think is unset: got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// A NON-thinking request (no `thinking` field) must resolve exactly as before,
// even with Router.Think configured: the seam fires only on the client's own
// signal, never on an ordinary turn.
func TestSelectNonThinkingRequestUnchanged(t *testing.T) {
	cfg := fourRouteCfg()
	req := &translate.AnthropicRequest{
		Model:    "claude-3-7-sonnet-20250219",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"think hard about this"`)}},
	}
	if requestWantsThinking(req) {
		t.Fatal("requestWantsThinking must be false for a request that carries no thinking field")
	}
	p, model, err := Select(cfg, req)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name != "main-prov" || model != "main-model-a" {
		t.Errorf("non-thinking request must resolve to Default: got (%q,%q), want (main-prov, main-model-a)", p.Name, model)
	}
}

// TestEstimateTokenCount checks the coarse estimator over its inputs: it counts
// system, message content and tool bytes, excludes max_tokens, and clears the
// threshold only for genuinely large prompts.
func TestEstimateTokenCount(t *testing.T) {
	if got := estimateTokenCount(nil); got != 0 {
		t.Errorf("estimateTokenCount(nil) = %d, want 0", got)
	}

	small := &translate.AnthropicRequest{
		Model:     "m",
		MaxTokens: 1 << 20, // huge OUTPUT budget must NOT count as prompt input
		Messages:  []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	if got := estimateTokenCount(small); got > DefaultLongContextThreshold {
		t.Errorf("small request estimated %d tokens, should be below threshold %d", got, DefaultLongContextThreshold)
	}

	big := bigContentRequest("m")
	if got := estimateTokenCount(big); got <= DefaultLongContextThreshold {
		t.Errorf("big request estimated %d tokens, should exceed threshold %d", got, DefaultLongContextThreshold)
	}

	// System block and tool payloads contribute to the estimate too.
	withTools := &translate.AnthropicRequest{
		Model:  "m",
		System: json.RawMessage(`"` + strings.Repeat("s", 40) + `"`),
		Tools: []translate.AnthropicTool{
			{Name: "t", Description: strings.Repeat("d", 40), InputSchema: json.RawMessage(strings.Repeat("i", 40))},
		},
	}
	if got := estimateTokenCount(withTools); got == 0 {
		t.Error("estimateTokenCount ignored system/tool payloads")
	}
}
