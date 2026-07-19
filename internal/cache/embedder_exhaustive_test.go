package cache

import (
	"bytes"
	"context"
	"math"
	"testing"
)

// isFinite reports whether every component is a finite float32.
func allFinite(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

func l2(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

// --- Property: determinism + codec round-trip over many inputs --------------
func TestLocalEmbedder_DeterministicProperty(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()
	inputs := []string{
		"a", "ab", "abc", "hello world",
		"please summarize the quarterly earnings report",
		"unicode: café — naïve — 日本語 — emoji 🚀",
		"   leading and trailing   ",
		"MiXeD CaSe ShOuLd NoT MaTtEr",
	}
	for _, in := range inputs {
		v1, err := le.Embed(ctx, in)
		if err != nil {
			t.Fatalf("embed %q: %v", in, err)
		}
		v2, _ := le.Embed(ctx, in)
		if !bytes.Equal(Float32ToBlob(v1), Float32ToBlob(v2)) {
			t.Fatalf("non-deterministic for %q", in)
		}
		// The on-disk codec must round-trip the produced vector exactly.
		back, err := BlobToFloat32(Float32ToBlob(v1))
		if err != nil {
			t.Fatalf("codec decode %q: %v", in, err)
		}
		if !bytes.Equal(Float32ToBlob(v1), Float32ToBlob(back)) {
			t.Fatalf("codec round-trip changed vector for %q", in)
		}
	}
}

// --- Property: non-empty text -> L2 norm ~ 1; empty -> exact zero -----------
func TestLocalEmbedder_NormProperty(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()

	for _, in := range []string{"x", "yz", "a moderately long sentence to embed here", "日本語のテキスト"} {
		v, _ := le.Embed(ctx, in)
		if !allFinite(v) {
			t.Fatalf("non-finite vector for %q", in)
		}
		if n := l2(v); math.Abs(n-1.0) > 1e-4 {
			t.Fatalf("L2 norm=%v want ~1 for %q", n, in)
		}
	}
	for _, empty := range []string{"", " ", "\t\n\r ", " "} {
		v, _ := le.Embed(ctx, empty)
		if n := l2(v); n != 0 {
			t.Fatalf("empty/whitespace %q must yield the zero vector, norm=%v", empty, n)
		}
	}
}

// --- Case- and whitespace-invariance: normalization collapses noise ---------
func TestLocalEmbedder_NormalizationInvariance(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()
	a, _ := le.Embed(ctx, "Hello   World")
	b, _ := le.Embed(ctx, "hello world")
	if !bytes.Equal(Float32ToBlob(a), Float32ToBlob(b)) {
		t.Fatal("case/whitespace normalization must make these identical vectors")
	}
	if sim := Cosine(a, b); math.Abs(sim-1.0) > 1e-6 {
		t.Fatalf("normalized variants cosine=%v want ~1", sim)
	}
}

// --- Dimension is honoured for a spread of widths ---------------------------
func TestLocalEmbedder_DimensionHonoured(t *testing.T) {
	ctx := context.Background()
	for _, dims := range []int{1, 2, 16, 64, 384, 768} {
		le := NewLocalEmbedder(dims)
		if le.Dims() != dims {
			t.Fatalf("Dims()=%d want %d", le.Dims(), dims)
		}
		v, _ := le.Embed(ctx, "some representative query text")
		if len(v) != dims {
			t.Fatalf("len(vec)=%d want %d", len(v), dims)
		}
		if len(Float32ToBlob(v)) != dims*4 {
			t.Fatalf("blob len=%d want %d", len(Float32ToBlob(v)), dims*4)
		}
	}
}

// --- Very short text (below the n-gram window) still yields a stable, unit
//
//	vector — the len(runes)<n fallback path.
func TestLocalEmbedder_ShortTextUnitVector(t *testing.T) {
	le := NewLocalEmbedder(64)
	ctx := context.Background()
	for _, in := range []string{"a", "ab"} { // both shorter than the trigram window
		v, _ := le.Embed(ctx, in)
		if n := l2(v); math.Abs(n-1.0) > 1e-4 {
			t.Fatalf("short text %q norm=%v want ~1", in, n)
		}
		v2, _ := le.Embed(ctx, in)
		if !bytes.Equal(Float32ToBlob(v), Float32ToBlob(v2)) {
			t.Fatalf("short text %q not deterministic", in)
		}
	}
}

// --- Similarity ordering: identical >= near-dup > unrelated -----------------
func TestLocalEmbedder_SimilarityGradient(t *testing.T) {
	le := NewLocalEmbedder(256)
	ctx := context.Background()

	base := "please reindex the search cluster before the nightly batch job runs"
	nearDup := "please reindex the search cluster before the nightly batch job starts" // one word
	related := "the search cluster reindex should run before the batch job"            // shared vocab, reworded
	unrelated := "bake a chocolate cake with three layers and vanilla frosting"

	vBase, _ := le.Embed(ctx, base)
	vDup, _ := le.Embed(ctx, nearDup)
	vRel, _ := le.Embed(ctx, related)
	vUnrel, _ := le.Embed(ctx, unrelated)

	simSelf := Cosine(vBase, vBase)
	simDup := Cosine(vBase, vDup)
	simRel := Cosine(vBase, vRel)
	simUnrel := Cosine(vBase, vUnrel)

	if math.Abs(simSelf-1.0) > 1e-6 {
		t.Fatalf("self cosine=%v want ~1", simSelf)
	}
	if !(simDup > simRel) {
		t.Fatalf("expected sim(near-dup)=%v > sim(related)=%v", simDup, simRel)
	}
	if !(simRel > simUnrel) {
		t.Fatalf("expected sim(related)=%v > sim(unrelated)=%v", simRel, simUnrel)
	}
	if simUnrel > 0.5 {
		t.Fatalf("unrelated cosine=%v unexpectedly high", simUnrel)
	}
}

// --- Fuzz: Embed never panics, always finite, norm is 0 or ~1 ---------------
func FuzzEmbed(f *testing.F) {
	for _, s := range []string{
		"", " ", "a", "abc", "hello world",
		"café 日本語 🚀", "\x00\x01\x02", "\xff\xfe invalid utf8",
		"   many    spaces   ", "UPPER lower MiXeD",
	} {
		f.Add(s)
	}
	le := NewLocalEmbedder(128)
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, text string) {
		v, err := le.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed returned an error for %q: %v (LocalEmbedder must never error)", text, err)
		}
		if len(v) != 128 {
			t.Fatalf("len(vec)=%d want 128 for %q", len(v), text)
		}
		if !allFinite(v) {
			t.Fatalf("non-finite component for %q", text)
		}
		n := l2(v)
		if n != 0 && math.Abs(n-1.0) > 1e-4 {
			t.Fatalf("norm=%v must be 0 (empty) or ~1 (normalized) for %q", n, text)
		}
		// Determinism under fuzzing too.
		v2, _ := le.Embed(ctx, text)
		if !bytes.Equal(Float32ToBlob(v), Float32ToBlob(v2)) {
			t.Fatalf("non-deterministic embed for %q", text)
		}
	})
}
