package challenges

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "deeply-nested-tool-schema",
		TestName: "TestChallenge_DeeplyNestedToolSchema",
		Hypothesis: "AnthropicTool.InputSchema is typed json.RawMessage, so " +
			"AnthropicToOpenAI never unmarshals (walks) it -- it is copied verbatim into " +
			"OpenAIFnDef.Parameters. A pathologically deep JSON-schema tree therefore cannot " +
			"trigger stack-overflow-by-recursive-descent AT THE TRANSLATION LAYER, no matter how " +
			"deep, because the translator never recurses into it at all.",
		ExpectedSafeOutcome: "A tool schema nested far beyond any structure a real client would " +
			"send (tens of thousands of levels) converts instantly and byte-identically, because " +
			"it is never parsed by this code path.",
	})
}

func TestChallenge_DeeplyNestedToolSchema(t *testing.T) {
	// Constructed as a Go []byte directly (not produced by parsing JSON text),
	// which is exactly how it would arrive in practice: the gateway's own
	// inbound json.Unmarshal of the whole HTTP request body is a SEPARATE
	// concern (bounded by Go's own encoding/json 10000-level depth guard --
	// see TestChallenge_DeepJSONRecursionDepthBoundary) from what
	// AnthropicToOpenAI does with an already-decoded RawMessage field.
	const depth = 50_000
	var b strings.Builder
	b.Grow(depth*6 + 8)
	for i := 0; i < depth; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString(`"leaf"`)
	for i := 0; i < depth; i++ {
		b.WriteByte('}')
	}
	deepSchema := json.RawMessage(b.String())

	req := &translate.AnthropicRequest{
		Model:    "m",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []translate.AnthropicTool{{Name: "deep", InputSchema: deepSchema}},
	}

	done := make(chan struct{})
	var out *translate.OpenAIRequest
	var convertErr error
	go func() {
		defer close(done)
		out, convertErr = translate.AnthropicToOpenAI(req, translate.Options{})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("conversion of a deeply-nested (but never re-parsed) tool schema did not return within 3s")
	}

	if convertErr != nil {
		t.Fatalf("conversion failed: %v", convertErr)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools count = %d, want 1", len(out.Tools))
	}
	got := out.Tools[0].Function.Parameters
	if len(got) != len(deepSchema) {
		t.Fatalf("parameters length = %d, want %d (verbatim passthrough)", len(got), len(deepSchema))
	}
	if string(got) != string(deepSchema) {
		t.Fatal("parameters bytes were altered -- expected an exact verbatim passthrough")
	}
	t.Logf("safe: a %d-level-deep tool schema (%d bytes) passed through unmodified and instantly", depth, len(got))
}
