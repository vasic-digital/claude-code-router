# Innovations dossier — `claude-code-router` (Go)

An implementation-ready research dossier for the next wave of capability in this
Go port of `claude-code-router`. Every proposal is anchored to code that exists
in this repository today (file + line references), states what is missing and
why it matters here, and breaks the work into **PHASES → TASKS → SUB-TASKS**
with runnable micro-POCs, diagrams, data definitions, acceptance criteria, and
backward-compatibility notes.

> This dossier is **design**, not code. Nothing here has been merged. It is
> written to be handed to an implementer who can start on any Phase 1 without
> re-deriving the context.

## What this gateway is today (the baseline every proposal builds on)

A single-binary Anthropic-Messages ⇄ OpenAI-chat-completions gateway:

- **Inbound**: exactly one route, `POST /v1/messages`, decoded into
  `translate.AnthropicRequest` (`internal/gateway/gateway.go:161`,
  `internal/gateway/messages.go:189-271`).
- **Routing**: `router.Select` — explicit `provider,model` selector, else
  haiku-tier → `Router.Background`, else `Router.Default`, else first-provider
  fallback (`internal/router/router.go:45-79`).
- **Translation**: `translate.AnthropicToOpenAI` (system-prompt promotion,
  content-block flattening, tool + vision conversion, `cache_control` stripping)
  (`internal/translate/anthropic.go:337-473`).
- **Upstream**: one HTTP attempt per call, streaming-safe header timeout, never
  leaks the API key (`internal/proxy/proxy.go:66-85`), wrapped in a
  single-provider retry loop (`internal/gateway/messages.go:294-383`).
- **Transport**: Gin, HTTP/1.1 + HTTP/2, opt-in HTTP/3 over QUIC, brotli → gzip
  → identity negotiation (`internal/gateway/gateway.go:166-199`,
  `internal/gateway/compress.go`).

## The five things grounding drew out (why these themes)

Reading the code surfaced concrete, already-half-built seams that the themes
below turn into features:

1. **`internal/logging` + `internal/gateway/logging_middleware.go` are fully
   built but never wired.** `docs/ARCHITECTURE.md` calls logging *"PLANNED — not
   called from any package yet."* → **Theme 04**.
2. **`internal/config/watch.go` (`Watcher`, hot-reload) is fully built but
   `cmd/ccr/serve.go:38` calls `config.Load` once and never uses it** — even
   though its own doc says *"a long-running gateway must not go blind to"* the
   toolkit rewriting `config.json` on every launch. → **Theme 06**.
3. **`router.BuildExecutionPlan` / `NextFallbackProvider` (cross-provider
   fallback) are built and tested but the retry loop
   (`doUpstreamWithRetry`) only ever retries the *same* provider.** → **Theme 05**.
4. **`Router.Think` / `Router.LongContext` are validated
   (`internal/config/config.go:139-153`) but never consumed by
   `router.Select`.** → **Theme 05**.
5. **The whole product is single-protocol, single-attempt, cache-less, and
   unobserved** — no inbound facades, no hedging, no cache, no metrics. →
   **Themes 01, 02, 03, 04**.

## The themes

| # | Theme | One line | Primary files it touches |
|---|---|---|---|
| [01](01-multi-protocol-inbound-facades.md) | Multi-protocol inbound facades | Accept OpenAI Chat, OpenAI Responses, and Gemini `generateContent` requests, translating each into the existing canonical `AnthropicRequest` pipeline | `gateway.go`, `messages.go`, new `internal/facade` |
| [02](02-semantic-response-cache.md) | Semantic response cache | Two-tier exact + embedding-similarity cache in front of the upstream call, cutting cost and tail latency | `messages.go`, new `internal/cache`, `config.go` |
| [03](03-request-hedging.md) | Request hedging / speculative retries | Race a delayed backup attempt against a slow-but-alive upstream to cut p99 without waiting out one provider's tail | `messages.go` (`doUpstreamWithRetry`), `internal/router/fallback.go` |
| [04](04-observability-and-metrics.md) | Observability & metrics | Wire the already-built redacting logger, add Prometheus RED + GenAI token metrics, then OpenTelemetry tracing on GenAI semconv | `gateway.go`, `management.go`, `logging_middleware.go`, new `internal/metrics` |
| [05](05-adaptive-provider-routing.md) | Adaptive provider routing | Consume `Think`/`LongContext`, wire cross-provider fallback chains, add circuit breakers + token-budget-aware weighted load balancing | `router.go`, `fallback.go`, `messages.go`, `config.go` |
| [06](06-transport-and-runtime-hardening.md) | Transport & runtime hardening | Hot-reload config via the built `Watcher`, expose TLS/HTTP-3 via CLI, graceful QUIC drain, and QUIC/UDP tuning | `gateway.go`, `serve.go`, `flags.go`, `config/watch.go` |

Read [`ROADMAP.md`](ROADMAP.md) for how the phases sequence across themes and
where the dependencies are.

## How each theme file is organised

Every theme file follows the same spine so an implementer can skim to the part
they need:

1. **Problem statement** — what is missing, in one paragraph.
2. **Why it matters here** — the exact code that makes this real, with line refs.
3. **Design overview** — the shape of the solution and the seam it plugs into.
4. **Phases → Tasks → Sub-tasks** — the ordered work breakdown.
5. **Micro-POC** — a small, self-contained Go (or shell) sketch that could
   actually run against this codebase's real types.
6. **Diagrams** — at least one `mermaid` block (renders on GitHub).
7. **Data definitions** — SQL DDL and/or a `mermaid classDiagram`, where the
   feature has persistent or structured state.
8. **Acceptance criteria** — how you know a phase is done.
9. **Risks & backward-compatibility** — what could break, and the invariant
   that keeps existing `claude_toolkit` installs working untouched.

## Cross-cutting design rules (non-negotiable, inherited from the codebase)

These are the conventions every proposal here already respects; an implementer
must keep respecting them:

- **The default install must not change behaviour.** Existing configs (the
  capitalised `"Providers"`/`"Router"` keys the toolkit writes) must route
  identically with every feature *off*. New behaviour is opt-in via a new,
  omittable config section or CLI flag — the exact pattern
  `auth.go`'s empty-key-list-disables-auth already uses.
- **Never leak a secret.** `proxy.go` and `logging/redact.go` set the bar: no
  `api_key` in an error, a log line, a cache key, a metric label, or a trace
  attribute, ever.
- **Never corrupt an in-flight SSE stream.** A streaming response commits its
  status and starts flushing immediately; any new feature (cache, hedge, retry)
  must make its decision *before* `streamAnthropicSSE` starts, exactly as
  `doUpstreamWithRetry` is careful to (`internal/gateway/messages.go:307-317`).
- **Portable Go, standard library first.** The logging package's "no
  third-party dependency" stance (`internal/logging/logging.go:14-23`) is the
  house style; every new dependency in this dossier is called out explicitly.

## Provenance

Grounded in a full read of `internal/gateway`, `internal/router`,
`internal/translate`, `internal/config`, `internal/proxy`, `internal/logging`,
`cmd/ccr`, `docs/ARCHITECTURE.md`, and `test/PORTING-MATRIX.md`, plus current
best-practice research (OpenTelemetry GenAI semantic conventions; quic-go HTTP/3
tuning; semantic-cache similarity-threshold tradeoffs; Google "tail at scale"
request hedging; OpenAI Responses / Gemini `generateContent` wire formats;
token-budget-aware LLM load balancing). Sources are cited inline in each theme.
