package cache

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// SemanticCache is the OPTIONAL Tier-2 wrapper the dossier's Phase 2 describes.
// It layers an embedding-similarity ("near-duplicate") tier on top of an
// existing exact Cache WITHOUT changing the exact tier's semantics: every
// Lookup consults the exact tier first and returns its verbatim result on a
// hit; the semantic tier is consulted ONLY on an exact miss, and only when a
// working Embedder is configured.
//
// # What is REAL here
//
//   - The wiring is real: exact-first, semantic-on-miss, scope-isolated
//     candidate search, and the exact tier as the single authority for an
//     entry's body and liveness (a semantic match re-reads the exact tier so
//     TTL/eviction are honoured — a stale candidate is never served and is
//     pruned).
//   - With a real Embedder (LocalEmbedder) the similarity is a genuine LEXICAL
//     near-duplicate signal: a re-asked prompt, a retry, or a one-word edit
//     scores high; unrelated text scores low. That is exactly the re-ask /
//     retry traffic Phase 2 targets.
//
// # What is a documented SEAM / boundary (no bluff)
//
//   - This is NOT deep paraphrase matching. LocalEmbedder keys off surface
//     character n-grams, so "reverse a list" vs "flip an array" is NOT reliably
//     caught. Deep paraphrase needs a learned embedding model wired as the
//     Embedder — the same seam semantic.go documents. This wrapper does not
//     fabricate that capability.
//   - With NopEmbedder() (or any Embedder that errors) the semantic tier is
//     fully INERT: the Embedder's error degrades every semantic path to a miss,
//     so a SemanticCache is byte-for-byte indistinguishable from the exact
//     Cache it wraps. This is proven in semantic_cache_test.go.
//
// SemanticCache is safe for concurrent use.
//
// # Method-signature decision (why not the Cache interface)
//
// SemanticCache deliberately does NOT implement Cache. The Cache interface's
// Lookup(key) and Store(key, e) take only the fingerprint key, and a hash key
// cannot drive similarity — by construction two near-duplicate requests hash to
// DIFFERENT keys, which is the whole reason the exact tier misses on them. The
// similarity query therefore needs the request's salient TEXT, which only the
// request itself carries. So both methods take the *translate.AnthropicRequest
// alongside the key:
//
//   - Lookup(key, req) — the key drives the exact tier; req supplies the salient
//     text (last user turn) to embed and the scope (system+tools hash) to filter
//     candidates on an exact miss.
//   - Store(key, req, e) — stores in the exact tier unchanged, then embeds req's
//     salient text and registers e as a scoped semantic candidate.
//
// Passing the request (not a pre-extracted string) keeps salient-text
// extraction and scoping inside the cache, so callers cannot accidentally embed
// the wrong text or mis-scope a candidate.
type SemanticCache struct {
	exact Cache
	index *SemanticIndex

	maxSalientRunes int
	// minSalientRunes is the floor below which a request's salient text is too
	// short to be a reliable near-duplicate signal: a bare "yes"/"continue" is
	// lexically identical across UNRELATED conversations in the same scope and
	// would cross-serve a wrong (though live) answer. Such requests skip the
	// semantic tier entirely — neither queried nor registered.
	minSalientRunes int
	// maxCandidatesPerScope bounds the per-scope candidate registry. The registry
	// is pruned only LAZILY (when a semantic hit ranks an already-evicted entry),
	// so without a cap an expired/evicted entry that is never re-queried would
	// linger forever, pinning its embedding + response body — an unbounded leak
	// in a long-running gateway. When the cap is exceeded the OLDEST candidates
	// are dropped (the exact tier remains the authority, so a dropped candidate
	// only forgoes a future semantic hit, never a correct exact hit).
	maxCandidatesPerScope int

	mu sync.RWMutex
	// candidates maps a scope id (SystemHash) to the entries in that scope that
	// carry an embedding. The exact tier owns the authoritative bodies; this
	// registry only holds the vectors the similarity search ranks over.
	candidates map[string][]*Entry

	lookups      atomic.Int64
	exactHits    atomic.Int64
	semanticHits atomic.Int64
	misses       atomic.Int64
}

// DefaultMaxSalientRunes bounds how much of the last user turn is embedded. A
// few thousand runes is plenty to characterise a request's intent while keeping
// the embed step cheap and bounding memory.
const DefaultMaxSalientRunes = 4096

// DefaultMinSalientRunes is the floor for semantic-tier participation (see
// SemanticCache.minSalientRunes): ~16 runes keeps out bare "yes"/"continue"
// turns that would cross-serve, while admitting any real question.
const DefaultMinSalientRunes = 16

// DefaultMaxCandidatesPerScope bounds the per-scope candidate registry (see
// SemanticCache.maxCandidatesPerScope) so a long-running gateway cannot leak.
const DefaultMaxCandidatesPerScope = 1024

// HitKind distinguishes how a Lookup was satisfied, so the caller (and metrics)
// can tell an exact replay apart from a similarity match — the two carry
// different confidence and belong on different counters.
type HitKind int

const (
	// HitNone means the lookup missed both tiers.
	HitNone HitKind = iota
	// HitExact means the exact (fingerprint) tier served the entry.
	HitExact
	// HitSemantic means the exact tier missed and a near-duplicate candidate
	// above the similarity threshold served the entry.
	HitSemantic
)

// String renders a stable, metric-friendly label for the hit kind.
func (h HitKind) String() string {
	switch h {
	case HitExact:
		return "exact"
	case HitSemantic:
		return "semantic"
	default:
		return "none"
	}
}

// NewSemanticCache wraps an exact Cache with a semantic tier driven by emb and
// the given cosine threshold. A nil emb falls back to NopEmbedder(), which makes
// the semantic tier inert (the wrapper then behaves exactly like exact). A
// non-positive threshold is left as-is and passed to the index; callers should
// pass a deliberate floor (e.g. ~0.85 for near-duplicate lexical matching).
func NewSemanticCache(exact Cache, emb Embedder, threshold float64) *SemanticCache {
	if emb == nil {
		emb = NopEmbedder()
	}
	return &SemanticCache{
		exact:                 exact,
		index:                 &SemanticIndex{Embedder: emb, Threshold: threshold},
		maxSalientRunes:       DefaultMaxSalientRunes,
		minSalientRunes:       DefaultMinSalientRunes,
		maxCandidatesPerScope: DefaultMaxCandidatesPerScope,
		candidates:            make(map[string][]*Entry),
	}
}

// Lookup consults the exact tier first (verbatim Cache.Lookup semantics). On an
// exact hit it returns (entry, HitExact, true). On an exact MISS, and only when
// a working Embedder is configured, it embeds req's salient text, searches the
// same-scope candidate set, and — if the nearest candidate is at or above the
// threshold AND is still live in the exact tier — returns (entry, HitSemantic,
// true). Otherwise (nil, HitNone, false).
//
// A semantic match is re-read from the exact tier so the returned entry is the
// authoritative, non-expired body with its HitCount bumped; a candidate the
// exact tier has since dropped is treated as a miss and pruned.
func (sc *SemanticCache) Lookup(key string, req *translate.AnthropicRequest) (*Entry, HitKind, bool) {
	sc.lookups.Add(1)

	if e, ok := sc.exact.Lookup(key); ok {
		sc.exactHits.Add(1)
		return e, HitExact, true
	}

	if e, ok := sc.semanticLookup(req); ok {
		sc.semanticHits.Add(1)
		return e, HitSemantic, true
	}

	sc.misses.Add(1)
	return nil, HitNone, false
}

// semanticLookup performs the exact-miss similarity search. It returns
// (nil,false) whenever the tier is inert (no/failing embedder), the request has
// no salient text, or nothing clears the threshold — every one of those is a
// clean miss, never a fabricated hit.
func (sc *SemanticCache) semanticLookup(req *translate.AnthropicRequest) (*Entry, bool) {
	if req == nil {
		return nil, false
	}
	text := salientText(req, sc.maxSalientRunes)
	if text == "" || len([]rune(text)) < sc.minSalientRunes {
		// Empty, or too short to be a reliable near-duplicate signal (see
		// minSalientRunes): a clean miss, never a cross-served wrong answer.
		return nil, false
	}

	vec, err := sc.index.Embedder.Embed(context.Background(), text)
	if err != nil || len(vec) == 0 {
		// Inert embedder (NopEmbedder → ErrNoEmbedder) or a transient failure:
		// degrade to a plain exact-tier miss. Never guess.
		return nil, false
	}

	scope := SystemHash(req)

	sc.mu.RLock()
	scoped := sc.candidates[scope]
	local := make([]*Entry, len(scoped))
	copy(local, scoped)
	sc.mu.RUnlock()

	cand, ok := sc.index.Nearest(vec, local)
	if !ok {
		return nil, false
	}

	// The exact tier is the source of truth for liveness and HitCount: re-read
	// the matched key. If it is gone (evicted/expired), this is a miss and the
	// stale candidate is pruned so it stops being ranked.
	if live, ok := sc.exact.Lookup(cand.Entry.Key); ok {
		return live, true
	}
	sc.pruneCandidate(scope, cand.Entry.Key)
	return nil, false
}

// Store writes to the exact tier UNCHANGED, then — best effort — embeds req's
// salient text and registers e as a semantic candidate in its scope. If the
// exact Store fails, that error is returned and no candidate is registered. A
// failing/absent embedder simply skips candidate registration, so the wrapper
// stays identical to the exact cache.
func (sc *SemanticCache) Store(key string, req *translate.AnthropicRequest, e *Entry) error {
	if e == nil {
		return nil
	}
	if err := sc.exact.Store(key, e); err != nil {
		return err
	}
	if req == nil {
		return nil
	}

	text := salientText(req, sc.maxSalientRunes)
	if text == "" || len([]rune(text)) < sc.minSalientRunes {
		// Too short (or absent) to register as a reliable near-duplicate
		// candidate — mirrors the semanticLookup guard so short turns never
		// cross-serve.
		return nil
	}
	vec, err := sc.index.Embedder.Embed(context.Background(), text)
	if err != nil || len(vec) == 0 {
		return nil // inert / transient: exact-only, no candidate
	}

	scope := SystemHash(req)
	cand := e.clone()
	cand.Key = key
	cand.Embedding = vec
	if cand.SystemHash == "" {
		cand.SystemHash = scope
	}

	sc.mu.Lock()
	sc.registerLocked(scope, key, cand)
	sc.mu.Unlock()
	return nil
}

// registerLocked inserts cand under scope, replacing any prior candidate with
// the same key (a re-Store of the same request updates the vector rather than
// duplicating it). Caller holds sc.mu.
func (sc *SemanticCache) registerLocked(scope, key string, cand *Entry) {
	list := sc.candidates[scope]
	for i, ex := range list {
		if ex.Key == key {
			list[i] = cand
			sc.candidates[scope] = list
			return
		}
	}
	list = append(list, cand)
	// Bound per-scope growth: drop the oldest beyond the cap. Copy into a fresh
	// slice (rather than reslice) so the dropped candidates — and the response
	// bodies they pin — are actually released, not just hidden behind a moved
	// slice header on the same backing array.
	if sc.maxCandidatesPerScope > 0 && len(list) > sc.maxCandidatesPerScope {
		trimmed := make([]*Entry, sc.maxCandidatesPerScope)
		copy(trimmed, list[len(list)-sc.maxCandidatesPerScope:])
		list = trimmed
	}
	sc.candidates[scope] = list
}

// pruneCandidate drops the candidate with key from scope. Used when the exact
// tier reports the entry is no longer live.
func (sc *SemanticCache) pruneCandidate(scope, key string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	list := sc.candidates[scope]
	for i, ex := range list {
		if ex.Key == key {
			sc.candidates[scope] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// Stats returns the wrapped exact tier's Stats verbatim. Note that a semantic
// hit is preceded by an exact-tier miss, so the exact tier's Miss counter
// counts it; SemanticHits() exposes the semantic-tier count the exact Stats
// cannot see.
func (sc *SemanticCache) Stats() Stats {
	// Report THIS wrapper's own lookup accounting, not the exact tier's verbatim.
	// A semantic hit does an exact miss FOLLOWED by an exact re-read (the
	// liveness check), so returning sc.exact.Stats() would count one logical
	// Lookup as two, plus a spurious Miss+Hit. Lookups/Hits/Misses here reflect
	// SemanticCache.Lookup calls (Hits = exact + semantic); Entries/Evictions/
	// Expirations come from the exact tier, which remains the authority for
	// stored entries.
	base := sc.exact.Stats()
	return Stats{
		Entries:     base.Entries,
		Lookups:     sc.lookups.Load(),
		Hits:        sc.exactHits.Load() + sc.semanticHits.Load(),
		Misses:      sc.misses.Load(),
		Evictions:   base.Evictions,
		Expirations: base.Expirations,
	}
}

// SemanticHits returns how many Lookups were satisfied by the semantic tier
// (after an exact miss). Companion to Stats for observability.
func (sc *SemanticCache) SemanticHits() int64 { return sc.semanticHits.Load() }

// ExactHits returns how many Lookups were satisfied by the exact tier.
func (sc *SemanticCache) ExactHits() int64 { return sc.exactHits.Load() }

// Close releases the wrapped exact tier.
func (sc *SemanticCache) Close() error { return sc.exact.Close() }

// salientText extracts the text of the last user turn, bounded to max runes. It
// mirrors the dossier's "last user message text" salient-text rule (system and
// tools are handled separately, via the scope hash). Non-text blocks
// (tool_use/tool_result/image) are ignored: they do not characterise the
// question and would only add noise to the lexical signal.
func salientText(req *translate.AnthropicRequest, max int) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		t := messageText(m.Content)
		if t == "" {
			continue
		}
		if max > 0 {
			if r := []rune(t); len(r) > max {
				t = string(r[:max])
			}
		}
		return t
	}
	return ""
}

// messageText renders an Anthropic message's polymorphic content to plain text:
// a bare JSON string is returned as-is; a content-block array contributes the
// concatenation of its type=="text" blocks. Anything that parses as neither
// yields "".
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []translate.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" && bl.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}
