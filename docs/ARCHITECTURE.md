# Architecture

This document describes the components and data flow of `claude-code-router` (Go), as they exist in the repository today, plus the PLANNED pieces needed to make it a runnable service. Every diagram distinguishes **Implemented** components/edges from **PLANNED** ones (dashed, labelled).

## Component graph

```mermaid
graph TD
    CC["Claude Code<br/>(Anthropic Messages API client)"]
    CCR["cmd/ccr<br/>PLANNED — empty directory,<br/>no main package"]

    subgraph gw["internal/gateway (Implemented)"]
        Server["Server<br/>gateway.go"]
        Compress["compressionMiddleware<br/>+ altSvcMiddleware<br/>compress.go"]
        Handler["handleMessages<br/>messages.go"]
        DefRouter["defaultRouter<br/>(Router.default only,<br/>no haiku/background)"]
        DefUpstream["defaultUpstream<br/>(plain net/http)"]
    end

    Config["internal/config<br/>Config / Provider / Route<br/>(Implemented)"]
    Translate["internal/translate<br/>AnthropicToOpenAI<br/>StripCacheControl<br/>(Implemented)"]
    Router["internal/router<br/>Select() — haiku-tier aware<br/>(Implemented, standalone)"]
    Proxy["internal/proxy<br/>Client.Do() — streaming-safe<br/>timeout, no-secret-leak errors<br/>(Implemented, standalone)"]
    Logging["internal/logging<br/>PLANNED — empty directory"]
    Upstream["Upstream provider<br/>(OpenAI-compatible<br/>chat-completions API)"]

    CC -- "HTTP/1.1, HTTP/2, or HTTP/3" --> Server
    CCR -. "PLANNED: config.Load + gateway.New + Start" .-> Server
    Server --> Compress
    Compress --> Handler
    Server -- "reads at boot" --> Config
    Handler -- "Server.Router (interface)" --> DefRouter
    Handler -- "Server.Upstream (interface)" --> DefUpstream
    Handler -- "AnthropicToOpenAI /<br/>response translation" --> Translate
    DefUpstream -- "POST, Authorization: Bearer" --> Upstream
    DefRouter -. "config.SplitRoute /<br/>ProviderByName" .-> Config

    Router -. "PLANNED: swap in as<br/>Server.Router before Start()" .-> Handler
    Proxy -. "PLANNED: swap in as<br/>Server.Upstream before Start()" .-> Handler
    Router --> Config
    Router --> Translate
    Proxy --> Config

    Logging -. "PLANNED: not called from<br/>any package yet" .-> Server

    classDef implemented fill:#1f6f43,stroke:#0f3d25,color:#fff;
    classDef planned fill:#6b6b6b,stroke:#333,color:#fff,stroke-dasharray: 4 3;
    class Server,Compress,Handler,DefRouter,DefUpstream,Config,Translate,Router,Proxy implemented;
    class CCR,Logging planned;
```

**Reading this diagram:** the solid box around `internal/gateway` is the only thing that runs end-to-end today. `internal/router` and `internal/proxy` are fully implemented and independently tested, but the gateway package deliberately does not import them (`internal/gateway/messages.go:19-27`) — it defines its own minimal `Router`/`Upstream` interfaces (`DefRouter`/`DefUpstream` above) so it works standalone. `Server.Router`/`Server.Upstream` are exported fields a caller can overwrite before `Start()` to get the fuller behaviour; whether `cmd/ccr` does that once it exists is unconfirmed. `internal/logging` is not called from anywhere yet.

## Request sequence (implemented path)

This is the sequence for the code that exists and is tested today — `POST /v1/messages` served through `defaultRouter`/`defaultUpstream`.

```mermaid
sequenceDiagram
    autonumber
    participant CC as Claude Code
    participant MW as compressionMiddleware
    participant H as handleMessages
    participant R as Server.Router<br/>(defaultRouter)
    participant T as translate.AnthropicToOpenAI
    participant U as Server.Upstream<br/>(defaultUpstream)
    participant P as Upstream provider

    CC->>MW: POST /v1/messages<br/>(Anthropic JSON, Accept-Encoding)
    MW->>H: forward (wraps response writer<br/>if compression negotiated)
    H->>H: decode body -> AnthropicRequest<br/>[400 on bad JSON]
    H->>R: Route(req)
    R-->>H: (Provider, model) or error<br/>[503 if no route]
    H->>T: AnthropicToOpenAI(req, Options{<br/>CleanCache, StreamOptions,<br/>EnsureToolParameters:true, Model})
    T-->>H: OpenAIRequest or error<br/>[400 e.g. unsupported image block]
    H->>H: json.Marshal(OpenAIRequest)<br/>[500 on encode failure]
    alt non-streaming request
        H->>H: ctx = context.WithTimeout(UpstreamTimeout)
    else streaming request
        H->>H: ctx = request context, no added deadline
    end
    H->>U: Do(ctx, provider, body)
    U->>P: POST provider.APIBaseURL<br/>Authorization: Bearer key<br/>Accept: text/event-stream if streaming
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

Sources: `internal/gateway/messages.go:178-244` (orchestration), `258-318` (error mapping), `322-382` (non-streaming), `384-547` (streaming). Verified end-to-end by `internal/gateway/messages_test.go`.

## Request sequence (PLANNED — full routing/proxy wiring)

If `cmd/ccr` (or any caller) swaps `Server.Router`/`Server.Upstream` for adapters around `internal/router.Select` and `internal/proxy.Client` before calling `Start()`, the sequence gains haiku-tier-aware routing and header-only upstream timeouts:

```mermaid
sequenceDiagram
    autonumber
    participant CC as Claude Code
    participant H as handleMessages
    participant R as internal/router.Select
    participant U as internal/proxy.Client.Do
    participant P as Upstream provider

    CC->>H: POST /v1/messages (model id may<br/>contain "haiku")
    H->>R: Select(cfg, req)
    Note over R: model contains "haiku" AND<br/>Router.background set?<br/>-> Router.background<br/>else -> Router.default<br/>else -> fallback: first provider,<br/>first model
    R-->>H: (Provider, model)
    H->>U: Do(ctx, provider, body, stream)
    Note over U: Transport.ResponseHeaderTimeout<br/>bounds only the header wait —<br/>never the streaming body
    U->>P: POST (Authorization never<br/>echoed into any error)
    P-->>U: response
    U-->>H: *http.Response
    H-->>CC: (as in the implemented sequence)
```

This diagram is a **design projection**, not a description of running code — no file in this repository currently performs this wiring. Sources for the individual behaviours: `internal/router/router.go:40-63`, `internal/proxy/proxy.go:26-84`.

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

Source: `internal/gateway/gateway.go:135-168` (`Start`), `internal/gateway/compress.go:120-128` (`altSvcMiddleware`, registered only when `EnableHTTP3`). Tested at `internal/gateway/gateway_test.go:165-192`.

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

Source: `internal/gateway/compress.go:39-118` (`negotiate`, `compressionMiddleware`). Negotiation matrix tested exhaustively at `internal/gateway/gateway_test.go:27-47` (e.g. `"br;q=0.1, gzip;q=0.9"` still resolves to brotli — preference is by capability, not `q`).

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

    note for Route "Only Default/Background currently\ndrive routing behaviour (internal/router\nand the gateway's defaultRouter).\nThink/LongContext are validated but\nunconsumed — PLANNED."
    note for Transformer "Known values: \"cleancache\", \"streamoptions\".\nMapped to translate.Options by\nrouter.TransformerOptions (standalone,\nnot wired into the live gateway) and,\nseparately, inline in messages.go\nvia Provider.Has(...)."
```

Source: `internal/config/config.go:31-76` (types), `internal/config/config.go:122-172` (`Validate`, `SplitRoute`), `internal/config/config.go:174-182` (`ProviderByName`).

## Why the gateway package doesn't import `internal/router`/`internal/proxy`

This is a deliberate seam, not an oversight, and worth calling out architecturally: three packages (`internal/router`, `internal/proxy`, `internal/gateway`) were built in parallel by separate efforts against the same `internal/config`/`internal/translate` foundations. Rather than have `internal/gateway` depend on the exact API shape `internal/router`/`internal/proxy` might settle on, `internal/gateway/messages.go` defines its **own** narrow interfaces (`Router`, `Upstream` — `internal/gateway/messages.go:29-39`) and ships minimal working default implementations, so the gateway is independently testable and functional before those integration decisions are finalised. The cost of this seam is that, as shipped, the live gateway's routing is `Router.default`-only (no haiku-tier awareness) and its upstream timeout semantics differ from `internal/proxy.Client`'s (a whole-call context deadline for non-streaming requests, vs. a response-header-only deadline) — see `docs/FAQ.md` Q10, Q10a, and Q18 for the exact behavioural differences, and `docs/USER_GUIDE.md` §4.1 for how to close the gap by assigning `Server.Router`/`Server.Upstream` before `Start()`.

## Summary: implemented vs. planned

| Layer | Status |
|---|---|
| Config load/validate | Implemented (`internal/config`) |
| Request translation (Anthropic → OpenAI) | Implemented (`internal/translate`) |
| Response translation (OpenAI → Anthropic, buffered + SSE) | Implemented, but lives in `internal/gateway/messages.go`, not `internal/translate` |
| `cache_control` stripping | Implemented as a function (`translate.StripCacheControl`); not observed to be called from `internal/gateway/messages.go` — `cleancache` is passed to `AnthropicToOpenAI` via `Options.CleanCache`, but that field is not read inside `AnthropicToOpenAI` itself (see `docs/FAQ.md` and the field's doc comment) |
| Gateway transport (HTTP/1.1, HTTP/2, HTTP/3, compression) | Implemented |
| `GET /health`, `GET /ready`, `POST /v1/messages` | Implemented |
| Full haiku-aware routing live in the gateway | PLANNED (library exists, not wired) |
| Streaming-safe/secret-safe upstream client live in the gateway | PLANNED (library exists, not wired) |
| `Router.think` / `Router.longContext` routing behaviour | PLANNED (config accepts and validates the fields; nothing consumes them) |
| CLI (`cmd/ccr`) | PLANNED (empty directory) |
| Structured logging (`internal/logging`) | PLANNED (empty directory) |
