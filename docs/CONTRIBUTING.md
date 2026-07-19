# Contributing

Thanks for looking at `claude-code-router`. This document covers building,
testing, extending the two data/scenario-driven test suites
(`test/helixqa/`, `test/challenges/`), the full set of test tiers under
`test/` and `internal/`, and what a review here expects. For the release
process (versioning, tagging, publishing) see
[`RELEASE.md`](RELEASE.md); for day-to-day usage see
[`../README.md`](../README.md) and [`USER_GUIDE.md`](USER_GUIDE.md).

## Clean-room constraint — read this before touching `internal/translate` or `internal/gateway`

This project is a **clean-room** Go reimplementation of
[`musistudio/claude-code-router`](https://github.com/musistudio/claude-code-router)
(Node.js, MIT — see [`../NOTICE`](../NOTICE) and
[`../LICENSE-UPSTREAM-MIT`](../LICENSE-UPSTREAM-MIT)). Concretely:

- **Never copy code (or a close paraphrase of code) from the upstream Node
  repository.** Behaviour is reproduced from *observed behaviour* — wire
  formats, CLI grammar, config layout, default ports — never from reading
  upstream's source and transliterating it. `test/PORTING-MATRIX.md`
  documents this precisely for the test suite: each row is a PORTED
  behavioural claim, a GAP (a real, `t.Skip`-marked Go test naming what's
  missing), or an N/A (an upstream-only subsystem this project doesn't
  implement, with the reason stated).
- If you're closing a GAP row, write the Go test from the *behavioural
  description* in `PORTING-MATRIX.md`, verify it against this repo's own
  running code, and update that row from GAP to PORTED in the same change.
- Keep the upstream attribution in `NOTICE` intact. Don't add a top-level
  `LICENSE` file without checking with a maintainer first — see
  `README.md` "Upstream attribution" for why the Go implementation's own
  licence is presently marked "to be confirmed."

## Build

```bash
make build              # bin/ccr for the host OS/ARCH
# or directly:
go build -o bin/ccr ./cmd/ccr
```

`make cross-compile` builds all six linux/darwin/windows × amd64/arm64
combinations into `dist/` (see [`../Makefile`](../Makefile), `make help`
for the full target list).

## Test

```bash
make test              # go test ./...
make test-race         # go test -race ./...      (required before any PR)
make test-short        # go test -short ./...
make fuzz              # every FuzzXxx func, 10s each (override: FUZZTIME=30s)
make bench             # every BenchmarkXxx func, -benchmem
make cover             # coverage profile + total percentage
make lint              # gofmt -l (fails on any output) + go vet
make all               # lint + test-race — the local release gate, run this before every PR
```

Or run `scripts/preflight.sh` directly — it's the same gate (gofmt, `go
vet`, `go build`, `go test -race`, plus a secret-pattern scan) in one
script, meant to be wired into a local git hook:

```bash
scripts/preflight.sh            # full gate
scripts/preflight.sh --fast     # skip go test -race for a quick local check
```

**There is no hosted CI** (see [`RELEASE.md`](RELEASE.md) "No-CI
constraint") — `make all` / `scripts/preflight.sh` passing locally, on your
own machine, before you open a PR *is* the gate. Nothing else runs it for
you.

### Running a single package or test

```bash
go test ./internal/translate/...
go test -run TestBankCases ./test/helixqa/...
go test -run 'TestChallenge_EmptyMessagesArray' ./test/challenges/...
```

## Test tiers present in `test/` (and co-located under `internal/`)

This project layers several distinct kinds of test, each answering a
different question. When adding a behaviour, ask which tier(s) it actually
belongs in — most PRs only touch one or two.

| Tier | Where | Question it answers |
|---|---|---|
| Unit / table-driven | `internal/*_test.go` (e.g. `internal/translate/anthropic_test.go`) | Does this function do what its doc comment says, case by case? |
| Ported-behaviour ("port") | `internal/**/*_port_test.go` (9 files) | Does this match a specific upstream Node CCR behaviour recorded in `test/PORTING-MATRIX.md`? Each file's header comment names the exact upstream test file(s) it ports. |
| Property-based | `internal/**/*_prop_test.go` (3 files: `translate`, `gateway`, `config`) | Does an invariant hold across many generated inputs, not just hand-picked cases? |
| Fuzz | `internal/**/*_fuzz_test.go` (4 `FuzzXxx` funcs) | Does *any* byte sequence crash this function? Run via `make fuzz`; the corpus lands in the build cache, never in `testdata/`, unless a run actually finds a crasher. |
| Benchmark | `internal/**/*_bench_test.go` (`BenchmarkXxx` funcs) | Is this fast enough, and did a change regress it? Run via `make bench`. |
| Cross-package integration | `test/` (package `test`; e.g. `gateway_race_test.go`) | Does the assembled gateway (compression + routing + translation, streaming and non-streaming) behave correctly and race-free end to end? |
| Declarative data bank | `test/helixqa/` | Does `translate.AnthropicToOpenAI` (optionally composed with `StripCacheControl`) produce the right output for a *declared* Anthropic-shaped input — addable with **zero Go code**? See below. |
| Adversarial challenges | `test/challenges/` | Does the router degrade *safely* (clean success or clean typed error — never a panic, never silent corruption) under a specific hostile/edge-case scenario? See below. |
| Chaos | `test/chaos/` | Does the gateway survive misbehaving *network* conditions — truncated bodies, mid-stream hangs, malformed SSE, oversized headers/bodies, connection refused, DNS failure? |
| Security | `test/security/` | Do API keys/proxy credentials never leak into logs or error strings; is CRLF header injection blocked at the wire; are SSRF-shaped schemes (`file://`, `gopher://`, ...) rejected by config validation; are body-size caps enforced? |
| Mutation | `test/mutation/` | Would the *existing* suite actually catch a specific hand-picked regression? A deterministic harness copies `internal/` into a `t.TempDir()`, applies one source mutation to the copy only, and asserts `go test ./internal/...` fails against it. A mutation that *survives* (tests still pass) is a real coverage hole — treat it as a finding, not noise, and either add the missing assertion or explain in the harness why it's expected to survive. |

Current scale, as a sanity check when you add to a tier (re-verify with the
commands shown — these are not hardcoded gates, just what was true when
this document was written): `test/helixqa/banks/` had 12 bank files (≥60
cases enforced by `runner_test.go`'s own floor); `test/challenges/` had 14
`TestChallenge_*` functions (≥12 enforced by `run_challenges_test.go`'s own
floor).

## Adding a HelixQA bank case

`test/helixqa/` is data-driven: a new case, or a whole new bank file, needs
**no Go code change** — `runner_test.go` globs every `banks/*.json` file at
test time.

1. Pick (or create) a `banks/<topic>.json` file. Every file needs
   `version`, `name`, and a non-empty `test_cases` array — see
   `test/helixqa/bank_schema.json` for the full, mechanically-enforced
   shape (a malformed bank is a loud `go test` failure, never a silently
   skipped case).
2. Add a case object:
   ```json
   {
     "id": "my-topic-01-short-description",
     "description": "What this case proves, in one sentence.",
     "category": "my_topic",
     "tags": ["my_topic"],
     "options": { "clean_cache": false },
     "input": { "model": "m", "messages": [{ "role": "user", "content": "hi" }] },
     "expect_error": false,
     "expect": { "messages": [{ "role": "user", "content": "hi" }] }
   }
   ```
   - `id` must match `^[a-z0-9][a-z0-9-]*$` and be unique across **every**
     bank file (cross-file collisions fail the suite).
   - Either set `"expect_error": true` (optionally with `error_contains`),
     or `"expect_error": false` with an `expect` object — see
     `test/helixqa/types.go`'s `Expect`/`MessageExpect`/`ToolExpect` for
     every assertion field available (all optional; a case only asserts
     what it cares about, never "and nothing else").
   - Set `"options": {"strip_cache_control_first": true}` if the case
     should model the real two-stage `cleancache` pipeline
     (`StripCacheControl` then `AnthropicToOpenAI`) rather than conversion
     alone — see the existing `banks/cache_control.json` for worked
     examples at every nesting level.
3. Run it:
   ```bash
   go test -run TestBankCases -v ./test/helixqa/...
   ```
   Check the logged per-category case counts to confirm your case was
   picked up, and that the total still clears `runner_test.go`'s floor.

## Adding a challenge

`test/challenges/` scenarios are plain Go tests that self-register into a
package-level registry so `run_challenges_test.go` can print one aggregate
report and enforce a minimum-count floor. Follow the existing shape (e.g.
`test/challenges/empty_messages_array_test.go`) exactly:

1. Create `test/challenges/<your_scenario>_test.go`, `package challenges`.
2. In an `init()`, call `registerChallenge` with a `ChallengeMeta`:
   `ID` (kebab-case, unique), `TestName` (must exactly match your test
   function's name), `Hypothesis` (the safe behaviour you're asserting),
   `ExpectedSafeOutcome` (what "safe" concretely means here — a clean
   success, or a clean, well-typed error). Set `Defect` only if the
   challenge is exposing a **real** bug you're documenting rather than
   proving the code already handles the case safely — mirror the `t.Skip`
   convention used elsewhere in this repo for that (name the defect
   explicitly, don't just weaken the assertion).
3. Write `func TestChallenge_<Name>(t *testing.T)` with the actual
   hostile/edge-case input and assertions. Sub-`t.Run` for multiple angles
   on the same hypothesis is fine and common (see the example file: two
   sub-tests, empty-messages-with and -without a system prompt).
4. Run it:
   ```bash
   go test -run 'TestChallenge_YourScenario' -v ./test/challenges/...
   go test -run TestRunChallenges -v ./test/challenges/...   # full aggregate report
   ```

A challenge that reveals real broken behaviour is exactly as valuable as
one that proves safety — write it, mark `Defect`, and open the fix as a
separate, reviewable change rather than silently patching around it in the
same commit.

## Code-review expectations

Reading this repository's own commit history (`git log`) is the best guide
to the standard actually enforced here — a few patterns recur across every
commit and are worth naming explicitly:

- **Reproduce before you fix, and say so.** Several fixes in this history
  (`cd5af80`, `3f4a8e8`, `ad3f644`) were found by a test or fuzzer, then
  independently reproduced against the real code *before* the fix landed,
  with the before/after behaviour spelled out in the commit message. A PR
  that says "fixes a bug" without showing the reproducing input and the
  actual before/after output is under-evidenced for this repo's standard.
- **Root-cause, don't surface-patch.** `cd5af80` didn't just handle the one
  fuzzer-reported input; it traced the defect to `json.Unmarshal`-into-`any`
  silently lossy-converting every numeric literal, and fixed the decode
  strategy, not the symptom.
- **State what you verified, not just what you changed.** Every commit in
  this history ends with a concrete verification claim ("all N packages
  pass `go test ./... -race`; gofmt and go vet clean", specific measured
  before/after values, mutation kill counts). Match that: your PR
  description should say what you ran and what it showed, not just what
  you edited.
- **Never weaken a test to make it pass.** If a test's assertion looks
  wrong once you understand the real behaviour, fix the assertion in a way
  that's *more* precise, not less, and say why in the commit (see
  `cd5af80`'s note on the fuzz test itself having encoded the bug as
  spec). Deleting or loosening an assertion to turn red green is not
  acceptable here.
- **A known gap is a `t.Skip("GAP: ...")` or `Defect`, never silence.**
  `test/PORTING-MATRIX.md` and `test/challenges/`'s `Defect` field exist
  specifically so missing/broken behaviour is visible and searchable, not
  swept under an untested code path.
- **No secret ever reaches a log line or an error string.** This is
  load-bearing enough to have its own dedicated tests
  (`test/security/apikey_leak_test.go`, and the proxy/gateway error-path
  tests) and its own `scripts/preflight.sh` gate. If your change adds a
  new place credentials flow through (a new upstream client, a new proxy),
  add a corresponding leak test in the same PR.
- **`gofmt` clean, `go vet` clean, `-race` clean, on every commit** — not
  "will fix formatting later." Run `make all` (or `scripts/preflight.sh`)
  before pushing, every time.
- **Never edit generated/vendored output by hand**, and never hand-edit
  `dist/` or `bin/` output — both are build artifacts (`.gitignore`) and
  regenerating them is always the right fix if they look wrong.

If you disagree with one of these on a specific PR, say so explicitly in
the PR description rather than silently deviating — the point is a
consistent, legible standard across contributors (human or agent), not
rigid process for its own sake.
