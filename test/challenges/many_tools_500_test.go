package challenges

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "500-tools-one-request",
		TestName: "TestChallenge_FiveHundredToolsInOneRequest",
		Hypothesis: "The tools-conversion loop in AnthropicToOpenAI is a plain O(n) range over " +
			"in.Tools with no hard-coded cap, so 500 declared tools should convert completely, " +
			"in order, without silently dropping any of them (a real regression risk if a future " +
			"change introduced a fixed-size buffer or an early-return heuristic).",
		ExpectedSafeOutcome: "Exactly 500 OpenAITool entries come out, in the same order they went " +
			"in, each correctly typed \"function\", within a small fraction of a second.",
	})
}

func TestChallenge_FiveHundredToolsInOneRequest(t *testing.T) {
	const n = 500
	req := &translate.AnthropicRequest{
		Model:    "m",
		Messages: []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	for i := 0; i < n; i++ {
		req.Tools = append(req.Tools, translate.AnthropicTool{
			Name:        fmt.Sprintf("tool_%03d", i),
			Description: fmt.Sprintf("tool number %d", i),
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		})
	}

	start := time.Now()
	out, err := translate.AnthropicToOpenAI(req, translate.Options{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("conversion with 500 tools failed: %v", err)
	}
	if len(out.Tools) != n {
		t.Fatalf("tools count = %d, want %d -- tools were dropped", len(out.Tools), n)
	}
	for i, tool := range out.Tools {
		want := fmt.Sprintf("tool_%03d", i)
		if tool.Function.Name != want {
			t.Fatalf("tools[%d].function.name = %q, want %q -- order not preserved", i, tool.Function.Name, want)
		}
		if tool.Type != "function" {
			t.Fatalf("tools[%d].type = %q, want function", i, tool.Type)
		}
	}
	if elapsed > time.Second {
		t.Fatalf("converting 500 tools took %v, want well under 1s", elapsed)
	}
	t.Logf("safe: all %d tools present, in order, in %v", n, elapsed)
}
