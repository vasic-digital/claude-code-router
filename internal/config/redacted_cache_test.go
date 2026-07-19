package config

import "testing"

// Regression: config show (config.Redacted) dropped the whole Cache block,
// hiding the v0.3.0 response-cache settings from an operator. Redacted must
// carry Cache through (it holds no credential), still redact api_keys, and stay
// a genuine deep copy.
func TestRedactedPreservesCache(t *testing.T) {
	c := &Config{
		Providers: []Provider{{Name: "p", APIBaseURL: "https://x/y", APIKey: "sk-secret", Models: []string{"m"}}},
		Router:    Route{Default: "p,m", CrossProviderFallback: true, Fallback: []string{"p,m"}},
		Cache:     &CacheConfig{Enabled: true, Backend: "memory", TTLSeconds: 300, MaxEntries: 500},
	}
	r := Redacted(c)

	if r.Cache == nil {
		t.Fatal("Redacted dropped the Cache block (config show would hide it)")
	}
	if !r.Cache.Enabled || r.Cache.Backend != "memory" || r.Cache.TTLSeconds != 300 || r.Cache.MaxEntries != 500 {
		t.Errorf("Cache not preserved faithfully: %+v", r.Cache)
	}
	if r.Providers[0].APIKey != RedactedMarker {
		t.Errorf("api_key not redacted: %q", r.Providers[0].APIKey)
	}
	// Router (value struct) carries the new fallback fields through.
	if !r.Router.CrossProviderFallback || len(r.Router.Fallback) != 1 {
		t.Errorf("Router fallback fields not preserved: %+v", r.Router)
	}
	// Deep copy: mutating the redacted copy must not touch the original.
	r.Cache.Enabled = false
	if !c.Cache.Enabled {
		t.Error("Redacted shared the Cache pointer instead of deep-copying it")
	}
	// A nil Cache stays nil (no spurious empty block in show).
	if Redacted(&Config{}).Cache != nil {
		t.Error("nil Cache should remain nil after Redacted")
	}
}
