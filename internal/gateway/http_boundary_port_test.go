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
//   - shouldSendBody(method) — N/A, proxy.Client.Do always POSTs a body; the
//     gateway never needs a GET/HEAD upstream call.
//
// Two pieces, however, describe real, in-scope gateway behaviour this
// repository does not implement yet and plausibly should once the
// /v1/messages handler (internal/gateway/messages.go, out of scope for this
// change) exists — those are recorded as GAPs below.

import "testing"

// TestInboundAuthTokenParsing_GAP documents upstream's readAuthToken/
// readHeader contract: a client may authenticate with either
// "Authorization: Bearer <token>" or "x-api-key: <token>", both trimmed of
// surrounding whitespace, and a header supplied as an array (as Node's http
// module does for repeated headers) resolves to its first value.
//
// This repository's gateway has no inbound-authentication concept at all:
// grep the package for "Authorization" and the only hits are proxy.Client.Do
// setting the OUTBOUND header from config.Provider.APIKey — nothing reads an
// incoming Authorization/x-api-key header from a Claude Code request. That
// may be intentional for a purely-localhost-bound default (opt.Host
// defaults to 127.0.0.1), but once EnableHTTP3/TLS or a non-default Host is
// used the gateway would accept unauthenticated requests from any reachable
// caller.
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
		"this repository; the gateway does not authenticate incoming requests at all. " +
		"(upstream: test/unit/gateway/http-boundary.test.mjs)")
}

// TestUpstreamResponseHeaderFiltering_GAP documents upstream's
// filteredResponseHeaders contract: when relaying an upstream provider's
// response back to the client, only a small allowlist of headers survives
// (content-type, x-request-id-shaped headers); hop-by-hop headers like
// "connection" and transport headers like "content-encoding" are dropped
// because the gateway's own encoding (see compress.go) — not the upstream's
// — governs what actually goes over the wire to Claude Code.
//
// proxy.Client.Do returns the raw *http.Response completely unfiltered;
// nothing in this repository currently relays that response to a client at
// all (no /v1/messages handler exists yet), so there is no header-filtering
// step for this test to exercise. Once that relay exists, it must not
// blindly copy every upstream response header — in particular a stale
// upstream Content-Encoding or Connection header reaching the client would
// conflict with compress.go's own negotiated encoding.
func TestUpstreamResponseHeaderFiltering_GAP(t *testing.T) {
	upstream := map[string]string{
		"connection":       "close",
		"content-encoding": "gzip",
		"content-type":     "application/json",
		"x-request-id":     "request-1",
	}
	want := map[string]string{
		"content-type": "application/json",
		"x-request-id": "request-1",
	}
	_, _ = upstream, want
	t.Skip("GAP: no response-header filtering exists when relaying an upstream response " +
		"to the client, because no code path relays an upstream response to a client yet " +
		"(no /v1/messages handler in this snapshot). (upstream: " +
		"test/unit/gateway/http-boundary.test.mjs)")
}
