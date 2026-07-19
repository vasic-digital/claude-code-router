package cache

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// semThreshold cleanly separates a one-word edit (cosine ~0.97 with the
// LocalEmbedder) from unrelated text (~0.44); measured, not guessed.
const semThreshold = 0.85

// userReq builds a minimal cacheable request whose salient text is a single
// user string turn, with no system prompt or tools (so every such request
// shares one scope).
func userReq(text string) *translate.AnthropicRequest {
	body, _ := json.Marshal(text)
	return &translate.AnthropicRequest{
		Model:    "test-model",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: body}},
	}
}

// userReqSystem is userReq with a system prompt, to exercise scope isolation.
func userReqSystem(system, text string) *translate.AnthropicRequest {
	body, _ := json.Marshal(text)
	sys, _ := json.Marshal(system)
	return &translate.AnthropicRequest{
		Model:    "test-model",
		System:   sys,
		Messages: []translate.AnthropicMessage{{Role: "user", Content: body}},
	}
}

func newEntry(model string, body string) *Entry {
	return &Entry{Model: model, OpenAIBody: []byte(body)}
}

// An exact hit is served verbatim by the wrapped tier and reported as HitExact,
// even with a real embedder configured.
func TestSemanticCache_ExactHit(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	req := userReq("summarise the release notes for version two")
	body := `{"choices":[{"message":{"content":"ok"}}]}`

	if err := sc.Store("k1", req, newEntry("test-model", body)); err != nil {
		t.Fatalf("store: %v", err)
	}
	e, kind, ok := sc.Lookup("k1", req)
	if !ok || kind != HitExact {
		t.Fatalf("want exact hit, got kind=%v ok=%v", kind, ok)
	}
	if string(e.OpenAIBody) != body {
		t.Fatalf("body mismatch: %s", e.OpenAIBody)
	}
	if sc.SemanticHits() != 0 {
		t.Fatalf("exact hit must not count as semantic: %d", sc.SemanticHits())
	}
}

// A near-duplicate (one-word edit) request has a DIFFERENT exact key, so the
// exact tier misses; the semantic tier then serves the stored body as HitSemantic.
func TestSemanticCache_SemanticHitNearDuplicate(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	stored := userReq("please refactor the cache lookup to include the resolved model name")
	body := `{"choices":[{"message":{"content":"done"}}]}`
	if err := sc.Store("k1", stored, newEntry("test-model", body)); err != nil {
		t.Fatalf("store: %v", err)
	}

	// one-word edit (name -> id); different fingerprint key => exact miss.
	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	e, kind, ok := sc.Lookup("k2-different-key", nearDup)
	if !ok || kind != HitSemantic {
		t.Fatalf("want semantic hit, got kind=%v ok=%v", kind, ok)
	}
	if string(e.OpenAIBody) != body {
		t.Fatalf("semantic hit served wrong body: %s", e.OpenAIBody)
	}
	if e.Key != "k1" {
		t.Fatalf("semantic hit resolved to wrong entry key: %q", e.Key)
	}
	if sc.SemanticHits() != 1 {
		t.Fatalf("SemanticHits=%d want 1", sc.SemanticHits())
	}
}

// An unrelated request clears neither tier: exact miss, and cosine below threshold.
func TestSemanticCache_UnrelatedMisses(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	stored := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", stored, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store: %v", err)
	}

	unrelated := userReq("what is the capital of france today")
	e, kind, ok := sc.Lookup("k-unrelated", unrelated)
	if ok || kind != HitNone || e != nil {
		t.Fatalf("want miss, got kind=%v ok=%v e=%v", kind, ok, e)
	}
	if sc.SemanticHits() != 0 {
		t.Fatalf("SemanticHits=%d want 0", sc.SemanticHits())
	}
}

// Different system prompts land in different scopes and must never cross-serve,
// even for byte-identical user text.
func TestSemanticCache_ScopeIsolation(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)
	stored := userReqSystem("you are agent A", "please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", stored, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Same (near-duplicate) user text but a different system prompt => different scope.
	otherScope := userReqSystem("you are agent B", "please refactor the cache lookup to include the resolved model id")
	_, kind, ok := sc.Lookup("k2", otherScope)
	if ok || kind != HitNone {
		t.Fatalf("cross-scope serve: kind=%v ok=%v", kind, ok)
	}
}

// With NopEmbedder the semantic tier is fully inert: the wrapper is
// indistinguishable from the exact cache — exact hits/misses only, never a
// semantic hit, for both a near-duplicate and identical-key replay.
func TestSemanticCache_NopEmbedderInert(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NopEmbedder(), semThreshold)
	stored := userReq("please refactor the cache lookup to include the resolved model name")
	body := `{"choices":[{"message":{"content":"done"}}]}`
	if err := sc.Store("k1", stored, newEntry("test-model", body)); err != nil {
		t.Fatalf("store: %v", err)
	}

	// exact replay still works
	if e, kind, ok := sc.Lookup("k1", stored); !ok || kind != HitExact || string(e.OpenAIBody) != body {
		t.Fatalf("exact replay broken under NopEmbedder: kind=%v ok=%v", kind, ok)
	}

	// near-duplicate that WOULD hit semantically with a real embedder must miss
	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k2", nearDup); ok || kind != HitNone {
		t.Fatalf("NopEmbedder produced a semantic hit: kind=%v ok=%v", kind, ok)
	}
	if sc.SemanticHits() != 0 {
		t.Fatalf("SemanticHits=%d want 0 under NopEmbedder", sc.SemanticHits())
	}

	// Cross-check: an exact-only cache would answer these three lookups identically.
	exact := NewMemoryLRU(MemoryOptions{})
	_ = exact.Store("k1", newEntry("test-model", body))
	if _, ok := exact.Lookup("k1"); !ok {
		t.Fatal("baseline exact cache lost the entry")
	}
	if _, ok := exact.Lookup("k2"); ok {
		t.Fatal("baseline exact cache should miss k2")
	}
}

// nil embedder must also be inert (defaults to NopEmbedder).
func TestSemanticCache_NilEmbedderInert(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), nil, semThreshold)
	req := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", req, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store: %v", err)
	}
	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k2", nearDup); ok || kind != HitNone {
		t.Fatalf("nil embedder not inert: kind=%v ok=%v", kind, ok)
	}
}

// A semantic candidate whose exact entry has been evicted must not be served:
// the exact tier is the liveness authority.
func TestSemanticCache_EvictedCandidateNotServed(t *testing.T) {
	// Bound the exact tier to a single entry so storing a second evicts the first.
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{MaxEntries: 1}), NewLocalEmbedder(0), semThreshold)
	first := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("k1", first, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store k1: %v", err)
	}
	// Second store (unrelated scope-mate) evicts k1 from the bounded exact tier.
	second := userReq("completely different unrelated prompt about weather patterns")
	if err := sc.Store("k2", second, newEntry("test-model", "{}")); err != nil {
		t.Fatalf("store k2: %v", err)
	}

	// A near-duplicate of the EVICTED k1: candidate vector still ranks, but the
	// exact tier no longer holds k1, so it must be a miss (and pruned).
	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("k-nd", nearDup); ok || kind != HitNone {
		t.Fatalf("served an evicted candidate: kind=%v ok=%v", kind, ok)
	}
}

// Concurrency: many goroutines Store and Lookup at once; -race must stay clean
// and a known near-duplicate must still resolve to a semantic hit at the end.
func TestSemanticCache_Concurrent(t *testing.T) {
	sc := NewSemanticCache(NewMemoryLRU(MemoryOptions{}), NewLocalEmbedder(0), semThreshold)

	const workers = 16
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				text := fmt.Sprintf("worker %d iteration %d please compute the checksum of the payload", w, i)
				key := fmt.Sprintf("k-%d-%d", w, i)
				req := userReq(text)
				if err := sc.Store(key, req, newEntry("test-model", "{}")); err != nil {
					t.Errorf("store: %v", err)
					return
				}
				// exact replay
				if _, kind, ok := sc.Lookup(key, req); !ok || kind != HitExact {
					t.Errorf("concurrent exact replay failed: kind=%v ok=%v", kind, ok)
					return
				}
				// near-duplicate lookup (different key) — may hit exact-miss then
				// semantic; we only require it not to panic / race.
				nd := userReq(text + " now")
				_, _, _ = sc.Lookup("nd-"+key, nd)
			}
		}(w)
	}
	wg.Wait()

	// Deterministic final check: store one entry, then a one-word-edit lookup hits.
	base := userReq("please refactor the cache lookup to include the resolved model name")
	if err := sc.Store("final", base, newEntry("test-model", `{"ok":1}`)); err != nil {
		t.Fatalf("final store: %v", err)
	}
	nearDup := userReq("please refactor the cache lookup to include the resolved model id")
	if _, kind, ok := sc.Lookup("final-nd", nearDup); !ok || kind != HitSemantic {
		t.Fatalf("post-concurrency semantic hit failed: kind=%v ok=%v", kind, ok)
	}
}
