package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/router"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// maxRequestBodyBytes caps an inbound POST /v1/messages body.
//
// 32MiB matches the cap already applied to upstream completion responses, so
// the two directions are symmetric. It is far above any realistic Claude Code
// request (the largest observed are a few MiB of conversation history) while
// still bounding what a single client can force the gateway to allocate.
const maxRequestBodyBytes = 32 << 20

// ---------- Seams: Router and Upstream ----------
//
// These are narrow, LOCAL interfaces — not internal/router.Router or
// internal/proxy's upstream client, which this package deliberately does not
// import (another agent owns those packages). Server.Router / Server.Upstream
// default to the minimal implementations below (defaultRouter,
// defaultUpstream) so the gateway works standalone; a caller that owns the
// fuller implementations can swap either field in after gateway.New, before
// Start.

// Router selects a provider+model pair for a given Anthropic-shaped request.
type Router interface {
	Route(req *translate.AnthropicRequest) (provider config.Provider, model string, err error)
}

// Upstream performs the HTTP call to a provider's OpenAI-compatible
// chat-completions endpoint and returns the raw response for the caller to
// translate (streaming or not). The caller closes resp.Body.
type Upstream interface {
	Do(ctx context.Context, p config.Provider, body []byte) (*http.Response, error)
}

// defaultRouter always resolves cfg.Router.Default. It intentionally does not
// implement background/think/longContext selection — that heuristic belongs
// to internal/router; this exists only so the gateway is functional before
// that package is wired in.
type defaultRouter struct{ cfg *config.Config }

func (r defaultRouter) Route(_ *translate.AnthropicRequest) (config.Provider, string, error) {
	if r.cfg.Router.Default == "" {
		return config.Provider{}, "", fmt.Errorf("no default route configured")
	}
	name, model, err := config.SplitRoute(r.cfg.Router.Default)
	if err != nil {
		return config.Provider{}, "", err
	}
	p := r.cfg.ProviderByName(name)
	if p == nil {
		return config.Provider{}, "", fmt.Errorf("route references unknown provider %q", name)
	}
	return *p, model, nil
}

// defaultUpstream is a plain net/http Upstream. It deliberately sets no
// http.Client.Timeout: a fixed client-wide timeout would kill legitimately
// long-lived SSE streams. Per-call deadlines are instead carried on the
// context handleMessages passes in (see the non-streaming timeout there).
type defaultUpstream struct{ client *http.Client }

func (u *defaultUpstream) Do(ctx context.Context, p config.Provider, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.APIBaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := u.client
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

// ---------- Anthropic response wire types ----------

type anthropicContentBlock struct {
	Type string `json:"type"`
	// type=text
	Text string `json:"text,omitempty"`
	// type=tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        anthropicUsage          `json:"usage"`
}

// ---------- OpenAI upstream wire types ----------

// openAIChatResponse is the non-streaming chat-completions response shape.
type openAIChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// openAIStreamChunk is one `data:` line of a chat-completions SSE stream.
type openAIStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// mapFinishReason translates an OpenAI finish_reason to an Anthropic
// stop_reason. Anthropic has no equivalent of "content_filter"; it is mapped
// to "end_turn" rather than inventing a value the client does not expect.
func mapFinishReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ---------- Handler ----------

// handleMessages serves POST /v1/messages: decode the Anthropic request,
// route it, translate to OpenAI, call the upstream, and translate the result
// back to Anthropic shape — streaming or not.
func (s *Server) handleMessages(c *gin.Context) {
	// Cap the INBOUND body. Both upstream-response reads already use
	// io.LimitReader (64KiB for error bodies, 32MiB for completions), but the
	// request path was an unbounded io.ReadAll: a single client could stream
	// an arbitrarily large body and drive the gateway to OOM, taking down
	// every other in-flight request with it. Found by the security suite.
	//
	// http.MaxBytesReader (rather than a bare io.LimitReader) is deliberate:
	// a LimitReader silently TRUNCATES at the cap, which would hand the JSON
	// decoder a body cut mid-token and surface as a confusing "invalid JSON"
	// for what is really an oversized request. MaxBytesReader instead returns
	// a distinct error, so the client is told the real reason.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// A *http.MaxBytesError means the cap was hit; 413 is the honest
		// status for that, not the generic 400 used for unreadable bodies.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds the %d-byte limit", maxRequestBodyBytes))
			return
		}
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("read request body: %v", err))
		return
	}

	var in translate.AnthropicRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	provider, model, err := s.Router.Route(&in)
	if err != nil {
		writeAnthropicError(c, http.StatusServiceUnavailable, "not_found_error", err.Error())
		return
	}

	outReq, err := translate.AnthropicToOpenAI(&in, translate.Options{
		CleanCache:    provider.Has("cleancache"),
		StreamOptions: provider.Has("streamoptions"),
		// Always on: a tool with no input_schema otherwise reaches upstreams
		// (e.g. Poe) that hard-reject it with a misleading "Field required".
		// Harmless for tools that already declare a schema.
		EnsureToolParameters: true,
		Model:                model,
	})
	if err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	outBody, err := json.Marshal(outReq)
	if err != nil {
		writeAnthropicError(c, http.StatusInternalServerError, "api_error", fmt.Sprintf("encode upstream request: %v", err))
		return
	}

	ctx := c.Request.Context()
	if !in.Stream {
		// Streaming responses are exempt from this deadline (an SSE session
		// legitimately outlives any fixed bound); non-streaming calls are not,
		// so a wedged upstream cannot hang the request forever.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opt.UpstreamTimeout)
		defer cancel()
	}

	resp, ok := s.doUpstreamWithRetry(c, ctx, provider, outBody)
	if !ok {
		// A terminal failure, an exhausted retry budget, or a cancelled
		// context has already written the response — nothing left to do.
		return
	}
	defer resp.Body.Close()

	if in.Stream {
		streamAnthropicSSE(c, resp.Body, model)
		return
	}
	respondNonStreaming(c, resp.Body, model)
}

// ---------- Retry loop ----------
//
// internal/router classifies failures (Retryable vs Terminal) and computes
// backoff, but proxy.Client.Do — reached here via s.Upstream.Do — makes
// exactly ONE HTTP attempt and knows nothing about retrying. This is the
// loop that actually drives those classifiers: it calls s.Upstream.Do up to
// s.opt.MaxAttempts times, deciding after each attempt whether to try again
// (and how long to wait first) purely from router.ClassifyStatus /
// router.ClassifyTransportError.
//
// The retryDelayAfter* indirections below exist so tests can swap in a
// near-zero delay and exercise the loop's control flow (attempt counts,
// terminal-vs-retryable branching, context cancellation) without actually
// waiting out router's real backoff floor (>=1s) on every retry scenario.
// Production code always runs with these pointed at the real router
// functions.
var (
	retryDelayAfterStatus       = router.FallbackRetryDelayAfterStatus
	retryDelayAfterNetworkError = router.FallbackRetryDelayAfterNetworkError
)

// doUpstreamWithRetry calls s.Upstream.Do against provider, retrying on a
// Retryable failure (see router.ClassifyStatus / router.ClassifyTransportError)
// up to s.opt.MaxAttempts total attempts, and NEVER retrying a Terminal one
// (a 401 retried is quota burned for a request that will fail identically
// every time).
//
// It returns (resp, true) on a successful (status < 400) response, which the
// caller owns and must resp.Body.Close(). It returns (nil, false) once an
// error has already been written to c via writeAnthropicError or
// forwardUpstreamError — covering a Terminal failure, an exhausted retry
// budget, or ctx ending (client disconnect, or the non-streaming request
// deadline) while waiting to retry.
//
// The critical invariant this function exists to uphold: it NEVER writes a
// single response byte to c except on that final, no-more-retries outcome.
// Every intermediate failed attempt is silently discarded (its body drained
// and closed, nothing sent to the client) so the caller is free to retry
// with a clean slate. Concretely, this means a streaming response is only
// ever handed to streamAnthropicSSE — which commits the HTTP status and
// begins flushing SSE events immediately — AFTER this function has already
// returned its final, no-further-retries answer; retrying after that point
// would corrupt an in-flight SSE conversation Claude Code is already
// consuming, and cannot happen because this function has, by construction,
// no further opportunity to run once streamAnthropicSSE starts.
func (s *Server) doUpstreamWithRetry(c *gin.Context, ctx context.Context, provider config.Provider, body []byte) (*http.Response, bool) {
	maxAttempts := s.opt.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			// The client disconnected, or (non-streaming only) the overall
			// request deadline already passed, before this attempt even
			// started. Hammering the upstream further would serve no one.
			writeAnthropicError(c, http.StatusGatewayTimeout, "api_error",
				fmt.Sprintf("request context ended before attempt %d: %v", attempt+1, err))
			return nil, false
		}

		resp, err := s.Upstream.Do(ctx, provider, body)
		if err != nil {
			if router.ClassifyTransportError(err) == router.Retryable && attempt+1 < maxAttempts {
				if !sleepForRetry(ctx, retryDelayAfterNetworkError(attempt)) {
					writeAnthropicError(c, http.StatusGatewayTimeout, "api_error",
						fmt.Sprintf("request context ended while waiting to retry: %v", ctx.Err()))
					return nil, false
				}
				continue
			}
			writeAnthropicError(c, http.StatusBadGateway, "api_error", fmt.Sprintf("upstream request failed: %v", err))
			return nil, false
		}

		if resp.StatusCode < 400 {
			return resp, true
		}

		if router.ClassifyStatus(resp.StatusCode) == router.Retryable && attempt+1 < maxAttempts {
			retryAfter := resp.Header.Get("Retry-After")
			// This attempt is being discarded outright, not just its status
			// code: drain (bounded — this is a body we are throwing away,
			// not relaying) and close so the connection can be reused and
			// nothing here leaks.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if !sleepForRetry(ctx, retryDelayAfterStatus(attempt, retryAfter)) {
				writeAnthropicError(c, http.StatusGatewayTimeout, "api_error",
					"request context ended while waiting to retry")
				return nil, false
			}
			continue
		}

		// Either Terminal (never retry — see the package doc on
		// router.Retryable/Terminal) or Retryable but out of attempts:
		// either way, report exactly what upstream said.
		forwardUpstreamError(c, resp)
		_ = resp.Body.Close()
		return nil, false
	}

	// Unreachable in practice: every branch above returns before the loop
	// can exit on its own condition — a retryable failure only `continue`s
	// when attempt+1 < maxAttempts guarantees another iteration is still in
	// range. This exists solely to give the function a terminating return
	// the compiler can see.
	writeAnthropicError(c, http.StatusInternalServerError, "api_error", "retry loop ended without a result (this is a bug)")
	return nil, false
}

// sleepForRetry blocks for d or until ctx is done, whichever comes first. It
// reports false when ctx ended the wait early — the caller must not retry in
// that case, since whoever would receive the retry's result is already gone
// (client disconnect) or the request's own deadline has passed.
//
// d <= 0 is treated as "no wait": it still checks ctx so a context that is
// already done is never silently treated as fine to proceed with.
func sleepForRetry(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeAnthropicError writes {"type":"error","error":{"type":...,"message":...}}
// at the given status, matching the Anthropic API's error shape.
func writeAnthropicError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// forwardUpstreamError maps a non-2xx upstream response to the Anthropic
// error shape, PRESERVING the upstream's exact status code — the caller
// (Claude Code) makes retry/backoff decisions based on that code, so
// collapsing everything to a generic 502 would be a lie about what happened.
func forwardUpstreamError(c *gin.Context, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	errType := "api_error"

	// Most OpenAI-compatible upstreams emit {"error":{"message":...,"type":...}}
	// or {"message":...}; unwrap either so the caller sees a clean message
	// instead of a raw JSON blob, but fall back to the raw body untouched so no
	// information is silently dropped when the shape is unrecognised.
	var nested struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		switch {
		case nested.Error.Message != "":
			msg = nested.Error.Message
			if nested.Error.Type != "" {
				errType = nested.Error.Type
			}
		case nested.Message != "":
			msg = nested.Message
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream returned status %d", resp.StatusCode)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		errType = "authentication_error"
	case http.StatusForbidden:
		errType = "permission_error"
	case http.StatusNotFound:
		errType = "not_found_error"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if errType == "api_error" {
			errType = "invalid_request_error"
		}
	}
	if resp.StatusCode >= 500 {
		errType = "api_error"
	}

	c.AbortWithStatusJSON(resp.StatusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": msg,
		},
	})
}

// respondNonStreaming decodes a complete OpenAI chat-completion response and
// re-encodes it as a single Anthropic message.
func respondNonStreaming(c *gin.Context, body io.Reader, model string) {
	raw, err := io.ReadAll(io.LimitReader(body, 32<<20))
	if err != nil {
		writeAnthropicError(c, http.StatusBadGateway, "api_error", fmt.Sprintf("read upstream response: %v", err))
		return
	}

	var oa openAIChatResponse
	// A malformed body must produce a clean typed error, not a panic — the
	// upstream is an untrusted third party and can send anything.
	if err := json.Unmarshal(raw, &oa); err != nil {
		writeAnthropicError(c, http.StatusBadGateway, "api_error", fmt.Sprintf("upstream returned malformed JSON: %v", err))
		return
	}
	if len(oa.Choices) == 0 {
		writeAnthropicError(c, http.StatusBadGateway, "api_error", "upstream returned no choices")
		return
	}
	choice := oa.Choices[0]

	content := []anthropicContentBlock{}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		content = append(content, anthropicContentBlock{Type: "text", Text: *choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage(`{}`)
		}
		content = append(content, anthropicContentBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
		})
	}

	stopReason := "end_turn"
	if choice.FinishReason != nil {
		stopReason = mapFinishReason(*choice.FinishReason)
	}

	var usage anthropicUsage
	if oa.Usage != nil {
		usage.InputTokens = oa.Usage.PromptTokens
		usage.OutputTokens = oa.Usage.CompletionTokens
	}

	id := oa.ID
	if id == "" {
		id = "msg_unknown"
	}

	c.JSON(http.StatusOK, anthropicMessage{
		ID:           id,
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   &stopReason,
		StopSequence: nil,
		Usage:        usage,
	})
}

// ---------- Streaming ----------
//
// Anthropic's SSE event sequence is: message_start, then for each content
// block content_block_start / content_block_delta* / content_block_stop,
// then message_delta (carrying stop_reason + final usage), then message_stop.
// Every event is flushed individually — buffering several before flushing
// would defeat streaming just as surely as not streaming at all.

type ssePayload = map[string]any

func emitSSE(w gin.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	w.Flush()
}

// streamAnthropicSSE reads an OpenAI-compatible chat-completions SSE stream
// from upstream and re-emits it as an Anthropic Messages SSE stream.
func streamAnthropicSSE(c *gin.Context, upstream io.Reader, model string) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	w.Flush() // open the connection immediately, before the first event

	var (
		msgID          string
		started        bool
		openIndex      = -1
		openType       string // "text" | "tool_use"
		nextBlockIndex int
		toolBlockOf    = map[int]int{} // OpenAI tool_calls[].index -> our block index
		stopReason     = "end_turn"
		inputTokens    int
		outputTokens   int
	)

	ensureStarted := func() {
		if started {
			return
		}
		started = true
		emitSSE(w, "message_start", ssePayload{
			"type": "message_start",
			"message": anthropicMessage{
				ID: msgID, Type: "message", Role: "assistant",
				Content: []anthropicContentBlock{}, Model: model,
				StopReason: nil, StopSequence: nil,
				Usage: anthropicUsage{},
			},
		})
	}

	closeOpenBlock := func() {
		if openIndex < 0 {
			return
		}
		emitSSE(w, "content_block_stop", ssePayload{
			"type": "content_block_stop", "index": openIndex,
		})
		openIndex = -1
		openType = ""
	}

	ensureTextBlock := func() int {
		if openIndex >= 0 && openType == "text" {
			return openIndex
		}
		closeOpenBlock()
		idx := nextBlockIndex
		nextBlockIndex++
		emitSSE(w, "content_block_start", ssePayload{
			"type": "content_block_start", "index": idx,
			"content_block": anthropicContentBlock{Type: "text", Text: ""},
		})
		openIndex, openType = idx, "text"
		return idx
	}

	ensureToolBlock := func(callIdx int, id, name string) int {
		if bi, ok := toolBlockOf[callIdx]; ok && openIndex == bi {
			return bi
		}
		closeOpenBlock()
		idx := nextBlockIndex
		nextBlockIndex++
		toolBlockOf[callIdx] = idx
		emitSSE(w, "content_block_start", ssePayload{
			"type": "content_block_start", "index": idx,
			"content_block": anthropicContentBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(`{}`)},
		})
		openIndex, openType = idx, "tool_use"
		return idx
	}

	reader := bufio.NewReader(upstream)
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if payload, ok := strings.CutPrefix(trimmed, "data:"); ok {
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var chunk openAIStreamChunk
				// A malformed chunk is skipped, not fatal: one bad line from an
				// untrusted upstream must not abort an otherwise-good stream
				// (the response has already started; there is no status code
				// left to change).
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if chunk.ID != "" && msgID == "" {
						msgID = chunk.ID
					}
					if chunk.Usage != nil {
						inputTokens = chunk.Usage.PromptTokens
						outputTokens = chunk.Usage.CompletionTokens
					}
					for _, ch := range chunk.Choices {
						if ch.Delta.Content != "" {
							ensureStarted()
							idx := ensureTextBlock()
							emitSSE(w, "content_block_delta", ssePayload{
								"type": "content_block_delta", "index": idx,
								"delta": ssePayload{"type": "text_delta", "text": ch.Delta.Content},
							})
						}
						for _, tc := range ch.Delta.ToolCalls {
							ensureStarted()
							idx := ensureToolBlock(tc.Index, tc.ID, tc.Function.Name)
							if tc.Function.Arguments != "" {
								emitSSE(w, "content_block_delta", ssePayload{
									"type": "content_block_delta", "index": idx,
									"delta": ssePayload{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
								})
							}
						}
						if ch.FinishReason != nil {
							stopReason = mapFinishReason(*ch.FinishReason)
						}
					}
				}
			}
		}
		if readErr != nil {
			break // EOF or a read error ends the stream; nothing left to send.
		}
	}

	ensureStarted() // covers a degenerate stream that went straight to [DONE]
	closeOpenBlock()

	sr := stopReason
	emitSSE(w, "message_delta", ssePayload{
		"type":  "message_delta",
		"delta": ssePayload{"stop_reason": sr, "stop_sequence": nil},
		"usage": anthropicUsage{InputTokens: inputTokens, OutputTokens: outputTokens},
	})
	emitSSE(w, "message_stop", ssePayload{"type": "message_stop"})
}
