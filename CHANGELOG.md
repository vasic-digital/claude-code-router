# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning is SemVer with a `v` prefix (see [`docs/RELEASE.md`](docs/RELEASE.md)).
`v0.1.0`–`v0.4.2` were tagged 2026-07-19; `v0.4.3`–`v0.4.8` (below) are the
current releases. Entries are drawn from this repository's real `git log`
history — nothing here is speculative.

## [0.4.8] - 2026-07-20

An authenticated outbound proxy becomes configurable — the last documented
"Known limitation" is now closed. Backward compatible: no `proxy` block = the
existing environment-only behaviour, unchanged.

### Added

- **Authenticated outbound proxy via a `proxy` config block.** A top-level
  `"proxy": {"url": "http://proxy.corp:8888", "username": "u", "password": "…"}`
  routes every upstream provider request through that proxy (HTTP Basic to the
  proxy itself), overriding the ambient `HTTP_PROXY`/`HTTPS_PROXY`. The
  already-built `proxy.NewWithUpstreamProxy` is now wired into `WireDefaults`,
  which returns an error on a bad proxy config (a hard, credential-free startup
  error). All three fields are required when the block is present — a partial
  block is rejected by `Validate` (at load) and by `ccr config validate`, since
  the proxy only activates when server+username+password are all set (a partial
  block would silently fall through to the environment). For an *un*authenticated
  proxy, keep using the `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment.
- **The proxy `password` is a secret and is redacted.** `config.Redacted`
  carries the `proxy` block into `ccr config show` with `password` replaced by
  `[REDACTED]` (url + username are shown) — the same guarantee provider
  `api_key`s get; the real password never appears in show/validate output, logs,
  error messages, or the proxy library's error text (built from the server URL
  alone). Pinned by a prefix-scanning redaction test, config validation tests
  (complete/incomplete/bad-scheme, `errors.Is`-matchable), and a wiring test that
  inspects the resolved proxy URL (host + Basic credentials) to prove requests
  actually route through it.

## [0.4.7] - 2026-07-20

Operator control surface: inbound authentication becomes configurable, the
retry budget is exposed, and `ccr start`/`ui` stop silently dropping gateway
flags. All backward compatible — defaults are unchanged.

### Added

- **Inbound gateway authentication is now operator-configurable.** `--api-key
  <key>` (repeatable) and `CCR_API_KEYS` (comma-separated list) populate the
  accepted-key list that the already-present `RequireAPIKey` middleware enforces
  on the four completion routes (`/v1/messages`, `/v1/chat/completions`, and
  their `/proxy` aliases) via `Authorization: Bearer <key>` or `x-api-key`;
  `/health` and `/ready` are never gated. The `--api-key` flag replaces the
  `CCR_API_KEYS` env list (flag > env). **Default remains an empty list =
  unauthenticated**, so existing loopback deployments are unaffected. Keys are
  compared in constant time and never echoed in the 401 body. `ccr start`/`ui`
  hand keys to the detached child through the inherited environment, never argv
  (a flag value would be visible in `ps`).
- **`--max-attempts <n>`** (env `CCR_MAX_ATTEMPTS`, must be ≥ 1) exposes the
  upstream retry budget (`gateway.Options.MaxAttempts`); default remains 3.

### Fixed

- **`ccr start` / `ccr ui` now forward the gateway flags to the detached `serve`
  child.** `--gateway-host`, `--gateway-port`, and the TLS/HTTP3 flags
  (`--tls-cert`/`--tls-key`/`--http3`) plus `--max-attempts` were parsed by
  `start`/`ui` but silently dropped when re-execing `serve`, so
  `ccr start --gateway-port N` bound the default and — more seriously —
  `ccr start --tls-cert … --tls-key …` silently served **plaintext**. The child
  argv is now built by a single tested helper (`serveChildArgs`) whose output
  round-trips losslessly back through the flag parser. (The `CCR_*` env path
  already worked and still does.)

## [0.4.6] - 2026-07-20

Documentation only — correct claims the v0.4.5 routing change made stale.

### Documentation

- README, `docs/FAQ.md`, `docs/ARCHITECTURE.md`, `docs/API.md`,
  `docs/LIVE_TESTING.md`, and `docs/DOC-AUDIT.md` no longer state that the
  OpenAI-inbound facade (`POST /v1/chat/completions`) "routes on model alone" or
  that `Router.longContext` "never triggers" for it — as of v0.4.5 the facade
  estimates its own request size, so long-context routing fires symmetrically
  for both inbound paths. `Router.think` remaining Anthropic-inbound only (the
  OpenAI facade has no `thinking` field) is now stated precisely, without
  implying the facade ignores request size.

## [0.4.5] - 2026-07-20

The OpenAI-compatible inbound facade now routes on request size too, so the two
inbound facades pick the long-context tier symmetrically. No change to any
forwarded upstream body.

### Fixed

- `POST /v1/chat/completions` now trips `Router.longContext` for a large prompt,
  closing a documented gap where the facade routed on model alone (it built a
  routing request carrying only Model+Stream, so the content-based estimate saw
  ~0 tokens and a big prompt always fell to `Router.default`). A new
  `routingRequestFromOpenAI` helper builds a routing-ONLY request from the
  inbound body — measuring each message's TEXT content (string content, or the
  text parts of a multi-part array; `image_url` data-URIs are excluded, since a
  base64 image's byte size is unrelated to its token cost) and folding tools
  across so `estimateTokenCount` approximates the true size. The synthetic
  request is used solely for `Route()`; the forwarded upstream body is unchanged
  (still the verbatim client body with only `model` overridden). Inert when
  `Router.longContext` is unset. Pinned by unit tests (large string / large
  parts-array → long-context provider, small and image-heavy/text-light →
  default, forwarded-body-unchanged for a large request, malformed body →
  degrades to model+stream) and the helper's own test.

## [0.4.4] - 2026-07-20

Streaming responses now record token usage, closing the last gen_ai
observability gap. No change to relayed bytes or any served behavior.

### Fixed

- Streaming responses now record `ccr_gen_ai_input_tokens_total` /
  `ccr_gen_ai_output_tokens_total`, at parity with non-streaming. Two relay
  paths that previously copied SSE byte-for-byte without decoding usage — the
  Anthropic-native streaming relay (`relayAnthropicResponse`) and the OpenAI
  facade streaming relay (`relayOpenAIResponse`) — now tee the stream through a
  `streamUsageScanner` that extracts usage as chunks fly by and records once at
  stream end. The client bytes are unchanged (the scanner observes each chunk
  only after it is written and flushed), and recording is nil-safe, secret-free
  (provider/model/int counts only), and single (no double-count with the
  already-recording OpenAI→Anthropic translation path `streamAnthropicSSE`).
  Anthropic-native streams always carry usage (message_start + message_delta),
  so recording is reliable there; the OpenAI facade forwards the client body
  verbatim and never injects `stream_options.include_usage`, so a client that
  did not request usage legitimately records 0 (best-effort, documented). Pinned
  by pure scanner unit tests, gateway streaming tests (exact token values +
  verbatim-relay preserved with a usage chunk present + no-usage→records-nothing),
  and the live `liveprod` matrix (a streaming `/v1/chat/completions` call whose
  13/5 usage moves the counters end-to-end).

## [0.4.3] - 2026-07-19

TLS and HTTP/3 become first-class `ccr serve` CLI flags (previously reachable
only through the `gateway.Options` library struct), a metrics-parity fix so the
OpenAI-compatible facade is attributed like the Anthropic path, and three more
LIVE end-to-end suites for production hardening.

### Added

- `ccr serve` (and `start`/`ui`/`web`) now accept `--tls-cert <path>` /
  `--tls-key <path>` (env `CCR_TLS_CERT` / `CCR_TLS_KEY`) to serve HTTPS
  (HTTP/2 over TLS via ALPN) and `--http3` / `--no-http3` (env `CCR_HTTP3`) to
  additionally serve QUIC with the `Alt-Svc: h3` advertisement. These map
  straight into `gateway.Options{CertFile, KeyFile, EnableHTTP3}`. The cert and
  key must be supplied together, and `--http3` requires TLS — both are rejected
  at parse time with a clear usage message (exit 2) rather than deep in the
  gateway (QUIC has no cleartext mode). The startup log now reports the actual
  scheme (`https://…`) and `(+HTTP/3)`.

### Fixed

- The OpenAI-compatible inbound facade (`POST /v1/chat/completions`) now records
  `ccr_gen_ai_upstream_requests_total` and the input/output token counters,
  exactly like the Anthropic `POST /v1/messages` path. Previously a
  chat-completions request reached the upstream but was invisible to those
  gen_ai metrics — only the RED `ccr_http_requests_total` middleware counted it.
  The upstream response is still relayed byte-for-byte; token usage is parsed
  read-only from the same buffered body (non-streaming only — streaming remains
  a documented, byte-for-byte relay that is not token-accounted). Surfaced by the
  new `test/liveprod` matrix and pinned by unit + live tests.
- `Server.Start()` now binds the TCP listener and loads the TLS certificate
  **synchronously**, so a bad `--tls-cert`/`--tls-key` path or an already-in-use
  port is a returned error instead of a nil return followed by a
  `listening on https://…` log line while the bind silently failed in a
  background goroutine. The QUIC/UDP socket is bound synchronously too (its error
  was previously swallowed entirely). TLS/HTTP2/HTTP3 negotiation is unchanged
  (the live transport suite still passes); pinned by new unit tests.

### Tests / Docs

- `test/livetls/` now drives the **real `ccr serve` subprocess** with
  `--tls-cert`/`--tls-key`/`--http3` (closing the prior gap where TLS/HTTP3 were
  reachable only in-process): HTTP/2-over-TLS (`proto=HTTP/2.0`, ALPN h2), the
  `Alt-Svc: h3=":port"` advertisement, a real HTTP/3-over-QUIC request
  (`proto=HTTP/3.0`), and `ccr serve --http3` without certs cleanly rejected
  (exit 2, correct message).
- `test/livegraceful/`: graceful SIGTERM shutdown under 16-concurrent load —
  clean exit 0 within the grace window, in-flight requests drain to well-formed
  Anthropic messages, upstream `started == finished` (nothing cut off), no
  panic/goroutine-dump/leak; plus an idle-shutdown case.
- `test/liveprod/`: a broad production matrix (endpoint surface, plain/memory/
  sqlite/semantic cache, cross-provider fallback, transformer flags, think-tier
  routing, and multi-provider selection) asserting exact RED + gen_ai metric
  deltas across BOTH the Anthropic and OpenAI-facade paths.
- `test/liveedge/`: adversarial robustness — 33MiB body → 413 and survives,
  malformed JSON → 400, malformed/empty/zero-choice upstream → clean `api_error`
  (never a broken 200), mid-stream EOF → well-formed SSE termination, a canary
  api_key never leaking into any client body or metric label, unicode/control
  round-trip, and concurrent valid+malformed with no cross-contamination.
- New CLI flags documented across `README.md`, `docs/USER_GUIDE.md`,
  `docs/ARCHITECTURE.md`, `docs/ADMIN_MANUAL.md`, and `docs/LIVE_TESTING.md`
  (the earlier "PLANNED / library-only" TLS/HTTP3 notes are retired).

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
