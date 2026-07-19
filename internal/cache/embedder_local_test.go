package cache

import (
	"bytes"
	"context"
	"math"
	"testing"
)

// The LocalEmbedder is a REAL embedder (lexical, not a learned model): identical
// text must yield the identical vector, byte-for-byte after the on-disk codec.
func TestLocalEmbedder_Deterministic(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()
	const text = "refactor the cache lookup to include the resolved model"

	a, err := le.Embed(ctx, text)
	if err != nil {
		t.Fatalf("embed a: %v", err)
	}
	b, err := le.Embed(ctx, text)
	if err != nil {
		t.Fatalf("embed b: %v", err)
	}
	if !bytes.Equal(Float32ToBlob(a), Float32ToBlob(b)) {
		t.Fatal("identical text produced different vectors (non-deterministic)")
	}
}

func TestLocalEmbedder_DimensionAndDefault(t *testing.T) {
	le := NewLocalEmbedder(128)
	if le.Dims() != 128 {
		t.Fatalf("Dims()=%d want 128", le.Dims())
	}
	v, err := le.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v) != 128 {
		t.Fatalf("len(vec)=%d want 128", len(v))
	}

	// Non-positive dims falls back to the documented default.
	def := NewLocalEmbedder(0)
	if def.Dims() != DefaultEmbedderDims {
		t.Fatalf("default Dims()=%d want %d", def.Dims(), DefaultEmbedderDims)
	}
}

func TestLocalEmbedder_L2Normalized(t *testing.T) {
	le := NewLocalEmbedder(256)
	v, err := le.Embed(context.Background(), "the norm of this vector should be one")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 1e-6 {
		t.Fatalf("L2 norm=%v want ~1.0", norm)
	}
}

func TestLocalEmbedder_EmptyIsZeroVector(t *testing.T) {
	le := NewLocalEmbedder(64)
	for _, text := range []string{"", "   ", "\t\n "} {
		v, err := le.Embed(context.Background(), text)
		if err != nil {
			t.Fatalf("embed %q: unexpected error %v", text, err)
		}
		if len(v) != 64 {
			t.Fatalf("len(vec)=%d want 64", len(v))
		}
		for i, x := range v {
			if x != 0 {
				t.Fatalf("empty text %q: vec[%d]=%v want 0", text, i, x)
			}
		}
		// A zero vector must never masquerade as similar to anything.
		if Cosine(v, v) != -1 {
			t.Fatalf("zero vector should give Cosine -1, got %v", Cosine(v, v))
		}
	}
}

// Similarity ordering: identical ~1.0, one-word edit high, unrelated low, and
// crucially sim(near-dup) > sim(unrelated).
func TestLocalEmbedder_SimilarityOrdering(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()

	base := "please summarize the quarterly earnings report for the finance team"
	dup := "please summarize the quarterly earnings report for the finance group" // one word changed
	unrelated := "compile the rust binary and run the integration test suite on ci"

	vBase, _ := le.Embed(ctx, base)
	vDup, _ := le.Embed(ctx, dup)
	vUnrel, _ := le.Embed(ctx, unrelated)

	simSelf := Cosine(vBase, vBase)
	simDup := Cosine(vBase, vDup)
	simUnrel := Cosine(vBase, vUnrel)

	if math.Abs(simSelf-1.0) > 1e-6 {
		t.Fatalf("identical cosine=%v want ~1.0", simSelf)
	}
	if simDup < 0.8 {
		t.Fatalf("near-duplicate cosine=%v want high (>=0.8)", simDup)
	}
	if !(simDup > simUnrel) {
		t.Fatalf("expected sim(dup)=%v > sim(unrelated)=%v", simDup, simUnrel)
	}
	if simUnrel > 0.5 {
		t.Fatalf("unrelated cosine=%v unexpectedly high", simUnrel)
	}
}

// End-to-end: a SemanticIndex driven by the LocalEmbedder resolves a
// near-duplicate query to the right stored Entry above threshold, and an
// unrelated query misses.
func TestLocalEmbedder_SemanticIndexEndToEnd(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()

	// Build candidate entries with embeddings, as the store would hold them.
	texts := map[string]string{
		"earnings": "please summarize the quarterly earnings report for the finance team",
		"weather":  "what is the weather forecast for san francisco this weekend",
		"build":    "compile the rust binary and run the integration test suite on ci",
	}
	var candidates []*Entry
	for key, txt := range texts {
		vec, err := le.Embed(ctx, txt)
		if err != nil {
			t.Fatalf("embed candidate %s: %v", key, err)
		}
		candidates = append(candidates, &Entry{Key: key, Embedding: vec})
	}

	si := &SemanticIndex{Embedder: le, Threshold: 0.8}

	// Near-duplicate of the "earnings" entry (one word changed) must hit it.
	nearDup := "please summarize the quarterly earnings report for the finance group"
	best, ok, err := si.Query(ctx, nearDup, candidates)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !ok {
		t.Fatal("expected a semantic hit for the near-duplicate query")
	}
	if best.Entry.Key != "earnings" {
		t.Fatalf("hit=%s want earnings", best.Entry.Key)
	}
	if best.Score < 0.8 {
		t.Fatalf("hit score=%v below threshold", best.Score)
	}

	// Wholly unrelated query must miss (no candidate above threshold).
	if _, ok, err := si.Query(ctx, "recipe for a three layer chocolate birthday cake", candidates); err != nil {
		t.Fatalf("query unrelated: %v", err)
	} else if ok {
		t.Fatal("unrelated query must miss, but got a hit")
	}
}
