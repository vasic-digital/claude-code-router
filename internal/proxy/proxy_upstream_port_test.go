package proxy

// Ports test/unit/proxy/proxy-upstream.test.mjs.
//
// Upstream Node CCR lets an operator route the gateway's OWN outbound calls
// to providers through a corporate/custom HTTP proxy
// (config.proxy.upstream: {mode: "custom", custom: {server, port, username,
// password}}), building a proxy URL with percent-encoded userinfo
// (customUpstreamProxyFromConfig / upstreamProxyUrl) and a matching HTTP
// Basic-Auth "Proxy-Authorization"-shaped header
// (upstreamProxyAuthorizationHeader). mode:"none" or an incomplete custom
// config (any of server/username/password empty) yields no proxy at all —
// silently falling through to a direct connection rather than half-applying
// a broken proxy.
//
// This repository's proxy.Client (see proxy.go's New) builds its transport
// by cloning http.DefaultTransport and setting only ResponseHeaderTimeout;
// there is no config surface for an outbound HTTP/SOCKS proxy anywhere —
// config.Config has no `proxy` section, and proxy.New takes only a timeout.
// An operator behind a corporate proxy that requires authenticated egress
// has no way to route this gateway's upstream calls through it (Go's
// http.ProxyFromEnvironment via HTTP_PROXY/HTTPS_PROXY env vars is the only
// avenue, and it is not exercised or documented anywhere in this package).

import "testing"

// TestCustomUpstreamProxyURLConstruction_GAP documents the exact URL and
// Basic-Auth header upstream constructs from a custom proxy config,
// including percent-encoding of "@"/":" in the username/password so a
// literal "@" in either does not truncate the userinfo early.
func TestCustomUpstreamProxyURLConstruction_GAP(t *testing.T) {
	type customProxy struct {
		server, username, password string
		port                       int
	}
	cfg := customProxy{server: "http://proxy.example.com:8888", username: "alice@example.com", password: "pa:ss", port: 8888}
	wantURL := "http://alice%40example.com:pa%3Ass@proxy.example.com:8888"
	_ = cfg
	_ = wantURL
	t.Skip("GAP: config.Config has no outbound/upstream proxy section at all, and " +
		"proxy.New / proxy.Client have no proxy-URL construction of any kind — Do() builds " +
		"an http.Client from http.DefaultTransport.Clone() with only ResponseHeaderTimeout " +
		"set. An operator cannot route this gateway's calls to providers through an " +
		"authenticated corporate proxy. (upstream: test/unit/proxy/proxy-upstream.test.mjs)")
}

// TestCustomUpstreamProxyNoneOrIncomplete_GAP documents the "no proxy
// configured" fallthrough: mode:"none", or mode:"custom" with any of
// server/username/password left empty, must not construct a (broken) proxy.
func TestCustomUpstreamProxyNoneOrIncomplete_GAP(t *testing.T) {
	t.Skip("GAP: there is no proxy-config validation to port because there is no proxy " +
		"config surface at all (see TestCustomUpstreamProxyURLConstruction_GAP). (upstream: " +
		"test/unit/proxy/proxy-upstream.test.mjs)")
}
