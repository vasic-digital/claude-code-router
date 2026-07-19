package challenges

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "10mb-request-body",
		TestName: "TestChallenge_TenMegabyteRequestBody",
		Hypothesis: "Neither translate.AnthropicToOpenAI nor proxy.Client.Do does anything " +
			"size-based -- no fixed buffer, no truncation, no accidental O(n^2) string " +
			"concatenation -- so a ~10MB single message should convert and be sent to an " +
			"upstream without truncation, corruption, or pathological slowness.",
		ExpectedSafeOutcome: "The converted request carries the full content length; the (local, " +
			"loopback-only, hermetic) fake upstream receives every byte; the whole round trip " +
			"completes well within the test's timeout.",
	})
}

// TestChallenge_TenMegabyteRequestBody drives a ~10MB user message through
// the full local pipeline: translate.AnthropicToOpenAI -> proxy.Client.Do.
// The "upstream" is an httptest.Server bound to 127.0.0.1 -- no real
// network access, fully hermetic.
func TestChallenge_TenMegabyteRequestBody(t *testing.T) {
	const tenMB = 10 * 1024 * 1024
	body := strings.Repeat("A", tenMB-len("START-END-MARKER")) + "START-END-MARKER"

	var receivedLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		receivedLen = len(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	req := &translate.AnthropicRequest{
		Model:     "m",
		MaxTokens: 100,
		Messages:  []translate.AnthropicMessage{{Role: "user", Content: json.RawMessage(mustQuote(body))}},
	}

	start := time.Now()
	out, err := translate.AnthropicToOpenAI(req, translate.Options{})
	if err != nil {
		t.Fatalf("AnthropicToOpenAI failed on a 10MB message: %v", err)
	}
	got, ok := out.Messages[0].Content.(string)
	if !ok || len(got) != len(body) || !strings.HasSuffix(got, "START-END-MARKER") {
		t.Fatalf("converted content was truncated or corrupted: len=%d want=%d, suffix-ok=%v", len(got), len(body), strings.HasSuffix(got, "START-END-MARKER"))
	}

	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	client := proxy.New(5 * time.Second)
	provider := &config.Provider{Name: "fake", APIBaseURL: upstream.URL, APIKey: "k"}
	resp, err := client.Do(t.Context(), provider, wire, false)
	if err != nil {
		t.Fatalf("proxy.Client.Do failed on a 10MB payload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream status = %d, want 200", resp.StatusCode)
	}
	if receivedLen != len(wire) {
		t.Fatalf("upstream received %d bytes, wire payload was %d bytes -- body was truncated/split incorrectly", receivedLen, len(wire))
	}

	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("10MB round trip took %v, want well under 5s on loopback (possible pathological slowness)", elapsed)
	}
	t.Logf("safe: 10MB body converted + sent + received intact in %v", elapsed)
}

func mustQuote(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // test-fixture construction only; a marshal failure here is a test bug, not a challenge outcome
	}
	return b
}
