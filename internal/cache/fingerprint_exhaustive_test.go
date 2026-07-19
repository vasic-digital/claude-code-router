package cache

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// --- Property: determinism across a diverse set of requests -----------------
//
// Fingerprint (and SystemHash) must be a pure function of their inputs: calling
// them repeatedly on logically-equal requests yields byte-identical 64-hex
// digests, with no dependence on map iteration order, clock, or goroutine
// scheduling.
func TestFingerprint_DeterministicProperty(t *testing.T) {
	build := []func() *translate.AnthropicRequest{
		func() *translate.AnthropicRequest { return baseReq() },
		func() *translate.AnthropicRequest {
			r := baseReq()
			r.System = json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
			r.Messages = []translate.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"one"},{"type":"text","text":"two"}]`)},
				{Role: "assistant", Content: json.RawMessage(`"ack"`)},
				{Role: "user", Content: json.RawMessage(`"three"`)},
			}
			r.Tools = []translate.AnthropicTool{
				{Name: "z", Description: "last", InputSchema: json.RawMessage(`{"type":"object","properties":{"b":{"type":"number"},"a":{"type":"string"}}}`)},
				{Name: "a", Description: "first", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}
			tp := 0.5
			r.TopP = &tp
			r.StopSequences = []string{"STOP", "END"}
			return r
		},
	}

	for i, mk := range build {
		first := Fingerprint(mk(), "openai", "gpt-4o")
		firstScope := SystemHash(mk())
		for rep := 0; rep < 50; rep++ {
			if got := Fingerprint(mk(), "openai", "gpt-4o"); got != first {
				t.Fatalf("case %d rep %d: Fingerprint not deterministic:\n first=%s\n got  =%s", i, rep, first, got)
			}
			if got := SystemHash(mk()); got != firstScope {
				t.Fatalf("case %d rep %d: SystemHash not deterministic", i, rep)
			}
		}
		if len(first) != 64 {
			t.Fatalf("case %d: digest len=%d want 64", i, len(first))
		}
	}
}

// --- Property: invariant to JSON key order and cache_control, deeply nested --
//
// A re-ordered-but-equivalent request, and one carrying arbitrary nested
// cache_control hints, must map to the same key as its clean canonical form.
func TestFingerprint_DeepKeyOrderAndCacheControlInvariant(t *testing.T) {
	clean := baseReq()
	clean.System = json.RawMessage(`[{"type":"text","text":"sys"}]`)
	clean.Messages = []translate.AnthropicMessage{
		{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"hi","meta":{"a":1,"b":2}}]`)},
	}
	clean.Tools = []translate.AnthropicTool{
		{Name: "t", Description: "d", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"x":{"type":"string"},"y":{"type":"number"}}}`)},
	}

	noisy := baseReq()
	// Same system content, cache_control added.
	noisy.System = json.RawMessage(`[{"text":"sys","type":"text","cache_control":{"type":"ephemeral"}}]`)
	// Same message: keys reordered, nested object keys reordered, cache_control sprinkled at two depths.
	noisy.Messages = []translate.AnthropicMessage{
		{Role: "user", Content: json.RawMessage(
			`[{"cache_control":{"type":"ephemeral"},"meta":{"b":2,"a":1},"text":"hi","type":"text"}]`)},
	}
	// Same tool: schema property keys reordered, cache_control injected in the schema.
	noisy.Tools = []translate.AnthropicTool{
		{Name: "t", Description: "d", InputSchema: json.RawMessage(
			`{"properties":{"y":{"type":"number"},"x":{"type":"string"}},"cache_control":{"type":"ephemeral"},"type":"object"}`)},
	}

	if a, b := Fingerprint(clean, "openai", "gpt-4o"), Fingerprint(noisy, "openai", "gpt-4o"); a != b {
		t.Fatalf("fingerprint changed under deep reorder / nested cache_control:\n clean=%s\n noisy=%s", a, b)
	}
	if a, b := SystemHash(clean), SystemHash(noisy); a != b {
		t.Fatalf("SystemHash changed under deep reorder / nested cache_control:\n clean=%s\n noisy=%s", a, b)
	}
}

// --- Property: provider/model scope means no collision across a full matrix --
func TestFingerprint_ProviderModelMatrixNoCollision(t *testing.T) {
	providers := []string{"openai", "azure", "deepseek", "anthropic", "groq"}
	models := []string{"gpt-4o", "gpt-4o-mini", "o1", "claude-3-5-sonnet"}

	seen := map[string]string{}
	for _, p := range providers {
		for _, m := range models {
			fp := Fingerprint(baseReq(), p, m)
			if len(fp) != 64 {
				t.Fatalf("%s/%s: bad digest len %d", p, m, len(fp))
			}
			label := p + "/" + m
			if prev, dup := seen[fp]; dup {
				t.Fatalf("collision: %s collided with %s (same body, different provider/model must never collide)", label, prev)
			}
			seen[fp] = label
		}
	}
	if len(seen) != len(providers)*len(models) {
		t.Fatalf("expected %d distinct keys, got %d", len(providers)*len(models), len(seen))
	}
}

// --- Property: a change in system OR tools changes the SystemHash -----------
func TestSystemHash_SensitiveToSystemAndTools(t *testing.T) {
	base := SystemHash(baseReq())

	sysChanged := baseReq()
	sysChanged.System = json.RawMessage(`"you are a pirate"`)
	if SystemHash(sysChanged) == base {
		t.Fatal("system change must change SystemHash")
	}

	toolAdded := baseReq()
	toolAdded.Tools = []translate.AnthropicTool{{Name: "calc", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	if SystemHash(toolAdded) == base {
		t.Fatal("added tool must change SystemHash")
	}

	toolNameChanged := baseReq()
	toolNameChanged.Tools = []translate.AnthropicTool{{Name: "calc", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	toolSchemaChanged := baseReq()
	toolSchemaChanged.Tools = []translate.AnthropicTool{{Name: "calc", InputSchema: json.RawMessage(`{"type":"object","required":["x"]}`)}}
	if SystemHash(toolNameChanged) == SystemHash(toolSchemaChanged) {
		t.Fatal("tool schema change must change SystemHash")
	}

	// The same system/tools change must ALSO change the full Fingerprint.
	if Fingerprint(sysChanged, "openai", "gpt-4o") == Fingerprint(baseReq(), "openai", "gpt-4o") {
		t.Fatal("system change must change Fingerprint too")
	}
	if Fingerprint(toolAdded, "openai", "gpt-4o") == Fingerprint(baseReq(), "openai", "gpt-4o") {
		t.Fatal("added tool must change Fingerprint too")
	}
}

// --- Property: transport-shaped / sampling fields are ignored ---------------
//
// The fingerprint identifies the *cacheable protocol content*, so Stream,
// Temperature, and Thinking (none of which belong to the canonical key) must
// never shift it. TopP and StopSequences, which DO belong to the key, must.
func TestFingerprint_IgnoredAndSensitiveFields(t *testing.T) {
	base := Fingerprint(baseReq(), "openai", "gpt-4o")

	// Ignored fields.
	streamed := baseReq()
	streamed.Stream = true
	tmp := 0.9
	sampled := baseReq()
	sampled.Temperature = &tmp
	thinking := baseReq()
	thinking.Thinking = json.RawMessage(`{"type":"enabled","budget_tokens":1024}`)
	for name, r := range map[string]*translate.AnthropicRequest{
		"stream": streamed, "temperature": sampled, "thinking": thinking,
	} {
		if Fingerprint(r, "openai", "gpt-4o") != base {
			t.Fatalf("%s must NOT change the fingerprint", name)
		}
	}

	// Sensitive fields.
	tp := 0.3
	topp := baseReq()
	topp.TopP = &tp
	stop := baseReq()
	stop.StopSequences = []string{"###"}
	for name, r := range map[string]*translate.AnthropicRequest{
		"top_p": topp, "stop_sequences": stop,
	} {
		if Fingerprint(r, "openai", "gpt-4o") == base {
			t.Fatalf("%s MUST change the fingerprint", name)
		}
	}
}

// --- Secret-free / opaque-digest confirmation -------------------------------
//
// The fingerprint is derived from request CONTENT (provider name + model +
// canonical body) via SHA-256, never from a credential: the function has no key
// parameter, so a credential cannot enter the key by construction. This test
// confirms the complementary property — the digest is an opaque hash, leaking
// none of its plaintext inputs (provider name, model, system, or message text)
// as recoverable substrings. A cache key that echoed request text back would be
// an information-disclosure hazard; a SHA-256 digest is not.
func TestFingerprint_SecretFreeOpaqueDigest(t *testing.T) {
	r := baseReq()
	r.System = json.RawMessage(`"SYSTEM-PROMPT-SECRET-MARKER"`)
	r.Messages = []translate.AnthropicMessage{msg("user", `"USER-TURN-SECRET-MARKER sk-abcdef0123456789"`)}

	fp := Fingerprint(r, "provider-name-marker", "model-id-marker")
	scope := SystemHash(r)

	// Digest is pure lowercase hex, fixed width.
	if len(fp) != 64 {
		t.Fatalf("digest len=%d want 64", len(fp))
	}
	for _, c := range fp {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("digest contains non-hex rune %q", c)
		}
	}

	for _, secret := range []string{
		"SYSTEM-PROMPT-SECRET-MARKER", "USER-TURN-SECRET-MARKER",
		"sk-abcdef0123456789", "provider-name-marker", "model-id-marker",
	} {
		if strings.Contains(fp, secret) || strings.Contains(scope, secret) {
			t.Fatalf("digest leaked plaintext %q — must be an opaque hash", secret)
		}
	}

	// Two requests differing only in a credential-shaped message token DO hash
	// differently (it is content), but neither reveals the token — the point is
	// the key is opaque, not that credentials are silently dropped from content.
	r2 := baseReq()
	r2.System = json.RawMessage(`"SYSTEM-PROMPT-SECRET-MARKER"`)
	r2.Messages = []translate.AnthropicMessage{msg("user", `"USER-TURN-SECRET-MARKER sk-DIFFERENT9999"`)}
	fp2 := Fingerprint(r2, "provider-name-marker", "model-id-marker")
	if strings.Contains(fp2, "sk-DIFFERENT9999") {
		t.Fatal("digest leaked credential-shaped content")
	}
}

// --- Fuzz: Fingerprint / SystemHash never panic on arbitrary content --------
//
// The canonicalizer must survive invalid-JSON, non-UTF8, and deeply-adversarial
// raw content in every polymorphic slot (system, message content, tool schema)
// without panicking, always emitting a 64-hex digest.
func FuzzFingerprint(f *testing.F) {
	seeds := []string{
		``, `null`, `"str"`, `123`, `{}`, `[]`,
		`{"cache_control":{"type":"ephemeral"}}`,
		`[{"type":"text","text":"hi"}]`,
		`{"a":{"b":{"c":[1,2,3]}}}`,
		"\xff\xfe\x00not json",
		`{unbalanced`, `[1,2,`,
	}
	for _, s := range seeds {
		f.Add(s, "openai", "gpt-4o")
	}

	f.Fuzz(func(t *testing.T, blob, provider, model string) {
		raw := json.RawMessage(blob)
		r := &translate.AnthropicRequest{
			Model:     model,
			MaxTokens: 7,
			System:    raw,
			Messages: []translate.AnthropicMessage{
				{Role: "user", Content: raw},
				{Role: "assistant", Content: json.RawMessage(blob)},
			},
			Tools: []translate.AnthropicTool{
				{Name: provider, Description: blob, InputSchema: raw},
			},
		}

		fp := Fingerprint(r, provider, model)
		if len(fp) != 64 {
			t.Fatalf("Fingerprint digest len=%d want 64 (blob=%q)", len(fp), blob)
		}
		sh := SystemHash(r)
		if len(sh) != 64 {
			t.Fatalf("SystemHash digest len=%d want 64 (blob=%q)", len(sh), blob)
		}
		// Determinism must hold even for adversarial input.
		if fp2 := Fingerprint(r, provider, model); fp2 != fp {
			t.Fatalf("Fingerprint non-deterministic on blob=%q", blob)
		}
	})
}
