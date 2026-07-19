package logging

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// RedactedMarker replaces every scrubbed value. It is a FIXED marker, never
// a prefix or partial rendering of the real value: logging even 8 characters
// of a key is a leak (that is enough to fingerprint or narrow a brute-force
// search against a key-management system), so there is no "safe" amount of
// a secret to preserve for debuggability.
const RedactedMarker = "[REDACTED]"

// ---------- Key-shape redaction ----------
//
// isSensitiveKey decides whether an ATTRIBUTE's key name alone is reason
// enough to scrub its entire value, regardless of what that value looks
// like. It must catch api_key, apikey, authorization, token, password,
// secret, bearer, and x-api-key (case-insensitive, per the task spec) while
// NOT catching innocuous compound keys that merely contain one of those
// words as a substring of a longer word — the sharpest example being
// "input_tokens" / "output_tokens" / "max_tokens", which this gateway logs
// constantly for usage accounting and which must NOT be wiped out by a
// naive substring match on "token".
//
// The approach: normalise the key two ways and check both.
//
//  1. Strip ALL separators (-, _, space, ...) and compare the result whole
//     against a set of known secret shapes. This is what catches
//     "x-api-key", "api_key", "apikey", and "API_KEY" — all of which
//     collapse to the same "apikey" string — as well as "Authorization"
//     and "Bearer" collapsing to themselves.
//  2. Split on separators (WITHOUT stripping them) into words and compare
//     each word individually against the same-ish set. This is what
//     catches "access_token" / "auth_token" (word "token") while correctly
//     REJECTING "input_tokens" (word "tokens", plural, not an exact match)
//     and "tokenizer" (a single word, not equal to "token").
//
// Together these two checks are deliberately narrower than a blanket
// substring match, because the failure mode of a false negative here (a
// genuine secret slips through, e.g. a key literally named "TOKEN") is
// caught by the SEPARATE value-shape redaction below (redactString) as a
// second line of defence, whereas the failure mode of a false positive
// (silently wiping out routine numeric usage fields on every log line)
// would make this package actively unpleasant to use and likely get it
// disabled outright — a worse outcome for security than the narrower rule.
var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

	// concatSensitive matches the fully-collapsed (separators stripped) key
	// or a pair of ADJACENT words collapsed together, i.e. whole-key
	// equality PLUS "api"+"key" as neighbouring words. The pair form is
	// what catches "x-api-key" (words x, api, key -> pair "api"+"key"),
	// "upstream_api_key", and "api_key_id_only" — real-world compound key
	// names where "api" and "key" are separate underscore/hyphen-delimited
	// words rather than one fused word, which a single-word check alone
	// would miss entirely.
	//
	// "privatekey" is included even though it is not in the task's spec'd
	// list: a PEM/SSH-style private key is unambiguously as sensitive as an
	// API key, and unlike e.g. "access_key" (AWS access key IDs are meant
	// to be logged — only the paired secret key is sensitive, and that
	// already matches via the word "secret") there is no legitimate
	// non-secret field plausibly named "private_key". Judgement call, not
	// part of the literal spec.
	concatSensitive = map[string]bool{
		"apikey":        true,
		"authorization": true,
		"password":      true,
		"secret":        true,
		"bearer":        true,
		"xapikey":       true,
		"privatekey":    true,
	}

	// wordSensitive matches an individual separator-delimited word within
	// the key, i.e. "access_token" matches on the word "token" without
	// "input_tokens" matching on "tokens".
	wordSensitive = map[string]bool{
		"token":         true,
		"apikey":        true,
		"authorization": true,
		"password":      true,
		"secret":        true,
		"bearer":        true,
	}
)

// isSensitiveKey reports whether key looks like the name of a secret, per
// the rules documented on the vars above: a whole-key match, an individual
// word match, or a pair of adjacent words that together spell a whole-key
// match (e.g. "api" next to "key").
func isSensitiveKey(key string) bool {
	if key == "" {
		return false
	}
	lower := strings.ToLower(key)

	if concatSensitive[nonAlnum.ReplaceAllString(lower, "")] {
		return true
	}

	words := nonAlnum.Split(lower, -1)
	for i, word := range words {
		if word == "" {
			continue
		}
		if wordSensitive[word] {
			return true
		}
		if i+1 < len(words) && concatSensitive[word+words[i+1]] {
			return true
		}
	}
	return false
}

// ---------- Value-shape redaction ----------
//
// redactString scrubs secret-SHAPED substrings out of a plain string,
// independent of any attribute key — this is what catches a key embedded in
// a free-form message string (e.g. an error message that happens to quote
// an Authorization header), and what catches a key stored under an
// innocuous-looking attribute name that isSensitiveKey has no reason to
// flag.
//
// Patterns, in the order applied:
//
//   - github_pat_... (GitHub fine-grained personal access tokens)
//   - sk-...          (the "sk-" secret-key prefix shared by OpenAI and
//     many OpenAI-compatible providers this router talks to,
//     including Anthropic's own "sk-ant-" keys)
//   - Bearer <token>  (an Authorization header value quoted verbatim in
//     text; the literal word "Bearer" is kept for
//     readability — it is not secret — only the token
//     itself is scrubbed)
//   - a generic long base64/hex-ish blob (>=24 contiguous characters from
//     the base64url-ish alphabet, optional "=" padding), as a catch-all for
//     API keys that carry no recognisable prefix at all.
//
// The 24-character floor on the generic blob pattern is a deliberate
// judgement call: it is chosen to sit comfortably below realistic secret
// lengths (AWS-style secret keys are 40 chars; most vendor API keys are
// 32-64) while sitting ABOVE the longest contiguous run of characters in a
// hyphenated UUID (max 12, between hyphens) — so ordinary request/trace IDs
// formatted as UUIDs are not swept up as false positives. A 32-character
// UN-hyphenated hex id (e.g. a UUID with the hyphens stripped) WOULD still
// match and get redacted; that is an accepted false positive in exchange
// for never under-redacting a real secret of similar shape — see
// RedactedMarker's doc comment on why this package always prefers to
// over-redact.
var (
	githubPatPattern   = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)
	skKeyPattern       = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)
	bearerTokenPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	genericBlobPattern = regexp.MustCompile(`\b[A-Za-z0-9+/_]{24,}={0,2}\b`)
)

// redactString applies every value-shape pattern to s, in order, returning
// the scrubbed result. Cheap to call on strings with no matches at all
// (regexp.ReplaceAllString is a no-op copy in that case), so callers do not
// need to pre-check "does this look sensitive" before calling it.
func redactString(s string) string {
	if s == "" {
		return s
	}
	s = githubPatPattern.ReplaceAllString(s, RedactedMarker)
	s = skKeyPattern.ReplaceAllString(s, RedactedMarker)
	s = bearerTokenPattern.ReplaceAllString(s, "Bearer "+RedactedMarker)
	s = genericBlobPattern.ReplaceAllString(s, RedactedMarker)
	return s
}

// ---------- slog.Handler wrapper ----------

// RedactingHandler wraps another slog.Handler, scrubbing every record and
// attribute that passes through it before handing the result to next. It
// implements slog.Handler itself, so it composes transparently with
// slog.New, Logger.With, and Logger.WithGroup — none of those callers need
// to know redaction is happening.
type RedactingHandler struct {
	next slog.Handler
}

// NewRedactingHandler wraps next. Exported (rather than folded privately
// into New/NewWithOptions) so a caller that needs a slog.Handler directly —
// for composing with some other handler chain — can still get the
// redaction guarantee without going through this package's own logger
// constructors.
func NewRedactingHandler(next slog.Handler) *RedactingHandler {
	return &RedactingHandler{next: next}
}

// Enabled delegates to next: redaction has no opinion on levels.
func (h *RedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle scrubs the record's message and every attribute (recursing into
// slog.Group values, see redactAttr) before delegating to next.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, redactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, nr)
}

// WithAttrs scrubs attrs BEFORE they reach next.WithAttrs. This matters
// because a concrete handler (e.g. slog.JSONHandler) pre-renders attrs
// added via Logger.With at the point WithAttrs is called, not later at
// Handle time — so redacting only inside Handle would miss anything logged
// via .With(...).
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &RedactingHandler{next: h.next.WithAttrs(redacted)}
}

// WithGroup delegates the grouping itself to next: nesting subsequent
// attributes under a named JSON/text group is a rendering concern the
// underlying handler already owns. Every attribute that eventually flows
// through this group still passes through WithAttrs or Handle above first,
// so it is still scrubbed — WithGroup only changes WHERE it is rendered,
// never whether it is redacted.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{next: h.next.WithGroup(name)}
}

// redactAttr scrubs a single attribute, recursing into slog.Group values so
// a secret nested arbitrarily deep (a group inside a group inside a
// message) is still caught.
//
// Order of checks matters:
//
//  1. Resolve first — an attribute may be a slog.LogValuer that only
//     produces its real value lazily; skipping this would let a
//     LogValuer-wrapped secret slip through unresolved (and, worse, get
//     re-resolved and printed unredacted downstream by next).
//  2. Sensitive KEY wins outright, even over a Group value: if a group is
//     itself named e.g. "credentials", scrubbing the whole thing to a
//     single marker is safer than recursing in and potentially leaving
//     some sibling field inside it unscrubbed because its own key looked
//     innocuous.
//  3. Otherwise, a Group value recurses per-child.
//  4. Otherwise, a String value is passed through redactString for
//     value-shape scrubbing (catches a secret under an innocuous key name).
//  5. Anything else (numbers, bools, times, ...) cannot carry a string
//     secret and is returned unchanged.
func redactAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()

	if isSensitiveKey(a.Key) {
		return slog.Attr{Key: a.Key, Value: slog.StringValue(RedactedMarker)}
	}

	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		redacted := make([]slog.Attr, len(group))
		for i, ga := range group {
			redacted[i] = redactAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(redacted...)}
	}

	if a.Value.Kind() == slog.KindString {
		if s := a.Value.String(); s != "" {
			if scrubbed := redactString(s); scrubbed != s {
				return slog.Attr{Key: a.Key, Value: slog.StringValue(scrubbed)}
			}
		}
	}

	return a
}
