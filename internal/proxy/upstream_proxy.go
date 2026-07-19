package proxy

// Outbound/corporate-proxy support for this package's http.Client.
//
// Ports the behavioural intent of upstream Node CCR's
// config.proxy.upstream: {mode: "custom", custom: {server, port, username,
// password}} — customUpstreamProxyFromConfig / upstreamProxyUrl /
// upstreamProxyAuthorizationHeader in test/unit/proxy/proxy-upstream.test.mjs
// — via Go's own net/http proxy primitives rather than reimplementing them:
//
//   - HTTP_PROXY / HTTPS_PROXY / NO_PROXY from the environment are honoured
//     for every Client (baseTransport), via golang.org/x/net/http/httpproxy
//     — the same well-tested parser net/http.ProxyFromEnvironment uses
//     internally, and already a transitive dependency of this module.
//   - An authenticated, explicitly-configured custom proxy (env vars alone
//     cannot carry credentials) is supported via NewWithUpstreamProxy, which
//     sets http.Transport.Proxy to a fixed http.ProxyURL built from
//     UpstreamProxyConfig.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// baseTransport builds the *http.Transport shared by New and
// NewWithUpstreamProxy: a clone of http.DefaultTransport (so keep-alives,
// dial timeouts, TLS defaults, etc. all match Go's own baseline) with
// ResponseHeaderTimeout set and Proxy explicitly recomputed from the current
// environment.
//
// That recomputation — rather than simply leaving the cloned
// http.DefaultTransport.Proxy field (which is the http.ProxyFromEnvironment
// function) in place — matters for a reason that is easy to miss: net/http
// caches its environment-variable lookup process-wide behind a sync.Once,
// the FIRST time any Transport actually calls it to route a request. Every
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY value read after that first call is
// silently ignored for the rest of the process, no matter how many new
// Clients are constructed later. httpproxy.FromEnvironment() performs the
// identical parsing without that global cache, so each call to baseTransport
// picks up whatever the environment says AT THAT MOMENT — which is both the
// behaviour an operator actually wants (this Client's proxy reflects the
// environment it was constructed in) and the only way this is reliably
// testable at all.
func baseTransport(timeout time.Duration) *http.Transport {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.ResponseHeaderTimeout = timeout
	proxyFunc := httpproxy.FromEnvironment().ProxyFunc()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}
	return transport
}

// ProxyMode selects how NewWithUpstreamProxy routes outbound provider
// requests.
type ProxyMode string

const (
	// ProxyModeNone routes directly, subject only to whatever
	// HTTP_PROXY/HTTPS_PROXY/NO_PROXY says (see baseTransport). This is the
	// zero value, so an unset/omitted mode never accidentally activates a
	// proxy.
	ProxyModeNone ProxyMode = "none"
	// ProxyModeCustom routes through Custom, PROVIDED every one of its
	// Server/Username/Password fields is populated — see upstreamProxyURL.
	ProxyModeCustom ProxyMode = "custom"
)

// CustomProxy is an explicit, authenticated corporate/custom HTTP proxy that
// outbound provider requests should be routed through — the Go analogue of
// upstream Node CCR's config.proxy.upstream.custom.
type CustomProxy struct {
	// Server is the proxy's scheme+host, e.g. "http://proxy.example.com" or
	// "http://proxy.example.com:8888". If it already carries a port, Port
	// below is ignored.
	Server string
	// Port is appended to Server when Server itself specifies none.
	Port int
	// Username and Password authenticate to the proxy itself (HTTP Basic,
	// via the standard "Proxy-Authorization" header) — distinct from, and
	// never confused with, any config.Provider.APIKey, which authenticates
	// to the upstream PROVIDER at the far end of that proxy.
	Username string
	Password string
}

// UpstreamProxyConfig mirrors upstream Node CCR's config.proxy.upstream
// shape: {mode, custom: {server, port, username, password}}.
type UpstreamProxyConfig struct {
	Mode   ProxyMode
	Custom CustomProxy
}

// NewWithUpstreamProxy builds a Client identical to New, except that when
// upstream.Mode is ProxyModeCustom and every one of Custom's
// Server/Username/Password fields is non-empty, outbound requests to
// providers are additionally routed through that authenticated proxy —
// overriding whatever HTTP_PROXY/HTTPS_PROXY would otherwise have applied,
// the same precedence an operator would expect from an explicit config over
// an ambient environment variable.
//
// mode "none", or an incomplete custom config (any of server/username/
// password left empty), silently falls through to New's plain,
// environment-only behaviour — mirroring upstream Node CCR's
// customUpstreamProxyFromConfig, which has the identical fallthrough rather
// than half-applying a broken proxy.
//
// The only error this can return is a malformed Custom.Server URL; that
// error text is built from Custom.Server alone (see upstreamProxyURL) and
// can never contain Custom.Username or Custom.Password.
func NewWithUpstreamProxy(timeout time.Duration, upstream UpstreamProxyConfig) (*Client, error) {
	transport := baseTransport(timeout)
	proxyURL, err := upstreamProxyURL(upstream)
	if err != nil {
		return nil, err
	}
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &Client{HTTP: &http.Client{Transport: transport}}, nil
}

// upstreamProxyURL resolves upstream to the *url.URL http.Transport.Proxy
// should route through, or (nil, nil) when no — or an incomplete — custom
// proxy is configured.
//
// Username/Password are embedded as URL userinfo via url.UserPassword,
// which percent-encodes reserved characters (including a literal "@" or ":"
// in either credential) automatically, so neither can truncate the userinfo
// early or otherwise corrupt the resulting URL.
func upstreamProxyURL(upstream UpstreamProxyConfig) (*url.URL, error) {
	if upstream.Mode != ProxyModeCustom {
		return nil, nil
	}
	c := upstream.Custom
	if c.Server == "" || c.Username == "" || c.Password == "" {
		return nil, nil
	}
	u, err := url.Parse(c.Server)
	if err != nil {
		// err.Error() from url.Parse echoes only its input (c.Server), which
		// carries no credential material — Username/Password are not yet
		// part of u at this point.
		return nil, fmt.Errorf("proxy: parse upstream proxy server %q: %w", c.Server, err)
	}
	if u.Port() == "" && c.Port != 0 {
		u.Host = fmt.Sprintf("%s:%d", u.Hostname(), c.Port)
	}
	u.User = url.UserPassword(c.Username, c.Password)
	return u, nil
}

// upstreamProxyAuthorizationHeader returns the HTTP Basic "Proxy-Authorization"
// header value net/http.Transport sends when routing through a proxy URL
// carrying userinfo — the same construction Transport's internal proxyAuth()
// performs (Basic base64("user:pass") of the RAW, decoded credentials, not
// their percent-encoded URL form). Exposed here purely so that encoding is
// independently unit-testable without a live proxy listener; production
// code never needs to call this; net/http builds and sends the header
// itself once transport.Proxy resolves to a URL with a User set.
func upstreamProxyAuthorizationHeader(u *url.URL) string {
	if u == nil || u.User == nil {
		return ""
	}
	password, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password))
}
