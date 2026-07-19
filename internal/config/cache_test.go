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

// Semantic without a threshold is valid: the 0 (absent) threshold means "use
// the built-in default" at BuildCache time, so no cross-serve happens on a bad
// value.
func TestCacheSemanticWithoutThresholdValid(t *testing.T) {
	c := baseValidConfig()
	c.Cache = &CacheConfig{Enabled: true, Semantic: true}
	if err := c.Validate(); err != nil {
		t.Errorf("Semantic with no threshold (uses default) must validate, got: %v", err)
	}
}

// A threshold inside (0,1] is valid, whether or not Semantic itself is set (the
// field is a plain cosine floor).
func TestCacheSemanticThresholdInRangeValid(t *testing.T) {
	for _, thr := range []float64{0.01, 0.5, 0.85, 1.0} {
		c := baseValidConfig()
		c.Cache = &CacheConfig{Enabled: true, Semantic: true, SemanticThreshold: thr}
		if err := c.Validate(); err != nil {
			t.Errorf("semantic_threshold %v must validate, got: %v", thr, err)
		}
	}
}

// A non-zero threshold outside (0,1] is rejected with the named error.
func TestCacheSemanticThresholdOutOfRangeErrors(t *testing.T) {
	for _, thr := range []float64{-0.1, 1.01, 2.0} {
		c := baseValidConfig()
		c.Cache = &CacheConfig{Enabled: true, Semantic: true, SemanticThreshold: thr}
		err := c.Validate()
		if err == nil {
			t.Fatalf("semantic_threshold %v must fail validation", thr)
		}
		if !errors.Is(err, ErrCacheSemanticThresholdRange) {
			t.Errorf("semantic_threshold %v: error must wrap ErrCacheSemanticThresholdRange, got: %v", thr, err)
		}
	}
}

// A disabled cache never validates the threshold — its fields are inert.
func TestCacheSemanticThresholdInertWhenDisabled(t *testing.T) {
	c := baseValidConfig()
	c.Cache = &CacheConfig{Enabled: false, SemanticThreshold: 9.0}
	if err := c.Validate(); err != nil {
		t.Errorf("a disabled cache must not validate its (inert) threshold, got: %v", err)
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
