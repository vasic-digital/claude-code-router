package gateway

// Proof that the semantic cache tier is wired end-to-end through the gateway:
// with Config.Cache.Semantic set, BuildCache yields a *cache.SemanticCache, and
// a near-duplicate (one-word-different) repeat of a cacheable non-streaming
// request is served from the semantic tier WITHOUT a second upstream call — even
// though its fingerprint differs from the first (so the exact tier misses).

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/cache"
	"github.com/vasic-digital/claude-code-router/internal/config"
)

// semanticReqBody is a cacheable (no temperature, non-streaming) request whose
// single user turn is long enough to clear the semantic tier's minimum-salient
// floor, so a one-word edit is a genuine near-duplicate rather than a too-short
// turn the tier deliberately ignores.
func semanticReqBody(userText string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": userText},
		},
	})
	return b
}

func TestSemanticCacheWiredNearDuplicateServedWithoutUpstream(t *testing.T) {
	built, err := BuildCache(&config.CacheConfig{Enabled: true, Semantic: true, SemanticThreshold: 0.6})
	if err != nil || built == nil {
		t.Fatalf("BuildCache(semantic) = (%v,%v), want a store", built, err)
	}
	sc, ok := built.(*cache.SemanticCache)
	if !ok {
		t.Fatalf("BuildCache(semantic) returned %T, want *cache.SemanticCache", built)
	}
	defer sc.Close()

	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		return openAICompletionResponse(), nil
	}}
	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = up
	s.Cache = built

	// MISS: one upstream call; the response is stored and registered as a
	// semantic candidate for this scope.
	rec1 := doMessages(t, s, semanticReqBody(
		"Summarize the quarterly financial report in three concise bullet points please"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, body=%s", rec1.Code, rec1.Body.String())
	}
	if got := atomic.LoadInt32(&up.calls); got != 1 {
		t.Fatalf("after MISS, upstream calls = %d, want 1", got)
	}

	// Near-duplicate: differs by one word, so its fingerprint differs (the exact
	// tier misses) but its salient text is a lexical near-duplicate — the
	// semantic tier must serve it with NO second upstream call.
	rec2 := doMessages(t, s, semanticReqBody(
		"Summarize the quarterly financial report in three short concise bullet points please"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if got := atomic.LoadInt32(&up.calls); got != 1 {
		t.Fatalf("after near-duplicate, upstream calls = %d, want still 1 (the semantic tier must serve it)", got)
	}
	if got := sc.SemanticHits(); got != 1 {
		t.Errorf("SemanticHits() = %d, want 1 (the second request must be a semantic hit)", got)
	}

	// The served body is the genuine translated Anthropic message.
	var msg anthropicMessage
	if err := json.Unmarshal(rec2.Body.Bytes(), &msg); err != nil {
		t.Fatalf("semantic-hit body is not valid Anthropic JSON: %v", err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "cached hello" {
		t.Errorf("semantic-hit content = %+v", msg.Content)
	}
}

// An UNRELATED second request in the same scope must NOT be served by the
// semantic tier: it reaches the upstream, proving the wiring is a real
// similarity gate and not a "serve anything in scope" bug.
func TestSemanticCacheWiredUnrelatedStillHitsUpstream(t *testing.T) {
	built, err := BuildCache(&config.CacheConfig{Enabled: true, Semantic: true, SemanticThreshold: 0.85})
	if err != nil || built == nil {
		t.Fatalf("BuildCache(semantic) = (%v,%v), want a store", built, err)
	}
	sc := built.(*cache.SemanticCache)
	defer sc.Close()

	up := &countingUpstream{do: func(int32) (*http.Response, error) {
		return openAICompletionResponse(), nil
	}}
	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Upstream = up
	s.Cache = built

	_ = doMessages(t, s, semanticReqBody(
		"Summarize the quarterly financial report in three concise bullet points please"))
	_ = doMessages(t, s, semanticReqBody(
		"Explain how photosynthesis converts sunlight into chemical energy in plants"))

	if got := atomic.LoadInt32(&up.calls); got != 2 {
		t.Fatalf("unrelated requests upstream calls = %d, want 2 (no false semantic hit)", got)
	}
	if got := sc.SemanticHits(); got != 0 {
		t.Errorf("SemanticHits() = %d, want 0 for unrelated requests", got)
	}
}
