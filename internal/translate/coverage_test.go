package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Regression: cache_control as a legitimate JSON Schema property NAME.
//
// Found by the challenges suite. stripKey deleted any key literally named
// "cache_control" anywhere in the tree. Inside a tool's input_schema
// "properties" object the keys are property names chosen by the tool author,
// so a tool declaring a property called "cache_control" got it silently
// deleted while "required":["cache_control"] survived — a self-contradictory
// schema, produced with no error and no panic.
// ---------------------------------------------------------------------------

func TestStripCacheControlKeepsSchemaPropertyNamedCacheControl(t *testing.T) {
	body := `{"tools":[{"name":"t","input_schema":{
		"type":"object",
		"properties":{"cache_control":{"type":"string"},"city":{"type":"string"}},
		"required":["cache_control"]}}]}`

	out, err := StripCacheControl([]byte(body))
	if err != nil {
		t.Fatalf("StripCacheControl: %v", err)
	}

	var doc struct {
		Tools []struct {
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, out)
	}
	schema := doc.Tools[0].InputSchema

	if _, ok := schema.Properties["cache_control"]; !ok {
		t.Errorf("the schema property named cache_control was deleted: %s", out)
	}
	if _, ok := schema.Properties["city"]; !ok {
		t.Errorf("an unrelated schema property was lost: %s", out)
	}
	// The invariant that actually matters: the schema must stay
	// self-consistent — everything in required must exist in properties.
	for _, req := range schema.Required {
		if _, ok := schema.Properties[req]; !ok {
			t.Errorf("schema is self-contradictory: required %q has no property: %s", req, out)
		}
	}
}

// The fix must not stop cache_control being stripped where it IS Anthropic
// metadata — otherwise upstreams that reject the field start failing again.
func TestStripCacheControlStillStripsRealMetadata(t *testing.T) {
	body := `{
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"t","cache_control":{"type":"ephemeral"},
			"input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}]
	}`
	out, err := StripCacheControl([]byte(body))
	if err != nil {
		t.Fatalf("StripCacheControl: %v", err)
	}
	if strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("real cache_control metadata survived: %s", out)
	}
}

// A cache_control nested INSIDE a property's own schema is still metadata and
// must go — the exemption covers property NAMES, not everything beneath them.
func TestStripCacheControlStripsMetadataNestedUnderAProperty(t *testing.T) {
	body := `{"tools":[{"name":"t","input_schema":{
		"type":"object",
		"properties":{"city":{"type":"string","cache_control":{"type":"ephemeral"}}}}}]}`

	out, err := StripCacheControl([]byte(body))
	if err != nil {
		t.Fatalf("StripCacheControl: %v", err)
	}
	if strings.Contains(string(out), "ephemeral") {
		t.Errorf("cache_control nested under a property survived: %s", out)
	}
	if !strings.Contains(string(out), `"city"`) {
		t.Errorf("the property itself was lost: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Coverage for the two mutants that SURVIVED the mutation harness. Both
// behaviours were verified correct; they simply had no test observing them,
// so a regression would have gone unnoticed.
// ---------------------------------------------------------------------------

// Surviving mutant 1: the newline separator joining multiple text blocks in a
// tool_result was unobserved, so dropping it changed nothing any test saw.
func TestToolResultWithMultipleTextBlocksJoinsWithNewline(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"text","text":"line one"},
				{"type":"text","text":"line two"},
				{"type":"text","text":"line three"}]}]`),
		}},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("want 1 tool message, got %d", len(out.Messages))
	}
	got, ok := out.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("tool content is %T, want string", out.Messages[0].Content)
	}
	if got != "line one\nline two\nline three" {
		t.Errorf("blocks joined as %q, want newline-separated", got)
	}
}

// Surviving mutant 2: EnsureToolParameters was never tested against a tool
// that ALREADY has a schema. messages.go sets it unconditionally on every
// request, so if it overwrote an existing schema, every real tool definition
// would be silently replaced by an empty one. Verified: it does not.
func TestEnsureToolParametersNeverOverwritesAnExistingSchema(t *testing.T) {
	const real = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
	in := &AnthropicRequest{
		Model:    "m",
		Tools:    []AnthropicTool{{Name: "get_weather", InputSchema: json.RawMessage(real)}},
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in, Options{EnsureToolParameters: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got := out.Tools[0].Function.Parameters

	var want, have any
	if err := json.Unmarshal([]byte(real), &want); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("emitted parameters invalid: %v (%s)", err, got)
	}
	wj, _ := json.Marshal(want)
	hj, _ := json.Marshal(have)
	if string(wj) != string(hj) {
		t.Errorf("existing tool schema was modified\n got: %s\nwant: %s", hj, wj)
	}
	// And the injection must still happen for a tool that genuinely has none.
	in2 := &AnthropicRequest{
		Model:    "m",
		Tools:    []AnthropicTool{{Name: "noschema"}},
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out2, err := AnthropicToOpenAI(in2, Options{EnsureToolParameters: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(out2.Tools[0].Function.Parameters), `"type":"object"`) {
		t.Errorf("empty schema not injected: %s", out2.Tools[0].Function.Parameters)
	}
}
