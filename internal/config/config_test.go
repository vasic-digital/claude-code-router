package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// The exact shape claude_toolkit's cma_run_provider writes at launch. If this
// test ever fails, the Go gateway has diverged from the config the toolkit
// actually produces, and every provider alias would break.
const toolkitShape = `{
  "Providers": [
    {
      "name": "chutes",
      "api_base_url": "https://llm.chutes.ai/v1/chat/completions",
      "api_key": "sk-test",
      "models": ["zai-org/GLM-5.2-TEE", "Qwen/Qwen3.6-27B-TEE"],
      "transformer": {"use": ["cleancache", "streamoptions"]}
    }
  ],
  "Router": {
    "default": "chutes,zai-org/GLM-5.2-TEE",
    "background": "chutes,Qwen/Qwen3.6-27B-TEE"
  }
}`

func TestLoadToolkitShape(t *testing.T) {
	c, err := Load(writeTemp(t, toolkitShape))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(c.Providers))
	}
	p := c.Providers[0]
	if p.Name != "chutes" {
		t.Errorf("name = %q, want chutes", p.Name)
	}
	// The stored URL is the COMPLETE endpoint; the gateway must not append.
	if p.APIBaseURL != "https://llm.chutes.ai/v1/chat/completions" {
		t.Errorf("api_base_url = %q", p.APIBaseURL)
	}
	if !p.Has("cleancache") || !p.Has("streamoptions") {
		t.Errorf("transformers not parsed: %+v", p.Transformer)
	}
	if p.Has("nonexistent") {
		t.Error("Has() returned true for an absent transformer")
	}
	prov, model, err := SplitRoute(c.Router.Default)
	if err != nil {
		t.Fatalf("SplitRoute: %v", err)
	}
	if prov != "chutes" || model != "zai-org/GLM-5.2-TEE" {
		t.Errorf("route = (%q,%q)", prov, model)
	}
}

// A missing config must not be fatal — the gateway should boot and report that
// nothing is configured, rather than refusing to start.
func TestLoadMissingFileIsEmptyNotError(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(c.Providers) != 0 {
		t.Errorf("want empty config, got %d providers", len(c.Providers))
	}
}

// Malformed JSON must be loud. Silently continuing risks routing to the wrong
// upstream with a half-parsed config.
func TestLoadMalformedIsError(t *testing.T) {
	if _, err := Load(writeTemp(t, `{"Providers": [`)); err == nil {
		t.Fatal("malformed JSON must return an error")
	}
}

func TestValidateRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"missing name":     `{"Providers":[{"api_base_url":"https://a/b"}]}`,
		"missing base url": `{"Providers":[{"name":"a"}]}`,
		"non-http scheme":  `{"Providers":[{"name":"a","api_base_url":"ftp://a/b"}]}`,
		"duplicate name": `{"Providers":[
			{"name":"a","api_base_url":"https://a/b"},
			{"name":"a","api_base_url":"https://c/d"}]}`,
		"route to unknown provider": `{"Providers":[{"name":"a","api_base_url":"https://a/b"}],
			"Router":{"default":"ghost,m"}}`,
		"route without comma": `{"Providers":[{"name":"a","api_base_url":"https://a/b"}],
			"Router":{"default":"a"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, body)); err == nil {
				t.Errorf("expected an error for %s", name)
			}
		})
	}
}

func TestSplitRoute(t *testing.T) {
	// A model id may contain commas; only the first comma separates.
	p, m, err := SplitRoute("prov,vendor/model,v2")
	if err != nil {
		t.Fatalf("SplitRoute: %v", err)
	}
	if p != "prov" || m != "vendor/model,v2" {
		t.Errorf("got (%q,%q), want (prov, vendor/model,v2)", p, m)
	}
	for _, bad := range []string{"", "noComma", ",model", "prov,"} {
		if _, _, err := SplitRoute(bad); err == nil {
			t.Errorf("SplitRoute(%q) should fail", bad)
		}
	}
}

func TestProviderByName(t *testing.T) {
	c, err := Load(writeTemp(t, toolkitShape))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.ProviderByName("chutes"); got == nil {
		t.Error("ProviderByName(chutes) = nil")
	}
	if got := c.ProviderByName("absent"); got != nil {
		t.Error("ProviderByName(absent) should be nil")
	}
}

// Live corroboration: if a real config exists on this machine, it must parse
// and validate. Skips cleanly on a machine without one, so the suite stays
// hermetic by default while still catching real-world drift when present.
func TestLoadRealConfigIfPresent(t *testing.T) {
	p := Path()
	if _, err := os.Stat(p); err != nil {
		t.Skipf("no live config at %s", p)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("live config at %s failed to load: %v", p, err)
	}
	t.Logf("live config OK: %d providers, default route %q",
		len(c.Providers), c.Router.Default)
}
