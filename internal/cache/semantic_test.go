package cache

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"mismatched len", []float32{1, 0}, []float32{1}, -1},
		{"empty", []float32{}, []float32{}, -1},
		{"zero magnitude", []float32{0, 0}, []float32{1, 1}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Cosine(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cosine=%v want %v", got, tc.want)
			}
		})
	}
}

func TestFloat32BlobRoundTrip(t *testing.T) {
	v := []float32{0.5, -1.25, 3.14159, 0, 42}
	blob := Float32ToBlob(v)
	if len(blob) != len(v)*4 {
		t.Fatalf("blob len=%d want %d", len(blob), len(v)*4)
	}
	back, err := BlobToFloat32(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back) != len(v) {
		t.Fatalf("len mismatch: %d vs %d", len(back), len(v))
	}
	for i := range v {
		if back[i] != v[i] {
			t.Fatalf("index %d: %v != %v", i, back[i], v[i])
		}
	}
	if _, err := BlobToFloat32([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for non-multiple-of-4 blob")
	}
}

// SemanticIndex.Nearest is real ranking: closest above threshold wins, nothing
// below threshold is returned.
func TestSemanticIndex_Nearest(t *testing.T) {
	si := NewSemanticIndex(0.9)
	candidates := []*Entry{
		{Key: "far", Embedding: []float32{0, 1}},
		{Key: "close", Embedding: []float32{0.99, 0.14}}, // ~cos 0.99 with query
		{Key: "mid", Embedding: []float32{0.7, 0.7}},
		{Key: "no-embedding"},
	}
	query := []float32{1, 0}

	best, ok := si.Nearest(query, candidates)
	if !ok {
		t.Fatal("expected a nearest hit above threshold")
	}
	if best.Entry.Key != "close" {
		t.Fatalf("nearest=%s want close", best.Entry.Key)
	}
	if best.Score < 0.9 {
		t.Fatalf("score %v below threshold", best.Score)
	}
}

func TestSemanticIndex_BelowThresholdMisses(t *testing.T) {
	si := NewSemanticIndex(0.99)
	candidates := []*Entry{{Key: "mid", Embedding: []float32{0.7, 0.7}}}
	if _, ok := si.Nearest([]float32{1, 0}, candidates); ok {
		t.Fatal("below-threshold candidate must miss")
	}
}

// The Embedder is an honest seam: Query fails explicitly rather than fabricating
// a vector. This documents (and tests) the not-implemented boundary.
func TestSemanticIndex_QueryEmbedderSeam(t *testing.T) {
	si := NewSemanticIndex(0.8)
	_, _, err := si.Query(context.Background(), "some text", nil)
	if !errors.Is(err, ErrNoEmbedder) {
		t.Fatalf("expected ErrNoEmbedder from the seam, got %v", err)
	}
}
