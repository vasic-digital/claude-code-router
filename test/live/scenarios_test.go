package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- config-JSON builders ----------

// providerJSON renders one Providers[] entry. protocol is emitted only when
// non-empty (an absent protocol is the on-disk default and exercises inference).
func providerJSON(name, url, key, model, protocol string) string {
	proto := ""
	if protocol != "" {
		proto = fmt.Sprintf(`,"protocol":%q`, protocol)
	}
	return fmt.Sprintf(`{"name":%q,"api_base_url":%q,"api_key":%q,"models":[%q]%s}`,
		name, url, key, model, proto)
}

// anthropicBody builds a POST /v1/messages request body. extra is appended raw
// (e.g. `,"stream":true` or `,"temperature":0.7`).
func anthropicBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}

// openAIBody builds a POST /v1/chat/completions request body.
func openAIBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}

// ---------- small assertion helpers ----------

func mustContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected to contain %q, got:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s: expected NOT to contain %q, got:\n%s", what, needle, haystack)
	}
}

func mustEqualInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", what, got, want)
	}
}

func mustEqualFloat(t *testing.T, what string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// parseAnthropicSSE collects the event names and concatenates every text_delta's
// text from an Anthropic Messages SSE stream.
func parseAnthropicSSE(body string) (events []string, text string) {
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if e, ok := strings.CutPrefix(line, "event:"); ok {
			events = append(events, strings.TrimSpace(e))
			continue
		}
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			d = strings.TrimSpace(d)
			if d == "" || d == "[DONE]" {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(d), &m) != nil {
				continue
			}
			if m["type"] == "content_block_delta" {
				if delta, ok := m["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
					if tx, ok := delta["text"].(string); ok {
						text += tx
					}
				}
			}
		}
	}
	return events, text
}

func hasEvent(events []string, name string) bool {
	for _, e := range events {
		if e == name {
			return true
		}
	}
	return false
}

// ---------- The live suite ----------

// TestLiveEndToEnd builds the ccr binary, then drives real HTTP through real
// `ccr serve` subprocesses against a fake upstream. Each scenario is a subtest
// so one failure is localized to that scenario.
func TestLiveEndToEnd(t *testing.T) {
	requireBinary(t)

	fake := newFakeUpstream(t)

	// --- handlers for the shared "basic" instance ---
	fake.handle("main", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(b, &probe)
		if probe.Stream {
			writeOpenAISSE(w, "chatcmpl-main-stream", []string{"Hello", ", world", "!"}, 13, 5)
			return
		}
		writeOpenAICompletion(w, "chatcmpl-main", "Hello from the upstream.", 11, 7)
	})
	fake.handle("err401", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		// The upstream error body deliberately does NOT echo any credential;
		// the gateway must not leak the request's api_key either.
		io.WriteString(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
	})
	fake.handle("anthro", func(w http.ResponseWriter, r *http.Request) {
		// Never reached: the OpenAI facade returns 501 before any upstream call.
		writeOpenAICompletion(w, "chatcmpl-anthro", "should-not-be-called", 1, 1)
	})

	basicCfg := fmt.Sprintf(`{"Providers":[%s,%s,%s],"Router":{"default":"main,main-model"}}`,
		providerJSON("main", fake.url("main"), "sk-main-key", "main-model", ""),
		providerJSON("err401", fake.url("err401"), "sk-live-SECRET401", "m401", ""),
		providerJSON("anthro", fake.url("anthro"), "sk-anthro-key", "claude-native", "anthropic"),
	)
	basic := startServe(t, basicCfg)

	// ---- Scenario 1: /health and /ready ----
	t.Run("health_and_ready", func(t *testing.T) {
		h := get(t, basic.gwURL("/health"))
		mustEqualInt(t, "GET /health status", h.status, 200)
		mustContain(t, "GET /health body", h.body, `"status":"ok"`)
		mustContain(t, "GET /health providers", h.body, `"providers":3`)

		rd := get(t, basic.gwURL("/ready"))
		mustEqualInt(t, "GET /ready status (default route present)", rd.status, 200)
		mustContain(t, "GET /ready body", rd.body, `"status":"ready"`)

		// /metrics must be live on the management server.
		m := get(t, basic.mgmtURL("/metrics"))
		mustEqualInt(t, "GET /metrics status", m.status, 200)
		mustContain(t, "GET /metrics body", m.body, "ccr_http_requests_total")

		// A separate instance with a provider but NO default route: /ready is 503.
		noRouteCfg := fmt.Sprintf(`{"Providers":[%s],"Router":{}}`,
			providerJSON("main", fake.url("main"), "sk-x", "main-model", ""))
		noRoute := startServe(t, noRouteCfg)
		rd2 := get(t, noRoute.gwURL("/ready"))
		mustEqualInt(t, "GET /ready status (no default route)", rd2.status, 503)
		mustContain(t, "GET /ready body (no default route)", rd2.body, "no default route")
	})

	// ---- Scenario 2: POST /v1/messages non-streaming translation + metrics ----
	t.Run("messages_nonstreaming_translation", func(t *testing.T) {
		before := scrapeMetrics(t, basic)
		res := post(t, basic.gwURL("/v1/messages"),
			anthropicBody("claude-sonnet-live", "hello there", ""), nil)
		mustEqualInt(t, "POST /v1/messages status", res.status, 200)
		mustContain(t, "response content-type", res.contentType, "application/json")

		// The client must receive a well-formed ANTHROPIC message (translation happened).
		mustContain(t, "translated type", res.body, `"type":"message"`)
		mustContain(t, "translated role", res.body, `"role":"assistant"`)
		mustContain(t, "translated text", res.body, "Hello from the upstream.")
		mustContain(t, "translated stop_reason", res.body, `"stop_reason":"end_turn"`)
		mustContain(t, "translated usage input", res.body, `"input_tokens":11`)
		mustContain(t, "translated usage output", res.body, `"output_tokens":7`)
		// It must NOT be the raw OpenAI object shape.
		mustNotContain(t, "no raw openai object", res.body, `"object":"chat.completion"`)

		after := scrapeMetrics(t, basic)
		mustEqualFloat(t, "ccr_http_requests_total{POST,/v1/messages,200} delta",
			metricValue(after, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"})-
				metricValue(before, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"}), 1)
		mustEqualFloat(t, "ccr_gen_ai_upstream_requests_total{main,main-model} delta",
			metricValue(after, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "main", "model": "main-model"})-
				metricValue(before, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "main", "model": "main-model"}), 1)
		mustEqualFloat(t, "ccr_gen_ai_input_tokens_total{main,main-model} delta",
			metricValue(after, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
				metricValue(before, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 11)
		mustEqualFloat(t, "ccr_gen_ai_output_tokens_total{main,main-model} delta",
			metricValue(after, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
				metricValue(before, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 7)
	})

	// ---- Scenario 3: POST /v1/messages streaming ----
	t.Run("messages_streaming_translation", func(t *testing.T) {
		before := scrapeMetrics(t, basic)
		res := post(t, basic.gwURL("/v1/messages"),
			anthropicBody("claude-sonnet-live", "stream please", `,"stream":true`), nil)
		mustEqualInt(t, "POST /v1/messages (stream) status", res.status, 200)
		mustContain(t, "stream content-type", res.contentType, "text/event-stream")

		events, text := parseAnthropicSSE(res.body)
		if !hasEvent(events, "message_start") {
			t.Fatalf("streaming: missing message_start event; events=%v", events)
		}
		if !hasEvent(events, "content_block_delta") {
			t.Fatalf("streaming: missing content_block_delta event; events=%v", events)
		}
		if !hasEvent(events, "message_stop") {
			t.Fatalf("streaming: missing message_stop event; events=%v", events)
		}
		if text != "Hello, world!" {
			t.Fatalf("streaming: concatenated text = %q, want %q", text, "Hello, world!")
		}

		after := scrapeMetrics(t, basic)
		mustEqualFloat(t, "streaming ccr_gen_ai_input_tokens_total{main,main-model} delta",
			metricValue(after, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
				metricValue(before, "ccr_gen_ai_input_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 13)
		mustEqualFloat(t, "streaming ccr_gen_ai_output_tokens_total{main,main-model} delta",
			metricValue(after, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"})-
				metricValue(before, "ccr_gen_ai_output_tokens_total", map[string]string{"provider": "main", "model": "main-model"}), 5)
	})

	// ---- Scenario 4: POST /v1/chat/completions (OpenAI facade) ----
	t.Run("openai_facade_relay_and_501", func(t *testing.T) {
		before := scrapeMetrics(t, basic)
		// Relay: a bare model routes to Router.Default (the OpenAI-shaped "main").
		res := post(t, basic.gwURL("/v1/chat/completions"),
			openAIBody("gpt-4o-live", "hi", ""), nil)
		mustEqualInt(t, "POST /v1/chat/completions status", res.status, 200)
		// The OpenAI response is relayed straight back (raw OpenAI object shape).
		mustContain(t, "relayed openai object", res.body, `"object":"chat.completion"`)
		mustContain(t, "relayed openai id", res.body, "chatcmpl-main")
		mustContain(t, "relayed openai content", res.body, "Hello from the upstream.")
		// It must NOT have been translated to the Anthropic message shape.
		mustNotContain(t, "not anthropic-shaped", res.body, `"type":"message"`)

		after := scrapeMetrics(t, basic)
		mustEqualFloat(t, "ccr_http_requests_total{POST,/v1/chat/completions,200} delta",
			metricValue(after, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/chat/completions", "status": "200"})-
				metricValue(before, "ccr_http_requests_total", map[string]string{"method": "POST", "path": "/v1/chat/completions", "status": "200"}), 1)

		// 501: an OpenAI-inbound request routed (via explicit selector) to an
		// Anthropic-native provider cannot be bridged.
		res501 := post(t, basic.gwURL("/v1/chat/completions"),
			openAIBody("anthro,claude-native", "hi", ""), nil)
		mustEqualInt(t, "POST /v1/chat/completions (anthropic-native) status", res501.status, 501)
		mustContain(t, "501 openai error envelope", res501.body, `"error"`)
		mustContain(t, "501 names the provider", res501.body, "anthro")
	})

	// ---- Scenario 5: upstream error mapping (401) ----
	t.Run("upstream_error_mapping_401", func(t *testing.T) {
		res := post(t, basic.gwURL("/v1/messages"),
			anthropicBody("err401,m401", "trigger 401", ""), nil)
		mustEqualInt(t, "401 mapped status", res.status, 401)
		mustContain(t, "401 anthropic error envelope", res.body, `"type":"error"`)
		mustContain(t, "401 mapped error type", res.body, `"authentication_error"`)
		// The client's api_key must never appear in the error body.
		mustNotContain(t, "no api_key leak in error body", res.body, "SECRET401")
		mustNotContain(t, "no bearer leak in error body", res.body, "Bearer")
	})

	// ---- Scenario 6: response cache (memory, exact tier) ----
	t.Run("cache_exact_hit_and_temperature_bypass", func(t *testing.T) {
		fake.handle("cachemain", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-cache", "cached answer", 5, 3)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"cachemain,cm"},"Cache":{"enabled":true,"backend":"memory"}}`,
			providerJSON("cachemain", fake.url("cachemain"), "sk-cache", "cm", ""))
		inst := startServe(t, cfg)

		body := anthropicBody("cache-model-live", "what is the capital of France?", "")

		// First request: exact miss -> upstream called once, stored.
		r1 := post(t, inst.gwURL("/v1/messages"), body, nil)
		mustEqualInt(t, "cache req#1 status", r1.status, 200)
		mustContain(t, "cache req#1 content", r1.body, "cached answer")
		mustEqualInt(t, "upstream calls after req#1", fake.count("cachemain"), 1)

		before := scrapeMetrics(t, inst)
		// Second identical request: served from cache, NO upstream call.
		r2 := post(t, inst.gwURL("/v1/messages"), body, nil)
		mustEqualInt(t, "cache req#2 status", r2.status, 200)
		mustContain(t, "cache req#2 content", r2.body, "cached answer")
		mustEqualInt(t, "upstream calls after identical req#2 (cache hit)", fake.count("cachemain"), 1)

		after := scrapeMetrics(t, inst)
		mustEqualFloat(t, "ccr_gen_ai_cache_lookups_total{exact,hit} delta on identical req",
			metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"})-
				metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"}), 1)

		// A temperature>0 request is NOT cacheable: two such requests each hit upstream.
		tbody := anthropicBody("cache-model-live", "what is the capital of France?", `,"temperature":0.7`)
		post(t, inst.gwURL("/v1/messages"), tbody, nil)
		post(t, inst.gwURL("/v1/messages"), tbody, nil)
		mustEqualInt(t, "upstream calls after two temperature>0 requests", fake.count("cachemain"), 3)
	})

	// ---- Scenario 7: cross-provider fallback ----
	t.Run("cross_provider_fallback", func(t *testing.T) {
		fake.handle("p1", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"primary is down","type":"server_error"}}`)
		})
		fake.handle("p2", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-p2", "answer-from-p2", 9, 4)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s,%s],"Router":{"default":"p1,shared","crossProviderFallback":true}}`,
			providerJSON("p1", fake.url("p1"), "sk-p1", "shared", ""),
			providerJSON("p2", fake.url("p2"), "sk-p2", "shared", ""))
		inst := startServe(t, cfg)

		res := post(t, inst.gwURL("/v1/messages"),
			anthropicBody("fallback-model-live", "hello", ""), nil)
		mustEqualInt(t, "fallback status", res.status, 200)
		mustContain(t, "fallback served secondary answer", res.body, "answer-from-p2")

		// Both real upstream providers were actually contacted.
		if fake.count("p1") < 1 {
			t.Fatalf("expected primary p1 to be hit at least once, got %d", fake.count("p1"))
		}
		if fake.count("p2") < 1 {
			t.Fatalf("expected secondary p2 to be hit at least once, got %d", fake.count("p2"))
		}

		m := scrapeMetrics(t, inst)
		// The primary always appears in upstream attribution.
		if !metricPresent(m, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "p1", "model": "shared"}) {
			t.Fatalf("ccr_gen_ai_upstream_requests_total missing primary p1:\n%s", m)
		}
		// Per-attempt attribution: the secondary that actually served the answer
		// should also appear. (The gateway's per-attempt attribution is being
		// fixed concurrently; if this is not yet landed it is reported, not
		// silently ignored — see the test's final report.)
		if !metricPresent(m, "ccr_gen_ai_upstream_requests_total", map[string]string{"provider": "p2", "model": "shared"}) {
			t.Errorf("PER-ATTEMPT ATTRIBUTION GAP: ccr_gen_ai_upstream_requests_total is missing the secondary provider p2 "+
				"that actually served the answer (only the primary is attributed). Upstream metrics:\n%s", m)
		}
	})

	// ---- Scenario 8: semantic cache (near-duplicate) ----
	t.Run("semantic_cache_near_duplicate", func(t *testing.T) {
		fake.handle("semmain", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-sem", "semantic answer", 7, 6)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"semmain,sm"},"Cache":{"enabled":true,"backend":"memory","semantic":true}}`,
			providerJSON("semmain", fake.url("semmain"), "sk-sem", "sm", ""))
		inst := startServe(t, cfg)

		// A long-enough salient turn; the near-duplicate changes exactly one word.
		msgA := "Please write a detailed summary of the quarterly financial report for the executive board meeting scheduled on Monday morning."
		msgB := "Please write a detailed summary of the quarterly financial report for the executive board meeting scheduled on Friday morning."

		r1 := post(t, inst.gwURL("/v1/messages"), anthropicBody("sem-model-live", msgA, ""), nil)
		mustEqualInt(t, "semantic req#1 status", r1.status, 200)
		mustContain(t, "semantic req#1 content", r1.body, "semantic answer")
		mustEqualInt(t, "upstream calls after semantic req#1", fake.count("semmain"), 1)

		before := scrapeMetrics(t, inst)
		r2 := post(t, inst.gwURL("/v1/messages"), anthropicBody("sem-model-live", msgB, ""), nil)
		mustEqualInt(t, "semantic req#2 status", r2.status, 200)
		mustContain(t, "semantic req#2 content", r2.body, "semantic answer")
		// Served from the semantic tier: NO second upstream call.
		mustEqualInt(t, "upstream calls after near-duplicate req#2 (semantic hit)", fake.count("semmain"), 1)

		after := scrapeMetrics(t, inst)
		mustEqualFloat(t, "ccr_gen_ai_cache_lookups_total{semantic,hit} delta on near-duplicate",
			metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "semantic", "result": "hit"})-
				metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "semantic", "result": "hit"}), 1)
	})

	// ---- Scenario 9: `ccr config validate` and `ccr config show` ----
	t.Run("config_validate_and_show", func(t *testing.T) {
		home := t.TempDir()
		cfgPath := filepath.Join(home, "config.json")
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"showprov,sm"},"Cache":{"enabled":true,"backend":"memory","semantic":true,"semantic_threshold":0.85}}`,
			providerJSON("showprov", "https://api.example.com/v1/chat/completions", "sk-show-SECRETXYZ", "sm", ""))
		if err := writeFile(cfgPath, cfg); err != nil {
			t.Fatalf("write config: %v", err)
		}

		code, out := runCCR(t, home, "config", "validate", cfgPath)
		mustEqualInt(t, "ccr config validate exit code", code, 0)
		mustContain(t, "ccr config validate output", out, "is valid")

		code2, out2 := runCCR(t, home, "config", "show", cfgPath)
		mustEqualInt(t, "ccr config show exit code", code2, 0)
		mustContain(t, "ccr config show redacts api_key", out2, "[REDACTED]")
		mustContain(t, "ccr config show includes Cache block", out2, "Cache")
		mustNotContain(t, "ccr config show must not leak the real key", out2, "SECRETXYZ")
	})
}
