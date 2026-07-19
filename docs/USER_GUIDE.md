# User Guide

This guide covers installing, configuring, and running `claude-code-router` (Go), plus troubleshooting. It is grounded entirely in the code present in this repository as of the state described below; anything not yet implemented is marked **PLANNED**.

> **Read this first.** `internal/gateway/messages.go` — the handler for `POST /v1/messages`, which decodes an Anthropic request, routes it, translates it to OpenAI shape, calls the upstream, and translates the response back (buffered or streamed) — is implemented and tested (`internal/gateway/messages.go`, `internal/gateway/messages_test.go`). `cmd/ccr` is a real, tested CLI (`start`/`ui`/`serve`/`web`/`stop`) — see §4. It launches the gateway with `Server.WireDefaults()` applied (`cmd/ccr/serve.go:44-51`), which installs the full haiku-tier-aware `internal/router.Select` and the streaming-safe `internal/proxy.Client` — so a CLI-launched gateway gets the fuller routing/upstream behaviour, not the gateway package's own minimal built-in defaults. See §4.1 for what that distinction means if you use `internal/gateway` as a library directly instead of through the CLI.

## 1. Install

`v0.1.0` is tagged and published as a [GitHub release](https://github.com/vasic-digital/claude-code-router/releases/tag/v0.1.0) — cross-compiled `linux`/`darwin`/`windows` × `amd64`/`arm64` archives plus `checksums.txt` (see `docs/RELEASE.md`). Download an artifact from there, or build from source:

```bash
git clone https://github.com/vasic-digital/claude-code-router.git
cd claude-code-router
go build -o bin/ccr ./cmd/ccr   # the CLI binary
go test ./...                    # full test suite
```

Once built, `bin/ccr --help` prints the full command grammar (reproduced in §4).

Requires Go **1.26.4** or compatible (`go.mod:3`). Direct dependencies: `github.com/gin-gonic/gin` v1.12.0 (HTTP routing), `github.com/quic-go/quic-go` v0.60.0 (HTTP/3), `github.com/andybalholm/brotli` v1.2.2 (compression) (`go.mod:5-9`).

## 2. Configuration

### 2.1 File location

`internal/config.Dir()` resolves the configuration directory (`internal/config/config.go:148-160`):

- **Linux/macOS**: `~/.claude-code-router`
- **Windows**, when `%APPDATA%` is set: `%APPDATA%\claude-code-router`
- Any platform where `os.UserHomeDir()` fails: falls back to the relative path `.claude-code-router`

The config file itself is `config.json` inside that directory (`internal/config/config.go:162`), i.e. `~/.claude-code-router/config.json` on Linux/macOS.

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

Field reference (`internal/config/config.go:46-144`):

| JSON key | Go field | Required | Notes |
|---|---|---|---|
| `Providers[].name` | `Provider.Name` | Yes | Must be non-empty and unique across the file |
| `Providers[].api_base_url` | `Provider.APIBaseURL` | Yes | Must start with `http://` or `https://`; must be the **complete** endpoint URL (e.g. ending in `/chat/completions`) — the proxy client posts to it verbatim (`internal/proxy/proxy.go:49-53`) |
| `Providers[].api_key` | `Provider.APIKey` | No (but needed for a real upstream) | Sent as `Authorization: Bearer <key>` (`internal/proxy/proxy.go:70`) |
| `Providers[].models` | `Provider.Models` | No | Used by the first-provider fallback and by bare-model resolution when no `Router.default` is set (`internal/router/router.go:72-86`) |
| `Providers[].transformer.use` | `Provider.Transformer.Use` | No | List of transformer names; known values today: `cleancache`, `streamoptions` (`internal/config/config.go:66-72`) |
| `Providers[].protocol` | `Provider.Protocol` | No | `"openai"` (default) or `"anthropic"`. **Absent** → inferred from `api_base_url` (an `api.anthropic.com`/`*.anthropic.com` host or an `/anthropic` path segment → `anthropic`; else `openai`). An `anthropic` provider receives the Anthropic-shaped request **unchanged** and its response is relayed back verbatim, instead of the OpenAI translation (`internal/config/config.go:87-130`, `internal/gateway/messages.go:233-295`). Any other value is a validation error |
| `Router.default` | `Route.Default` | No | `"provider,model"` string |
| `Router.background` | `Route.Background` | No | `"provider,model"` string; used for Claude Code's cheap/background ("haiku") tier |
| `Router.think` | `Route.Think` | No | Accepted and validated; routing branch is **wired and unit-tested but inert today** — no `thinking` signal reaches the router, so it never fires (see `docs/FAQ.md` Q11) |
| `Router.longContext` | `Route.LongContext` | No | Accepted and validated, and **live**: a request whose estimated prompt exceeds ~60000 tokens routes here when this is set (see `docs/FAQ.md` Q11) |

### 2.3 Loading and validation behaviour

`Load(path)` (`internal/config/config.go:170-186`):

- **File missing** → returns an empty, valid `*Config{}` and **no error**. The gateway is designed to boot in this state and report "not configured" via `/health`/`/ready`, rather than refusing to start.
- **File present but malformed JSON** → returns an error. This is deliberate: silently continuing with a half-parsed config risks routing requests to the wrong upstream.
- **File present, valid JSON, but fails `Validate()`** → returns an error. `Validate()` checks (`internal/config/config.go:190-233`):
  - every provider has a non-empty, unique `name`;
  - every provider's `api_base_url` is non-empty and starts with `http://`/`https://`;
  - every non-empty `Router` route (`default`, `background`, `think`, `longContext`) parses as `"provider,model"` and references a provider that actually exists in `Providers`;
  - every provider's `protocol`, if set, is one of `"openai"`/`"anthropic"` (an unrecognised value is a named error, not a silent fallback — `internal/config/config.go:206-215`).

### 2.4 Route string syntax

A route is `"provider,model"`. Only the **first** comma is the separator — everything after it, including further commas, is the model id verbatim (`internal/config/config.go:239-249`, tested at `internal/config/config_test.go:110-124`). This matters for providers whose model ids legitimately contain commas.

## 3. Provider setup walkthrough

1. Pick a provider that exposes an OpenAI-compatible chat-completions endpoint (this is the norm for the ~20 providers this router targets).
2. Find the **complete** chat-completions URL — not just the host. For example, `https://api.deepseek.com/chat/completions`, not `https://api.deepseek.com`. Getting this wrong is the single most common misconfiguration; see `docs/FAQ.md`.
3. Add a `Providers[]` entry with `name`, `api_base_url`, `api_key`, and the `models` you intend to route to.
4. If the upstream rejects Anthropic-only fields like `cache_control` with a hard error, add `"transformer": {"use": ["cleancache"]}`. This strips `cache_control` recursively from the whole outgoing request, including inside a tool's `input_schema` — the one place the typed request conversion cannot reach on its own, because `input_schema` travels through as an untouched `json.RawMessage` (`internal/translate/anthropic.go:441-461`, calling `StripCacheControl`; see `docs/FAQ.md` Q5).
5. If you stream and want token-usage numbers on the final SSE chunk, add `"streamoptions"` to the same `use` array — this sets `stream_options.include_usage` on the outgoing OpenAI request only while streaming (`internal/translate/anthropic.go:195-197`, tested at `internal/translate/anthropic_test.go:173-190`).
6. Point `Router.default` (and optionally `Router.background`) at `"<name>,<model>"`.
7. If you configure **only** providers and no `Router` block at all, the router falls back to the first provider in the file and the first entry in its `models` list (`internal/router/router.go:73-86`) — convenient for a single-provider setup, but be aware of the implicit choice.

### 3.1 Background ("haiku") routing

Claude Code sends a different, cheaper model id for background work (summarisation, title generation, etc.) — an id that *contains* the substring `haiku` (e.g. `claude-3-5-haiku-20241022`), not one that equals it exactly. `router.Select` detects this with a case-insensitive substring match and prefers `Router.background` when it is set; if `Router.background` is empty, background requests fall through to `Router.default` like any other request (`internal/router/router.go:26-34`, `65-71`). This means a single-route config remains valid and works for both tiers.

### 3.2 `claude_toolkit` compatibility

If you already run [`claude_toolkit`](https://github.com/vasic-digital)'s multi-account setup, `claude-providers.sh`'s `cma_run_provider` already writes `config.json` in exactly the shape this router expects — that shape is the literal fixture pinned by `internal/config/config_test.go:18-35`. No config changes should be required to point an existing toolkit-managed provider alias at this Go gateway instead of the Node one.

## 4. Running the gateway

`cmd/ccr` is a real, tested CLI. Build it once (`go build -o bin/ccr ./cmd/ccr`), then:

```bash
bin/ccr start          # background: gateway on 127.0.0.1:3456 + management on 127.0.0.1:3458
bin/ccr ui              # same as start, but also opens the management UI in a browser
bin/ccr serve            # foreground (blocks until Ctrl-C / SIGTERM) — alias: web
bin/ccr stop            # stops what "start"/"ui" started
```

Full grammar (verbatim from `bin/ccr --help`, sourced from `cmd/ccr/main.go:28-61`):

```
ccr - Claude Code Router

Usage:
  ccr start [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr ui    [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr serve [--host <host>] [--port <port>] [--open|--no-open] [--gateway|--no-gateway]
  ccr stop
  ccr <profile-name-or-id> [cli|app] [-- <agent args>]
```

(`start`/`ui`/`serve`/`web` additionally accept `--gateway-port <port>` and `--gateway-host <host>` — see below.)

Worked examples:

```bash
# Start in the background, without opening a browser.
bin/ccr start --no-open

# Run in the foreground under a process supervisor (see docs/ADMIN_MANUAL.md).
bin/ccr serve

# Run the router service but skip the Anthropic gateway (management UI only).
bin/ccr serve --no-gateway

# Put the management interface on a different host/port (e.g. to expose it
# on the LAN — think carefully about this, see docs/ADMIN_MANUAL.md §5).
bin/ccr start --host 0.0.0.0 --port 9000

# Same, via environment variables instead of flags (flags still win if both
# are given).
CCR_WEB_HOST=0.0.0.0 CCR_WEB_PORT=9000 bin/ccr start

# Put the GATEWAY itself on a different port, e.g. because something else
# already holds 3456. Works with "serve"/"web" directly and (via the
# environment-variable form only — see the note below) with "start"/"ui".
bin/ccr serve --gateway-port 3999
CCR_GATEWAY_PORT=3999 bin/ccr start

# Expose the gateway beyond loopback — e.g. inside a container, where
# 127.0.0.1 is the container's OWN loopback and a published port can never
# reach it otherwise. Think carefully before doing this outside a container;
# see docs/ADMIN_MANUAL.md §5.
bin/ccr serve --gateway-host 0.0.0.0

# Stop the background service.
bin/ccr stop
```

Once running, point Claude Code at the gateway:

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:3456 claude
```

**Important — two separate HTTP servers, two separate ports:**

| Server | Default address | Purpose | Endpoints |
|---|---|---|---|
| **Gateway** (`internal/gateway.Server`) | `127.0.0.1:3456` by default, configurable via `--gateway-host`/`--gateway-port`/`CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT` | The Anthropic-compatible API Claude Code talks to | `GET /health`, `GET /ready`, `POST /v1/messages` |
| **Management** (`cmd/ccr`'s own tiny server) | `127.0.0.1:3458` by default, configurable via `--host`/`--port`/`CCR_WEB_HOST`/`CCR_WEB_PORT` | Control-plane placeholder — a real web UI is out of scope for now | `GET /health` (own shape, see below), `GET /` (placeholder HTML page) |

`--host`/`--port` configure the **management** server; `--gateway-host`/`--gateway-port` configure the **gateway** (`cmd/ccr/flags.go:9-43`, `cmd/ccr/serve.go:46`) — the two have always been logically independent, but until this release the gateway's address was hardcoded. The gateway still defaults to `127.0.0.1` **on purpose**: it holds live provider API keys, so binding it to every interface has to be a deliberate act, not the default. Set `--gateway-host 0.0.0.0` explicitly inside a container — `127.0.0.1` there is the container's *own* loopback, unreachable from a published port no matter how it's mapped. `--gateway`/`--no-gateway` controls whether the gateway starts at all; `--open`/`--no-open` controls whether a browser is launched at the management URL.

**`ccr start`/`ui` do not forward `--gateway-host`/`--gateway-port` to the detached `serve` child** (`cmd/ccr/service.go:104-114` only forwards `--host`, `--port`, `--gateway`/`--no-gateway`, `--open`/`--no-open`). The flags are accepted and validated by `start`/`ui` but then silently dropped — only the `CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT` environment-variable form survives into the child (environment variables are inherited by the detached process; the flags are not re-passed). Use `ccr serve`/`web` directly, or the environment-variable form, until this is fixed.

The management server's `/health` has its **own**, differently-shaped body — don't confuse it with the gateway's:

```json
{"providers": 2, "service": "ccr-management", "status": "ok"}
```

(Verified live: `curl http://127.0.0.1:3458/health`, source `cmd/ccr/management.go:34-41`. Key order shown alphabetically because it's Go's `encoding/json` marshaling a `map[string]any`, which always sorts map keys.)

### 4.1 Background service lifecycle (`start`/`ui`/`stop`)

`start` and `ui` don't run the server themselves — they re-exec the same binary as `ccr serve` in a **fully detached** child process (`setsid` on Unix), then return immediately (`cmd/ccr/service.go:77-143`):

- The child's PID, host, port, `--gateway` flag, and start time are recorded in `~/.claude-code-router/service.json` — note this is the **management** host/port; the gateway's own `--gateway-host`/`--gateway-port` are neither recorded here nor forwarded to the child at all (see §4's note on that gap).
- The child's stdout/stderr are redirected to `~/.claude-code-router/service.log` — check this file if something goes wrong after `start` reports success, since there is no other way to see the child's own output.
- Running `start`/`ui` again while a tracked process is still alive is refused, reporting the existing PID and management URL, rather than starting a second instance (`cmd/ccr/service.go:93-96`).
- `stop` sends `SIGTERM`, polls for up to 5 seconds, then `SIGKILL`s if the process hasn't exited, and always removes the pidfile — including when the pidfile pointed at an already-dead process (a "stale pidfile", cleaned up and reported rather than silently ignored) (`cmd/ccr/service.go:145-184`).
- `stop` with no service running exits non-zero and prints `ccr is not running.` (verified: `internal/... cmd/ccr/main_test.go:67-77`).

### 4.2 The routing/upstream wiring, and when it matters

`internal/gateway/messages.go` defines its own narrow `Router`/`Upstream` interfaces with minimal in-package default implementations (`defaultRouter`/`defaultUpstream`), so the gateway package compiles and serves correctly on its own (`internal/gateway/messages.go:30-93`). A separate file, `internal/gateway/wiring.go`, adapts the real `internal/router.Select` (haiku-tier-aware routing) and `internal/proxy.Client` (streaming-safe timeout, secret-safe errors) onto those same interfaces via `Server.WireDefaults(timeout)`.

**`cmd/ccr` always calls `WireDefaults`** before starting the gateway (`cmd/ccr/serve.go:44-51`) — so every gateway started through the CLI (`start`/`ui`/`serve`/`web`) gets the fuller behaviour: haiku-tier requests route to `Router.background` when set, and the upstream client bounds only the wait for response headers rather than the whole call. This only matters to you if you use `internal/gateway` as a **library** directly (your own `main.go`, not `cmd/ccr`) and forget to call `srv.WireDefaults(0)` after `gateway.New` and before `Start()` — in that case you'd silently get the minimal built-ins instead (`Router.default`-only routing, a plain `net/http` call with no special timeout handling).

### 4.3 Validating and inspecting config (`ccr config`)

Two config subcommands ship in the binary (they are dispatched by `cmd/ccr/main.go` but are not part of the `--help` usage text, which is pinned to the upstream v3.0.6 grammar):

```bash
# Report EVERY structural problem in one pass; exit 0 iff valid, 1 otherwise.
bin/ccr config validate                 # defaults to ~/.claude-code-router/config.json
bin/ccr config validate ./staging.json  # or an explicit path

# Print the effective config as JSON with every api_key replaced by [REDACTED].
bin/ccr config show
```

`validate` uses a non-short-circuiting checker (`config.LoadForValidation` + `config.CheckAll`), so a config with several mistakes gets one complete report instead of a fix-one-rerun loop (`cmd/ccr/config_cmd.go`, `internal/config/validate_cmd.go`). `show` replaces each provider's `api_key` with the fixed marker `[REDACTED]` — the real key's bytes are never marshalled at all, so the output is safe to paste into a bug report or a screen share (`config.Redacted`). Neither command starts a server; both are pure functions over the file.

## 5. TLS and HTTP/3

`internal/gateway.Options` supports TLS and HTTP/3 (`internal/gateway/gateway.go:35-47`), but `cmd/ccr` does **not** currently expose `--cert`/`--key`/`--http3` flags — `cmdServe` always constructs `gateway.Options{Host: flags.GatewayHost, Port: flags.GatewayPort}` with no TLS fields set (`cmd/ccr/serve.go:46`). Reaching TLS/HTTP-3 today means using `internal/gateway` as a library directly, calling `gateway.New(cfg, gateway.Options{CertFile: ..., KeyFile: ..., EnableHTTP3: true})` yourself. Treat CLI flags for this as **PLANNED**.

- Plain HTTP on `127.0.0.1` is the default because that is what Claude Code and the existing toolkit expect out of the box; TLS/HTTP-3 are opt-in (`internal/gateway/gateway.go:12-16`).
- Setting **both** `CertFile` and `KeyFile` enables TLS for the HTTP/1.1 and HTTP/2 listener (`internal/gateway/gateway.go:219`, `233-234`).
- `EnableHTTP3` additionally serves QUIC on the same address and advertises it via an `Alt-Svc: h3=":<port>"; ma=86400` response header on every request (`internal/gateway/compress.go:120-128`).
- **HTTP/3 requires TLS.** QUIC has no cleartext mode. If `EnableHTTP3` is set without both `CertFile` and `KeyFile`, `Start()` returns an explicit error rather than silently downgrading to HTTP/1.1 — silently downgrading would misreport the transport actually in use (`internal/gateway/gateway.go:223`, tested at `internal/gateway/gateway_test.go:165-174`).
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
| `GET /ready` returns 503 with `"no default route configured"` | `Router.default` is unset — this check looks **only** at `Router.default`, not at whether a provider has models (`internal/gateway/gateway.go:175-179`) | Set `Router.default` |
| Config fails to load with a JSON parse error | Malformed `config.json` (trailing comma, unclosed bracket, etc.) | Validate the JSON; a missing file is fine, but broken JSON is a hard error by design |
| Config fails to load with `"api_base_url must be http(s)"` | A `api_base_url` uses a non-`http(s)` scheme, or is missing entirely | Use the full `https://…` chat-completions URL |
| Config fails to load with `"duplicate provider name"` | Two `Providers[]` entries share a `name` | Rename one |
| Config fails to load with `"references unknown provider"` | A `Router` route names a provider not present in `Providers[]` | Fix the typo, or add the missing provider |
| `POST /v1/messages` returns `503` with `{"error":{"type":"not_found_error",...}}` | No route resolvable — `Router.default` empty, or it names a provider not in `Providers[]` (`internal/gateway/messages.go:222-226`) | Fix `Router.default` |
| `POST /v1/messages` returns `502` (`api_error`) after a delay of a few seconds | Upstream transport failure retried up to `Options.MaxAttempts` times (default 3) with exponential backoff before giving up, or an upstream response with malformed JSON / zero `choices` (`internal/gateway/messages.go:344-416` — retry loop; `489-499` — malformed/empty response) | Check the named provider's `api_base_url` and reachability; the delay before the `502` is expected, not a hang |
| `POST /v1/messages` returns the upstream's own 4xx/5xx | The gateway preserves the upstream's exact status code rather than collapsing everything to a generic error, so Claude Code's retry/backoff logic sees the real signal. A `429`/`5xx` is retried internally first (see the row above); only a `Terminal` status (per `internal/router.ClassifyStatus`) or an exhausted retry budget is actually forwarded (`internal/gateway/messages.go:378-399`, `448-504`) | Treat it like a normal upstream API error for that provider |
| Upstream call fails but the error never shows the API key | If launched via `cmd/ccr` (which calls `WireDefaults`), this is the verified behaviour of `internal/proxy.Client` (`internal/proxy/proxy.go:61-64`, `internal/proxy/proxy_test.go:175-217`). If you construct `gateway.New` as a library **without** calling `WireDefaults`, you get the smaller built-in `defaultUpstream` (`internal/gateway/messages.go:77-93`) instead, which has no equivalent dedicated key-leak test — treat its error-safety as unconfirmed in that case | Check the provider name/base URL named in the error, then verify the key out-of-band |
| A request carrying an `x-api-key`/`Authorization` header is treated no differently from one without | `gateway.RequireAPIKey` is mounted on `POST /v1/messages` (`internal/gateway/gateway.go:201`), but `cmd/ccr` never populates `Options.APIKeys` — no CLI flag or `config.json` field exists for it yet — so the accepted-key list is always empty, which the middleware itself documents as "authentication disabled" (`internal/gateway/auth.go`) | Expected today; see README.md "Known limitations" |
| A vision/image request | Image content blocks (base64 or URL source) are converted to OpenAI `image_url` parts, including inside `tool_result` content — an unsupported media type, oversized payload, or malformed source is still a named `400` error rather than a silent drop (`internal/translate/anthropic.go:237-335`) | Not an error path any more for supported PNG/JPEG/GIF/WebP images; see `docs/FAQ.md` Q12 |
| A **streaming** response never times out against a wedged upstream that never sends anything at all | Depends on wiring: `handleMessages` itself only applies `UpstreamTimeout` to the request context for **non-streaming** calls, never for streaming (`internal/gateway/messages.go:248-256`). If launched via `cmd/ccr`, `internal/proxy.Client`'s `ResponseHeaderTimeout` (also set from `UpstreamTimeout`, default 10 minutes) separately bounds the wait for the upstream's *response headers* on a streaming call too — so a CLI-launched gateway times out a streaming call that never even gets headers, but once headers (and therefore the SSE stream) start, the body can keep flowing indefinitely (`internal/gateway/wiring.go:65-71`, `internal/proxy/proxy.go:26-44`). A gateway built as a library without `WireDefaults` has no such protection at all | Expected once streaming has started; if a request never gets a first byte back, expect it to fail after `UpstreamTimeout` when CLI-launched |
| A **non-streaming** request is cut off after `UpstreamTimeout`, even though headers came back quickly | By design: `handleMessages`'s `UpstreamTimeout` bounds the *entire* non-streaming call, retries included, via `context.WithTimeout` (`internal/gateway/messages.go:248-256`) regardless of wiring — this is stricter than `internal/proxy.Client`'s own `ResponseHeaderTimeout`, which only bounds the header wait (`internal/proxy/proxy.go:26-44`); the context deadline from `messages.go` is what actually governs a non-streaming call's total duration | Raise the gateway's `UpstreamTimeout` (currently not CLI-exposed — see §4.2/§5) if your provider's non-streaming responses are slow to fully arrive |

| `ccr start`/`ui` prints "ccr is already running (pid …)" | A tracked service is already alive per `~/.claude-code-router/service.json` | Use that instance, or `ccr stop` it first |
| `ccr stop` prints "ccr is not running." and exits non-zero | No pidfile, or the pidfile's process is already dead (a stale pidfile is cleaned up automatically either way) | Nothing to do — it's already stopped |
| `ccr start` reports success but the gateway/management server isn't actually reachable | The detached child's own errors (e.g. a port already in use by something else) go to `~/.claude-code-router/service.log`, not to `ccr start`'s own stdout, since the parent only confirms the child *process* launched, not that it bound successfully | Check `~/.claude-code-router/service.log` |
| Unsure whether you're hitting the gateway or the management interface | They're two different servers on two different ports/response shapes by default — see the table in §4 | `curl :3456/health` (gateway: `{"status":"ok","providers":N}`) vs. `curl :3458/health` (management: `{"status":"ok","service":"ccr-management","providers":N}`) |
| `ccr <name>` (anything not `start`/`ui`/`serve`/`web`/`stop`/`help`) prints `Profile "<name>" was not found or is disabled.` | This reimplementation has no profile store yet — every non-command first argument hits this path by design, matching the upstream CLI's own behaviour for an unknown profile | Use `start`/`ui`/`serve`/`web`/`stop` |

For the underlying reasoning behind each of these behaviours, see `docs/FAQ.md`. For deployment concerns (systemd, Docker, firewalling, backups), see `docs/ADMIN_MANUAL.md`.
