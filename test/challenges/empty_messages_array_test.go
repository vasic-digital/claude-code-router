package challenges

import (
	"encoding/json"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "empty-messages-array",
		TestName: "TestChallenge_EmptyMessagesArray",
		Hypothesis: "An explicit, empty Messages slice ([]) is a legitimate shape a client could " +
			"send (e.g. a client-side bug, or a deliberate \"just set up the system prompt\" probe) " +
			"and must not be treated as malformed input.",
		ExpectedSafeOutcome: "translate.AnthropicToOpenAI succeeds with no error; the output carries " +
			"only whatever the system prompt contributes (zero-or-one message), never a panic from " +
			"ranging over an empty (not nil) slice.",
	})
}

func TestChallenge_EmptyMessagesArray(t *testing.T) {
	t.Run("empty_messages_no_system", func(t *testing.T) {
		req := &translate.AnthropicRequest{
			Model:    "m",
			Messages: []translate.AnthropicMessage{}, // explicit, non-nil, empty
		}
		out, err := translate.AnthropicToOpenAI(req, translate.Options{})
		if err != nil {
			t.Fatalf("unexpected error on an empty messages array: %v", err)
		}
		if len(out.Messages) != 0 {
			t.Fatalf("messages = %d, want 0", len(out.Messages))
		}
	})

	t.Run("empty_messages_with_system", func(t *testing.T) {
		req := &translate.AnthropicRequest{
			Model:    "m",
			System:   json.RawMessage(`"you are helpful"`),
			Messages: []translate.AnthropicMessage{},
		}
		out, err := translate.AnthropicToOpenAI(req, translate.Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out.Messages) != 1 || out.Messages[0].Role != "system" {
			t.Fatalf("messages = %+v, want exactly one system message", out.Messages)
		}
	})
	t.Log("safe: an empty (not nil) messages array never panics and never errors")
}
