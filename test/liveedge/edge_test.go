package liveedge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// canarySecret is a provider api_key configured on disk. Under EVERY error path
// exercised below, this string must NEVER appear in a client-visible response
// body, nor in any /metrics label. A single occurrence is a real secret leak.
const canarySecret = "sk-canary-DO-NOT-LEAK-9c3f1a2b7e"

// providerJSON renders one Providers[] entry (protocol emitted only when set).
func providerJSON(name, url, key, model, protocol string) string {
	proto := ""
	if protocol != "" {
		proto = fmt.Sprintf(`,"protocol":%q`, protocol)
	}
	return fmt.Sprintf(`{"name":%q,"api_base_url":%q,"api_key":%q,"models":[%q]%s}`,
		name, url, key, model, proto)
}

// anthropicBody builds a POST /v1/messages body. content is JSON-encoded (not
// %q) so control chars / NUL / unicode are correctly escaped as valid JSON.
// extra is appended raw.
func anthropicBody(model, content, extra string) string {
	cb, _ := json.Marshal(content)
	return fmt.Sprintf(`{"model":%q,"max_tokens":256,"messages":[{"role":"user","content":%s}]%s}`,
		model, cb, extra)
}

// openAIBody builds a POST /v1/chat/completions body.
func openAIBody(model, content, extra string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]%s}`,
		model, content, extra)
}

// parseAnthropicSSEEvents returns the ordered event names of an Anthropic
// Messages SSE stream.
func parseAnthropicSSEEvents(body string) []string {
	var events []string
	for _, line := range strings.Split(body, "\n") {
		if e, ok := strings.CutPrefix(line, "event:"); ok {
			events = append(events, strings.TrimSpace(e))
		}
	}
	return events
}

func hasEvent(events []string, name string) bool {
	for _, e := range events {
		if e == name {
			return true
		}
	}
	return false
}

// TestLiveEdge boots ONE ccr serve subprocess against a fake upstream and drives
// a battery of malformed / hostile REAL HTTP at it. Every subtest asserts the
// response shape AND that the server survives; a final subtest proves no panic /
// goroutine dump escaped into the serve log after the whole adversarial batch.
func TestLiveEdge(t *testing.T) {
	requireBinary(t)

	fake := newFakeUpstream(t)

	// --- upstream handlers ---

	// "main": a well-behaved OpenAI completion (the default route).
	fake.handle("main", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-main", "Hello from the upstream.", 11, 7)
	})
	// "echo": echoes the (translated) last user message content back as the
	// completion, so a round-trip can prove unicode/content is not corrupted.
	fake.handle("echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var oreq struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		content := ""
		if json.Unmarshal(b, &oreq) == nil && len(oreq.Messages) > 0 {
			var s string
			if json.Unmarshal(oreq.Messages[len(oreq.Messages)-1].Content, &s) == nil {
				content = s
			}
		}
		writeOpenAICompletion(w, "chatcmpl-echo", content, 3, 3)
	})
	// "err401" / "err500": upstreams that fail without echoing any credential.
	fake.handle("err401", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
	})
	fake.handle("err500", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
	})
	// "garbage": a 200 whose body is NOT JSON — the gateway must not relay it as
	// a valid message. The marker must never reach the client.
	fake.handle("garbage", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "GARBAGE_NOT_JSON_zzz <html>not a completion</html>")
	})
	// "emptyup": a 200 with an empty body.
	fake.handle("emptyup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	// "zeroch": a 200 that is valid JSON but carries zero choices.
	fake.handle("zeroch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	})
	// "bigup": a 200 with a large (but < cap) completion body.
	fake.handle("bigup", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-big", "BIGMARKER"+strings.Repeat("B", 8<<20), 2, 2)
	})
	// "trickle": a STREAMING upstream that flushes a partial SSE then closes the
	// connection mid-stream (no [DONE]). Done via a raw hijack so the response is
	// close-delimited and ends with an abrupt EOF in the middle of a data line.
	fake.handle("trickle", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Close-delimited HTTP/1.1 response: no Content-Length, no chunked.
		io.WriteString(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		_ = buf.Flush()
		io.WriteString(buf, "data: {\"id\":\"tk\",\"object\":\"chat.completion.chunk\",\"choices\":"+
			"[{\"index\":0,\"delta\":{\"content\":\"Partial\"},\"finish_reason\":null}]}\n\n")
		_ = buf.Flush()
		time.Sleep(40 * time.Millisecond) // slow trickle
		// A partial data line with NO terminating newline, then abrupt close.
		io.WriteString(buf, "data: {\"id\":\"tk\",\"choi")
		_ = buf.Flush()
		// return -> conn.Close(): EOF mid-stream, no [DONE].
	})
	// "anthro": Anthropic-native provider (never actually called on the OpenAI
	// facade — it 501s first).
	fake.handle("anthro", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAICompletion(w, "chatcmpl-anthro", "should-not-be-called", 1, 1)
	})

	cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"main,mm"}}`,
		strings.Join([]string{
			providerJSON("main", fake.url("main"), "sk-main", "mm", ""),
			providerJSON("echo", fake.url("echo"), "sk-echo", "em", ""),
			providerJSON("err401", fake.url("err401"), "sk-plain-401", "m401", ""),
			providerJSON("err500", fake.url("err500"), "sk-plain-500", "m500", ""),
			providerJSON("garbage", fake.url("garbage"), "sk-plain-g", "mg", ""),
			providerJSON("emptyup", fake.url("emptyup"), "sk-plain-e", "me", ""),
			providerJSON("zeroch", fake.url("zeroch"), "sk-plain-z", "mz", ""),
			providerJSON("bigup", fake.url("bigup"), "sk-plain-b", "mb", ""),
			providerJSON("trickle", fake.url("trickle"), "sk-plain-t", "mt", ""),
			providerJSON("anthro", fake.url("anthro"), "sk-anthro", "claude-native", "anthropic"),
			// Canary providers: identical secret api_key, pointed at failing upstreams.
			providerJSON("canary401", fake.url("err401"), canarySecret, "cm", ""),
			providerJSON("canary500", fake.url("err500"), canarySecret, "cm", ""),
			providerJSON("canaryGarbage", fake.url("garbage"), canarySecret, "cm", ""),
		}, ","))
	si := startServe(t, cfg)

	// clientBodies accumulates EVERY client-visible response body seen across the
	// secret-safety subtest for a final grep-count == 0 canary assertion.
	var (
		bodyMu       sync.Mutex
		clientBodies []string
	)
	record := func(b string) {
		bodyMu.Lock()
		clientBodies = append(clientBodies, b)
		bodyMu.Unlock()
	}

	// ---- OVERSIZED body -> 413, server stays up, no OOM/crash ----
	t.Run("oversized_body_413_and_survives", func(t *testing.T) {
		// A body comfortably larger than the 32MiB inbound cap.
		huge := anthropicBody("main,mm", strings.Repeat("A", 33<<20), "")
		res := postRawOversized(t, si.gwHostPort(), "/v1/messages", huge)
		mustEqualInt(t, "oversized POST status", res.status, 413)
		mustContain(t, "oversized error shape", res.body, `"type":"error"`)
		mustContain(t, "oversized error type", res.body, "invalid_request_error")
		mustContain(t, "oversized error mentions limit", res.body, "limit")
		assertServerUp(t, si)
	})

	// ---- MALFORMED JSON -> 400 invalid_request_error, no crash ----
	t.Run("malformed_json_400", func(t *testing.T) {
		res := post(t, si.gwURL("/v1/messages"), `{"model":"main,mm", this is not json`, nil)
		mustEqualInt(t, "malformed POST status", res.status, 400)
		mustContain(t, "malformed error shape", res.body, `"type":"error"`)
		mustContain(t, "malformed error type", res.body, "invalid_request_error")
		assertServerUp(t, si)
	})

	// ---- WRONG METHOD / UNKNOWN ROUTE / classified-not-served ----
	t.Run("wrong_method_and_unknown_routes", func(t *testing.T) {
		// GET on a POST-only endpoint: gin (HandleMethodNotAllowed off) answers 404.
		g := request(t, http.MethodGet, si.gwURL("/v1/messages"))
		if g.status != http.StatusNotFound && g.status != http.StatusMethodNotAllowed {
			t.Fatalf("GET /v1/messages: got %d, want 404 or 405", g.status)
		}
		// Unknown path.
		n := post(t, si.gwURL("/v1/nonsense"), `{}`, nil)
		mustEqualInt(t, "POST /v1/nonsense status", n.status, 404)
		// /v1/responses classifies to a known family but is deliberately NOT served.
		r := post(t, si.gwURL("/v1/responses"), `{"model":"main,mm"}`, nil)
		mustEqualInt(t, "POST /v1/responses status (classified-not-served)", r.status, 404)
		assertServerUp(t, si)
	})

	// ---- OpenAI facade edge: invalid JSON -> OpenAI 400; anthropic-native -> 501 ----
	t.Run("openai_facade_invalid_json_and_501", func(t *testing.T) {
		bad := post(t, si.gwURL("/v1/chat/completions"), `{"model": bad json`, nil)
		mustEqualInt(t, "openai invalid JSON status", bad.status, 400)
		mustContain(t, "openai error envelope", bad.body, `"error"`)
		mustContain(t, "openai error type", bad.body, "invalid_request_error")
		mustContain(t, "openai error code null", bad.body, `"code":null`)

		n501 := post(t, si.gwURL("/v1/chat/completions"), openAIBody("anthro,claude-native", "hi", ""), nil)
		mustEqualInt(t, "openai->anthropic-native status", n501.status, 501)
		mustContain(t, "501 openai envelope", n501.body, `"error"`)
		mustContain(t, "501 names the provider", n501.body, "anthro")
		assertServerUp(t, si)
	})

	// ---- UPSTREAM MISBEHAVIOR: garbage / empty / zero-choices / large / EOF ----
	t.Run("upstream_garbage_200_clean_api_error", func(t *testing.T) {
		res := post(t, si.gwURL("/v1/messages"), anthropicBody("garbage,mg", "go", ""), nil)
		mustEqualInt(t, "garbage upstream status", res.status, 502)
		mustContain(t, "garbage mapped to api_error", res.body, "api_error")
		// The raw garbage must NOT be relayed as if it were a valid message.
		mustNotContain(t, "garbage not relayed", res.body, "GARBAGE_NOT_JSON_zzz")
		mustNotContain(t, "garbage not a message", res.body, `"type":"message"`)
		assertServerUp(t, si)
	})

	t.Run("upstream_empty_body_clean_api_error", func(t *testing.T) {
		res := post(t, si.gwURL("/v1/messages"), anthropicBody("emptyup,me", "go", ""), nil)
		mustEqualInt(t, "empty upstream status", res.status, 502)
		mustContain(t, "empty mapped to api_error", res.body, "api_error")
		mustNotContain(t, "empty not a message", res.body, `"type":"message"`)
		assertServerUp(t, si)
	})

	t.Run("upstream_zero_choices_clean_api_error", func(t *testing.T) {
		res := post(t, si.gwURL("/v1/messages"), anthropicBody("zeroch,mz", "go", ""), nil)
		mustEqualInt(t, "zero-choices upstream status", res.status, 502)
		mustContain(t, "zero-choices message", res.body, "no choices")
		mustNotContain(t, "zero-choices not a message", res.body, `"type":"message"`)
		assertServerUp(t, si)
	})

	t.Run("upstream_large_under_cap_ok", func(t *testing.T) {
		res := post(t, si.gwURL("/v1/messages"), anthropicBody("bigup,mb", "go", ""), nil)
		mustEqualInt(t, "large-under-cap status", res.status, 200)
		mustContain(t, "large-under-cap translated", res.body, `"type":"message"`)
		mustContain(t, "large-under-cap content marker", res.body, "BIGMARKER")
		assertServerUp(t, si)
	})

	t.Run("streaming_upstream_eof_midstream_terminates_cleanly", func(t *testing.T) {
		// A gateway hang here FAILS via post()'s bounded client timeout rather than
		// blocking forever. A clean end means the SSE the client got is still
		// well-formed-terminated (message_stop present).
		res := post(t, si.gwURL("/v1/messages"), anthropicBody("trickle,mt", "go", `,"stream":true`), nil)
		mustEqualInt(t, "midstream-EOF status", res.status, 200)
		mustContain(t, "midstream-EOF is SSE", res.contentType, "text/event-stream")
		events := parseAnthropicSSEEvents(res.body)
		if !hasEvent(events, "message_start") {
			t.Fatalf("midstream-EOF: missing message_start; events=%v", events)
		}
		if !hasEvent(events, "message_stop") {
			t.Fatalf("midstream-EOF: stream not well-formed-terminated (no message_stop); events=%v", events)
		}
		assertServerUp(t, si)
	})

	// ---- SECRET SAFETY under errors: canary api_key must never leak ----
	t.Run("secret_never_leaks_under_errors", func(t *testing.T) {
		r401 := post(t, si.gwURL("/v1/messages"), anthropicBody("canary401,cm", "go", ""), nil)
		record(r401.body)
		mustEqualInt(t, "canary 401 status", r401.status, 401)
		mustNotContain(t, "canary key not in 401 body", r401.body, canarySecret)
		mustNotContain(t, "no bearer in 401 body", r401.body, "Bearer")

		r500 := post(t, si.gwURL("/v1/messages"), anthropicBody("canary500,cm", "go", ""), nil)
		record(r500.body)
		// A 500 from upstream is Retryable and exhausts to a 502 api_error envelope.
		if r500.status != 500 && r500.status != 502 {
			t.Fatalf("canary 500 status: got %d, want 500 or 502", r500.status)
		}
		mustNotContain(t, "canary key not in 500 body", r500.body, canarySecret)

		rG := post(t, si.gwURL("/v1/messages"), anthropicBody("canaryGarbage,cm", "go", ""), nil)
		record(rG.body)
		mustEqualInt(t, "canary garbage status", rG.status, 502)
		mustNotContain(t, "canary key not in garbage body", rG.body, canarySecret)

		// /metrics must not carry the secret in any label.
		metrics := scrapeMetrics(t, si)
		record(metrics)
		mustNotContain(t, "canary key not in /metrics", metrics, canarySecret)

		// Final grep-count == 0 across every captured client body + metrics.
		bodyMu.Lock()
		defer bodyMu.Unlock()
		leaks := 0
		for _, b := range clientBodies {
			leaks += strings.Count(b, canarySecret)
		}
		mustEqualInt(t, "canary total occurrences across all captured client bodies + metrics", leaks, 0)
	})

	// ---- UNICODE / control chars handled without corruption (round-trip) ----
	t.Run("unicode_and_control_chars_roundtrip", func(t *testing.T) {
		// Real multibyte UTF-8, an emoji, a RTL override, and JSON-escaped control
		// chars. The echo upstream returns the translated content verbatim.
		marker := "\u65e5\u672c\u8a9e-\U0001F680-\u202e-tail-\u0000\u0007-END"
		body := anthropicBody("echo,em", marker, "")
		res := post(t, si.gwURL("/v1/messages"), body, nil)
		mustEqualInt(t, "unicode status", res.status, 200)
		mustContain(t, "unicode translated", res.body, `"type":"message"`)
		// Re-decode the Anthropic message and compare the text block byte-for-byte.
		var msg struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(res.body), &msg); err != nil {
			t.Fatalf("unicode: response is not valid JSON: %v\n%s", err, res.body)
		}
		got := ""
		for _, b := range msg.Content {
			if b.Type == "text" {
				got += b.Text
			}
		}
		if got != marker {
			t.Fatalf("unicode corrupted: got %q, want %q", got, marker)
		}
		assertServerUp(t, si)
	})

	// ---- CONCURRENT malformed + valid mixed: no cross-contamination ----
	t.Run("concurrent_mixed_no_cross_contamination", func(t *testing.T) {
		const pairs = 16
		type outcome struct {
			idx      int
			valid    bool
			status   int
			body     string
			expected string // for valid requests, the unique echo marker
		}
		results := make([]outcome, 0, pairs*2)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for i := 0; i < pairs; i++ {
			i := i
			// One VALID request with a unique marker echoed back.
			wg.Add(1)
			go func() {
				defer wg.Done()
				marker := fmt.Sprintf("VALID-%d-nonce-%d", i, i*7+3)
				r := post(t, si.gwURL("/v1/messages"), anthropicBody("echo,em", marker, ""), nil)
				mu.Lock()
				results = append(results, outcome{i, true, r.status, r.body, marker})
				mu.Unlock()
			}()
			// One MALFORMED request.
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := post(t, si.gwURL("/v1/messages"), fmt.Sprintf(`{"model":"echo,em", broken-%d`, i), nil)
				mu.Lock()
				results = append(results, outcome{i, false, r.status, r.body, ""}) //nolint
				mu.Unlock()
			}()
		}
		wg.Wait()

		validSeen, malformedSeen := 0, 0
		for _, o := range results {
			if o.valid {
				validSeen++
				mustEqualInt(t, fmt.Sprintf("concurrent valid #%d status", o.idx), o.status, 200)
				// Its own marker present, and no OTHER request's marker leaked in.
				mustContain(t, fmt.Sprintf("concurrent valid #%d own marker", o.idx), o.body, o.expected)
				for _, other := range results {
					if other.valid && other.expected != o.expected {
						mustNotContain(t, fmt.Sprintf("valid #%d cross-contaminated by %q", o.idx, other.expected),
							o.body, other.expected)
					}
				}
			} else {
				malformedSeen++
				mustEqualInt(t, fmt.Sprintf("concurrent malformed #%d status", o.idx), o.status, 400)
				mustContain(t, fmt.Sprintf("concurrent malformed #%d error shape", o.idx), o.body, `"type":"error"`)
				mustNotContain(t, fmt.Sprintf("malformed #%d has no VALID content", o.idx), o.body, "VALID-")
			}
		}
		mustEqualInt(t, "concurrent valid count", validSeen, pairs)
		mustEqualInt(t, "concurrent malformed count", malformedSeen, pairs)
		assertServerUp(t, si)
	})

	// ---- No panic / goroutine dump escaped into the serve log ----
	t.Run("no_panic_after_adversarial_batch", func(t *testing.T) {
		if !si.alive() {
			t.Fatalf("gateway subprocess is not alive after the adversarial batch\n--- output ---\n%s", si.out.String())
		}
		log := si.out.String()
		for _, bad := range []string{"panic:", "runtime error", "goroutine ", "[Recovery]", "fatal error:"} {
			if strings.Contains(log, bad) {
				t.Fatalf("serve log contains %q after adversarial batch — a crash/panic leaked:\n%s", bad, log)
			}
		}
	})
}
