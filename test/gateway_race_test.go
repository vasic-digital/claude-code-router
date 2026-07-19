// Package test holds cross-package integration tests that exercise the
// gateway as a whole rather than a single internal package in isolation.
//
// This file specifically drives the real gateway HTTP handler concurrently
// to prove the request path (compression middleware, routing, translation,
// both non-streaming and SSE-streaming response handling) has no data races
// under -race, and that every concurrent response is well-formed.
package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
)

// fakeUpstream serves both non-streaming and SSE-streaming OpenAI-shaped
// chat-completions responses, branching on the "stream" field of the
// translated request body — the same signal the real gateway forwards from
// the incoming Anthropic request's "stream" flag. It is read by many
// concurrent goroutines (via net/http's own server loop, one goroutine per
// connection), so it must not touch any shared mutable state itself; it
// doesn't, every value here is request-local.
func fakeUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		if req.Stream {
			fl, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flusher", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			lines := []string{
				`{"id":"chatcmpl-race","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"id":"chatcmpl-race","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
				`{"id":"chatcmpl-race","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
				`{"id":"chatcmpl-race","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
			for _, l := range lines {
				fmt.Fprintf(w, "data: %s\n\n", l)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "chatcmpl-race",
			"choices": [{"index":0,"message":{"role":"assistant","content":"concurrent hello"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":3,"completion_tokens":2}
		}`)
	}))
}

func raceTestServer(upstreamURL string) *gateway.Server {
	cfg := &config.Config{
		Providers: []config.Provider{{
			Name:       "race-provider",
			APIBaseURL: upstreamURL,
			APIKey:     "sk-race-test",
			Models:     []string{"race-model"},
		}},
		Router: config.Route{Default: "race-provider,race-model"},
	}
	return gateway.New(cfg, gateway.Options{})
}

func anthropicRaceBody(stream bool, worker int) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"stream":     stream,
		"messages": []map[string]any{
			{"role": "user", "content": fmt.Sprintf("hello from worker %d", worker)},
		},
	})
	return b
}

// TestConcurrentGatewayRequestsHaveNoDataRace drives the real gateway
// handler — compression middleware, routing, Anthropic<->OpenAI translation,
// both non-streaming JSON and SSE-streaming responses — from 50 goroutines
// at once. Run with `go test -race` to get the actual guarantee; run without
// it, this still asserts every concurrent response is well-formed.
func TestConcurrentGatewayRequestsHaveNoDataRace(t *testing.T) {
	upstream := fakeUpstream()
	defer upstream.Close()

	s := raceTestServer(upstream.URL)
	handler := s.Handler()

	const numWorkers = 50
	var (
		wg          sync.WaitGroup
		okCount     int64
		failCount   int64
		streamCount int64
		jsonCount   int64
	)

	acceptEncodings := []string{"", "gzip", "br", "gzip, br"}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			stream := worker%2 == 0
			body := anthropicRaceBody(stream, worker)

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", acceptEncodings[worker%len(acceptEncodings)])
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				atomic.AddInt64(&failCount, 1)
				t.Errorf("worker %d: status = %d, want 200, body = %s", worker, rec.Code, rec.Body.String())
				return
			}

			// Decompress if the middleware compressed the body, mirroring
			// what a real client would do, so well-formedness is checked on
			// the actual bytes a caller would see.
			decoded, err := decompressBody(rec.Header().Get("Content-Encoding"), rec.Body.Bytes())
			if err != nil {
				atomic.AddInt64(&failCount, 1)
				t.Errorf("worker %d: failed to decompress response (encoding=%q): %v", worker, rec.Header().Get("Content-Encoding"), err)
				return
			}

			if stream {
				if !assertWellFormedSSE(t, worker, decoded) {
					atomic.AddInt64(&failCount, 1)
					return
				}
				atomic.AddInt64(&streamCount, 1)
			} else {
				if !assertWellFormedAnthropicJSON(t, worker, decoded) {
					atomic.AddInt64(&failCount, 1)
					return
				}
				atomic.AddInt64(&jsonCount, 1)
			}
			atomic.AddInt64(&okCount, 1)
		}(i)
	}

	wg.Wait()

	if failCount != 0 {
		t.Fatalf("%d/%d workers failed (see individual errors above)", failCount, numWorkers)
	}
	if okCount != numWorkers {
		t.Fatalf("okCount = %d, want %d", okCount, numWorkers)
	}
	if streamCount+jsonCount != numWorkers {
		t.Fatalf("streamCount(%d) + jsonCount(%d) != numWorkers(%d)", streamCount, jsonCount, numWorkers)
	}
	t.Logf("50-goroutine concurrent run: %d streaming responses, %d non-streaming responses, all well-formed",
		streamCount, jsonCount)
}

func assertWellFormedAnthropicJSON(t *testing.T, worker int, body []byte) bool {
	t.Helper()
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason *string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Errorf("worker %d: non-streaming response is not valid JSON: %v\nbody: %s", worker, err, body)
		return false
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("worker %d: type/role = %q/%q, want message/assistant", worker, msg.Type, msg.Role)
		return false
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "concurrent hello" {
		t.Errorf("worker %d: content = %+v, want a single text block \"concurrent hello\"", worker, msg.Content)
		return false
	}
	if msg.StopReason == nil || *msg.StopReason != "end_turn" {
		t.Errorf("worker %d: stop_reason = %v, want end_turn", worker, msg.StopReason)
		return false
	}
	return true
}

func assertWellFormedSSE(t *testing.T, worker int, body []byte) bool {
	t.Helper()
	text := string(body)
	var events []string
	for _, line := range strings.Split(text, "\n") {
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
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
		t.Errorf("worker %d: events = %v, want %v", worker, events, want)
		return false
	}
	for i, ev := range want {
		if events[i] != ev {
			t.Errorf("worker %d: event[%d] = %q, want %q (full: %v)", worker, i, events[i], ev, events)
			return false
		}
	}
	if !strings.Contains(text, `"text":"Hel"`) || !strings.Contains(text, `"text":"lo"`) {
		t.Errorf("worker %d: streamed deltas missing from body:\n%s", worker, text)
		return false
	}
	return true
}
