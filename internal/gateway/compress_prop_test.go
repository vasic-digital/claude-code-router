package gateway

import (
	"math/rand"
	"strings"
	"testing"
)

// ---------- small deterministic generator (no external dependency) ----------

var encodingTokens = []string{"br", "gzip", "deflate", "identity", "sdch", "*"}

func randAcceptEncoding(r *rand.Rand) string {
	n := r.Intn(4) // 0..3 parts
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		tok := encodingTokens[r.Intn(len(encodingTokens))]
		if r.Intn(2) == 0 {
			// attach a q-value, occasionally a degenerate one.
			qs := []string{"0", "0.1", "0.5", "0.9", "1", "1.0", "", "abc"}
			tok += ";q=" + qs[r.Intn(len(qs))]
		}
		parts[i] = tok
	}
	sep := ", "
	if r.Intn(2) == 0 {
		sep = ","
	}
	return strings.Join(parts, sep)
}

// randomizeCase flips the case of each letter independently, using the same
// rand stream so the transformation itself is deterministic for a given
// seed, while still exercising many different case patterns across
// iterations.
func randomizeCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			if r.Intn(2) == 0 {
				b[i] = c - 'a' + 'A'
			}
		} else if c >= 'A' && c <= 'Z' {
			if r.Intn(2) == 0 {
				b[i] = c - 'A' + 'a'
			}
		}
	}
	return string(b)
}

// Property: negotiate is a pure function of its input (same input -> same
// output, called repeatedly) and is case-insensitive with respect to the
// token spelling (only the token letters vary; q-value digits and
// punctuation are untouched by randomizeCase).
func TestPropertyNegotiateDeterministicAndCaseInsensitive(t *testing.T) {
	r := rand.New(rand.NewSource(123))
	for i := 0; i < propIterations; i++ {
		header := randAcceptEncoding(r)

		// Determinism: calling twice on the identical string yields the
		// identical result.
		first := negotiate(header)
		second := negotiate(header)
		if first != second {
			t.Fatalf("iteration %d: negotiate(%q) not deterministic: %q then %q", i, header, first, second)
		}

		// Case-insensitivity: re-casing only the letters must not change the
		// outcome.
		recased := randomizeCase(r, header)
		got := negotiate(recased)
		if got != first {
			t.Fatalf("iteration %d: negotiate(%q) = %q but negotiate(%q) = %q (re-casing changed the result)",
				i, header, first, recased, got)
		}
	}
}

const propIterations = 500
