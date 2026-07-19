# Documentation accuracy audit

Audit of every factual claim in `README.md` and `docs/*.md` against the actual
code in this repository, performed by tracing each claim to the specific
file/function it describes and, where practical, exercising the built binary.

## Methodology and a load-bearing caveat

This repository moved **very** fast during the audit — the ground shifted
several times mid-pass. A first pass (recorded in an earlier revision of this
file) documented an in-flight state where an inbound-auth middleware, a retry
loop, and vision support had just landed. While the continuation pass was
running, the main engineer landed a substantially larger body of work, which
`go build ./...` confirms is coherent (it compiles and the whole test tree
builds) at end of run:

- **Provider `protocol` field + Anthropic-native passthrough.**
  `config.Provider` now carries an optional `protocol` (`"openai"`/`"anthropic"`,
  `config.go:31-63`), resolved via `ResolvedProtocol()` (explicit wins, else
  inferred from `api_base_url`; `config.go:87-130`) and validated
  (`config.go:206-215`). `internal/translate.AnthropicPassthrough`
  (`anthropic.go:495-516`) sends an Anthropic-shaped request through unchanged,
  and `handleMessages` routes an `anthropic`-protocol provider to it and relays
  the response verbatim (`relayAnthropicResponse`), skipping OpenAI translation
  (`messages.go:233-295`).
- **OpenAI chat-completions inbound facade + path→protocol classifier.**
  `internal/gateway/protocol.go` (`requestProtocolForPath` /
  `shouldApplyGatewayRouting`) and `internal/gateway/openai_inbound.go`
  (`handleInbound` dispatcher + an OpenAI-compatible inbound handler) now serve
  `POST /v1/chat/completions` alongside `/v1/messages`, each also under the
  `/proxy/v1/...` alias (`gateway.go:132-208`). OpenAI Responses and Gemini are
  recognised by the classifier but not routed (they `404`).
- **Structured logging is now WIRED.** `routes()` mounts `LoggingMiddleware` as
  the outermost middleware (`gateway.go:152`), and `Options.Logger` (nil for
  `cmd/ccr`) falls back to an env-configured redacting logger, so
  `CCR_LOG_LEVEL`/`CCR_LOG_FORMAT` are live.
- **All four upstream GAP skips are closed.** `grep -r 't.Skip("GAP' internal/`
  now returns nothing. The provider-protocol, ambiguous-bare-model,
  `requestProtocolForPath`, and `shouldApplyGatewayRouting` GAP placeholders are
  all replaced by real, passing tests.

Per the task's instruction to audit against whatever the code says at the
moment it is read, and to re-verify at end of run, this document describes the
**end-of-run** state (build passing). Because the gateway package in particular
was under continuous active edit, **every `file.go:NN` citation in the docs was
re-derived against the tree at end of run** — `config.go` shifted ~+68 lines,
`internal/gateway/messages.go` +25/+56, and `internal/gateway/gateway.go` grew
by roughly +45 (twice) as `New`, the `Options` struct, and `routes()` expanded.
If the engineer's in-flight edits continue past this audit, the gateway.go
citations specifically may drift again; they were correct at the moment of the
final verification below.

**Final verification performed:**

- `go build ./...` — succeeds.
- Gateway anchors spot-checked against citations: `LoggingMiddleware` mount at
  `gateway.go:152`, `RequireAPIKey` at `201`, health `160`, ready `168`,
  `Start()` `212`, HTTP/3-requires-TLS error `223`; `Load` `config.go:170`,
  `Validate` `190`, `SplitRoute` `239`; `handleMessages` `messages.go:189`,
  `doUpstreamWithRetry` `344`, `forwardUpstreamError` `448`, `respondNonStreaming`
  `538`, `streamAnthropicSSE` `621`; `AnthropicPassthrough` `anthropic.go:495`,
  `StripCacheControl` `538`. All match.
- Earlier live smoke test (first pass) measured a 3.0s wall-clock 3-attempt
  retry sequence against a connection-refused upstream and confirmed the
  `/health`/`/ready` JSON shapes and the `--gateway-port`/`--gateway-host` flags.

## Summary

| Verdict | Count (distinct claims) |
|---|---|
| ACCURATE | ~52 |
| STALE (corrected this pass) | ~44 |
| UNVERIFIABLE | 2 |

Counts cover distinct factual claims (flags, endpoints, config fields,
transformer/passthrough behaviour, logging, known-limitations items, release
status). Separately, **dozens** of `file.go:NN` citations were re-derived to
current line numbers because of the concurrent code growth; those are treated as
part of correcting the claim they annotate, not double-counted.

---

## 1. CLI flags (`cmd/ccr/flags.go`, `cmd/ccr/main.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `--host`/`--port` (management, default `127.0.0.1:3458`, env `CCR_WEB_HOST`/`CCR_WEB_PORT`) | ACCURATE | `cmd/ccr/flags.go:27-28`, `59-68`, `100-115` |
| `--gateway-host`/`--gateway-port` (gateway, default `127.0.0.1:3456`, env `CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT`); loopback default because the gateway holds live keys; a container needs `0.0.0.0` because `127.0.0.1` is the container's own loopback | ACCURATE (now documented in README/USER_GUIDE/ADMIN + `--help`) | `cmd/ccr/flags.go:37,43,70-99`, `main.go:50-58`, `serve.go:46` |
| `ccr start`/`ui` do **not** forward `--gateway-host`/`--gateway-port` to the detached `serve` child (only env survives) | ACCURATE (documented as a known limitation) | `cmd/ccr/service.go:104-114` forwards only `--host`/`--port`/`--gateway`/`--open` |
| `-h`/`--help`/`help`/no-args print the usage text, exit 0; unknown first arg → `Profile "<name>" was not found or is disabled.`, exit 1 | ACCURATE | `cmd/ccr/main.go:71-97`, tested `main_test.go` |
| `ccr config validate [path]` / `ccr config show [path]` exist; `show` redacts every `api_key` | ACCURATE — was doc'd as **PLANNED/not present** (STALE, corrected) | `cmd/ccr/config_cmd.go`, `internal/config/validate_cmd.go`; `config.Redacted` replaces `APIKey` with `[REDACTED]` before marshalling (never truncates) |

## 2. Gateway endpoints (`internal/gateway/gateway.go`, `protocol.go`, `openai_inbound.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| "Exactly three routes" / "the gateway only accepts the Anthropic Messages endpoint" | **STALE (corrected)** | `routes()` now registers `GET /health`, `GET /ready`, and **four** POST paths — `/v1/messages`, `/proxy/v1/messages`, `/v1/chat/completions`, `/proxy/v1/chat/completions` — all dispatched through `handleInbound` (`gateway.go:132-208`, `openai_inbound.go:44`) |
| `GET /health` / `GET /ready` always unauthenticated, own JSON shapes | ACCURATE | `gateway.go:160-182` |
| Inbound OpenAI chat-completions facade; `501` if routed to an Anthropic-native provider | ACCURATE (newly documented) | `openai_inbound.go` |
| OpenAI Responses / Gemini recognised by the classifier but not served (`404`) | ACCURATE (newly documented) | `protocol.go:54-101`; no routes registered for them |
| Management server: separate `net/http.ServeMux`, own `/health`, placeholder `/`, no auth, cannot be disabled | ACCURATE | `cmd/ccr/management.go` |

## 3. Config (`internal/config/config.go`, `watch.go`, `validate_cmd.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `Providers[].name/api_base_url/api_key/models/transformer.use` shape + validation | ACCURATE | `config.go:46-72`, `190-233` |
| `Providers[].protocol` (`"openai"`/`"anthropic"`, optional, inferred when absent, validated) | ACCURATE (newly landed + documented) | `config.go:31-63`, `87-130`, `206-215` |
| `Router.default/background/think/longContext` validated the same; only `default`/`background` drive routing | ACCURATE | `config.go:133-138`, `217-231`; `router.go` |
| Missing file → empty valid config; malformed JSON / failed `Validate()` → error | ACCURATE | `config.go:170-186` |
| Route splits on first comma only | ACCURATE | `config.go:239-249` |
| Config hot-reload (`config.Watcher`): rejects a reload that fails parse/`Validate()`, keeps last good config, reports via `onError`; tolerates a briefly-absent file | ACCURATE — **but not wired into `ccr serve`** (loads once at `serve.go:38`); documented honestly | `config/watch.go` |
| No `config.json` field for inbound gateway API keys or `MaxAttempts` | ACCURATE | `config.go` has no such field |

## 4. Transformers & translation (`internal/translate/anthropic.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `streamoptions` adds `stream_options.include_usage` only while streaming | ACCURATE | `anthropic.go:351-353`, `messages.go:249-250` |
| `cleancache` genuinely strips `cache_control` from tool `input_schema`s | ACCURATE (was a no-op historically; corrected in an earlier pass) | `anthropic.go:455-461`; `messages.go:249` |
| Image (vision) blocks → OpenAI `image_url` parts (base64/URL, incl. inside `tool_result`); named error, not silent drop | ACCURATE | `anthropic.go:237-335`, `409-416` |
| Anthropic-native passthrough (`AnthropicPassthrough`) sends the request unchanged for `protocol: "anthropic"` providers | ACCURATE (newly landed) | `anthropic.go:495-516`, wired `messages.go:233-295` |
| `cache_control` stripped at any JSON depth, `json.Number`-safe, schema-aware | ACCURATE | `anthropic.go:538-585` |

## 5. Retry/fallback (`internal/gateway/messages.go`, `internal/router/fallback.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| A real retry loop drives the classifier/backoff policy; up to `MaxAttempts` (default 3); never retries `Terminal`; never retries after a response byte is written | ACCURATE | `messages.go:319-416` (`doUpstreamWithRetry`); `fallback.go` |
| 32MiB inbound body cap → `413` | ACCURATE | `messages.go:28`, `189-214` |
| Upstream error forwarded preserving the exact status code | ACCURATE | `messages.go:448-504` |
| `MaxAttempts` has no CLI/config surface | ACCURATE (known limitation) | `cmd/ccr` never sets it |

## 6. Inbound auth (`internal/gateway/auth.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| `RequireAPIKey` mounted route-scoped on the completion routes only; `/health`/`/ready` never gated; empty key list disables auth entirely; constant-time compare; fixed `401`, never leaks the presented key | ACCURATE | `auth.go`; mounted at `gateway.go:201-207` |
| `cmd/ccr` never populates `Options.APIKeys` → unauthenticated by default | ACCURATE (known limitation) | no CLI flag / config field |

## 7. Structured logging (`internal/logging`, `internal/gateway/logging_middleware.go`)

| Claim | Verdict | Evidence |
|---|---|---|
| "`internal/logging` is an empty directory / PLANNED / not wired" | **STALE (corrected)** — the package is complete AND now mounted | `logging.go` (leveled slog + `CCR_LOG_LEVEL`/`CCR_LOG_FORMAT`), `redact.go` (secret redaction), `LoggingMiddleware` mounted at `gateway.go:152`; nil `Options.Logger` → `logging.New(os.Stderr)` fallback |
| Access log is metadata-only (method/path/status/duration/bytes/request-id); never bodies or header values | ACCURATE | `logging_middleware.go` |

## 8. GAPs

All four upstream GAP `t.Skip` placeholders are **closed** (zero remain):

| Former GAP | Now | Evidence |
|---|---|---|
| Provider protocol/type field | Closed | `config.go` `protocol` field; `provider_protocol_type_port_test.go` ("GAP CLOSED") |
| Ambiguous bare-model resolution | Closed (safely) | `resolveBareModel` resolves an unambiguous bare model only when no `Router.default`; two+ providers → loud error; `Router.default` always wins (`router.go:72-86`, `selector.go`) |
| `requestProtocolForPath` | Closed | `protocol.go:54-75` (real classifier) |
| `shouldApplyGatewayRouting` | Closed | `protocol.go:88-101` |

## 9. Release/version status

| Claim | Verdict | Evidence |
|---|---|---|
| "No tag cut yet" / "no published release artifact yet" | **STALE (corrected)** to match README's `v0.1.0` published statement across RELEASE.md and ADMIN_MANUAL §7 | Asserted per README's `v0.1.0` GitHub-release claim; **not independently re-run** here because this pass was constrained not to run `git`/`gh` — see UNVERIFIABLE below |

## 10. Citation accuracy

`README.md` + `docs/*.md` carry ~150 `path/file.go:NN` citations. Every cited
file exists. Because `config.go`, `internal/gateway/gateway.go`,
`internal/gateway/messages.go`, and `internal/translate/anthropic.go` all grew
during the audit, their citations were re-derived to current line numbers
(function-level anchors verified against `grep -n` at end of run — see the
Final verification list). A bare-range sweep (citations written as `…:NN`,
`MM-PP` without repeating the file prefix) was run to catch the trailing
second-range form and cleared.

## 11. UNVERIFIABLE

| Claim | Why |
|---|---|
| Windows `%APPDATA%` config-dir resolution and `spawn_windows.go` detach behaviour | Code reads correctly (`config.go:148-160`, `spawn_windows.go`) but not exercised — audit ran on Linux with no Windows host |
| `v0.1.0` is tagged/published as a GitHub release | The docs assert it and it is kept internally consistent, but this pass was constrained not to run `git`/`gh`, so publication could not be independently re-confirmed here |

---

## Known limitations (re-derived at end of run)

Current, honest list (mirrored in `README.md`):

1. **Inbound gateway authentication has no operator-facing switch.** `RequireAPIKey`
   is mounted on the completion routes, but `cmd/ccr` never sets `Options.APIKeys`
   (no flag/env/config), so the key list is always empty ⇒ auth disabled.
2. **`--gateway-host`/`--gateway-port` are not forwarded by `ccr start`/`ui`** to
   the detached `serve` child (only the `CCR_GATEWAY_*` env form survives).
3. **`Router.think` routing is wired but inert** (narrowed post-audit — see the
   reconciliation note below). The `chooseRoute` branch exists and is unit-tested,
   but `translate.AnthropicRequest` has no `thinking` field, so
   `requestWantsThinking` always returns false and think-routing never fires until
   a caller-side thinking signal is added. `Router.longContext` is **no longer a
   limitation** — it fires in production when an estimated prompt exceeds
   `DefaultLongContextThreshold` (60000 tokens) and the route is set
   (`internal/router/selector.go:95-138`, `router.go:130-175`).
4. **The retry loop's attempt budget (`MaxAttempts`, default 3) has no CLI/config
   surface.**
5. **An authenticated, explicitly-configured outbound proxy**
   (`proxy.NewWithUpstreamProxy`) is implemented/tested but not wired into
   `WireDefaults`/`cmd/ccr`, and has no `config.json` section. (Env
   `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` proxying *is* live.)
6. **Config hot-reload is wired into `ccr serve`, but the live gateway is not
   hot-swapped in place** (narrowed post-audit — see the reconciliation note
   below). `serve` now runs a `configReloader` on `config.json`
   (`cmd/ccr/serve.go:93-112`, `cmd/ccr/reload.go`) that validates each change,
   logs an accepted one, keeps the previous good config on a rejected one, and
   stops on shutdown. The remaining boundary: `gateway.Server` holds its
   `*config.Config` in an unexported field with no public setter, so a validated
   reload is kept as the latest known-good config (`Current()`) but the running
   gateway keeps serving its startup config — a restart is still required for it
   to take effect.
7. **Inbound OpenAI Responses / Gemini are recognised by the classifier but not
   served** (no route ⇒ `404`); an OpenAI-inbound request routed to an
   Anthropic-native provider is an explicit `501`.

Items closed during this documentation pass (no longer limitations): the
retry-loop wiring, vision/image support, `cleancache` actually stripping tool
schemas, the provider `protocol` field + Anthropic-native passthrough,
unambiguous bare-model resolution, the OpenAI chat-completions inbound facade +
path→protocol classifier, and structured/per-request logging being wired in.

**Post-audit reconciliation (limitations #3 and #6, narrowed after the audit
tables above were written).** Two features landed from other engineers *after*
this audit's Config-section verdicts (§3, rows on "only `default`/`background`
drive routing" and "hot-reload… not wired into `ccr serve`") were recorded, so
those §3 rows describe the audit-time state and are intentionally left as the
historical record; the *current* truth lives in limitations #3 and #6 above and
in the code:
- `Router.longContext` now fires in production — an estimated prompt over
  `DefaultLongContextThreshold` (60000 tokens) routes there when configured
  (`internal/router/router.go:130-175`, `internal/router/selector.go:95-138`);
  `Router.think` routing is wired and unit-tested but inert, because
  `translate.AnthropicRequest` has no `thinking` field for `requestWantsThinking`
  to read (`internal/router/selector.go:140-167`).
- Config hot-reload **is** now wired into `ccr serve`/`start`/`ui`/`web`
  (`cmd/ccr/serve.go:93-112`, `cmd/ccr/reload.go`): a validated change is logged
  and kept as latest-known-good, an invalid change is rejected with the previous
  good config retained, and the watcher stops on shutdown — but the running
  gateway is not hot-swapped in place (no public config setter on
  `gateway.Server`), so a restart is still required for a reload to take effect.

## web/index.html self-contained result

Verified with a grep for every external-load vector: **0** `<link>`, **0**
`<script src>`, **0** `@import`, **0** CSS `url(http…)`, **0**
`fetch`/`XMLHttpRequest`/`WebSocket`/`EventSource`, **0** CDN/font-host
references (googleapis/gstatic/cdnjs/unpkg/jsdelivr). The only `http(s)://`
strings are two `<a href>` attribution links to the upstream GitHub repo (which
do not fire on load), example `curl` command text, placeholder input values, and
one `^https?://` validation regex. Inline `<style>`/`<script>` only; the two
`URL(…)` matches are `URL.createObjectURL`/`revokeObjectURL` for the local
config-download blob. **Self-contained: confirmed.**
