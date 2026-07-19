package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/andybalholm/brotli"
)

func BenchmarkNegotiate(b *testing.B) {
	headers := []string{
		"gzip, br",
		"br;q=0.1, gzip;q=0.9",
		"",
		"identity",
		"gzip, deflate, br",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		negotiate(headers[i%len(headers)])
	}
}

// realistic50KBJSON builds a JSON body shaped like a real Claude Code
// streaming/non-streaming response payload: an assistant message with tool
// calls, repeated to land at roughly 50KB, so the brotli/gzip comparison
// below measures something representative rather than an arbitrary blob.
func realistic50KBJSON(b *testing.B) []byte {
	b.Helper()
	type toolCall struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Fn   struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	type choice struct {
		Index   int    `json:"index"`
		Message any    `json:"message"`
		Finish  string `json:"finish_reason"`
	}
	type message struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []toolCall `json:"tool_calls,omitempty"`
	}

	var choices []choice
	for i := 0; i < 90; i++ {
		msg := message{
			Role: "assistant",
			Content: fmt.Sprintf(
				"This is a representative response chunk %d containing prose that a real "+
					"model completion would produce: explanation, some repeated structure, and "+
					"enough text to approximate realistic entropy for a compression benchmark. "+
					"Padding the sentence further so the overall payload lands close to fifty "+
					"kilobytes, which is a realistic size for a Claude Code turn with several "+
					"tool calls and a few paragraphs of assistant prose mixed together.", i),
		}
		if i%3 == 0 {
			tc := toolCall{ID: fmt.Sprintf("tu_%d", i), Type: "function"}
			tc.Fn.Name = "read_file"
			tc.Fn.Arguments = fmt.Sprintf(`{"path":"/repo/src/file_%d.go"}`, i)
			msg.ToolCalls = []toolCall{tc}
		}
		choices = append(choices, choice{Index: i, Message: msg, Finish: "stop"})
	}
	body := map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "bench-model",
		"choices": choices,
		"usage":   map[string]int{"prompt_tokens": 1234, "completion_tokens": 5678, "total_tokens": 6912},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		b.Fatalf("marshal benchmark body: %v", err)
	}
	// "Realistic ~50KB" is a target, not an exact figure; the acceptable band
	// just needs to keep this from silently regressing to a trivially small
	// (and therefore unrepresentative) payload.
	if len(raw) < 40000 || len(raw) > 65000 {
		b.Fatalf("benchmark body is %d bytes, want it in the ~40-65KB band (~50KB target)", len(raw))
	}
	return raw
}

func BenchmarkCompressionBrotli(b *testing.B) {
	raw := realistic50KBJSON(b)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		w := brotli.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			b.Fatalf("brotli write: %v", err)
		}
		if err := w.Close(); err != nil {
			b.Fatalf("brotli close: %v", err)
		}
		if i == 0 {
			b.ReportMetric(float64(buf.Len()), "compressed_bytes")
			b.ReportMetric(float64(len(raw))/float64(buf.Len()), "ratio")
		}
	}
}

func BenchmarkCompressionGzip(b *testing.B) {
	raw := realistic50KBJSON(b)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			b.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			b.Fatalf("gzip close: %v", err)
		}
		if i == 0 {
			b.ReportMetric(float64(buf.Len()), "compressed_bytes")
			b.ReportMetric(float64(len(raw))/float64(buf.Len()), "ratio")
		}
	}
}
