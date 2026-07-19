package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// A realistic-shaped fake secret. Long enough (>=24 contiguous chars) to
// trip the generic blob pattern even in isolation, and distinctive enough
// that any survival in output is unambiguous.
const fakeAPIKey = "sk-abcdefghijklmnopqrstuvwxyz0123456789"

// newBufferLogger builds a JSON-format logger at debug level (so every test
// case's log call is actually emitted) writing into buf, for tests to
// inspect the raw output.
func newBufferLogger(buf *bytes.Buffer) *slog.Logger {
	return NewWithOptions(buf, slog.LevelDebug, FormatJSON)
}

// decodeLine parses a single JSON log line into a generic map for field
// assertions.
func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

// ---------- isSensitiveKey ----------

func TestIsSensitiveKey(t *testing.T) {
	cases := map[string]bool{
		// The exact spec'd list, various casings/separators.
		"api_key":       true,
		"apikey":        true,
		"APIKey":        true,
		"API_KEY":       true,
		"Authorization": true,
		"authorization": true,
		"token":         true,
		"Token":         true,
		"password":      true,
		"Password":      true,
		"secret":        true,
		"Secret":        true,
		"bearer":        true,
		"Bearer":        true,
		"x-api-key":     true,
		"X-Api-Key":     true,
		"X-API-KEY":     true,

		// Compound keys that should still match on a whole word.
		"access_token":     true,
		"auth_token":       true,
		"refresh_token":    true,
		"client_secret":    true,
		"api_key_id_only":  true, // contains the whole word-pair api,key
		"upstream_api_key": true,

		// Must NOT match: plural/near-miss words that merely contain a
		// sensitive word as a substring.
		"input_tokens":  false,
		"output_tokens": false,
		"max_tokens":    false,
		"tokenizer":     false,
		"secretary":     false,
		"bearerish":     false,
		"password_hint": false, // "hint" is fine to log; key isn't "password" alone... but this DOES contain word "password" -> should redact
	}
	// password_hint contains the exact word "password" as a whole
	// underscore-delimited word, so per the documented word-match rule it
	// SHOULD be treated as sensitive (better to over-redact a hint field
	// than risk a real password slipping through a similarly-shaped key).
	cases["password_hint"] = true

	for key, want := range cases {
		if got := isSensitiveKey(key); got != want {
			t.Errorf("isSensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// ---------- redactString ----------

func TestRedactStringValuePatterns(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		mustNotHave []string
		mustHave    string
	}{
		{
			name:        "sk- prefixed key",
			in:          "using key " + fakeAPIKey + " for this call",
			mustNotHave: []string{fakeAPIKey},
			mustHave:    RedactedMarker,
		},
		{
			name:        "github fine-grained PAT",
			in:          "token github_pat_11ABCDEFG0123456789abcdefghijklmnopqrstuvwxyz",
			mustNotHave: []string{"11ABCDEFG0123456789abcdefghijklmnopqrstuvwxyz"},
			mustHave:    RedactedMarker,
		},
		{
			name:        "Bearer token in free text",
			in:          "Authorization: Bearer abcdefgh12345678ijklmnop",
			mustNotHave: []string{"abcdefgh12345678ijklmnop"},
			mustHave:    "Bearer " + RedactedMarker,
		},
		{
			name:        "generic long blob with no recognisable prefix",
			in:          "leaked value=ZmFrZVNlY3JldFZhbHVlVGhhdElzTG9uZw==",
			mustNotHave: []string{"ZmFrZVNlY3JldFZhbHVlVGhhdElzTG9uZw=="},
			mustHave:    RedactedMarker,
		},
		{
			name:        "short benign string is untouched",
			in:          "hello world",
			mustNotHave: nil,
			mustHave:    "hello world",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactString(tc.in)
			for _, forbidden := range tc.mustNotHave {
				if strings.Contains(got, forbidden) {
					t.Errorf("redactString(%q) = %q, still contains secret %q", tc.in, got, forbidden)
				}
			}
			if !strings.Contains(got, tc.mustHave) {
				t.Errorf("redactString(%q) = %q, want it to contain %q", tc.in, got, tc.mustHave)
			}
		})
	}
}

// A UUID (hyphenated, so no contiguous run reaches the 24-char floor) must
// survive untouched — request/trace IDs are exactly the kind of "long
// identifier that is not a secret" this package must not clobber, or the
// package becomes unpleasant enough to use that people disable it.
func TestRedactStringDoesNotTouchHyphenatedUUIDs(t *testing.T) {
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	got := redactString("request_id=" + uuid)
	if !strings.Contains(got, uuid) {
		t.Errorf("redactString clobbered a hyphenated UUID: got %q, want it to still contain %q", got, uuid)
	}
}

// ---------- Handler-level: the tests the task explicitly asks for ----------

// A key never survives when passed as a plain attribute.
func TestHandlerRedactsSensitiveAttr(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("upstream call", "api_key", fakeAPIKey, "provider", "openrouter")

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived in output: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	if line["api_key"] != RedactedMarker {
		t.Errorf("api_key = %v, want %q", line["api_key"], RedactedMarker)
	}
	if line["provider"] != "openrouter" {
		t.Errorf("unrelated attr provider = %v, want it untouched (\"openrouter\")", line["provider"])
	}
}

// A key never survives when it is embedded inside a formatted message
// string rather than passed as a structured attribute.
func TestHandlerRedactsKeyInsideMessageString(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("upstream request failed using key " + fakeAPIKey)

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived inside message string: %s", out)
	}
	if !strings.Contains(out, RedactedMarker) {
		t.Errorf("expected the redaction marker in output, got: %s", out)
	}
}

// A key never survives when nested inside a slog.Group, at any depth.
func TestHandlerRedactsKeyInsideNestedGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("request",
		slog.Group("headers",
			slog.String("Authorization", "Bearer "+fakeAPIKey),
			slog.Group("nested",
				slog.String("x-api-key", fakeAPIKey),
			),
		),
	)

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived inside a nested group: %s", out)
	}

	line := decodeLine(t, buf.Bytes())
	headers, ok := line["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers group missing or wrong shape: %v", line)
	}
	if headers["Authorization"] != RedactedMarker {
		t.Errorf("headers.Authorization = %v, want %q", headers["Authorization"], RedactedMarker)
	}
	nested, ok := headers["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested group missing or wrong shape: %v", headers)
	}
	if nested["x-api-key"] != RedactedMarker {
		t.Errorf("headers.nested.x-api-key = %v, want %q", nested["x-api-key"], RedactedMarker)
	}
}

// Attrs attached via Logger.With must be scrubbed too — With pre-renders
// into the handler chain, so a redaction implementation that only hooks
// Handle (and not WithAttrs) would miss this entirely.
func TestHandlerRedactsAttrsAttachedViaWith(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf).With("token", fakeAPIKey)

	logger.Info("did a thing")

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived through Logger.With: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	if line["token"] != RedactedMarker {
		t.Errorf("token = %v, want %q", line["token"], RedactedMarker)
	}
}

// Logger.WithGroup must still redact everything logged under it — grouping
// changes WHERE an attribute renders, not whether it is scrubbed.
func TestHandlerRedactsAttrsUnderWithGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf).WithGroup("auth")

	logger.Info("login", "password", fakeAPIKey)

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived through Logger.WithGroup: %s", out)
	}
	line := decodeLine(t, buf.Bytes())
	group, ok := line["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth group missing or wrong shape: %v", line)
	}
	if group["password"] != RedactedMarker {
		t.Errorf("auth.password = %v, want %q", group["password"], RedactedMarker)
	}
}

// A value that merely LOOKS secret-shaped, stored under a completely
// innocuous key name, must still be scrubbed — this is the "VALUE matching
// common key shapes" half of the spec, independent of the "KEY looks like a
// secret" half.
func TestHandlerRedactsSecretShapedValueUnderInnocuousKey(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("note", "debug_note", "leaked "+fakeAPIKey)

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived under an innocuous key name: %s", out)
	}
}

// Text format must redact identically to JSON — the redaction handler sits
// BELOW the format choice, so this is really a test that the wrapping order
// in NewWithOptions is correct.
func TestHandlerRedactsUnderTextFormatToo(t *testing.T) {
	var buf bytes.Buffer
	logger := NewWithOptions(&buf, slog.LevelDebug, FormatText)

	logger.Info("upstream call", "api_key", fakeAPIKey)

	out := buf.String()
	if strings.Contains(out, fakeAPIKey) {
		t.Fatalf("raw key survived under text format: %s", out)
	}
	if !strings.Contains(out, RedactedMarker) {
		t.Errorf("expected redaction marker in text output, got: %s", out)
	}
}

// A routine numeric usage field must NOT be collateral damage — proving the
// redaction is precise, not a sledgehammer that makes the logger useless.
func TestHandlerDoesNotRedactRoutineTokenCountFields(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufferLogger(&buf)

	logger.Info("usage", "input_tokens", 128, "output_tokens", 42, "max_tokens", 4096)

	line := decodeLine(t, buf.Bytes())
	if line["input_tokens"] != float64(128) {
		t.Errorf("input_tokens = %v, want 128 (untouched)", line["input_tokens"])
	}
	if line["output_tokens"] != float64(42) {
		t.Errorf("output_tokens = %v, want 42 (untouched)", line["output_tokens"])
	}
	if line["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096 (untouched)", line["max_tokens"])
	}
}
