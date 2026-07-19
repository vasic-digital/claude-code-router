package cache

import (
	"encoding/json"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// Cacheable reports whether a request may be served from / stored in the cache,
// and — when it may not — a short machine-stable reason.
//
// This is the REQUEST-side gate. It enforces the two dossier gates that a
// request alone can decide:
//
//   - Streaming gate: a streaming request is not cacheable in Phase 1. A cached
//     body is a single buffered response; faithfully replaying it as an SSE
//     stream is Phase 3 work, so until then streaming requests bypass the cache
//     entirely rather than risk a malformed replay.
//   - Temperature gate: a sampled response (temperature > 0) is
//     non-deterministic and therefore not reusable. Temperature is a *float64
//     in AnthropicRequest, so "unset" (nil) is distinguishable from an explicit
//     0 — both are cacheable, any non-zero value is not.
//
// The reason strings are stable identifiers suitable for a metric label.
func Cacheable(req *translate.AnthropicRequest) (bool, string) {
	if req == nil {
		return false, "nil-request"
	}
	if req.Stream {
		return false, "streaming"
	}
	if req.Temperature != nil && *req.Temperature != 0 {
		return false, "temperature>0"
	}
	return true, ""
}

// ResponseCacheable reports whether a buffered upstream response body may be
// stored, and a reason when it may not. This is the RESPONSE-side gate, applied
// after a successful (2xx) upstream call and before Store.
//
// It enforces:
//
//   - Error gate: an error-shaped body (top-level "error", or no choices) is
//     never cached — only genuine successful generations are reusable.
//   - Tool gate: a response carrying tool_calls (OpenAI shape) / tool_use is not
//     cached unless allowToolResponses is true, because the answer depends on
//     live tool state that will differ on the next request.
//
// body is the OpenAI chat-completion JSON the gateway buffers today
// (respondNonStreaming already ReadAlls it). A body that does not parse as a
// chat completion is refused, conservatively.
func ResponseCacheable(body []byte, allowToolResponses bool) (bool, string) {
	if len(body) == 0 {
		return false, "empty-body"
	}
	var probe struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false, "unparseable-body"
	}
	if len(probe.Error) > 0 && string(probe.Error) != "null" {
		return false, "error-in-body"
	}
	if len(probe.Choices) == 0 {
		return false, "no-choices"
	}
	if !allowToolResponses {
		for _, ch := range probe.Choices {
			if len(ch.Message.ToolCalls) > 0 {
				return false, "tool-call-response"
			}
			if ch.FinishReason == "tool_calls" {
				return false, "tool-call-response"
			}
		}
	}
	return true, ""
}
