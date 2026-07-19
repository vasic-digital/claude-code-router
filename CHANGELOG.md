# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
No tagged release has been cut yet — see [`docs/RELEASE.md`](docs/RELEASE.md)
for the version scheme this project intends to adopt (SemVer, `v` prefix)
once the first tag lands. Until then everything lives under `[Unreleased]`,
seeded from this repository's real `git log --oneline` history (oldest
first below, matching commit order) — nothing here is speculative.

## [Unreleased]

### Added

- Config loader/validator (`internal/config`) reading the exact
  `~/.claude-code-router/config.json` shape `claude_toolkit`'s
  `cma_run_provider` already writes, including the capitalised
  `Providers`/`Router` keys, so existing installs need no changes.
  (`98593e4`)
- Anthropic ↔ OpenAI request translation (`internal/translate`): system
  prompt promotion, polymorphic content-block flattening, `tool_use` /
  `tool_result` conversion, recursive `cache_control` stripping, optional
  empty-tool-schema backfill. Image content blocks return an explicit error
  instead of being silently dropped. (`98593e4`)
- Gateway listener (`internal/gateway`) on Gin: HTTP/1.1, HTTP/2, optional
  HTTP/3 over QUIC (TLS-gated, advertised via `Alt-Svc`), and
  brotli → gzip → identity content-encoding negotiation. (`98593e4`)
- Provider/model routing (`internal/router.Select`): haiku-tier requests
  prefer `Router.background`, everything else `Router.default`, with named
  errors (not silent fallback) for every unroutable case. (`d41072e`)
- Upstream HTTP client (`internal/proxy`): posts to `api_base_url` verbatim,
  keeps the response-header timeout separate from the body-read timeout so
  streaming responses are never cut short, and never lets an API key reach
  an error string. (`d41072e`)
- `ccr` CLI (`cmd/ccr`): `start` / `ui` / `serve` (alias `web`) / `stop`,
  reproducing the upstream Node CLI's grammar and unknown-profile behaviour
  exactly, with pidfile-based background service management. (`7715fce`)
- `POST /v1/messages`, non-streaming and SSE-streaming, with upstream
  errors mapped to the Anthropic error shape and the original status code
  preserved. (`7715fce`)
- Fuzz and property-based test suites for `internal/translate`. (`7715fce`)
- Wiring of the real `internal/router`/`internal/proxy` implementations into
  the gateway's `serve` path via `Server.WireDefaults`, so a CLI-launched
  gateway gets haiku-tier-aware routing instead of the package's minimal
  standalone defaults. (`996c4f6`)
- Ported upstream `musistudio/claude-code-router` test-suite behaviour
  (`test/PORTING-MATRIX.md`), a HelixQA declarative test bank
  (`test/helixqa/`), adversarial challenge scenarios (`test/challenges/`),
  chaos scenarios (`test/chaos/`), security tests for key-leak/CRLF-
  injection/SSRF (`test/security/`), a mutation-testing harness
  (`test/mutation/`), an inbound request body size cap
  (`http.MaxBytesReader`, 32MiB), and full project documentation
  (`README.md`, `docs/USER_GUIDE.md`, `docs/ADMIN_MANUAL.md`, `docs/FAQ.md`,
  `docs/API.md`, `docs/ARCHITECTURE.md`, `web/index.html`). (`b7adde9`)
- `--gateway-port` flag and `CCR_GATEWAY_PORT` environment variable to make
  the Anthropic-compatible gateway's port configurable, separately from the
  management interface's `--port`/`CCR_WEB_PORT`. (`40ccafd`)
- Explicit per-request provider/model selector (`Provider/model` or
  `Provider,model` in the request's model field), taking precedence over
  `Router.default`/`Router.background`. (`82e0d97`)
- Upstream failure classification and fallback-planning tables
  (`ClassifyStatus`, `ClassifyTransportError`, `ClassifyRouteFailure`,
  `BuildExecutionPlan`, `NextFallbackProvider`) ported from upstream;
  terminal vs. retryable status codes are distinguished, though a retry
  loop is not yet wired to them. (`82e0d97`)
- Outbound `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` support in
  `internal/proxy` for operators behind a corporate proxy. (`82e0d97`)
- Inbound gateway authentication middleware (`internal/gateway/auth.go`):
  `Authorization: Bearer` or `x-api-key`, constant-time comparison,
  disabled by default (empty key list) so existing callers are unaffected.
  Not yet wired into the route table by default. (`82e0d97`)

### Fixed

- `internal/translate.StripCacheControl` decoded JSON with `json.Unmarshal`
  into `any`, silently converting every numeric literal to `float64` —
  corrupting large integers and rejecting some valid numbers outright.
  Fixed by decoding with `json.Decoder` + `UseNumber()` so every literal
  round-trips byte-for-byte. (`cd5af80`)
- `internal/translate.stripKey` deleted any key literally named
  `cache_control` anywhere in the request tree, including inside a tool's
  `input_schema.properties` — where such a key is a property name chosen by
  the tool author, not Anthropic metadata — producing a self-contradictory
  JSON Schema (`required` referencing a deleted property). Fixed to skip
  deletion of a schema `properties` object's own immediate keys while still
  stripping `cache_control` nested deeper inside a property's own schema.
  (`3f4a8e8`)
- The `cleancache` transformer was a complete no-op: `StripCacheControl`
  existed but was never called, and `Options.CleanCache` was read from
  config but never consulted during translation, so a tool's `input_schema`
  (forwarded as raw JSON) could still carry `cache_control` to an upstream
  that rejects unknown fields — precisely the failure `cleancache` exists
  to prevent. Fixed so the flag is applied end-to-end, verified against the
  actual bytes leaving the gateway. (`ad3f644`)

[Unreleased]: https://github.com/vasic-digital/claude-code-router/commits/main
