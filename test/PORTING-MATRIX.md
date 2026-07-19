# Upstream test-suite porting matrix

Ports the behavioural intent of the upstream `musistudio/claude-code-router`
(Node) test suites listed below into Go table-driven tests. Source snapshot:
`/tmp/ccr-upstream` (read-only). Target: this Go module,
`github.com/vasic-digital/claude-code-router`.

Legend:

- **PORTED** — our Go code has the equivalent observable behaviour; a real,
  passing Go test asserts it.
- **GAP** — our Go code is missing the behaviour. Historically such a row
  cited a Go test that was `t.Skip("GAP: ...")`-ed so the missing contract
  was visible without failing the build. **As of this revision there are no
  GAP-skipped tests left**: every one was either converted into a real,
  passing test (the behaviour turned out to be in-scope and was implemented)
  or reclassified N/A once the blocker its skip cited was removed. Verified:
  `grep -rn 't.Skip("GAP' internal/ cmd/ test/ --include='*.go'` returns
  **zero matches**. (The two conditional `t.Skipf`/`t.Skip` calls inside
  `TestLiveConfigStillOpenAI` — "no live config" / "no providers" — are
  environment guards, not GAP skips; and `test/challenges/…_defect_test.go`'s
  `t.Skip("DEFECT: …")` is a challenge-suite defect marker, not a port GAP.
  None of these are counted below.)
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
behaviour mapped onto something this Go router's own docs treat as in-scope,
it has since been implemented and is PORTED with a real, passing Go test
(earlier revisions marked several of these GAP, with a skipped Go test
spelling out the contract; those skips are all gone now).

**Methodology for this revision.** The verifying grep
`grep -rn 't.Skip("GAP' internal/ cmd/ test/ --include='*.go'` returns **zero
matches** — every GAP skip in the tree is closed. For each row below the cited
test file was reopened, its current function names confirmed, and its
verdict / citation / reason rewritten to match the code that exists today; no
row cites a `*_GAP` test, because none exists.

`internal/gateway/messages.go` (the actual `/v1/messages` HTTP handler) did
not exist when this porting task began; it landed mid-task, built by another
agent concurrently, and remains off-limits to edit here. Its arrival — plus
subsequent work adding a per-provider `protocol` field with anthropic-native
passthrough (`internal/config/config.go`, `internal/translate.AnthropicPassthrough`,
`messages.go`), an OpenAI-compatible inbound facade
(`internal/gateway/openai_inbound.go`), reusable path→protocol classifiers
(`internal/gateway/protocol.go`), an explicit-selector / bare-model router path
(`internal/router/selector.go`), a retry/backoff loop (`messages.go`'s
`doUpstreamWithRetry` driving `internal/router/fallback.go`), and custom/env
upstream-proxy support (`internal/proxy/upstream_proxy.go`) — closed every row
this matrix previously marked GAP. Each such row is now PORTED, or N/A where
only the *blocker its old skip cited* went away while the feature itself stays
out of scope by design. All four affected packages (`internal/config`,
`internal/gateway`, `internal/router`, `internal/proxy`) build and pass.

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
| Per-provider `anthropic-beta` header merge, scoped to the routed provider only | N/A (former blocker removed) | `internal/config/provider_protocol_type_port_test.go` → `TestResolvedProtocol` / `TestValidateProtocol`. The reason earlier revisions gave — "`config.Provider` has no protocol/type field to scope such a merge to Anthropic-native providers" — is now **false**: `config.Provider` carries an optional `protocol` field (`"openai"`/`"anthropic"`) with conservative `api_base_url` inference (`config.go`'s `ResolvedProtocol`) and validation, and the gateway does route an anthropic-native provider **untranslated** on both legs (`messages.go` via `translate.AnthropicPassthrough`). The specific *anthropic-beta header templating/merge* stays N/A for a different, still-true reason: there is no provider-plugin / OAuth-plugin / auth abstraction to own such a directive, and `proxy.Client.Do` never accepts or merges client headers at all (items 7 and 9) — it builds a fresh three-header set itself, so there is no client `anthropic-beta` token to merge in the first place. |

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
| `readAuthToken`/`readHeader` (Bearer / `x-api-key` extraction) | PORTED | `internal/gateway/http_boundary_port_test.go` → `TestInboundAuthTokenParsing`. `RequireAPIKey` (`internal/gateway/auth.go`) reads both `Authorization: Bearer <token>` and `x-api-key` (trimmed of surrounding whitespace) and is mounted on `POST /v1/messages` (`gateway.go`): a token matching a configured key is accepted (200), a wrong/missing/empty one rejected (401), via either header spelling, and a 401 never echoes the configured key. (When no API keys are configured the gateway stays open by design — see `auth.go`'s package doc — which is why this is authentication, not a hard requirement.) |
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
| "explicit provider selectors without capability routing strip provider prefix upstream" / "model-chain fallback model selectors must not keep stale target provider headers" (client-supplied `Provider/model` selector routing) | PORTED | `internal/router/explicit_provider_selector_port_test.go` → `TestSelectHonoursExplicitProviderModelSelector` (+ `TestSelectExplicitSelectorOverridesHaikuTier`, `TestSelectExplicitSelectorErrors`). `router.Select` now reads an explicit `Provider/model` (or legacy `Provider,model`) selector out of `req.Model` (`selector.go`'s `resolveExplicitSelector`) and routes to exactly that provider/model, taking precedence over `Router.Default`/`Background` and the haiku tier; an unknown provider, or a model the named provider does not list, is a loud named error rather than a silent fall back to Default. |
| "fallback retry delay backs off retryable HTTP statuses" / "... backs off network errors" | PORTED | `internal/router/fallback_retry_classification_port_test.go` → `TestFallbackRetryDelay`. `FallbackRetryDelayAfterStatus`/`FallbackRetryDelayAfterNetworkError` (`internal/router/fallback.go`) port the exact 1000ms-base, doubling-per-attempt schedule with a `Retry-After`-header override floored at the base. The former reason — "No retry/backoff logic exists anywhere; `proxy.Client.Do` makes exactly one attempt" — is now **false**: `proxy.Client.Do` still makes one attempt, but `internal/gateway/messages.go`'s `doUpstreamWithRetry` wraps it in a real retry loop that retries the routed provider (up to `Options.MaxAttempts`) on a `router.Retryable` status/transport error and backs off via exactly these functions. |
| "gateway strips unsupported OpenAI upstream request parameters" (`thinking`/`reasoning_split` dropped, `reasoning` kept) | N/A | Multi-protocol capability-harmonization concern specific to Node CCR's OpenAI Responses/reasoning-model support; `translate.AnthropicRequest` has no `thinking`/`reasoning` fields at all (out of scope per its own struct definition), and unknown JSON fields are simply not represented, not selectively stripped. |

## 8. `test/unit/gateway/routing-architecture.test.mjs`

`ModelRegistry` selector canonicalization/ambiguity rejection,
`compileRouterConfig`'s rules DSL (conflict/diagnostic detection),
`RoutePolicyEngine`, `createRouteExecutionPlan`, `classifyRouteFailure`,
`shouldApplyGatewayRouting`/protocol detection, the Gemini path-embedded-model
adapter, and agent request enrichers.

| Assertion | Status | Our test / reason |
|---|---|---|
| "model registry canonicalizes provider models and rejects ambiguous bare models" | PORTED (safely) | `internal/router/explicit_provider_selector_port_test.go` → `TestSelectResolvesUnambiguousBareModelWhenNoDefault`, `TestSelectRejectsAmbiguousBareModelWhenNoDefault`, `TestSelectDefaultWinsOverBareModelResolution`, `TestSelectExplicitSelectorDisambiguatesAndWins`. `router.Select` now resolves a bare (non-prefixed) model to its single owning provider and **refuses an ambiguous bare model with a loud named error** (`selector.go`'s `resolveBareModel`) — upstream's core safety property. It is ported as a *subordinate* path: bare-model lookup runs only in the no-route window (neither `Router.Default` nor haiku→`Background` applies), so a configured `Router.Default` always wins and ordinary Claude Code default routing is unchanged — deliberately avoiding upstream's supreme-registry behaviour that would silently bypass a set Default. |
| "model registry accepts known internal provider suffixes only" (`::openai_chat_completions::cred:`) | N/A | No credential-suffix/multi-credential-per-provider concept exists; `config.Provider` has a single `api_key` field. |
| "router config compilation ..." (5 cases: invalid rules disabled, final rewrite wins, invalid fallback models filtered, provider/model conflicts rejected, disabled-rule diagnostics ignored) | N/A | No rules DSL (`Router.rules`) exists; `config.Route` is `{Default, Background, Think, LongContext}` strings only. |
| "route policy engine returns the first matching policy" | N/A | No `RoutePolicyEngine` exists, though `router.Select`'s own two-branch order (haiku→Background else Default) is a fixed, first-match-wins policy in spirit — already covered by the existing `TestSelectBackgroundRouteForHaikuModel`/`TestSelectDefaultRouteForOrdinaryModel`. |
| "execution planner includes primary and de-duplicated fallback attempts" | PORTED (pure fn; not yet wired into the live loop) | `internal/router/fallback_retry_classification_port_test.go` → `TestBuildExecutionPlanDedup` (+ `TestBuildExecutionPlanNoFallbacks`, `TestNextFallbackProvider*`). `BuildExecutionPlan` (`fallback.go`) ports `createRouteExecutionPlan`'s de-duplicated primary-then-fallbacks ordering, and `NextFallbackProvider` maps a classification to the next candidate (never advancing on a Terminal failure). Scope note: these are exercised as pure functions; the live `doUpstreamWithRetry` loop currently retries the *same* routed provider rather than walking this multi-provider plan, so the plan / next-candidate machinery is ported and tested but not yet wired into the gateway's request path. |
| "failure classifier keeps retry and model-chain policies explicit" | PORTED | `internal/router/fallback_retry_classification_port_test.go` → `TestClassifyRouteFailure` (+ `TestClassifyStatus`, `TestClassifyTransportError`). `ClassifyRouteFailure` (`fallback.go`) ports `classifyRouteFailure`'s mode-aware table verbatim: `"retry"` mode falls back only on 429/5xx while `"model-chain"` mode advances on every failure, with the correct `client`/`server` `failureClass` either way. The status/transport `Retryable`/`Terminal` split (`ClassifyStatus`/`ClassifyTransportError`) is the slice the live `doUpstreamWithRetry` loop consumes. |
| "gateway routing runs for body-model protocols independent of agent user-agent" (`shouldApplyGatewayRouting`) | PORTED | `internal/gateway/protocol_endpoints_port_test.go` → `TestShouldApplyGatewayRouting` (+ `TestOnlyPOSTMessagesIsRoutingEligible`). `shouldApplyGatewayRouting` is a real, callable classifier (`internal/gateway/protocol.go`): POST + a path allowlist across all five protocol families, with interaction sub-resources (`.../interaction-123`, `.../cancel`) correctly excluded. Duplicate of `protocol-endpoints.test.mjs`; see item 14. |
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
| Authenticated custom proxy URL + Basic-Auth header construction | PORTED | `internal/proxy/proxy_upstream_port_test.go` → `TestCustomUpstreamProxyURLConstruction` (+ `TestNewWithUpstreamProxyActuallyRoutesThroughTheConfiguredProxy`, `TestCustomUpstreamProxyCredentialsNeverAppearInErrors`, and the port/host-precedence cases). `internal/proxy/upstream_proxy.go` adds `NewWithUpstreamProxy` + `upstreamProxyURL` + `upstreamProxyAuthorizationHeader`: a `custom` proxy config builds a proxy URL with percent-encoded userinfo and a matching HTTP Basic `Proxy-Authorization` header from the RAW (decoded) credentials, and a real `Do()` call actually travels through the configured proxy carrying that header. Proxy credentials never leak into error text. |
| `mode:"none"` / incomplete config → no proxy constructed | PORTED | `internal/proxy/proxy_upstream_port_test.go` → `TestCustomUpstreamProxyNoneOrIncompleteFallsThrough`. `mode:"none"`, a zero value, or any incomplete `custom` config (empty server/username/password) yields no proxy — a clean fall-through to a direct connection, never a half-applied broken proxy. (`baseTransport` additionally honours `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` for every client: `TestEnvironmentHTTPProxyIsHonoured`, `TestEnvironmentNoProxyBypassesConfiguredProxy`.) |

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
| Protocol-specific base derivation specifically (needs a provider `type`/protocol field to derive FOR) | N/A (former blocker removed) | `internal/config/provider_protocol_type_port_test.go` → `TestResolvedProtocol` / `TestValidateProtocol`. The earlier reason — "`config.Provider` has no protocol/type field, so there would be nothing to key derivation on" — is now **false**: the `protocol` field exists and is tested (item 3). Per-protocol *URL derivation* nonetheless stays N/A by the same intentional design as the row above — `api_base_url` is used verbatim (`TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation`), so there is no base to re-derive per protocol regardless. The protocol field instead drives request/response *translation* (anthropic-native passthrough), not URL rewriting. |

## 14. `test/unit/routing/protocol-endpoints.test.mjs`

`requestProtocolForPath` (path → wire-protocol classification across all 4
protocols and their `/proxy/v1/*` aliases) and `shouldApplyGatewayRouting`
(POST + path-allowlist gate, with sub-resource paths under an otherwise-
routable prefix explicitly excluded).

| Assertion | Status | Our test / reason |
|---|---|---|
| `requestProtocolForPath` full table (11 recognised shapes + 2 unrecognised) | PORTED (classifier); 3 of 5 families recognised-not-served | `internal/gateway/protocol_endpoints_port_test.go` → `TestRequestProtocolForPath` (+ `TestProxyAliasReachesMessagesHandler`). `requestProtocolForPath` (`internal/gateway/protocol.go`) is now a real, reusable classifier and is load-bearing — `handleInbound` (`openai_inbound.go`) dispatches on it, and `routes()` registers the POST paths. It recognises all five families and the `/proxy/v1/*` aliases, and unrecognised shapes resolve to `""` (no guess). Honest served-subset scope: two families have live handlers — `anthropic_messages` (`/v1/messages`) and `openai_chat_completions` (`/v1/chat/completions`, `openai_inbound.go`); `openai_responses`, `gemini_generate_content`, and `gemini_interactions` are classified but not yet served — recognised-not-served, which is documented scope, not a GAP skip. |
| `shouldApplyGatewayRouting` (POST-only, path allowlist, sub-resource exclusion) | PORTED | `internal/gateway/protocol_endpoints_port_test.go` → `TestShouldApplyGatewayRouting` (+ `TestOnlyPOSTMessagesIsRoutingEligible`). `shouldApplyGatewayRouting` is a real callable function running the full POST-only + path-allowlist + interaction-sub-resource-exclusion table. `TestOnlyPOSTMessagesIsRoutingEligible` additionally pins the live wiring: GET/PUT/PATCH/DELETE on `/v1/messages` are 404 (gin), only `POST` is registered. |

---

## Summary

Row counts are per matrix row as displayed (grouped rows, not individual
upstream cases). The **GAP column is zero for every file** — no
`t.Skip("GAP…")` remains anywhere.

| File | PORTED | GAP | N/A |
|---|---:|---:|---:|
| 1. codex-patch-bridge.test.mjs | 0 | 0 | 1 row (4 cases) |
| 2. gateway-billing-sync.test.mjs | 0 | 0 | 1 |
| 3. gateway-claude-code-oauth.test.mjs | 0 | 0 | 2 |
| 4. gateway-runtime-change.test.mjs | 0 | 0 | 1 row (4 cases) |
| 5. gateway-status.test.mjs | 0 | 0 | 1 |
| 6. http-boundary.test.mjs | 2 | 0 | 10 |
| 7. router-builtins.test.mjs | 3 | 0 | 2 rows (grouped, ~42 of 49 individual cases) |
| 8. routing-architecture.test.mjs | 4 | 0 | 6 |
| 9. upstream-header-sanitizer.test.mjs | 1 | 0 | 1 |
| 10. gateway-client-disconnect.test.mjs | 1 | 0 | 0 |
| 11. gateway-virtual-models.test.mjs | 0 | 0 | 2 (grouped, 40 individual cases) |
| 12. proxy-upstream.test.mjs | 2 | 0 | 0 |
| 13. provider-url.test.mjs | 0 | 0 | 2 (documented via real tests) |
| 14. protocol-endpoints.test.mjs | 2 | 0 | 0 |

Totals (row cells): **15 PORTED · 0 GAP · N/A across the rest**. Every file
that once carried a GAP row (3, 6, 7, 8, 12, 13, 14) now carries none.

Distinct Go tests in the nine port files: **42 real, passing test
functions, and zero GAP skips** (verified by re-running
`go test ./internal/config/ ./internal/gateway/ ./internal/router/
./internal/proxy/` — all `ok`). The count per file:

- `internal/gateway/protocol_endpoints_port_test.go` — **4** (`TestRequestProtocolForPath`, `TestShouldApplyGatewayRouting`, `TestOnlyPOSTMessagesIsRoutingEligible`, `TestProxyAliasReachesMessagesHandler`) — all PORTED
- `internal/gateway/http_boundary_port_test.go` — **2** (`TestInboundAuthTokenParsing`, `TestUpstreamResponseHeaderNeverLeaksToClient`) — both PORTED
- `internal/gateway/client_disconnect_port_test.go` — **1** (`TestClientDisconnectClosesUpstreamConnection`) — PORTED
- `internal/config/provider_url_port_test.go` — **2** (`TestAPIBaseURLUsedVerbatimNoPerProtocolDerivation`, `TestValidateRejectsSchemeLessAPIBaseURL`) — N/A-documenting, real assertions
- `internal/config/provider_protocol_type_port_test.go` — **3** (`TestResolvedProtocol`, `TestValidateProtocol`, `TestLiveConfigStillOpenAI`) — all real; `TestLiveConfigStillOpenAI` holds two *conditional environment guards* ("no live config" / "no providers"), which are NOT GAP skips
- `internal/router/explicit_provider_selector_port_test.go` — **8** (explicit-selector + bare-model resolution) — all PORTED
- `internal/router/fallback_retry_classification_port_test.go` — **12** (classify / execution-plan / next-fallback / backoff) — all PORTED
- `internal/proxy/proxy_upstream_port_test.go` — **9** (custom + env upstream proxy) — all PORTED
- `internal/proxy/upstream_header_sanitizer_port_test.go` — **1** (`TestDoOnlySendsAllowlistedHeaders`) — PORTED

Plus **1 cross-reference to pre-existing PORTED coverage**
(`TestStreamOptionsOnlyWhenStreaming`, already in
`internal/translate/anthropic_test.go`) for the `stream_options.include_usage`
row of file 7.

**GAP tests remaining: 0.** Every `*_GAP` skip cited by earlier revisions was
either converted to one of the real tests above or its row reclassified N/A;
the only `t.Skip` calls left in the tree are environment guards and the
challenge-suite `DEFECT` marker, neither of which is a port GAP.

## Remaining scope (no GAP skips left)

Every prioritised GAP earlier revisions listed here has been closed: explicit
`Provider/model` selectors and ambiguous-bare-model rejection are ported
(`selector.go`); `requestProtocolForPath`/`shouldApplyGatewayRouting` are real
callable classifiers (`protocol.go`); inbound `Authorization`/`x-api-key`
authentication exists (`auth.go`); the relay never leaks an upstream response
header by construction (`messages.go`); custom + env upstream-proxy support
exists (`upstream_proxy.go`); and `config.Provider` carries a `protocol` field
with anthropic-native passthrough (`config.go`, `translate.AnthropicPassthrough`).
What remains is honest scope, not a hidden GAP:

1. **The multi-provider fallback CHAIN is ported and tested but not yet wired
   into the live request path.** `BuildExecutionPlan` / `NextFallbackProvider`
   / `ClassifyRouteFailure` (`fallback.go`) are real, passing pure functions,
   but the gateway's `doUpstreamWithRetry` loop currently retries the *same*
   routed provider (driven by `ClassifyStatus`/`ClassifyTransportError` +
   `FallbackRetryDelayAfter*`) rather than walking a plan across providers.
   Wiring the plan into the loop is the one open follow-up.
   (`internal/router/fallback_retry_classification_port_test.go`)
2. **Three of five wire-protocol families are classified but not served.**
   `requestProtocolForPath` recognises `openai_responses`,
   `gemini_generate_content`, and `gemini_interactions`, but only
   `anthropic_messages` (`/v1/messages`) and `openai_chat_completions`
   (`/v1/chat/completions`) have live handlers. Recognised-not-served is
   documented scope. (`internal/gateway/protocol_endpoints_port_test.go`)
3. **Intentional, documented N/A design choices remain N/A** — `api_base_url`
   is used verbatim (no per-protocol URL-derivation layer), `proxy.Client.Do`
   never forwards or merges caller headers (a fresh three-header set instead of
   a sanitized forward), and the large upstream subsystems this repository
   never set out to replicate (ClaudeCodeRouterPlugin rules DSL, ToolHub/MCP,
   billing sync, "Fusion" hosted web search, the Codex `apply_patch` bridge,
   OAuth provider plugins, the Electron core/gateway process split) stay N/A by
   design, per the package docs cited in each row above.
