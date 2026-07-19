// Package cache implements the response cache described in
// docs/research/innovations/02-semantic-response-cache.md.
//
// It sits (in a future gateway change — NOT wired here) between request
// translation and the paid upstream call in handleMessages, so that repeated
// requests are served from a local store instead of paying a provider
// round-trip.
//
// This package is deliberately self-contained: it depends only on the standard
// library, modernc.org/sqlite (pure-Go, no CGO — preserves the static-binary
// property), and internal/translate for the real AnthropicRequest shape a cache
// key is derived from. It does NOT import internal/gateway; the gateway will
// import it.
//
// # Tiers
//
//   - Tier 1 (exact) is fully implemented: a canonical, secret-free SHA-256
//     fingerprint of the routed request keys an LRU (in-memory) or SQLite
//     (persistent) store, both behind the Cache interface, both with TTL/expiry.
//   - Tier 2 (semantic) is a REAL similarity seam, not a stub: the cosine math
//     and vector codec are implemented and tested, and SemanticIndex performs a
//     genuine brute-force nearest-neighbour search. The one piece that is
//     intentionally left as a documented, not-implemented extension is the
//     Embedder — producing an embedding requires a live model, and this package
//     will not ship a fake one. See semantic.go.
//
// # Safety gates
//
// Correctness of a response cache lives or dies on its gates. This package
// implements them as tested predicates (see fingerprint.go and gate.go):
//
//   - Temperature gate: never cache a sampled (temperature > 0) request.
//   - Streaming gate: never cache a streaming request (Phase 1 cannot faithfully
//     replay an SSE stream; streaming replay is Phase 3).
//   - Tool gate: never cache a response that carries tool_use / tool_calls
//     unless the caller explicitly opts in (the answer depends on live tool
//     state).
//   - Error gate: never cache a non-success / error-shaped upstream body.
//   - Scope isolation: the fingerprint includes the provider name and resolved
//     model, so two different providers/models can never collide on one key, and
//     the system+tools hash keeps two agents with different instructions apart.
package cache

import "time"

// Entry is one cached upstream response plus the metadata the store and the
// (future) semantic tier need. It mirrors the cache_entries / cache_embeddings
// DDL in the dossier. No API key, Authorization header, or raw provider
// credential is ever stored on an Entry — only the provider NAME and model.
type Entry struct {
	// Key is the hex SHA-256 fingerprint (see Fingerprint). Set by the caller.
	Key string
	// ProviderName is the resolved provider's NAME (never its API key).
	ProviderName string
	// Model is the resolved upstream model id.
	Model string
	// SystemHash is sha256(canonical system + tools) for scope isolation.
	SystemHash string
	// OpenAIBody is the buffered upstream response (OpenAI chat shape) to replay
	// verbatim on a hit. Treated as immutable once stored.
	OpenAIBody []byte
	// InputTokens / OutputTokens are the usage of the original generation, kept
	// so analytics can report tokens saved on a hit.
	InputTokens  int
	OutputTokens int
	// HitCount is how many times this entry has been served. Maintained by the
	// store; informational on a returned Entry.
	HitCount int
	// Embedding is the optional semantic-tier vector. nil in Phase 1.
	Embedding []float32
	// CreatedAt / ExpiresAt bound the entry's life. A zero ExpiresAt means "no
	// expiry"; otherwise a Lookup after ExpiresAt is a miss.
	CreatedAt time.Time
	ExpiresAt time.Time
}

// expired reports whether the entry is past its expiry at time now. A zero
// ExpiresAt is never expired.
func (e *Entry) expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt)
}

// clone returns a shallow copy of the entry. OpenAIBody and Embedding are
// shared (both are immutable once stored); everything else is copied so a
// caller can read a returned Entry without racing the store's own bookkeeping.
func (e *Entry) clone() *Entry {
	cp := *e
	return &cp
}

// Stats is a snapshot of a store's counters. It feeds the Theme 04 metrics /
// management UI in a later phase.
type Stats struct {
	// Entries is the number of live (non-expired) entries currently held.
	Entries int
	// Lookups, Hits, Misses count calls to Lookup. Hits+Misses == Lookups.
	Lookups int64
	Hits    int64
	Misses  int64
	// Evictions counts entries dropped to honour the size/byte bound (LRU only).
	Evictions int64
	// Expirations counts entries dropped because they were past ExpiresAt.
	Expirations int64
}

// Cache is the interface the gateway will consume. Both the in-memory LRU and
// the SQLite store implement it. Implementations must be safe for concurrent
// use.
type Cache interface {
	// Lookup returns the live entry for key and true, or (nil, false) on a miss
	// (absent OR expired). A hit increments the entry's HitCount.
	Lookup(key string) (*Entry, bool)
	// Store inserts or replaces the entry under key. If e.ExpiresAt is zero and
	// the store was configured with a TTL, the store sets ExpiresAt to
	// now+TTL; a non-zero ExpiresAt is honoured as-is. CreatedAt is set to now
	// when zero.
	Store(key string, e *Entry) error
	// Stats returns a snapshot of the store's counters.
	Stats() Stats
	// Close releases any resources (a no-op for the in-memory store).
	Close() error
}
