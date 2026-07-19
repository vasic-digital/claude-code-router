package config

import (
	"math/rand"
	"strings"
	"testing"
)

// ---------- small deterministic generator (no external dependency) ----------

// providerChars/modelChars deliberately exclude the comma: SplitRoute finds
// the FIRST comma to separate provider from model, so a provider containing
// one would not round-trip (that is the documented, correct behaviour, not a
// bug — see SplitRoute's doc comment on why only the first comma counts).
// Leading/trailing whitespace is likewise excluded from the generated pieces
// themselves because SplitRoute trims both sides; a generated piece that
// itself started or ended with whitespace would not compare equal to the
// trimmed result, which would be testing trimming, not round-tripping.
const routeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./"

func randRouteComponent(r *rand.Rand, allowComma bool) string {
	chars := routeChars
	if allowComma {
		chars += ",,,," // weight commas in, since the model half is allowed to embed them
	}
	n := 1 + r.Intn(20)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(chars[r.Intn(len(chars))])
	}
	return b.String()
}

// Property: for any provider WITHOUT a comma and any model (which may itself
// contain commas), SplitRoute(provider+","+model) round-trips exactly to
// (provider, model).
func TestPropertySplitRouteRoundTrips(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for i := 0; i < propIterations; i++ {
		provider := randRouteComponent(r, false)
		model := randRouteComponent(r, true)

		route := provider + "," + model
		gotP, gotM, err := SplitRoute(route)
		if err != nil {
			t.Fatalf("iteration %d: SplitRoute(%q) errored: %v", i, route, err)
		}
		if gotP != provider {
			t.Fatalf("iteration %d: SplitRoute(%q) provider = %q, want %q", i, route, gotP, provider)
		}
		if gotM != model {
			t.Fatalf("iteration %d: SplitRoute(%q) model = %q, want %q", i, route, gotM, model)
		}
	}
}

const propIterations = 500
