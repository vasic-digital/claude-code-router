package gateway

import "testing"

// FuzzNegotiate drives arbitrary Accept-Encoding header values through
// negotiate. Invariants: it must never panic, and the result must always be
// one of "", "br", "gzip" — never any other string, and never a value that
// leaks part of the input back out (which would indicate the switch fell
// through unexpectedly).
func FuzzNegotiate(f *testing.F) {
	seeds := []string{
		"",
		"identity",
		"gzip",
		"br",
		"gzip, br",
		"br;q=0.1, gzip;q=0.9",
		"gzip, deflate",
		"deflate",
		"*",
		"br;q=0, gzip",
		"gzip;q=0",
		"GZIP",
		"gzip;q=abc",                      // malformed q value
		"br;q=",                           // empty q value
		";;;",                             // pure separators
		",,,",                             // empty tokens
		"br;q=1;q=2",                      // duplicate params
		"br ; q = 0.5",                    // stray whitespace around params
		"\t\ngzip\r\n",                    // control whitespace
		"br;level=high",                   // non-q parameter
		"BR, GZIP, br,gzip",               // repeated + mixed case
		"gzip;q=999999999999999999999999", // absurd numeric q
		"gzip;q=-1",                       // negative q
		"gzip;q=nan",
		"gzip, deflate, br, identity, sdch, gzip, br, gzip, br, gzip, br, gzip, br, gzip, br", // long/repeated
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, header string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("negotiate panicked on input %q: %v", header, r)
			}
		}()

		got := negotiate(header)
		switch got {
		case "", "br", "gzip":
			// ok
		default:
			t.Fatalf("negotiate(%q) = %q, want one of \"\", \"br\", \"gzip\"", header, got)
		}
	})
}
