package cache

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

// LocalEmbedder is a real, deterministic, dependency-free Embedder that turns
// text into a fixed-dimension vector using the feature-hashing (a.k.a. "hashing
// trick") of character n-grams. It activates the semantic tier's documented
// ErrNoEmbedder seam WITHOUT a live model and WITHOUT a network call.
//
// # What it IS
//
//   - A genuine, reproducible text->vector map. Identical text always yields the
//     identical vector (byte-for-byte after Float32ToBlob); no rand, no clock,
//     no goroutine ordering. It is safe for concurrent use (no shared state).
//   - A LEXICAL similarity signal: two strings that share character n-grams land
//     near each other in cosine space, so near-duplicates (a re-asked prompt, a
//     retried request, a one-word edit) score high and unrelated text scores low.
//     That is exactly the near-duplicate / re-ask traffic the dossier's Phase 2
//     targets, and cosine over these vectors is meaningful because they are
//     L2-normalised.
//
// # What it is NOT
//
//   - It is NOT a learned/neural embedding model and does not pretend to be one.
//     It has no semantic understanding: it keys off surface character overlap,
//     so genuine PARAPHRASE with little lexical overlap ("How do I reverse a
//     list?" vs "What's the way to flip an array?") is NOT reliably captured.
//     For deep paraphrase equivalence a real embedding provider is still the
//     right tool — this is a working, honest local approximation, not a fake of
//     a learned model.
//   - It is NOT a stub: it never returns ErrNoEmbedder and produces usable
//     vectors for real traffic.
//
// The approach: normalise (lowercase + whitespace-collapse) the text, slide a
// character n-gram window over it, hash each n-gram to a bucket with a signed
// contribution (feature hashing — a second hash bit picks the sign so bucket
// collisions cancel on average rather than always accumulating), then
// L2-normalise the accumulated vector.
type LocalEmbedder struct {
	dims  int
	ngram int
}

// DefaultEmbedderDims is the vector width NewLocalEmbedder uses when given a
// non-positive dims. 256 is a good balance: wide enough that hash collisions are
// rare for the short salient texts the cache embeds, narrow enough to stay cheap
// to store (256*4 = 1 KiB per entry) and to cosine-compare.
const DefaultEmbedderDims = 256

// defaultNgram is the character-window width. 3 (trigrams) is the classic choice
// for lexical near-duplicate detection: small enough to survive single-word
// edits, large enough to be discriminative.
const defaultNgram = 3

// NewLocalEmbedder returns a LocalEmbedder producing dims-dimensional vectors. A
// non-positive dims falls back to DefaultEmbedderDims, so NewLocalEmbedder(0) is
// a sane default constructor.
func NewLocalEmbedder(dims int) *LocalEmbedder {
	if dims <= 0 {
		dims = DefaultEmbedderDims
	}
	return &LocalEmbedder{dims: dims, ngram: defaultNgram}
}

// Dims reports the (stable) dimensionality of vectors this embedder produces.
func (le *LocalEmbedder) Dims() int { return le.dims }

// Embed implements Embedder. It never errors on normal input: empty or
// whitespace-only text yields a valid all-zero vector (documented — Cosine
// treats a zero-magnitude vector as "never similar", returning -1, so an empty
// query can never produce a false cache hit). The context is accepted for
// interface conformance; this embedder does no I/O and does not consult it.
func (le *LocalEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, le.dims)

	norm := normalizeForEmbedding(text)
	if norm == "" {
		return vec, nil // valid zero/base vector for empty input
	}

	runes := []rune(norm)
	n := le.ngram
	if len(runes) < n {
		// Text shorter than one window: hash the whole thing as a single gram
		// so very short prompts still get a stable, non-zero vector.
		n = len(runes)
	}

	for i := 0; i+n <= len(runes); i++ {
		gram := string(runes[i : i+n])
		bucket, sign := hashGram(gram, le.dims)
		vec[bucket] += sign
	}

	l2Normalize(vec)
	return vec, nil
}

// normalizeForEmbedding lowercases and collapses runs of Unicode whitespace to a
// single space, trimming the ends. This makes the embedding invariant to
// case and incidental whitespace so "Hello  World" and "hello world" map close.
func normalizeForEmbedding(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// hashGram maps a character n-gram to (bucket, sign) via two independent FNV-1a
// hashes. The bucket hash chooses the dimension; the sign hash chooses +1/-1 so
// that unrelated n-grams colliding in the same bucket tend to cancel instead of
// systematically inflating it (the standard signed feature-hashing trick).
func hashGram(gram string, dims int) (bucket int, sign float32) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(gram))
	bucket = int(h.Sum32() % uint32(dims))

	s := fnv.New32a()
	_, _ = s.Write([]byte{0x01}) // salt so the sign hash is independent of the bucket hash
	_, _ = s.Write([]byte(gram))
	if s.Sum32()&1 == 0 {
		return bucket, 1
	}
	return bucket, -1
}

// l2Normalize scales vec in place to unit L2 length. A zero vector is left
// untouched (its magnitude is undefined; Cosine reports -1 for it).
func l2Normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	inv := 1.0 / math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) * inv)
	}
}
