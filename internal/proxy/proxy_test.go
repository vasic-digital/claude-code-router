package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

func testProvider(baseURL, apiKey string) *config.Provider {
	return &config.Provider{
		Name:       "test-provider",
		APIBaseURL: baseURL,
		APIKey:     apiKey,
		Models:     []string{"m1"},
	}
}

// The URL, method, headers and body actually presented to the upstream must
// match what Do was asked to send — table-driven across the streaming and
// non-streaming cases, since that is the one header that differs between
// them.
func TestDoSendsCorrectRequest(t *testing.T) {
	cases := []struct {
		name       string
		stream     bool
		wantAccept string
	}{
		{name: "non-streaming", stream: false, wantAccept: ""},
		{name: "streaming", stream: true, wantAccept: "text/event-stream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotAuth, gotContentType, gotAccept, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotContentType = r.Header.Get("Content-Type")
				gotAccept = r.Header.Get("Accept")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			// The path is part of APIBaseURL and must be hit VERBATIM — the
			// client must never append anything to it.
			base := srv.URL + "/v1/chat/completions"
			p := testProvider(base, "sk-secret-key")

			c := New(5 * time.Second)
			resp, err := c.Do(context.Background(), p, []byte(`{"model":"m1"}`), tc.stream)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", gotMethod)
			}
			if gotPath != "/v1/chat/completions" {
				t.Errorf("path = %q, want /v1/chat/completions (verbatim, no appended suffix)", gotPath)
			}
			if gotAuth != "Bearer sk-secret-key" {
				t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-secret-key")
			}
			if gotContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotContentType)
			}
			if gotAccept != tc.wantAccept {
				t.Errorf("Accept = %q, want %q", gotAccept, tc.wantAccept)
			}
			if gotBody != `{"model":"m1"}` {
				t.Errorf("body = %q", gotBody)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// A non-2xx upstream response must be handed back as-is — status code and
// body intact — rather than converted into a Go error, so the gateway can
// relay a faithful error to Claude Code.
func TestDoSurfacesNon2xxWithStatusAndBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "400 bad request", status: http.StatusBadRequest, body: `{"error":"bad model"}`},
		{name: "401 unauthorized", status: http.StatusUnauthorized, body: `{"error":"invalid api key"}`},
		{name: "429 rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`},
		{name: "500 upstream error", status: http.StatusInternalServerError, body: `{"error":"boom"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(5 * time.Second)
			resp, err := c.Do(context.Background(), testProvider(srv.URL, "k"), []byte(`{}`), false)
			if err != nil {
				t.Fatalf("Do returned an error for a non-2xx status, want the response surfaced instead: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(b) != tc.body {
				t.Errorf("body = %q, want %q", string(b), tc.body)
			}
		})
	}
}

// Cancelling the caller's context must abort the in-flight request rather
// than waiting it out, and the error must be attributable to cancellation.
func TestDoHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test releases or times out
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	c := New(30 * time.Second) // long enough that only cancellation ends this
	start := time.Now()
	_, err := c.Do(ctx, testProvider(srv.URL, "k"), []byte(`{}`), false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should unwrap to context.Canceled, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Do took %v to return after cancellation, want it to abort promptly", elapsed)
	}
}

// The single most important safety property of this package: whatever goes
// wrong in Do, the API key must never appear in the returned error text.
// This is exercised against several distinct failure modes so no one error
// path can regress silently.
func TestDoErrorNeverContainsAPIKey(t *testing.T) {
	const secretKey = "sk-super-secret-do-not-leak-1234567890"

	cases := []struct {
		name string
		p    *config.Provider
	}{
		{
			name: "connection refused (unreachable port)",
			p:    testProvider("http://127.0.0.1:1", secretKey),
		},
		{
			name: "malformed URL",
			p:    testProvider("http://%zz", secretKey),
		},
		{
			name: "unresolvable host",
			p:    testProvider("http://this-host-should-not-exist.invalid.example", secretKey),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			c := New(2 * time.Second)
			resp, err := c.Do(ctx, tc.p, []byte(`{}`), false)
			if err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatal("expected an error for this failure mode")
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Fatalf("error text leaked the API key: %v", err)
			}
			if strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("error text leaked the Authorization scheme, suggesting header material leaked: %v", err)
			}
		})
	}
}

// New's response-header timeout must bound only the wait for headers, not
// the total time spent reading a streaming body — otherwise every SSE
// session would be truncated mid-stream once it outlives the configured
// duration, which defeats the point of streaming.
func TestStreamingBodyIsNotCutShortByResponseTimeout(t *testing.T) {
	const chunkDelay = 60 * time.Millisecond
	const numChunks = 5 // total streaming time > the client's configured timeout

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush() // headers go out immediately
		for i := 0; i < numChunks; i++ {
			time.Sleep(chunkDelay)
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	// Configured timeout is shorter than the total streaming duration
	// (numChunks * chunkDelay), but headers arrive well within it.
	c := New(30 * time.Millisecond)
	resp, err := c.Do(context.Background(), testProvider(srv.URL, "k"), []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	got := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: chunk-") {
			got++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream was cut short: %v (got %d/%d chunks)", err, got, numChunks)
	}
	if got != numChunks {
		t.Errorf("received %d chunks, want %d — stream was truncated", got, numChunks)
	}
}
