package cache

import (
	"encoding/json"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func msg(role, content string) translate.AnthropicMessage {
	return translate.AnthropicMessage{Role: role, Content: json.RawMessage(content)}
}

func baseReq() *translate.AnthropicRequest {
	return &translate.AnthropicRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		System:    json.RawMessage(`"you are a helpful assistant"`),
		Messages: []translate.AnthropicMessage{
			msg("user", `"what is 2+2?"`),
		},
	}
}

// same request -> same key (determinism)
func TestFingerprint_Deterministic(t *testing.T) {
	a := Fingerprint(baseReq(), "openai", "gpt-4o")
	b := Fingerprint(baseReq(), "openai", "gpt-4o")
	if a != b {
		t.Fatalf("fingerprint not deterministic:\n a=%s\n b=%s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-hex-char sha256, got %d chars", len(a))
	}
}

// field reorder inside content objects must not change the key, and neither must
// Anthropic-only cache_control noise.
func TestFingerprint_KeyOrderAndCacheControlInvariant(t *testing.T) {
	r1 := baseReq()
	r1.Messages = []translate.AnthropicMessage{
		{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]`)},
	}
	r2 := baseReq()
	// same block, keys reordered, cache_control absent
	r2.Messages = []translate.AnthropicMessage{
		{Role: "user", Content: json.RawMessage(
			`[{"text":"hi","type":"text"}]`)},
	}
	if Fingerprint(r1, "openai", "gpt-4o") != Fingerprint(r2, "openai", "gpt-4o") {
		t.Fatal("fingerprint changed under key reorder / cache_control noise; must be invariant")
	}
}

// different model, provider, system, tools, or messages MUST change the key
// (scope sensitivity / no collision).
func TestFingerprint_ScopeSensitive(t *testing.T) {
	base := Fingerprint(baseReq(), "openai", "gpt-4o")

	cases := map[string]func() (string, string, *translate.AnthropicRequest){
		"different model": func() (string, string, *translate.AnthropicRequest) {
			return "openai", "gpt-4o-mini", baseReq()
		},
		"different provider": func() (string, string, *translate.AnthropicRequest) {
			return "azure", "gpt-4o", baseReq()
		},
		"different system": func() (string, string, *translate.AnthropicRequest) {
			r := baseReq()
			r.System = json.RawMessage(`"you are a pirate"`)
			return "openai", "gpt-4o", r
		},
		"different message": func() (string, string, *translate.AnthropicRequest) {
			r := baseReq()
			r.Messages = []translate.AnthropicMessage{msg("user", `"what is 3+3?"`)}
			return "openai", "gpt-4o", r
		},
		"added tool": func() (string, string, *translate.AnthropicRequest) {
			r := baseReq()
			r.Tools = []translate.AnthropicTool{{Name: "calc", InputSchema: json.RawMessage(`{"type":"object"}`)}}
			return "openai", "gpt-4o", r
		},
		"different max_tokens": func() (string, string, *translate.AnthropicRequest) {
			r := baseReq()
			r.MaxTokens = 2048
			return "openai", "gpt-4o", r
		},
	}

	seen := map[string]string{base: "base"}
	for name, f := range cases {
		p, m, r := f()
		got := Fingerprint(r, p, m)
		if got == base {
			t.Errorf("%s: fingerprint collided with base (must differ)", name)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s: fingerprint collided with %s", name, other)
		}
		seen[got] = name
	}
}

// two different providers/models never produce the same key for the same body.
func TestFingerprint_NoProviderCollision(t *testing.T) {
	r := baseReq()
	openai := Fingerprint(r, "openai", "gpt-4o")
	azure := Fingerprint(r, "azure", "gpt-4o")
	deepseek := Fingerprint(r, "deepseek", "gpt-4o")
	if openai == azure || openai == deepseek || azure == deepseek {
		t.Fatal("provider scope leaked: different providers must never collide")
	}
}

// the streaming flag must not affect the key (a cached body is transport-agnostic).
func TestFingerprint_StreamFlagIgnored(t *testing.T) {
	r1 := baseReq()
	r2 := baseReq()
	r2.Stream = true
	if Fingerprint(r1, "openai", "gpt-4o") != Fingerprint(r2, "openai", "gpt-4o") {
		t.Fatal("stream flag must not change the fingerprint")
	}
}

// SystemHash isolates scope by system prompt + tools.
func TestSystemHash_Scope(t *testing.T) {
	a := baseReq()
	b := baseReq()
	b.System = json.RawMessage(`"different instructions"`)
	if SystemHash(a) == SystemHash(b) {
		t.Fatal("different system prompts must yield different scope hashes")
	}
	// same content, cache_control noise -> same scope
	c := baseReq()
	c.System = json.RawMessage(`"you are a helpful assistant"`)
	if SystemHash(a) != SystemHash(c) {
		t.Fatal("identical system prompts must yield identical scope hashes")
	}
}
