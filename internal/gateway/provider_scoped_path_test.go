package gateway

// ATM-852 (2026-07-23): provider-scoped inbound paths. Claude Code composes
// its request URL as ANTHROPIC_BASE_URL + "/v1/messages"; to let /usage (and
// any client-side accounting) recognize WHICH provider an alias routes to,
// the launcher exports http://127.0.0.1:3456/<provider> — so every routed
// request arrives as /<provider>/v1/messages. The gateway MUST accept and
// strip that leading segment for CONFIGURED providers, keep bare /v1/*
// working unchanged (backward compatibility), and 404 unknown segments
// (a typo must not silently route to the default provider).
//
// RED (pre-fix): /fake/v1/messages returns 404 — these tests FAIL.
// Paired §1.1 mutation: drop the provider-scoped route registration -> the
// scoped tests here FAIL again while the bare-path control stays green.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// scopedUpstream is a minimal OpenAI-shaped upstream good enough for one
// non-streaming round trip.
func scopedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-scoped",
			"choices": [{"index":0,"message":{"role":"assistant","content":"scoped ok"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":2,"completion_tokens":2}
		}`))
	}))
}

func TestProviderScopedMessagesPathRoutes(t *testing.T) {
	upstream := scopedUpstream(t)
	defer upstream.Close()
	s := testServerWithUpstream(t, upstream.URL) // provider name: "fake"

	req := httptest.NewRequest(http.MethodPost, "/fake/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /fake/v1/messages status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProviderScopedUnknownProviderIs404(t *testing.T) {
	upstream := scopedUpstream(t)
	defer upstream.Close()
	s := testServerWithUpstream(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/not-configured/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /not-configured/v1/messages status = %d, want 404 (an unknown segment must not silently route)", rec.Code)
	}
}

func TestBareV1PathStillWorks(t *testing.T) {
	upstream := scopedUpstream(t)
	defer upstream.Close()
	s := testServerWithUpstream(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/messages status = %d, want 200 — the bare path must keep working (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestProviderScopedChatCompletionsPathRoutes(t *testing.T) {
	upstream := scopedUpstream(t)
	defer upstream.Close()
	s := testServerWithUpstream(t, upstream.URL)

	body := []byte(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/fake/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /fake/v1/chat/completions status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}
