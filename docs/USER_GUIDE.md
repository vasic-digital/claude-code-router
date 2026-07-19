# User Guide

This guide covers installing, configuring, and running `claude-code-router` (Go), plus troubleshooting. It is grounded entirely in the code present in this repository as of the state described below; anything not yet implemented is marked **PLANNED**.

> **Read this first.** `internal/gateway/messages.go` — the handler for `POST /v1/messages`, which decodes an Anthropic request, routes it, translates it to OpenAI shape, calls the upstream, and translates the response back (buffered or streamed) — is implemented and tested (`internal/gateway/messages.go`, `internal/gateway/messages_test.go`). What is **still absent** is `cmd/ccr`, the CLI entrypoint — so there is no way to launch the gateway as a standalone process today; everything below that talks about invoking a `ccr` binary describes the **target, PLANNED** shape, built from the `Options`/`Config` types that already exist and are tested. One more nuance worth knowing before you configure anything: the live `POST /v1/messages` handler currently uses its own **minimal built-in** routing/upstream-call logic (`Router.default` only, no haiku-tier awareness; a plain `net/http` call with no special timeout handling) rather than the fuller, independently-tested `internal/router`/`internal/proxy` packages — see §4.1.

## 1. Install

### 1.1 PLANNED: `go install`

Once `cmd/ccr` contains a `main` package:

```bash
go install github.com/vasic-digital/claude-code-router/cmd/ccr@latest
```

### 1.2 Available today: build from source

```bash
git clone https://github.com/vasic-digital/claude-code-router.git
cd claude-code-router
go build ./...      # compiles every package that currently has one (no cmd/ccr binary yet)
go test ./...        # runs the full test suite
```

Requires Go **1.26.4** or compatible (`go.mod:3`). Direct dependencies: `github.com/gin-gonic/gin` v1.12.0 (HTTP routing), `github.com/quic-go/quic-go` v0.60.0 (HTTP/3), `github.com/andybalholm/brotli` v1.2.2 (compression) (`go.mod:5-9`).

## 2. Configuration

### 2.1 File location

`internal/config.Dir()` resolves the configuration directory (`internal/config/config.go:78-91`):

- **Linux/macOS**: `~/.claude-code-router`
- **Windows**, when `%APPDATA%` is set: `%APPDATA%\claude-code-router`
- Any platform where `os.UserHomeDir()` fails: falls back to the relative path `.claude-code-router`

The config file itself is `config.json` inside that directory (`internal/config/config.go:94`), i.e. `~/.claude-code-router/config.json` on Linux/macOS.

### 2.2 File shape

The JSON schema is intentionally **byte-compatible** with the upstream Node router, including capitalised top-level keys. This is the exact fixture the test suite pins against — the shape `claude_toolkit`'s `cma_run_provider` writes at launch (`internal/config/config_test.go:21-35`):

```json
{
  "Providers": [
    {
      "name": "chutes",
      "api_base_url": "https://llm.chutes.ai/v1/chat/completions",
      "api_key": "sk-test",
      "models": ["zai-org/GLM-5.2-TEE", "Qwen/Qwen3.6-27B-TEE"],
      "transformer": {"use": ["cleancache", "streamoptions"]}
    }
  ],
  "Router": {
    "default": "chutes,zai-org/GLM-5.2-TEE",
    "background": "chutes,Qwen/Qwen3.6-27B-TEE"
  }
}
```

Field reference (`internal/config/config.go:31-76`):

| JSON key | Go field | Required | Notes |
|---|---|---|---|
| `Providers[].name` | `Provider.Name` | Yes | Must be non-empty and unique across the file |
| `Providers[].api_base_url` | `Provider.APIBaseURL` | Yes | Must start with `http://` or `https://`; must be the **complete** endpoint URL (e.g. ending in `/chat/completions`) — the proxy client posts to it verbatim (`internal/proxy/proxy.go:49-53`) |
| `Providers[].api_key` | `Provider.APIKey` | No (but needed for a real upstream) | Sent as `Authorization: Bearer <key>` (`internal/proxy/proxy.go:70`) |
| `Providers[].models` | `Provider.Models` | No | Used by the fallback route when `Router` is empty (`internal/router/router.go:73-86`) |
| `Providers[].transformer.use` | `Provider.Transformer.Use` | No | List of transformer names; known values today: `cleancache`, `streamoptions` (`internal/config/config.go:43-49`) |
| `Router.default` | `Route.Default` | No | `"provider,model"` string |
| `Router.background` | `Route.Background` | No | `"provider,model"` string; used for Claude Code's cheap/background ("haiku") tier |
| `Router.think` | `Route.Think` | No | Accepted and validated; **not yet consulted by routing logic** (PLANNED) |
| `Router.longContext` | `Route.LongContext` | No | Accepted and validated; **not yet consulted by routing logic** (PLANNED) |

### 2.3 Loading and validation behaviour

`Load(path)` (`internal/config/config.go:96-118`):

- **File missing** → returns an empty, valid `*Config{}` and **no error**. The gateway is designed to boot in this state and report "not configured" via `/health`/`/ready`, rather than refusing to start.
- **File present but malformed JSON** → returns an error. This is deliberate: silently continuing with a half-parsed config risks routing requests to the wrong upstream.
- **File present, valid JSON, but fails `Validate()`** → returns an error. `Validate()` checks (`internal/config/config.go:122-155`):
  - every provider has a non-empty, unique `name`;
  - every provider's `api_base_url` is non-empty and starts with `http://`/`https://`;
  - every non-empty `Router` route (`default`, `background`, `think`, `longContext`) parses as `"provider,model"` and references a provider that actually exists in `Providers`.

### 2.4 Route string syntax

A route is `"provider,model"`. Only the **first** comma is the separator — everything after it, including further commas, is the model id verbatim (`internal/config/config.go:157-172`, tested at `internal/config/config_test.go:110-124`). This matters for providers whose model ids legitimately contain commas.

## 3. Provider setup walkthrough

1. Pick a provider that exposes an OpenAI-compatible chat-completions endpoint (this is the norm for the ~20 providers this router targets).
2. Find the **complete** chat-completions URL — not just the host. For example, `https://api.deepseek.com/chat/completions`, not `https://api.deepseek.com`. Getting this wrong is the single most common misconfiguration; see `docs/FAQ.md`.
3. Add a `Providers[]` entry with `name`, `api_base_url`, `api_key`, and the `models` you intend to route to.
4. If the upstream rejects Anthropic-only fields like `cache_control` with a hard error, add `"transformer": {"use": ["cleancache"]}`. **Caveat:** as currently wired, this flag has no observable effect on the outgoing request — see `docs/FAQ.md` Q5 for the exact gap (the byte-level `translate.StripCacheControl` function that would enforce it is never called from `internal/gateway/messages.go`). In practice this rarely matters, since the typed request conversion already drops `cache_control` for every content path it models; it would only bite if `cache_control` were hiding inside a raw, unmodelled JSON blob such as a tool's `input_schema`.
5. If you stream and want token-usage numbers on the final SSE chunk, add `"streamoptions"` to the same `use` array — this sets `stream_options.include_usage` on the outgoing OpenAI request only while streaming (`internal/translate/anthropic.go:195-197`, tested at `internal/translate/anthropic_test.go:173-190`).
6. Point `Router.default` (and optionally `Router.background`) at `"<name>,<model>"`.
7. If you configure **only** providers and no `Router` block at all, the router falls back to the first provider in the file and the first entry in its `models` list (`internal/router/router.go:73-86`) — convenient for a single-provider setup, but be aware of the implicit choice.

### 3.1 Background ("haiku") routing

Claude Code sends a different, cheaper model id for background work (summarisation, title generation, etc.) — an id that *contains* the substring `haiku` (e.g. `claude-3-5-haiku-20241022`), not one that equals it exactly. `router.Select` detects this with a case-insensitive substring match and prefers `Router.background` when it is set; if `Router.background` is empty, background requests fall through to `Router.default` like any other request (`internal/router/router.go:26-34`, `65-71`). This means a single-route config remains valid and works for both tiers.

### 3.2 `claude_toolkit` compatibility

If you already run [`claude_toolkit`](https://github.com/vasic-digital)'s multi-account setup, `claude-providers.sh`'s `cma_run_provider` already writes `config.json` in exactly the shape this router expects — that shape is the literal fixture pinned by `internal/config/config_test.go:18-35`. No config changes should be required to point an existing toolkit-managed provider alias at this Go gateway instead of the Node one, once the gateway's `POST /v1/messages` endpoint exists.

## 4. Running the gateway (PLANNED — pending `cmd/ccr`)

There is no CLI in the repository yet — `cmd/ccr` is an empty directory. The gateway's HTTP surface itself, though, is fully implemented and independently runnable from within the module (e.g. from a test, or a temporary `main.go` during development):

```go
cfg, err := config.Load(config.Path())
if err != nil { /* handle */ }
srv := gateway.New(cfg, gateway.Options{})
if err := srv.Start(); err != nil { /* handle */ }
// srv now serves GET /health, GET /ready, POST /v1/messages on 127.0.0.1:3456.
```

### 4.1 The gateway's built-in routing/upstream logic (important)

`internal/gateway/messages.go` intentionally does **not** import `internal/router` or `internal/proxy` — those packages are owned and tested separately. Instead it defines two small local interfaces and wires in minimal default implementations so the gateway works standalone (`internal/gateway/messages.go:19-82`):

- **`Router`** — `defaultRouter` resolves `cfg.Router.Default` only. It does **not** implement the haiku-tier/background-route heuristic that `internal/router.Select` provides (`internal/router/router.go:26-34`) — every request, including Claude Code's cheap background-tier calls, is routed to `Router.default` regardless of `Router.background`.
- **`Upstream`** — `defaultUpstream` is a plain `net/http.Client` call with no `ResponseHeaderTimeout` set (it falls back to `http.DefaultClient` when no client is supplied) and none of `internal/proxy.Client`'s explicit no-secret-leak error handling beyond what the standard library already gives you.

Both fields are exported on `*gateway.Server` (`Server.Router`, `Server.Upstream`) precisely so a caller who wants the fuller behaviour can swap them in after `gateway.New` and before `Start()`:

```go
srv := gateway.New(cfg, gateway.Options{})
// srv.Router = <adapter around internal/router.Select>   // PLANNED wiring
// srv.Upstream = <adapter around internal/proxy.Client>  // PLANNED wiring
srv.Start()
```

Whether `cmd/ccr` performs this swap once it exists is unconfirmed — it does not exist yet. Until it does (or until you wire it yourself), a live gateway only routes to `Router.default` and never to `Router.background`, `Router.think`, or `Router.longContext`.

### 4.2 Target CLI shape (PLANNED)

Once `cmd/ccr` exists, the intended shape (built from `internal/gateway.Options`, `internal/gateway/gateway.go:35-47`) is expected to be something like:

```bash
ccr start                     # binds 127.0.0.1:3456 by default
ccr start --host 0.0.0.0 --port 8787
ccr start --cert server.crt --key server.key            # enables TLS (h1/h2)
ccr start --cert server.crt --key server.key --http3     # + HTTP/3 over QUIC
```

**Do not treat these flag names as final** — they are inferred from the `Options` struct fields, not read from `cmd/ccr` source, which does not exist yet. `internal/gateway.New` applies these defaults when the corresponding option is zero (`internal/gateway/gateway.go:70-89`):

| Option | Default |
|---|---|
| `Host` | `127.0.0.1` |
| `Port` | `3456` (matches the Node implementation and every existing toolkit config — `internal/gateway/gateway_test.go:194-201`) |
| `UpstreamTimeout` | 10 minutes (bounds only the wait for upstream response headers, never a streaming body — see §5) |

Once started, point Claude Code's base URL at the gateway (exact env var/flag TBD until `cmd/ccr`/Claude Code integration is finalised):

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:3456 claude
```

## 5. TLS and HTTP/3

- Plain HTTP on `127.0.0.1` is the default because that is what Claude Code and the existing toolkit expect out of the box; TLS/HTTP-3 are opt-in (`internal/gateway/gateway.go:12-16`).
- Setting **both** `CertFile` and `KeyFile` enables TLS for the HTTP/1.1 and HTTP/2 listener (`internal/gateway/gateway.go:142`, `154-160`).
- `EnableHTTP3` additionally serves QUIC on the same address and advertises it via an `Alt-Svc: h3=":<port>"; ma=86400` response header on every request (`internal/gateway/compress.go:120-128`).
- **HTTP/3 requires TLS.** QUIC has no cleartext mode. If `EnableHTTP3` is set without both `CertFile` and `KeyFile`, `Start()` returns an explicit error rather than silently downgrading to HTTP/1.1 — silently downgrading would misreport the transport actually in use (`internal/gateway/gateway.go:143-147`, tested at `internal/gateway/gateway_test.go:165-174`).
- When TLS is enabled but `EnableHTTP3` is not, the `Alt-Svc` header is never sent — the gateway never advertises a capability it doesn't have (`internal/gateway/gateway_test.go:176-184`).

Generating a self-signed certificate for local testing:

```bash
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt -days 365 -nodes -subj "/CN=localhost"
```

## 6. Content-encoding negotiation

Every response passes through `compressionMiddleware` (`internal/gateway/compress.go:84-118`), which:

1. Parses the request's `Accept-Encoding` header, splitting on commas, trimming whitespace, and honouring `;q=` weights — a token with `q=0` is treated as explicitly unacceptable (`internal/gateway/compress.go:44-81`, tested at `internal/gateway/gateway_test.go:27-47`).
2. Picks **brotli** if the client accepts it at all (regardless of any listed `q` preference versus gzip — brotli is preferred purely because it compresses JSON/SSE better), otherwise **gzip**, otherwise sends the response uncompressed.
3. Sets `Content-Encoding` to the chosen value, adds `Vary: Accept-Encoding`, and removes any `Content-Length` header (the compressed body length differs from the original — an uncorrected `Content-Length` would make clients truncate or hang) (`internal/gateway/compress.go:103-107`).
4. Flushes the compressor (not just the socket) on every `Flush()` call, which matters for SSE: without it, streamed tokens sit in the compression buffer and the client sees nothing until the stream ends (`internal/gateway/compress.go:28-37`).

No client action is required — this happens for every response, including `/health` and `/ready`.

## 7. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Gateway won't start, error mentions "HTTP/3 requires TLS" | `EnableHTTP3` set without both `CertFile` and `KeyFile` | Supply both, or drop `EnableHTTP3` |
| `GET /ready` returns 503 with `"no providers configured"` | `config.json` missing or has an empty `Providers` array | Add at least one provider |
| `GET /ready` returns 503 with `"no default route configured"` | `Router.default` is unset — this check looks **only** at `Router.default`, not at whether a provider has models (`internal/gateway/gateway.go:120-124`) | Set `Router.default` |
| Config fails to load with a JSON parse error | Malformed `config.json` (trailing comma, unclosed bracket, etc.) | Validate the JSON; a missing file is fine, but broken JSON is a hard error by design |
| Config fails to load with `"api_base_url must be http(s)"` | A `api_base_url` uses a non-`http(s)` scheme, or is missing entirely | Use the full `https://…` chat-completions URL |
| Config fails to load with `"duplicate provider name"` | Two `Providers[]` entries share a `name` | Rename one |
| Config fails to load with `"references unknown provider"` | A `Router` route names a provider not present in `Providers[]` | Fix the typo, or add the missing provider |
| `POST /v1/messages` returns `503` with `{"error":{"type":"not_found_error",...}}` | No route resolvable — `Router.default` empty, or it names a provider not in `Providers[]` (`internal/gateway/messages.go:47-60`, `191-195`) | Fix `Router.default` |
| `POST /v1/messages` returns `502` (`api_error`) | Upstream transport failure, malformed upstream JSON, or an upstream response with zero `choices` (`internal/gateway/messages.go:227-244`, `332-339`) | Check the named provider's `api_base_url` and reachability |
| `POST /v1/messages` returns the upstream's own 4xx/5xx | The gateway preserves the upstream's exact status code rather than collapsing everything to a generic error, so Claude Code's retry/backoff logic sees the real signal (`internal/gateway/messages.go:258-262`) | Treat it like a normal upstream API error for that provider |
| Upstream call fails but the error never shows the API key | Verified behaviour of the standalone `internal/proxy.Client` (`internal/proxy/proxy.go:61-64`, `internal/proxy/proxy_test.go:175-217`). The live gateway's built-in `defaultUpstream` (`internal/gateway/messages.go:62-82`) is a much smaller code path with no equivalent dedicated test — treat its error-safety as unconfirmed until `internal/proxy.Client` is wired in as `Server.Upstream` (see §4.1) | Check the provider name/base URL named in the error, then verify the key out-of-band |
| A vision/image request fails immediately with an explicit error | Image content blocks are not translated yet — a deliberate hard failure, not a silent drop, so the model is never asked to answer about a picture it never saw (`internal/translate/anthropic.go:260-265`) | Not supported yet; see `docs/FAQ.md` |
| A **streaming** response never times out, even against a wedged upstream | By design in the live handler: `handleMessages` only applies `UpstreamTimeout` to the request context for **non-streaming** calls; a streaming call's context carries no added deadline at all (`internal/gateway/messages.go:217-225`) | Expected; rely on the upstream's own behaviour or a network-level timeout if you need one |
| A **non-streaming** request is cut off after `UpstreamTimeout`, even though headers came back quickly | Also by design, but note this differs from the standalone `internal/proxy.Client`: the live handler's `UpstreamTimeout` bounds the *entire* non-streaming call via `context.WithTimeout` (`internal/gateway/messages.go:222-224`), not just the wait for response headers the way `internal/proxy.New`'s `ResponseHeaderTimeout` does (`internal/proxy/proxy.go:26-44`) | Raise `UpstreamTimeout` if your provider's non-streaming responses are slow to fully arrive |

For the underlying reasoning behind each of these behaviours, see `docs/FAQ.md`. For deployment concerns (systemd, Docker, firewalling, backups), see `docs/ADMIN_MANUAL.md`.
