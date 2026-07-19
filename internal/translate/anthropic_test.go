package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The system prompt is the highest-stakes conversion: Anthropic sends it as a
// top-level field, OpenAI as a leading role:"system" message. Dropping it is
// silent — the model just never receives its instructions.
func TestSystemPromptBecomesLeadingSystemMessage(t *testing.T) {
	in := &AnthropicRequest{
		Model:     "m",
		MaxTokens: 100,
		System:    json.RawMessage(`"You are a helpful assistant."`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system + user): %s", len(out.Messages), mustJSON(t, out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", out.Messages[0].Role)
	}
	if out.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("system content = %v", out.Messages[0].Content)
	}
}

// The system field is polymorphic: a block array must flatten too.
func TestSystemPromptAsBlockArray(t *testing.T) {
	in := &AnthropicRequest{
		Model:  "m",
		System: json.RawMessage(`[{"type":"text","text":"A"},{"type":"text","text":"B"}]`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	got, _ := out.Messages[0].Content.(string)
	if !strings.Contains(got, "A") || !strings.Contains(got, "B") {
		t.Errorf("flattened system = %q, want both A and B", got)
	}
}

// Content-block arrays must flatten to a plain string: some upstreams reject a
// block array outright with "Input should be a valid string" (the real error
// that motivated sarvam_proxy.py).
func TestContentBlockArrayFlattensToString(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"one"},{"type":"text","text":"two"}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	s, ok := out.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("content is %T, want string", out.Messages[0].Content)
	}
	if s != "one\ntwo" {
		t.Errorf("content = %q, want \"one\\ntwo\"", s)
	}
}

// tool_use -> message.tool_calls, with arguments kept as a JSON *string*.
func TestToolUseBecomesToolCalls(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Paris"}}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].ToolCalls) != 1 {
		t.Fatalf("want 1 message with 1 tool call, got %s", mustJSON(t, out.Messages))
	}
	tc := out.Messages[0].ToolCalls[0]
	if tc.ID != "tu_1" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	// Arguments must be a JSON string, not an object.
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments is not a JSON string: %q", tc.Function.Arguments)
	}
	if args["city"] != "Paris" {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
}

// tool_result -> a separate role:"tool" message keyed by tool_call_id.
func TestToolResultBecomesToolMessage(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"tu_1","content":"22C sunny"},
				  {"type":"text","text":"thanks"}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want tool message then user message, got %s", mustJSON(t, out.Messages))
	}
	if out.Messages[0].Role != "tool" || out.Messages[0].ToolCallID != "tu_1" {
		t.Errorf("messages[0] = %+v, want role=tool tool_call_id=tu_1", out.Messages[0])
	}
	if out.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want user", out.Messages[1].Role)
	}
}

// Poe rejects a tool definition with no "parameters" using a misleading
// "Field required" — the exact bug poe_proxy.py exists to fix.
func TestEnsureToolParametersInjectsEmptySchema(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Tools: []AnthropicTool{{Name: "noparams", Description: "d"}},
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{EnsureToolParameters: true})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %d", len(out.Tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(out.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatalf("parameters not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("parameters = %s, want an object schema", out.Tools[0].Function.Parameters)
	}
	// Without the option the field must stay absent (opt-in only).
	out2, _ := AnthropicToOpenAI(in, Options{})
	if len(out2.Tools[0].Function.Parameters) != 0 {
		t.Errorf("parameters injected without the option: %s", out2.Tools[0].Function.Parameters)
	}
}

func TestStreamOptionsOnlyWhenStreaming(t *testing.T) {
	base := &AnthropicRequest{
		Model:    "m",
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	// Streaming + option -> include_usage true.
	base.Stream = true
	out, _ := AnthropicToOpenAI(base, Options{StreamOptions: true})
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("streaming request missing stream_options.include_usage")
	}
	// Non-streaming must NOT carry stream_options; some upstreams 400 on it.
	base.Stream = false
	out2, _ := AnthropicToOpenAI(base, Options{StreamOptions: true})
	if out2.StreamOptions != nil {
		t.Error("stream_options set on a non-streaming request")
	}
}

// cache_control is Anthropic-only; upstreams that do not know it reject the
// WHOLE request, so every nesting level must be stripped.
func TestStripCacheControlAtEveryDepth(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],
		"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}]
	}`)
	out, err := StripCacheControl(raw)
	if err != nil {
		t.Fatalf("StripCacheControl: %v", err)
	}
	if strings.Contains(string(out), "cache_control") {
		t.Errorf("cache_control survived: %s", out)
	}
	// Everything else must be preserved.
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if v["model"] != "m" {
		t.Errorf("model lost: %s", out)
	}
	if !strings.Contains(string(out), `"text":"hi"`) {
		t.Errorf("message text lost: %s", out)
	}
}

// The router overrides which model id goes upstream.
func TestModelOverride(t *testing.T) {
	in := &AnthropicRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, _ := AnthropicToOpenAI(in, Options{Model: "glm-5.2"})
	if out.Model != "glm-5.2" {
		t.Errorf("model = %q, want the override glm-5.2", out.Model)
	}
}

// Unsupported vision content must fail loudly rather than silently dropping the
// image and letting the model answer confidently about a picture it never saw.
func TestImageBlockIsExplicitError(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]`)},
		},
	}
	if _, err := AnthropicToOpenAI(in, Options{}); err == nil {
		t.Fatal("image block must return an explicit error, not be silently dropped")
	}
}

func TestMalformedContentIsError(t *testing.T) {
	in := &AnthropicRequest{
		Model:    "m",
		Messages: []AnthropicMessage{{Role: "user", Content: json.RawMessage(`12345`)}},
	}
	if _, err := AnthropicToOpenAI(in, Options{}); err == nil {
		t.Fatal("numeric content should be rejected")
	}
}

// Sampling params and stop sequences must survive, with stop_sequences renamed
// to OpenAI's "stop".
func TestSamplingParamsAndStopSequences(t *testing.T) {
	temp, topP := 0.5, 0.9
	in := &AnthropicRequest{
		Model:         "m",
		Temperature:   &temp,
		TopP:          &topP,
		StopSequences: []string{"DONE"},
		Messages:      []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out.Temperature == nil || *out.Temperature != 0.5 {
		t.Errorf("temperature lost")
	}
	if out.TopP == nil || *out.TopP != 0.9 {
		t.Errorf("top_p lost")
	}
	if len(out.Stop) != 1 || out.Stop[0] != "DONE" {
		t.Errorf("stop = %v, want [DONE]", out.Stop)
	}
}
