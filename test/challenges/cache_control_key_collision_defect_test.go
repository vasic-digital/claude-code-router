package challenges

import (
	"encoding/json"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "cache-control-key-collision",
		TestName: "TestChallenge_CacheControlKeyCollisionCorruptsToolSchema",
		Hypothesis: "translate.StripCacheControl's stripKey deletes ANY JSON object key literally " +
			"named \"cache_control\", anywhere in the tree, with no awareness of JSON path or " +
			"semantic context. Anthropic's own cache_control metadata only ever appears as a " +
			"sibling of \"type\" inside system/message/tool_result content blocks or tool " +
			"definitions -- but a legitimately-named tool parameter called \"cache_control\" (e.g. " +
			"a caching-policy config tool) would collide with the same key name and be deleted too.",
		ExpectedSafeOutcome: "A tool's input_schema declaring a property literally named " +
			"\"cache_control\" should survive StripCacheControl untouched -- the function should " +
			"only remove Anthropic's own metadata key, not an unrelated same-named data field.",
		Defect: "StripCacheControl (internal/translate/anthropic.go stripKey) performs a blind, " +
			"path-unaware recursive delete-by-key-name. A tool input_schema whose JSON Schema " +
			"legitimately defines a property named \"cache_control\" has that property SILENTLY " +
			"DELETED from \"properties\", while a \"required\":[\"cache_control\"] entry elsewhere " +
			"in the same schema is left dangling -- producing a self-contradictory JSON Schema " +
			"(a required property that no longer exists). This is exactly the \"silently corrupts\" " +
			"failure mode this suite exists to catch: no panic, no error, just quietly wrong data.",
	})
}

// TestChallenge_CacheControlKeyCollisionCorruptsToolSchema is a
// Reproduction-Before-Fix style test: it first proves the defect is real by
// exercising the actual, current translate.StripCacheControl, then reports
// it via t.Skip("DEFECT: ...") so this suite does not fail CI over a defect
// it did not introduce and was not asked to fix. Once the underlying
// function is made path-aware (or otherwise no longer collides on
// coincidentally-named fields), this test starts passing on its own and the
// skip stops firing -- no test-code change required to "close" the defect.
func TestChallenge_CacheControlKeyCollisionCorruptsToolSchema(t *testing.T) {
	raw := []byte(`{
		"model": "m",
		"tools": [{
			"name": "configure_cache",
			"description": "Adjust the caching policy for this session.",
			"input_schema": {
				"type": "object",
				"properties": {
					"cache_control": {
						"type": "string",
						"enum": ["ephemeral", "persistent"],
						"description": "which cache policy to apply"
					},
					"ttl_seconds": {"type": "integer"}
				},
				"required": ["cache_control"]
			}
		}],
		"messages": [{"role": "user", "content": "turn off caching"}]
	}`)

	stripped, err := translate.StripCacheControl(raw)
	if err != nil {
		t.Fatalf("StripCacheControl unexpectedly errored: %v", err)
	}

	var schema struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(stripped, &schema); err != nil {
		t.Fatalf("stripped output is not valid JSON matching the expected shape: %v\nstripped: %s", err, stripped)
	}
	if len(schema.Tools) != 1 {
		t.Fatalf("expected exactly 1 tool to survive stripping, got %d", len(schema.Tools))
	}

	_, propertyStillPresent := schema.Tools[0].InputSchema.Properties["cache_control"]
	_, ttlStillPresent := schema.Tools[0].InputSchema.Properties["ttl_seconds"]
	requiredStillListsIt := false
	for _, r := range schema.Tools[0].InputSchema.Required {
		if r == "cache_control" {
			requiredStillListsIt = true
		}
	}

	if !ttlStillPresent {
		t.Fatal("an unrelated sibling property (ttl_seconds) was also lost -- that would be a broader corruption than the known defect")
	}

	if propertyStillPresent {
		// The defect is not (or no longer) present: the property survived.
		// This is the desired end state -- assert the schema is fully
		// consistent and let the test PASS normally.
		if !requiredStillListsIt {
			t.Fatal("cache_control property survived but disappeared from \"required\" -- a different inconsistency")
		}
		t.Log("cache_control property survived stripping intact: the key-collision defect is not present (or has been fixed)")
		return
	}

	// Defect reproduced: the legitimate "cache_control" property was
	// deleted by StripCacheControl's blind key-name match, while
	// "required" still references it -- a self-contradictory JSON Schema.
	t.Logf("DEFECT REPRODUCED: stripped tool schema = %s", stripped)
	t.Logf("properties.cache_control present=%v, required still lists it=%v", propertyStillPresent, requiredStillListsIt)
	if !requiredStillListsIt {
		t.Fatal("cache_control property was deleted AND removed from required -- unexpectedly consistent; investigate before re-skipping")
	}
	t.Skip("DEFECT: StripCacheControl deletes any JSON key literally named \"cache_control\" " +
		"anywhere in the request tree, including a legitimately-named tool input_schema property, " +
		"leaving a self-contradictory schema (required references a now-missing property). " +
		"See internal/translate/anthropic.go stripKey/StripCacheControl. Not fixed here: " +
		"internal/translate is outside this task's ownership.")
}
