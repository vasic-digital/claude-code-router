package gateway

// Ports test/integration/gateway/gateway-client-disconnect.test.mjs
// ("gateway treats downstream client aborts as expected stream cleanup").
//
// Upstream spins up a real upstream SSE server and a real gateway, starts a
// streaming request, reads one chunk, then cancels the client's reader and
// aborts the fetch — asserting (a) the upstream connection actually closes
// (cleanup happened, not a leaked connection ticking forever) and (b)
// neither an uncaughtException nor an unhandledRejection was raised (no
// crash from writing to a socket that is already gone).
//
// This was N/A when this test-porting task began, because
// internal/gateway/messages.go (the actual /v1/messages streaming relay)
// did not exist yet in this snapshot. It has since landed (added by another
// agent concurrently with this change, and remains off-limits to edit
// here), so the real behaviour is now directly testable — and it turns out
// to be PORTED: handleMessages threads c.Request.Context() through to
// s.Upstream.Do (see the "ctx := c.Request.Context()" / "s.Upstream.Do(ctx,
// ...)" lines), and Go's net/http server cancels that context the moment
// the client connection goes away. Because the SAME context governs the
// outbound upstream request, http.Client propagates the cancellation to the
// upstream connection automatically — no bespoke AbortController wiring is
// needed the way upstream's Node implementation requires, Go's context
// propagation gives this "for free" once the ctx threading itself is
// correct (which it is; see TestClientDisconnectClosesUpstreamConnection
// below).
//
// This uses real net/http servers rather than httptest.ResponseRecorder,
// because a recorder has no real underlying connection to close — the
// disconnect signal upstream tests for does not exist without one.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func TestClientDisconnectClosesUpstreamConnection(t *testing.T) {
	var upstreamSawCancellation atomic.Bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// A long-lived tick stream, mirroring upstream's setInterval(...) —
		// it must be interrupted by the client disconnect rather than
		// running to completion.
		for i := 0; i < 200; i++ {
			select {
			case <-r.Context().Done():
				upstreamSawCancellation.Store(true)
				return
			default:
			}
			fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tick\"}}]}\n\n")
			fl.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Providers: []config.Provider{{Name: "p", APIBaseURL: upstream.URL, APIKey: "k", Models: []string{"m"}}},
		Router:    config.Route{Default: "p,m"},
	}
	s := New(cfg, Options{})
	gw := httptest.NewServer(s.Handler())
	defer gw.Close()

	body, _ := json.Marshal(map[string]any{
		"model": "claude-3-5-sonnet", "max_tokens": 10, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Read at least one real chunk so the stream is genuinely flowing before
	// tearing it down — cancelling before anything was ever sent would not
	// exercise mid-stream cleanup at all.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}

	// The downstream client aborts: cancel the request context and close the
	// body, exactly like reader.cancel() + controller.abort() upstream.
	cancel()
	_ = resp.Body.Close()

	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if upstreamSawCancellation.Load() {
			return // PASS: the upstream connection was torn down promptly.
		}
		select {
		case <-deadline:
			t.Fatal("upstream never observed the client disconnect within 2s — the " +
				"gateway is not propagating client cancellation to the upstream request")
		case <-tick.C:
		}
	}
}
