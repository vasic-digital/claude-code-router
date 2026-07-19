# helixqa test bank + challenges

Two complementary, hermetic (no network, no secrets, no real provider calls)
Go test suites for `internal/translate.AnthropicToOpenAI` and the router's
config/routing/proxy layers:

- **`test/helixqa/`** (this directory) — a data-driven test bank. JSON case
  files under `banks/` declare Anthropic-shaped requests and their expected
  OpenAI-shaped translation (or expected error); `runner_test.go` loads and
  executes every one of them. Adding a case, or a whole new bank file,
  requires **no Go code change**.
- **`test/challenges/`** (sibling directory) — adversarial "try to break the
  router" scenarios in the shape established by the `digital.vasic.challenges`
  module used elsewhere in this ecosystem: each challenge states a
  hypothesis and an expected *safe* outcome, then proves the code either
  upholds it or fails cleanly — never panics, never silently corrupts data.

Both suites are part of the normal Go module and run with plain `go test`;
there is no separate binary or runner to install.

## Why "helixqa"

The name deliberately matches the ecosystem-standard "HelixQA test-bank"
shape: a bank file keyed by `test_cases`, loadable by
`digital.vasic.challenges`' `pkg/bank` loader (which explicitly folds a
`test_cases` root key into its own `challenges` key "so HelixQA can use its
existing YAML test banks"). It does **not**, however, import that module —
see "Why not import `digital.vasic.challenges` directly" below.

Before writing this suite we verified what "HelixQA" actually is inside this
codebase's own ecosystem, rather than assuming the name implies a runner:
`claude_toolkit/submodules/LLMsVerifier/llm-verifier/pkg/helixqa/models.go`
is a **hardcoded vision-model quality/reliability/cost registry**
(`VisionModelRegistry()`), not a test-bank runner of any kind — it has no
loader, no case format, no execution engine. This confirms a prior audit's
finding: "HelixQA" in *this* codebase's submodule tree is not an existing
test-bank runner we could or should reuse; it is an unrelated, narrowly
scoped registry. `test/helixqa/` in claude-code-router is a fresh,
purpose-built test bank that merely follows the same *external* naming and
file-shape convention documented in `digital.vasic.challenges/pkg/bank`, so
it stays recognisable to anyone who has used HelixQA banks elsewhere.

### Why not import `digital.vasic.challenges` directly

`digital.vasic.challenges` (checked out at
`claude_toolkit/submodules/challenges/`) is a full-featured, heavier
framework: challenge registries with dependency ordering, an assertion
engine, Markdown/JSON/HTML reporters, live WebSocket monitoring, Prometheus
metrics, a plugin system, and a bridge to a separate `digital.vasic.containers`
module. Pulling it in as a Go dependency would:

- require network access to `go get` it and its transitive dependencies
  (MongoDB driver, JWT/LDAP/NTLM auth libraries, etc.) — this suite must
  build and run with **no network**;
- pull in functionality (container orchestration, live dashboards, metrics
  export) this router's test bank has no use for;
- couple `claude-code-router`'s test suite to a submodule of an entirely
  different project (`claude_toolkit`), which this task was explicitly
  scoped to leave untouched.

Instead, `test/helixqa/` and `test/challenges/` are small, self-contained
reimplementations of exactly the pieces they need: a JSON/YAML-shaped bank
loader (`types.go`), a hand-rolled JSON Schema draft 2020-12 *subset*
validator (`schema.go` — only the keywords `bank_schema.json` actually uses),
a subset-match assertion helper (`matcher.go`), and — for challenges — a
minimal self-registering test registry (`registry.go`) mirroring the shape
of `pkg/registry` + `pkg/runner` without the extra machinery. The file/field
naming (`test_cases`, `id`/`description`/`category`/`tags`, a challenge's
hypothesis + expected-safe-outcome shape) intentionally matches the
established conventions so the *ideas* transfer even though the Go code does
not import the module.

## Running

```bash
# Everything in both suites:
go test ./test/helixqa/... ./test/challenges/... -race -v

# Just the bank runner:
go test ./test/helixqa/... -race -v

# Just the challenges:
go test ./test/challenges/... -race -v

# One specific bank case or challenge by name:
go test ./test/helixqa/... -run 'TestBankCases/cache_control.json/cachectrl-07' -v
go test ./test/challenges/... -run 'TestChallenge_TenMegabyteRequestBody' -v
```

`gofmt -w .` and `go vet ./...` are expected to be clean for both
directories; `go build ./...` at the module root also covers them (a
pre-existing, unrelated build failure in `internal/gateway` — that package
is mid-edit by another workstream and references an as-yet-uncommitted
`messages.go` — is not something this suite touches, exercises, or depends
on; both suites only import `internal/config`, `internal/translate`,
`internal/router`, and `internal/proxy`).

## `test/helixqa/`: adding a bank case

1. Pick (or create) a `banks/<category>.json` file. Every file must match
   `bank_schema.json`:

   ```json
   {
     "version": "1.0",
     "name": "<bank name>",
     "description": "<optional>",
     "test_cases": [
       {
         "id": "kebab-case-unique-id",
         "description": "one sentence explaining what this pins down and why",
         "category": "<matches the category list below>",
         "tags": ["optional", "free-form", "labels"],
         "options": {
           "clean_cache": false,
           "stream_options": false,
           "ensure_tool_parameters": false,
           "model": "",
           "strip_cache_control_first": false
         },
         "input": { "...": "an Anthropic-shaped request body" },
         "expect_error": false,
         "error_contains": "optional substring, only checked when expect_error is true",
         "expect": { "...": "see 'the expect object', only used when expect_error is false" }
       }
     ]
   }
   ```

2. `id` must be lowercase kebab-case (`^[a-z0-9][a-z0-9-]*$`) and unique
   across **every** bank file, not just the one you're editing —
   `runner_test.go` fails loudly on a cross-file id collision.
3. `input` is decoded straight into `translate.AnthropicRequest` — write it
   exactly as Claude Code would send it over the wire.
4. Pick **one** of:
   - `"expect_error": true` (+ optional `"error_contains"`, checked as a
     substring against either a JSON-decode error or the error returned by
     `AnthropicToOpenAI`/`StripCacheControl`), or
   - `"expect_error": false` (+ optional `"expect"` — omit it entirely if
     the case only needs to prove the conversion *succeeds*, with no further
     assertions).
5. Run `go test ./test/helixqa/... -race -v` — a brand-new `banks/*.json`
   file, or a brand-new case inside an existing one, is picked up
   automatically; **no `.go` file needs to change.**

### The `options` object

Mirrors `translate.Options`, plus one extra knob modelling the real
two-stage pipeline a `"cleancache"` provider transformer runs:

| Field | Effect |
|---|---|
| `clean_cache` | Passed straight through as `translate.Options.CleanCache`. **Characterisation note:** this field is currently *not read anywhere* inside `AnthropicToOpenAI` itself — see `cachectrl-09` in `banks/cache_control.json`. The actual stripping mechanism is `translate.StripCacheControl`, run on the raw bytes by the caller. |
| `stream_options` | `translate.Options.StreamOptions` — adds `stream_options.include_usage` on streaming requests only. |
| `ensure_tool_parameters` | `translate.Options.EnsureToolParameters` — injects an empty object schema onto a tool with no `input_schema`. |
| `model` | `translate.Options.Model` — the router's model override. |
| `strip_cache_control_first` | Not part of `translate.Options` at all. When `true`, the runner calls `translate.StripCacheControl` on the raw JSON bytes and re-decodes the *stripped* result before converting — this is what actually exercises cache_control removal at every nesting level. |

### The `expect` object

Every field is a **subset match**: only fields you set are checked, so a
case only has to assert what it actually cares about. See
`bank_schema.json` for the exact shape, or any existing bank file for
examples. Highlights:

- `messages_count` / `tools_count` — exact counts.
- `messages` / `tools` — arrays matched **positionally** against the
  converted output; each element only checks the sub-fields you set
  (`role`, `content` / `content_contains` / `content_null`,
  `tool_calls_count`, `tool_call_names`, `tool_call_arguments_contains`,
  `tool_call_id` for messages; `name`, `description`, `parameters_object`,
  `parameters_absent` for tools).
- `temperature_null` / `top_p_null` / `stream_options_null` — assert a
  pointer field is `nil` (distinct from asserting it equals its zero value).

### Categories in this bank

| Category | What it covers |
|---|---|
| `system_prompt` | Anthropic's top-level `system` field (string + block-array forms), including empty/omitted/malformed shapes. |
| `multi_turn` | Multi-turn histories: ordering, role alternation (and its absence), mixed content shapes across turns. |
| `tool_use` | `tool_use` blocks → `tool_calls`, including empty/omitted `input`, `ensure_tool_parameters`. |
| `tool_result` | `tool_result` blocks → `role:tool` messages, ordering relative to other content, `is_error` (currently inert). |
| `parallel_tool_calls` | Multiple `tool_use`/`tool_result` blocks in a single turn. |
| `empty_null_content` | Empty string / empty array / JSON `null` / omitted-key content, and the (verified, real) "turn silently disappears" behaviour those all share. |
| `unicode_emoji` | CJK, RTL (Arabic/Hebrew), ZWJ emoji sequences, combining diacritics, unicode in `stop_sequences` and model ids. |
| `long_content` | 10KB–100KB content strings, proving no truncation. |
| `cache_control` | `cache_control` at every documented nesting level, both stripped (`strip_cache_control_first`) and left in place. |
| `sampling_params` | `temperature` / `top_p` (incl. the nil-vs-zero distinction) and `stop_sequences` → `stop`. |
| `streaming` | `stream` + `stream_options.include_usage`, including the "never on a non-streaming request" rule. |
| `malformed` | Decode-level type errors, structurally invalid content, and characterisation cases for inputs that are unusual but currently tolerated. |

## `test/helixqa/README.md` also covers `test/challenges/`

### Adding a challenge

1. Create `test/challenges/<short_name>_test.go`, `package challenges`.
2. Write one (or a small `t.Run` family of) `func TestChallenge_<Name>(t *testing.T)`.
3. In an `init()` in the same file, call `registerChallenge(ChallengeMeta{...})`
   with:
   - `ID` — short kebab-case id.
   - `TestName` — the exact Go function name (for cross-referencing with
     `go test -list`).
   - `Hypothesis` — what you expect the code to do and *why* (cite the
     specific function/behaviour you're relying on).
   - `ExpectedSafeOutcome` — what "safe" concretely means here: a clean
     success, or a clean, specific error — never a panic, never silent data
     loss/corruption.
   - `Defect` — leave empty for a "handles cleanly" proof. Only set it when
     the test below actually demonstrates a real defect (see next section).
4. `go test ./test/challenges/... -race -v` runs it automatically — Go's own
   test discovery finds every `TestChallenge_*` function, and
   `run_challenges_test.go`'s `TestRunChallenges` additionally asserts a
   completeness floor (≥ 12 challenges) and prints one summary line per
   challenge from the registry.

### When a challenge finds a real defect

Per the task rules: **do not paper over it.** Reproduce it with real
assertions first (so the finding has hard evidence in the test log even
though the test doesn't hard-fail CI), then call
`t.Skip("DEFECT: <one-line description>")`. See
`cache_control_key_collision_defect_test.go` for the pattern used here: it
actually runs `translate.StripCacheControl`, decodes the result, and only
calls `t.Skip` *after* confirming the corruption really happened — if the
underlying code is later fixed, this test starts passing on its own (the
`t.Skip` branch is simply never reached), with no test-code change required.

### Current findings

- **DEFECT** (`cache-control-key-collision`, `test/challenges/cache_control_key_collision_defect_test.go`):
  `translate.StripCacheControl` deletes *any* JSON object key literally
  named `cache_control`, anywhere in the request tree, with no awareness of
  JSON path. A tool `input_schema` that legitimately defines a property
  named `cache_control` (e.g. a caching-policy parameter) has that property
  silently deleted, while a `"required":["cache_control"]` entry elsewhere
  in the same schema is left dangling — a self-contradictory JSON Schema
  produced with no error and no panic. `internal/translate` was outside
  this task's ownership, so this is reported rather than fixed.
- **Soft finding** (not a defect, documented in
  `test/challenges/conflicting_transformers_test.go` and
  `banks/cache_control.json` case `cachectrl-09`): `config.Validate` never
  whitelists `Transformer.Use` entries, so a misspelled transformer name
  (e.g. `"cleancach"` instead of `"cleancache"`) is silently accepted and
  has zero effect — an operator's typo produces a config that *looks* valid
  but doesn't behave as intended, with no warning anywhere.
- Several `malformed`/`empty_null_content`/`tool_use` bank cases are
  **characterisations**, not bugs: e.g. an entirely empty/null/omitted
  message content silently drops the whole turn rather than erroring, and
  `tool_use.input` is never validated to actually be a JSON object before
  being forwarded as `arguments`. Neither crashes or corrupts adjacent
  data; both are pinned down explicitly (search either directory for the
  `characterisation` tag) so future changes can't silently alter this
  behaviour without a test noticing.

## Hermeticity

Neither suite makes a real network call. `test/challenges/` uses
`httptest.Server` (bound to `127.0.0.1`, closed at the end of each test) for
every scenario that needs to observe what actually reaches "an upstream" —
the 10MB-body challenge and the query-string/fragment challenge. No API key,
token, or other credential appears anywhere in either suite.
