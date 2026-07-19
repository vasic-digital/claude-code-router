package cache

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// --- Cacheable: exhaustive table over temperature / streaming ---------------
func TestCacheable_ExhaustiveTable(t *testing.T) {
	cases := []struct {
		name       string
		stream     bool
		temp       *float64
		wantOK     bool
		wantReason string
	}{
		{"clean unset temp", false, nil, true, ""},
		{"explicit zero temp", false, f64(0), true, ""},
		{"tiny positive temp", false, f64(1e-9), false, "temperature>0"},
		{"temp 0.5", false, f64(0.5), false, "temperature>0"},
		{"temp 2.0", false, f64(2.0), false, "temperature>0"},
		{"negative temp still non-zero", false, f64(-0.1), false, "temperature>0"},
		{"streaming beats clean temp", true, nil, false, "streaming"},
		{"streaming beats zero temp", true, f64(0), false, "streaming"},
		{"streaming checked before temperature", true, f64(0.7), false, "streaming"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &translate.AnthropicRequest{Model: "m", Stream: tc.stream, Temperature: tc.temp}
			ok, reason := Cacheable(r)
			if ok != tc.wantOK {
				t.Fatalf("Cacheable=%v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason=%q want %q", reason, tc.wantReason)
			}
			// Property: a refusal always carries a non-empty, stable reason.
			if !ok && reason == "" {
				t.Fatal("refusal must carry a reason label")
			}
			// Property: acceptance carries an empty reason.
			if ok && reason != "" {
				t.Fatalf("acceptance must have empty reason, got %q", reason)
			}
		})
	}
}

func TestCacheable_NilRequestReason(t *testing.T) {
	ok, reason := Cacheable(nil)
	if ok {
		t.Fatal("nil request must not be cacheable")
	}
	if reason != "nil-request" {
		t.Fatalf("reason=%q want nil-request", reason)
	}
}

// --- ResponseCacheable: exhaustive table over body shapes -------------------
func TestResponseCacheable_ExhaustiveTable(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		allowTools bool
		wantOK     bool
		wantReason string
	}{
		{
			name: "plain assistant answer", wantOK: true, wantReason: "",
			body: `{"choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}]}`,
		},
		{
			name: "explicit error:null is not an error", wantOK: true, wantReason: "",
			body: `{"error":null,"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		},
		{
			name: "empty body", wantOK: false, wantReason: "empty-body", body: ``,
		},
		{
			name: "whitespace body is unparseable", wantOK: false, wantReason: "unparseable-body", body: `   `,
		},
		{
			name: "non-json body", wantOK: false, wantReason: "unparseable-body", body: `not json at all`,
		},
		{
			name: "error object present", wantOK: false, wantReason: "error-in-body",
			body: `{"error":{"message":"boom","type":"server_error"}}`,
		},
		{
			name: "error string present", wantOK: false, wantReason: "error-in-body",
			body: `{"error":"rate limited"}`,
		},
		{
			name: "no choices key", wantOK: false, wantReason: "no-choices", body: `{"id":"x"}`,
		},
		{
			name: "empty choices array", wantOK: false, wantReason: "no-choices", body: `{"choices":[]}`,
		},
		{
			name: "tool_calls array blocked by default", wantOK: false, wantReason: "tool-call-response",
			body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "finish_reason tool_calls without array still blocked", wantOK: false, wantReason: "tool-call-response",
			body: `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "second choice carries tool_calls -> blocked", wantOK: false, wantReason: "tool-call-response",
			body: `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"},{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"n","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "tool_calls allowed when opted in", wantOK: true, wantReason: "", allowTools: true,
			body: `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ResponseCacheable([]byte(tc.body), tc.allowTools)
			if ok != tc.wantOK {
				t.Fatalf("ResponseCacheable=%v want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason=%q want %q", reason, tc.wantReason)
			}
			if !ok && reason == "" {
				t.Fatal("refusal must carry a reason label")
			}
			if ok && reason != "" {
				t.Fatalf("acceptance must have empty reason, got %q", reason)
			}
		})
	}
}

// --- Property: allowToolResponses only ever loosens, never tightens ---------
//
// For any body, enabling allowToolResponses can only turn a refusal into an
// acceptance (the tool gate is the only predicate it controls); it can never
// flip an acceptance into a refusal, and it never changes a non-tool refusal.
func TestResponseCacheable_AllowToolsMonotonic(t *testing.T) {
	bodies := []string{
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"c","type":"function","function":{"name":"n","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"content":""},"finish_reason":"tool_calls"}]}`,
		`{"error":{"message":"x"}}`,
		`{"choices":[]}`,
		``,
		`nope`,
	}
	for _, b := range bodies {
		okStrict, rStrict := ResponseCacheable([]byte(b), false)
		okLoose, _ := ResponseCacheable([]byte(b), true)

		if okStrict && !okLoose {
			t.Fatalf("body %q: allowing tools turned an accept into a refusal (non-monotonic)", b)
		}
		// A refusal that is NOT the tool gate must be unaffected by the flag.
		if !okStrict && rStrict != "tool-call-response" && okLoose {
			t.Fatalf("body %q: non-tool refusal %q wrongly loosened by allowTools", b, rStrict)
		}
	}
}
