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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Protocol names the wire protocol a provider's upstream speaks.
const (
	// ProtocolOpenAI: the upstream accepts OpenAI chat-completions requests.
	// This is the default for every provider that does not say otherwise,
	// because it is what the overwhelming majority of upstreams — and every
	// provider the toolkit configures today — actually speak.
	ProtocolOpenAI = "openai"
	// ProtocolAnthropic: the upstream is an Anthropic-native Messages API
	// endpoint. A request to such a provider is sent UNCHANGED (Anthropic
	// shape in, Anthropic shape out) rather than translated to OpenAI shape by
	// internal/translate.AnthropicToOpenAI.
	ProtocolAnthropic = "anthropic"
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
	// Protocol is the wire protocol the upstream speaks: "openai" (the
	// default) or "anthropic". It is OPTIONAL: an ABSENT protocol behaves
	// exactly as before this field existed — the provider is treated as an
	// OpenAI chat-completions upstream — so every config already on disk (none
	// of which carries this key) keeps working byte-for-byte. See
	// ResolvedProtocol for how an absent value is inferred, and Validate for
	// the accepted set.
	Protocol string `json:"protocol,omitempty"`
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

// ResolvedProtocol reports the wire protocol to use for this provider.
//
// An EXPLICIT Protocol field always wins. When it is absent, the protocol is
// INFERRED from api_base_url purely as a convenience for existing configs: a
// URL that unmistakably names an Anthropic endpoint — the api.anthropic.com
// host (or any *.anthropic.com), or a URL carrying an "/anthropic" path
// segment (the toolkit's proxy convention for a native base) — resolves to
// "anthropic"; everything else resolves to "openai".
//
// Inference is deliberately conservative so that no ordinary OpenAI-shaped
// provider is ever silently reclassified: it yields "anthropic" only for a URL
// that could not plausibly be anything else. An operator who needs certainty
// (or whose native endpoint the heuristic does not recognise) sets Protocol
// explicitly, which always overrides inference.
func (p *Provider) ResolvedProtocol() string {
	if p.Protocol != "" {
		return p.Protocol
	}
	if isAnthropicNativeURL(p.APIBaseURL) {
		return ProtocolAnthropic
	}
	return ProtocolOpenAI
}

// isAnthropicNativeURL reports whether raw unmistakably names an Anthropic
// Messages endpoint. It parses rather than substring-matches so that a query
// string or an unrelated host such as "api.anthropic-proxy.example.com" (which
// is NOT *.anthropic.com) does not trigger a false positive.
func isAnthropicNativeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "anthropic.com" || strings.HasSuffix(host, ".anthropic.com") {
		return true
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "anthropic" {
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
	// CrossProviderFallback, when true, lets the gateway try another configured
	// provider that ALSO serves the routed model if the primary fails with a
	// RETRYABLE class (5xx / 429 / transport) after its same-provider retries.
	// A Terminal failure (400/401/404/...) never falls back. Default false =>
	// byte-identical single-provider behaviour. Applies only to non-streaming
	// OpenAI-provider requests (streaming cannot fall back mid-stream; an
	// Anthropic-native primary is not eligible).
	CrossProviderFallback bool `json:"crossProviderFallback,omitempty"`
	// Fallback is an optional explicit ordered "provider,model" chain tried
	// (in order) before the auto-discovered same-model providers. Ignored
	// unless CrossProviderFallback is true. Each entry must name a configured
	// provider.
	Fallback []string `json:"fallback,omitempty"`
}

// Config is the whole configuration document.
type Config struct {
	Providers []Provider `json:"Providers"`
	Router    Route      `json:"Router"`
	// Cache configures the optional response cache (see internal/cache). It is
	// OMITEMPTY and a POINTER: an ABSENT/nil Cache means caching is disabled —
	// exactly today's behaviour — so every config already on disk (none of
	// which carries this key) serialises and behaves byte-for-byte unchanged.
	Cache *CacheConfig `json:"Cache,omitempty"`
}

// CacheConfig configures the gateway's response cache. The whole feature is
// off unless Enabled is true; a nil *CacheConfig on Config is likewise off.
type CacheConfig struct {
	// Enabled turns the response cache on. False (the zero value) leaves the
	// gateway's request path byte-identical to a build with no cache at all.
	Enabled bool `json:"enabled"`
	// Backend selects the store: "" or "memory" (in-process LRU, the default)
	// or "sqlite" (persistent, survives restart).
	Backend string `json:"backend,omitempty"`
	// Path is the SQLite database path. REQUIRED when Backend == "sqlite";
	// ignored for the memory backend.
	Path string `json:"path,omitempty"`
	// TTLSeconds bounds an entry's life. 0 means "no expiry".
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// MaxEntries bounds the in-memory LRU. 0 means "use a sane default".
	MaxEntries int `json:"max_entries,omitempty"`
	// AllowToolResponses opts the response-side gate into caching tool-call
	// responses (off by default, since a tool answer depends on live state).
	AllowToolResponses bool `json:"allow_tool_responses,omitempty"`
}

// Cache validation errors, exported so callers (and tests) can match them with
// errors.Is rather than string-comparing.
var (
	// ErrCacheBackendUnknown is returned when an enabled cache names a backend
	// other than "", "memory", or "sqlite".
	ErrCacheBackendUnknown = errors.New(`cache backend must be "", "memory", or "sqlite"`)
	// ErrCacheSQLitePathRequired is returned when an enabled cache selects the
	// sqlite backend without a path.
	ErrCacheSQLitePathRequired = errors.New(`cache path is required when backend is "sqlite"`)
)

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
		// An empty protocol means "infer" (see ResolvedProtocol); anything
		// other than the two known values is a typo we must reject loudly
		// rather than silently treat as OpenAI — silently mishandling a
		// misspelled "anthropic " is exactly the failure this field prevents.
		switch p.Protocol {
		case "", ProtocolOpenAI, ProtocolAnthropic:
		default:
			return fmt.Errorf("provider %q: protocol %q is not one of %q, %q",
				p.Name, p.Protocol, ProtocolOpenAI, ProtocolAnthropic)
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
	// Each explicit fallback entry must be a "provider,model" selector naming a
	// configured provider — a typo here would otherwise surface only at request
	// time as a skipped attempt.
	for i, f := range c.Router.Fallback {
		name, _, err := SplitRoute(f)
		if err != nil {
			return fmt.Errorf("Router.fallback[%d]: %w", i, err)
		}
		if !seen[name] {
			return fmt.Errorf("Router.fallback[%d] references unknown provider %q", i, name)
		}
	}
	// An absent/nil Cache is always valid (caching disabled). A disabled Cache
	// is likewise unconstrained — its fields are inert until Enabled flips on.
	if c.Cache != nil && c.Cache.Enabled {
		switch c.Cache.Backend {
		case "", "memory", "sqlite":
		default:
			return fmt.Errorf("Cache.backend %q: %w", c.Cache.Backend, ErrCacheBackendUnknown)
		}
		if c.Cache.Backend == "sqlite" && c.Cache.Path == "" {
			return fmt.Errorf("Cache: %w", ErrCacheSQLitePathRequired)
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
