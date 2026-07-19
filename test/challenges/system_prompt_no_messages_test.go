package challenges

import (
	"encoding/json"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "system-prompt-no-messages",
		TestName: "TestChallenge_SystemPromptWithNoMessagesKeyAtAll",
		Hypothesis: "A request carrying only a system prompt, with the \"messages\" key omitted " +
			"from the JSON entirely (Messages is a nil slice, not merely empty), is structurally " +
			"different from the empty_messages_array challenge and must be handled identically: " +
			"cleanly, with exactly the system message emitted.",
		ExpectedSafeOutcome: "No panic ranging over a nil slice; output is exactly one system " +
			"message and nothing else.",
	})
}

func TestChallenge_SystemPromptWithNoMessagesKeyAtAll(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":10,"system":"You only need to greet the user."}`)
	var req translate.AnthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if req.Messages != nil {
		t.Fatalf("precondition failed: Messages = %#v, want a true nil slice (key was never in the JSON)", req.Messages)
	}

	out, err := translate.AnthropicToOpenAI(&req, translate.Options{})
	if err != nil {
		t.Fatalf("unexpected error for a system-only, no-messages-key request: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want exactly 1 (the system message)", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("messages[0].role = %q, want system", out.Messages[0].Role)
	}
	content, _ := out.Messages[0].Content.(string)
	if content != "You only need to greet the user." {
		t.Fatalf("system content = %q, want it preserved exactly", content)
	}
	t.Log("safe: a genuinely nil Messages slice (no \"messages\" key in the wire JSON) converts cleanly to a single system message")
}
