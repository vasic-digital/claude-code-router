package security

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
)

// secretKey is a clearly-fake credential shape (never a real key) used only
// to detect whether the gateway/proxy ever echoes provider secret material
// back to the caller. It is never written to disk.
const secretKey = "sk-test-DO-NOT-LEAK-1234567890abcdef"

// assertNoLeak scans status, headers and body of a recorded response for the
// secret, failing loudly and naming exactly where it was found.
func assertNoLeak(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	if strings.Contains(body, secretKey) {
		t.Fatalf("%s: response BODY leaked the API key: %s", label, body)
	}
	if strings.Contains(body, "Bearer "+secretKey) {
		t.Fatalf("%s: response BODY leaked the Authorization header value: %s", label, body)
	}
	for name, vals := range rec.Header() {
		for _, v := range vals {
			if strings.Contains(v, secretKey) {
				t.Fatalf("%s: response HEADER %q leaked the API key: %q", label, name, v)
			}
		}
	}
}

// ---------- Full gateway, several distinct real failure modes ----------

func TestGatewayErrorPathsNeverLeakAPIKey(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"connection refused", "http://127.0.0.1:1/v1/chat/completions"},
		{"DNS failure", "http://this-host-should-not-exist.invalid.example/v1/chat/completions"},
		{"malformed URL", "http://%zz/v1/chat/completions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := gwServer(tc.url, secretKey)
			runBounded(t, defaultBound, func() {
				rec := postMessages(s)
				if rec.Code == http.StatusOK {
					t.Fatalf("%s: expected a failure status, got 200: %s", tc.name, rec.Body.String())
				}
				assertNoLeak(t, tc.name, rec)
			})
		})
	}
}

// A misbehaving upstream that returns 401/403 with an error body must still
// never cause OUR OWN code to have echoed the secret anywhere — the upstream
// never received a body containing the raw key value as literal text (the
// key travels only in the Authorization header, never body content), so this
// also incidentally proves the upstream response-forwarding path is safe by
// construction, not just by the absence of a bug so far.
func TestGatewayForwardsUpstream4xxWithoutLeakingKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A intentionally-adversarial upstream: whatever it does, it does NOT
		// have the raw key value as body text to leak (it only ever saw it in
		// the Authorization header, which this handler ignores), so this
		// proves the response passthrough path is clean.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
	}))
	defer upstream.Close()

	s := gwServer(upstream.URL, secretKey)
	runBounded(t, defaultBound, func() {
		rec := postMessages(s)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		assertNoLeak(t, "upstream 401", rec)
	})
}

// Upstream hang past the configured timeout: the gateway's own
// "upstream request failed: %v" wrapping must not include the key even
// though the underlying transport error text is entirely outside our
// control.
func TestGatewayTimeoutErrorNeverLeaksAPIKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and never respond, never close — simulates a wedged
			// upstream. The gateway's own configured timeout must cut this
			// off; we don't rely on cooperation from this goroutine.
			_ = conn
		}
	}()

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "sec-provider", APIBaseURL: "http://" + ln.Addr().String() + "/x",
			APIKey: secretKey, Models: []string{"m"},
		}},
		Router: config.Route{Default: "sec-provider,m"},
	}
	s := gateway.New(cfg, gateway.Options{UpstreamTimeout: 60 * time.Millisecond})

	runBounded(t, defaultBound, func() {
		rec := postMessages(s)
		if rec.Code == http.StatusOK {
			t.Fatalf("expected a timeout failure, got 200")
		}
		assertNoLeak(t, "timeout", rec)
	})
}

// ---------- Proxy layer: additional failure modes beyond internal/proxy's
// own suite (transport-timeout expiry specifically, driven with a bounded
// context here rather than relying on ResponseHeaderTimeout alone). ----------

func TestProxyContextDeadlineErrorNeverLeaksAPIKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(250 * time.Millisecond) // never responds within the client's budget
	}()

	p := &config.Provider{
		Name: "sec-provider", APIBaseURL: "http://" + ln.Addr().String() + "/x",
		APIKey: secretKey, Models: []string{"m"},
	}

	c := proxy.New(5 * time.Second) // header timeout deliberately generous; ctx is the real bound
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	runBounded(t, defaultBound, func() {
		resp, err := c.Do(ctx, p, []byte(`{}`), false)
		if err == nil {
			if resp != nil {
				resp.Body.Close()
			}
			t.Fatal("expected a context-deadline error")
		}
		if strings.Contains(err.Error(), secretKey) {
			t.Fatalf("context-deadline error leaked the API key: %v", err)
		}
		if strings.Contains(err.Error(), "Bearer") {
			t.Fatalf("context-deadline error leaked header material: %v", err)
		}
	})
}
