# Upstream test-suite porting matrix

Ports the behavioural intent of the upstream `musistudio/claude-code-router`
(Node) test suites listed below into Go table-driven tests. Source snapshot:
`/tmp/ccr-upstream` (read-only). Target: this Go module,
`github.com/vasic-digital/claude-code-router`.

Legend:

- **PORTED** — our Go code has the equivalent observable behaviour; a real,
  passing Go test asserts it.
- **GAP** — our Go code is missing the behaviour; a Go test exists and is
  `t.Skip("GAP: ...")`-ed so it is visible without failing the build.
- **N/A** — upstream-only concern (a Node CCR subsystem — Electron-style
  core/gateway process split, billing telemetry, ToolHub/MCP, OAuth
  provider-plugin auth, ClaudeCodeRouterPlugin's built-in rules engine,
  Fusion vendor-specific hosted web search, ...) that this Go router does
  not implement and, per its own package docs (see `internal/*/*.go`
  header comments), does not intend to replicate. No test written.

This repository is a small, deliberate reimplementation — a single static
`config.json`-driven Anthropic→OpenAI translator/proxy (see `internal/`
package docs) — not a line-for-line port of the much larger Node CCR
product (rules DSL, virtual model profiles, ToolHub/MCP runtime, OAuth
provider plugins, billing sync, Codex `apply_patch` bridge, corporate
upstream proxy support, Electron hot-reload). Most N/A rows below reflect
that architectural gap rather than an oversight; where a piece of upstream
behaviour maps onto something this Go router's own docs treat as in-scope,
it is GAP instead, with a skipped Go test spelling out the exact contract.

`internal/gateway/messages.go` (the actual `/v1/messages` HTTP handler) did
not exist when this porting task began; it landed mid-task, built by another
agent concurrently, and remains off-limits to edit here. Its arrival made
several rows below newly testable — those were re-verified against the real
code and updated (from GAP/N-A to PORTED where the behaviour turned out to
already be correct) rather than left describing a stale snapshot. A few GAPs
below still note they depend on further work in that file.

---

## 1. `test/unit/gateway/codex-patch-bridge.test.mjs`

Codex `apply_patch` ↔ `virtual_apply_patch` bridging for the OpenAI
Responses API (`/v1/responses`): rewrites a `custom` "apply_patch" tool and
its prior tool-call/output into a virtual `function` tool round-trip, edits
tool descriptions to discourage shell-based file edits, and rewrites SSE
`response.output_item.done` events back to `custom_tool_call` shape.

| Assertion | Status | Our test / reason |
|---|---|---|
| All 4 test cases (rewrite request tools/input, leave GPT models untouched, discourage shell edits, rewrite response/SSE back to `custom_tool_call`) | N/A | This Go router has no OpenAI Responses API (`/v1/responses`) support and no Codex-specific tool-rewriting layer at all — `internal/translate` only converts Anthropic Messages → OpenAI chat-completions (see `translate/anthropic.go` package doc). |

## 2. `test/unit/gateway/gateway-billing-sync.test.mjs`

`compileCoreGatewayConfig` disabling the full-trace billing webhook/queue
while keeping raw-trace observability and the upstream-header-sanitizer
plugin active, in an Electron-style core+renderer config compiler.

| Assertion | Status | Our test / reason |
|---|---|---|
| Billing webhook/queue disabled while raw-trace sync stays enabled | N/A | No billing telemetry, no core/gateway process split, and no config-compiler step exist anywhere in this repository — `config.Config` is `{Providers, Router}` only. |

## 3. `test/unit/gateway/gateway-claude-code-oauth.test.mjs`

`normalizeClaudeCodeOauthProviderPlugins` templating a static
`anthropic-beta` header into a request-scoped directive; per-provider
merging of the client's `anthropic-beta` tokens with the OAuth plugin's own
token, scoped only to the routed provider.

| Assertion | Status | Our test / reason |
|---|---|---|
| OAuth provider-plugin header normalization | N/A | No provider-plugin/auth-plugin abstraction exists; `config.Provider` is a flat `{name, api_base_url, api_key, models, transformer}` record with a single static Bearer API key, and `proxy.Client.Do` never forwards or merges client headers at all (see item 7 below). |
| Per-provider `anthropic-beta` header merge, scoped to the routed provider only | GAP (generalised) | `internal/config/provider_protocol_type_port_test.go` → `TestProviderProtocolTypeField_GAP`. The underlying reason this can't be ported directly is that `config.Provider` has no protocol/type field to scope such a merge to "Anthropic-native providers only" in the first place. |

## 4. `test/unit/gateway/gateway-runtime-change.test.mjs`

`shouldRestartGatewayForRuntimeConfigChange` diffing an Electron app's live
config (ToolHub, upstream proxy, observability sampling) to decide whether
the gateway process needs a hot restart.

| Assertion | Status | Our test / reason |
|---|---|---|
| All 4 cases (ToolHub / upstream-proxy / raw-trace changes restart; main-process-only sampling change does not) | N/A | No hot-reload/runtime-config-diffing concept exists — this Go binary is started once with a config path (`config.Load`) and has no live-reload or restart-decision logic. |

## 5. `test/unit/gateway/gateway-status.test.mjs`

`gatewayService.start()`/`getStatus()` persisting a preflight validation
failure ("Core gateway host must be 127.0.0.1 or ::1.") for later polling,
in the dual-process core/gateway architecture.

| Assertion | Status | Our test / reason |
|---|---|---|
| Preflight failure persisted and pollable via `getStatus()` | N/A | No core/gateway process split (hence no "coreHost must be loopback" concern) and no async start-then-poll status object — `gateway.Server.Start()` is synchronous and returns its error directly. The closest analogous "fail loudly instead of silently degrading" property this repository does have is already covered by the existing `TestHTTP3WithoutTLSIsAnExplicitError` in `internal/gateway/gateway_test.go`. |

## 6. `test/unit/gateway/http-boundary.test.mjs`

A family of pure HTTP-boundary helpers: client identification, auth-token
extraction (`Authorization`/`x-api-key`), a remote-control-only
query-string auth carve-out, header forwarding/stripping in both
directions, internal core-auth header injection, response-header
filtering, and JSON body helpers (object-only parsing, a parse cache,
ownership transfer).

| Assertion | Status | Our test / reason |
|---|---|---|
| `inferGatewayClient` (client identity from user-agent/API-key/proxy-mode) | N/A | No client-identity/observability concept exists in this gateway. |
| `readAuthToken`/`readHeader` (Bearer / `x-api-key` extraction) | GAP | `internal/gateway/http_boundary_port_test.go` → `TestInboundAuthTokenParsing_GAP`. The gateway has **no inbound authentication at all** — nothing reads an incoming `Authorization`/`x-api-key` header. |
| `readRemoteControlQueryAuthToken` (query-string auth scoped to `/__ccr/remote/*`) | N/A | No remote-control endpoint family exists. |
| `forwardHeaders` (hop-by-hop/local-auth/observability stripping, duplicate-header joining) | N/A | `proxy.Client.Do`'s signature (`ctx, provider, body, stream`) has no caller-header parameter at all — it is a protocol-translating proxy (Anthropic-in, OpenAI-out) that deliberately builds a fresh, minimal header set from `config.Provider` rather than forwarding arbitrary client headers; see item 7's PORTED test, which proves this by construction. |
| `stripLocalGatewayAuthHeaders` / `omitLocalObservabilityHeaders` | N/A | Same reasoning — there is nothing to strip because nothing is ever forwarded. |
| `withCoreGatewayAuthHeader` (`x-ccr-core-auth` injection, throws if uninitialized) | N/A | No core/gateway process split. |
| `filteredResponseHeaders` (allowlist upstream→client response headers) | PORTED | `internal/gateway/http_boundary_port_test.go` → `TestUpstreamResponseHeaderNeverLeaksToClient`. `internal/gateway/messages.go` landed mid-task with a real `/v1/messages` relay; verified against it directly. It achieves a STRICTER version of upstream's allowlist by construction: `respondNonStreaming`/`streamAnthropicSSE` build the client response entirely from the upstream response BODY and never read or copy a single upstream response header at all, so nothing — not even a header a denylist might miss — can leak through. |
| `parseJsonObject`/`parseJsonObjectSafe` (object-only JSON, rejects arrays/`null`) | N/A (see item 12 for the structurally-typed analogue) | No generic "parse and validate top-level shape" helper exists; `internal/translate.AnthropicRequest` is unmarshaled directly into a typed struct. |
| `parseJsonObjectCached`/`takeJsonObject`/`releaseJsonObject` (parse-cache/ownership pattern) | N/A | Node-specific allocation-avoidance pattern; Go's typed unmarshal has no equivalent need. |
| `shouldSendBody(method)` | N/A | `proxy.Client.Do` always POSTs a body; there is no GET/HEAD upstream call to gate. |
| `shouldCaptureGatewayUsage(method, path)` | N/A | No usage-capture/observability concept exists. |
| `abortSignalMessage` (AbortSignal reason → string) | N/A | Go uses `context.Context` cancellation with `context.Canceled`/`context.DeadlineExceeded`, already exercised by the existing `TestDoHonoursContextCancellation` in `internal/proxy/proxy_test.go`. |

## 7. `test/unit/gateway/router-builtins.test.mjs`

The largest upstream file (49 `test()` cases, 2038 lines): `ClaudeCodeRouterPlugin`'s
built-in rules engine (`claude-code`/`codex`/Grok profile routing), router
rules DSL execution, ToolHub MCP resolver-instruction injection, ordinary
subagent-model instruction injection into Agent/Task/Workflow tools, billing
system-block stripping, plus a handful of lower-level upstream-shaping
assertions (`stream_options.include_usage`, provider-prefix stripping,
stale-header cleanup, fallback-retry backoff, unsupported-parameter
stripping) embedded in the same file.

| Assertion (grouped) | Status | Our test / reason |
|---|---|---|
| Built-in `claude-code`/`codex`/Grok profile routing, `ClaudeCodeRouterPlugin`, router rules DSL, ToolHub resolver injection, subagent-model instruction injection (Agent/Task/Workflow tools), billing system-block stripping — ~40 of the 49 cases | N/A | None of `ClaudeCodeRouterPlugin`, a rules DSL, ToolHub/MCP, subagent-model injection, or billing-block handling exist in this repository. `router.Select` is a ~90-line pure function with exactly two configured routes (Default/Background) plus a haiku substring check — there is no plugin/profile/rules layer to route through. |
| "OpenAI chat completion streaming attempts request upstream usage chunks" (`stream_options.include_usage`) | PORTED | Already covered by the existing `internal/translate/anthropic_test.go` → `TestStreamOptionsOnlyWhenStreaming` (`translate.Options.StreamOptions` / the `streamoptions` transformer). |
| "explicit provider selectors without capability routing strip provider prefix upstream" / "model-chain fallback model selectors must not keep stale target provider headers" (client-supplied `Provider/model` selector routing) | GAP | `internal/router/explicit_provider_selector_port_test.go` → `TestSelectIgnoresExplicitProviderModelSelector_GAP`. `router.Select` never reads a `Provider/model` (or legacy `Provider,model`) selector out of the request body — see the file's full writeup. |
| "fallback retry delay backs off retryable HTTP statuses" / "... backs off network errors" | GAP | `internal/router/fallback_retry_classification_port_test.go` → `TestFallbackRetryDelay_GAP`. No retry/backoff logic exists anywhere — `proxy.Client.Do` makes exactly one attempt. |
| "gateway strips unsupported OpenAI upstream request parameters" (`thinking`/`reasoning_split` dropped, `reasoning` kept) | N/A | Multi-protocol capability-harmonization concern specific to Node CCR's OpenAI Responses/reasoning-model support; `translate.AnthropicRequest` has no `thinking`/`reasoning` fields at all (out of scope per its own struct definition), and unknown JSON fields are simply not represented, not selectively stripped. |

## 8. `test/unit/gateway/routing-architecture.test.mjs`

`ModelRegistry` selector canonicalization/ambiguity rejection,
`compileRouterConfig`'s rules DSL (conflict/diagnostic detection),
`RoutePolicyEngine`, `createRouteExecutionPlan`, `classifyRouteFailure`,
`shouldApplyGatewayRouting`/protocol detection, the Gemini path-embedded-model
adapter, and agent request enrichers.

| Assertion | Status | Our test / reason |
|---|---|---|
| "model registry canonicalizes provider models and rejects ambiguous bare models" | GAP | `internal/router/explicit_provider_selector_port_test.go` → `TestModelRegistryAmbiguousBareModelRejection_GAP` (points back to the primary selector-routing GAP; `router.Select` has no bare-model resolution to be ambiguous about). |
| "model registry accepts known internal provider suffixes only" (`::openai_chat_completions::cred:`) | N/A | No credential-suffix/multi-credential-per-provider concept exists; `config.Provider` has a single `api_key` field. |
| "router config compilation ..." (5 cases: invalid rules disabled, final rewrite wins, invalid fallback models filtered, provider/model conflicts rejected, disabled-rule diagnostics ignored) | N/A | No rules DSL (`Router.rules`) exists; `config.Route` is `{Default, Background, Think, LongContext}` strings only. |
| "route policy engine returns the first matching policy" | N/A | No `RoutePolicyEngine` exists, though `router.Select`'s own two-branch order (haiku→Background else Default) is a fixed, first-match-wins policy in spirit — already covered by the existing `TestSelectBackgroundRouteForHaikuModel`/`TestSelectDefaultRouteForOrdinaryModel`. |
| "execution planner includes primary and de-duplicated fallback attempts" | GAP | `internal/router/fallback_retry_classification_port_test.go` → `TestExecutionPlanDedup_GAP`. |
| "failure classifier keeps retry and model-chain policies explicit" | GAP | `internal/router/fallback_retry_classification_port_test.go` → `TestClassifyRouteFailure_GAP`. |
| "gateway routing runs for body-model protocols independent of agent user-agent" (`shouldApplyGatewayRouting`) | GAP, with a PORTED subset | `internal/gateway/protocol_endpoints_port_test.go` → `TestShouldApplyGatewayRouting_GAP` + `TestOnlyPOSTMessagesIsRoutingEligible` (duplicate of `protocol-endpoints.test.mjs`, see item 14 for the split). |
| "Gemini path-model adapter routes and restores generateContent requests" | N/A | No Gemini support of any kind — `internal/translate` converts Anthropic Messages → OpenAI chat-completions only, per its package doc. |
| "body-model protocols do not require route input adaptation" | N/A | This repository's one supported protocol (Anthropic Messages) is always body-model, so the adapter's "no-op" branch is true by construction — there is no adapter layer to test the absence of. |
| "agent enrichers run only for matching agent contexts" | N/A | No multi-agent (`claude-code` vs `codex` vs ...) request-enrichment concept exists; Claude Code is this router's only client. |

## 9. `test/unit/gateway/upstream-header-sanitizer.test.mjs`

`sanitizeUpstreamProviderHeaders` strips CCR-owned headers (`x-ccr-*`,
`x-auth-api-key-id`, `x-auth-sub`) from the outbound provider request while
preserving genuine provider-facing headers; a gateway plugin hook applies
this at the final upstream-request boundary.

| Assertion | Status | Our test / reason |
|---|---|---|
| CCR-owned headers stripped, provider headers (`authorization`, `x-auth-token`, `x-client-request-id`) preserved | PORTED | `internal/proxy/upstream_header_sanitizer_port_test.go` → `TestDoOnlySendsAllowlistedHeaders`. Ported via an equivalent-but-different mechanism: `proxy.Client.Do` never accepts or forwards caller headers at all, so it achieves the same "no internal header ever reaches the provider" guarantee by construction (an allowlist of exactly 3 headers it builds itself) rather than by a denylist filter applied to a forwarded set. |
| Plugin-hook wiring (`createGatewayPlugin().providerHooks`) | N/A | No plugin/hook system exists. |

## 10. `test/integration/gateway/gateway-client-disconnect.test.mjs`

End-to-end: a real HTTP server simulating an upstream Codex SSE stream, a
real gateway `proxyRequest` relay, and a client that cancels its reader and
aborts — asserting the upstream connection actually closes and neither an
`uncaughtException` nor an `unhandledRejection` is raised.

| Assertion | Status | Our test / reason |
|---|---|---|
| Downstream client abort closes the upstream connection cleanly, no process-level exceptions | PORTED | `internal/gateway/client_disconnect_port_test.go` → `TestClientDisconnectClosesUpstreamConnection`. `internal/gateway/messages.go` (the `/v1/messages` relay) did not exist when this task began but landed mid-task; re-verified directly against it with a real `net/http` server pair (not `httptest.ResponseRecorder`, which has no real connection to close). `handleMessages` threads `c.Request.Context()` through to `s.Upstream.Do`, and Go's own `net/http` server cancels that context the instant the client connection goes away — the SAME context governs the outbound upstream request, so `http.Client` propagates the cancellation to the upstream connection automatically. No bespoke `AbortController` wiring (what upstream's Node implementation needs) is required; Go's context propagation gives this for free once the threading itself is correct, and it is. `proxy.Client.Do`'s own context-cancellation behaviour is separately covered by the existing `TestDoHonoursContextCancellation`/`TestStreamingBodyIsNotCutShortByResponseTimeout` in `internal/proxy/proxy_test.go`. |

## 11. `test/integration/gateway/gateway-virtual-models.test.mjs`

40 `test()` cases, 1490 lines, all one feature area: "Fusion" vendor-specific
fixed-base/vision-model routing, MCP/ToolHub core-auth-token injection into
built-in runtimes, and hosted-web-search protocol bridging (request
injection, response/SSE synthesis, and stop-reason preservation) replicated
across all four supported wire protocols (Anthropic Messages, OpenAI
chat-completions, OpenAI Responses, Gemini).

| Assertion (grouped) | Status | Our test / reason |
|---|---|---|
| Fusion fixed-base/vision provider rewriting; MCP/ToolHub core-auth-token injection | N/A | No "Fusion" vendor concept, no MCP/ToolHub runtime, no core-auth-token exists anywhere in this repository. |
| Hosted web-search request injection / response synthesis / SSE rewriting, for each of Anthropic Messages, OpenAI chat, OpenAI Responses, and Gemini (the remaining ~35 cases) | N/A | This repository has no hosted-web-search product feature, and 3 of the 4 protocols involved (OpenAI Responses, Gemini, and any wire protocol other than Anthropic Messages as the OUTBOUND target) do not exist in `internal/translate` at all. |

## 12. `test/unit/proxy/proxy-upstream.test.mjs`

Custom corporate/SOCKS upstream proxy configuration
(`customUpstreamProxyFromConfig`): percent-encoded userinfo in the
constructed proxy URL, a matching HTTP Basic `Proxy-Authorization`-shaped
header, and a no-op fallthrough for `mode:"none"` or an incomplete config.

| Assertion | Status | Our test / reason |
|---|---|---|
| Authenticated custom proxy URL + Basic-Auth header construction | GAP | `internal/proxy/proxy_upstream_port_test.go` → `TestCustomUpstreamProxyURLConstruction_GAP`. `config.Config` has no `proxy` section at all; `proxy.New`/`proxy.Client` build a transport with only `ResponseHeaderTimeout` set — an operator cannot route this gateway's outbound calls through an authenticated corporate proxy. |
| `mode:"none"` / incomplete config → no proxy constructed | GAP | `internal/proxy/proxy_upstream_port_test.go` → `TestCustomUpstreamProxyNoneOrIncomplete_GAP`. |

## 13. `test/unit/providers/provider-url.test.mjs`

`parseProviderBaseUrl`/`normalizeProviderBaseUrl`/`providerBaseUrlForProtocol`/
`providerUrlWithDefaultScheme`: strips endpoint paths, credentials, query,
and fragment from a raw base URL; derives a different effective base per
wire protocol from the same input; defaults a scheme (`http://` for
localhost, `https://` otherwise) for a scheme-less input; preserves
versioned "bypass"/nested-app-path Gemini bases verbatim.

| Assertion | Status | Our test / reason |
|---|---|---|
| All 5 cases (credential/query/fragment stripping + per-protocol base derivation; local/Gemini variants; versioned Vertex-bypass base preservation; nested versioned Gemini base preservation; default-scheme derivation) | N/A (documented via 2 real tests) | `internal/config/provider_url_port_test.go` → `TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation`, `TestValidateRejectsSchemeLessAPIBaseURL`. Per `config.go`'s own field doc and `proxy.go`'s package doc ("`p.APIBaseURL` is used VERBATIM... this function must never [append a suffix]"), this repository deliberately has **no** URL-derivation layer: the operator supplies the complete literal endpoint, and a scheme-less URL is a hard config error rather than something to default a scheme onto. This is an intentional, documented, opposite design choice, not an oversight — the two tests above pin down our actual (different) behaviour for regression coverage. |
| Protocol-specific base derivation specifically (needs a provider `type`/protocol field to derive FOR) | GAP (generalised with item 3) | `internal/config/provider_protocol_type_port_test.go` → `TestProviderProtocolTypeField_GAP`. `config.Provider` has no protocol/type field, so even if URL derivation existed there would be nothing to key it on. |

## 14. `test/unit/routing/protocol-endpoints.test.mjs`

`requestProtocolForPath` (path → wire-protocol classification across all 4
protocols and their `/proxy/v1/*` aliases) and `shouldApplyGatewayRouting`
(POST + path-allowlist gate, with sub-resource paths under an otherwise-
routable prefix explicitly excluded).

| Assertion | Status | Our test / reason |
|---|---|---|
| `requestProtocolForPath` full table (11 recognised shapes + 2 unrecognised) | GAP | `internal/gateway/protocol_endpoints_port_test.go` → `TestRequestProtocolForPath_GAP`. `internal/gateway/messages.go` landed mid-task and now registers a real `POST /v1/messages` route (see `gateway.go`'s `routes()`), so the very first table row (`/messages` family → `anthropic_messages`) has a real counterpart — but there is still no REUSABLE path→protocol classifier function anywhere (routing is a single hardcoded gin route, not a callable classifier), and none of the other 3 protocol families (OpenAI chat-completions, OpenAI Responses, Gemini) or the `/proxy/v1/*` aliases are recognised at any path. Still GAP overall. |
| `shouldApplyGatewayRouting` (POST-only, path allowlist, sub-resource exclusion) | GAP, with a PORTED subset | `internal/gateway/protocol_endpoints_port_test.go` → `TestShouldApplyGatewayRouting_GAP` (full table, still GAP: no callable function, no multi-path allowlist). The POST-vs-other-methods slice of it, restricted to the one real endpoint, is genuinely PORTED: `TestOnlyPOSTMessagesIsRoutingEligible` in the same file proves GET/PUT/PATCH/DELETE on `/v1/messages` are not routing-eligible (gin 404s them; only `POST` is registered). |

---

## Summary

| File | PORTED | GAP | N/A |
|---|---:|---:|---:|
| 1. codex-patch-bridge.test.mjs | 0 | 0 | 4 |
| 2. gateway-billing-sync.test.mjs | 0 | 0 | 1 |
| 3. gateway-claude-code-oauth.test.mjs | 0 | 1 (shared w/ #13) | 1 |
| 4. gateway-runtime-change.test.mjs | 0 | 0 | 4 |
| 5. gateway-status.test.mjs | 0 | 0 | 1 |
| 6. http-boundary.test.mjs | 1 | 1 | 10 |
| 7. router-builtins.test.mjs | 1 | 2 | 2 rows (grouped, ~46 of 49 individual cases) |
| 8. routing-architecture.test.mjs | 0 (+1 partial) | 3 (1 shared) | 6 |
| 9. upstream-header-sanitizer.test.mjs | 1 | 0 | 1 |
| 10. gateway-client-disconnect.test.mjs | 1 | 0 | 0 |
| 11. gateway-virtual-models.test.mjs | 0 | 0 | 2 (grouped, 40 individual cases) |
| 12. proxy-upstream.test.mjs | 0 | 2 | 0 |
| 13. provider-url.test.mjs | 0 | 1 (shared w/ #3) | 1 (documented via 2 tests) |
| 14. protocol-endpoints.test.mjs | 0 (+1 partial) | 2 (shared w/ #8) | 0 |

Distinct Go tests written: **6 new passing tests** —
`TestDoOnlySendsAllowlistedHeaders` (real behaviour-equivalence PASS,
PORTED); `TestUpstreamResponseHeaderNeverLeaksToClient` and
`TestClientDisconnectClosesUpstreamConnection` (PORTED, verified directly
against `internal/gateway/messages.go`, which landed mid-task — see the
note at the top of this document); `TestOnlyPOSTMessagesIsRoutingEligible`
(PORTED, the real subset of upstream's `shouldApplyGatewayRouting` that
this repository's single endpoint can exercise); and two N/A-documenting
regression tests pinning down our actual, intentionally different
behaviour, `TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation` and
`TestValidateRejectsSchemeLessAPIBaseURL` — plus **1 cross-reference to
pre-existing PORTED coverage** (`TestStreamOptionsOnlyWhenStreaming`,
already in `internal/translate/anthropic_test.go` before this change), and
**12 distinct GAP tests** (`t.Skip`-ed, several rows above point at the
same GAP test where upstream duplicates a concern across files):

- `internal/gateway/protocol_endpoints_port_test.go` — 2 GAP tests + 1 PORTED test (`TestOnlyPOSTMessagesIsRoutingEligible`)
- `internal/gateway/http_boundary_port_test.go` — 1 GAP test + 1 PORTED test (`TestUpstreamResponseHeaderNeverLeaksToClient`)
- `internal/gateway/client_disconnect_port_test.go` — 1 PORTED test (`TestClientDisconnectClosesUpstreamConnection`)
- `internal/config/provider_url_port_test.go` — 2 tests (N/A, real assertions)
- `internal/config/provider_protocol_type_port_test.go` — 1 GAP test
- `internal/router/explicit_provider_selector_port_test.go` — 2 GAP tests
- `internal/router/fallback_retry_classification_port_test.go` — 3 GAP tests
- `internal/proxy/proxy_upstream_port_test.go` — 2 GAP tests
- `internal/proxy/upstream_header_sanitizer_port_test.go` — 1 PORTED test

## Prioritised GAP summary

1. **No per-request routing signal at all beyond "haiku" substring
   matching.** `router.Select` cannot honour an explicit `Provider/model`
   selector, cannot detect an ambiguous bare model across providers, and
   has no fallback/retry chain when the chosen provider fails. This is the
   single biggest behavioural gap relative to upstream's `ModelRegistry` +
   `RoutePolicyEngine` + `createRouteExecutionPlan` + `classifyRouteFailure`
   pipeline. (`internal/router/explicit_provider_selector_port_test.go`,
   `internal/router/fallback_retry_classification_port_test.go`)
2. **No path/method-based protocol routing exists yet** —
   `requestProtocolForPath`/`shouldApplyGatewayRouting` have no equivalent
   anywhere, because the `/v1/messages` handler itself doesn't exist in
   this snapshot. Whoever wires up `internal/gateway/messages.go` needs
   this contract. (`internal/gateway/protocol_endpoints_port_test.go`)
3. **No inbound authentication.** Nothing reads an incoming
   `Authorization`/`x-api-key` header; the gateway accepts any caller that
   can reach it. (`internal/gateway/http_boundary_port_test.go` →
   `TestInboundAuthTokenParsing_GAP`)
4. **No response-header filtering on the relay path** (once one exists) —
   risk of a stale upstream `Content-Encoding`/`Connection` header
   conflicting with `compress.go`'s own negotiated encoding.
   (`internal/gateway/http_boundary_port_test.go` →
   `TestUpstreamResponseHeaderFiltering_GAP`)
5. **No outbound/upstream proxy support** — an operator behind an
   authenticated corporate proxy cannot route this gateway's provider calls
   through it. (`internal/proxy/proxy_upstream_port_test.go`)
6. **No provider protocol/type field** — every configured provider is
   unconditionally treated as OpenAI-chat-completions-shaped; a provider
   whose `api_base_url` is a real Anthropic-native (or Gemini, or OpenAI
   Responses) endpoint cannot be proxied to correctly.
   (`internal/config/provider_protocol_type_port_test.go`)
