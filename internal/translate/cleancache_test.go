package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression guard for a real defect: the "cleancache" transformer was a
// complete no-op on outgoing requests.
//
// Text and system blocks lose cache_control for free, because they decode into
// typed structs with no such field. A tool's input_schema does not: it is
// json.RawMessage and is forwarded verbatim, so cache_control inside a tool
// schema reached the upstream regardless of the flag. Providers that reject
// unknown fields rejected the entire request — exactly what cleancache exists
// to prevent. Measured before the fix: cache_control was present in the
// outgoing request with CleanCache both false AND true.
func TestCleanCacheStripsCacheControlFromToolSchemas(t *testing.T) {
	newReq := func() *AnthropicRequest {
		return &AnthropicRequest{
			Model: "m",
			Tools: []AnthropicTool{{
				Name: "get_weather",
				InputSchema: json.RawMessage(
					`{"type":"object","properties":{"city":{"type":"string"}},"cache_control":{"type":"ephemeral"}}`),
			}},
			Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}
	}

	// CleanCache ON: the metadata must be gone.
	on, err := AnthropicToOpenAI(newReq(), Options{CleanCache: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	onJSON, _ := json.Marshal(on)
	if strings.Contains(string(onJSON), "cache_control") {
		t.Errorf("cleancache enabled but cache_control still reaches the upstream: %s", onJSON)
	}
	// ...and the rest of the schema must survive intact.
	if !strings.Contains(string(on.Tools[0].Function.Parameters), `"city"`) {
		t.Errorf("stripping damaged the tool schema: %s", on.Tools[0].Function.Parameters)
	}
	if !strings.Contains(string(on.Tools[0].Function.Parameters), `"type":"object"`) {
		t.Errorf("schema type lost: %s", on.Tools[0].Function.Parameters)
	}

	// CleanCache OFF: passthrough is preserved, so the flag genuinely controls
	// behaviour rather than the fix simply always stripping.
	off, err := AnthropicToOpenAI(newReq(), Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	offJSON, _ := json.Marshal(off)
	if !strings.Contains(string(offJSON), "cache_control") {
		t.Errorf("cleancache disabled but cache_control was stripped anyway: %s", offJSON)
	}
}

// The schema-aware exemption must hold through this path too: a property
// legitimately NAMED cache_control is user data and must not be deleted, or a
// tool definition is silently corrupted.
func TestCleanCacheKeepsSchemaPropertyNamedCacheControl(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Tools: []AnthropicTool{{
			Name: "t",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"cache_control":{"type":"string"}},"required":["cache_control"]}`),
		}},
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in, Options{CleanCache: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(out.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatalf("emitted schema invalid: %v", err)
	}
	if _, ok := schema.Properties["cache_control"]; !ok {
		t.Errorf("a legitimate schema property named cache_control was deleted: %s",
			out.Tools[0].Function.Parameters)
	}
	for _, req := range schema.Required {
		if _, ok := schema.Properties[req]; !ok {
			t.Errorf("schema left self-contradictory: required %q has no property", req)
		}
	}
}

// A tool with no schema must still work under CleanCache, alone and combined
// with EnsureToolParameters.
func TestCleanCacheWithEmptyOrAbsentSchema(t *testing.T) {
	in := &AnthropicRequest{
		Model:    "m",
		Tools:    []AnthropicTool{{Name: "noschema"}},
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	if _, err := AnthropicToOpenAI(in, Options{CleanCache: true}); err != nil {
		t.Fatalf("CleanCache with an absent schema: %v", err)
	}
	out, err := AnthropicToOpenAI(in, Options{CleanCache: true, EnsureToolParameters: true})
	if err != nil {
		t.Fatalf("CleanCache + EnsureToolParameters: %v", err)
	}
	if !strings.Contains(string(out.Tools[0].Function.Parameters), `"type":"object"`) {
		t.Errorf("empty schema not injected: %s", out.Tools[0].Function.Parameters)
	}
}

// A malformed schema must be forwarded unchanged rather than dropped: sending
// a tool definition as-is beats losing the tool entirely.
func TestCleanCacheKeepsMalformedSchemaRatherThanDroppingIt(t *testing.T) {
	bad := json.RawMessage(`{"type":"object",`)
	in := &AnthropicRequest{
		Model:    "m",
		Tools:    []AnthropicTool{{Name: "t", InputSchema: bad}},
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in, Options{CleanCache: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if string(out.Tools[0].Function.Parameters) != string(bad) {
		t.Errorf("malformed schema was altered: got %s, want %s",
			out.Tools[0].Function.Parameters, bad)
	}
}
