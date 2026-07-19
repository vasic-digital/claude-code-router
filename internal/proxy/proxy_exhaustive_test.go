package proxy

// Exhaustive CONFIRMATION/VALIDATION/VERIFICATION coverage for the upstream
// client, focused on the security-critical guarantees: the api_base_url is
// posted VERBATIM (path AND query preserved, nothing appended), the api_key
// never reaches an error string on any failure path, and the response-HEADER
// timeout fires on a stalled upstream while NOT cutting a slow body short.
// Plus the edge cases the task calls out: empty api_key, and non-http /
// malformed bases handled cleanly (no panic, no leak).
//
// This file does NOT restate proxy_test.go / proxy_upstream_port_test.go /
// upstream_header_sanitizer_port_test.go. In particular the existing suite
// already proves: a single correct request (one path), non-2xx surfaced,
// context cancellation, key-absent-in-error for three transport failure modes,
// a slow streaming body NOT truncated by the header timeout, the allowlisted
// header set, and the full custom-proxy / env-proxy / NO_PROXY matrix. The
// additions here are the header-timeout-FIRES direction, verbatim URL with
// query/root/trailing-slash, empty-key behaviour, non-http/malformed bases,
// and concurrent reuse of one Client.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDoUsesAPIBaseURLVerbatim confirms the "no path re-append" guarantee
// across shapes the single existing case does not cover: a root path, a
// multi-segment versioned path, a URL carrying a query string (which must be
// preserved intact — an azure-style ?api-version=... would break if dropped),
// and a trailing slash. The client must hit exactly what config.Provider gave
// it, byte-for-byte, and never synthesize a "/chat/completions" suffix.
func TestDoUsesAPIBaseURLVerbatim(t *testing.T) {
	cases := []struct {
		name    string
		suffix  string
		wantURI string
	}{
		{"root path", "/", "/"},
		{"multi-segment versioned path", "/v4/openai/deployments/x/chat/completions", "/v4/openai/deployments/x/chat/completions"},
		{"query string preserved", "/v1/chat/completions?api-version=2024-02-01&foo=bar", "/v1/chat/completions?api-version=2024-02-01&foo=bar"},
		{"trailing slash preserved", "/v1/chat/completions/", "/v1/chat/completions/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotURI string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotURI = r.URL.RequestURI() // path + (encoded) query, exactly as received
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := New(5 * time.Second)
			resp, err := c.Do(context.Background(), testProvider(srv.URL+tc.suffix, "sk-secret"), []byte(`{}`), false)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()

			if gotURI != tc.wantURI {
				t.Errorf("upstream saw RequestURI %q, want %q (api_base_url must be used verbatim, nothing appended, query preserved)", gotURI, tc.wantURI)
			}
		})
	}
}

// TestResponseHeaderTimeoutFiresWhenHeadersStall is the complement to the
// existing "slow BODY is not cut short" test: when the upstream never even
// sends response headers, the ResponseHeaderTimeout MUST fire and Do must
// return an error promptly (well before the upstream would eventually respond)
// — and that error must not leak the api_key. Together the two tests pin the
// exact intended boundary of New's timeout: it bounds the header wait only.
func TestResponseHeaderTimeoutFiresWhenHeadersStall(t *testing.T) {
	const secretKey = "sk-header-timeout-secret-do-not-leak-123456"

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never send headers until the test is tearing down
		w.WriteHeader(http.StatusOK)
	}))
	// Release the handler only after the assertions, so it does not send
	// headers within the timeout window and skew the test.
	defer srv.Close()
	defer close(block)

	c := New(100 * time.Millisecond)
	start := time.Now()
	resp, err := c.Do(context.Background(), testProvider(srv.URL, secretKey), []byte(`{}`), false)
	elapsed := time.Since(start)

	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected an error when the upstream never sends response headers")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Do took %v to fail; the response-header timeout should have fired near 100ms", elapsed)
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Fatalf("header-timeout error leaked the api_key: %v", err)
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("header-timeout error leaked Authorization header material: %v", err)
	}
}

// TestDoEmptyAPIKeyBehaviour documents the ACTUAL behaviour when a provider is
// configured with an empty api_key. NOTE: Do sets the Authorization header
// unconditionally ("Bearer " + APIKey), so with an empty key the upstream
// still receives an Authorization header whose value is the bare scheme
// "Bearer" with no token (the trailing space is trimmed on receipt). There is
// no secret to leak in this case; this test locks in that a missing key
// yields a tokenless header rather than, say, a panic or a "Bearer <garbage>".
func TestDoEmptyAPIKeyBehaviour(t *testing.T) {
	var present bool
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	resp, err := c.Do(context.Background(), testProvider(srv.URL, ""), []byte(`{}`), false)
	if err != nil {
		t.Fatalf("Do with empty api_key errored: %v", err)
	}
	resp.Body.Close()

	if !present {
		t.Log("Authorization header omitted entirely for empty api_key")
		return
	}
	if tok := strings.TrimSpace(gotAuth); tok != "Bearer" {
		t.Errorf("Authorization = %q, want a tokenless %q for an empty api_key (no credential material)", gotAuth, "Bearer")
	}
}

// TestDoNonHTTPOrMalformedBaseHandledCleanly: a base URL that is not a usable
// http(s) endpoint must produce a normal error — never a panic — and that
// error must never contain the api_key. (config validation rejects such bases
// up front, but Do itself must still fail safe if handed one directly.)
func TestDoNonHTTPOrMalformedBaseHandledCleanly(t *testing.T) {
	const secretKey = "sk-nonhttp-secret-do-not-leak-0987654321"

	cases := []struct {
		name string
		base string
	}{
		{"unsupported scheme", "ftp://example.invalid/x"},
		{"no scheme or host", "not-a-valid-url"},
		{"empty base", ""},
		{"scheme only, no host", "http://"},
		{"control chars in url", "http://exa\x7fmple.invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			c := New(2 * time.Second)
			resp, err := c.Do(ctx, testProvider(tc.base, secretKey), []byte(`{}`), false)
			if err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatalf("expected an error for a non-http/malformed base %q", tc.base)
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Fatalf("error for base %q leaked the api_key: %v", tc.base, err)
			}
			if strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("error for base %q leaked Authorization header material: %v", tc.base, err)
			}
		})
	}
}

// TestConcurrentDoReusesOneClientSafely: the doc comment on Client promises a
// single http.Client is meant to be shared across every call for the life of
// the process. Prove that concurrent Do calls on one Client are race-free (run
// with -race) and all succeed — a shared-transport bug would surface here.
func TestConcurrentDoReusesOneClientSafely(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(5 * time.Second)
	p := testProvider(srv.URL+"/v1/chat/completions", "sk-shared-secret")

	const goroutines = 40
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Do(context.Background(), p, []byte(`{"model":"m1"}`), false)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Do failed: %v", err)
	}
}
