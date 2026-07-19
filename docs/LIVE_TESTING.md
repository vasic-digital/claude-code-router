# Live (end-to-end) testing

> **Status.** This document describes the **contract** of the `test/live/`
> end-to-end harness — what it builds, how it runs, and what each scenario
> proves. Some scenarios may still be landing in `test/live/`; where a scenario
> is described here it defines the behaviour the harness is expected to assert,
> not a run this document witnessed. The hermetic unit/integration suites under
> `internal/…` and `test/` (challenges, chaos, mutation, security) remain the
> primary correctness gate; live testing is the **integration proof** on top of
> them.

The unit and integration tests exercise the router's packages in isolation
(`internal/translate`, `internal/router`, `internal/cache`, `internal/gateway`,
`internal/metrics`, `cmd/ccr`). Live testing closes the last gap: it builds the
**real `ccr` binary**, runs it as a **subprocess** exactly as an operator would,
points it at a **fake upstream** it fully controls, and drives it over **real
HTTP** — then scrapes the management server's `/metrics` to confirm the data
plane recorded what the requests did.

## What the harness does

1. **Builds `ccr`.** `go build ./cmd/ccr` into a temp path, so the test drives
   the same binary `ccr serve` ships — not an in-process handler. (Building is
   part of `go test`; no separate step.)
2. **Starts a fake upstream.** An `httptest.Server` (or equivalent local
   listener) that speaks the OpenAI chat-completions shape and, for
   Anthropic-native scenarios, the Messages shape. It is scripted per test: it
   can return a canned success, a streaming SSE body, a specific HTTP status
   (429/5xx/4xx), or a token-usage block — so translation, error mapping, and
   fallback can each be provoked deterministically.
3. **Writes a config** pointing the router's provider(s) at that fake upstream's
   URL, then **starts `ccr serve`** as a child process. Serve brings up two
   loopback listeners: the **gateway** (Anthropic-compatible API, default
   `127.0.0.1:3456`) and the **management** server (default `127.0.0.1:3458`),
   which is where `/metrics` and `/health` live — the gateway hot path never
   serves `/metrics`.
4. **Drives real HTTP** against the gateway (`POST /v1/messages`, the OpenAI
   facade `POST /v1/chat/completions`) and against the management server
   (`GET /metrics`, `GET /health`), asserting on status, headers, and body.
5. **Tears down**: signals the child (SIGINT/SIGTERM, exercising the graceful
   `shutdownGrace` drain in `cmd/ccr/serve.go`) and stops the fake upstream.

Because the router binds loopback ports, the harness either uses the defaults on
an otherwise-idle host or assigns free ports and passes them through
`--gateway-port` / `--port`.

## Running it

```bash
# The whole live suite (builds ccr, starts subprocesses, drives HTTP):
go test ./test/live/...

# Verbose, to watch each scenario:
go test -v ./test/live/...

# A single scenario by name:
go test -run TestLive_CacheHit ./test/live/...
```

The live suite needs a working Go toolchain and the ability to bind loopback
TCP ports and spawn a child process; it makes **no outbound network calls** (the
only "upstream" is the in-test fake). Scenarios that require the optional SQLite
cache backend are guarded the same way the rest of the tree guards
`sqlite`-tagged builds.

## What each scenario proves

| Scenario | What it drives | What it proves |
|---|---|---|
| **Translation** | `POST /v1/messages` with an Anthropic body to an OpenAI-shaped provider | The gateway converts Anthropic → OpenAI on the way out and OpenAI → Anthropic on the way back; the client sees a well-formed Messages response. |
| **Streaming** | `POST /v1/messages` with `"stream": true` | The upstream SSE stream is relayed as Anthropic SSE events end-to-end; bytes flush incrementally and the stream terminates cleanly. |
| **OpenAI facade** | `POST /v1/chat/completions` | The OpenAI-inbound facade accepts an OpenAI request, routes it, and returns an OpenAI-shaped response. (Routing estimates the request's own size, so a large body trips `Router.longContext` symmetrically with `/v1/messages` — see `routingRequestFromOpenAI`; `Router.think` remains Anthropic-inbound only, as OpenAI has no `thinking` field.) |
| **Error mapping** | Fake upstream returns `4xx`/`5xx` | A upstream failure surfaces to the client as the correct HTTP status/error shape rather than a 200 with a broken body. |
| **Cache HIT** | Enable `Cache`, send the same non-streaming request twice | The second identical request is served from the cache with **no second upstream call**, and `ccr_gen_ai_cache_lookups_total{tier="exact",result="hit"}` increments. |
| **Cross-provider fallback** | Two providers serving the same model, primary returns a **retryable** status, `Router.crossProviderFallback: true` | A retryable primary failure advances to the next provider and the client gets a success; a **terminal** (`4xx`) failure does **not** fall back. |
| **Semantic cache** | Enable `Cache.semantic`, send a near-duplicate (re-asked / one-word-edit) non-streaming request | On an exact miss the lexical near-duplicate tier can serve a prior answer; recorded as `ccr_gen_ai_cache_lookups_total{tier="semantic",result="hit"}`. (Off by default; lexical, not learned — see USER_GUIDE §8.4.) |
| **Config validate/show** | `ccr config validate <path>` / `ccr config show <path>` | `validate` exits `0` on a good config and `1` (reporting every problem) on a bad one; `show` prints effective JSON with `api_key` replaced by `[REDACTED]`. |
| **Metrics** | `GET /metrics` on the management server after driving traffic | The Prometheus text-exposition output exposes the expected families with the counts the driven requests produced (see below). |

## Scraping and asserting `/metrics`

`/metrics` is a plain-text Prometheus exposition on the **management** server
(`127.0.0.1:3458` by default), never on the gateway. After driving requests, the
harness scrapes it and asserts on these families (all defined in
`internal/metrics/metrics.go`):

```
# HELP ccr_http_requests_total Total HTTP requests handled, by method, route template and status code.
# TYPE ccr_http_requests_total counter
ccr_http_requests_total{method="POST",path="/v1/messages",status="200"} 1

# TYPE ccr_http_request_duration_seconds histogram
# TYPE ccr_http_inflight_requests gauge
# TYPE ccr_gen_ai_upstream_requests_total counter        # {provider,model}
# TYPE ccr_gen_ai_input_tokens_total counter             # {provider,model}
# TYPE ccr_gen_ai_output_tokens_total counter            # {provider,model}
# TYPE ccr_gen_ai_cache_lookups_total counter            # {tier,result}
```

Labels are bounded and secret-free by construction: `path` is the route
**template** (`/v1/messages`, not a raw URL; an unmatched path collapses to
`/(unmatched)`), `provider` is the resolved provider **name** (never its API
key), and `model` is the resolved model id. A cache HIT increments
`ccr_gen_ai_cache_lookups_total` **without** an accompanying
`ccr_gen_ai_upstream_requests_total` bump — the served-from-cache request never
reaches the upstream — which is exactly the invariant the Cache-HIT scenario
checks.

## Additional live suites

Beyond `test/live/` (the core scenarios above), six focused suites cover the
transport, operational, production-matrix, and adversarial surfaces, each
building `ccr` and driving it over real loopback:

- **`test/livetls/`** — TLS transport: HTTP/2 over TLS (ALPN h2), the `Alt-Svc`
  h3 advertisement, a real HTTP/3-over-QUIC request, and HTTP/3-without-TLS
  erroring. (TLS/HTTP3 are reached via `gateway.Options`; the `ccr serve` CLI
  does not yet expose them — a documented gap.)
- **`test/livereload/`** — config hot-reload: a validated change is
  detected + logged, an invalid one is rejected while the server stays up, and
  the honest boundary holds (the running gateway is not swapped in place —
  restart to apply).
- **`test/liveload/`** — concurrency + soak: 500 concurrent requests with exact
  metric equality, the in-flight gauge quiescing to 0, cache-under-load bounds,
  200 concurrent streams, and a multi-second soak with zero errors and no panics.
- **`test/livegraceful/`** — graceful SIGTERM shutdown, driven through the real
  `ccr serve` subprocess: `TestGracefulShutdownUnderLoad` fires 16 concurrent
  `POST /v1/messages` against a deliberately slow upstream, sends SIGTERM
  mid-drain, and asserts a clean exit 0 within the ~10s `shutdownGrace` window,
  every completed response is a well-formed Anthropic message (never a
  truncated/garbage body), the upstream's started/finished counts line up (so
  nothing was cut off), no panic/goroutine-dump/leak marker appears in the log,
  and the management `/metrics` listener is gone after exit.
  `TestGracefulShutdownIdle` proves an idle server (no in-flight requests) also
  exits 0 promptly on SIGTERM, logging "shutting down..." with no crash.
- **`test/liveprod/`** — a broad production matrix (`TestLiveProductionMatrix`),
  each subtest standing up its own fresh upstream and serve instance:
  `endpoint_surface` (every routable endpoint reaches the right handler; `GET
  /v1/messages` → 404/405; `POST /v1/responses` → 404; negative probes collapse
  to the `/(unmatched)` metrics bucket with no raw-path label leak);
  `config_plain` (exact RED + gen_ai metric deltas across 1 non-stream + 1
  stream `/v1/messages` call and 1 `/v1/chat/completions` call — all three
  upstream calls are attributed in `ccr_gen_ai_upstream_requests_total` and the
  token counters, across BOTH the Anthropic path and the OpenAI facade, which
  now records upstream + non-streaming token usage at parity with
  `/v1/messages`); `cache_memory` / `cache_sqlite` (a HIT skips
  the upstream and moves `ccr_gen_ai_cache_lookups_total{tier="exact",result="hit"}`
  for both cache backends); `cache_semantic` (a near-duplicate request hits the
  semantic tier); `cross_provider_fallback` (per-attempt provider attribution —
  both the failed primary and the secondary that actually served appear in
  `ccr_gen_ai_upstream_requests_total`); `transformer_cleancache_streamoptions`
  (`stream_options.include_usage` is injected into the upstream body and
  `cache_control` is stripped from a forwarded tool schema, both verified at the
  wire); `router_think_routing` (a request carrying a `thinking` field routes to
  the think provider; the default provider is never hit); `multi_provider_routing`
  (a bare default-tier model, a haiku-tier background model, and an explicit
  `"provider,model"` selector each reach their intended, distinct provider).
- **`test/liveedge/`** — adversarial/robustness testing against one `ccr serve`
  subprocess: an oversized 33MiB body → 413 with the server surviving;
  malformed JSON → 400; wrong method / unknown route / `POST /v1/responses` →
  404/405; the OpenAI facade's own invalid JSON → 400 in an OpenAI error
  envelope, and an anthropic-native route on that facade → 501; upstream
  misbehavior (a non-JSON garbage 200, an empty 200 body, a valid-JSON
  zero-choices 200) each map to a clean 502 `api_error` and are never relayed as
  if they were a valid message, while a large-but-under-cap body still returns
  200; a mid-stream upstream EOF still terminates the client's SSE
  well-formed (`message_stop` present, no hang); a canary `api_key` is asserted
  to never leak into any client-visible response body or any `/metrics` label
  (grep-count 0) across 401/500/garbage-upstream error paths; a
  unicode/control-character payload round-trips byte-for-byte through an echo
  upstream; 16 concurrent valid + 16 concurrent malformed requests run
  interleaved with no cross-contamination between per-request markers; and a
  final subtest asserts no panic/goroutine-dump/fatal-error marker leaked into
  the serve log after the whole adversarial batch.

Run one (`go test ./test/livetls/...`) or all sequentially
(`go test ./test/live/... ./test/livetls/... ./test/livereload/... ./test/liveload/... ./test/livegraceful/... ./test/liveprod/... ./test/liveedge/... -p 1`).
The free-port helpers retry a transient ephemeral-bind failure, so heavy
concurrent port churn does not spuriously fail a run.

## Toolkit-side live proof

The companion `claude_toolkit` repository proves the bundled router builds and
serves from the operator's side. `scripts/claude-ccr-build.sh` initialises the
`claude-code-router` submodule, runs `go build -o bin/ccr ./cmd/ccr`, installs
it as `ccr`, and self-checks the router grammar (`ccr --help` must advertise
`ccr start` and `ccr serve`); `scripts/tests/test_ccr_build.sh` asserts that
script's contract hermetically. `scripts/tests/verify_ccr_live.sh` is the
end-to-end LIVE proof: it builds the bundled Go `ccr` into a temp dir, boots
`ccr serve`, and probes it — `/health`, `/ready`, a `POST /v1/messages` whose
502 Anthropic error envelope must not leak the api_key, the `/metrics` counters,
and `ccr config validate`/`show` (redaction) — writing captured evidence to
`scripts/tests/proof/ccr-go-live.txt`.

## See also

- `docs/ADMIN_MANUAL.md` — scraping `/metrics` in production, the management vs.
  gateway split.
- `docs/USER_GUIDE.md` §8 — the response cache (exact and semantic tiers).
- `docs/ARCHITECTURE.md` — the request lifecycle the live scenarios exercise.
</content>
</invoke>
