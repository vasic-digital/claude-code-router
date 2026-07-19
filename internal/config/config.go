// Package config loads and validates the router's on-disk configuration.
//
// The file format is deliberately identical to the Node implementation's
// ~/.claude-code-router/config.json, because existing tooling (notably the
// claude_toolkit provider aliases) already writes that shape at launch time:
//
//	{
//	  "Providers": [
//	    {"name": "...", "api_base_url": "...", "api_key": "...",
//	     "models": ["strong", "fast"],
//	     "transformer": {"use": ["cleancache", "streamoptions"]}}
//	  ],
//	  "Router": {"default": "<provider>,<model>", "background": "<provider>,<model>"}
//	}
//
// Capitalised keys ("Providers", "Router") are not idiomatic Go/JSON, but they
// are the established wire format; renaming them would silently break every
// installed config, so they are preserved exactly.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Provider is one upstream model endpoint.
type Provider struct {
	Name string `json:"name"`
	// APIBaseURL is the FULL chat-completions URL, not a base to append to.
	// The Node implementation and the toolkit both store the complete
	// endpoint (e.g. https://api.deepseek.com/chat/completions), so the
	// gateway must not re-append a path.
	APIBaseURL  string       `json:"api_base_url"`
	APIKey      string       `json:"api_key"`
	Models      []string     `json:"models"`
	Transformer *Transformer `json:"transformer,omitempty"`
}

// Transformer names the request/response fixups to apply for a provider.
// Known values: "cleancache" (strip Anthropic cache_control blocks that
// OpenAI-shaped upstreams reject) and "streamoptions" (add
// stream_options.include_usage so token usage is reported when streaming).
type Transformer struct {
	Use []string `json:"use,omitempty"`
}

// Has reports whether transformer t is enabled for this provider.
func (p *Provider) Has(t string) bool {
	if p.Transformer == nil {
		return false
	}
	for _, u := range p.Transformer.Use {
		if u == t {
			return true
		}
	}
	return false
}

// Route selects a provider+model pair, serialised as "provider,model".
type Route struct {
	Default     string `json:"default,omitempty"`
	Background  string `json:"background,omitempty"`
	Think       string `json:"think,omitempty"`
	LongContext string `json:"longContext,omitempty"`
}

// Config is the whole configuration document.
type Config struct {
	Providers []Provider `json:"Providers"`
	Router    Route      `json:"Router"`
}

// Dir returns the platform configuration directory, matching the Node
// implementation: ~/.claude-code-router on Unix, %APPDATA% on Windows.
func Dir() string {
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "claude-code-router")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude-code-router"
	}
	return filepath.Join(home, ".claude-code-router")
}

// Path returns the configuration file path.
func Path() string { return filepath.Join(Dir(), "config.json") }

// Load reads and validates the configuration at path.
//
// A missing file is NOT an error: it yields an empty, valid config so the
// gateway can start and report "no providers configured" rather than refusing
// to boot. Malformed JSON IS an error — silently continuing with a partial
// config is how requests get routed to the wrong upstream.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

// Validate checks structural invariants that would otherwise surface as
// confusing upstream errors at request time.
func (c *Config) Validate() error {
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("Providers[%d]: name is required", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("Providers[%d]: duplicate provider name %q", i, p.Name)
		}
		seen[p.Name] = true
		if p.APIBaseURL == "" {
			return fmt.Errorf("provider %q: api_base_url is required", p.Name)
		}
		if !strings.HasPrefix(p.APIBaseURL, "http://") && !strings.HasPrefix(p.APIBaseURL, "https://") {
			return fmt.Errorf("provider %q: api_base_url must be http(s), got %q", p.Name, p.APIBaseURL)
		}
	}
	for label, r := range map[string]string{
		"default": c.Router.Default, "background": c.Router.Background,
		"think": c.Router.Think, "longContext": c.Router.LongContext,
	} {
		if r == "" {
			continue
		}
		name, _, err := SplitRoute(r)
		if err != nil {
			return fmt.Errorf("Router.%s: %w", label, err)
		}
		if !seen[name] {
			return fmt.Errorf("Router.%s references unknown provider %q", label, name)
		}
	}
	return nil
}

// SplitRoute parses a "provider,model" route string.
//
// The model half may itself contain commas in principle, so the split is on
// the FIRST comma only; everything after it is the model id.
func SplitRoute(route string) (provider, model string, err error) {
	i := strings.Index(route, ",")
	if i < 0 {
		return "", "", fmt.Errorf("route %q must be \"provider,model\"", route)
	}
	provider = strings.TrimSpace(route[:i])
	model = strings.TrimSpace(route[i+1:])
	if provider == "" || model == "" {
		return "", "", fmt.Errorf("route %q must be \"provider,model\"", route)
	}
	return provider, model, nil
}

// ProviderByName returns the named provider, or nil.
func (c *Config) ProviderByName(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}
