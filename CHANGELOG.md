# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning is SemVer with a `v` prefix (see [`docs/RELEASE.md`](docs/RELEASE.md)).
`v0.1.0`–`v0.4.1` were tagged 2026-07-19; `v0.4.2` (below) is the current
release. Entries are drawn from this repository's real `git log` history —
nothing here is speculative.

## [0.4.2] - 2026-07-19

Three more LIVE end-to-end suites (transport, hot-reload, load/soak) and a
robustness fix so the live suites survive back-to-back port pressure. Tests only
— no change to the gateway or any served behavior.

### Tests

- `test/livetls/`: real TLS transport proof — HTTP/2 over TLS (ALPN h2,
  `resp.Proto == HTTP/2.0`), the `Alt-Svc: h3=":port"` advertisement, a real
  HTTP/3-over-QUIC request (handshake completes, `HTTP/3.0`), and HTTP/3 without
  TLS correctly erroring (no silent downgrade). (Surfaced a gap: `ccr serve`
  exposes no TLS/HTTP3 flags — reachable only via `gateway.Options`.)
- `test/livereload/`: config hot-reload proven live — a validated change is
  detected+logged, an invalid change is rejected while the server stays up, and
  the honest boundary holds (the running gateway is NOT swapped in place —
  restart to apply), plus clean shutdown.
- `test/liveload/`: concurrency + soak — 500 concurrent `/v1/messages` all `200`
  with EXACT metric equality (`requests==500`, tokens `5500/3500`), in-flight
  gauge quiesces to 0, cache-under-load `upstream_hits <= workers` with
  `hits+misses==500`, 200 concurrent streams complete, and a 4s soak of ~44k
  requests with zero errors and no panics.

### Fixed

- The live harnesses' free-port helper retries a transient
  "address already in use" on an ephemeral `:0` bind (bounded), so heavy
  concurrent port churn / `TIME_WAIT` no longer spuriously fails a run.

## [0.4.1] - 2026-07-19

Metrics-attribution polish plus a comprehensive LIVE end-to-end test harness. No
behavior change to served responses; every opt-in feature behaves as in v0.4.0.

### Changed

- The `ccr_gen_ai_upstream_requests_total` metric is attributed per ATTEMPTED
  provider — cross-provider fallback now records EACH provider tried, not only
  the primary (pinned by a unit test). Anthropic-native non-streaming responses
  now record token usage; the 32MiB upstream-response cap is a named const; and
  `SemanticCache.Stats()` reports its own lookup accounting instead of
  double-counting the exact tier. (`ad11c24`)

### Tests / Docs

- `test/live/`: a genuine end-to-end harness — it builds `ccr`, starts a fake
  upstream and `ccr serve` as a SUBPROCESS on loopback, then drives real HTTP and
  scrapes the management server's `/metrics`. Nine scenarios pass: non-streaming
  translation, streaming SSE, the OpenAI facade (relay + 501), upstream 401 error
  mapping (no key leak), exact cache HIT + temperature bypass, cross-provider
  fallback (with per-provider metric attribution), semantic-cache near-duplicate,
  and `ccr config validate`/`show` redaction.
- New `docs/LIVE_TESTING.md`; v0.4.0 feature docs (metrics, semantic cache,
  Router.think) across README and `docs/*`.

## [0.4.0] - 2026-07-19

Observability and the semantic cache tier are now WIRED live, plus content-aware
Think routing and exhaustive verification suites. Every addition is opt-in and
default OFF/inert — an existing config's request path stays byte-identical to
v0.3.0. Full suite green under `-race` incl. chaos/security/mutation/helixqa.

### Added

- Prometheus metrics: a `/metrics` endpoint on the loopback management server
  exposing RED HTTP metrics (requests_total by method/route-template/status,
  in-flight gauge, duration histogram) and `gen_ai.*` counters (upstream
  requests, input/output tokens for streaming AND non-streaming, cache lookups
  by tier). Labels carry only method/route-template/provider-name/model — no
  secrets. Self-contained (no `client_golang`). Live-proven against a running
  binary. (`e8b7f50`, `8fac80c`)
- Semantic response cache WIRED: `Cache.semantic` (+ `Cache.semantic_threshold`,
  default 0.85) makes the gateway serve a near-duplicate prior request on an
  exact miss via the local lexical embedder — exact-first, scope-isolated,
  bounded registry, short-turn guard. A new `ResponseCache` interface lets the
  gateway consume the exact or semantic tier uniformly. Off by default.
  (`e8b7f50`)
- `Router.think` activated: a request carrying Anthropic's `thinking` field
  routes to `Router.think` when configured. The OpenAI translation still drops
  `thinking`; passthrough preserves it. (`103aa89`)

### Tests

- Exhaustive confirmation/validation/verification suites across cache
  (fingerprint/gate/memory/sqlite/embedder/semantic + 2 fuzzers), metrics (a
  Prometheus exposition-grammar validator + property + 24-writer concurrency),
  logging (redaction leaves no secret fragment + 3 fuzzers), and proxy (verbatim
  URL, no-key-in-error). All green under `-race`. (`d196018`)

## [0.3.0] - 2026-07-19

Response cache and cross-provider fallback are now WIRED and live on the
gateway's request path — both opt-in and default OFF, so an existing config's
request path stays byte-identical to v0.2.0. Full suite green under `-race`
incl. chaos/security/mutation/helixqa.

### Added

- Response cache (`internal/cache`, wired in `internal/gateway`): an optional
  top-level `Cache` config block (default OFF). On the non-streaming,
  OpenAI-provider path a cacheable request is served from a local store on a HIT
  with NO upstream call; a MISS stores the response. Backends: in-memory LRU or
  pure-Go SQLite (no CGO). Safety gates never cache temperature>0, streaming,
  tool-call, or error responses. `config show` displays the (secret-free) Cache
  block. (`025f8e9`, `609bc60`)
- Cross-provider fallback (`Router.crossProviderFallback` + `Router.fallback`,
  default OFF): when enabled, a RETRYABLE failure (5xx/429/transport) that
  exhausts the primary's same-provider retries advances to the next provider
  serving the model (via `router.BuildProviderPlan`), re-translating the request
  per provider. A Terminal failure (400/401/404) never falls back; streaming and
  Anthropic-native primaries are not eligible. (`13d222f`)
- Semantic cache tier (`SemanticCache`, opt-in, not yet returned by the gateway
  cache builder): exact-first, near-duplicate on an exact miss via a
  deterministic local lexical embedder (`LocalEmbedder`); a bounded per-scope
  registry and a short-turn guard prevent unbounded growth and cross-serving.
  Honestly a lexical near-duplicate signal, not a learned model. (`59ad10e`,
  `681789e`, `609bc60`)
- Content-aware routing: `Router.longContext` fires from an estimated token
  threshold (Anthropic inbound); `Router.think` wired but inert pending a
  caller-side signal. (`914f002`)
- `internal/metrics`: self-contained Prometheus text-exposition recorder (RED +
  `gen_ai.*` token/cache counters), no `client_golang` dependency. Package
  available; gateway/management endpoint wiring pending. (`8fac80c`)

### Fixed

- Panicking request handlers are now access-logged: `gin.Recovery()` is mounted
  INSIDE `LoggingMiddleware`, so a recovered 500 is logged with the correct
  status. (`c881086`)
- `config show` no longer drops the `Cache` block (`config.Redacted` now carries
  it through, still redacting api_keys). (`609bc60`)

## [0.2.0] - 2026-07-19

Multi-protocol gateway: an OpenAI-compatible inbound facade and Anthropic-native
passthrough on top of the v0.1.0 Anthropic↔OpenAI core, plus content-aware
routing, config hot-reload, and access logging with secret redaction. All four
remaining upstream test-port GAP skips are closed (0 `t.Skip("GAP")` in the
tree); full suite green under `-race` including chaos/security/mutation/helixqa.

### Added

- Provider `protocol` field (`"openai"` | `"anthropic"`) with conservative
  `api_base_url` inference and validation; an **absent** field behaves exactly
  as before (OpenAI), so every existing config is unchanged. An Anthropic-native
  provider is proxied UNTRANSLATED on both legs (request passthrough + verbatim
  response relay, no upstream-header leak). (`80b90db`)
- OpenAI-compatible inbound facade: `POST /v1/chat/completions` (+ `/proxy/v1/*`
  alias) forwards an OpenAI request to an OpenAI-shaped provider with the routed
  model overridden and relays the OpenAI response verbatim; errors use the OpenAI
  envelope via an `errorResponder` threaded through the shared retry loop. An
  OpenAI-inbound request routed to an Anthropic-native provider returns an
  explicit `501` rather than mistranslating. (`80b90db`)
- Reusable path→protocol classifier (`requestProtocolForPath`) and
  routing-eligibility predicate (`shouldApplyGatewayRouting`) matching upstream's
  full table across five protocol families; a classifier-driven `handleInbound`
  dispatcher. Anthropic Messages and OpenAI chat-completions are served live;
  OpenAI Responses and the two Gemini families are classified but not served
  (`404`). (`80b90db`)
- Content-aware routing: `Router.LongContext` fires when an estimated prompt
  token count exceeds the threshold (Anthropic-inbound); `Router.Think` routing
  is wired and tested but inert pending a caller-side thinking signal. With both
  unset, routing is byte-identical to before. (`914f002`)
- Config hot-reload wired into `ccr serve`/`start`/`ui`/`web` via
  `config.Watcher`: a validated change is logged, an invalid one is rejected and
  the previous good config retained, the watcher is stopped on shutdown. The
  running gateway is not swapped in place — a restart applies the new config.
  (`12f62af`)
- Structured access logging mounted with secret redaction: only request metadata
  is logged (never a header value or body); any secret is scrubbed to the fixed
  `[REDACTED]` marker (never a prefix). Honors `CCR_LOG_LEVEL` / `CCR_LOG_FORMAT`.
  (`539ea2f`)
- Unambiguous bare-model resolution as a subordinate route: resolves a bare model
  to its sole owning provider only when no `Router.Default`/`Background` applies;
  an ambiguous bare model is rejected loudly, and an explicit `Router.Default`
  always wins — no request can silently bypass a configured default. (`e0757d5`)

### Changed

- Protocol inference classifies a URL as Anthropic-native only for an
  `*.anthropic.com` host or an `/anthropic` path segment; an explicit `protocol`
  always overrides inference. Verified against the live 20-provider config that
  no real provider is reclassified. (`d69a793`)

### Docs

- Full documentation audit + reconciliation; `PORTING-MATRIX` synced to the
  0-GAP reality (15 PORTED / 0 GAP / 29 N/A, 42 passing port tests); an
  implementation-ready innovations dossier under `docs/research/innovations/`.

## [0.1.0] - 2026-07-19

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
