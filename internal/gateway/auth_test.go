package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newAuthTestEngine builds a minimal gin engine with RequireAPIKey(keys) as
// its only middleware, in front of a trivial 200 handler — isolated from
// Server/New entirely, so these tests exercise exactly the middleware's own
// contract and nothing gateway.go does around it.
func newAuthTestEngine(keys []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.Use(RequireAPIKey(keys))
	eng.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return eng
}

func doAuthRequest(eng *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)
	return rec
}

// assertAnthropicUnauthorized checks both the status code and the exact
// Anthropic error envelope shape: {"type":"error","error":{"type":
// "authentication_error","message":...}}.
func assertAnthropicUnauthorized(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 body is not valid JSON: %s", rec.Body.String())
	}
	if body.Type != "error" {
		t.Errorf("top-level type = %q, want %q", body.Type, "error")
	}
	if body.Error.Type != "authentication_error" {
		t.Errorf("error.type = %q, want %q", body.Error.Type, "authentication_error")
	}
	if body.Error.Message == "" {
		t.Error("error.message is empty")
	}
}

// TestRequireAPIKeyTableDriven covers the full accept/reject matrix in one
// place: an empty key list disables auth entirely (backwards compatibility),
// a valid key via either supported header passes, and every rejection shape
// (wrong key, missing header, empty/whitespace-only Bearer value) 401s —
// with whitespace trimming applying to a genuinely valid key on both
// header forms.
func TestRequireAPIKeyTableDriven(t *testing.T) {
	cases := []struct {
		name       string
		keys       []string
		headers    map[string]string
		wantStatus int
	}{
		{"no keys configured, no headers -> auth disabled, passes", nil, nil, http.StatusOK},
		{"no keys configured, garbage header -> still passes (disabled)", nil,
			map[string]string{"Authorization": "Bearer garbage"}, http.StatusOK},
		{"valid bearer", []string{"k1"}, map[string]string{"Authorization": "Bearer k1"}, http.StatusOK},
		{"valid x-api-key", []string{"k1"}, map[string]string{"x-api-key": "k1"}, http.StatusOK},
		{"bearer value with surrounding whitespace trimmed", []string{"k1"},
			map[string]string{"Authorization": " Bearer k1 "}, http.StatusOK},
		{"x-api-key value with surrounding whitespace trimmed", []string{"k1"},
			map[string]string{"x-api-key": " k1 "}, http.StatusOK},
		{"one of several accepted keys (first)", []string{"k1", "k2"},
			map[string]string{"x-api-key": "k1"}, http.StatusOK},
		{"one of several accepted keys (second)", []string{"k1", "k2"},
			map[string]string{"x-api-key": "k2"}, http.StatusOK},
		{"wrong key -> 401", []string{"k1"}, map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"wrong x-api-key -> 401", []string{"k1"}, map[string]string{"x-api-key": "nope"}, http.StatusUnauthorized},
		{"missing header entirely -> 401", []string{"k1"}, nil, http.StatusUnauthorized},
		{"empty bearer value (no token at all) -> 401", []string{"k1"},
			map[string]string{"Authorization": "Bearer"}, http.StatusUnauthorized},
		{"whitespace-only bearer value -> 401", []string{"k1"},
			map[string]string{"Authorization": "Bearer   "}, http.StatusUnauthorized},
		{"malformed Authorization (not Bearer-shaped), no x-api-key -> 401", []string{"k1"},
			map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, http.StatusUnauthorized},
		{"empty x-api-key value -> 401", []string{"k1"}, map[string]string{"x-api-key": ""}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newAuthTestEngine(tc.keys)
			rec := doAuthRequest(eng, tc.headers)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusUnauthorized {
				assertAnthropicUnauthorized(t, rec)
			}
		})
	}
}

// TestRequireAPIKeyRejectsBareKeyInAuthorizationWithoutBearerScheme makes
// sure a caller cannot smuggle a bare key through Authorization without the
// "Bearer " scheme and have it silently accepted.
func TestRequireAPIKeyRejectsBareKeyInAuthorizationWithoutBearerScheme(t *testing.T) {
	eng := newAuthTestEngine([]string{"k1"})
	rec := doAuthRequest(eng, map[string]string{"Authorization": "k1"})
	assertAnthropicUnauthorized(t, rec)
}

// TestRequireAPIKeyPresentedKeyNeverLeaksInResponse is the load-bearing
// safety property: whatever a client presents — right or wrong — it must
// never come back in the 401 body or in any response header. A right key
// obviously must not leak either, but that case degenerates to "the
// response is a normal 200 with no key anywhere in it", so only the
// rejection paths are worth asserting here.
func TestRequireAPIKeyPresentedKeyNeverLeaksInResponse(t *testing.T) {
	const secretGuess = "sk-attacker-supplied-guess-must-never-echo-back-0102030405"
	eng := newAuthTestEngine([]string{"the-real-key"})

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"wrong bearer key", map[string]string{"Authorization": "Bearer " + secretGuess}},
		{"wrong x-api-key", map[string]string{"x-api-key": secretGuess}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAuthRequest(eng, tc.headers)
			assertAnthropicUnauthorized(t, rec)

			if strings.Contains(rec.Body.String(), secretGuess) {
				t.Errorf("response body leaked the presented key: %s", rec.Body.String())
			}
			for name, values := range rec.Header() {
				for _, v := range values {
					if strings.Contains(v, secretGuess) {
						t.Errorf("response header %s leaked the presented key: %q", name, v)
					}
				}
			}
		})
	}
}

// TestRequireAPIKeyDoesNotLeakAcceptedKeysEither: the configured/accepted
// keys themselves must not appear in a rejection response either — a caller
// who probes with garbage must not be able to fish the real key out of an
// error message.
func TestRequireAPIKeyDoesNotLeakAcceptedKeysEither(t *testing.T) {
	const realKey = "the-actual-configured-secret-key-9988776655"
	eng := newAuthTestEngine([]string{realKey})

	rec := doAuthRequest(eng, map[string]string{"Authorization": "Bearer totally-wrong"})
	assertAnthropicUnauthorized(t, rec)

	if strings.Contains(rec.Body.String(), realKey) {
		t.Errorf("response body leaked the configured key: %s", rec.Body.String())
	}
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, realKey) {
				t.Errorf("response header %s leaked the configured key: %q", name, v)
			}
		}
	}
}

// TestRequireAPIKeyMutatingCallerSliceAfterConstructionHasNoEffect proves the
// defensive copy in RequireAPIKey: mutating the slice the caller passed in,
// after the handler was built, must not change what the handler accepts.
func TestRequireAPIKeyMutatingCallerSliceAfterConstructionHasNoEffect(t *testing.T) {
	keys := []string{"k1"}
	eng := newAuthTestEngine(keys)
	keys[0] = "k1-mutated"

	// The original key must still work (the handler kept its own copy)...
	rec := doAuthRequest(eng, map[string]string{"x-api-key": "k1"})
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — mutating the caller's slice must not affect the handler's accepted keys", rec.Code)
	}
	// ...and the mutated value must NOT work, proving it truly is a copy.
	rec2 := doAuthRequest(eng, map[string]string{"x-api-key": "k1-mutated"})
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a key that was never actually configured", rec2.Code)
	}
}
