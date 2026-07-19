package translate

import (
	"strings"
	"testing"
)

// Regression guard for a defect found by fuzzing FuzzStripCacheControl.
//
// StripCacheControl originally did json.Unmarshal into `any`, which turns every
// JSON number into a float64. For a passthrough proxy that had two effects,
// both verified against the pre-fix implementation before the fix landed:
//
//   - "1E700" was rejected outright: "cannot unmarshal number 1E700 into Go
//     value of type float64". A request Claude Code sent in good faith failed.
//   - Far worse because it was silent: 12345678901234567890 came back out as
//     12345678901234567000. The proxy corrupted a value it was only ever meant
//     to forward untouched, and nothing reported an error.
//
// The fix decodes with json.Decoder + UseNumber(), which keeps each literal
// verbatim as a json.Number. These cases fail against the old implementation
// and pass against the current one.
func TestStripCacheControlPreservesNumericLiteralsVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		input string
		// want is the literal that must survive byte-for-byte.
		want string
	}{
		{
			name:  "magnitude overflowing float64 is no longer rejected",
			input: `{"a":1E700}`,
			want:  "1E700",
		},
		{
			name:  "large integer is not silently rounded",
			input: `{"id":12345678901234567890}`,
			want:  "12345678901234567890",
		},
		{
			name:  "high-precision decimal keeps every digit",
			input: `{"x":0.12345678901234567890}`,
			want:  "0.12345678901234567890",
		},
		{
			name:  "negative exponent survives",
			input: `{"tiny":1E-700}`,
			want:  "1E-700",
		},
		{
			name:  "cache_control still stripped while a big int is preserved",
			input: `{"id":12345678901234567890,"cache_control":{"type":"ephemeral"}}`,
			want:  "12345678901234567890",
		},
		{
			name:  "deeply nested big int preserved and nested cache_control stripped",
			input: `{"m":[{"content":[{"n":98765432109876543210,"cache_control":{"type":"ephemeral"}}]}]}`,
			want:  "98765432109876543210",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := StripCacheControl([]byte(tc.input))
			if err != nil {
				t.Fatalf("StripCacheControl(%s) errored: %v", tc.input, err)
			}
			got := string(out)
			if !strings.Contains(got, tc.want) {
				t.Errorf("numeric literal not preserved verbatim\ninput: %s\n  got: %s\n want substring: %s",
					tc.input, got, tc.want)
			}
			if strings.Contains(got, "cache_control") {
				t.Errorf("cache_control survived stripping: %s", got)
			}
		})
	}
}

// Genuinely malformed JSON must still be an error — the UseNumber change must
// not turn the decoder permissive.
func TestStripCacheControlStillRejectsMalformedJSON(t *testing.T) {
	for _, bad := range []string{`{`, `{"a":}`, `not json`, `{"a":01}`} {
		if _, err := StripCacheControl([]byte(bad)); err == nil {
			t.Errorf("StripCacheControl(%q) should have failed", bad)
		}
	}
}
