package challenges

import (
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "deep-json-recursion-depth",
		TestName: "TestChallenge_DeepJSONRecursionDepthBoundary",
		Hypothesis: "translate.StripCacheControl's stripKey walks the decoded JSON tree " +
			"recursively with no explicit depth guard of its own. If an attacker could hand it an " +
			"arbitrarily deep JSON document as raw bytes, stripKey's own recursion could in " +
			"principle be driven deep enough to exhaust the goroutine stack (a DoS via a fatal, " +
			"unrecoverable stack overflow, not a normal Go panic). This challenge determines " +
			"empirically whether that is actually reachable, given that StripCacheControl's FIRST " +
			"step is a normal encoding/json.Unmarshal into `any`.",
		ExpectedSafeOutcome: "Go's own encoding/json enforces a hard-coded max nesting depth (10000 " +
			"levels) during Unmarshal and returns a clean, ordinary error beyond it -- so " +
			"stripKey's unbounded recursion is never actually reachable at pathological depth: " +
			"the json.Unmarshal call inside StripCacheControl rejects the oversized document " +
			"BEFORE stripKey ever runs. Depths at or under 10000 must succeed quickly and safely.",
	})
}

// buildNestedObject returns `{"a":{"a":{...."leaf"...}}}` nested depth
// levels deep, as raw bytes (not constructed via any Go struct -- this
// probes the JSON *decoder's* own depth handling, which is the layer that
// actually stands between an adversarial request body and stripKey's
// recursion).
func buildNestedObject(depth int) []byte {
	var b strings.Builder
	b.Grow(depth*6 + 8)
	for i := 0; i < depth; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString(`"leaf"`)
	for i := 0; i < depth; i++ {
		b.WriteByte('}')
	}
	return []byte(b.String())
}

func TestChallenge_DeepJSONRecursionDepthBoundary(t *testing.T) {
	t.Run("depth_10000_succeeds_safely", func(t *testing.T) {
		raw := buildNestedObject(10000)
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("StripCacheControl panicked (should never happen; fatal stack overflow cannot even be recovered, so a genuine one would abort the whole test binary instead) at depth 10000: %v", r)
			}
		}()
		if _, err := translate.StripCacheControl(raw); err != nil {
			t.Fatalf("StripCacheControl failed at depth 10000, which Go's own decoder is documented to accept: %v", err)
		}
	})

	t.Run("depth_10001_fails_cleanly_not_a_crash", func(t *testing.T) {
		raw := buildNestedObject(10001)
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("StripCacheControl panicked instead of failing cleanly at depth 10001: %v", r)
			}
		}()
		_, err := translate.StripCacheControl(raw)
		if err == nil {
			t.Fatal("expected StripCacheControl to reject a JSON document beyond the decoder's max nesting depth")
		}
		if !strings.Contains(err.Error(), "max depth") {
			t.Errorf("error = %q, want it to mention the max-depth guard (confirms the decoder's own protection fired, not some other failure)", err.Error())
		}
		t.Logf("safe: depth 10001 rejected cleanly by encoding/json before stripKey's own recursion ever ran: %v", err)
	})
}
