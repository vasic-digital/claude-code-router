package translate

import (
	"encoding/json"
	"fmt"
	"testing"
)

func smallRequest() *AnthropicRequest {
	return &AnthropicRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		System:    json.RawMessage(`"You are a helpful assistant."`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"What is the capital of France?"`)},
		},
		Tools: []AnthropicTool{
			{Name: "get_weather", Description: "Get the weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		},
	}
}

// largeRequest builds a 100-message conversation mixing plain text turns,
// tool_use turns and tool_result turns, roughly approximating a long-running
// Claude Code session.
func largeRequest() *AnthropicRequest {
	const n = 100
	messages := make([]AnthropicMessage, 0, n)
	for i := 0; i < n; i++ {
		switch i % 3 {
		case 0:
			text, _ := json.Marshal(fmt.Sprintf("This is message number %d with some representative body text about a coding task, spanning a full sentence or two so the payload resembles a real turn.", i))
			messages = append(messages, AnthropicMessage{Role: "user", Content: text})
		case 1:
			block := fmt.Sprintf(`[{"type":"tool_use","id":"tu_%d","name":"read_file","input":{"path":"/tmp/file_%d.go"}}]`, i, i)
			messages = append(messages, AnthropicMessage{Role: "assistant", Content: json.RawMessage(block)})
		default:
			block := fmt.Sprintf(`[{"type":"tool_result","tool_use_id":"tu_%d","content":"file contents for iteration %d\nline two\nline three"}]`, i-1, i)
			messages = append(messages, AnthropicMessage{Role: "user", Content: json.RawMessage(block)})
		}
	}
	return &AnthropicRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		System:    json.RawMessage(`"You are a coding assistant working inside a large repository."`),
		Messages:  messages,
		Tools: []AnthropicTool{
			{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "write_file", Description: "Write a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`)},
		},
	}
}

func BenchmarkAnthropicToOpenAISmall(b *testing.B) {
	req := smallRequest()
	opt := Options{CleanCache: true, StreamOptions: true, EnsureToolParameters: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := AnthropicToOpenAI(req, opt); err != nil {
			b.Fatalf("convert: %v", err)
		}
	}
}

func BenchmarkAnthropicToOpenAILarge(b *testing.B) {
	req := largeRequest()
	opt := Options{CleanCache: true, StreamOptions: true, EnsureToolParameters: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := AnthropicToOpenAI(req, opt); err != nil {
			b.Fatalf("convert: %v", err)
		}
	}
}

// largeCacheControlBody is a big nested JSON body salted with cache_control
// at every level, mirroring a real long-conversation request that a
// cache-control-stripping upstream would reject unless cleaned.
func largeCacheControlBody(b *testing.B) []byte {
	b.Helper()
	type block struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control,omitempty"`
	}
	type message struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
	msgs := make([]message, 0, 200)
	for i := 0; i < 200; i++ {
		msgs = append(msgs, message{
			Role: "user",
			Content: []block{
				{Type: "text", Text: fmt.Sprintf("message body %d, padded with representative text to approximate a real payload size.", i), CacheControl: json.RawMessage(`{"type":"ephemeral"}`)},
			},
		})
	}
	body := map[string]any{
		"model":    "m",
		"messages": msgs,
		"tools": []map[string]any{
			{"name": "t1", "cache_control": map[string]string{"type": "ephemeral"}},
			{"name": "t2", "cache_control": map[string]string{"type": "ephemeral"}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		b.Fatalf("marshal seed body: %v", err)
	}
	return raw
}

func BenchmarkStripCacheControlLarge(b *testing.B) {
	raw := largeCacheControlBody(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := StripCacheControl(raw); err != nil {
			b.Fatalf("strip: %v", err)
		}
	}
}
