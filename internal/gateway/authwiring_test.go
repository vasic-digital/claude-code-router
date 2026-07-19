package gateway

// Tests that RequireAPIKey (auth.go) is actually MOUNTED on the route table
// (gateway.go's routes()), not merely written and unit-tested in isolation
// (see auth_test.go, which exercises the middleware directly against a
// throwaway gin engine and never touches Server/New at all). These tests
// drive the real *Server built by New, through Options.APIKeys, to prove:
//
//   - POST /v1/messages is gated when APIKeys is non-empty.
//   - GET /health and /ready are NEVER gated, regardless of APIKeys — a
//     supervisor must always be able to probe liveness/readiness.
//   - An EMPTY (zero-value) APIKeys list leaves /v1/messages open too, for
//     backwards compatibility with the toolkit that drives this gateway
//     today and sends no client key at all.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// authWiringTestServer builds a real Server (via New) with a working
// upstream, so a request that clears auth can be observed reaching the
// handler and getting a normal 200 — not just "not a 401".
func authWiringTestServer(t *testing.T, apiKeys []string) (*Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
			"id": "chatcmpl-auth-ok",
			"choices": [{"index":0,"message":{"role":"assistant","content":"authorized"},"finish_reason":"stop"}],
			"usage": {"prompt_tokens":1,"completion_tokens":1}
		}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Providers: []config.Provider{{
			Name: "fake", APIBaseURL: upstream.URL, APIKey: "sk-test", Models: []string{"fake-model"},
		}},
		Router: config.Route{Default: "fake,fake-model"},
	}
	return New(cfg, Options{APIKeys: apiKeys}), upstream
}

// --- auth enforced on /v1/messages ---

func TestAuthEnforcedOnMessagesEndpoint(t *testing.T) {
	s, _ := authWiringTestServer(t, []string{"the-real-key"})

	// No key presented at all -> 401, and the handler must never have run
	// (proven by the body being the fixed 401 envelope, not anything the
	// upstream/handler would have produced).
	noKeyReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	noKeyRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(noKeyRec, noKeyReq)
	if noKeyRec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401, body=%s", noKeyRec.Code, noKeyRec.Body.String())
	}

	// Wrong key -> also 401.
	wrongKeyReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	wrongKeyReq.Header.Set("x-api-key", "not-the-real-key")
	wrongKeyRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(wrongKeyRec, wrongKeyReq)
	if wrongKeyRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401, body=%s", wrongKeyRec.Code, wrongKeyRec.Body.String())
	}

	// Correct key via x-api-key -> reaches the real handler and completes
	// the round trip.
	viaXAPIKey := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	viaXAPIKey.Header.Set("x-api-key", "the-real-key")
	xRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(xRec, viaXAPIKey)
	if xRec.Code != http.StatusOK {
		t.Fatalf("correct x-api-key: status = %d, want 200, body=%s", xRec.Code, xRec.Body.String())
	}

	// Correct key via Authorization: Bearer -> also reaches the handler.
	viaBearer := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
	viaBearer.Header.Set("Authorization", "Bearer the-real-key")
	bRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(bRec, viaBearer)
	if bRec.Code != http.StatusOK {
		t.Fatalf("correct bearer key: status = %d, want 200, body=%s", bRec.Code, bRec.Body.String())
	}
}

// --- /health and /ready still open, regardless of APIKeys ---

func TestHealthAndReadyStayUnauthenticatedWithAPIKeysConfigured(t *testing.T) {
	s, _ := authWiringTestServer(t, []string{"the-real-key"})

	healthRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(healthRec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if healthRec.Code != http.StatusOK {
		t.Fatalf("/health with APIKeys configured and no key presented: status = %d, want 200 (must stay unauthenticated so a supervisor can always probe it)", healthRec.Code)
	}

	readyRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(readyRec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if readyRec.Code != http.StatusOK {
		t.Fatalf("/ready with APIKeys configured and no key presented: status = %d, want 200 (must stay unauthenticated so a supervisor can always probe it)", readyRec.Code)
	}
}

// --- empty key list disables auth (backwards compatibility) ---

func TestEmptyAPIKeysLeavesMessagesEndpointOpen(t *testing.T) {
	// nil (the zero value New() sees when a caller never sets Options.APIKeys
	// at all) must behave identically to an explicit empty slice — both mean
	// "no keys configured".
	for name, keys := range map[string][]string{"nil": nil, "empty slice": {}} {
		t.Run(name, func(t *testing.T) {
			s, _ := authWiringTestServer(t, keys)

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(anthropicReqBody(false)))
			// Deliberately no Authorization/x-api-key header at all — this is
			// exactly how the toolkit that drives this gateway today calls
			// it, and it must keep working unchanged.
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — an empty/unset APIKeys list must leave /v1/messages unauthenticated, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
