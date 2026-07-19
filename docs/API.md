# API Reference

This is the non-interactive HTTP reference for the **gateway's** three routes, all registered in `internal/gateway/gateway.go:121-162` and served on `127.0.0.1:3456` by default — independently configurable via `--gateway-host`/`--gateway-port` (`CCR_GATEWAY_HOST`/`CCR_GATEWAY_PORT`), see `docs/USER_GUIDE.md` §4. All three are un-versioned in the URL path (the API version is implicit — `/v1/messages` mirrors Anthropic's own path). `GET /health` and `GET /ready` are always unauthenticated by design (a supervisor must be able to probe them regardless of auth configuration); `POST /v1/messages` has route-scoped API-key middleware mounted (`RequireAPIKey`, `internal/gateway/gateway.go:161`) but it is unconfigurable from `cmd/ccr` today, so it is unauthenticated too in practice — see "Authentication" below.

> **Not covered here:** `cmd/ccr` also runs a second, separate HTTP server — the "management" interface, `127.0.0.1:3458` by default (`--host`/`--port`/`CCR_WEB_HOST`/`CCR_WEB_PORT`) — with its own, differently-shaped `GET /health` (`{"providers":N,"service":"ccr-management","status":"ok"}`) and a placeholder `GET /` HTML page. It is a separate `net/http.ServeMux` in `cmd/ccr/management.go`, described in its own code comment as deliberately minimal (a real web UI is out of scope for now). See `docs/USER_GUIDE.md` §4 and `docs/ADMIN_MANUAL.md` §8.

| Method | Path | Purpose | Status |
|---|---|---|---|
| `GET` | `/health` | Liveness probe | Implemented |
| `GET` | `/ready` | Readiness probe | Implemented |
| `POST` | `/v1/messages` | Anthropic-compatible chat completion, translated and routed to an upstream provider | Implemented |

Every response, on every route, passes through the compression middleware described in [Headers](#headers) below — this includes `/health` and `/ready`, not just `/v1/messages`.

---

## `GET /health`

Liveness only. Always `200` once the process is accepting connections; says nothing about whether any configured provider is actually reachable (`internal/gateway/gateway.go:105-110`).

**Request:** no body, no parameters.

**Response — `200 OK`:**

```json
{"status": "ok", "providers": 2}
```

- `status` is always the literal string `"ok"`.
- `providers` is `len(cfg.Providers)` — the count of providers currently loaded, not the count that are reachable.

**curl:**

```bash
curl -s http://127.0.0.1:3456/health | jq
```

---

## `GET /ready`

Readiness: green only when the router could actually resolve a request today (`internal/gateway/gateway.go:113-127`).

**Request:** no body, no parameters.

**Responses:**

| Condition | Status | Body |
|---|---|---|
| At least one provider configured **and** `Router.default` non-empty | `200` | `{"status": "ready"}` |
| `Providers` array is empty | `503` | `{"status": "no providers configured"}` |
| Providers exist but `Router.default` is empty | `503` | `{"status": "no default route configured"}` |

**curl:**

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3456/ready
```

> This check looks only at `Router.default`. The router that a CLI-launched gateway actually uses, `internal/router.Select` (wired in by `cmd/ccr` via `Server.WireDefaults` — `internal/gateway/wiring.go`), *additionally* falls back to the first provider's first model when no `Router` block is configured at all (`internal/router/router.go:73-86`) — so in that one specific case, `/ready` can report `503` even though `POST /v1/messages` would actually succeed. (A gateway built as a library without `WireDefaults` has no such fallback, and `/ready` matches its built-in `defaultRouter` exactly.) See `docs/FAQ.md` Q10/Q10a.

---

## `POST /v1/messages`

The Anthropic Messages API-compatible endpoint Claude Code actually talks to. Implemented in `internal/gateway/messages.go:189-271` (`handleMessages`), which delegates the actual upstream call to a retry loop, `doUpstreamWithRetry` (`internal/gateway/messages.go:294-383` — see "Processing pipeline" below). Route-scoped middleware, `RequireAPIKey`, also sits in front of this handler (`internal/gateway/gateway.go:161`) — see "Authentication" below.

### Processing pipeline

1. Read and JSON-decode the request body into an `AnthropicRequest` (`internal/translate.AnthropicRequest`). A read failure or invalid JSON → `400`.
2. **Route** the request via `Server.Router.Route(&in)` to a `(config.Provider, model string)` pair — on a CLI-launched gateway this is `internal/router.Select`'s haiku-tier-aware policy (see `docs/FAQ.md` Q10). Failure (no route configured / route names an unknown provider) → `503`.
3. **Translate** Anthropic → OpenAI via `translate.AnthropicToOpenAI`, with per-provider options derived from the routed provider's `transformer.use` list (`CleanCache`, `StreamOptions`) plus `EnsureToolParameters` **always on** and `Model` set to the routed model id. A translation failure (e.g. an image content block) → `400`.
4. JSON-encode the translated request. An encode failure → `500` (should not happen for a request that already parsed and translated successfully).
5. For **non-streaming** requests only, wrap the outbound call's context in a deadline of `Options.UpstreamTimeout` (default 10 minutes). Streaming requests get no added deadline.
6. Call the routed provider via `Server.Upstream.Do(ctx, provider, body)`. A transport-level failure (DNS, connection refused, context deadline, etc.) → `502`.
7. If the upstream responds with a status `>= 400`, forward it (see [Error shape](#error-shape) below) preserving the exact status code.
8. Otherwise, translate the OpenAI-shaped upstream response back to Anthropic shape — buffered (`respondNonStreaming`) or streamed (`streamAnthropicSSE`) depending on the request's `"stream"` field — and write it to the client.

### Request body

The same `AnthropicRequest` shape documented in `internal/translate/anthropic.go:32-50` — this is the Anthropic Messages API request schema:

```json
{
  "model": "claude-3-5-sonnet-20241022",
  "max_tokens": 1024,
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user", "content": "hi"}
  ],
  "tools": [
    {"name": "get_weather", "description": "...", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}
  ],
  "temperature": 0.7,
  "top_p": 0.9,
  "stop_sequences": ["DONE"],
  "stream": false
}
```

| Field | Type | Notes |
|---|---|---|
| `model` | string | Overridden on the outgoing upstream request by the model the router resolved — the client's own `model` value only affects **routing** (via the haiku-tier substring check, when the fuller router is wired in), not which upstream model id is actually requested. |
| `max_tokens` | int | Passed through verbatim. |
| `system` | string \| block array | Flattened to plain text and sent as a leading `role:"system"` message upstream. |
| `messages[]` | array | `role` + `content` (string or content-block array: `text`, `tool_use`, `tool_result`; `image` is rejected — see `docs/FAQ.md` Q12). |
| `tools[]` | array | `name`, `description`, `input_schema`. An empty/absent `input_schema` is backfilled to `{"type":"object","properties":{}}` before being sent upstream (always on for this endpoint). |
| `temperature`, `top_p` | float, optional | Passed through. |
| `stop_sequences` | []string, optional | Renamed to OpenAI's `stop` upstream. |
| `stream` | bool | Selects [streaming](#streaming-response) vs [buffered](#non-streaming-response) response handling. |

### Non-streaming response

**`200 OK`**, `Content-Type: application/json` (Gin's default for `c.JSON`), body shape (`internal/gateway/messages.go:101-110`, `322-382`):

```json
{
  "id": "chatcmpl-1",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "text", "text": "Hello there"}
  ],
  "model": "fake-model",
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {"input_tokens": 5, "output_tokens": 3}
}
```

This exact shape is asserted end-to-end by test, including the routed model id appearing both in the request the fake upstream receives and in the translated response (`internal/gateway/messages_test.go:57-102`).

| Field | Notes |
|---|---|
| `id` | The upstream's own response `id`; falls back to the literal `"msg_unknown"` if the upstream didn't send one. |
| `content[]` | A `text` block if the upstream message had non-empty content, followed by one `tool_use` block per upstream tool call (`input` defaults to `{}` if the upstream sent empty/invalid JSON arguments). |
| `stop_reason` | Mapped from the upstream's `finish_reason`: `"length"` → `"max_tokens"`, `"tool_calls"` → `"tool_use"`, anything else (including absent) → `"end_turn"`. Anthropic has no `content_filter` equivalent, so that upstream value also maps to `"end_turn"` rather than inventing a new stop reason (`internal/gateway/messages.go:159-171`). |
| `stop_sequence` | Always `null` — the upstream response does not carry which stop sequence (if any) was matched. |
| `usage` | `input_tokens`/`output_tokens` copied from the upstream's `usage.prompt_tokens`/`usage.completion_tokens`, or zero if the upstream sent no `usage` object. |

### Streaming response

**`200 OK`**, sent when the request body has `"stream": true`. Headers (`internal/gateway/messages.go:405-411`):

```
Content-Type: text/event-stream; charset=utf-8
Cache-Control: no-cache
Connection: keep-alive
```

The connection is opened and flushed **before** the first event, and every subsequent event is flushed individually as it's produced — this is asserted by a flush-counting test, not just a Content-Type check (`internal/gateway/messages_test.go:106-194`).

Event sequence, matching Anthropic's own Messages streaming protocol:

```
event: message_start
data: {"type":"message_start","message":{"id":"","type":"message","role":"assistant","content":[],"model":"fake-model","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
```

(Reproduced from the exact fixture and assertions in `internal/gateway/messages_test.go:106-194` — a 2-chunk `"Hello"` text stream.)

| Event | When | Notes |
|---|---|---|
| `message_start` | Once, on the first content delta (or, for a degenerate stream that goes straight to `[DONE]`, still emitted before `message_stop` — `internal/gateway/messages_test.go:199-220`) | Carries an initially-empty `message` object. |
| `content_block_start` | Whenever a new content block opens (a text run, or a new tool call) | `index` increments per block; a text block and a tool-call block never share an index. |
| `content_block_delta` | Per upstream chunk carrying content | `delta.type` is `"text_delta"` (with `text`) for text, or `"input_json_delta"` (with `partial_json`) for incrementally-arriving tool-call arguments. |
| `content_block_stop` | Whenever the current block closes (a different block starts, or the stream ends) | |
| `message_delta` | Once, after the last content block closes | Carries the mapped `stop_reason` and the **final** cumulative `usage`, taken from whichever upstream chunk included a `usage` object (typically the last one). |
| `message_stop` | Once, terminal event | Stream closes after this. |

The upstream's `data: [DONE]` line ends the read loop (OpenAI convention); a malformed individual `data:` line from the upstream is silently skipped rather than aborting the stream — by the time a bad line arrives, `200` and headers have already been sent, so there is no status code left to change (`internal/gateway/messages.go:483-535`).

### Error shape

Every error response — from the gateway itself or forwarded from an upstream — uses the same body shape (`internal/gateway/messages.go:246-256`):

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "human-readable description"
  }
}
```

Verified end-to-end for both an upstream-mapped error and a malformed-upstream-body error (`internal/gateway/messages_test.go:224-284`, `286-310`).

#### Status codes originating in the gateway

| Status | `error.type` | Cause |
|---|---|---|
| `400` | `invalid_request_error` | Unreadable request body, invalid request JSON, or a translation failure (e.g. an unsupported `image` content block) |
| `503` | `not_found_error` | No route could be resolved (`Router.default` unset, or it names an unknown provider) |
| `502` | `api_error` | Upstream transport failure (network error, DNS, timeout); or the upstream returned a `200` with malformed JSON, or with zero `choices` |
| `500` | `api_error` | Failed to JSON-encode the already-translated outgoing request (an internal encoding failure, not a client-input problem) |

#### Status codes forwarded from the upstream

When the upstream itself returns `>= 400`, the gateway preserves that **exact** status code — verified by test for both a `429` and a `500` (`internal/gateway/messages_test.go:224-259`) — and derives `error.type` from it (`internal/gateway/messages.go:293-309`):

| Upstream status | `error.type` |
|---|---|
| `401` | `authentication_error` |
| `403` | `permission_error` |
| `404` | `not_found_error` |
| `429` | `rate_limit_error` |
| `400` or `422` | `invalid_request_error` (unless the upstream body already specified a different `error.type`, which is preserved instead) |
| any `5xx` | `api_error` |
| anything else | `api_error` (default) |

`error.message` is extracted from the upstream's own body when it matches either `{"error":{"message":...,"type":...}}` or `{"message":...}` (most OpenAI-compatible upstreams use one of these); otherwise the raw upstream body (up to 64 KiB) is used verbatim, so no information is silently dropped just because the shape is unrecognised (`internal/gateway/messages.go:262-291`).

### curl examples

**Non-streaming:**

```bash
curl -s http://127.0.0.1:3456/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "hi"}]
  }' | jq
```

**Streaming** (SSE — `curl -N` disables buffering so events print as they arrive):

```bash
curl -N -s http://127.0.0.1:3456/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "stream": true,
    "messages": [{"role": "user", "content": "hi"}]
  }'
```

**Compressed** (add brotli negotiation):

```bash
curl -s --compressed http://127.0.0.1:3456/v1/messages \
  -H 'Accept-Encoding: br' \
  -H 'Content-Type: application/json' \
  -d '{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'
```

---

## Headers

Applies to **every** route, including `/health` and `/ready` (`internal/gateway/gateway.go:98-101`, `internal/gateway/compress.go:84-118`).

### `Content-Encoding` / `Vary`

Set only when the client's `Accept-Encoding` negotiates to something other than identity (`internal/gateway/compress.go:39-118`):

- `br` if the client's `Accept-Encoding` lists brotli as acceptable at all (a `q=0` token is treated as unacceptable; brotli otherwise wins regardless of any listed `q=` preference vs. gzip).
- `gzip` if brotli isn't acceptable but gzip is.
- Neither header is set (response is uncompressed) if the client sends no `Accept-Encoding`, sends only unsupported/wildcard tokens, or explicitly excludes both.

When a `Content-Encoding` is applied, `Vary: Accept-Encoding` is always added, and any `Content-Length` header is always **removed** — the compressed body length differs from the original, so a stale `Content-Length` would make clients truncate the response or hang (`internal/gateway/compress.go:103-107`).

| Request `Accept-Encoding` | Response `Content-Encoding` |
|---|---|
| (absent) | (none) |
| `identity` | (none) |
| `gzip` | `gzip` |
| `br` | `br` |
| `gzip, br` | `br` (brotli wins even listed second) |
| `br;q=0.1, gzip;q=0.9` | `br` (preference is by capability, not `q`) |
| `br;q=0, gzip` | `gzip` (`q=0` excludes brotli) |
| `*` | (none — wildcard is not a concrete encoding) |

(Full negotiation matrix tested at `internal/gateway/gateway_test.go:27-47`.)

### `Alt-Svc`

Set to `h3=":<port>"; ma=86400` on **every** response, but **only** when the server was constructed with `Options.EnableHTTP3 = true` (`internal/gateway/compress.go:120-128`, `internal/gateway/gateway.go:99-101`). Absent entirely otherwise — the gateway never advertises a transport it isn't actually serving (`internal/gateway/gateway_test.go:176-192`).

### `Content-Type`

- `GET /health`, `GET /ready`, and the non-streaming `POST /v1/messages` response: `application/json; charset=utf-8` (Gin's `c.JSON` default).
- The streaming `POST /v1/messages` response: `text/event-stream; charset=utf-8`, plus `Cache-Control: no-cache` and `Connection: keep-alive` (`internal/gateway/messages.go:407-409`).

---

## Summary status-code table

| Status | Route(s) | Meaning |
|---|---|---|
| `200` | all | Success |
| `400` | `POST /v1/messages` | Malformed request JSON, unreadable body, or a translation failure |
| `500` | `POST /v1/messages` | Internal encode failure on an already-valid translated request |
| `502` | `POST /v1/messages` | Upstream transport failure, or a malformed/empty `200` response from the upstream |
| `503` | `GET /ready`, `POST /v1/messages` | Not ready (no providers / no default route), or the router couldn't resolve a provider for this specific request |
| upstream's own `4xx`/`5xx` | `POST /v1/messages` | Forwarded verbatim from the provider, re-shaped into the Anthropic error envelope |
