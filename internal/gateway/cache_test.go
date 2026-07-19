package gateway

// End-to-end proof that the response cache is wired into handleMessages: an
// identical repeat of a cacheable non-streaming request is served locally
// WITHOUT a second upstream call, while non-cacheable requests (temperature>0,
// streaming) always reach the upstream, and a nil Cache leaves behaviour
// exactly as it was.
//
// The recordingUpstream / countingUpstream fakes and anthropicReqBody live in
// the sibling *_test.go files (anthropic_passthrough_test.go, retry_test.go).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/cache"
	"github.com/vasic-digital/claude-code-router/internal/config"
)

// openAICompletionBody is a canned, cacheable (no tool_calls, no error)
// OpenAI chat-completion response. Each call returns a FRESH reader so the
// same upstream fake can answer more than one request.
func openAICompletionResponse() *http.Response {
	const body = `{
		"id": "chatcmpl-cache",
		"choices": [{"index":0,"message":{"role":"assistant","content":"cached hello"},"finish_reason":"stop"}],
		"usage": {"prompt_tokens":5,"completion_tokens":2}
	}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// anthropicReqBodyTemp is anthropicReqBody with an explicit temperature, so the
// request-side gate (cache.Cacheable) refuses to cache it.
func anthropicReqBodyTemp(temperature float64) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":       "claude-3-5-sonnet",
		"max_tokens":  100,
		"temperature": temperature,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	return b
}

func cacheTestServer(t *testing.T, up Upstream, c cache.Cache) *Server {
	t.Helper()
	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = up
	s.Cache = c
	return s
}

func doMessages(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// MISS then HIT: the second identical request must be served from cache with
// NO second upstream call, and produce the same client response.
func TestCacheMissThenHitSkipsUpstream(t *testing.T) {
	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		return openAICompletionResponse(), nil
	}}
	mem := cache.NewMemoryLRU(cache.MemoryOptions{MaxEntries: 16})
	s := cacheTestServer(t, up, mem)

	rec1 := doMessages(t, s, anthropicReqBody(false))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, body=%s", rec1.Code, rec1.Body.String())
	}
	if got := atomic.LoadInt32(&up.calls); got != 1 {
		t.Fatalf("after MISS, upstream calls = %d, want 1", got)
	}

	rec2 := doMessages(t, s, anthropicReqBody(false))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	// The whole point: the HIT must NOT have called upstream again.
	if got := atomic.LoadInt32(&up.calls); got != 1 {
		t.Fatalf("after HIT, upstream calls = %d, want still 1 (the second request must be served from cache)", got)
	}

	// Same response bytes on the HIT as on the MISS.
	if rec1.Body.String() != rec2.Body.String() {
		t.Errorf("HIT response differs from MISS response:\nmiss=%s\nhit =%s", rec1.Body.String(), rec2.Body.String())
	}
	// And it is genuinely the translated Anthropic message.
	var msg anthropicMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &msg); err != nil {
		t.Fatalf("HIT body is not valid Anthropic JSON: %v", err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "cached hello" {
		t.Errorf("HIT content = %+v", msg.Content)
	}

	// Cache counters corroborate exactly one hit and one miss.
	st := mem.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Errorf("cache stats = %+v, want Hits=1 Misses=1", st)
	}
}

// A temperature>0 request is non-deterministic and must never be cached: two
// identical such requests must BOTH reach the upstream.
func TestCacheTemperatureNeverCached(t *testing.T) {
	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		return openAICompletionResponse(), nil
	}}
	mem := cache.NewMemoryLRU(cache.MemoryOptions{MaxEntries: 16})
	s := cacheTestServer(t, up, mem)

	_ = doMessages(t, s, anthropicReqBodyTemp(0.7))
	_ = doMessages(t, s, anthropicReqBodyTemp(0.7))

	if got := atomic.LoadInt32(&up.calls); got != 2 {
		t.Fatalf("temperature>0 upstream calls = %d, want 2 (never cached)", got)
	}
	if st := mem.Stats(); st.Entries != 0 {
		t.Errorf("cache entries = %d, want 0 (a sampled response must not be stored)", st.Entries)
	}
}

// A streaming request must bypass the cache entirely: it is never stored, so a
// repeat still reaches the upstream.
func TestCacheStreamingBypassesCache(t *testing.T) {
	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		const sse = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}}
	mem := cache.NewMemoryLRU(cache.MemoryOptions{MaxEntries: 16})
	s := cacheTestServer(t, up, mem)

	_ = doMessages(t, s, anthropicReqBody(true))
	_ = doMessages(t, s, anthropicReqBody(true))

	if got := atomic.LoadInt32(&up.calls); got != 2 {
		t.Fatalf("streaming upstream calls = %d, want 2 (streaming is never cached)", got)
	}
	if st := mem.Stats(); st.Entries != 0 || st.Lookups != 0 {
		t.Errorf("cache stats = %+v, want no entries and no lookups (streaming must not touch the cache)", st)
	}
}

// With a nil Cache (the default), the request path must be byte-identical to
// today: every request reaches the upstream, no lookup, no store.
func TestCacheNilBehaviourUnchanged(t *testing.T) {
	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		return openAICompletionResponse(), nil
	}}
	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = up
	// s.Cache stays nil.

	rec1 := doMessages(t, s, anthropicReqBody(false))
	rec2 := doMessages(t, s, anthropicReqBody(false))
	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("statuses = %d,%d, want 200,200", rec1.Code, rec2.Code)
	}
	if got := atomic.LoadInt32(&up.calls); got != 2 {
		t.Fatalf("nil-cache upstream calls = %d, want 2 (no caching)", got)
	}
}

// BuildCache is the config->store seam: disabled/nil yields no cache; memory
// and sqlite yield working stores; sqlite without a path errors.
func TestBuildCache(t *testing.T) {
	// nil and disabled → (nil, nil).
	if c, err := BuildCache(nil); err != nil || c != nil {
		t.Errorf("BuildCache(nil) = (%v,%v), want (nil,nil)", c, err)
	}
	if c, err := BuildCache(&config.CacheConfig{Enabled: false, Backend: "sqlite"}); err != nil || c != nil {
		t.Errorf("BuildCache(disabled) = (%v,%v), want (nil,nil)", c, err)
	}

	// memory (default backend) → a live store.
	c, err := BuildCache(&config.CacheConfig{Enabled: true})
	if err != nil || c == nil {
		t.Fatalf("BuildCache(memory) = (%v,%v), want a store", c, err)
	}
	_ = c.Close()

	// sqlite with a path → a live store.
	dbPath := t.TempDir() + "/cache.db"
	sc, err := BuildCache(&config.CacheConfig{Enabled: true, Backend: "sqlite", Path: dbPath})
	if err != nil || sc == nil {
		t.Fatalf("BuildCache(sqlite) = (%v,%v), want a store", sc, err)
	}
	_ = sc.Close()

	// sqlite without a path → error, no store.
	if bad, err := BuildCache(&config.CacheConfig{Enabled: true, Backend: "sqlite"}); err == nil || bad != nil {
		t.Errorf("BuildCache(sqlite,no-path) = (%v,%v), want (nil, error)", bad, err)
	}
}
