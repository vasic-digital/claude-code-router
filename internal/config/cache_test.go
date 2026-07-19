package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// baseValidConfig is a minimal valid config the cache tests attach a Cache to,
// so a validation failure can only be about the Cache block itself.
func baseValidConfig() *Config {
	return &Config{
		Providers: []Provider{{Name: "p1", APIBaseURL: "https://up.example/v1/chat/completions"}},
		Router:    Route{Default: "p1,m1"},
	}
}

// An absent (nil) Cache is always valid and, crucially, serialises to a config
// byte-identical to one written before the field existed — no "Cache" key.
func TestCacheAbsentIsValidAndOmitted(t *testing.T) {
	c := baseValidConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("nil Cache must validate, got: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "Cache") {
		t.Errorf("a nil Cache must be OMITTED from JSON (byte-identical to pre-cache configs); got: %s", b)
	}
}

// A disabled Cache is valid regardless of the other (inert) fields.
func TestCacheDisabledIsAlwaysValid(t *testing.T) {
	c := baseValidConfig()
	c.Cache = &CacheConfig{Enabled: false, Backend: "bogus", Path: ""}
	if err := c.Validate(); err != nil {
		t.Fatalf("a disabled Cache must validate no matter its fields, got: %v", err)
	}
}

func TestCacheEnabledMemoryBackendsValid(t *testing.T) {
	for _, backend := range []string{"", "memory"} {
		c := baseValidConfig()
		c.Cache = &CacheConfig{Enabled: true, Backend: backend}
		if err := c.Validate(); err != nil {
			t.Errorf("enabled memory backend %q must validate, got: %v", backend, err)
		}
	}
}

func TestCacheEnabledSQLiteRequiresPath(t *testing.T) {
	c := baseValidConfig()
	c.Cache = &CacheConfig{Enabled: true, Backend: "sqlite"} // no Path
	err := c.Validate()
	if err == nil {
		t.Fatal("enabled sqlite backend without a path must fail validation")
	}
	if !errors.Is(err, ErrCacheSQLitePathRequired) {
		t.Errorf("error must wrap ErrCacheSQLitePathRequired, got: %v", err)
	}

	// With a path it is valid.
	c.Cache.Path = "/tmp/ccr-cache.db"
	if err := c.Validate(); err != nil {
		t.Errorf("enabled sqlite backend WITH a path must validate, got: %v", err)
	}
}

func TestCacheEnabledBadBackendErrors(t *testing.T) {
	c := baseValidConfig()
	c.Cache = &CacheConfig{Enabled: true, Backend: "redis"}
	err := c.Validate()
	if err == nil {
		t.Fatal("an unknown backend on an enabled cache must fail validation")
	}
	if !errors.Is(err, ErrCacheBackendUnknown) {
		t.Errorf("error must wrap ErrCacheBackendUnknown, got: %v", err)
	}
}

// The Cache block round-trips through Load (parse + Validate) from the exact
// on-disk JSON shape the schema documents.
func TestCacheLoadsFromDiskShape(t *testing.T) {
	const shape = `{
	  "Providers": [
	    {"name": "p1", "api_base_url": "https://up.example/v1/chat/completions", "api_key": "k", "models": ["m1"]}
	  ],
	  "Router": {"default": "p1,m1"},
	  "Cache": {"enabled": true, "backend": "sqlite", "path": "/var/lib/ccr/cache.db", "ttl_seconds": 3600, "max_entries": 2048, "allow_tool_responses": true}
	}`
	c, err := Load(writeTemp(t, shape))
	if err != nil {
		t.Fatalf("Load with a Cache block failed: %v", err)
	}
	if c.Cache == nil || !c.Cache.Enabled {
		t.Fatal("Cache did not parse")
	}
	if c.Cache.Backend != "sqlite" || c.Cache.Path != "/var/lib/ccr/cache.db" {
		t.Errorf("Cache fields wrong: %+v", c.Cache)
	}
	if c.Cache.TTLSeconds != 3600 || c.Cache.MaxEntries != 2048 || !c.Cache.AllowToolResponses {
		t.Errorf("Cache scalar fields wrong: %+v", c.Cache)
	}
}
