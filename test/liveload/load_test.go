package liveload

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Load parameters. Kept as named constants so the evidence is unambiguous.
const (
	loadN   = 500 // concurrency-correctness + cache-under-load request count
	loadW   = 32  // worker goroutines (the cache upstream-hit bound)
	streamM = 200 // concurrent streaming requests
	soakSec = 4   // sustained-soak duration in seconds

	// Fixed per-response usage of the canned non-streaming completion.
	usageIn  = 11 // prompt_tokens  -> Anthropic input_tokens
	usageOut = 7  // completion_tokens -> Anthropic output_tokens
)

// TestLiveLoadAndSoak builds the ccr binary, then drives CONCURRENT and
// SUSTAINED real HTTP through `ccr serve` subprocesses against a fake upstream,
// proving the gateway is correct under load AND that the Recorder's counters add
// up EXACTLY. Each scenario is a subtest so a failure is localized. This test is
// slower (~5-15s); run it alone with:
//
//	go test ./test/liveload/... -run TestLiveLoadAndSoak -count=1 -v
func TestLiveLoadAndSoak(t *testing.T) {
	requireBinary(t)

	// ---- Scenario 1: CONCURRENCY CORRECTNESS ----
	// Fire N POST /v1/messages across W goroutines. Every response must be a
	// well-formed Anthropic message (200); the moved counters must be EXACT:
	//   ccr_http_requests_total{POST,/v1/messages,200}      == N
	//   sum ccr_gen_ai_upstream_requests_total              == N
	//   ccr_gen_ai_input_tokens_total{main,main-model}      == N * usageIn
	//   ccr_gen_ai_output_tokens_total{main,main-model}     == N * usageOut
	t.Run("concurrency_correctness", func(t *testing.T) {
		fake := newFakeUpstream(t)
		fake.handle("main", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-load", "Hello from the upstream.", usageIn, usageOut)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"main,main-model"}}`,
			providerJSON("main", fake.url("main"), "sk-main-key", "main-model"))
		si := startServe(t, cfg)

		before := scrapeMetrics(t, si)

		results := runConcurrent(loadN, loadW, func(i int) (httpResult, error) {
			return rawPost(si.gwURL("/v1/messages"),
				anthropicBody("claude-sonnet-live", fmt.Sprintf("hello there #%d", i), ""), nil)
		})

		// Every single response must be a well-formed 200 Anthropic message.
		ok := 0
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("request #%d errored under load: %v\n--- serve output ---\n%s",
					r.idx, r.err, si.out.String())
			}
			if r.res.status != http.StatusOK {
				t.Fatalf("request #%d: status %d (want 200), body:\n%s\n--- serve output ---\n%s",
					r.idx, r.res.status, r.res.body, si.out.String())
			}
			for _, need := range []string{`"type":"message"`, `"role":"assistant"`,
				"Hello from the upstream.", `"stop_reason":"end_turn"`,
				fmt.Sprintf(`"input_tokens":%d`, usageIn), fmt.Sprintf(`"output_tokens":%d`, usageOut)} {
				if !strings.Contains(r.res.body, need) {
					t.Fatalf("request #%d: response missing %q, body:\n%s", r.idx, need, r.res.body)
				}
			}
			ok++
		}
		mustEqualInt(t, "well-formed 200 Anthropic responses", ok, loadN)
		assertNoPanic(t, si)

		after := scrapeMetrics(t, si)

		// ccr_http_requests_total{POST,/v1/messages,200} == N EXACTLY.
		reqDelta := metricValue(after, "ccr_http_requests_total",
			map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"}) -
			metricValue(before, "ccr_http_requests_total",
				map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"})
		mustEqualFloat(t, "ccr_http_requests_total{POST,/v1/messages,200} == N", reqDelta, float64(loadN))

		// sum of ccr_gen_ai_upstream_requests_total == N (single provider here).
		upDelta := metricValue(after, "ccr_gen_ai_upstream_requests_total",
			map[string]string{"provider": "main", "model": "main-model"}) -
			metricValue(before, "ccr_gen_ai_upstream_requests_total",
				map[string]string{"provider": "main", "model": "main-model"})
		mustEqualFloat(t, "sum ccr_gen_ai_upstream_requests_total == N", upDelta, float64(loadN))
		// Cross-check against the physical fake-upstream hit count.
		mustEqualInt(t, "fake upstream hits == N", fake.count("main"), loadN)

		// token counters == N * per-response usage.
		inDelta := metricValue(after, "ccr_gen_ai_input_tokens_total",
			map[string]string{"provider": "main", "model": "main-model"}) -
			metricValue(before, "ccr_gen_ai_input_tokens_total",
				map[string]string{"provider": "main", "model": "main-model"})
		mustEqualFloat(t, "ccr_gen_ai_input_tokens_total == N*usageIn", inDelta, float64(loadN*usageIn))

		outDelta := metricValue(after, "ccr_gen_ai_output_tokens_total",
			map[string]string{"provider": "main", "model": "main-model"}) -
			metricValue(before, "ccr_gen_ai_output_tokens_total",
				map[string]string{"provider": "main", "model": "main-model"})
		mustEqualFloat(t, "ccr_gen_ai_output_tokens_total == N*usageOut", outDelta, float64(loadN*usageOut))

		t.Logf("concurrency_correctness: N=%d W=%d — %d/%d well-formed 200s; requests=%v upstream=%v in=%v out=%v",
			loadN, loadW, ok, loadN, reqDelta, upDelta, inDelta, outDelta)
	})

	// ---- Scenario 2: IN-FLIGHT GAUGE quiesces to 0 ----
	// After the load quiesces, ccr_http_inflight_requests == 0 (firm). A
	// best-effort concurrent scrape during load is expected to observe > 0.
	t.Run("inflight_gauge_quiesces_to_zero", func(t *testing.T) {
		fake := newFakeUpstream(t)
		fake.handle("main", func(w http.ResponseWriter, r *http.Request) {
			// A tiny delay widens the mid-load window so the gauge is observably >0.
			time.Sleep(2 * time.Millisecond)
			writeOpenAICompletion(w, "chatcmpl-inflight", "in flight ok", usageIn, usageOut)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"main,main-model"}}`,
			providerJSON("main", fake.url("main"), "sk-main-key", "main-model"))
		si := startServe(t, cfg)

		// Best-effort: sample the gauge while the load runs.
		var maxInflight int64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if m, err := rawGet(si.mgmtURL("/metrics")); err == nil {
					v := int64(metricValue(m.body, "ccr_http_inflight_requests", nil))
					if v > atomic.LoadInt64(&maxInflight) {
						atomic.StoreInt64(&maxInflight, v)
					}
				}
				time.Sleep(time.Millisecond) // throttle: sample, don't busy-spin
			}
		}()

		results := runConcurrent(loadN, loadW, func(i int) (httpResult, error) {
			return rawPost(si.gwURL("/v1/messages"),
				anthropicBody("claude-sonnet-live", fmt.Sprintf("inflight #%d", i), ""), nil)
		})
		close(stop)
		<-done

		for _, r := range results {
			if r.err != nil || r.res.status != http.StatusOK {
				t.Fatalf("request #%d failed under load: err=%v status=%d\n--- serve output ---\n%s",
					r.idx, r.err, r.res.status, si.out.String())
			}
		}
		assertNoPanic(t, si)

		// Best-effort mid-load observation (never fails the test).
		if mi := atomic.LoadInt64(&maxInflight); mi > 0 {
			t.Logf("mid-load ccr_http_inflight_requests peaked at %d (best-effort)", mi)
		} else {
			t.Logf("mid-load ccr_http_inflight_requests peak not captured (timing) — best-effort only")
		}

		// FIRM: once quiesced the gauge must be exactly 0 (no leaked in-flight).
		// Retry briefly to let the final DecInFlight defers settle.
		var gauge float64
		for i := 0; i < 20; i++ {
			gauge = metricValue(scrapeMetrics(t, si), "ccr_http_inflight_requests", nil)
			if gauge == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		mustEqualFloat(t, "ccr_http_inflight_requests quiesced == 0", gauge, 0)
		t.Logf("inflight_gauge_quiesces_to_zero: N=%d W=%d — quiesced gauge=0", loadN, loadW)
	})

	// ---- Scenario 3: CACHE UNDER LOAD ----
	// Fire N IDENTICAL cacheable requests concurrently. The fake upstream must be
	// hit a SMALL number of times (<= W: a race between the first misses is
	// acceptable), ccr_gen_ai_cache_lookups_total{result="hit"} must be large,
	// and hits+misses must add up to N exactly (misses == upstream hits).
	t.Run("cache_under_load", func(t *testing.T) {
		fake := newFakeUpstream(t)
		fake.handle("cachemain", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-cacheload", "cached answer", 5, 3)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"cachemain,cm"},"Cache":{"enabled":true,"backend":"memory"}}`,
			providerJSON("cachemain", fake.url("cachemain"), "sk-cache", "cm"))
		si := startServe(t, cfg)

		before := scrapeMetrics(t, si)

		// IDENTICAL body every time (no index) so all N requests are one cache key.
		body := anthropicBody("cache-model-live", "what is the capital of France?", "")
		results := runConcurrent(loadN, loadW, func(i int) (httpResult, error) {
			return rawPost(si.gwURL("/v1/messages"), body, nil)
		})
		for _, r := range results {
			if r.err != nil || r.res.status != http.StatusOK || !strings.Contains(r.res.body, "cached answer") {
				t.Fatalf("cache request #%d failed under load: err=%v status=%d body=%s\n--- serve output ---\n%s",
					r.idx, r.err, r.res.status, r.res.body, si.out.String())
			}
		}
		assertNoPanic(t, si)

		upstreamHits := fake.count("cachemain")
		if upstreamHits > loadW {
			t.Fatalf("cache under load: upstream hit %d times, want <= W (%d) — cache is over-calling upstream",
				upstreamHits, loadW)
		}
		if upstreamHits < 1 {
			t.Fatalf("cache under load: upstream hit 0 times — nothing was actually fetched")
		}

		after := scrapeMetrics(t, si)
		hits := metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"}) -
			metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "hit"})
		misses := metricValue(after, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "miss"}) -
			metricValue(before, "ccr_gen_ai_cache_lookups_total", map[string]string{"tier": "exact", "result": "miss"})

		// hits must be large (the overwhelming majority served from cache).
		if hits < float64(loadN-loadW) {
			t.Fatalf("cache under load: hit lookups = %v, want >= N-W (%d) — cache not serving under load",
				hits, loadN-loadW)
		}
		// Exact bookkeeping: every request did one lookup, and every miss is one
		// upstream call, so hits+misses == N and misses == the physical hit count.
		mustEqualFloat(t, "cache lookups hits+misses == N", hits+misses, float64(loadN))
		mustEqualFloat(t, "cache misses == physical upstream hits", misses, float64(upstreamHits))

		t.Logf("cache_under_load: N=%d W=%d — upstream_hits=%d (<= W) cache_hits=%v cache_misses=%v (hits+misses=N=%d)",
			loadN, loadW, upstreamHits, hits, misses, loadN)
	})

	// ---- Scenario 4: STREAMING UNDER LOAD ----
	// Fire M concurrent stream:true requests; each must yield a complete Anthropic
	// SSE (message_start..message_stop) with the expected text; then the in-flight
	// gauge returns to 0.
	t.Run("streaming_under_load", func(t *testing.T) {
		fake := newFakeUpstream(t)
		fake.handle("smain", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAISSE(w, "chatcmpl-stream-load", []string{"Hello", ", world", "!"}, 13, 5)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"smain,sm"}}`,
			providerJSON("smain", fake.url("smain"), "sk-stream", "sm"))
		si := startServe(t, cfg)

		results := runConcurrent(streamM, loadW, func(i int) (httpResult, error) {
			return rawPost(si.gwURL("/v1/messages"),
				anthropicBody("claude-sonnet-live", fmt.Sprintf("stream please #%d", i), `,"stream":true`), nil)
		})

		ok := 0
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("stream request #%d errored under load: %v\n--- serve output ---\n%s",
					r.idx, r.err, si.out.String())
			}
			if r.res.status != http.StatusOK || !strings.Contains(r.res.contentType, "text/event-stream") {
				t.Fatalf("stream request #%d: status %d ct %q, body:\n%s",
					r.idx, r.res.status, r.res.contentType, r.res.body)
			}
			events, text := parseAnthropicSSE(r.res.body)
			for _, ev := range []string{"message_start", "content_block_delta", "message_stop"} {
				if !hasEvent(events, ev) {
					t.Fatalf("stream request #%d: missing %s event; events=%v body:\n%s",
						r.idx, ev, events, r.res.body)
				}
			}
			if text != "Hello, world!" {
				t.Fatalf("stream request #%d: concatenated text = %q, want %q", r.idx, text, "Hello, world!")
			}
			ok++
		}
		mustEqualInt(t, "complete Anthropic SSE streams", ok, streamM)
		mustEqualInt(t, "fake upstream stream hits == M", fake.count("smain"), streamM)
		assertNoPanic(t, si)

		// In-flight gauge returns to 0.
		var gauge float64
		for i := 0; i < 20; i++ {
			gauge = metricValue(scrapeMetrics(t, si), "ccr_http_inflight_requests", nil)
			if gauge == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		mustEqualFloat(t, "ccr_http_inflight_requests quiesced == 0 (after streaming)", gauge, 0)
		t.Logf("streaming_under_load: M=%d W=%d — %d/%d complete SSE streams; quiesced gauge=0",
			streamM, loadW, ok, streamM)
	})

	// ---- Scenario 5: SUSTAINED SOAK ----
	// A short BOUNDED loop (deadline via time.After, no hanging sleep) of steady
	// requests; assert zero errors and no panic / goroutine-dump in the serve log.
	t.Run("sustained_soak", func(t *testing.T) {
		fake := newFakeUpstream(t)
		fake.handle("soak", func(w http.ResponseWriter, r *http.Request) {
			writeOpenAICompletion(w, "chatcmpl-soak", "soak ok", usageIn, usageOut)
		})
		cfg := fmt.Sprintf(`{"Providers":[%s],"Router":{"default":"soak,sm"}}`,
			providerJSON("soak", fake.url("soak"), "sk-soak", "sm"))
		si := startServe(t, cfg)

		before := scrapeMetrics(t, si)

		const workers = 16
		// A firm wall-clock deadline (NOT an open sleep): each worker loops until
		// time.Now() passes end, so every worker exits independently and the loop
		// is strictly bounded to soakSec seconds.
		end := time.Now().Add(time.Duration(soakSec) * time.Second)
		var sent, okCount int64
		var firstErr atomic.Value // error

		var wg sync.WaitGroup
		for g := 0; g < workers; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(end) {
					n := atomic.AddInt64(&sent, 1)
					res, err := rawPost(si.gwURL("/v1/messages"),
						anthropicBody("claude-sonnet-live", fmt.Sprintf("soak #%d", n), ""), nil)
					if err != nil {
						firstErr.CompareAndSwap(nil, fmt.Errorf("request #%d: %w", n, err))
						continue
					}
					if res.status != http.StatusOK || !strings.Contains(res.body, "soak ok") {
						firstErr.CompareAndSwap(nil, fmt.Errorf("request #%d: status %d body %s", n, res.status, res.body))
						continue
					}
					atomic.AddInt64(&okCount, 1)
				}
			}()
		}
		wg.Wait()

		if e := firstErr.Load(); e != nil {
			t.Fatalf("soak saw a failed request: %v\n--- serve output ---\n%s", e.(error), si.out.String())
		}
		sentN := atomic.LoadInt64(&sent)
		okN := atomic.LoadInt64(&okCount)
		if okN < 1 {
			t.Fatalf("soak completed zero successful requests (sent=%d)", sentN)
		}
		assertNoPanic(t, si)

		// The moved counter must account for every OK request (delta >= okN; the
		// last few in-flight at the deadline may or may not be scraped yet, so we
		// assert the counter is at least the confirmed-OK count).
		after := scrapeMetrics(t, si)
		got := metricValue(after, "ccr_http_requests_total",
			map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"}) -
			metricValue(before, "ccr_http_requests_total",
				map[string]string{"method": "POST", "path": "/v1/messages", "status": "200"})
		if got < float64(okN) {
			t.Fatalf("soak: ccr_http_requests_total delta %v < confirmed OK responses %d", got, okN)
		}
		t.Logf("sustained_soak: %ds, %d workers — sent=%d ok=%d, zero errors, no panic; requests_total delta=%v",
			soakSec, workers, sentN, okN, got)
	})
}

// ---------- small assertion + SSE helpers ----------

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

// assertNoPanic fails if the serve subprocess log shows a panic or goroutine dump.
func assertNoPanic(t *testing.T, si *serveInstance) {
	t.Helper()
	logs := si.out.String()
	for _, marker := range []string{"panic:", "goroutine ", "runtime error:", "fatal error:"} {
		if strings.Contains(logs, marker) {
			t.Fatalf("serve log contains %q — the gateway crashed/dumped under load:\n%s", marker, logs)
		}
	}
}

// parseAnthropicSSE collects event names and concatenates every text_delta text
// from an Anthropic Messages SSE stream.
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
