package translate

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// ---------- small deterministic generators (no external dependency) ----------
//
// These are hand-rolled, seeded pseudo-random generators standing in for a
// property-testing library. Determinism (fixed seed) matters here: a failure
// must reproduce on every run without needing to capture and replay a seed
// out of band.

const propIterations = 500

var textWords = []string{
	"hello", "world", "the quick brown fox", "", "a", "1234567890",
	"unicode: héllo 😀 ‮", "line1\nline2", "  spaced  ", "\"quoted\"",
	"back\\slash", "\t\ttabbed",
}

// messageWords deliberately excludes "": a message whose ONLY content is an
// empty text block is dropped entirely by AnthropicToOpenAI (see the
// `if text != "" || len(calls) > 0` guard), which would break the
// order/count-preservation property below for a reason that has nothing to
// do with round-tripping non-empty text.
var messageWords = []string{
	"hello", "world", "the quick brown fox", "a", "1234567890",
	"unicode: héllo 😀 ‮", "line1\nline2", "  spaced  ", "\"quoted\"",
	"back\\slash", "\t\ttabbed",
}

var roles = []string{"user", "assistant"}

func randText(r *rand.Rand) string {
	return textWords[r.Intn(len(textWords))]
}

func randMessageText(r *rand.Rand) string {
	return messageWords[r.Intn(len(messageWords))]
}

func randRole(r *rand.Rand) string {
	return roles[r.Intn(len(roles))]
}

// genTextOnlyRequest builds an AnthropicRequest whose messages carry exactly
// one text content block each — the shape the round-trip property reasons
// about. A system prompt is attached about half the time.
func genTextOnlyRequest(r *rand.Rand) (*AnthropicRequest, []string, []string) {
	n := r.Intn(8) // 0..7 messages
	msgRoles := make([]string, n)
	msgTexts := make([]string, n)
	messages := make([]AnthropicMessage, n)
	for i := 0; i < n; i++ {
		role := randRole(r)
		text := randMessageText(r)
		msgRoles[i] = role
		msgTexts[i] = text
		content, err := json.Marshal(text)
		if err != nil {
			panic(err)
		}
		messages[i] = AnthropicMessage{Role: role, Content: content}
	}

	req := &AnthropicRequest{
		Model:    "prop-model",
		Messages: messages,
	}
	if r.Intn(2) == 0 {
		sys := randText(r)
		if sys != "" { // empty system text is dropped by design; excluded here
			b, err := json.Marshal(sys)
			if err != nil {
				panic(err)
			}
			req.System = b
			return req, msgRoles, msgTexts
		}
	}
	return req, msgRoles, msgTexts
}

// Property: for generated Anthropic requests with only text content,
// converting to OpenAI must preserve message ORDER and total text content.
func TestPropertyRoundTripPreservesOrderAndText(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := 0; i < propIterations; i++ {
		req, wantRoles, wantTexts := genTextOnlyRequest(r)

		out, err := AnthropicToOpenAI(req, Options{})
		if err != nil {
			t.Fatalf("iteration %d: convert failed: %v (req=%+v)", i, err, req)
		}

		gotMessages := out.Messages
		hasSystem := len(req.System) > 0
		if hasSystem {
			if len(gotMessages) == 0 || gotMessages[0].Role != "system" {
				t.Fatalf("iteration %d: expected leading system message", i)
			}
			gotMessages = gotMessages[1:]
		}

		if len(gotMessages) != len(wantRoles) {
			t.Fatalf("iteration %d: got %d messages, want %d (req=%+v, out=%+v)",
				i, len(gotMessages), len(wantRoles), req, out.Messages)
		}
		for j, m := range gotMessages {
			if m.Role != wantRoles[j] {
				t.Fatalf("iteration %d: message[%d].role = %q, want %q (order not preserved)",
					i, j, m.Role, wantRoles[j])
			}
			gotText, _ := m.Content.(string)
			if gotText != wantTexts[j] {
				t.Fatalf("iteration %d: message[%d].content = %q, want %q",
					i, j, gotText, wantTexts[j])
			}
		}
	}
}

// ---------- StripCacheControl idempotence ----------

// genJSONValue builds a random JSON-marshalable tree, sometimes salted with
// "cache_control" keys at random depths, so idempotence is exercised on
// realistic nested shapes rather than only flat ones.
func genJSONValue(r *rand.Rand, depth int) any {
	if depth <= 0 || r.Intn(4) == 0 {
		switch r.Intn(5) {
		case 0:
			return randText(r)
		case 1:
			return r.Intn(1000)
		case 2:
			return r.Intn(2) == 0
		case 3:
			return nil
		default:
			return r.Float64()
		}
	}
	switch r.Intn(2) {
	case 0:
		n := r.Intn(4)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = genJSONValue(r, depth-1)
		}
		return arr
	default:
		n := r.Intn(4)
		obj := make(map[string]any, n+1)
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("k%d", i)
			obj[key] = genJSONValue(r, depth-1)
		}
		// Salt with cache_control some of the time, at this level.
		if r.Intn(3) == 0 {
			obj["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return obj
	}
}

func TestPropertyStripCacheControlIsIdempotent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < propIterations; i++ {
		v := genJSONValue(r, 4)
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("iteration %d: marshal seed value: %v", i, err)
		}

		once, err := StripCacheControl(raw)
		if err != nil {
			t.Fatalf("iteration %d: first strip: %v", i, err)
		}
		twice, err := StripCacheControl(once)
		if err != nil {
			t.Fatalf("iteration %d: second strip: %v", i, err)
		}

		// Compare via decoded structures, not raw bytes: map key order is not
		// stable across encodings in general, but json.Marshal on Go maps
		// sorts keys deterministically, so this also holds byte-for-byte;
		// decoding first makes the property robust to that implementation
		// detail rather than relying on it.
		var oneVal, twoVal any
		if err := json.Unmarshal(once, &oneVal); err != nil {
			t.Fatalf("iteration %d: decode once: %v", i, err)
		}
		if err := json.Unmarshal(twice, &twoVal); err != nil {
			t.Fatalf("iteration %d: decode twice: %v", i, err)
		}
		oneJSON, _ := json.Marshal(oneVal)
		twoJSON, _ := json.Marshal(twoVal)
		if string(oneJSON) != string(twoJSON) {
			t.Fatalf("iteration %d: not idempotent:\n once=%s\n twice=%s", i, oneJSON, twoJSON)
		}
	}
}
