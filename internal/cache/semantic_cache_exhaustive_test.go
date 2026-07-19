package cache

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// userReqTools builds a request whose scope is distinguished by a tool
// definition (rather than a system prompt), to exercise tool-based scoping.
func userReqTools(toolName, text string) *translate.AnthropicRequest {
	body, _ := json.Marshal(text)
	return &translate.AnthropicRequest{
		Model:    "test-model",
		Tools:    []translate.AnthropicTool{{Name: toolName, InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []translate.AnthropicMessage{{Role: "user", Content: body}},
	}
}

// --- Exact-first authority: the exact tier short-circuits the semantic tier --
//
// When the very request that was stored is looked up under its own key, the
// exact tier serves it and the semantic path is never consulted — even though
// the request's own text is a perfect self-similarity match that WOULD score
// 1.0 semantically.
func TestSemanticCache_ExactFirstShortCircuits(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	req := userReq("please compute the running median over the sliding window of samples")
	body := `{"choices":[{"message":{"content":"exact"}}]}`
	if err := sc.Store("k1", req, newEntry("test-model", body)); err != nil {
		t.Fatalf("store: %v", err)
	}
	e, kind, ok := sc.Lookup("k1", req)
	if !ok || kind != HitExact {
		t.Fatalf("want exact hit, got kind=%v ok=%v", kind, ok)
	}
	if string(e.OpenAIBody) != body {
		t.Fatalf("wrong body: %s", e.OpenAIBody)
	}
	if sc.SemanticHits() != 0 {
		t.Fatalf("SemanticHits=%d want 0 (exact must short-circuit)", sc.SemanticHits())
	}
}

// --- Semantic tier serves the NEAREST of several scope-mates ----------------
func TestSemanticCache_NearestOfManyCandidates(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)

	stored := map[string]struct{ key, body, text string }{
		"earnings": {"kE", "EARNINGS", "please summarize the quarterly earnings report for the finance team"},
		"weather":  {"kW", "WEATHER", "what is the extended weather forecast for the pacific northwest region"},
		"deploy":   {"kD", "DEPLOY", "roll out the new gateway deployment to the production cluster tonight"},
	}
	for _, s := range stored {
		if err := sc.Store(s.key, userReq(s.text), newEntry("test-model", s.body)); err != nil {
			t.Fatalf("store %s: %v", s.key, err)
		}
	}

	// Near-duplicate of the earnings entry (one word changed) under a fresh key.
	nearDup := userReq("please summarize the quarterly earnings report for the finance group")
	e, kind, ok := sc.Lookup("query-key", nearDup)
	if !ok || kind != HitSemantic {
		t.Fatalf("want semantic hit, got kind=%v ok=%v", kind, ok)
	}
	if string(e.OpenAIBody) != "EARNINGS" || e.Key != "kE" {
		t.Fatalf("resolved to wrong candidate: key=%q body=%q", e.Key, e.OpenAIBody)
	}
}

// --- Tool-based scope isolation: different tools never cross-serve ----------
func TestSemanticCache_ToolScopeIsolation(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	stored := userReqTools("search", "please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", stored, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store: %v", err)
	}
	// Byte-near-identical user text but a DIFFERENT tool => different scope.
	other := userReqTools("calculator", "please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k2", other); ok || kind != HitNone {
		t.Fatalf("cross-tool-scope serve: kind=%v ok=%v", kind, ok)
	}
	// Same tool + near-duplicate text DOES semantic-hit (control).
	same := userReqTools("search", "please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k3", same); !ok || kind != HitSemantic {
		t.Fatalf("same-tool near-dup should hit: kind=%v ok=%v", kind, ok)
	}
}

// --- Liveness authority: a TTL-EXPIRED candidate is never served, and pruned -
func TestSemanticCache_ExpiredCandidateNotServedAndPruned(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	exact := NewMemoryLRU(MemoryOptions{TTL: 30 * time.Second})
	exact.now = clk.now
	sc := NewSemanticCache(exact, NewLocalEmbedder(0), semThreshold)

	stored := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", stored, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Advance past the exact tier's TTL so k1 is expired there.
	clk.add(31 * time.Second)

	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k-nd", nearDup); ok || kind != HitNone {
		t.Fatalf("served an expired candidate: kind=%v ok=%v", kind, ok)
	}

	// The stale candidate must have been pruned from the scope registry.
	scope := SystemHash(stored)
	sc.mu.RLock()
	n := len(sc.candidates[scope])
	sc.mu.RUnlock()
	if n != 0 {
		t.Fatalf("expired candidate not pruned: registry still holds %d", n)
	}
}

// --- Store-side short-turn guard: a short turn registers NO candidate -------
func TestSemanticCache_ShortTurnNotRegistered(t *testing.T) {
	exact := NewMemoryLRU(MemoryOptions{})
	sc := NewSemanticCache(exact, NewLocalEmbedder(0), 0.5) // low threshold: would match if registered

	short := reqWithLastUser("ok") // below DefaultMinSalientRunes
	if err := sc.Store("kShort", short, &Entry{Key: "kShort", OpenAIBody: []byte("SHORT")}); err != nil {
		t.Fatalf("store: %v", err)
	}
	// Exact replay still works (the exact tier is unconditional).
	if _, kind, ok := sc.Lookup("kShort", short); !ok || kind != HitExact {
		t.Fatalf("exact replay of short turn failed: kind=%v ok=%v", kind, ok)
	}
	// But it must NOT have been registered as a semantic candidate.
	scope := SystemHash(short)
	sc.mu.RLock()
	n := len(sc.candidates[scope])
	sc.mu.RUnlock()
	if n != 0 {
		t.Fatalf("short turn wrongly registered as a candidate: %d in registry", n)
	}
}

// --- nil request on the semantic path is a clean miss (no panic) ------------
func TestSemanticCache_NilRequestLookupMisses(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	// exact miss (nothing stored) then nil req => semanticLookup returns miss.
	if e, kind, ok := sc.Lookup("absent", nil); ok || kind != HitNone || e != nil {
		t.Fatalf("nil-req lookup: kind=%v ok=%v e=%v", kind, ok, e)
	}
	// Store with a nil request must not panic and must skip candidate registration.
	if err := sc.Store("k", nil, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store nil-req: %v", err)
	}
	// The exact entry is still retrievable by key.
	if _, ok := sc.exact.Lookup("k"); !ok {
		t.Fatal("exact entry lost when stored with a nil request")
	}
}

// --- Store propagates the exact tier's error and registers no candidate -----
func TestSemanticCache_StoreErrorPropagates(t *testing.T) {
	// A closed SQLite cache fails every Store — a real exact-tier error.
	path := t.TempDir() + "/exact.db"
	exact, err := NewSQLiteCache(path, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = exact.Close() // now every Store errors

	sc := NewSemanticCache(exact, NewLocalEmbedder(0), semThreshold)
	req := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", req, newEntry("test-model", "{}")); err == nil {
		t.Fatal("Store must return the exact tier's error")
	}
	// A failed exact Store must NOT have leaked a semantic candidate.
	scope := SystemHash(req)
	sc.mu.RLock()
	n := len(sc.candidates[scope])
	sc.mu.RUnlock()
	if n != 0 {
		t.Fatalf("failed Store registered %d candidate(s); must register none", n)
	}
}

// --- Re-Store of the same key updates the candidate, never duplicates -------
func TestSemanticCache_ReStoreUpdatesCandidate(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	req := userReq("please refactor the cache lookup to include the resolved model name")
	scope := SystemHash(req)

	for i := 0; i < 5; i++ {
		if err := sc.Store("k1", req, newEntry("test-model", fmt.Sprintf(`{"v":%d}`, i))); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}
	sc.mu.RLock()
	n := len(sc.candidates[scope])
	sc.mu.RUnlock()
	if n != 1 {
		t.Fatalf("re-Store of same key produced %d candidates, want 1", n)
	}
	// The latest body is what a near-duplicate resolves to.
	nd := userReq("please refactor the cache lookup to include the resolved model id")
	e, kind, ok := sc.Lookup("k-nd", nd)
	if !ok || kind != HitSemantic {
		t.Fatalf("near-dup lookup: kind=%v ok=%v", kind, ok)
	}
	if string(e.OpenAIBody) != `{"v":4}` {
		t.Fatalf("stale body served: %s want {\"v\":4}", e.OpenAIBody)
	}
}

// --- Counter exactness invariant under concurrency --------------------------
//
// Every Lookup lands in exactly one of {exactHit, semanticHit, miss}; the three
// internal counters must therefore sum to the lookup count, with no double- or
// under-counting, even under concurrent load. -race must stay clean.
func TestSemanticCache_ConcurrentCounterInvariant(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)

	const workers = 12
	const perWorker = 60
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				text := fmt.Sprintf("worker %d step %d please compute the checksum of the given payload", w, i)
				key := fmt.Sprintf("k-%d-%d", w, i)
				req := userReq(text)
				if err := sc.Store(key, req, newEntry("test-model", "{}")); err != nil {
					t.Errorf("store: %v", err)
					return
				}
				_, _, _ = sc.Lookup(key, req)                                    // exact hit
				_, _, _ = sc.Lookup("miss-"+key, userReq(text+" appended tail")) // exact miss (maybe semantic)
			}
		}(w)
	}
	wg.Wait()

	lookups := sc.lookups.Load()
	sum := sc.exactHits.Load() + sc.semanticHits.Load() + sc.misses.Load()
	if lookups != sum {
		t.Fatalf("counter invariant broken: lookups=%d != exact(%d)+semantic(%d)+miss(%d)=%d",
			lookups, sc.exactHits.Load(), sc.semanticHits.Load(), sc.misses.Load(), sum)
	}
	if lookups != int64(workers*perWorker*2) {
		t.Fatalf("lookups=%d want %d", lookups, workers*perWorker*2)
	}
	if got, want := sc.exactHits.Load(), int64(workers*perWorker); got != want {
		t.Fatalf("exactHits=%d want %d (each own-key replay is a guaranteed exact hit)", got, want)
	}
	if sc.SemanticHits() != sc.semanticHits.Load() {
		t.Fatal("SemanticHits() accessor disagrees with internal counter")
	}
}
