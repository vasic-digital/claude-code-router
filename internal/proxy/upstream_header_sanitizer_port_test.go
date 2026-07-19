package proxy

// Ports test/unit/gateway/upstream-header-sanitizer.test.mjs.
//
// Upstream Node CCR builds each outbound provider request by starting from
// a broad, mutable header bag (whatever the internal pipeline accumulated —
// CCR-owned routing/credential/auth-profile headers included) and then runs
// sanitizeUpstreamProviderHeaders as an explicit DENYLIST pass immediately
// before the request leaves the process, stripping every "x-ccr-*"/
// "x-auth-api-key-id"/"x-auth-sub" header while preserving genuine
// provider-facing headers (authorization, arbitrary provider-specific
// x-auth-token, x-client-request-id, ...).
//
// PORTED, not GAP: this repository's proxy.Client.Do achieves the same
// externally-observable guarantee — no internal/CCR-owned header material
// ever reaches an upstream provider — by construction rather than by
// denylist. Do's signature is (ctx, provider, body, stream); it does not
// accept a caller-supplied header map at all, and it builds exactly three
// headers from scratch every call: Authorization (from
// config.Provider.APIKey), Content-Type, and — only when streaming —
// Accept. There is no code path through which any other header (CCR-owned
// or otherwise) could reach the outbound request, so the "sanitizer" here
// is the absence of a forwarding mechanism rather than a filter applied to
// one. The test below proves exactly that allowlisted set leaves the
// process and nothing else does.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func TestDoOnlySendsAllowlistedHeaders(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &config.Provider{Name: "p", APIBaseURL: srv.URL, APIKey: "sk-secret", Models: []string{"m"}}
	c := New(5 * time.Second)

	resp, err := c.Do(context.Background(), p, []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	// net/http and the Go HTTP client itself add a handful of transport
	// headers (Host is not part of r.Header; User-Agent, Accept-Encoding
	// and — for a request built from a []byte via bytes.NewReader, whose
	// length net/http can compute up front — Content-Length are all added
	// automatically) that are not application headers and are not what
	// upstream's sanitizer is guarding against — exclude exactly those
	// before asserting the allowlist.
	transportNoise := map[string]bool{
		"User-Agent":      true,
		"Accept-Encoding": true,
		"Content-Length":  true,
	}
	var got []string
	for k := range gotHeaders {
		if transportNoise[k] {
			continue
		}
		got = append(got, k)
	}
	sort.Strings(got)

	want := []string{"Accept", "Authorization", "Content-Type"}
	if len(got) != len(want) {
		t.Fatalf("outbound headers = %v, want exactly %v (no CCR-internal or arbitrary "+
			"extra header can ever leak upstream, because Do() never forwards caller "+
			"headers — it only ever sets these)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("outbound headers = %v, want %v", got, want)
			break
		}
	}
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want the provider's own bearer token only", auth)
	}
}
