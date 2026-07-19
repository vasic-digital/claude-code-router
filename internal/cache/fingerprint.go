package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// Fingerprint is a stable, secret-free cache key for a routed request.
//
// It NEVER includes the provider API key — only the provider NAME and the
// resolved model — mirroring the no-secret-in-derived-data rule the proxy and
// logging code enforce. The key is:
//
//	sha256( canonical-JSON( provider, model, max_tokens, system, messages,
//	                        tools, top_p, stop_sequences ) )
//
// Canonicalisation makes the key invariant to noise that must not change
// identity, and sensitive to everything that must:
//
//   - object key order inside system / message content / tool schemas is
//     normalised (json.Marshal sorts map keys), so a re-ordered but equivalent
//     request maps to the same key;
//   - Anthropic-only cache_control metadata is stripped recursively, so a
//     request with and without prompt-cache hints share one key;
//   - the streaming flag is dropped (a cached body is protocol-shaped, not
//     transport-shaped);
//   - number literals keep full fidelity via json.Number, so re-encoding does
//     not shift the key.
//
// It is sensitive to model, provider, system, tools, and messages, so two
// different providers/models — or two agents with different instructions —
// never collide.
func Fingerprint(req *translate.AnthropicRequest, providerName, model string) string {
	type canonReq struct {
		Provider string   `json:"p"`
		Model    string   `json:"m"`
		MaxTok   int      `json:"mt"`
		System   any      `json:"s,omitempty"`
		Messages []any    `json:"msg"`
		Tools    []any    `json:"tools,omitempty"`
		TopP     *float64 `json:"top_p,omitempty"`
		Stop     []string `json:"stop,omitempty"`
	}

	c := canonReq{
		Provider: providerName,
		Model:    model,
		MaxTok:   req.MaxTokens,
		System:   canonicalizeRaw(req.System),
		TopP:     req.TopP,
		Stop:     req.StopSequences,
	}
	for _, m := range req.Messages {
		c.Messages = append(c.Messages, map[string]any{
			"role":    m.Role,
			"content": canonicalizeRaw(m.Content),
		})
	}
	for _, t := range req.Tools {
		c.Tools = append(c.Tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"schema":      canonicalizeRaw(t.InputSchema),
		})
	}

	b, _ := json.Marshal(c) // map keys are sorted by encoding/json → stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SystemHash returns sha256(canonical system + tools), the scope identity used
// to isolate the semantic tier's candidate set (and stored on an Entry). Two
// requests with different system prompts or different tools get different
// scopes and therefore never cross-serve.
func SystemHash(req *translate.AnthropicRequest) string {
	scope := map[string]any{
		"s": canonicalizeRaw(req.System),
	}
	var tools []any
	for _, t := range req.Tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"schema":      canonicalizeRaw(t.InputSchema),
		})
	}
	scope["tools"] = tools
	b, _ := json.Marshal(scope)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalizeRaw decodes a raw JSON value with full number fidelity and
// removes every cache_control key from the tree, returning a generic value
// whose re-marshalling is stable (encoding/json sorts map keys). A nil or empty
// raw value canonicalises to nil so it is omitted from the key.
func canonicalizeRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		// Not valid JSON on its own (should not happen for a well-formed
		// request) — fall back to the raw string so identity is still stable.
		return string(raw)
	}
	return stripCacheControl(v)
}

// stripCacheControl recursively removes cache_control keys, mirroring
// translate.StripCacheControl's discipline but scoped to this package so it does
// not depend on an unexported helper.
func stripCacheControl(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, sub := range t {
			t[k] = stripCacheControl(sub)
		}
		return t
	case []any:
		for i, sub := range t {
			t[i] = stripCacheControl(sub)
		}
		return t
	default:
		return v
	}
}
