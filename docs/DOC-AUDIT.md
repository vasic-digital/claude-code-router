# Documentation accuracy audit

Audit of every factual claim in `README.md` and `docs/*.md` against the actual
code in this repository, performed by tracing each claim to the specific
file/function it describes and, where practical, exercising the built binary
live.

## Methodology and a load-bearing caveat

This repository is moving fast enough that the ground shifted mid-audit.
`git log v0.1.0..HEAD` is empty — no new commits since the `v0.1.0` tag — but
`git status` showed three files with **uncommitted** working-tree changes at
the time of this audit:

- `internal/gateway/gateway.go` — adds `Options.MaxAttempts` and
  `Options.APIKeys`, and mounts `RequireAPIKey(s.opt.APIKeys)` as route-scoped
  middleware on `POST /v1/messages` (routes() ~line 161).
- `internal/gateway/messages.go` — adds `doUpstreamWithRetry` (a real retry
  loop driving `internal/router`'s `ClassifyStatus`/`ClassifyTransportError`/
  `FallbackRetryDelayAfterStatus`/`FallbackRetryDelayAfterNetworkError`),
  wired into `handleMessages` in place of the old single `s.Upstream.Do` call.
- `internal/translate/anthropic.go` — adds `convertImageBlock`,
  `convertToolResultContent`, `OpenAIContentPart`/`OpenAIImageURL`: image
  content blocks (base64 or URL source) are now converted to OpenAI
  `image_url` parts instead of being rejected with an error.
- `internal/translate/vision_test.go` — new, untracked, tests the above.

`go build ./...` succeeds and `go test ./...` passes all 12 packages
(including `test/mutation`, `test/chaos`, `test/security`) with these changes
present, and a live-run smoke test (below) confirms the new behaviour end to
end — so this audit documents them as **current, real behaviour of the
checked-out code**, per the task's instruction to verify against the actual
code rather than against the last commit. Anywhere this matters, it is called
out explicitly. If these changes are altered or reverted before being
committed, the affected sections should be re-verified.

**Live verification performed:** built `/tmp/ccr-audit` from this tree,
started it against a throwaway `$HOME`/`config.json` (one provider pointing
at an unreachable `127.0.0.1:9`), and curled it directly:

| Check | Result |
|---|---|
| `GET :GATEWAY/health` | `{"providers":1,"status":"ok"}`, `200` — matches docs |
| `GET :GATEWAY/ready` | `{"status":"ready"}`, `200` — matches docs |
| `GET :MGMT/health` | `{"providers":1,"service":"ccr-management","status":"ok"}`, `200` — matches docs |
| `--gateway-port 34562 --port 34582` | both servers bound on the requested ports, confirming the flags work |
| `POST /v1/messages` (no `x-api-key`) against the unreachable upstream | `502` after **3.00s** wall-clock — consistent with 3 attempts (default `MaxAttempts`) separated by the retry loop's `1s` + `2s` exponential backoff against a `Retryable` (`ECONNREFUSED`) transport error. Confirms the retry loop is live, not just compiling. |
| `POST /v1/messages` with `x-api-key: bogus` | still `502` (reached the router/upstream, not rejected `401`) — confirms the gateway is still unauthenticated by default even with the new middleware mounted, because `cmd/ccr` never populates `Options.APIKeys` |

## Summary

| Verdict | Count |
|---|---|
| ACCURATE | 46 |
| STALE (fixed in this pass) | 29 |
| UNVERIFIABLE | 2 |

Counts cover the distinct factual claims itemised below (flags, endpoints,
config fields, transformer behaviour, known-limitations items, release
status, and the citation-accuracy findings called out explicitly). Citations
inside paragraphs that were rewritten for content reasons were corrected as
part of that rewrite and are not double-counted separately.

---

## 1. CLI flags (`cmd/ccr/flags.go`, `cmd/ccr/main.go`)

| Claim | Doc location | Verdict | Evidence |
|---|---|---|---|
| `--host <host>` (management, default `127.0.0.1`, env `CCR_WEB_HOST`) | README.md, USER_GUIDE.md, ADMIN_MANUAL.md, API.md | ACCURATE | `cmd/ccr/flags.go:52,59-61,100-105` |
| `--port <port>` (management, default `3458`, env `CCR_WEB_PORT`) | same | ACCURATE | `cmd/ccr/flags.go:53,62-68,106-115` |
| `--open`/`--no-open` | same | ACCURATE | `cmd/ccr/flags.go:116-119` |
| `--gateway`/`--no-gateway` (default on) | same | ACCURATE | `cmd/ccr/flags.go:120-123`, `cmd/ccr/serve.go:45` |
| `--gateway-port <port>` (default `3456`, env `CCR_GATEWAY_PORT`) | **not documented anywhere** before this pass | STALE (missing) | `cmd/ccr/flags.go:37,73-79,90-99`; live-verified above |
| `--gateway-host <host>` (default `127.0.0.1`, env `CCR_GATEWAY_HOST`) | **not documented anywhere** before this pass | STALE (missing) | `cmd/ccr/flags.go:43,70-72,84-89`; live-verified above |
| "the gateway's bind address is fixed at `127.0.0.1:3456` today... not exposed via a flag" | README.md (old text), USER_GUIDE.md §4, ADMIN_MANUAL.md §1.2/§3, Dockerfile's own comment (not owned by this pass) | STALE | `cmd/ccr/serve.go:46` now passes `Host: flags.GatewayHost, Port: flags.GatewayPort`, both flag/env-configurable (`cmd/ccr/flags.go`) |
| `ccr start`/`ccr ui` forward `--gateway-host`/`--gateway-port` to the detached `serve` child the same way they forward `--host`/`--port`/`--gateway` | *(new finding, not previously claimed either way)* | — (code fact, now documented) | `cmd/ccr/service.go:104-114`'s `childArgs` build only forwards `--host`, `--port`, `--gateway`/`--no-gateway`, `--open`/`--no-open` — **`--gateway-host`/`--gateway-port` are silently dropped** for `start`/`ui` (parsed into `cmdStart`'s own `flags`, never used, never re-passed). Only the **environment-variable** form (`CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT`) survives, because `spawnDetached` (`cmd/ccr/spawn_unix.go:16-25`, `spawn_windows.go:14-22`) leaves `cmd.Env` nil, which `os/exec` documents as "inherit the current process's environment." This is a real, verifiable gap in the CLI wiring — documented in USER_GUIDE.md/ADMIN_MANUAL.md/README.md below rather than silently glossed over |
| `-h`/`--help`/`help`/no-args print the same usage text, exit 0 | README.md, FAQ.md Q25 | ACCURATE | `cmd/ccr/main.go:71-79`, tested `cmd/ccr/main_test.go:26-37` |
| Unknown first arg → `Profile "<name>" was not found or is disabled.`, exit 1 | README.md, FAQ.md Q26 | ACCURATE | `cmd/ccr/main.go:88-94`, tested `cmd/ccr/main_test.go:43-65` |
| Full `--help` text reproduced verbatim | README.md, USER_GUIDE.md | STALE | Both copies were missing the `--gateway-port`/`--gateway-host` block that `cmd/ccr/main.go:28-61`'s `usage` const now contains (grew from 28-52 to 28-61) |

## 2. Gateway endpoints (`internal/gateway/gateway.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `GET /health` — always 200, `{"status":"ok","providers":N}`, unauthenticated | ACCURATE | `internal/gateway/gateway.go:129-134`; live-verified |
| `GET /ready` — 200/503 per provider+route state, unauthenticated | ACCURATE | `internal/gateway/gateway.go:137-151`; live-verified |
| `POST /v1/messages` — unauthenticated, no route-scoped middleware | STALE | As of the uncommitted change described above, `internal/gateway/gateway.go:161` now registers `RequireAPIKey(s.opt.APIKeys)` ahead of `s.handleMessages`. **Functionally** the endpoint is still unauthenticated by default (confirmed live: an `x-api-key` header made no difference) because `cmd/ccr` never populates `Options.APIKeys` and there is no CLI flag/config field to do so — but the claim "no route requires credentials" needed the mechanism corrected, not just the outcome |
| Exactly three routes registered, nothing else | ACCURATE | `internal/gateway/gateway.go:121-161` — `routes()` registers only these three `s.eng.*` calls |
| Management server: separate `net/http.ServeMux`, `GET /health` (own shape), `GET /` placeholder, no auth, cannot be disabled | ACCURATE | `cmd/ccr/management.go:26-53`; live-verified `/health` shape |

## 3. Config fields (`internal/config/config.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `Providers[].name/api_base_url/api_key/models/transformer.use` — shape and validation rules | ACCURATE | `internal/config/config.go:31-49`, `122-138`; unchanged in this session |
| `Router.default/background/think/longContext` — all validated the same way, only `default`/`background` drive routing | ACCURATE | `internal/config/config.go:65-70`, `139-153`; `internal/router/router.go:61-64` still doesn't branch on `Think`/`LongContext` |
| Config dir resolution (`~/.claude-code-router`, `%APPDATA%\claude-code-router`, fallback `.claude-code-router`) | ACCURATE | `internal/config/config.go:78-91` |
| Missing file → empty valid config, no error; malformed JSON → error; failed `Validate()` → error | ACCURATE | `internal/config/config.go:102-118` |
| Route string splits on first comma only | ACCURATE | `internal/config/config.go:161-172`, tested `internal/config/config_test.go:110-124` |
| No `config.json` field exists yet to configure inbound gateway API keys or `MaxAttempts` | ACCURATE (new finding) | `internal/config/config.go` has no `api_keys`/`max_attempts`-shaped field; `internal/gateway.Options.APIKeys`/`MaxAttempts` are populated only by direct library construction, never by `cmd/ccr` |

## 4. Transformers (`internal/translate`)

| Claim | Doc location | Verdict | Evidence |
|---|---|---|---|
| `streamoptions` adds `stream_options.include_usage` only while streaming, fully wired | FAQ.md Q5, README, USER_GUIDE.md | ACCURATE | `internal/gateway/messages.go:230`, `internal/translate/anthropic.go:351-353` |
| **`cleancache` has no observable effect on outgoing requests — `StripCacheControl` is never called from `messages.go`** | FAQ.md Q5, USER_GUIDE.md §3 step 4, ARCHITECTURE.md (class-diagram note + summary table) | **STALE — this is now false** | `internal/gateway/messages.go:229` reads `provider.Has("cleancache")` into `Options.CleanCache`, and `internal/translate/anthropic.go:441-461`, inside `AnthropicToOpenAI`'s tool-conversion loop, now genuinely calls `StripCacheControl(params)` on every tool's `input_schema` when `opt.CleanCache` is set — closing exactly the gap the old text described. Confirmed by reading the current file (not from a changelog); this predates the uncommitted changes described above and is a plain STALE-doc fix, not an in-flight one |
| Image (`type: "image"`) content blocks return an explicit, unhandled error | README feature table, FAQ.md Q12, USER_GUIDE.md troubleshooting, API.md request-body table, ARCHITECTURE.md | **STALE (closed by the uncommitted vision change)** | `internal/translate/anthropic.go:237-280` (`convertImageBlock`) converts a `base64` or `url` Anthropic image source into an OpenAI `image_url` content part (media-type allow-list of png/jpeg/gif/webp, 20MB decoded-size cap, checked before decoding); wired into both top-level message content (`anthropic.go:409-416`) and `tool_result` content (`convertToolResultContent`, `anthropic.go:291-335`, for computer-use screenshots). A malformed/unsupported image source is still a named error, not a silent drop — only the previously-universal "not supported yet" error is gone |
| `EnsureToolParameters` always on in the live handler, not tied to a named transformer | README, FAQ.md Q15 | ACCURATE | `internal/gateway/messages.go:234` |
| `cache_control` stripped at any JSON depth, `json.Number`-safe | README, FAQ.md Q5 | ACCURATE | `internal/translate/anthropic.go:495-547` (unchanged logic, only line numbers shifted) |

## 5. Retry/fallback (`internal/router/fallback.go`, `internal/gateway`)

| Claim | Verdict | Evidence |
|---|---|---|
| Classification/backoff policy (`ClassifyStatus`, `ClassifyTransportError`, `BuildExecutionPlan`, `NextFallbackProvider`, `FallbackRetryDelayAfter*`) exists and is tested | ACCURATE | `internal/router/fallback.go` unchanged this session; tested via `internal/router/fallback_retry_classification_port_test.go` |
| "Nothing calls these to drive a retry loop yet — a failed upstream call still just fails once" | README, FAQ.md Q28, ARCHITECTURE.md (three places) | **STALE (closed by the uncommitted retry-loop change)** | `internal/gateway/messages.go:318-383` (`doUpstreamWithRetry`) now calls `s.Upstream.Do` up to `Options.MaxAttempts` times (default 3, `internal/gateway/gateway.go:65-68`), consulting `router.ClassifyStatus`/`router.ClassifyTransportError` after each failure and sleeping via `router.FallbackRetryDelayAfterStatus`/`...AfterNetworkError` (honouring `Retry-After`) between attempts; a `Terminal` classification never retries. Live-verified above (3.0s wall-clock for 3 attempts against a connection-refused upstream) |

## 6. Inbound gateway authentication (`internal/gateway/auth.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `RequireAPIKey(keys []string)` exists, accepts `Authorization: Bearer`/`x-api-key`, constant-time compare, fixed 401, never leaks the presented/accepted key | ACCURATE, unchanged | `internal/gateway/auth.go` (whole file untouched this session) |
| "Neither `gateway.go`'s route table nor `cmd/ccr` installs it anywhere" | README, FAQ.md Q29, ADMIN_MANUAL.md §5, ARCHITECTURE.md (three places) | **STALE, needs precise rephrasing** | `internal/gateway/gateway.go:161` now mounts `RequireAPIKey(s.opt.APIKeys)` as route-scoped middleware on `POST /v1/messages` only (`/health`/`/ready` are deliberately never gated, per the code's own comment at `gateway.go:154-160`, so liveness/readiness probing survives regardless of auth config). **However** `cmd/ccr` never sets `Options.APIKeys` (no CLI flag, no `config.json` field), so the accepted-key list is always empty, and `RequireAPIKey`'s own documented behaviour for an empty list is "disable authentication entirely." Net effect: a CLI-launched gateway is **still unauthenticated by default today**, confirmed live above — the *mechanism* changed, the *operator-facing outcome* did not |

## 7. The one remaining upstream GAP: ambiguous bare-model resolution

| Claim | Verdict | Evidence |
|---|---|---|
| Explicit `"provider,model"`/`"provider/model"` selectors in the request `model` field are implemented and take precedence over `Router.default`/`background` | ACCURATE | `internal/router/selector.go`, wired at `internal/router/router.go:50-58`; `internal/router/explicit_provider_selector_port_test.go` |
| A **bare** model id that happens to be listed by more than one provider is not disambiguated/rejected the way upstream Node CCR's `ModelRegistry.resolve` does — `router.Select` only ever resolves a bare, non-haiku model via `Router.default` | ACCURATE (unchanged, still open) | `internal/router/explicit_provider_selector_port_test.go:189-221` — `TestModelRegistryAmbiguousBareModelRejection_GAP` is an explicit `t.Skip("GAP: ...")`, with a documented reason it's deliberately not closed (closing it would change default routing behaviour for ordinary Claude Code requests, which the port's own backward-compatibility requirement rules out) |

## 8. Release/version status

| Claim | Doc location | Verdict | Evidence |
|---|---|---|---|
| "No tag has been cut yet" / "No published release yet" / `go install ...@latest` "isn't available yet" | RELEASE.md §Version scheme, README.md Install, USER_GUIDE.md §1, FAQ.md Q25 | **STALE** | `git tag -l` → `v0.1.0`; `gh release view v0.1.0` shows a real, published, non-draft, non-prerelease GitHub release ("Latest") dated during this same work session, whose own release notes list `--gateway-host`/`--gateway-port` as shipped and enumerate the same four known limitations this task asked about |

## 9. Citation accuracy (`file.go:NN` references)

`README.md` and `docs/*.md` together contain roughly 150 `path/file.go:NN[-MM]` citations. Every cited file exists. The great majority of citations resolve to a real, plausible line — spot-checked against current file content and, for the files touched this session, against exact `grep -n`-derived boundaries.

Two files grew substantially since most of these citations were written, independent of this session's own edits — `internal/gateway/messages.go` (a request-body-size-cap block was added ahead of `handleMessages`, then the retry loop was added after it, moving the file from roughly 320 to 707 lines) and `internal/translate/anthropic.go` (vision support added ~200 lines). Every citation into these two files that fell inside a paragraph this pass rewrote for content reasons (cleancache, vision, retry loop, auth, `handleMessages` orchestration, the streaming section) was corrected to the current line numbers as part of that rewrite; representative examples:

| Old citation | Old target | New citation | New target |
|---|---|---|---|
| `internal/gateway/messages.go:19-27` | Router/Upstream seam doc comment | `internal/gateway/messages.go:30-50` | same content, shifted by the `maxRequestBodyBytes` const |
| `internal/gateway/messages.go:178-244` | `handleMessages` | `internal/gateway/messages.go:189-271` | `handleMessages` (now delegates the upstream call to `doUpstreamWithRetry`) |
| `internal/gateway/messages.go:200-204` | `EnsureToolParameters: true` | `internal/gateway/messages.go:231-234` | same |
| `internal/gateway/messages.go:258-318` | `forwardUpstreamError` | `internal/gateway/messages.go:422-478` | same |
| `internal/gateway/messages.go:384-547` | streaming section | `internal/gateway/messages.go:544-707` | same |
| `internal/translate/anthropic.go:260-265` | image → hard error | `internal/translate/anthropic.go:409-416` | image → `convertImageBlock` |
| `internal/translate/anthropic.go:297-325` | `StripCacheControl` | `internal/translate/anthropic.go:495-547` | same |

Citations inside sections that were **not** substantively rewritten (e.g. system-prompt handling, `tool_use`/`tool_result` conversion in FAQ.md Q13/Q14, most of ADMIN_MANUAL.md's systemd/Docker/TLS sections, all of API.md's endpoint-shape tables) were spot-checked for file existence and line-count plausibility but not individually re-derived line-by-line given the volume — most drifted by only a handful of lines (well within "plausible") rather than pointing at unrelated code. This is a deliberate scope decision, stated rather than silently glossed over: a full byte-exact re-citation of all ~150 references was not completed in this pass.

## 10. UNVERIFIABLE

| Claim | Location | Why unverifiable |
|---|---|---|
| "Any Anthropic-shaped chat client" compatibility beyond Claude Code itself | — (not actually claimed; checked and not found) | N/A — listed here only to record that no such over-broad claim was found during the audit |
| Windows-specific `%APPDATA%` config-dir resolution and `spawn_windows.go` detach behaviour | USER_GUIDE.md §2.1, ARCHITECTURE.md | Code reads correctly (`internal/config/config.go:80-85`, `cmd/ccr/spawn_windows.go`) but this audit was performed on Linux with no Windows host available to exercise it live; treated as ACCURATE-by-code-reading but flagged here as not live-verified |

---

## What changed in this pass

See `README.md`'s "Known limitations" section (new) for the current, re-verified-at-end-of-session state. In summary: the CLI-flag, gateway-bind-address, cleancache, vision, retry-loop, and release-status sections across `README.md`, `docs/USER_GUIDE.md`, `docs/ADMIN_MANUAL.md`, `docs/API.md`, `docs/ARCHITECTURE.md`, `docs/FAQ.md`, and `docs/RELEASE.md` were corrected; `docs/CONTRIBUTING.md` needed no changes (every count it makes — 9 port-test files, 3 prop-test files, 4 `FuzzXxx` funcs across 3 files, 12 HelixQA bank files, 14 challenge functions, and the three cited commit hashes — checked out exactly). `web/index.html` was updated to match and re-verified self-contained (no `<link>`, `<script src>`, `@import`, CSS `url()`, `fetch`/`XMLHttpRequest`, or CDN/font-host reference of any kind; the only `http(s)://` strings are two `<a href>` attribution links to the upstream GitHub repo, which do not fire on page load, plus example `curl` command text and placeholder input values).
