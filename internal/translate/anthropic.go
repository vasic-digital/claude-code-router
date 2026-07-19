// Package translate converts between the Anthropic Messages API (which Claude
// Code speaks) and the OpenAI chat-completions API (which most upstream
// providers speak).
//
// This is the heart of the router. Claude Code always emits Anthropic-shaped
// requests; the great majority of providers only accept OpenAI-shaped ones.
//
// Fidelity notes, each learned from a real upstream rejection:
//
//   - Anthropic carries the system prompt as a TOP-LEVEL "system" field, while
//     OpenAI carries it as a role:"system" message at the head of "messages".
//     Dropping this is silent and severe: the model simply never receives its
//     instructions.
//   - Anthropic content is a LIST of typed blocks; OpenAI content is usually a
//     plain string. Some upstreams hard-reject a block array with
//     "Input should be a valid string".
//   - Anthropic tool calls are tool_use/tool_result content blocks; OpenAI uses
//     message.tool_calls plus role:"tool" messages keyed by tool_call_id.
//   - "cache_control" is Anthropic-only. Upstreams that do not know it reject
//     the whole request, so the cleancache transformer strips it.
//   - Anthropic image content blocks (base64 or url source) become OpenAI
//     image_url parts. The plain-string-vs-array choice for message content
//     is therefore conditional: text-only stays a plain string (some
//     upstreams hard-reject an array there, see above), but the moment a
//     message carries an image the content MUST be an array, because an
//     image cannot be represented inside a plain string at all.
package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ---------- Anthropic wire types ----------

// AnthropicRequest is the POST /v1/messages body.
type AnthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []AnthropicMessage `json:"messages"`
	// System is polymorphic in the real API: a bare string OR a list of
	// content blocks. Kept as RawMessage so both survive round-tripping.
	System        json.RawMessage `json:"system,omitempty"`
	Tools         []AnthropicTool `json:"tools,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	// Thinking is Anthropic's extended-thinking request field (e.g.
	// {"type":"enabled","budget_tokens":1024}). It is kept as an optional
	// RawMessage so it round-trips byte-for-byte and its ABSENCE is identical
	// to before this field existed (omitempty ⇒ a request without it encodes
	// exactly as it always did). The translator does NOT forward it to OpenAI
	// (which has no such field — AnthropicToOpenAI builds OpenAIRequest from
	// named fields and ignores this one), and AnthropicPassthrough preserves it
	// via the raw bytes regardless. Its only consumer is the router's
	// requestWantsThinking, which reads it to activate Router.Think routing.
	Thinking json.RawMessage `json:"thinking,omitempty"`
}

type AnthropicMessage struct {
	Role string `json:"role"`
	// Content is a string or a []ContentBlock — again polymorphic.
	Content json.RawMessage `json:"content"`
}

// ContentBlock is one typed piece of message content.
type ContentBlock struct {
	Type string `json:"type"`
	// type=text
	Text string `json:"text,omitempty"`
	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// type=tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	// type=image
	Source json.RawMessage `json:"source,omitempty"`
	// Anthropic-only; stripped by the cleancache transformer.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ---------- OpenAI wire types ----------

type OpenAIRequest struct {
	Model         string          `json:"model"`
	Messages      []OpenAIMessage `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Tools         []OpenAITool    `json:"tools,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
}

// StreamOptions.include_usage makes an upstream report token usage on the
// final SSE chunk; without it, streaming responses carry no usage at all.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAIContentPart is one element of an OpenAI multi-part message content
// array. Only "text" and "image_url" are produced by this package.
type OpenAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

type OpenAIImageURL struct {
	URL string `json:"url"`
}

type OpenAIToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function OpenAIFnCall `json:"function"`
}

type OpenAIFnCall struct {
	Name string `json:"name"`
	// Arguments is a JSON string, not an object — an OpenAI quirk that must
	// be preserved or upstreams reject the payload.
	Arguments string `json:"arguments"`
}

type OpenAITool struct {
	Type     string      `json:"type"`
	Function OpenAIFnDef `json:"function"`
}

type OpenAIFnDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ---------- Conversion ----------

// Options controls provider-specific fixups.
type Options struct {
	// CleanCache strips Anthropic cache_control blocks (transformer
	// "cleancache"). Required by upstreams that reject unknown fields.
	CleanCache bool
	// StreamOptions adds stream_options.include_usage on streaming requests
	// (transformer "streamoptions").
	StreamOptions bool
	// EnsureToolParameters guarantees every tool has a non-empty parameters
	// object. Some upstreams (Poe) reject a tool without it with a
	// misleading "Field required".
	EnsureToolParameters bool
	// Model overrides the model id sent upstream (the router picks this).
	Model string
}

// decodeContent normalises the polymorphic content field into blocks.
// A bare JSON string becomes a single text block.
func decodeContent(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content is neither string nor block array: %w", err)
	}
	return blocks, nil
}

// systemText flattens the polymorphic system field to plain text.
func systemText(raw json.RawMessage) (string, error) {
	blocks, err := decodeContent(raw)
	if err != nil {
		return "", err
	}
	var out string
	for _, b := range blocks {
		if b.Type == "text" {
			if out != "" {
				out += "\n\n"
			}
			out += b.Text
		}
	}
	return out, nil
}

// maxImageDecodedBytes caps how large a single base64-encoded image payload
// may be once decoded (a generous allowance for a screenshot or photo, not a
// generic file-transfer channel). Checked BEFORE decoding, so an oversized
// payload is rejected without ever allocating a buffer for it.
const maxImageDecodedBytes = 20 * 1024 * 1024 // 20MB

// allowedImageMediaTypes are the four raster formats Anthropic's Messages API
// documents for image content blocks. Anything else is rejected explicitly
// here rather than forwarded for the upstream to reject less clearly.
var allowedImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// anthropicImageSource mirrors the two shapes Anthropic's image content block
// "source" field takes: an inline base64 payload, or a remote URL.
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// convertImageBlock turns one Anthropic "image" content block into the OpenAI
// image_url content part.
//
// Errors here never name a message index themselves: this helper is shared
// between top-level message content and tool_result content (a computer-use
// tool_result can itself carry a screenshot), and each call site knows which
// Anthropic message index the error should be attributed to.
//
// A malformed or unsupported source is always an explicit, named error —
// silently dropping an image would let the model answer confidently about a
// picture it never actually saw, which is the whole reason vision content
// used to be rejected outright rather than mishandled.
func convertImageBlock(b ContentBlock) (OpenAIContentPart, error) {
	if len(b.Source) == 0 {
		return OpenAIContentPart{}, fmt.Errorf("image block has no source")
	}
	var src anthropicImageSource
	if err := json.Unmarshal(b.Source, &src); err != nil {
		return OpenAIContentPart{}, fmt.Errorf("image source is malformed: %w", err)
	}

	switch src.Type {
	case "base64":
		if !allowedImageMediaTypes[src.MediaType] {
			return OpenAIContentPart{}, fmt.Errorf(
				"image media_type %q is not one of image/png, image/jpeg, image/gif, image/webp", src.MediaType)
		}
		if src.Data == "" {
			return OpenAIContentPart{}, fmt.Errorf("base64 image source has no data")
		}
		// Guard against absurd payloads BEFORE decoding: DecodedLen is a
		// cheap arithmetic bound, so an oversized payload never causes an
		// unbounded allocation in DecodeString below.
		if n := base64.StdEncoding.DecodedLen(len(src.Data)); n > maxImageDecodedBytes {
			return OpenAIContentPart{}, fmt.Errorf(
				"base64 image payload is ~%d bytes decoded, exceeds the %d-byte cap", n, maxImageDecodedBytes)
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return OpenAIContentPart{}, fmt.Errorf("base64 image data does not decode: %w", err)
		}
		return OpenAIContentPart{
			Type:     "image_url",
			ImageURL: &OpenAIImageURL{URL: "data:" + src.MediaType + ";base64," + src.Data},
		}, nil
	case "url":
		if src.URL == "" {
			return OpenAIContentPart{}, fmt.Errorf("url image source has no url")
		}
		return OpenAIContentPart{
			Type:     "image_url",
			ImageURL: &OpenAIImageURL{URL: src.URL},
		}, nil
	default:
		return OpenAIContentPart{}, fmt.Errorf("unsupported image source type %q", src.Type)
	}
}

// convertToolResultContent flattens a tool_result's polymorphic content into
// either a plain string or an ordered array of OpenAI content parts.
//
// The array form is used only when the tool_result carries at least one
// image (e.g. a screenshot returned by a computer-use tool); otherwise this
// reproduces the exact pre-vision behaviour byte-for-byte, including the
// tolerant fallback to the raw content bytes when it isn't decodable as
// blocks at all — that leniency predates vision support and is not this
// change's concern to tighten.
func convertToolResultContent(raw json.RawMessage, msgIdx int) (any, error) {
	content := string(raw)
	inner, err := decodeContent(raw)
	if err != nil {
		return content, nil
	}

	hasImage := false
	for _, ib := range inner {
		if ib.Type == "image" {
			hasImage = true
			break
		}
	}
	if !hasImage {
		var flat string
		for _, ib := range inner {
			if ib.Type == "text" {
				if flat != "" {
					flat += "\n"
				}
				flat += ib.Text
			}
		}
		if flat != "" {
			content = flat
		}
		return content, nil
	}

	parts := make([]OpenAIContentPart, 0, len(inner))
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			parts = append(parts, OpenAIContentPart{Type: "text", Text: ib.Text})
		case "image":
			part, err := convertImageBlock(ib)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: tool_result: %w", msgIdx, err)
			}
			parts = append(parts, part)
		}
	}
	return parts, nil
}

// AnthropicToOpenAI converts a Claude Code request into an OpenAI one.
func AnthropicToOpenAI(in *AnthropicRequest, opt Options) (*OpenAIRequest, error) {
	model := in.Model
	if opt.Model != "" {
		model = opt.Model
	}
	out := &OpenAIRequest{
		Model:       model,
		MaxTokens:   in.MaxTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.StopSequences,
		Stream:      in.Stream,
	}
	if in.Stream && opt.StreamOptions {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// System prompt: top-level (Anthropic) -> leading system message (OpenAI).
	// Losing this silently strips the model's entire instruction set.
	if len(in.System) > 0 {
		sys, err := systemText(in.System)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if sys != "" {
			out.Messages = append(out.Messages, OpenAIMessage{Role: "system", Content: sys})
		}
	}

	for i, m := range in.Messages {
		blocks, err := decodeContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}

		var text string
		// parts mirrors text (and adds image_url entries) in block order, but
		// is only USED when the message actually carries an image — see the
		// hasImage branch below.
		var parts []OpenAIContentPart
		var hasImage bool
		var calls []OpenAIToolCall
		// tool_result blocks become separate role:"tool" messages, which must
		// be emitted BEFORE the message that carries the remaining content.
		var toolResults []OpenAIMessage

		for _, b := range blocks {
			switch b.Type {
			case "text":
				if text != "" {
					text += "\n"
				}
				text += b.Text
				parts = append(parts, OpenAIContentPart{Type: "text", Text: b.Text})
			case "tool_use":
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				calls = append(calls, OpenAIToolCall{
					ID: b.ID, Type: "function",
					Function: OpenAIFnCall{Name: b.Name, Arguments: args},
				})
			case "tool_result":
				content, err := convertToolResultContent(b.Content, i)
				if err != nil {
					return nil, err
				}
				toolResults = append(toolResults, OpenAIMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Content: content,
				})
			case "image":
				part, err := convertImageBlock(b)
				if err != nil {
					return nil, fmt.Errorf("messages[%d]: %w", i, err)
				}
				parts = append(parts, part)
				hasImage = true
			}
		}

		out.Messages = append(out.Messages, toolResults...)
		switch {
		case hasImage:
			// At least one image forces the ARRAY content form: an image
			// cannot be represented inside a plain string at all. Text-only
			// messages deliberately do NOT take this path (see the case
			// below) — some upstreams hard-reject an array for text-only
			// content, which is exactly why sarvam_proxy.py exists.
			out.Messages = append(out.Messages, OpenAIMessage{Role: m.Role, ToolCalls: calls, Content: parts})
		case text != "" || len(calls) > 0:
			msg := OpenAIMessage{Role: m.Role, ToolCalls: calls}
			if text != "" {
				msg.Content = text
			} else {
				// An assistant turn that is purely tool calls still needs the
				// key present; null is the accepted encoding.
				msg.Content = nil
			}
			out.Messages = append(out.Messages, msg)
		}
	}

	for _, t := range in.Tools {
		params := t.InputSchema
		// CleanCache must be applied HERE, not only to the typed fields.
		//
		// Text and system blocks lose cache_control automatically, because
		// they are decoded into typed structs that simply have no such field.
		// A tool's input_schema is different: it is json.RawMessage and is
		// forwarded VERBATIM, so any cache_control inside it travelled all the
		// way to the upstream. The transformer was therefore a no-op, and
		// providers that reject unknown fields rejected the whole request —
		// precisely the failure cleancache exists to prevent.
		//
		// StripCacheControl is schema-aware: it will not delete a schema
		// property legitimately NAMED cache_control (see stripKey).
		if opt.CleanCache && len(params) > 0 {
			if cleaned, err := StripCacheControl(params); err == nil {
				params = cleaned
			}
			// On error the original schema is kept: a tool definition we
			// cannot re-encode is better sent as-is than dropped.
		}
		if opt.EnsureToolParameters && len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIFnDef{
				Name: t.Name, Description: t.Description, Parameters: params,
			},
		})
	}
	return out, nil
}

// AnthropicPassthrough prepares an Anthropic-shaped request body for an
// Anthropic-NATIVE upstream (a config.Provider whose ResolvedProtocol is
// "anthropic"), which must receive the request UNCHANGED rather than translated
// to OpenAI shape.
//
// It applies only the two provider-scoped fixups that are meaningful for a
// native endpoint:
//
//   - opt.Model overrides the top-level "model" (the router's chosen model id);
//   - opt.CleanCache strips cache_control — for a self-hosted Anthropic-COMPATIBLE
//     upstream that does not implement prompt caching. A real Anthropic endpoint
//     accepts cache_control, so a provider pointed at api.anthropic.com should
//     leave the cleancache transformer OFF.
//
// Every other field is preserved VERBATIM. It works on the raw JSON rather than
// the typed AnthropicRequest precisely so that fields this package does not
// model (metadata, tool_choice, top_k, thinking, ...) are not silently dropped:
// a passthrough that quietly discarded fields would defeat its own purpose.
// UseNumber keeps large-integer / high-precision literals intact, exactly as
// StripCacheControl does and for the same reason (see its doc).
func AnthropicPassthrough(raw []byte, opt Options) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("anthropic request body must be a JSON object")
	}
	if opt.Model != "" {
		m["model"] = opt.Model
	}
	if opt.CleanCache {
		// stripKey is schema-aware: a tool input_schema property legitimately
		// NAMED cache_control is preserved (see stripKey); only Anthropic
		// cache_control metadata is removed.
		v = stripKey(v, "cache_control")
	}
	return json.Marshal(v)
}

// StripCacheControl removes every cache_control key from a raw request body.
//
// This operates on the generic JSON tree rather than the typed structs because
// cache_control can appear at several nesting levels (system blocks, message
// content blocks, tool definitions), and an upstream that does not recognise
// the field rejects the ENTIRE request — so none may be missed.
// Decoding uses UseNumber() rather than a plain json.Unmarshal into `any`.
// Plain unmarshalling converts every JSON number to float64, which has two
// real consequences for a passthrough proxy:
//
//   - Any literal whose magnitude overflows float64 (found by fuzzing:
//     "1E700") makes the WHOLE request fail with "cannot unmarshal number
//     into Go value of type float64" — a request Claude Code sent in good
//     faith would be rejected outright.
//   - Worse because it is silent: a large integer id such as
//     12345678901234567890 would be re-encoded as 1.2345678901234567e+19,
//     corrupting a value we were only ever meant to pass through untouched.
//
// json.Number keeps the original literal verbatim, so this function now only
// removes cache_control and changes nothing else about the document.
func StripCacheControl(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(stripKey(v, "cache_control"))
}

// stripKey removes every occurrence of key from the JSON tree, EXCEPT where
// the key is user data rather than Anthropic metadata.
//
// The exception matters. A tool's input_schema is a JSON Schema, and inside
// its "properties" object the map keys are property NAMES chosen by whoever
// wrote the tool. A tool may legitimately declare a property called
// "cache_control". Deleting blindly produced a self-contradictory schema:
//
//	"properties": {}                     <- the property was removed
//	"required":   ["cache_control"]      <- but the requirement remained
//
// silently, with no error, corrupting a tool definition we only had to pass
// through. Found by the challenges suite.
//
// inProperties tracks whether the current map is a JSON Schema "properties"
// object. Its keys are skipped for deletion, but their VALUES are still
// walked, so an Anthropic cache_control nested deeper inside a property's own
// schema is still removed.
func stripKey(v any, key string) any {
	return stripKeyIn(v, key, false)
}

func stripKeyIn(v any, key string, inProperties bool) any {
	switch t := v.(type) {
	case map[string]any:
		if !inProperties {
			delete(t, key)
		}
		for k, sub := range t {
			// A child map named "properties" is a JSON Schema property bag:
			// its immediate keys are user-chosen names, not metadata.
			t[k] = stripKeyIn(sub, key, k == "properties")
		}
		return t
	case []any:
		for i, sub := range t {
			// Array elements are never property-name positions.
			t[i] = stripKeyIn(sub, key, false)
		}
		return t
	}
	return v
}
