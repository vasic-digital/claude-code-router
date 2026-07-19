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
package translate

import (
	"bytes"
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
				content := string(b.Content)
				// tool_result content is itself polymorphic; flatten to text
				// so upstreams that demand a string are satisfied.
				if inner, err := decodeContent(b.Content); err == nil {
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
				}
				toolResults = append(toolResults, OpenAIMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Content: content,
				})
			case "image":
				// Vision passthrough is not yet implemented. Failing loudly
				// beats silently dropping the image and returning a confident
				// answer about a picture the model never saw.
				return nil, fmt.Errorf("messages[%d]: image content blocks are not supported yet", i)
			}
		}

		out.Messages = append(out.Messages, toolResults...)
		if text != "" || len(calls) > 0 {
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
