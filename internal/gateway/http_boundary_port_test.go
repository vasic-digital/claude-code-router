package gateway

// Ports test/unit/gateway/http-boundary.test.mjs.
//
// Upstream exercises a large family of pure HTTP-boundary helpers
// (@ccr/core/gateway/http/io.ts, @ccr/core/gateway/http/body.ts) used by
// Node CCR's hand-rolled proxy layer: client identification from
// user-agent/API-key/proxy-mode, Bearer/x-api-key auth-token extraction,
// a remote-control-only query-string auth carve-out, hop-by-hop/local-auth/
// observability header stripping on the way OUT to the internal core
// process, x-ccr-core-auth injection on the way IN to it, response-header
// filtering on the way back to the client, and a family of JSON body
// helpers (object-only parsing, a parse cache, ownership-transfer/release).
//
// Most of this has no counterpart here because the architecture is
// different in kind, not just in detail: Node CCR runs a renderer/core
// process split with an internal HTTP hop between them (hence
// "core-gateway auth header", "remote control" query tokens, a JSON parse
// cache keyed by ownership semantics). This repository's gateway is a
// single process with no internal hop, so:
//   - inferGatewayClient (proxy-mode/user-agent client identification) — N/A,
//     no client-identity/observability concept exists here at all.
//   - withCoreGatewayAuthHeader / readRemoteControlQueryAuthToken — N/A, no
//     core/gateway process split, so there is no internal auth boundary to
//     scope a query-string token to.
//   - parseJsonObjectCached/takeJsonObject/releaseJsonObject — N/A, a
//     Node-specific allocation-avoidance pattern; internal/translate decodes
//     each request once into typed Go structs and does not need a manual
//     parse cache.
//   - shouldSendBody(method) — N/A, the gateway's Upstream seam always POSTs
//     a body; there is no GET/HEAD upstream call to gate.
//
// Two pieces describe real, in-scope gateway behaviour:
//   - inbound Authorization/x-api-key parsing is genuinely missing (GAP);
//   - response-header filtering, once messages.go's handleMessages/
//     streamAnthropicSSE/respondNonStreaming existed to relay a response at
//     all, turned out to already hold — PORTED, not GAP, see below.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// TestInboundAuthTokenParsing_GAP documents upstream's readAuthToken/
// readHeader contract: a client may authenticate with either
// "Authorization: Bearer <token>" or "x-api-key: <token>", both trimmed of
// surrounding whitespace, and a header supplied as an array (as Node's http
// module does for repeated headers) resolves to its first value.
//
// This repository's gateway has no inbound-authentication concept at all:
// handleMessages (internal/gateway/messages.go) never reads c.Request's
// Authorization or x-api-key header — it goes straight from body to router
// to upstream. That may be intentional for a purely-localhost-bound default
// (opt.Host defaults to 127.0.0.1), but once EnableHTTP3/TLS or a
// non-default Host is used the gateway would accept unauthenticated
// requests from any reachable caller.
func TestInboundAuthTokenParsing_GAP(t *testing.T) {
	cases := []struct {
		headers map[string]string
		want    string
	}{
		{map[string]string{"authorization": " Bearer secret-token "}, "secret-token"},
		{map[string]string{"x-api-key": " api-key-token "}, "api-key-token"},
		{map[string]string{}, ""},
	}
	_ = cases
	t.Skip("GAP: no inbound Authorization/x-api-key token parsing exists anywhere in " +
		"this repository; handleMessages does not authenticate incoming requests at all. " +
		"(upstream: test/unit/gateway/http-boundary.test.mjs)")
}

// TestUpstreamResponseHeaderNeverLeaksToClient is the PORTED counterpart of
// upstream's filteredResponseHeaders (an allowlist of content-type/
// x-request-id-shaped headers survives the relay to the client; hop-by-hop
// and transport headers like "connection"/"content-encoding" do not).
//
// handleMessages (internal/gateway/messages.go, added after this
// test-porting task began) achieves a STRICTER version of the same
// guarantee by construction rather than by filtering a forwarded set: both
// respondNonStreaming and streamAnthropicSSE build the client response
// entirely from the upstream response BODY, and never read or copy a single
// header off the upstream *http.Response at all. So whatever an upstream
// sends — including headers upstream's own sanitizer would need to
// explicitly strip, like "Connection: close" or a stale
// "Content-Encoding" — categorically cannot reach the client, in either the
// streaming or non-streaming path.
func TestUpstreamResponseHeaderNeverLeaksToClient(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{"non-streaming", false},
		{"streaming", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A header no legitimate client-facing response would ever
				// carry — the one an eavesdropping client absolutely must
				// never see. (Content-Encoding/Connection are deliberately
				// NOT used here: net/http's own transport transparently
				// strips/decodes Content-Encoding on the way in, and
				// streamAnthropicSSE legitimately sets its OWN Connection
				// value for SSE keep-alive, so neither is a clean signal for
				// "did an upstream header leak" — this header name is.)
				w.Header().Set("X-Upstream-Internal-Secret", "must-not-leak")
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, "data: [DONE]\n\n")
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()

			cfg := &config.Config{
				Providers: []config.Provider{{Name: "p", APIBaseURL: upstream.URL, APIKey: "k", Models: []string{"m"}}},
				Router:    config.Route{Default: "p,m"},
			}
			s := New(cfg, Options{})
			body, _ := json.Marshal(map[string]any{
				"model": "claude-3-5-sonnet", "max_tokens": 10, "stream": tc.stream,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if v := rec.Header().Get("X-Upstream-Internal-Secret"); v != "" {
				t.Errorf("client response leaked an upstream-only header: X-Upstream-Internal-Secret: %q", v)
			}
		})
	}
}
