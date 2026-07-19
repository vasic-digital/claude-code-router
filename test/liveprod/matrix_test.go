package liveprod

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

// TestLiveProductionMatrix sweeps the (endpoint x config-permutation) matrix
// against real `ccr serve` subprocesses. Every permutation is a top-level
// subtest that stands up its OWN fresh fake upstream and its OWN serve instance,
// so a failure is localized and no permutation's state leaks into another.
//
// It is deliberately BROAD where test/live is deep: test/live proves one
// scenario per behaviour; this proves the endpoint surface and the config knobs
// hold together across the product of endpoints and configurations, asserting
// the moved RED + gen_ai counters for each cell.
func TestLiveProductionMatrix(t *testing.T) {
	requireBinary(t)

	t.Run("endpoint_surface", testEndpointSurface)
	t.Run("config_plain", testConfigPlain)
	t.Run("cache_memory_hit_skips_upstream", func(t *testing.T) {
		testCacheBackendHit(t, `"backend":"memory"`)
	})
	t.Run("cache_sqlite_hit_skips_upstream", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "ccr-cache.db")
		testCacheBackendHit(t, fmt.Sprintf(`"backend":"sqlite","path":%q`, dbPath))
	})
	t.Run("cache_semantic_near_duplicate", testCacheSemantic)
	t.Run("cross_provider_fallback", testCrossProviderFallback)
	t.Run("transformer_cleancache_streamoptions", testTransformer)
	t.Run("router_think_routing", testRouterThink)
	t.Run("multi_provider_routing", testMultiProviderRouting)
}

// ---------- Endpoint surface: every path reaches (or is refused by) the right handler ----------

// testEndpointSurface drives one plain instance across the full endpoint
// surface: the two inbound completion endpoints (direct + /proxy alias), the
// gateway's /health and /ready, the management /metrics, and the two negative
// probes (GET /v1/messages must not be routable; POST /v1/responses is
// classified-but-not-served → 404). It then scrapes /metrics and asserts the RED
// path templates for every driven endpoint appeared, and that the negatives
// collapsed to the "/(unmatched)" bucket rather than leaking a raw path label.
func testEndpointSurface(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("main", openAIEcho("chatcmpl-main", "Hello from the upstream."))

	cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"main,main-model"}}`,
		providerJSON("main", fake.url("main"), "sk-main", "main-model", ""))
	si := startServe(t, cfg)

	// --- POST /v1/messages (non-streaming): reaches the Anthropic translator. ---
	m := post(t, si.gwURL("/v1/messages"), anthropicBody("live-model", "hi", ""), nil)
	mustEqualInt(t, "POST /v1/messages status", m.status, 200)
	mustContain(t, "messages content-type", m.contentType, "application/json")
	mustContain(t, "messages translated type", m.body, `"type":"message"`)
	mustContain(t, "messages translated text", m.body, "Hello from the upstream.")
	mustNotContain(t, "messages not raw openai", m.body, `"object":"chat.completion"`)

	// --- POST /v1/messages (streaming): reaches the SSE translator. ---
	ms := post(t, si.gwURL("/v1/messages"), anthropicBody("live-model", "stream", `,"stream":true`), nil)
	mustEqualInt(t, "POST /v1/messages (stream) status", ms.status, 200)
	mustContain(t, "messages stream content-type", ms.contentType, "text/event-stream")
	events, text := parseAnthropicSSE(ms.body)
	if !hasEvent(events, "message_start") || !hasEvent(events, "message_stop") {
		t.Fatalf("streaming: missing framing events; events=%v", events)
	}
	if text != "Hello, world!" {
		t.Fatalf("streaming concatenated text = %q, want %q", text, "Hello, world!")
	}

	// --- POST /v1/chat/completions: reaches the OpenAI facade (raw relay). ---
	oc := post(t, si.gwURL("/v1/chat/completions"), openAIBody("live-model", "hi", ""), nil)
	mustEqualInt(t, "POST /v1/chat/completions status", oc.status, 200)
	mustContain(t, "chat/completions raw openai object", oc.body, `"object":"chat.completion"`)
	mustNotContain(t, "chat/completions not anthropic-shaped", oc.body, `"type":"message"`)

	// --- Alias reachability: /proxy/v1/... classify identically. ---
	pm := post(t, si.gwURL("/proxy/v1/messages"), anthropicBody("live-model", "hi", ""), nil)
	mustEqualInt(t, "POST /proxy/v1/messages status", pm.status, 200)
	mustContain(t, "proxy messages translated", pm.body, `"type":"message"`)
	pc := post(t, si.gwURL("/proxy/v1/chat/completions"), openAIBody("live-model", "hi", ""), nil)
	mustEqualInt(t, "POST /proxy/v1/chat/completions status", pc.status, 200)
	mustContain(t, "proxy chat/completions raw openai", pc.body, `"object":"chat.completion"`)

	// --- Liveness / readiness on the gateway. ---
	h := get(t, si.gwURL("/health"))
	mustEqualInt(t, "GET /health status", h.status, 200)
	mustContain(t, "GET /health body", h.body, `"status":"ok"`)
	mustContain(t, "GET /health providers", h.body, `"providers":1`)
	rd := get(t, si.gwURL("/ready"))
	mustEqualInt(t, "GET /ready status", rd.status, 200)
	mustContain(t, "GET /ready body", rd.body, `"status":"ready"`)

	// --- /metrics on the MANAGEMENT server. ---
	mm := get(t, si.mgmtURL("/metrics"))
	mustEqualInt(t, "GET /metrics status", mm.status, 200)
	mustContain(t, "GET /metrics exposes RED", mm.body, "ccr_http_requests_total")

	// --- Negative: GET /v1/messages is NOT routable (POST-only route). ---
	gm := reqMethod(t, http.MethodGet, si.gwURL("/v1/messages"))
	if gm.status != 404 && gm.status != 405 {
		t.Fatalf("GET /v1/messages: got %d, want 404 or 405 (not routable)", gm.status)
	}

	// --- Negative: POST /v1/responses is classified-but-not-served → 404. ---
	rr := post(t, si.gwURL("/v1/responses"), openAIBody("live-model", "hi", ""), nil)
	mustEqualInt(t, "POST /v1/responses status (classified-not-served)", rr.status, 404)

	// --- Metrics: every driven endpoint's route TEMPLATE appears; negatives
	//     collapse to the single "/(unmatched)" bucket (no raw-path leak). ---
	text2 := scrapeMetrics(t, si)
	for _, p := range []string{"/v1/messages", "/v1/chat/completions", "/proxy/v1/messages", "/proxy/v1/chat/completions"} {
		if !metricPresent(text2, "ccr_http_requests_total", map[string]string{"method": "POST", "path": p, "status": "200"}) {
			t.Errorf("ccr_http_requests_total missing {POST,%s,200}:\n%s", p, text2)
		}
	}
	if !metricPresent(text2, "ccr_http_requests_total", map[string]string{"path": "/(unmatched)"}) {
		t.Errorf("negative probes did not collapse to /(unmatched):\n%s", text2)
	}
	if metricPresent(text2, "ccr_http_requests_total", map[string]string{"path": "/v1/responses"}) {
		t.Errorf("raw path /v1/responses leaked as a metric label (should be /(unmatched))")
	}
	// The gateway attributes upstream + tokens for the /v1/messages path.
	if !metricPresent(text2, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "main", "model": "main-model"}) {
		t.Errorf("ccr_gen_ai_upstream_requests_total missing {main,main-model}:\n%s", text2)
	}
}

// ---------- Config (a): plain — no cache, no fallback ----------

// testConfigPlain drives a controlled request set (1 non-stream + 1 stream
// /v1/messages, 1 /v1/chat/completions) against a single provider and asserts
// the EXACT RED + gen_ai deltas.
//
// METRIC PARITY (fixed): the OpenAI inbound facade (handleOpenAIChatCompletions)
// now records RecordUpstream AND RecordTokens — on BOTH its non-streaming path
// (parsed usage block) and its streaming path (usage tee in relayRawStream) —
// exactly like the Anthropic path, closing a prior gap where /v1/chat/completions
// calls reached the upstream but contributed nothing to the gen_ai counters. So
// all FOUR upstream calls are attributed: upstream delta == 4, with token
// counters covering /v1/messages non-stream (11/7) + stream (13/5) and the facade
// non-stream (11/7) + stream (13/5) = input 48, output 24. This subtest pins that
// parity — including the streaming token-tee — live.
func testConfigPlain(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("main", openAIEcho("chatcmpl-main", "plain answer"))

	cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"main,main-model"}}`,
		providerJSON("main", fake.url("main"), "sk-main", "main-model", ""))
	si := startServe(t, cfg)

	before := scrapeMetrics(t, si)

	r1 := post(t, si.gwURL("/v1/messages"), anthropicBody("m", "hi", ""), nil)
	mustEqualInt(t, "plain messages non-stream status", r1.status, 200)
	mustContain(t, "plain messages content", r1.body, "plain answer")

	r2 := post(t, si.gwURL("/v1/messages"), anthropicBody("m", "stream", `,"stream":true`), nil)
	mustEqualInt(t, "plain messages stream status", r2.status, 200)
	_, txt := parseAnthropicSSE(r2.body)
	if txt != "Hello, world!" {
		t.Fatalf("plain stream text = %q, want %q", txt, "Hello, world!")
	}

	r3 := post(t, si.gwURL("/v1/chat/completions"), openAIBody("m", "hi", ""), nil)
	mustEqualInt(t, "plain chat/completions status", r3.status, 200)
	mustContain(t, "plain chat/completions raw openai", r3.body, `"object":"chat.completion"`)

	// r4: a STREAMING facade call. Its token usage (13/5, from openAIEcho's
	// stream branch) must also be recorded now — the streaming token-accounting
	// fix — via the usage tee in relayRawStream, while the SSE is relayed
	// verbatim.
	r4 := post(t, si.gwURL("/v1/chat/completions"), openAIBody("m", "hi", `,"stream":true`), nil)
	mustEqualInt(t, "plain chat/completions stream status", r4.status, 200)
	mustContain(t, "plain chat/completions stream chunk", r4.body, "chat.completion.chunk")

	after := scrapeMetrics(t, si)

	// All four calls reached the upstream.
	mustEqualInt(t, "plain: upstream reached by all 4 requests", fake.count("main"), 4)

	// RED: exact per-path request counts.
	mustEqualFloat(t, "RED ccr_http_requests_total{POST,/v1/messages,200} delta",
		metricValue(after, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"})-
			metricValue(before, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"}), 2)
	mustEqualFloat(t, "RED ccr_http_requests_total{POST,/v1/chat/completions,200} delta",
		metricValue(after, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/chat/completions", "status": "200"})-
			metricValue(before, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/chat/completions", "status": "200"}), 2)

	// gen_ai upstream attribution: ALL FOUR calls are attributed now — the two
	// /v1/messages calls and both OpenAI-facade calls (non-stream + stream), at
	// parity with the Anthropic path. See METRIC PARITY note above.
	mustEqualFloat(t, "gen_ai upstream_requests_total{main,main-model} delta (both paths)",
		metricValue(after, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "main", "model": "main-model"})-
			metricValue(before, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "main", "model": "main-model"}), 4)

	// Tokens: /v1/messages non-stream (11/7) + stream (13/5) + facade
	// chat/completions non-stream (11/7) + facade stream (13/5) = input 48,
	// output 24. The two streaming calls prove the usage-tee accounting.
	mustEqualFloat(t, "gen_ai input_tokens_total{main,main-model} delta",
		metricValue(after, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
			metricValue(before, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 48)
	mustEqualFloat(t, "gen_ai output_tokens_total{main,main-model} delta",
		metricValue(after, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
			metricValue(before, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 24)
}

// ---------- Config (b/c): cache backends — a HIT skips the upstream ----------

// testCacheBackendHit is parameterised over the cache backend JSON fragment
// (memory or sqlite) and asserts the SAME contract for both: a second identical
// request is served from cache without a second upstream call, and the
// exact-tier hit counter moves by exactly one.
func testCacheBackendHit(t *testing.T, backendJSON string) {
	fake := newFakeUpstream(t)
	fake.handle("cm", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-cache", "cached answer", 5, 3)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"cm,cache-model"},"Cache":{"enabled":true,%s}}`,
		providerJSON("cm", fake.url("cm"), "sk-cm", "cache-model", ""), backendJSON)
	si := startServe(t, cfg)

	body := anthropicBody("cache-req-model", "what is the capital of France?", "")

	// First request: exact miss → upstream called once, stored.
	r1 := post(t, si.gwURL("/v1/messages"), body, nil)
	mustEqualInt(t, "cache req#1 status", r1.status, 200)
	mustContain(t, "cache req#1 content", r1.body, "cached answer")
	mustEqualInt(t, "upstream calls after req#1 (miss)", fake.count("cm"), 1)

	before := scrapeMetrics(t, si)

	// Second identical request: served from cache, NO upstream call.
	r2 := post(t, si.gwURL("/v1/messages"), body, nil)
	mustEqualInt(t, "cache req#2 status", r2.status, 200)
	mustContain(t, "cache req#2 content", r2.body, "cached answer")
	mustEqualInt(t, "upstream calls after identical req#2 (HIT skips upstream)", fake.count("cm"), 1)

	after := scrapeMetrics(t, si)
	mustEqualFloat(t, "ccr_gen_ai_cache_lookups_total{exact,hit} delta on identical req",
		metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"})-
			metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"}), 1)

	// The HIT is still attributed to the provider/model (replayed usage 5/3).
	mustEqualFloat(t, "cache HIT still records replayed input tokens",
		metricValue(after, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "cm", "model": "cache-model"})-
			metricValue(before, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "cm", "model": "cache-model"}), 5)
}

// ---------- Config (d): semantic cache — a near-duplicate hits ----------

func testCacheSemantic(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("sm", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-sem", "semantic answer", 7, 6)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"sm,sem-model"},"Cache":{"enabled":true,"backend":"memory","semantic":true}}`,
		providerJSON("sm", fake.url("sm"), "sk-sm", "sem-model", ""))
	si := startServe(t, cfg)

	// A long-enough salient turn; the near-duplicate changes exactly one word.
	msgA := "Please write a detailed summary of the quarterly financial report for the executive board meeting scheduled on Monday morning."
	msgB := "Please write a detailed summary of the quarterly financial report for the executive board meeting scheduled on Friday morning."

	r1 := post(t, si.gwURL("/v1/messages"), anthropicBody("sem-req", msgA, ""), nil)
	mustEqualInt(t, "semantic req#1 status", r1.status, 200)
	mustContain(t, "semantic req#1 content", r1.body, "semantic answer")
	mustEqualInt(t, "upstream calls after semantic req#1", fake.count("sm"), 1)

	before := scrapeMetrics(t, si)
	r2 := post(t, si.gwURL("/v1/messages"), anthropicBody("sem-req", msgB, ""), nil)
	mustEqualInt(t, "semantic req#2 status", r2.status, 200)
	mustContain(t, "semantic req#2 content", r2.body, "semantic answer")
	mustEqualInt(t, "upstream calls after near-duplicate req#2 (semantic HIT)", fake.count("sm"), 1)

	after := scrapeMetrics(t, si)
	mustEqualFloat(t, "ccr_gen_ai_cache_lookups_total{semantic,hit} delta on near-duplicate",
		metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "semantic", "result": "hit"})-
			metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "semantic", "result": "hit"}), 1)
}

// ---------- Config (e): cross-provider fallback ----------

func testCrossProviderFallback(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("p1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"primary is down","type":"server_error"}}`)
	})
	fake.handle("p2", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-p2", "answer-from-p2", 9, 4)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s,%s],"Router":{"default":"p1,shared","crossProviderFallback":true}}`,
		providerJSON("p1", fake.url("p1"), "sk-p1", "shared", ""),
		providerJSON("p2", fake.url("p2"), "sk-p2", "shared", ""))
	si := startServe(t, cfg)

	res := post(t, si.gwURL("/v1/messages"), anthropicBody("fallback-req", "hello", ""), nil)
	mustEqualInt(t, "fallback status", res.status, 200)
	mustContain(t, "fallback served secondary answer", res.body, "answer-from-p2")

	if fake.count("p1") < 1 {
		t.Fatalf("expected primary p1 to be hit at least once, got %d", fake.count("p1"))
	}
	if fake.count("p2") < 1 {
		t.Fatalf("expected secondary p2 to be hit at least once, got %d", fake.count("p2"))
	}

	m := scrapeMetrics(t, si)
	if !metricPresent(m, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "p1", "model": "shared"}) {
		t.Fatalf("ccr_gen_ai_upstream_requests_total missing primary p1:\n%s", m)
	}
	// Per-attempt attribution: the secondary that actually served must ALSO appear.
	if !metricPresent(m, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "p2", "model": "shared"}) {
		t.Errorf("PER-ATTEMPT ATTRIBUTION GAP: ccr_gen_ai_upstream_requests_total is missing secondary p2 "+
			"that served the answer (only the primary attributed). Upstream metrics:\n%s", m)
	}
}

// ---------- Config (f): provider transformer cleancache + streamoptions ----------

// testTransformer configures a provider with transformer {cleancache,
// streamoptions} and drives ONE streaming request carrying a tool whose
// input_schema holds a cache_control metadata key. It then inspects the exact
// bytes the upstream received:
//   - streamoptions ⇒ the translated OpenAI body carries
//     "stream_options":{"include_usage":true} (added only on streaming requests);
//   - cleancache    ⇒ the cache_control metadata key was stripped from the
//     forwarded tool schema.
//
// Both are the transformer applied end-to-end, observed at the wire.
func testTransformer(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("tp", func(w http.ResponseWriter, r *http.Request) {
		// Streaming upstream so the SSE translator path (and streamoptions) runs.
		writeOpenAISSE(w, "chatcmpl-tp", []string{"ok"}, 8, 2)
	})

	cfg := fmt.Sprintf(`{"Providers":[{"name":"tp","api_base_url":%q,"api_key":"sk-tp","models":["tm"],"transformer":{"use":["cleancache","streamoptions"]}}],"Router":{"default":"tp,tm"}}`,
		fake.url("tp"))
	si := startServe(t, cfg)

	// A streaming Anthropic request with a tool schema salted with a cache_control
	// metadata KEY (not a property NAMED cache_control, which cleancache keeps).
	body := `{"model":"tm-req","max_tokens":256,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"get_weather","description":"gets weather",` +
		`"input_schema":{"type":"object","properties":{"loc":{"type":"string"}},"cache_control":{"type":"ephemeral"}}}]}`

	before := scrapeMetrics(t, si)
	res := post(t, si.gwURL("/v1/messages"), body, nil)
	mustEqualInt(t, "transformer stream status", res.status, 200)
	mustContain(t, "transformer stream content-type", res.contentType, "text/event-stream")
	events, _ := parseAnthropicSSE(res.body)
	if !hasEvent(events, "message_stop") {
		t.Fatalf("transformer stream missing message_stop; events=%v", events)
	}

	// Inspect the exact upstream request body.
	upBody := fake.lastBody("tp")
	mustContain(t, "streamoptions added stream_options.include_usage", upBody, `"stream_options"`)
	mustContain(t, "streamoptions include_usage true", upBody, `"include_usage":true`)
	mustContain(t, "tool schema still forwarded", upBody, "get_weather")
	mustNotContain(t, "cleancache stripped cache_control from tool schema", upBody, "cache_control")

	after := scrapeMetrics(t, si)
	mustEqualFloat(t, "transformer upstream attributed {tp,tm}",
		metricValue(after, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "tp", "model": "tm"})-
			metricValue(before, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "tp", "model": "tm"}), 1)
	mustEqualFloat(t, "transformer stream tokens recorded (output 2)",
		metricValue(after, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "tp", "model": "tm"})-
			metricValue(before, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "tp", "model": "tm"}), 2)
}

// ---------- Config (g): Router.think — a `thinking` request routes to the think provider ----------

// testRouterThink configures distinct default and think providers and sends a
// request carrying a non-null `thinking` field. The router (rule 3) must route
// it to the think provider — observed via which upstream the fake sees hit and
// the provider label on the upstream metric.
func testRouterThink(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("mainp", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-main", "from-default", 3, 3)
	})
	fake.handle("thinkp", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-think", "from-think", 12, 9)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s,%s],"Router":{"default":"mainp,main-model","think":"thinkp,think-model"}}`,
		providerJSON("mainp", fake.url("mainp"), "sk-main", "main-model", ""),
		providerJSON("thinkp", fake.url("thinkp"), "sk-think", "think-model", ""))
	si := startServe(t, cfg)

	// Ordinary model id (no explicit selector, not haiku) + a thinking field.
	body := anthropicBody("ordinary-model", "reason about this", `,"thinking":{"type":"enabled","budget_tokens":1024}`)
	before := scrapeMetrics(t, si)
	res := post(t, si.gwURL("/v1/messages"), body, nil)
	mustEqualInt(t, "think routing status", res.status, 200)
	mustContain(t, "think routing served think provider", res.body, "from-think")

	// The think provider was hit; the default provider was NOT.
	mustEqualInt(t, "think provider hit", fake.count("thinkp"), 1)
	mustEqualInt(t, "default provider NOT hit for a thinking request", fake.count("mainp"), 0)

	after := scrapeMetrics(t, si)
	mustEqualFloat(t, "upstream attributed to think provider {thinkp,think-model}",
		metricValue(after, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "thinkp", "model": "think-model"})-
			metricValue(before, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "thinkp", "model": "think-model"}), 1)
	if metricPresent(after, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "mainp", "model": "main-model"}) {
		t.Errorf("MISROUTE: a thinking request attributed to the DEFAULT provider mainp:\n%s", after)
	}
}

// ---------- Multi-provider routing: default vs background(haiku) vs explicit selector ----------

// testMultiProviderRouting proves the three body-driven routing modes each reach
// their intended provider against one config carrying all three:
//   - a bare, non-haiku model    → Router.default   → defprov;
//   - a haiku-tier model         → Router.background → bgprov;
//   - an explicit "prov,model"   → that exact prov   → selprov.
func testMultiProviderRouting(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.handle("defprov", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-def", "from-default", 1, 1)
	})
	fake.handle("bgprov", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-bg", "from-background", 1, 1)
	})
	fake.handle("selprov", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-sel", "from-selector", 1, 1)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s,%s,%s],"Router":{"default":"defprov,default-model","background":"bgprov,bg-model"}}`,
		providerJSON("defprov", fake.url("defprov"), "sk-def", "default-model", ""),
		providerJSON("bgprov", fake.url("bgprov"), "sk-bg", "bg-model", ""),
		providerJSON("selprov", fake.url("selprov"), "sk-sel", "sel-model", ""))
	si := startServe(t, cfg)

	// (1) default: a bare, non-haiku model id.
	rd := post(t, si.gwURL("/v1/messages"), anthropicBody("regular-model", "hi", ""), nil)
	mustEqualInt(t, "default route status", rd.status, 200)
	mustContain(t, "default route served defprov", rd.body, "from-default")

	// (2) background: a haiku-tier model id.
	rb := post(t, si.gwURL("/v1/messages"), anthropicBody("claude-3-5-haiku-test", "hi", ""), nil)
	mustEqualInt(t, "background route status", rb.status, 200)
	mustContain(t, "background route served bgprov", rb.body, "from-background")

	// (3) explicit selector: "selprov,sel-model" pins the exact upstream.
	rs := post(t, si.gwURL("/v1/messages"), anthropicBody("selprov,sel-model", "hi", ""), nil)
	mustEqualInt(t, "explicit selector status", rs.status, 200)
	mustContain(t, "explicit selector served selprov", rs.body, "from-selector")

	// Each provider was reached exactly once by its intended request.
	mustEqualInt(t, "defprov hit once", fake.count("defprov"), 1)
	mustEqualInt(t, "bgprov hit once", fake.count("bgprov"), 1)
	mustEqualInt(t, "selprov hit once", fake.count("selprov"), 1)

	// Upstream attribution carries the correct provider+model per mode.
	m := scrapeMetrics(t, si)
	for _, want := range []map[string]string{
		{"provider": "defprov", "model": "default-model"},
		{"provider": "bgprov", "model": "bg-model"},
		{"provider": "selprov", "model": "sel-model"},
	} {
		if !metricPresent(m, "ccr_gen_ai_upstream_requests_total", want) {
			t.Errorf("ccr_gen_ai_upstream_requests_total missing %v:\n%s", want, m)
		}
	}
}
