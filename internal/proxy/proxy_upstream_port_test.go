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
// PORTED, not GAP (as of the upstream_proxy.go additions in this file's
// package): baseTransport gives every Client HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY support via golang.org/x/net/http/httpproxy (see that file's doc
// comment for why a fresh-per-Client lookup is used instead of net/http's
// own process-cached http.ProxyFromEnvironment), and NewWithUpstreamProxy +
// upstreamProxyURL + upstreamProxyAuthorizationHeader give this package the
// authenticated-custom-proxy config surface, URL construction, and
// Proxy-Authorization encoding upstream's suite exercises.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestCustomUpstreamProxyURLConstruction is the direct port of upstream's
// "builds the proxy URL with percent-encoded userinfo" case: a literal "@"
// in the username and ":" in the password must not truncate the userinfo or
// otherwise corrupt the URL, and the Basic-Auth header must be built from
// the RAW (decoded) credentials, not their percent-encoded URL form.
func TestCustomUpstreamProxyURLConstruction(t *testing.T) {
	cfg := UpstreamProxyConfig{
		Mode: ProxyModeCustom,
		Custom: CustomProxy{
			Server:   "http://proxy.example.com:8888",
			Username: "alice@example.com",
			Password: "pa:ss",
			Port:     8888,
		},
	}
	const wantURL = "http://alice%40example.com:pa%3Ass@proxy.example.com:8888"

	got, err := upstreamProxyURL(cfg)
	if err != nil {
		t.Fatalf("upstreamProxyURL: %v", err)
	}
	if got == nil {
		t.Fatal("upstreamProxyURL returned nil for a complete custom config")
	}
	if got.String() != wantURL {
		t.Errorf("proxy URL = %q, want %q", got.String(), wantURL)
	}

	const wantAuth = "Basic YWxpY2VAZXhhbXBsZS5jb206cGE6c3M=" // base64("alice@example.com:pa:ss")
	if gotAuth := upstreamProxyAuthorizationHeader(got); gotAuth != wantAuth {
		t.Errorf("Proxy-Authorization = %q, want %q", gotAuth, wantAuth)
	}
}

// TestCustomUpstreamProxyPortAppendedWhenServerHasNone covers the case where
// Server carries no port of its own and Port must be appended to Host.
func TestCustomUpstreamProxyPortAppendedWhenServerHasNone(t *testing.T) {
	cfg := UpstreamProxyConfig{
		Mode: ProxyModeCustom,
		Custom: CustomProxy{
			Server:   "http://proxy.example.com",
			Username: "u",
			Password: "p",
			Port:     3128,
		},
	}
	got, err := upstreamProxyURL(cfg)
	if err != nil {
		t.Fatalf("upstreamProxyURL: %v", err)
	}
	if got == nil || got.Host != "proxy.example.com:3128" {
		t.Fatalf("host = %v, want proxy.example.com:3128", got)
	}
}

// TestCustomUpstreamProxyServerPortWinsOverConfigPort: when Server already
// specifies a port, the separate Port field must not override it.
func TestCustomUpstreamProxyServerPortWinsOverConfigPort(t *testing.T) {
	cfg := UpstreamProxyConfig{
		Mode: ProxyModeCustom,
		Custom: CustomProxy{
			Server:   "http://proxy.example.com:9000",
			Username: "u",
			Password: "p",
			Port:     3128, // must be ignored — Server already has a port
		},
	}
	got, err := upstreamProxyURL(cfg)
	if err != nil {
		t.Fatalf("upstreamProxyURL: %v", err)
	}
	if got == nil || got.Host != "proxy.example.com:9000" {
		t.Fatalf("host = %v, want proxy.example.com:9000 (Server's own port must win)", got)
	}
}

// TestCustomUpstreamProxyNoneOrIncompleteFallsThrough is the direct port of
// upstream's "mode:none, or mode:custom with an incomplete config, yields no
// proxy at all" case, table-driven across every way the config can be
// incomplete.
func TestCustomUpstreamProxyNoneOrIncompleteFallsThrough(t *testing.T) {
	cases := []struct {
		name string
		cfg  UpstreamProxyConfig
	}{
		{"zero value", UpstreamProxyConfig{}},
		{"mode none with a fully populated custom config", UpstreamProxyConfig{
			Mode:   ProxyModeNone,
			Custom: CustomProxy{Server: "http://proxy.example.com:8888", Username: "u", Password: "p"},
		}},
		{"mode custom, empty server", UpstreamProxyConfig{
			Mode:   ProxyModeCustom,
			Custom: CustomProxy{Server: "", Username: "u", Password: "p"},
		}},
		{"mode custom, empty username", UpstreamProxyConfig{
			Mode:   ProxyModeCustom,
			Custom: CustomProxy{Server: "http://proxy.example.com:8888", Username: "", Password: "p"},
		}},
		{"mode custom, empty password", UpstreamProxyConfig{
			Mode:   ProxyModeCustom,
			Custom: CustomProxy{Server: "http://proxy.example.com:8888", Username: "u", Password: ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := upstreamProxyURL(tc.cfg)
			if err != nil {
				t.Fatalf("upstreamProxyURL: %v", err)
			}
			if got != nil {
				t.Fatalf("proxy URL = %v, want nil (no proxy) for an incomplete/none config", got)
			}
		})
	}
}

// TestNewWithUpstreamProxyRejectsMalformedServer: a malformed Server URL
// must surface as an error from NewWithUpstreamProxy, not a panic or a
// silently-broken Client.
func TestNewWithUpstreamProxyRejectsMalformedServer(t *testing.T) {
	_, err := NewWithUpstreamProxy(time.Second, UpstreamProxyConfig{
		Mode:   ProxyModeCustom,
		Custom: CustomProxy{Server: "http://%zz", Username: "u", Password: "p"},
	})
	if err == nil {
		t.Fatal("expected an error for a malformed proxy server URL")
	}
}

// TestNewWithUpstreamProxyActuallyRoutesThroughTheConfiguredProxy is a full
// functional proof, not just URL construction: a Do() call must actually
// travel through the configured proxy — carrying the expected
// Proxy-Authorization header — rather than merely computing the right URL
// and never using it.
//
// This works without the target host existing at all: for a plain-http
// absolute-form request, net/http's Transport opens its TCP connection to
// the PROXY's address and sends the full request-URI on the wire, so
// "reaching the proxy" and "reaching the (fake) target" are the same
// observable event here.
func TestNewWithUpstreamProxyActuallyRoutesThroughTheConfiguredProxy(t *testing.T) {
	var gotRequestURI, gotProxyAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse test proxy URL: %v", err)
	}

	c, err := NewWithUpstreamProxy(5*time.Second, UpstreamProxyConfig{
		Mode: ProxyModeCustom,
		Custom: CustomProxy{
			Server:   proxyURL.Scheme + "://" + proxyURL.Hostname(),
			Port:     mustAtoi(t, proxyURL.Port()),
			Username: "alice",
			Password: "s3cret",
		},
	})
	if err != nil {
		t.Fatalf("NewWithUpstreamProxy: %v", err)
	}

	p := testProvider("http://fake-upstream.internal.example/v1/chat/completions", "k")
	resp, err := c.Do(context.Background(), p, []byte(`{}`), false)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the proxy", resp.StatusCode)
	}
	if gotRequestURI != "http://fake-upstream.internal.example/v1/chat/completions" {
		t.Errorf("proxy saw RequestURI %q, want the full absolute target URL (proof the request "+
			"actually travelled through the proxy rather than being sent directly)", gotRequestURI)
	}
	const wantAuth = "Basic YWxpY2U6czNjcmV0" // base64("alice:s3cret")
	if gotProxyAuth != wantAuth {
		t.Errorf("Proxy-Authorization = %q, want %q", gotProxyAuth, wantAuth)
	}
}

// TestCustomUpstreamProxyCredentialsNeverAppearInErrors mirrors
// TestDoErrorNeverContainsAPIKey (proxy_test.go): whatever goes wrong
// reaching the configured proxy, neither its username nor its password may
// ever appear in the returned error text.
func TestCustomUpstreamProxyCredentialsNeverAppearInErrors(t *testing.T) {
	const proxyUser = "corp-proxy-user"
	const proxyPass = "sk-proxy-super-secret-do-not-leak-0987654321"

	cases := []struct {
		name   string
		server string
	}{
		{"connection refused (unreachable proxy port)", "http://127.0.0.1:1"},
		{"unresolvable proxy host", "http://this-proxy-should-not-exist.invalid.example"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewWithUpstreamProxy(2*time.Second, UpstreamProxyConfig{
				Mode: ProxyModeCustom,
				Custom: CustomProxy{
					Server:   tc.server,
					Username: proxyUser,
					Password: proxyPass,
				},
			})
			if err != nil {
				t.Fatalf("NewWithUpstreamProxy: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			p := testProvider("http://fake-upstream.internal.example/v1/chat/completions", "provider-key")
			resp, err := c.Do(ctx, p, []byte(`{}`), false)
			if err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatal("expected an error for an unreachable proxy")
			}
			if strings.Contains(err.Error(), proxyPass) {
				t.Fatalf("error text leaked the proxy password: %v", err)
			}
			if strings.Contains(err.Error(), proxyUser) {
				t.Fatalf("error text leaked the proxy username: %v", err)
			}
		})
	}
}

// TestEnvironmentHTTPProxyIsHonoured proves baseTransport's env-proxy wiring
// (see upstream_proxy.go) is not just constructed but actually taken: a
// plain New(...) client, with HTTP_PROXY pointed at a local proxy stand-in,
// must route its request through that proxy without any explicit
// per-Client proxy config at all.
func TestEnvironmentHTTPProxyIsHonoured(t *testing.T) {
	var hit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")

	c := New(5 * time.Second)
	p := testProvider("http://fake-upstream.internal.example/v1/chat/completions", "k")
	resp, err := c.Do(context.Background(), p, []byte(`{}`), false)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if !hit {
		t.Fatal("HTTP_PROXY was set but the request never reached the proxy — env proxy not honoured")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the proxy", resp.StatusCode)
	}
}

// TestEnvironmentNoProxyBypassesConfiguredProxy proves NO_PROXY is
// respected: HTTP_PROXY points at a guaranteed-broken address, but NO_PROXY
// exempts the real target's host, so the request must reach the real target
// directly rather than failing through the broken proxy.
func TestEnvironmentNoProxyBypassesConfiguredProxy(t *testing.T) {
	var proxyHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		w.WriteHeader(http.StatusTeapot) // distinct from the real target's response
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"direct":true}`))
	}))
	defer target.Close()
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", targetURL.Hostname())

	c := New(5 * time.Second)
	p := testProvider(target.URL+"/v1/chat/completions", "k")
	resp, err := c.Do(context.Background(), p, []byte(`{}`), false)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if proxyHit {
		t.Fatal("request went through the (NO_PROXY-exempted) proxy instead of connecting directly")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 from the direct target", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "direct") {
		t.Errorf("body = %q, want the direct target's response", body)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("mustAtoi: %q is not numeric", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
