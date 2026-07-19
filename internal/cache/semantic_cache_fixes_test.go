package cache

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// reqWithLastUser builds a minimal request whose last user turn is text and
// whose scope (empty system/tools) is constant, so several such requests share
// one semantic scope.
func reqWithLastUser(text string) *translate.AnthropicRequest {
	content, _ := json.Marshal(text)
	return &translate.AnthropicRequest{
		Messages: []translate.AnthropicMessage{{Role: "user", Content: content}},
	}
}

// A SHORT salient turn (below minSalientRunes) must skip the semantic tier even
// at a low threshold — otherwise two unrelated conversations ending in the same
// "yes"/"continue" would cross-serve a wrong (though live) answer. And the guard
// must NOT over-block a genuinely long near-duplicate.
func TestSemanticCache_ShortTurnGuardedButLongNearDupStillMatches(t *testing.T) {
	exact := NewMemoryLRU(MemoryOptions{MaxEntries: 100})
	sc := NewSemanticCache(exact, NewLocalEmbedder(0), 0.5) // low threshold: WOULD match if unguarded

	long := reqWithLastUser("please reverse this list of integers for me")
	if err := sc.Store("kLong", long, &Entry{Key: "kLong", OpenAIBody: []byte("LONG-ANSWER")}); err != nil {
		t.Fatalf("store: %v", err)
	}

	// A short lookup is guarded to a miss even though a candidate exists and the
	// threshold is low.
	if e, kind, ok := sc.Lookup("kShort", reqWithLastUser("yes")); ok || kind != HitNone {
		t.Errorf("short lookup not guarded: kind=%v ok=%v entry=%v", kind, ok, e)
	}

	// Sanity: a sufficiently-long near-duplicate (one-word edit) STILL matches —
	// the guard blocks only short turns, not real questions.
	dup := reqWithLastUser("please reverse this list of integers for us")
	if _, kind, ok := sc.Lookup("kDup", dup); !ok || kind != HitSemantic {
		t.Errorf("long near-duplicate should still match: kind=%v ok=%v", kind, ok)
	}
}

// The per-scope candidate registry must stay bounded so a long-running gateway
// cannot leak embeddings + response bodies indefinitely.
func TestSemanticCache_RegistryIsBounded(t *testing.T) {
	exact := NewMemoryLRU(MemoryOptions{MaxEntries: 100000})
	sc := NewSemanticCache(exact, NewLocalEmbedder(0), 0.85)
	sc.maxCandidatesPerScope = 8 // small cap for the test

	for i := 0; i < 50; i++ {
		r := reqWithLastUser(fmt.Sprintf("please compute the factorial of the number %d for me", i))
		if err := sc.Store(fmt.Sprintf("k%d", i), r, &Entry{Key: fmt.Sprintf("k%d", i), OpenAIBody: []byte("A")}); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}

	scope := SystemHash(reqWithLastUser("scope-probe")) // same empty system/tools scope
	sc.mu.RLock()
	n := len(sc.candidates[scope])
	sc.mu.RUnlock()

	if n == 0 {
		t.Fatal("registry empty — candidates were not registered at all")
	}
	if n > 8 {
		t.Errorf("registry grew to %d candidates, want <= 8 (the cap)", n)
	}
}
