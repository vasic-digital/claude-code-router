package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// ---------- FuzzAnthropicToOpenAI ----------
//
// Invariant under fuzz: AnthropicToOpenAI must NEVER panic on arbitrary JSON
// request bodies, no matter how malformed. Returning an error is fine and
// expected for garbage input; a panic would take down the whole gateway
// process for every other in-flight request.

func FuzzAnthropicToOpenAI(f *testing.F) {
	// Seed with the real fixtures used by anthropic_test.go, so the fuzzer
	// starts from known-good and known-edge-case shapes rather than from
	// nothing.
	seeds := []string{
		// Plain text round trip.
		`{"model":"m","max_tokens":100,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`,
		// System as a block array.
		`{"model":"m","system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"messages":[{"role":"user","content":"hi"}]}`,
		// Content block array.
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}]}`,
		// tool_use.
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Paris"}}]}]}`,
		// tool_result then text.
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"22C sunny"},{"type":"text","text":"thanks"}]}]}`,
		// tool with no input_schema.
		`{"model":"m","tools":[{"name":"noparams","description":"d"}],"messages":[{"role":"user","content":"hi"}]}`,
		// Streaming.
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		// cache_control at every depth.
		`{"model":"m","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}]}`,
		// Image block (explicit error path, not a panic path).
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`,
		// Malformed content (numeric).
		`{"model":"m","messages":[{"role":"user","content":12345}]}`,
		// Sampling params + stop sequences.
		`{"model":"m","temperature":0.5,"top_p":0.9,"stop_sequences":["DONE"],"messages":[{"role":"user","content":"hi"}]}`,
		// Empty request.
		`{}`,
		// Nasty inputs: deeply nested arrays.
		`{"model":"m","messages":[{"role":"user","content":[[[[[[[[[["x"]]]]]]]]]]}]}`,
		// Huge string.
		`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 20000) + `"}]}`,
		// Null content.
		`{"model":"m","messages":[{"role":"user","content":null}]}`,
		// Unicode, including surrogate-adjacent and RTL/emoji content.
		`{"model":"m","messages":[{"role":"user","content":"h\u00e9llo \ud83d\ude00 \u202e evil"}]}`,
		// Empty objects everywhere.
		`{"model":"","messages":[{"role":"","content":{}}]}`,
		// Non-object top level.
		`[1,2,3]`,
		// Just a string.
		`"not an object"`,
		// A number.
		`42`,
		// Malformed JSON (truncated).
		`{"model":"m","messages":[{"role":"user","content":"hi"`,
		// Tool with malformed input schema (not an object).
		`{"model":"m","tools":[{"name":"t","input_schema":"not-a-schema"}],"messages":[{"role":"user","content":"hi"}]}`,
		// tool_use with malformed input.
		`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu","name":"n","input":"not-an-object"}]}]}`,
		// Content block missing "type".
		`{"model":"m","messages":[{"role":"user","content":[{"text":"no type field"}]}]}`,
		// Message with no role.
		`{"model":"m","messages":[{"content":"hi"}]}`,
		// Deeply nested tool_result content.
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"nested"}]}]}]}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("AnthropicToOpenAI panicked on input %q: %v", body, r)
			}
		}()

		var in AnthropicRequest
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			// Not a well-formed AnthropicRequest at all — AnthropicToOpenAI is
			// never called with input that doesn't even decode into the typed
			// struct by real callers (the gateway decodes first), but we still
			// must not panic if it were.
			return
		}

		for _, opt := range []Options{
			{},
			{CleanCache: true},
			{StreamOptions: true},
			{EnsureToolParameters: true},
			{CleanCache: true, StreamOptions: true, EnsureToolParameters: true, Model: "override"},
		} {
			_, _ = AnthropicToOpenAI(&in, opt)
		}
	})
}

// ---------- FuzzStripCacheControl ----------
//
// Invariants:
//  1. Never panics.
//  2. If the input is JSON that encoding/json's own generic (`any`) decode
//     can represent, the output is always valid JSON too.
//  3. The substring "cache_control" never survives in the output.
//
// A REAL FUZZER FINDING, since FIXED in the implementation.
//
// The first `go test -fuzz=FuzzStripCacheControl` run flagged the one-token
// body `1E700`: syntactically valid JSON, yet StripCacheControl returned
// "cannot unmarshal number 1E700 into Go value of type float64". The original
// implementation decoded with json.Unmarshal into `any`, which turns every
// number into a float64.
//
// Investigating showed the rejection was only the visible half. The same
// float64 conversion also SILENTLY rounded 12345678901234567890 to
// 12345678901234567000 — a proxy corrupting a value it was only ever meant to
// forward, with no error at all. That was the more dangerous defect.
//
// StripCacheControl now decodes with json.Decoder + UseNumber(), so literals
// survive verbatim. Both the probe and the output decode below use UseNumber
// to match; probing with plain json.Unmarshal would re-pin the old lossy
// behaviour as if it were the specification. numbers_test.go carries explicit
// regression cases, and the minimized input is kept as a permanent corpus
// entry at testdata/fuzz/FuzzStripCacheControl/9e76694541809c1e.
func FuzzStripCacheControl(f *testing.F) {
	seeds := []string{
		`{"model":"m","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}]}`,
		`{}`,
		`[]`,
		`null`,
		`42`,
		`"cache_control"`,
		`{"cache_control":"top level"}`,
		`{"a":{"b":{"c":{"cache_control":1}}}}`,
		`[{"cache_control":1},{"cache_control":2}]`,
		`{"cache_controlish":"not the key, just a prefix"}`,
		`not json at all`,
		`{"unterminated`,
		`{"nested_array":[[{"cache_control":{}}],[[{"cache_control":{}}]]]}`,
		`{"unicode_key_cache_control":1}`,
		// The exact finding above: syntactically valid JSON, but not
		// representable by encoding/json's generic decode.
		`1E700`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, body string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("StripCacheControl panicked on input %q: %v", body, r)
			}
		}()

		raw := []byte(body)
		// The probe must mirror what StripCacheControl actually does — a
		// json.Decoder with UseNumber() — not a plain json.Unmarshal.
		//
		// This originally probed with plain json.Unmarshal, which converts
		// every JSON number to float64, and on that basis the fuzzer flagged
		// "1E700" as a StripCacheControl bug. Investigation showed the
		// opposite: float64 decoding WAS the defect. It rejected 1E700
		// outright and, worse, silently rounded 12345678901234567890 to
		// 12345678901234567000 — a proxy corrupting a value it only had to
		// forward. StripCacheControl now uses UseNumber so literals survive
		// verbatim (see numbers_test.go). Had this probe been left as-is, it
		// would have pinned that lossy behaviour in place as the spec.
		probeDecodable := func(b []byte) bool {
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			var probe any
			return d.Decode(&probe) == nil
		}
		wasDecodable := probeDecodable(raw)

		out, err := StripCacheControl(raw)
		if err != nil {
			// Non-decodable input -> error out is fine.
			if wasDecodable {
				t.Fatalf("StripCacheControl errored on decodable input %q: %v", body, err)
			}
			return
		}
		if !wasDecodable {
			t.Fatalf("StripCacheControl succeeded on input %q that its own decoder cannot decode; output: %q", body, out)
		}

		// Invariant 2: decodable input implies valid JSON out.
		if !json.Valid(out) {
			t.Fatalf("StripCacheControl produced invalid JSON for input %q: output %q", body, out)
		}

		// Invariant 3: no object has a "cache_control" KEY left, at any
		// depth. This is deliberately a structural walk rather than a raw
		// substring search on the bytes: fuzzing arbitrary JSON can (and
		// does, see seed `"cache_control"`) produce a bare JSON *string
		// whose VALUE happens to equal "cache_control" — that is legitimate
		// data, not a key, and must survive untouched. A substring check
		// cannot tell the two apart and would flag that correct behaviour as
		// a bug.
		// UseNumber here too: the output legitimately contains literals such
		// as 1E700 that a plain float64-based decode cannot represent, and
		// preserving those verbatim is the point of the fix.
		outDec := json.NewDecoder(bytes.NewReader(out))
		outDec.UseNumber()
		var decoded any
		if err := outDec.Decode(&decoded); err != nil {
			t.Fatalf("decode stripped output for input %q: %v", body, err)
		}
		if path, found := findCacheControlKey(decoded, ""); found {
			t.Fatalf("cache_control key survived stripping at %s: input %q -> output %q", path, body, out)
		}
	})
}

// findCacheControlKey walks a decoded JSON value looking for any map with a
// literal "cache_control" key, returning a breadcrumb path for diagnostics.
func findCacheControlKey(v any, path string) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["cache_control"]; ok {
			return path + ".cache_control", true
		}
		for k, sub := range t {
			if p, found := findCacheControlKey(sub, path+"."+k); found {
				return p, true
			}
		}
	case []any:
		for i, sub := range t {
			if p, found := findCacheControlKey(sub, fmt.Sprintf("%s[%d]", path, i)); found {
				return p, true
			}
		}
	}
	return "", false
}
