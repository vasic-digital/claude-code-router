package helixqa

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// loadSchema reads and parses bank_schema.json into a generic tree suitable
// for validateSchema. A malformed schema file fails the whole suite loudly
// -- it is the gate every bank file must pass, so it must itself be valid.
func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("bank_schema.json")
	if err != nil {
		t.Fatalf("read bank_schema.json: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("bank_schema.json is not valid JSON: %v", err)
	}
	return schema
}

// TestBankSchemaIsWellFormed is a cheap smoke test that fails fast, with a
// clear message, if bank_schema.json itself has been broken.
func TestBankSchemaIsWellFormed(t *testing.T) {
	schema := loadSchema(t)
	for _, key := range []string{"type", "required", "properties"} {
		if _, ok := schema[key]; !ok {
			t.Errorf("bank_schema.json: top-level %q is missing", key)
		}
	}
}

// TestMalformedBankFailsSchemaValidation is a meta-test proving the schema
// gate is real, per the task's "a malformed bank is a loud failure rather
// than silently skipped cases" requirement: a case missing required fields
// must be rejected by validateSchema, not silently ignored.
func TestMalformedBankFailsSchemaValidation(t *testing.T) {
	schema := loadSchema(t)

	cases := map[string]string{
		"missing id/category/input/expect_error": `{"version":"1.0","name":"bad","test_cases":[{"description":"oops"}]}`,
		"unknown top-level property":             `{"version":"1.0","name":"bad","test_cases":[],"bogus":true}`,
		"unknown case property":                  `{"version":"1.0","name":"bad","test_cases":[{"id":"x","description":"d","category":"c","input":{},"expect_error":false,"bogus":1}]}`,
		"id with uppercase / invalid pattern":    `{"version":"1.0","name":"bad","test_cases":[{"id":"Bad_ID!","description":"d","category":"c","input":{},"expect_error":false}]}`,
		"empty test_cases array":                 `{"version":"1.0","name":"bad","test_cases":[]}`,
		"expect_error wrong type":                `{"version":"1.0","name":"bad","test_cases":[{"id":"x","description":"d","category":"c","input":{},"expect_error":"yes"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var doc any
			if err := json.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatalf("test fixture is not valid JSON: %v", err)
			}
			errs := validateSchema(schema, doc)
			if len(errs) == 0 {
				t.Fatalf("expected schema validation to reject this bank, got zero errors")
			}
			t.Logf("correctly rejected: %s", strings.Join(errs, "; "))
		})
	}
}

// TestBankCases loads every bank under banks/, validates it against
// bank_schema.json, then executes each declared case against
// translate.AnthropicToOpenAI (optionally composed with
// translate.StripCacheControl, see OptionsSpec.StripCacheFirst). Adding a
// new *.json file under banks/, or a new case inside an existing one,
// requires NO change to this file -- see README.md.
func TestBankCases(t *testing.T) {
	schema := loadSchema(t)

	entries, err := os.ReadDir("banks")
	if err != nil {
		t.Fatalf("read banks/: %v", err)
	}

	total := 0
	byCategory := map[string]int{}
	seenIDs := map[string]string{} // case id -> source file, catches cross-file collisions too

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join("banks", entry.Name())

		t.Run(entry.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s is not valid JSON: %v", path, err)
			}
			if errs := validateSchema(schema, doc); len(errs) > 0 {
				t.Fatalf("%s fails bank_schema.json validation:\n  %s", path, strings.Join(errs, "\n  "))
			}

			var bank Bank
			if err := json.Unmarshal(raw, &bank); err != nil {
				t.Fatalf("%s: decode: %v", path, err)
			}
			if len(bank.TestCases) == 0 {
				t.Fatalf("%s: bank declares zero test cases", path)
			}

			for _, c := range bank.TestCases {
				c := c
				if prev, dup := seenIDs[c.ID]; dup {
					t.Fatalf("duplicate case id %q (also defined via %s)", c.ID, prev)
				}
				seenIDs[c.ID] = path

				t.Run(c.ID, func(t *testing.T) {
					runCase(t, c)
				})
				total++
				byCategory[c.Category]++
			}
		})
	}

	if total == 0 {
		t.Fatal("no bank cases were loaded from banks/")
	}

	cats := make([]string, 0, len(byCategory))
	for k := range byCategory {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	t.Logf("helixqa bank runner: %d case(s) across %d categor(y/ies)", total, len(cats))
	for _, c := range cats {
		t.Logf("  %-24s %d case(s)", c, byCategory[c])
	}

	const minCases = 60
	if total < minCases {
		t.Fatalf("expected at least %d bank cases (task requirement), found %d", minCases, total)
	}
}

// runCase executes one bank case: it optionally runs the cleancache
// transformer pipeline (StripCacheControl -> re-decode), then always runs
// translate.AnthropicToOpenAI, and checks the outcome against the case's
// declared expectations.
func runCase(t *testing.T, c Case) {
	t.Helper()

	input := []byte(c.Input)

	if c.Options.StripCacheFirst {
		stripped, err := translate.StripCacheControl(input)
		if err != nil {
			if !c.ExpectError {
				t.Fatalf("StripCacheControl: unexpected error: %v", err)
			}
			if c.ErrorContains != "" && !strings.Contains(err.Error(), c.ErrorContains) {
				t.Fatalf("StripCacheControl error %q does not contain %q", err.Error(), c.ErrorContains)
			}
			return
		}
		input = stripped
	}

	var req translate.AnthropicRequest
	if err := json.Unmarshal(input, &req); err != nil {
		if !c.ExpectError {
			t.Fatalf("unexpected input decode error: %v\ninput: %s", err, input)
		}
		if c.ErrorContains != "" && !strings.Contains(err.Error(), c.ErrorContains) {
			t.Fatalf("decode error %q does not contain %q", err.Error(), c.ErrorContains)
		}
		return
	}

	opts := translate.Options{
		CleanCache:           c.Options.CleanCache,
		StreamOptions:        c.Options.StreamOptions,
		EnsureToolParameters: c.Options.EnsureToolParameters,
		Model:                c.Options.Model,
	}
	out, err := translate.AnthropicToOpenAI(&req, opts)

	if c.ExpectError {
		if err == nil {
			t.Fatalf("expected an error, got success: %s", mustJSON(t, out))
		}
		if c.ErrorContains != "" && !strings.Contains(err.Error(), c.ErrorContains) {
			t.Fatalf("error %q does not contain %q", err.Error(), c.ErrorContains)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v\ninput: %s", err, input)
	}
	if c.Expect == nil {
		return // case only asserts that conversion succeeds
	}
	if mismatches := matchExpect(out, c.Expect); len(mismatches) > 0 {
		t.Fatalf("%d mismatch(es):\n  %s\ngot: %s", len(mismatches), strings.Join(mismatches, "\n  "), mustJSON(t, out))
	}
}
