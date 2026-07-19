package cache

import (
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func f64(v float64) *float64 { return &v }

func TestCacheable_TemperatureGate(t *testing.T) {
	cases := []struct {
		name string
		temp *float64
		want bool
	}{
		{"unset temperature is cacheable", nil, true},
		{"temperature 0 is cacheable", f64(0), true},
		{"temperature 0.7 is NOT cacheable", f64(0.7), false},
		{"temperature 1 is NOT cacheable", f64(1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &translate.AnthropicRequest{Model: "m", Temperature: tc.temp}
			got, reason := Cacheable(r)
			if got != tc.want {
				t.Fatalf("Cacheable=%v want %v (reason=%q)", got, tc.want, reason)
			}
			if !got && reason == "" {
				t.Fatal("refusal must carry a reason")
			}
		})
	}
}

func TestCacheable_StreamingGate(t *testing.T) {
	r := &translate.AnthropicRequest{Model: "m", Stream: true}
	got, reason := Cacheable(r)
	if got {
		t.Fatal("streaming request must not be cacheable")
	}
	if reason != "streaming" {
		t.Fatalf("reason=%q want streaming", reason)
	}
}

func TestCacheable_NilRequest(t *testing.T) {
	if ok, _ := Cacheable(nil); ok {
		t.Fatal("nil request must not be cacheable")
	}
}

func TestResponseCacheable_ToolGate(t *testing.T) {
	toolResp := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)

	if ok, reason := ResponseCacheable(toolResp, false); ok {
		t.Fatal("tool-call response must not be cached by default")
	} else if reason != "tool-call-response" {
		t.Fatalf("reason=%q want tool-call-response", reason)
	}

	if ok, _ := ResponseCacheable(toolResp, true); !ok {
		t.Fatal("tool-call response must be cacheable when explicitly allowed")
	}
}

func TestResponseCacheable_PlainAnswer(t *testing.T) {
	ok, reason := ResponseCacheable(
		[]byte(`{"choices":[{"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}]}`), false)
	if !ok {
		t.Fatalf("plain answer must be cacheable, got refusal %q", reason)
	}
}

func TestResponseCacheable_ErrorAndEmpty(t *testing.T) {
	if ok, _ := ResponseCacheable([]byte(`{"error":{"message":"boom"}}`), false); ok {
		t.Fatal("error body must never be cached")
	}
	if ok, _ := ResponseCacheable(nil, false); ok {
		t.Fatal("empty body must never be cached")
	}
	if ok, _ := ResponseCacheable([]byte(`not json`), false); ok {
		t.Fatal("unparseable body must never be cached")
	}
	if ok, _ := ResponseCacheable([]byte(`{"choices":[]}`), false); ok {
		t.Fatal("no-choices body must never be cached")
	}
}
