package logging

// Exhaustive CONFIRMATION/VALIDATION/VERIFICATION coverage for the redaction
// layer, focused on the one guarantee that actually matters here: a secret
// never survives to the wire, in any shape, on any path, under concurrency,
// and on arbitrary (fuzzed) input — while an ordinary word is never
// over-redacted into uselessness.
//
// This file deliberately does NOT restate what redact_test.go /
// logging_test.go already prove (the isSensitiveKey table, the basic
// per-pattern redactString cases, the plain-attr / message / nested-group /
// With / WithGroup handler cases, level/format env parsing). It adds the
// gaps: the FIXED-marker property (whole secret gone, no fragment survives),
// the no-false-positive property, the LogValuer / sensitive-key-wins /
// non-string-value pre-render leak paths, all-levels redaction, concurrency
// (torn-line + race), and fuzzers.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// ---------- FIXED-marker property: the whole secret, never a prefix ----------

// A recognised secret shape must be replaced ENTIRELY by the fixed marker —
// no leading/trailing fragment of the real secret may survive. redact.go's
// RedactedMarker doc comment is explicit that even 8 characters of a key is a
// leak, so this asserts exact-equality (not just "contains the marker"), which
// is what pins down "fixed marker, never a prefix".
func TestRedactStringReplacesWholeSecretWithFixedMarker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"sk- key alone collapses to exactly the marker", "sk-abcdefghij0123456789ABCDEF", RedactedMarker},
		{"github PAT alone collapses to exactly the marker", "github_pat_11ABCDEFG0123456789abcdefghij", RedactedMarker},
		{"prefixless 40-char blob collapses to exactly the marker", "0123456789abcdef0123456789abcdef01234567", RedactedMarker},
		{"Bearer keeps only the non-secret literal scheme", "Bearer abcdefgh12345678ijklmnop", "Bearer " + RedactedMarker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactString(tc.in); got != tc.want {
				t.Errorf("redactString(%q) = %q, want exactly %q (whole secret must be replaced, no fragment left)", tc.in, got, tc.want)
			}
		})
	}
}

// No prefix or suffix of a realistic secret may survive: for a family of
// secret-shaped inputs, assert that NO contiguous 8+ character slice of the
// original secret body remains anywhere in the output. This is the strong
// form of "never a prefix" — it would catch a redactor that kept the first or
// last N characters "for debuggability".
func TestRedactStringLeavesNoRecognisableFragment(t *testing.T) {
	secrets := []string{
		"sk-ant-api03-DEADBEEFdeadbeef0123456789ABCDEF",
		"github_pat_11ABCDEFG0deadbeef0123456789abcdef",
		"0123456789abcdef0123456789abcdef01234567",
		"AKIAIOSFODNN7EXAMPLEwJalrXUtnFEMIK7MDENG",
	}
	for _, secret := range secrets {
		out := redactString("prefix " + secret + " suffix")
		// Slide an 8-char window across the secret; none of those windows may
		// appear verbatim in the scrubbed output.
		for i := 0; i+8 <= len(secret); i++ {
			frag := secret[i : i+8]
			if strings.Contains(out, frag) {
				t.Errorf("redaction of %q left recognisable 8-char fragment %q in output %q", secret, frag, out)
				break
			}
		}
	}
}

// Multiple distinct secrets in one string must ALL be redacted — a redactor
// that stops after the first match would leak the rest.
func TestRedactStringRedactsEverySecretInOneString(t *testing.T) {
	in := "key1=sk-aaaaaaaaaa1111111111 key2=sk-bbbbbbbbbb2222222222 auth=Bearer cccccccccc3333333333"
	out := redactString(in)
	for _, leaked := range []string{"sk-aaaaaaaaaa1111111111", "sk-bbbbbbbbbb2222222222", "cccccccccc3333333333"} {
		if strings.Contains(out, leaked) {
			t.Errorf("redactString left secret %q in %q", leaked, out)
		}
	}
}

// ---------- No-false-positive property: ordinary text is untouched ----------

// Redaction must be precise, not a sledgehammer: ordinary log text — words,
// numbers, small ids, hyphenated UUIDs, key=value fragments — must pass
// through byte-for-byte unchanged. A package that mangles routine output gets
// disabled, which is a worse security outcome than a narrow rule (see
// redact.go's own reasoning).
func TestRedactStringDoesNotOverRedactOrdinaryText(t *testing.T) {
	cases := []string{
		"hello world",
		"the quick brown fox jumps",
		"error: connection refused",
		"model=gpt-4-turbo",
		"temperature=0.7 top_p=0.9",
		"user_id=42 request done",
		"status=200 ok",
		"path=/v1/chat",
		"duration=1.234s elapsed",
		"abc123def456",
		"550e8400-e29b-41d4-a716-446655440000",
		"retrying attempt 3 of 5",
		"provider=openrouter region=us-east",
	}
	for _, in := range cases {
		if got := redactString(in); got != in {
			t.Errorf("over-redacted ordinary text: redactString(%q) = %q (want unchanged)", in, got)
		}
	}
}

// ---------- Sensitive KEY wins, regardless of value shape ----------

// A sensitive KEY must scrub its value even when that value is not a string
// (int, bool, float) — the key alone is reason enough, so the value's Kind
// must not create a bypass.
func TestHandlerRedactsNonStringValueUnderSensitiveKey(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("m", "secret", 1234567, "authorization", true, "password", 3.14)

	line := decodeLine(t, buf.Bytes())
	for _, k := range []string{"secret", "authorization", "password"} {
		if line[k] != RedactedMarker {
			t.Errorf("%s = %v (%T), want %q — a sensitive key must redact any value kind", k, line[k], line[k], RedactedMarker)
		}
	}
}

// A group whose KEY is itself sensitive must be collapsed WHOLE to the marker
// (never recursed into), so a sibling with an innocuous-looking name inside it
// can't slip through. This exercises redactAttr's "sensitive key wins even
// over a Group value" branch, which the existing nested-group test (whose
// group key is innocuous) does not reach.
func TestHandlerCollapsesGroupUnderSensitiveKey(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("m",
		slog.Group("secret",
			slog.String("inner_note", "sibling-would-otherwise-survive"),
			slog.Int("count", 5),
		),
	)

	out := buf.String()
	if strings.Contains(out, "sibling-would-otherwise-survive") {
		t.Fatalf("a sibling inside a sensitively-named group survived: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	if line["secret"] != RedactedMarker {
		t.Errorf("secret group = %v, want the whole group collapsed to %q", line["secret"], RedactedMarker)
	}
}

// ---------- LogValuer pre-render leak paths ----------

// fakeLogValuer defers producing its real value until Resolve() is called —
// the shape redactAttr must resolve BEFORE inspecting, or a secret wrapped in
// one slips past unredacted and is re-resolved (unredacted) downstream.
type fakeLogValuer struct{ v slog.Value }

func (f fakeLogValuer) LogValue() slog.Value { return f.v }

// A LogValuer resolving to a secret-SHAPED string under an INNOCUOUS key must
// still be value-scrubbed — proves redactAttr resolves before the value-shape
// check.
func TestHandlerRedactsSecretShapedLogValuerUnderInnocuousKey(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("m", "detail", fakeLogValuer{v: slog.StringValue("leaked " + fakeAPIKey)})

	if out := buf.String(); strings.Contains(out, fakeAPIKey) {
		t.Fatalf("secret survived through a LogValuer under an innocuous key: %s", out)
	}
}

// A LogValuer under a SENSITIVE key must be scrubbed to the marker after
// resolution — the key wins even though the raw attribute was a LogValuer.
func TestHandlerRedactsLogValuerUnderSensitiveKey(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("m", "api_key", fakeLogValuer{v: slog.StringValue(fakeAPIKey)})

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("secret survived through a LogValuer under a sensitive key: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	if line["api_key"] != RedactedMarker {
		t.Errorf("api_key = %v, want %q", line["api_key"], RedactedMarker)
	}
}

// A LogValuer resolving to a whole GROUP that contains a sensitive child must
// be recursed into after resolution, so the nested secret is caught.
func TestHandlerRedactsLogValuerResolvingToGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("m", "payload", fakeLogValuer{v: slog.GroupValue(
		slog.String("token", fakeAPIKey),
		slog.String("ok", "fine"),
	)})

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("secret survived through a LogValuer resolving to a group: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	payload, ok := line["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload group missing or wrong shape: %v", line)
	}
	if payload["token"] != RedactedMarker {
		t.Errorf("payload.token = %v, want %q", payload["token"], RedactedMarker)
	}
	if payload["ok"] != "fine" {
		t.Errorf("payload.ok = %v, want it untouched (\"fine\")", payload["ok"])
	}
}

// ---------- Redaction is level-independent ----------

// Redaction has no opinion on level (redact.go's Enabled just delegates), so a
// secret must be scrubbed identically whether logged at debug, info, warn, or
// error. The existing suite only exercises Info.
func TestHandlerRedactsAtEveryLevel(t *testing.T) {
	emit := map[string]func(l *slog.Logger){
		"debug": func(l *slog.Logger) { l.Debug("m", "api_key", fakeAPIKey) },
		"info":  func(l *slog.Logger) { l.Info("m", "api_key", fakeAPIKey) },
		"warn":  func(l *slog.Logger) { l.Warn("m", "api_key", fakeAPIKey) },
		"error": func(l *slog.Logger) { l.Error("m", "api_key", fakeAPIKey) },
	}
	for name, fn := range emit {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newBufferLogger(&buf) // debug level, so every call emits
			fn(logger)
			out := buf.String()
			if out == "" {
				t.Fatalf("expected a log line at level %s", name)
			}
			if strings.Contains(out, fakeAPIKey) {
				t.Fatalf("secret survived at level %s: %s", name, out)
			}
		})
	}
}

// ---------- Concurrency: race-free and never a torn line ----------

// Concurrent logging through ONE logger must be race-free (run this file with
// -race) and must never interleave a torn line: every emitted line must be a
// complete, valid JSON object, the count must be exact, and the secret must be
// redacted on every one. slog serialises writes behind the handler's mutex, so
// this both proves that guarantee holds through the RedactingHandler wrapper
// and gives the race detector something to chew on.
func TestConcurrentLoggingIsRaceFreeAndNotTorn(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	const goroutines = 50
	const perGoroutine = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				logger.Info("concurrent", "g", id, "i", i, "api_key", fakeAPIKey)
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d — a torn/dropped write would change this count", len(lines), goroutines*perGoroutine)
	}
	for _, ln := range lines {
		if ln == "" {
			t.Fatal("empty log line — indicates a torn concurrent write")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line is not a complete JSON object (torn write?): %v\nline: %q", err, ln)
		}
		if m["api_key"] != RedactedMarker {
			t.Errorf("api_key not redacted under concurrency: %v", m["api_key"])
		}
		if strings.Contains(ln, fakeAPIKey) {
			t.Errorf("secret leaked under concurrency in line: %q", ln)
		}
	}
}

// ---------- Fuzzers ----------

// FuzzRedactString: on ARBITRARY input the redactor must never panic; and when
// a KNOWN, well-formed secret is appended, that secret must never appear
// verbatim in the output no matter what noise precedes it.
func FuzzRedactString(f *testing.F) {
	seeds := []string{
		"", "hello world", fakeAPIKey,
		"Bearer abcdefgh12345678ijklmnop",
		"github_pat_11ABCDEFG0123456789abcdefghij",
		"0123456789abcdef0123456789abcdef01234567",
		"input_tokens=128 output_tokens=42",
		"550e8400-e29b-41d4-a716-446655440000",
		"sk-", "sk", "===", "\x00\x01\x02\n\t",
		strings.Repeat("A", 5000),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	const known = "sk-KNOWNfakeSECRET0123456789abcdEF"
	f.Fuzz(func(t *testing.T, s string) {
		// 1. Must never panic on arbitrary input.
		_ = redactString(s)

		// 2. A known secret, appended after a separator so any noise `s`
		//    cannot break its leading "sk-", must never survive verbatim.
		tainted := s + " " + known
		if got := redactString(tainted); strings.Contains(got, known) {
			t.Fatalf("known secret survived redaction\ninput:  %q\noutput: %q", tainted, got)
		}
	})
}

// FuzzIsSensitiveKey: arbitrary key names must never panic the classifier.
func FuzzIsSensitiveKey(f *testing.F) {
	for _, s := range []string{"", "api_key", "x-api-key", "input_tokens", "\x00", strings.Repeat("_", 1000), "Ünîcødé_key"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		_ = isSensitiveKey(key)
	})
}

// FuzzLoggerNeverLeaks: drive the full handler (message, sensitive-key attr,
// secret-shaped value, and a free-form attr that fuses arbitrary noise onto
// the secret) with fuzzed input; the injected secret must never reach the
// rendered output on ANY of those pre-render paths.
func FuzzLoggerNeverLeaks(f *testing.F) {
	for _, s := range []string{"", "hello", "weird \x00 msg", strings.Repeat("x", 300)} {
		f.Add(s)
	}
	const secret = "sk-FUZZsecretVALUE0123456789abcdEF"
	f.Fuzz(func(t *testing.T, s string) {
		var buf bytes.Buffer
		logger := newBufferLogger(&buf)
		logger.Info(s,
			"api_key", secret,
			"note", "value "+secret,
			slog.String("free", s+secret),
		)
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("secret leaked to output for message %q: %s", s, buf.String())
		}
	})
}
