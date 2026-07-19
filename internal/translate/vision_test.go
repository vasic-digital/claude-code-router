package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// A real (tiny but valid) base64 payload. Anthropic's docs use the classic
// 1x1 PNG for examples; what matters for these tests is only that it is
// syntactically valid base64, not that it decodes to a real image.
const validPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

// decodeImageURLPart is a small helper: given an OpenAIMessage.Content that
// is expected to be a []OpenAIContentPart (round-tripped through JSON, since
// that is what a real caller sees), return the parts.
func decodeContentParts(t *testing.T, content any) []OpenAIContentPart {
	t.Helper()
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	var parts []OpenAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("content is not a part array: %v (%s)", err, raw)
	}
	return parts
}

// A lone base64 image must convert to a single-element image_url array, not
// a plain string (images cannot be represented as a string at all).
func TestImageBlockBase64Alone(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + validPNGBase64 + `"}}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	parts := decodeContentParts(t, out.Messages[0].Content)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1: %+v", len(parts), parts)
	}
	if parts[0].Type != "image_url" {
		t.Errorf("part type = %q, want image_url", parts[0].Type)
	}
	if parts[0].ImageURL == nil {
		t.Fatal("image_url is nil")
	}
	want := "data:image/png;base64," + validPNGBase64
	if parts[0].ImageURL.URL != want {
		t.Errorf("image_url = %q, want %q", parts[0].ImageURL.URL, want)
	}
}

// A message mixing text and an image must become an ARRAY of parts, in the
// same order the blocks appeared in the Anthropic request.
func TestImageAndTextMixedProducesOrderedArray(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"text","text":"what is this?"},` +
					`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + validPNGBase64 + `"}},` +
					`{"type":"text","text":"be specific"}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	parts := decodeContentParts(t, out.Messages[0].Content)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3: %+v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this?" {
		t.Errorf("parts[0] = %+v, want text %q", parts[0], "what is this?")
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Errorf("parts[1] = %+v, want image_url", parts[1])
	}
	if parts[2].Type != "text" || parts[2].Text != "be specific" {
		t.Errorf("parts[2] = %+v, want text %q", parts[2], "be specific")
	}
}

// The url source shape must convert to image_url with the url passed through
// unchanged (no data: URI wrapping).
func TestImageBlockURLSource(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	parts := decodeContentParts(t, out.Messages[0].Content)
	if len(parts) != 1 || parts[0].Type != "image_url" {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[0].ImageURL.URL != "https://example.com/cat.png" {
		t.Errorf("url = %q", parts[0].ImageURL.URL)
	}
}

// Regression guard: a text-only message must still emit a PLAIN STRING, not
// an array — some upstreams hard-reject an array for text-only content
// (sarvam_proxy.py exists precisely because of that). This must hold true
// even after vision support is added.
func TestTextOnlyMessageStillPlainStringAfterVisionSupport(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	s, ok := out.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("content is %T, want string: %v", out.Messages[0].Content, out.Messages[0].Content)
	}
	if s != "hello" {
		t.Errorf("content = %q, want %q", s, "hello")
	}
}

// A malformed image source (missing the required fields for its declared
// type) must be an explicit error naming the message index, never a silent
// drop.
func TestImageBlockMalformedSourceIsNamedError(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"missing data for base64", `{"type":"base64","media_type":"image/png"}`},
		{"missing url for url source", `{"type":"url"}`},
		{"source is not even an object", `"just a string"`},
		{"unknown source type", `{"type":"ftp","location":"ftp://x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &AnthropicRequest{
				Model: "m",
				Messages: []AnthropicMessage{
					{Role: "user", Content: json.RawMessage(`[{"type":"image","source":` + tc.source + `}]`)},
				},
			}
			_, err := AnthropicToOpenAI(in, Options{})
			if err == nil {
				t.Fatal("malformed image source must be an explicit error")
			}
			if !strings.Contains(err.Error(), "messages[0]") {
				t.Errorf("error does not name the message index: %v", err)
			}
		})
	}
}

// An unknown media_type must be rejected: Anthropic documents exactly
// png/jpeg/gif/webp, and forwarding anything else lets an upstream reject it
// with a far less clear error (or, worse, silently mishandle it).
func TestImageBlockUnknownMediaTypeIsError(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"` + validPNGBase64 + `"}}]`)},
		},
	}
	_, err := AnthropicToOpenAI(in, Options{})
	if err == nil {
		t.Fatal("unknown media_type must be rejected")
	}
	if !strings.Contains(err.Error(), "messages[0]") {
		t.Errorf("error does not name the message index: %v", err)
	}
}

// An oversized base64 payload must be rejected with a clear error rather than
// the router allocating an unbounded buffer to decode it.
func TestImageBlockOversizedPayloadIsError(t *testing.T) {
	// Build a base64 string whose DECODED length exceeds the 20MB cap.
	// base64 expands ~4/3, so ~28MB of 'A' characters decodes to ~21MB.
	huge := strings.Repeat("A", 28*1024*1024)
	// Pad to a multiple of 4 so this would otherwise be syntactically valid.
	for len(huge)%4 != 0 {
		huge += "A"
	}
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + huge + `"}}]`)},
		},
	}
	_, err := AnthropicToOpenAI(in, Options{})
	if err == nil {
		t.Fatal("oversized base64 payload must be rejected")
	}
	if !strings.Contains(err.Error(), "messages[0]") {
		t.Errorf("error does not name the message index: %v", err)
	}
}

// Multiple images in one message must all survive, in order, alongside any
// text.
func TestMultipleImagesInOneMessage(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"text","text":"compare these"},` +
					`{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},` +
					`{"type":"image","source":{"type":"url","url":"https://example.com/b.png"}}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	parts := decodeContentParts(t, out.Messages[0].Content)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3: %+v", len(parts), parts)
	}
	if parts[1].ImageURL.URL != "https://example.com/a.png" {
		t.Errorf("parts[1] url = %q", parts[1].ImageURL.URL)
	}
	if parts[2].ImageURL.URL != "https://example.com/b.png" {
		t.Errorf("parts[2] url = %q", parts[2].ImageURL.URL)
	}
}

// An image inside a tool_result (e.g. a computer-use screenshot) must convert
// to an array-form tool message rather than being dropped or mangled into a
// raw JSON blob string.
func TestImageInToolResult(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"tu_1","content":[` +
					`{"type":"text","text":"screenshot taken"},` +
					`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + validPNGBase64 + `"}}` +
					`]}]`)},
		},
	}
	out, err := AnthropicToOpenAI(in, Options{})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (the tool message)", len(out.Messages))
	}
	msg := out.Messages[0]
	if msg.Role != "tool" || msg.ToolCallID != "tu_1" {
		t.Fatalf("message = %+v, want role=tool tool_call_id=tu_1", msg)
	}
	parts := decodeContentParts(t, msg.Content)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2: %+v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "screenshot taken" {
		t.Errorf("parts[0] = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Errorf("parts[1] = %+v, want image_url", parts[1])
	}
}

// A malformed image inside a tool_result must still be a named, explicit
// error rather than silently forwarding a broken payload.
func TestImageInToolResultMalformedIsError(t *testing.T) {
	in := &AnthropicRequest{
		Model: "m",
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"tu_1","content":[` +
					`{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"` + validPNGBase64 + `"}}` +
					`]}]`)},
		},
	}
	_, err := AnthropicToOpenAI(in, Options{})
	if err == nil {
		t.Fatal("malformed image inside a tool_result must be an explicit error")
	}
	if !strings.Contains(err.Error(), "messages[0]") {
		t.Errorf("error does not name the message index: %v", err)
	}
}
