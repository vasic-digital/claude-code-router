package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// flushCountingRecorder wraps httptest.ResponseRecorder to count how many
// times Flush was actually invoked — the recorder's own Flushed field is
// just a bool, which cannot prove "every event was flushed", only "at least
// one was".
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCountingRecorder) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

func testServerWithUpstream(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "fake",
			APIBaseURL: upstreamURL,
			APIKey:     "sk-test",
			Models:     []string{"fake-model"},
		}},
		Router: config.Route{Default: "fake,fake-model"},
	}
	return New(cfg, Options{})
}

func anthropicReqBody(stream bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"stream":     stream,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	return b
}

// --- Non-streaming round trip ---

func TestNonStreamingRoundTripProducesValidAnthropicJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" {
			t.Fatalf("unexpected empty path")
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("upstream: decode request: %v", err)
		}
		if req["model"] != "fake-model" {
			t.Errorf("upstream saw model = %v, want fake-model (the routed model)", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "chatcmpl-1",
			"choices": [{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":5,"completion_tokens":3}
		}`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var msg anthropicMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response is not valid Anthropic JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("type/role = %q/%q", msg.Type, msg.Role)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hello there" {
		t.Fatalf("content = %+v", msg.Content)
	}
	if msg.StopReason == nil || *msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg.StopReason)
	}
	if msg.Usage.InputTokens != 5 || msg.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", msg.Usage)
	}
}

// --- SSE streaming: event order, per-event flush, [DONE] termination ---

func TestStreamingProducesCorrectEventOrderAndFlushesEvery(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		lines := []string{
			`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`{"id":"chatcmpl-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-2","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		}
		for _, l := range lines {
			fmt.Fprintf(w, "data: %s\n\n", l)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(true)))
	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	var events []string
	var deltas []string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var d map[string]any
			if json.Unmarshal([]byte(data), &d) == nil {
				if d["type"] == "content_block_delta" {
					if delta, ok := d["delta"].(map[string]any); ok {
						if txt, ok := delta["text"].(string); ok {
							deltas = append(deltas, txt)
						}
					}
				}
			}
		}
	}

	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i, ev := range want {
		if events[i] != ev {
			t.Errorf("event[%d] = %q, want %q (full sequence: %v)", i, events[i], ev, events)
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Errorf("streamed text = %q, want \"Hello\"", strings.Join(deltas, ""))
	}

	// Every SSE event must be individually flushed, not just the response as a
	// whole — the gateway's own comments call this out as the difference
	// between real streaming and something that merely claims to stream.
	if rec.flushes < len(want) {
		t.Errorf("flushes = %d, want at least %d (one per event)", rec.flushes, len(want))
	}

	// message_delta must carry stop_reason + final usage.
	if !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("message_delta missing stop_reason end_turn:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":5`) || !strings.Contains(rec.Body.String(), `"output_tokens":2`) {
		t.Errorf("final usage missing/incorrect:\n%s", rec.Body.String())
	}
}

// A stream that never sends any delta (immediate [DONE]) must still produce a
// well-formed, terminated event sequence rather than hanging or omitting
// message_start.
func TestStreamingHandlesImmediateDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(true)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, ev := range []string{"message_start", "message_delta", "message_stop"} {
		if !strings.Contains(body, "event: "+ev) {
			t.Errorf("missing event %q in:\n%s", ev, body)
		}
	}
}

// --- Error mapping ---

func TestUpstream4xxMapsToAnthropicErrorPreservingStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (preserved from upstream)", rec.Code)
	}
	assertAnthropicErrorShape(t, rec.Body.Bytes(), "rate limited")
}

func TestUpstream5xxMapsToAnthropicErrorPreservingStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal error, not even JSON`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (preserved from upstream)", rec.Code)
	}
	assertAnthropicErrorShape(t, rec.Body.Bytes(), "internal error, not even JSON")
}

// --- Malformed upstream JSON must not panic ---

func TestMalformedUpstreamJSONIsCleanErrorNotPanic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `not json at all {{{`)
	}))
	defer upstream.Close()

	s := testServerWithUpstream(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()

	// A panic here would fail the test via gin's recovery -> 500 with a
	// generic gin body, OR (without recovery) crash the test binary; either
	// way this call must return normally.
	s.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("malformed upstream body must not be reported as success, got 200: %s", rec.Body.String())
	}
	assertAnthropicErrorShape(t, rec.Body.Bytes(), "")
}

func assertAnthropicErrorShape(t *testing.T, body []byte, wantMessageContains string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, body)
	}
	if e.Type != "error" {
		t.Errorf("top-level type = %q, want \"error\"", e.Type)
	}
	if e.Error.Type == "" {
		t.Errorf("error.type is empty")
	}
	if e.Error.Message == "" {
		t.Errorf("error.message is empty")
	}
	if wantMessageContains != "" && !strings.Contains(e.Error.Message, wantMessageContains) {
		t.Errorf("error.message = %q, want it to contain %q", e.Error.Message, wantMessageContains)
	}
}
