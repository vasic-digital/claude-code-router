package translate

import (
	"encoding/json"
	"testing"
)

// decode is a small helper: parse a JSON object body into a generic map so a
// test can assert on individual fields regardless of key order.
func decodeObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not a JSON object: %v\n%s", err, b)
	}
	return m
}

// The defining property of the passthrough: fields the typed AnthropicRequest
// does NOT model must survive. If AnthropicPassthrough round-tripped through the
// struct it would silently drop these, sending a native upstream a lobotomised
// request.
func TestAnthropicPassthroughPreservesUnmodelledFields(t *testing.T) {
	raw := []byte(`{
		"model": "claude-sonnet-4",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": "hi"}],
		"system": "be terse",
		"top_k": 40,
		"tool_choice": {"type": "auto"},
		"thinking": {"type": "enabled", "budget_tokens": 1024},
		"metadata": {"user_id": "u-123", "big": 12345678901234567890}
	}`)

	out, err := AnthropicPassthrough(raw, Options{})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	m := decodeObj(t, out)

	for _, k := range []string{"top_k", "tool_choice", "thinking", "metadata", "system"} {
		if _, ok := m[k]; !ok {
			t.Errorf("passthrough dropped field %q (body: %s)", k, out)
		}
	}

	// A large integer id must survive byte-for-byte in the OUTPUT, not be
	// re-encoded through float64 into 1.2345...e+19 — the silent-corruption
	// failure UseNumber exists to prevent. Asserted on the raw output bytes:
	// decoding through a plain map[string]any (as decodeObj does) would itself
	// lose the literal to float64, so it cannot be the check here.
	if !containsBytes(out, "12345678901234567890") {
		t.Errorf("large integer literal not preserved in output:\n%s", out)
	}
}

func TestAnthropicPassthroughModelOverride(t *testing.T) {
	raw := []byte(`{"model":"as-sent","max_tokens":10,"messages":[]}`)

	// With an override, the router's chosen model replaces what the client sent.
	out, err := AnthropicPassthrough(raw, Options{Model: "routed-model"})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	if got := decodeObj(t, out)["model"]; got != "routed-model" {
		t.Errorf("model = %v, want routed-model", got)
	}

	// With no override, the client's model is left exactly as sent.
	out2, err := AnthropicPassthrough(raw, Options{})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	if got := decodeObj(t, out2)["model"]; got != "as-sent" {
		t.Errorf("model = %v, want as-sent (no override)", got)
	}
}

func TestAnthropicPassthroughCleanCache(t *testing.T) {
	raw := []byte(`{
		"model": "m", "max_tokens": 10,
		"system": [{"type": "text", "text": "sys", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}]}]
	}`)

	// CleanCache OFF: a real Anthropic endpoint accepts cache_control, so it must
	// be preserved untouched.
	kept, err := AnthropicPassthrough(raw, Options{})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	if !containsBytes(kept, "cache_control") {
		t.Errorf("cache_control was stripped with CleanCache off:\n%s", kept)
	}

	// CleanCache ON: for a compatible upstream that rejects it, every occurrence
	// is removed.
	stripped, err := AnthropicPassthrough(raw, Options{CleanCache: true})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	if containsBytes(stripped, "cache_control") {
		t.Errorf("cache_control survived CleanCache on:\n%s", stripped)
	}
}

// A tool input_schema property legitimately NAMED cache_control must NOT be
// deleted by CleanCache — the same schema-aware guarantee AnthropicToOpenAI
// relies on, exercised here for the passthrough path.
func TestAnthropicPassthroughCleanCacheKeepsSchemaProperty(t *testing.T) {
	raw := []byte(`{
		"model": "m", "max_tokens": 10, "messages": [],
		"tools": [{"name": "t", "input_schema": {
			"type": "object",
			"properties": {"cache_control": {"type": "string"}},
			"required": ["cache_control"]
		}}]
	}`)
	out, err := AnthropicPassthrough(raw, Options{CleanCache: true})
	if err != nil {
		t.Fatalf("AnthropicPassthrough: %v", err)
	}
	// The property (and thus the still-satisfiable "required") must remain.
	if !containsBytes(out, `"cache_control"`) {
		t.Errorf("CleanCache deleted a schema property named cache_control:\n%s", out)
	}
}

func TestAnthropicPassthroughRejectsNonObject(t *testing.T) {
	for _, body := range []string{`[]`, `"a string"`, `42`, `null`} {
		if _, err := AnthropicPassthrough([]byte(body), Options{}); err == nil {
			t.Errorf("AnthropicPassthrough(%s) = nil error, want a rejection", body)
		}
	}
	// Malformed JSON is likewise an error, not a silent empty body.
	if _, err := AnthropicPassthrough([]byte(`{not json`), Options{}); err == nil {
		t.Error("AnthropicPassthrough(malformed) = nil error, want a rejection")
	}
}

func containsBytes(b []byte, sub string) bool {
	s, n := string(b), len(sub)
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return true
		}
	}
	return false
}
