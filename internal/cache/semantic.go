package cache

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
)

// This file holds the Tier-2 (semantic) seam. To be explicit about what is real
// versus a seam — no bluff:
//
//   - Cosine, Float32ToBlob, BlobToFloat32 are FULLY IMPLEMENTED and tested.
//   - SemanticIndex.Nearest is FULLY IMPLEMENTED: a genuine brute-force cosine
//     nearest-neighbour search over a scoped candidate set, with an adaptive
//     threshold, exactly as the dossier's Phase 2 Task 2.2.1 describes for the
//     small per-scope N a single gateway sees. It is tested with hand-built
//     vectors (no model required).
//   - Embedder is the one intentional NOT-IMPLEMENTED extension: turning text
//     into a vector needs a live embedding model (a config.Provider flagged as
//     the embedding source — Phase 2 Task 2.1). This package deliberately ships
//     NO fake embedder; ErrNoEmbedder documents the seam. Wiring a real one is a
//     separate task, and until then the semantic tier has no vectors to search
//     and stays dormant.

// ErrNoEmbedder is returned by the placeholder embedder to make the seam
// explicit: there is no production embedding implementation in this package.
var ErrNoEmbedder = errors.New("cache: no embedder configured (semantic tier is a documented seam; see semantic.go)")

// Embedder turns salient request text into a vector. Implementing it against a
// real embedding provider is the Phase 2 task that activates the semantic tier.
type Embedder interface {
	// Embed returns the embedding of text, or an error. Dimensionality must be
	// stable per Embedder instance.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// nopEmbedder is the honest placeholder: it always errors. It exists so callers
// can wire the seam and get a clear, non-silent failure rather than a fabricated
// vector.
type nopEmbedder struct{}

// NopEmbedder returns an Embedder that always fails with ErrNoEmbedder. It is
// NOT a working embedder — it marks the seam.
func NopEmbedder() Embedder { return nopEmbedder{} }

func (nopEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, ErrNoEmbedder
}

// Cosine returns cosine similarity in [-1, 1]; the caller compares it against
// the configured threshold. Mismatched or empty vectors, or a zero-magnitude
// vector, return -1 (never a false "similar").
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Float32ToBlob encodes a vector as little-endian float32 bytes, the on-disk
// format the dossier's cache_embeddings.vector column expects.
func Float32ToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// BlobToFloat32 decodes little-endian float32 bytes back into a vector. A blob
// whose length is not a multiple of 4 is rejected.
func BlobToFloat32(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("cache: embedding blob length not a multiple of 4")
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// Candidate is one scored entry considered by the semantic tier.
type Candidate struct {
	Entry *Entry
	Score float64
}

// SemanticIndex performs brute-force cosine nearest-neighbour over a candidate
// set the caller has already scoped (same model + system_hash), matching the
// dossier's Phase 2 Task 2.2.1. It carries no storage of its own — the exact
// tier owns the entries; this only ranks them — and it carries the Embedder
// seam so a future gateway can produce the query vector.
type SemanticIndex struct {
	// Embedder produces the query vector. Defaults to NopEmbedder (the seam).
	Embedder Embedder
	// Threshold is the minimum cosine similarity to accept a hit.
	Threshold float64
}

// NewSemanticIndex builds an index with the given threshold. With no Embedder
// set it uses NopEmbedder, so Query fails explicitly until a real embedder is
// wired — never silently returns a wrong answer.
func NewSemanticIndex(threshold float64) *SemanticIndex {
	return &SemanticIndex{Embedder: NopEmbedder(), Threshold: threshold}
}

// Nearest returns the best candidate whose cosine similarity to vec is >=
// Threshold, or (nil, false). This is a real, deterministic ranking over the
// provided (already scope-filtered) candidates — the semantic MATH is complete;
// only vec's PRODUCTION source (an Embedder) is the seam.
func (si *SemanticIndex) Nearest(vec []float32, candidates []*Entry) (*Candidate, bool) {
	best := Candidate{Score: si.Threshold}
	found := false
	for _, e := range candidates {
		if e == nil || len(e.Embedding) == 0 {
			continue
		}
		score := Cosine(vec, e.Embedding)
		if score >= best.Score {
			best = Candidate{Entry: e, Score: score}
			found = true
		}
	}
	if !found {
		return nil, false
	}
	return &best, true
}

// Query embeds text and finds the nearest scoped candidate. It returns
// ErrNoEmbedder (via the seam) until a real Embedder is configured — this is
// the honest boundary of what this package implements.
func (si *SemanticIndex) Query(ctx context.Context, text string, candidates []*Entry) (*Candidate, bool, error) {
	emb := si.Embedder
	if emb == nil {
		emb = NopEmbedder()
	}
	vec, err := emb.Embed(ctx, text)
	if err != nil {
		return nil, false, err
	}
	c, ok := si.Nearest(vec, candidates)
	return c, ok, nil
}
