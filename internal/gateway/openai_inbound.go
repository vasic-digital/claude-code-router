package gateway

// OpenAI-compatible inbound facade.
//
// Alongside the primary Anthropic Messages endpoint (POST /v1/messages), the
// gateway accepts OpenAI chat-completions requests (POST /v1/chat/completions,
// and the /proxy/v1/... alias) so an OpenAI-SDK client can reach any routed
// provider. Because every provider the toolkit configures today is itself
// OpenAI-shaped, the common path is a near-passthrough: the client's OpenAI
// request is forwarded with only the routed model overridden, and the
// provider's OpenAI response is relayed straight back to the client, which
// asked for OpenAI shape.
//
// The one case this facade does NOT yet serve is an OpenAI-inbound request that
// routes to an Anthropic-NATIVE provider: that needs OpenAI->Anthropic request
// AND response translation (the reverse of internal/translate), which is not
// implemented. Rather than send an OpenAI body to a Messages API that will
// reject it with a confusing error, the facade fails with an explicit 501 that
// names the provider and the two supported alternatives. No real config today
// has an Anthropic-native provider, so this path is unreachable in practice —
// but it is handled honestly rather than silently mishandled.

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

	"github.com/gin-gonic/gin"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// handleInbound is the classifier-driven entrypoint every routable POST path is
// registered to. It uses requestProtocolForPath (the ported classifier) to
// dispatch to the handler for the request's protocol family, so that classifier
// is genuinely load-bearing on every inbound request rather than dead code.
func (s *Server) handleInbound(c *gin.Context) {
	switch requestProtocolForPath(c.Request.URL.Path) {
	case protoAnthropicMessages:
		s.handleMessages(c)
	case protoOpenAIChatCompletions:
		s.handleOpenAIChatCompletions(c)
	default:
		// Unreachable given the registered routes (routes() only mounts paths
		// that classify to the two served families), but fail explicitly rather
		// than silently 200 a path we do not actually handle.
		writeAnthropicError(c, http.StatusNotFound, "not_found_error",
			"unsupported inbound protocol for path "+c.Request.URL.Path)
	}
}

// openAIInboundRequest is the minimal shape the facade needs from an inbound
// OpenAI chat-completions body: the model (for routing and override) and the
// stream flag (to choose the response path). Every other field is forwarded to
// the upstream verbatim, so nothing else needs modelling.
type openAIInboundRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleOpenAIChatCompletions(c *gin.Context) {
	// Same inbound-body cap as /v1/messages: a distinct 413 (via MaxBytesReader)
	// rather than a truncated body surfacing as a confusing parse error.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds the %d-byte limit", maxRequestBodyBytes))
			return
		}
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("read request body: %v", err))
		return
	}

	var in openAIInboundRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	// Reuse the same model-based router. It selects on model/stream, which a
	// minimal AnthropicRequest carries — no OpenAI-specific Router method is
	// needed, and the fuller internal/router selection logic applies unchanged.
	//
	// CAVEAT (no bluff): because this AnthropicRequest carries only Model+Stream
	// (never the OpenAI body's messages), internal/router's content-based
	// LongContext tier cannot see this request's true size — it estimates ~0
	// tokens and never trips, so a large /v1/chat/completions body routes to
	// Router.Default rather than Router.LongContext. See estimateTokenCount's
	// doc in internal/router/selector.go. Estimating from the OpenAI body is a
	// documented future item.
	provider, model, rerr := s.Router.Route(&translate.AnthropicRequest{Model: in.Model, Stream: in.Stream})
	if rerr != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "not_found_error", rerr.Error())
		return
	}

	if provider.ResolvedProtocol() == config.ProtocolAnthropic {
		writeOpenAIError(c, http.StatusNotImplemented, "invalid_request_error",
			fmt.Sprintf("model %q routes to Anthropic-native provider %q; the OpenAI-compatible inbound endpoint "+
				"cannot bridge to an Anthropic upstream yet — route this model to an OpenAI-shaped provider, or call POST /v1/messages",
				in.Model, provider.Name))
		return
	}

	outBody, err := overrideModelField(body, model)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "api_error", fmt.Sprintf("encode upstream request: %v", err))
		return
	}

	ctx := c.Request.Context()
	if !in.Stream {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opt.UpstreamTimeout)
		defer cancel()
	}

	// Attribute the upstream call for observability, symmetric with the
	// Anthropic single-provider branch (handleMessages). This facade does no
	// cross-provider fallback — it calls doUpstreamWithRetry with canFallback
	// false — so exactly one provider is ever attempted, hence one record here.
	// Without this, a /v1/chat/completions request reached the upstream yet
	// contributed nothing to ccr_gen_ai_upstream_requests_total; only the RED
	// ccr_http_requests_total middleware counted it — an attribution gap.
	if s.Metrics != nil {
		s.Metrics.RecordUpstream(provider.Name, model)
	}

	resp, ok, _ := s.doUpstreamWithRetry(c, ctx, provider, outBody, openAIResponder{}, false)
	if !ok {
		return
	}
	defer resp.Body.Close()

	s.relayOpenAIResponse(c, resp, in.Stream, provider.Name, model)
}

// overrideModelField sets the top-level "model" on a JSON object body while
// preserving every other field verbatim (UseNumber keeps large/high-precision
// literals intact, as elsewhere). An empty model leaves the body unchanged.
func overrideModelField(raw []byte, model string) ([]byte, error) {
	if model == "" {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	m["model"] = model
	return json.Marshal(v)
}

// relayOpenAIResponse forwards an OpenAI-shaped upstream response to the client
// unchanged (the client asked for OpenAI shape and the provider produced it).
// Like every other response path here, it copies NO upstream header — the
// response is rebuilt from the body alone.
//
// Both paths record token usage for observability. Non-streaming parses the
// OpenAI usage block from the SAME buffered body it relays (read-only). Streaming
// tees the SSE through openAIStreamUsageScanner as it forwards each chunk
// verbatim, recording once at stream end. Best-effort: an OpenAI stream only
// carries a usage chunk when the client requested stream_options.include_usage —
// the facade forwards the body verbatim and never injects it, so a client that
// did not ask for usage legitimately records 0 (documented, not a bug).
func (s *Server) relayOpenAIResponse(c *gin.Context, resp *http.Response, stream bool, provider, model string) {
	if stream {
		in, out := relayRawStream(c.Writer, resp.Body, &openAIStreamUsageScanner{})
		if s.Metrics != nil {
			s.Metrics.RecordTokens(provider, model, in, out)
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponseBytes))
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "api_error", fmt.Sprintf("read upstream response: %v", err))
		return
	}
	// Best-effort token accounting: the body is OpenAI-shaped, so its
	// usage.{prompt,completion}_tokens are read directly. A body without a usage
	// block (or one that does not parse) simply records nothing — RecordTokens
	// ignores non-positive counts. The bytes relayed below are unchanged.
	if s.Metrics != nil {
		var oa openAIChatResponse
		if json.Unmarshal(raw, &oa) == nil && oa.Usage != nil {
			s.Metrics.RecordTokens(provider, model, oa.Usage.PromptTokens, oa.Usage.CompletionTokens)
		}
	}
	c.Data(http.StatusOK, "application/json", raw)
}

// streamUsageScanner observes each raw SSE chunk as relayRawStream forwards it
// verbatim, extracting token usage without buffering the stream. totals() is
// read ONCE after the relay loop ends, so at most one RecordTokens call happens
// per request — no double-count.
type streamUsageScanner interface {
	observe(chunk string)
	totals() (input, output int)
}

// sseData returns the JSON payload of an SSE `data:` line, or "" for any other
// line (event:/id: fields, blanks, or the terminal "[DONE]" sentinel). Parsing
// is best-effort: a malformed line from an untrusted upstream yields "" and is
// skipped, never aborting the relay.
func sseData(chunk string) string {
	line := strings.TrimSpace(chunk)
	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return ""
	}
	return payload
}

// openAIStreamUsageScanner extracts prompt/completion tokens from the terminal
// usage chunk of an OpenAI chat-completions SSE stream. The last positive value
// of each wins; a stream without a usage chunk yields 0/0.
type openAIStreamUsageScanner struct{ input, output int }

func (s *openAIStreamUsageScanner) observe(chunk string) {
	payload := sseData(chunk)
	if payload == "" {
		return
	}
	var c openAIStreamChunk
	if json.Unmarshal([]byte(payload), &c) != nil || c.Usage == nil {
		return
	}
	if c.Usage.PromptTokens > 0 {
		s.input = c.Usage.PromptTokens
	}
	if c.Usage.CompletionTokens > 0 {
		s.output = c.Usage.CompletionTokens
	}
}

func (s *openAIStreamUsageScanner) totals() (int, int) { return s.input, s.output }

// anthropicUsageEvent is the minimal shape needed to read usage out of an
// Anthropic-native SSE stream: input_tokens arrive in message_start
// (message.usage), output_tokens in message_delta (usage).
type anthropicUsageEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *anthropicUsage `json:"usage"`
}

// anthropicStreamUsageScanner extracts usage from an Anthropic-native SSE
// stream. The last positive input/output seen wins (message_delta usage is
// cumulative in the Anthropic spec, so the final delta carries the total).
type anthropicStreamUsageScanner struct{ input, output int }

func (s *anthropicStreamUsageScanner) observe(chunk string) {
	payload := sseData(chunk)
	if payload == "" {
		return
	}
	var ev anthropicUsageEvent
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Message != nil && ev.Message.Usage != nil && ev.Message.Usage.InputTokens > 0 {
		s.input = ev.Message.Usage.InputTokens
	}
	if ev.Usage != nil {
		if ev.Usage.InputTokens > 0 {
			s.input = ev.Usage.InputTokens
		}
		if ev.Usage.OutputTokens > 0 {
			s.output = ev.Usage.OutputTokens
		}
	}
}

func (s *anthropicStreamUsageScanner) totals() (int, int) { return s.input, s.output }

// relayRawStream copies an SSE body through to the client line by line,
// flushing each line so events arrive as they are produced. Shared by the
// Anthropic-native and OpenAI relay paths. Only the Content-Type/cache headers
// this gateway itself sets are written; no upstream header is forwarded.
//
// If scan is non-nil it observes each chunk (after the verbatim write, so the
// client bytes are never affected) and relayRawStream returns the accumulated
// (input, output) token totals for the caller to record once; a nil scan
// returns 0, 0.
func relayRawStream(w gin.ResponseWriter, r io.Reader, scan streamUsageScanner) (input, output int) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	w.Flush() // open the connection immediately, before the first event

	reader := bufio.NewReader(r)
	for {
		chunk, readErr := reader.ReadString('\n')
		if len(chunk) > 0 {
			if _, werr := io.WriteString(w, chunk); werr != nil {
				break // client hung up; nothing more we can do
			}
			w.Flush()
			if scan != nil {
				scan.observe(chunk)
			}
		}
		if readErr != nil {
			break
		}
	}
	if scan != nil {
		return scan.totals()
	}
	return 0, 0
}

// ---------- Error-envelope strategy ----------
//
// The retry loop (doUpstreamWithRetry) is shared by both inbound facades, but a
// failure must be reported in the envelope the CLIENT expects: Anthropic shape
// for /v1/messages, OpenAI shape for /v1/chat/completions. errorResponder is
// that strategy; each handler passes the responder matching its inbound family.

type errorResponder interface {
	writeError(c *gin.Context, status int, errType, message string)
	forwardUpstream(c *gin.Context, resp *http.Response)
}

// anthropicResponder emits the Anthropic error shape (the pre-existing
// behaviour of /v1/messages, unchanged).
type anthropicResponder struct{}

func (anthropicResponder) writeError(c *gin.Context, status int, errType, message string) {
	writeAnthropicError(c, status, errType, message)
}
func (anthropicResponder) forwardUpstream(c *gin.Context, resp *http.Response) {
	forwardUpstreamError(c, resp)
}

// openAIResponder emits the OpenAI error shape:
// {"error":{"message":...,"type":...,"code":null}}.
type openAIResponder struct{}

func (openAIResponder) writeError(c *gin.Context, status int, errType, message string) {
	writeOpenAIError(c, status, errType, message)
}
func (openAIResponder) forwardUpstream(c *gin.Context, resp *http.Response) {
	forwardOpenAIUpstreamError(c, resp)
}

// writeOpenAIError writes the OpenAI error envelope at the given status.
func writeOpenAIError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"code":    nil,
		},
	})
}

// forwardOpenAIUpstreamError maps a non-2xx OpenAI upstream response to the
// client, PRESERVING the upstream status. If the upstream body is already an
// OpenAI error object it is relayed verbatim; otherwise the raw text is wrapped
// in the OpenAI error envelope so the client always sees a well-formed error.
func forwardOpenAIUpstreamError(c *gin.Context, resp *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &probe) == nil && len(probe.Error) > 0 {
		// Already an OpenAI-shaped {"error":{...}} — pass it through untouched.
		c.Data(resp.StatusCode, "application/json", raw)
		c.Abort()
		return
	}

	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		msg = fmt.Sprintf("upstream returned status %d", resp.StatusCode)
	}
	writeOpenAIError(c, resp.StatusCode, openAIErrTypeForStatus(resp.StatusCode), msg)
}

// openAIErrTypeForStatus maps an HTTP status to an OpenAI error "type".
func openAIErrTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}
