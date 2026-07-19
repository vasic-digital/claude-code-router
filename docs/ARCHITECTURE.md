# Architecture

This document describes the components and data flow of `claude-code-router` (Go), as they exist in the repository today, plus the remaining PLANNED pieces. Every diagram distinguishes **Implemented** components/edges from **PLANNED** ones (dashed, labelled).

## Component graph

```mermaid
graph TD
    CC["Claude Code<br/>(Anthropic Messages API client)"]
    CCR["cmd/ccr<br/>start / ui / serve / web / stop<br/>(Implemented)"]
    MGMT["cmd/ccr management server<br/>127.0.0.1:3458 default<br/>own /health, placeholder /<br/>(Implemented, minimal)"]

    subgraph gw["internal/gateway (Implemented)"]
        Server["Server<br/>gateway.go"]
        Compress["compressionMiddleware<br/>+ altSvcMiddleware<br/>compress.go"]
        Handler["handleMessages<br/>messages.go"]
        Seams["Router / Upstream seams<br/>(narrow local interfaces)<br/>messages.go:29-39"]
        Wiring["wiring.go: WireDefaults()<br/>routerAdapter + upstreamAdapter"]
    end

    Config["internal/config<br/>Config / Provider / Route<br/>(Implemented)"]
    Translate["internal/translate<br/>AnthropicToOpenAI<br/>StripCacheControl<br/>(Implemented)"]
    Router["internal/router<br/>Select() — haiku-tier aware<br/>(Implemented)"]
    Proxy["internal/proxy<br/>Client.Do() — streaming-safe<br/>timeout, no-secret-leak errors<br/>(Implemented)"]
    Logging["internal/logging<br/>PLANNED — empty directory"]
    Upstream["Upstream provider<br/>(OpenAI-compatible<br/>chat-completions API)"]

    CC -- "HTTP/1.1, HTTP/2, or HTTP/3" --> Server
    CCR -- "config.Load + gateway.New(Options{Port:3456})<br/>+ WireDefaults(0) + Start()" --> Server
    CCR --> MGMT
    Server --> Compress
    Compress --> Handler
    Server -- "reads at boot" --> Config
    Handler -- "Server.Router" --> Seams
    Handler -- "Server.Upstream" --> Seams
    Handler -- "AnthropicToOpenAI /<br/>response translation" --> Translate
    Seams -- "installed by" --> Wiring
    Wiring -- "routerAdapter wraps" --> Router
    Wiring -- "upstreamAdapter wraps" --> Proxy
    Proxy -- "POST, Authorization: Bearer" --> Upstream
    Router --> Config
    Router --> Translate

    Logging -. "PLANNED: not called from<br/>any package yet" .-> Server

    classDef implemented fill:#1f6f43,stroke:#0f3d25,color:#fff;
    classDef planned fill:#6b6b6b,stroke:#333,color:#fff,stroke-dasharray: 4 3;
    class Server,Compress,Handler,Seams,Wiring,Config,Translate,Router,Proxy,CCR,MGMT implemented;
    class Logging planned;
```

**Reading this diagram:** `internal/gateway/messages.go` declares its own narrow `Router`/`Upstream` interfaces plus minimal in-package default implementations, so the gateway package compiles and serves correctly on its own, without importing `internal/router`/`internal/proxy` directly (`internal/gateway/messages.go:19-27`, `29-39`). A separate file in the *same* package, `internal/gateway/wiring.go`, adapts the real `internal/router.Select` and `internal/proxy.Client` onto those seams via `Server.WireDefaults(timeout)`. **`cmd/ccr` always calls `WireDefaults`** before starting a gateway (`cmd/ccr/serve.go:44-51`), so a CLI-launched gateway gets the full haiku-tier-aware routing and streaming-safe upstream client — the minimal built-ins (`defaultRouter`/`defaultUpstream`) only matter if `internal/gateway` is used as a library directly, without also calling `WireDefaults`. `internal/logging` is not called from anywhere yet. `cmd/ccr` also runs a second, much smaller HTTP server (the "management" interface, fixed default `127.0.0.1:3458`) alongside the gateway — it is a separate `net/http.ServeMux` in `cmd/ccr/management.go`, not part of `internal/gateway` at all.

## Request sequence: `POST /v1/messages`, as launched via `cmd/ccr`

```mermaid
sequenceDiagram
    autonumber
    participant CC as Claude Code
    participant MW as compressionMiddleware
    participant H as handleMessages
    participant R as routerAdapter<br/>(wraps router.Select)
    participant T as translate.AnthropicToOpenAI
    participant U as upstreamAdapter<br/>(wraps proxy.Client.Do)
    participant P as Upstream provider

    CC->>MW: POST /v1/messages<br/>(Anthropic JSON, Accept-Encoding)
    MW->>H: forward (wraps response writer<br/>if compression negotiated)
    H->>H: decode body -> AnthropicRequest<br/>[400 on bad JSON]
    H->>R: Route(req)
    Note over R: model contains "haiku" AND<br/>Router.background set?<br/>-> Router.background<br/>else -> Router.default<br/>else -> fallback: first provider,<br/>first model
    R-->>H: (Provider, model) or error<br/>[503 if no route]
    H->>T: AnthropicToOpenAI(req, Options{<br/>CleanCache, StreamOptions,<br/>EnsureToolParameters:true, Model})
    T-->>H: OpenAIRequest or error<br/>[400 e.g. unsupported image block]
    H->>H: json.Marshal(OpenAIRequest)<br/>[500 on encode failure]
    alt non-streaming request
        H->>H: ctx = context.WithTimeout(UpstreamTimeout)
    else streaming request
        H->>H: ctx = request context, no context deadline added here
    end
    H->>U: Do(ctx, provider, body)
    Note over U: proxy.Client's Transport.ResponseHeaderTimeout<br/>bounds only the header wait — for a streaming<br/>call this is the ONLY timeout in play
    U->>P: POST provider.APIBaseURL<br/>Authorization: Bearer key<br/>(Authorization never echoed into any error)
    P-->>U: HTTP response (2xx or error)
    U-->>H: *http.Response or transport error<br/>[502 on transport error]
    alt upstream status >= 400
        H->>CC: forward status code,<br/>Anthropic error envelope
    else non-streaming (stream:false)
        H->>H: respondNonStreaming:<br/>OpenAI JSON -> AnthropicMessage
        H->>MW: 200, JSON body
        MW->>CC: (br/gzip-encoded if negotiated)
    else streaming (stream:true)
        loop each upstream SSE chunk
            H->>H: streamAnthropicSSE:<br/>map chunk -> Anthropic event(s)
            H->>CC: event: ...\ndata: ...\n\n<br/>(flushed immediately)
        end
        H->>CC: message_delta, message_stop
    end
```

Sources: `internal/gateway/messages.go:178-244` (orchestration), `258-318` (error mapping), `322-382` (non-streaming), `384-547` (streaming); `internal/gateway/wiring.go` (adapters); `cmd/ccr/serve.go:44-51` (the `WireDefaults` call). Verified end-to-end by `internal/gateway/messages_test.go`, `internal/router/router_test.go`, `internal/proxy/proxy_test.go`.

## Request sequence: `internal/gateway` used as a library, without `WireDefaults`

If you build `gateway.New(cfg, opt)` yourself and skip `srv.WireDefaults(0)`, you get the package's own minimal built-ins instead — this is what `internal/gateway` falls back to on its own, and is **not** what `cmd/ccr` does:

```mermaid
sequenceDiagram
    autonumber
    participant CC as Claude Code
    participant H as handleMessages
    participant R as defaultRouter
    participant U as defaultUpstream
    participant P as Upstream provider

    CC->>H: POST /v1/messages
    H->>R: Route(req)
    Note over R: resolves Router.default ONLY —<br/>no haiku-tier check, no fallback<br/>to a first-provider default
    R-->>H: (Provider, model) or error
    H->>U: Do(ctx, provider, body)
    Note over U: plain net/http call, http.DefaultClient<br/>if none supplied — no ResponseHeaderTimeout,<br/>so a streaming call has NO timeout protection at all
    U->>P: POST (Authorization: Bearer key)
    P-->>U: response
    U-->>H: *http.Response
    H-->>CC: (translation/streaming/error handling identical<br/>to the CLI-wired path — only routing/timeout differ)
```

Sources: `internal/gateway/messages.go:41-82` (`defaultRouter`, `defaultUpstream`).

## Transport negotiation

### Protocol selection (evaluated once, at `Start()`)

```mermaid
stateDiagram-v2
    [*] --> Configuring: gateway.New(cfg, Options)
    Configuring --> CheckHTTP3: Start() called

    CheckHTTP3 --> Error: EnableHTTP3 == true<br/>AND (CertFile == "" OR KeyFile == "")
    Error --> [*]: return error<br/>"HTTP/3 requires TLS"<br/>(gateway.go:143-147)

    CheckHTTP3 --> ServeH3AndTLS: EnableHTTP3 == true<br/>AND CertFile/KeyFile set
    CheckHTTP3 --> ServeTLSOnly: EnableHTTP3 == false<br/>AND CertFile/KeyFile set
    CheckHTTP3 --> ServePlainHTTP: CertFile == "" AND KeyFile == ""

    ServeH3AndTLS --> Serving: http3.Server on UDP port<br/>+ h1h2 ListenAndServeTLS on TCP port<br/>+ Alt-Svc header on every response
    ServeTLSOnly --> Serving: h1h2 ListenAndServeTLS<br/>(HTTP/1.1 + HTTP/2 via ALPN "h2")<br/>no Alt-Svc header
    ServePlainHTTP --> Serving: h1h2 ListenAndServe<br/>(HTTP/1.1 only)<br/>no Alt-Svc header

    Serving --> [*]: Shutdown(ctx)
```

Source: `internal/gateway/gateway.go:135-168` (`Start`), `internal/gateway/compress.go:120-128` (`altSvcMiddleware`, registered only when `EnableHTTP3`). Tested at `internal/gateway/gateway_test.go:165-192`. **Note:** `cmd/ccr` always calls `gateway.New(cfg, gateway.Options{Port: defaultGatewayPort})` with no TLS fields set (`cmd/ccr/serve.go:46`) — so in practice, a CLI-launched gateway always takes the `ServePlainHTTP` branch today. `CertFile`/`KeyFile`/`EnableHTTP3` are only reachable via direct library use; see `docs/USER_GUIDE.md` §5.

### Content-encoding negotiation (evaluated per-request)

```mermaid
stateDiagram-v2
    [*] --> ParseHeader: request arrives,<br/>read Accept-Encoding

    ParseHeader --> NoEncoding: header absent or empty
    ParseHeader --> Tokenize: header present

    Tokenize --> EvaluateTokens: split on comma,<br/>trim, parse ;q= weight<br/>per token (case-insensitive)

    EvaluateTokens --> BrotliAcceptable: "br" token present<br/>with q != 0
    EvaluateTokens --> GzipOnly: "gzip" token present<br/>with q != 0, no usable "br"
    EvaluateTokens --> NoEncoding: neither concrete token<br/>acceptable (e.g. only "*",<br/>"identity", or q=0'd out)

    BrotliAcceptable --> EncodeBrotli: brotli.NewWriter wraps<br/>the response writer
    GzipOnly --> EncodeGzip: gzip.NewWriter wraps<br/>the response writer

    EncodeBrotli --> SetHeaders: Content-Encoding: br<br/>Vary: Accept-Encoding<br/>Content-Length: (removed)
    EncodeGzip --> SetHeaders2: Content-Encoding: gzip<br/>Vary: Accept-Encoding<br/>Content-Length: (removed)
    NoEncoding --> PassThrough: response written<br/>uncompressed, headers untouched

    SetHeaders --> FlushPerWrite: every Flush() call flushes<br/>the compressor, not just the socket<br/>(critical for SSE)
    SetHeaders2 --> FlushPerWrite
    FlushPerWrite --> Close: handler returns -><br/>compressor Close() flushes trailer
    PassThrough --> [*]
    Close --> [*]
```

Source: `internal/gateway/compress.go:39-118` (`negotiate`, `compressionMiddleware`). Negotiation matrix tested exhaustively at `internal/gateway/gateway_test.go:27-47` (e.g. `"br;q=0.1, gzip;q=0.9"` still resolves to brotli — preference is by capability, not `q`). This middleware wraps **every** response on the gateway (3456) — the separate management server (3458, `cmd/ccr/management.go`) does not use it and sends plain, uncompressed responses.

## `cmd/ccr` process/service lifecycle

```mermaid
stateDiagram-v2
    [*] --> NotRunning
    NotRunning --> Detaching: ccr start / ccr ui
    Detaching --> Running: spawnDetached (setsid) launches<br/>"ccr serve" as a child;<br/>PID/host/port written to<br/>~/.claude-code-router/service.json
    NotRunning --> RunningForeground: ccr serve / ccr web<br/>(blocks this terminal)
    RunningForeground --> NotRunning: SIGINT/SIGTERM -><br/>graceful shutdown (10s grace)

    Running --> Running: ccr start / ui again while<br/>tracked PID is alive -> refused,<br/>reports existing PID (exit 1)
    Running --> NotRunning: ccr stop -> SIGTERM,<br/>poll up to 5s, SIGKILL if still alive,<br/>pidfile removed
    Running --> StaleTracked: tracked process dies<br/>on its own (crash, OOM, ...)
    StaleTracked --> NotRunning: ccr stop (or a new start)<br/>detects dead PID, cleans up<br/>the stale pidfile
```

Source: `cmd/ccr/service.go` (pidfile read/write/remove, `processAlive` via signal 0, `spawnDetached` via `setsid` on Unix), `cmd/ccr/serve.go:80-95` (signal handling + graceful shutdown). Tested at `cmd/ccr/main_test.go:67-86`.

## Config data model

```mermaid
classDiagram
    class Config {
        +Provider[] Providers
        +Route Router
        +Validate() error
        +ProviderByName(name) *Provider
    }
    class Provider {
        +string Name
        +string APIBaseURL
        +string APIKey
        +string[] Models
        +Transformer* Transformer
        +Has(name string) bool
    }
    class Transformer {
        +string[] Use
    }
    class Route {
        +string Default
        +string Background
        +string Think
        +string LongContext
    }
    class SplitRoute {
        <<function>>
        +SplitRoute(route string) (provider, model string, err error)
    }

    Config "1" *-- "0..*" Provider : Providers
    Config "1" *-- "1" Route : Router
    Provider "1" o-- "0..1" Transformer : Transformer
    Route ..> SplitRoute : default/background/think/longContext\nparsed as "provider,model"
    Provider ..> SplitRoute : referenced by name

    note for Route "Default/Background drive routing\n(internal/router.Select, wired in by\ncmd/ccr). Think/LongContext are\nvalidated but unconsumed — PLANNED."
    note for Transformer "Known values: \"cleancache\", \"streamoptions\".\nstreamoptions is fully wired end-to-end.\ncleancache is read into Options.CleanCache\nbut nothing currently consumes that field —\nsee docs/FAQ.md Q5."
```

Source: `internal/config/config.go:31-76` (types), `internal/config/config.go:122-172` (`Validate`, `SplitRoute`), `internal/config/config.go:174-182` (`ProviderByName`).

## Why the gateway package doesn't import `internal/router`/`internal/proxy` directly

This is a deliberate seam, not an oversight, and worth calling out architecturally: `internal/router`, `internal/proxy`, and `internal/gateway` were built in parallel by separate efforts against the same `internal/config`/`internal/translate` foundations. Rather than have `internal/gateway` depend on the exact API shape `internal/router`/`internal/proxy` might settle on, `internal/gateway/messages.go` defines its **own** narrow interfaces (`Router`, `Upstream` — `internal/gateway/messages.go:29-39`) and ships minimal working default implementations, so the gateway package is independently testable and functional on its own. A later file in the same package, `internal/gateway/wiring.go`, adapts the real packages onto those seams (`routerAdapter`, `upstreamAdapter`, `Server.WireDefaults`), and `cmd/ccr` always calls it (`cmd/ccr/serve.go:44-51`). The seam still matters for anyone using `internal/gateway` as a library directly: skip the `WireDefaults` call and you silently get `Router.default`-only routing and an upstream client with no header-timeout protection — see `docs/FAQ.md` Q10/Q10a for the exact behavioural differences, and `docs/USER_GUIDE.md` §4.2 for how the CLI closes the gap.

## Explicitly out of scope (not merely unimplemented)

`test/PORTING-MATRIX.md` — produced by porting the *behavioural intent* of the upstream Node router's own test suites into this Go module — draws a hard line between "missing but wanted" (**GAP**, tracked by a Go test) and "an entire upstream subsystem this router never intended to replicate" (**N/A**). The N/A list is architecturally significant and unchanged: no Electron-style core/gateway process split, no billing telemetry, no ToolHub/MCP runtime, no OAuth provider-plugin auth, no router rules DSL / `ModelRegistry` / `RoutePolicyEngine`, no "Fusion" vendor-specific routing, no hosted web-search bridging, and no protocols beyond Anthropic Messages → OpenAI chat-completions (no OpenAI Responses, no Gemini).

Several GAPs the matrix file originally tracked as missing have since landed, at least partially — the file's own prose predates this and is stale on these specific rows; trust the code:

- **Explicit per-request provider/model selector** — landed and **live**: `router.Select` resolves a `"provider,model"`/`"provider/model"`-shaped request `model` field before any other routing rule (`internal/router/selector.go`, `internal/router/router.go:26-30`, `50-58`).
- **Corporate/authenticated outbound proxy** — landed, **partially wired**: environment-variable proxying (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`) is automatic for every `proxy.Client` (`internal/proxy/upstream_proxy.go`); an authenticated custom proxy (`proxy.NewWithUpstreamProxy`) is implemented and tested but not wired into `WireDefaults`, and `config.json` has no `proxy` section to configure it from.
- **Retry/fallback on a failed upstream call** — the classification/backoff *policy* is implemented and tested (`internal/router/fallback.go`: `ClassifyStatus`, `ClassifyTransportError`, `BuildExecutionPlan`, `NextFallbackProvider`, `FallbackRetryDelayAfterStatus`/`...AfterNetworkError`), but nothing calls these to actually drive a multi-attempt retry loop yet — a failed upstream call still just fails once.
- **Inbound gateway authentication** — `gateway.RequireAPIKey(keys []string)` is implemented and tested (`internal/gateway/auth.go`) but is an opt-in `gin.HandlerFunc` factory that neither `gateway.go`'s route table nor `cmd/ccr` installs anywhere — the CLI-launched gateway remains unauthenticated by default.
- **Provider protocol/type field** — still genuinely unimplemented: every configured provider is treated as OpenAI-chat-completions-shaped, unconditionally.

See `docs/FAQ.md` Q10, Q28, Q29 for the operator-facing version of this.

## Summary: implemented vs. planned

| Layer | Status |
|---|---|
| Config load/validate | Implemented (`internal/config`) |
| Request translation (Anthropic → OpenAI) | Implemented (`internal/translate`) |
| Response translation (OpenAI → Anthropic, buffered + SSE) | Implemented, but lives in `internal/gateway/messages.go`, not `internal/translate` |
| `cache_control` stripping | Implemented as a function (`translate.StripCacheControl`, `json.Number`-safe); not called from `internal/gateway/messages.go` — `cleancache` reaches `Options.CleanCache`, but nothing currently reads that field (`docs/FAQ.md` Q5) |
| Gateway transport (HTTP/1.1, HTTP/2, HTTP/3, compression) | Implemented; TLS/HTTP-3 not yet CLI-exposed |
| `GET /health`, `GET /ready`, `POST /v1/messages` | Implemented |
| CLI (`cmd/ccr`: `start`/`ui`/`serve`/`web`/`stop`, pidfile service management) | Implemented |
| Separate management control-plane server | Implemented, deliberately minimal (own `/health`, placeholder `/`) |
| Full haiku-aware routing + streaming-safe upstream client, live in the CLI-launched gateway | Implemented, via `Server.WireDefaults` (always called by `cmd/ccr`) |
| Same, for `internal/gateway` used as a **library** without calling `WireDefaults` | Falls back to minimal built-ins — a real, permanent seam, not a gap to be closed |
| `Router.think` / `Router.longContext` routing behaviour | PLANNED (config accepts and validates the fields; nothing consumes them) |
| Structured logging (`internal/logging`) | PLANNED (empty directory) |
| Explicit per-request provider/model selector (`"provider,model"`/`"provider/model"` in the request `model` field) | Implemented and live (`internal/router/selector.go`, wired into `router.Select`) |
| Environment-variable outbound proxy (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`) | Implemented and live for every `proxy.Client` (`internal/proxy/upstream_proxy.go`) |
| Authenticated custom outbound proxy | Implemented as a library function (`proxy.NewWithUpstreamProxy`); not wired into `WireDefaults`; no `config.json` section to configure it |
| Retry/fallback classification policy (which failures to retry, backoff schedule, execution planning) | Implemented and tested (`internal/router/fallback.go`); nothing calls it to drive an actual retry loop yet |
| Inbound gateway authentication | Implemented as an opt-in library function (`gateway.RequireAPIKey`); not installed on any route by `gateway.go` or `cmd/ccr` — unauthenticated by default |
| Provider protocol/type field (every provider currently treated as OpenAI-chat-completions-shaped) | GAP — not built |
| Multi-protocol (OpenAI Responses, Gemini), ToolHub/MCP, billing, rules DSL, hosted web search, Electron core/gateway split | **N/A — out of scope by design**, not merely unimplemented |
