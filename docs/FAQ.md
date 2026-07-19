# FAQ

Every answer below is grounded in code in this repository. Where the honest answer is "not yet implemented," that is stated explicitly and marked **PLANNED**.

---

**Q1. What does this project actually do?**

It runs an HTTP gateway that Claude Code talks to using the Anthropic Messages API, and translates each request into an OpenAI-compatible chat-completions request for whichever upstream provider your `config.json` routes it to (`internal/translate/anthropic.go:1-6`). It is a clean-room Go reimplementation of the Node `@musistudio/claude-code-router` (`NOTICE:1-11`).

**Q2. Why does HTTP/3 require TLS?**

QUIC — the transport HTTP/3 runs on — has no cleartext mode; encryption is baked into the protocol itself, not layered on top the way TLS sits on top of TCP for HTTP/1.1/2. The gateway enforces this at startup: if `EnableHTTP3` is set without both `CertFile` and `KeyFile`, `Start()` returns an explicit error rather than silently falling back to plaintext HTTP/1.1 while still claiming HTTP/3 support — that would misreport the transport actually in use (`internal/gateway/gateway.go:127-132`, tested at `internal/gateway/gateway_test.go:165-174`).

**Q3. Why is brotli preferred over gzip?**

Because it compresses JSON and Server-Sent Events markedly better, and Claude Code's traffic is almost entirely one or the other. `negotiate()` returns `"br"` whenever the client's `Accept-Encoding` lists it as acceptable at all — it does not compare `q=` weights between brotli and gzip, only whether each is acceptable (`q=0` excludes it); brotli simply wins by capability, not preference score (`internal/gateway/compress.go:39-81`, tested explicitly with `"br;q=0.1, gzip;q=0.9"` still resolving to `br` at `internal/gateway/gateway_test.go:35`).

**Q4. Why are `Providers` and `Router` capitalised in `config.json`, when that's not idiomatic Go/JSON?**

Because that is the wire format the Node implementation, and every existing `claude_toolkit`-managed installation, already use. Renaming the keys to lowercase would be more idiomatic but would silently break every installed config on the day of upgrade — so the capitalisation is preserved exactly, on purpose (`internal/config/config.go:16-18`).

**Q5. What do the `cleancache` and `streamoptions` transformers do?**

- `cleancache` strips Anthropic-only `cache_control` blocks from the outgoing request. Upstreams that don't recognise the field tend to reject the **entire** request rather than ignore the unknown key, so every occurrence — at any nesting depth (system blocks, message content blocks, tool definitions) — has to go. `translate.StripCacheControl` walks the generic JSON tree recursively for exactly this reason (`internal/translate/anthropic.go:297-325`, tested at `internal/translate/anthropic_test.go:194-220`). It decodes with `json.Decoder.UseNumber()` rather than a plain `json.Unmarshal` into `any`, specifically so it never touches any number literal it passes through: a plain unmarshal converts every JSON number to `float64`, which both rejects any literal whose magnitude overflows `float64` (a fuzz-discovered case, `"1E700"`) and — worse — silently corrupts a large integer id like a 20-digit snowflake id into lossy scientific notation.
- `streamoptions` adds `stream_options.include_usage: true` to the OpenAI request, but **only** when the request is actually streaming — some upstreams 400 on a non-streaming request that carries `stream_options` at all. Without it, a streamed response would report no token usage whatsoever on completion (`internal/translate/anthropic.go:195-197`, tested at `internal/translate/anthropic_test.go:173-190`).

Both are opt-in per provider via `"transformer": {"use": [...]}`  (`internal/config/config.go:43-49`). `streamoptions` is fully wired end-to-end today: the live `POST /v1/messages` handler reads `provider.Has("streamoptions")` directly into `Options.StreamOptions` (`internal/gateway/messages.go:199`), and `AnthropicToOpenAI` acts on it.

`cleancache`, however, currently has **no observable effect on outgoing requests** as wired: `provider.Has("cleancache")` is read into `Options.CleanCache` (`internal/gateway/messages.go:198`), but `AnthropicToOpenAI` never reads that field in its body — the typed Anthropic→OpenAI struct conversion already drops `cache_control` naturally for every content path it models (`OpenAIMessage`/`OpenAITool` simply have no such field), and the dedicated byte-level function that *would* catch a `cache_control` key hiding inside an untouched `json.RawMessage` (e.g. a tool's `input_schema`) — `translate.StripCacheControl` — is not called anywhere in `internal/gateway/messages.go`. If you rely on `cleancache` to strip `cache_control` from a nested raw schema today, verify it isn't reaching the upstream by other means; treat full `cleancache` wiring as **PLANNED**. (The standalone `internal/router.TransformerOptions`, `internal/router/router.go:88-97`, has the same story: it maps the config flag onto `translate.Options.CleanCache`, but nothing downstream currently consumes that flag either.)

**Q6. Why must `api_base_url` be the complete URL rather than just a host?**

Because the proxy client posts to it **verbatim** — it never appends a path. `config.Provider`'s doc comment spells this out explicitly: the field is documented as already being the complete chat-completions endpoint, e.g. `https://api.deepseek.com/chat/completions` (`internal/config/config.go:33-36`), and `proxy.Client.Do` builds the outgoing request straight from it with no suffixing (`internal/proxy/proxy.go:49-53`, `66`). Configuring just the host would send requests to the provider's root URL and fail identically for every request.

**Q7. What happens if `config.json` is missing entirely?**

Nothing fails. `Load()` treats a missing file as an empty, valid config — zero providers, empty routes — and returns no error, so the gateway can still boot and report "nothing configured" through `/health`/`/ready` rather than refusing to start outright (`internal/config/config.go:102-108`, tested at `internal/config/config_test.go:70-78`).

**Q8. What happens if `config.json` exists but is malformed JSON?**

That **is** a hard error. A missing file is a legitimate "not configured yet" state; a broken file is not — silently continuing with a partially-parsed config risks routing real requests to the wrong upstream with the wrong credentials, which is worse than refusing to start (`internal/config/config.go:109-113`, tested at `internal/config/config_test.go:82-86`).

**Q9. What happens if the JSON is valid but semantically wrong** (e.g. a route pointing at a provider that doesn't exist)?

`Load()` calls `Validate()` after parsing, which also errors out — for a duplicate provider name, a missing/non-empty-but-non-http(s) `api_base_url`, a malformed route string, or a route naming a provider absent from `Providers[]` (`internal/config/config.go:114-118`, `122-155`; all six cases are individually tested at `internal/config/config_test.go:88-108`).

**Q10. How does the router decide which provider and model to use for a request?**

There are, as of this writing, **two different answers**, because the live gateway doesn't currently use the fuller of the two:

- `internal/router.Select` (`internal/router/router.go:40-63`) — the fuller, independently-tested implementation:
  1. If the incoming request's model id contains `"haiku"` (case-insensitive — Claude Code's cheap/background tier uses ids like `claude-3-5-haiku-20241022`, which *contain* rather than *equal* the tier name) **and** `Router.background` is set, use that route.
  2. Otherwise use `Router.default`.
  3. If the resulting route string is empty — no `Router` block at all — fall back to the **first** provider in `Providers[]` and the **first** entry in its `Models` list.
  4. Any failure along the way (unknown provider referenced, malformed route string, empty providers list, or a fallback provider with no models) returns an explicit error rather than guessing.
- `internal/gateway/messages.go`'s **built-in `defaultRouter`** — what `POST /v1/messages` actually uses today, unless a caller explicitly replaces `Server.Router` after `gateway.New`: it resolves `Router.default` only, with no haiku/background awareness and no fallback (`internal/gateway/messages.go:41-60`).

See Q10a for why, and `docs/ARCHITECTURE.md` for the full picture.

**Q10a. Why does the live gateway not just use `internal/router.Select`?**

`internal/gateway/messages.go` deliberately avoids importing `internal/router` (or `internal/proxy`): those packages are being developed independently in the same parallel effort, and the gateway package defines its own narrow `Router`/`Upstream` interfaces plus minimal default implementations so it works standalone regardless of the exact API those sibling packages settle on (`internal/gateway/messages.go:19-27`). `Server.Router` and `Server.Upstream` are exported specifically so a caller — expected to be `cmd/ccr`, once it exists — can swap in the fuller implementations. As of this writing that wiring has not happened, so a running gateway only ever routes to `Router.default`.

**Q11. What are `Router.think` and `Router.longContext` for?**

They exist in the config schema and are validated exactly like `default`/`background` (each must parse as `"provider,model"` and reference a real provider — `internal/config/config.go:139-153`), matching the upstream Node config's fields. However, `router.Select` does not currently branch on them at all (`internal/router/router.go:40-63`) — they can be set in `config.json` today, but nothing in the router yet reads them. **PLANNED.**

**Q12. Why do images currently error instead of being silently dropped?**

Because silently dropping an image and letting the model answer confidently about a picture it never saw is a worse failure mode than a loud, immediate error. `AnthropicToOpenAI` returns an explicit `"image content blocks are not supported yet"` error the moment it encounters a `type: "image"` content block, rather than skipping it (`internal/translate/anthropic.go:260-265`, tested at `internal/translate/anthropic_test.go:236-247`). Vision passthrough is not implemented yet.

**Q13. How does the system prompt survive translation?**

Anthropic carries it as a top-level `"system"` field (string or block array); OpenAI has no equivalent field — it expects a `role: "system"` message at the head of the `messages` array. `AnthropicToOpenAI` flattens the (possibly block-array) system field to plain text and, if non-empty, prepends it as the first OpenAI message (`internal/translate/anthropic.go:199-209`). Losing this conversion would silently strip the model's entire instruction set — the code comment calls this out as the single highest-stakes conversion in the file (`internal/translate/anthropic.go:8-13`).

**Q14. How are Anthropic tool calls translated?**

Anthropic represents them as `tool_use` content blocks inside an assistant message and `tool_result` blocks inside a user message. OpenAI instead uses `message.tool_calls` (with JSON arguments encoded as a **string**, not an object — an OpenAI quirk that must be preserved exactly or upstreams reject the payload) plus separate `role: "tool"` messages keyed by `tool_call_id`. `tool_result` blocks are emitted as their own message **before** the remaining content of that turn, matching OpenAI's expected ordering (`internal/translate/anthropic.go:17-19`, `229-258`, tested at `internal/translate/anthropic_test.go:87-140`).

**Q15. What is `EnsureToolParameters` for?**

Some upstreams (Poe, per the code comment) reject a tool definition that has no `parameters`/`input_schema` object with a misleading `"Field required"` error rather than treating it as "no parameters." When `Options.EnsureToolParameters` is set, a tool with an empty schema gets `{"type":"object","properties":{}}` backfilled; it is opt-in, so a caller that doesn't need it gets the field left absent exactly as sent (`internal/translate/anthropic.go:138-141`, `282-293`, tested at `internal/translate/anthropic_test.go:144-171`). It is not currently wired to a named config transformer the way `cleancache`/`streamoptions` are (`internal/router/router.go:92-97` only maps those two) — how it gets enabled per-provider is **PLANNED**.

**Q16. Does the gateway remove `Content-Length` on compressed responses — and why?**

Yes. Once a response body is brotli- or gzip-encoded, its length differs from the original, so any `Content-Length` computed beforehand is now wrong; the middleware explicitly deletes the header on a compressed response, because a stale value would make clients truncate the body or hang waiting for more bytes that never arrive (`internal/gateway/compress.go:103-107`, tested at `internal/gateway/gateway_test.go:124-133`).

**Q17. Why does `Alt-Svc` only appear when HTTP/3 is enabled?**

Because advertising `Alt-Svc: h3=...` promises clients they can upgrade to an HTTP/3 endpoint that, if HTTP/3 isn't actually being served, doesn't exist — a broken promise. The middleware that sets the header is only registered when `Options.EnableHTTP3` is true (`internal/gateway/gateway.go:87-89`), and this is asserted directly by test: no `EnableHTTP3` → no `Alt-Svc` header at all (`internal/gateway/gateway_test.go:176-184`).

**Q18. Why does the upstream proxy client use a response-header timeout instead of an overall request timeout?**

Because Claude Code's requests can legitimately stream for a long time — the model keeps generating, and the body keeps arriving in SSE chunks. `http.Client.Timeout` bounds the *entire* request including reading the body, which would cut a slow-but-healthy stream short once it outlives a fixed budget. The standalone `internal/proxy.New` sets only `Transport.ResponseHeaderTimeout`, which catches a genuinely unresponsive upstream (no headers within the timeout) while never truncating a legitimate stream (`internal/proxy/proxy.go:26-44`). This is explicitly tested: a stream configured to run five times longer than the client's timeout still delivers every chunk, because only the *header* wait is time-bounded (`internal/proxy/proxy_test.go:219-266`).

Note that the live `POST /v1/messages` handler takes a **different** approach today, since it doesn't use `internal/proxy` by default (see Q10a): `handleMessages` wraps the request `context` in a `context.WithTimeout(ctx, UpstreamTimeout)` for **non-streaming** calls only, leaving streaming calls with no added deadline at all (`internal/gateway/messages.go:217-225`). The net effect for streaming is the same (never cut short), but for non-streaming it differs: a context timeout bounds the *whole* call including body read, not just the wait for headers.

**Q19. Can an upstream error response leak my provider API key?**

No, by design and by test. `proxy.Client.Do` never wraps the outgoing `*http.Request` (which carries the `Authorization: Bearer <key>` header) into any returned error text — it only ever names the provider and the URL, which contain no secret. This is exercised against connection-refused, malformed-URL, and unresolvable-host failures with a real secret string, asserting the key text never appears in the error (`internal/proxy/proxy.go:61-64`, `internal/proxy/proxy_test.go:175-217`).

**Q20. What's the difference between `GET /health` and `GET /ready`?**

`/health` is a liveness probe: it always returns `200` once the process is serving HTTP, along with a `providers` count — it says nothing about whether any provider is actually usable. `/ready` is a readiness probe: it only returns `200` when the router could actually resolve a request — i.e. at least one provider is configured **and** `Router.default` is non-empty — returning `503` with a specific reason string otherwise (`internal/gateway/gateway.go:93-115`, tested at `internal/gateway/gateway_test.go:135-161`). Deliberately, both are unauthenticated and dependency-free, so an external supervisor can distinguish "process up" from "upstream reachable" without needing credentials.

**Q21. Why is the default bind address `127.0.0.1` and not `0.0.0.0`?**

Because that is what Claude Code and the existing `claude_toolkit` deployments already expect: a local, loopback-only gateway. `New()` only applies the `127.0.0.1` default when `Options.Host` is left empty — it's a deliberate compatibility choice, not an oversight (`internal/gateway/gateway.go:12-16`, `61-63`). Exposing it beyond loopback is the operator's explicit choice, and `docs/ADMIN_MANUAL.md` recommends putting an authenticating reverse proxy in front if you do.

**Q22. Why is the default port 3456?**

To match the Node implementation and every config already written by existing tooling — changing it would break installed setups for no benefit (`internal/gateway/gateway.go:65-66`, comment at `internal/gateway/gateway_test.go:194-197`).

**Q23. A route string's model id has a comma in it — does that break parsing?**

No. `SplitRoute` splits on the **first** comma only; everything after it — commas included — is the model id (`internal/config/config.go:157-172`). This is explicitly tested with `"prov,vendor/model,v2"` correctly parsing to provider `prov`, model `vendor/model,v2` (`internal/config/config_test.go:110-124`).

**Q24. Does this project contain any code copied from the Node original?**

No. It is explicitly documented as a **clean-room** reimplementation: the wire formats, CLI grammar, config layout, and default ports are reproduced *deliberately* (for compatibility with existing installs), but no upstream source was copied (`NOTICE:1-11`). The upstream MIT licence text is retained verbatim in `LICENSE-UPSTREAM-MIT` purely for attribution.

**Q25. Why can't I `go install` or run a `ccr` binary right now?**

Because `cmd/ccr` is currently an empty directory — there is no `main` package yet, and therefore no CLI. This is now purely a packaging gap, not a functional one: `POST /v1/messages` itself is fully implemented and tested (`internal/gateway/messages.go`, `internal/gateway/messages_test.go`) — request/response translation in both directions, streaming, and error mapping all work. What's missing is only the executable entrypoint that would load `config.json`, call `gateway.New`, and call `Start()`. See the Status table in `README.md` for exactly what's implemented versus PLANNED.

**Q26. What does a `POST /v1/messages` error response look like, and which HTTP status codes does it use?**

Every error — whether it originates in the gateway itself (bad JSON, no route, a translation failure like an image block) or is forwarded from the upstream provider — uses the same Anthropic-style shape: `{"type":"error","error":{"type":"<error_type>","message":"<message>"}}` (`internal/gateway/messages.go:246-256`). Gateway-originated errors use `400` for a malformed/untranslatable request, `503` when no route can be resolved, `502` for an upstream transport failure or a malformed/empty upstream response, and `500` only for the (should-be-rare) case of failing to encode the already-validated outgoing request. A **non-2xx upstream response** is different: its exact status code is preserved and forwarded as-is — `forwardUpstreamError` maps the status to an Anthropic error `type` (e.g. `401`→`authentication_error`, `403`→`permission_error`, `404`→`not_found_error`, `429`→`rate_limit_error`, `400`/`422`→`invalid_request_error`, any `5xx`→`api_error`) rather than collapsing everything to a generic `502`, because Claude Code's retry/backoff behaviour depends on seeing the real status code (`internal/gateway/messages.go:258-318`, tested at `internal/gateway/messages_test.go:224-259`). Full reference: `docs/API.md`.

**Q27. How does a streamed response map OpenAI SSE chunks to Anthropic SSE events?**

`streamAnthropicSSE` reads the upstream's `data: {...}` lines (terminating on `data: [DONE]`) and re-emits the standard Anthropic Messages event sequence: one `message_start`, then per content block a `content_block_start` / one-or-more `content_block_delta` / `content_block_stop` triplet (text uses `text_delta`, tool-call argument fragments use `input_json_delta`), then a closing `message_delta` (carrying the mapped `stop_reason` and final token usage) and `message_stop` (`internal/gateway/messages.go:384-547`). Every individual event is flushed the moment it's written — not batched — which is asserted directly by test via a flush-counting response recorder (`internal/gateway/messages_test.go:15-27`, `180-185`). A malformed individual chunk line from the upstream is skipped rather than aborting the whole stream, since by the time a bad line arrives the response has already started and there is no status code left to change (`internal/gateway/messages.go:494-498`).

**Q28. Is this project's Go code itself under the MIT licence?**

Unconfirmed — there is no top-level `LICENSE` file in this repository, only `LICENSE-UPSTREAM-MIT`, which is the *upstream* Node project's licence text retained for attribution per `NOTICE`. Do not assume the Go code inherits MIT until an explicit licence file is added.
