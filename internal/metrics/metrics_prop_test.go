package metrics

// metrics_prop_test.go holds seeded property tests and a snapshot-consistency
// concurrency test. Properties are checked against a reference model computed
// independently of the recorder, so a bug in the recorder cannot hide behind
// the same bug in the assertion.

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProp_RequestCountsMatchModel records a seeded random stream of requests
// and asserts every ccr_http_requests_total series and every histogram _count
// equals an independently-maintained tally. Deterministic via a fixed seed.
func TestProp_RequestCountsMatchModel(t *testing.T) {
	const seed = 0xC0FFEE
	rng := rand.New(rand.NewSource(seed))

	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	paths := []string{"/v1/messages", "/v1/models", "/health", "/a/b", "/"}
	statuses := []int{200, 201, 204, 400, 404, 429, 500, 503}
	durs := []time.Duration{
		1 * time.Millisecond, 7 * time.Millisecond, 40 * time.Millisecond,
		300 * time.Millisecond, 900 * time.Millisecond, 4 * time.Second, 20 * time.Second,
	}

	r := New()
	reqModel := map[string]int{} // method|path|status -> count
	durModel := map[string]int{} // method|path -> observation count

	const N = 8000
	for i := 0; i < N; i++ {
		m := methods[rng.Intn(len(methods))]
		p := paths[rng.Intn(len(paths))]
		s := statuses[rng.Intn(len(statuses))]
		d := durs[rng.Intn(len(durs))]

		r.RecordRequest(m, p, s, d)
		reqModel[m+"|"+p+"|"+strconv.Itoa(s)]++
		durModel[m+"|"+p]++
	}

	out := scrape(r)
	validateExposition(t, out)

	for k, want := range reqModel {
		parts := splitPipe(k)
		m, p, s := parts[0], parts[1], parts[2]
		key := fmt.Sprintf(`ccr_http_requests_total{method=%q,path=%q,status=%q}`, m, p, s)
		got, ok := findSeries(t, out, key)
		if !ok {
			t.Fatalf("missing counter series %s", key)
		}
		if got != strconv.Itoa(want) {
			t.Errorf("%s = %s, want %d", key, got, want)
		}
	}

	for k, want := range durModel {
		parts := splitPipe(k)
		key := fmt.Sprintf(`ccr_http_request_duration_seconds_count{method=%q,path=%q}`, parts[0], parts[1])
		got, ok := findSeries(t, out, key)
		if !ok {
			t.Fatalf("missing histogram _count %s", key)
		}
		if got != strconv.Itoa(want) {
			t.Errorf("%s = %s, want %d", key, got, want)
		}
	}
}

// splitPipe splits on '|' without importing strings for one call site.
func splitPipe(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// TestProp_TokenAccumulationIgnoresNegatives records a seeded stream of token
// pairs (some negative) and asserts the counters equal the sum of the strictly
// positive components only — negatives must never underflow the uint64 counter.
func TestProp_TokenAccumulationIgnoresNegatives(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	r := New()

	var wantIn, wantOut uint64
	const N = 5000
	for i := 0; i < N; i++ {
		in := rng.Intn(4001) - 1000 // [-1000, 3000]
		out := rng.Intn(4001) - 1000
		r.RecordTokens("prov", "model", in, out)
		if in > 0 {
			wantIn += uint64(in)
		}
		if out > 0 {
			wantOut += uint64(out)
		}
	}

	text := scrape(r)
	validateExposition(t, text)

	if v, ok := findSeries(t, text, `ccr_gen_ai_input_tokens_total{provider="prov",model="model"}`); !ok || v != strconv.FormatUint(wantIn, 10) {
		t.Errorf("input tokens = %q (ok=%v), want %d", v, ok, wantIn)
	}
	if v, ok := findSeries(t, text, `ccr_gen_ai_output_tokens_total{provider="prov",model="model"}`); !ok || v != strconv.FormatUint(wantOut, 10) {
		t.Errorf("output tokens = %q (ok=%v), want %d", v, ok, wantOut)
	}
}

// TestProp_CacheHitMissPartition asserts, over a seeded stream across several
// tiers, that hit+miss per tier equals the total lookups for that tier and each
// matches the model exactly.
func TestProp_CacheHitMissPartition(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	tiers := []string{"exact", "semantic", "prefix"}
	r := New()

	hits := map[string]int{}
	miss := map[string]int{}
	const N = 6000
	for i := 0; i < N; i++ {
		tier := tiers[rng.Intn(len(tiers))]
		hit := rng.Intn(2) == 0
		r.RecordCache(tier, hit)
		if hit {
			hits[tier]++
		} else {
			miss[tier]++
		}
	}

	out := scrape(r)
	validateExposition(t, out)

	for _, tier := range tiers {
		hk := fmt.Sprintf(`ccr_gen_ai_cache_lookups_total{tier=%q,result="hit"}`, tier)
		mk := fmt.Sprintf(`ccr_gen_ai_cache_lookups_total{tier=%q,result="miss"}`, tier)
		hv, _ := findSeries(t, out, hk)
		mv, _ := findSeries(t, out, mk)
		if hv != strconv.Itoa(hits[tier]) {
			t.Errorf("%s = %q, want %d", hk, hv, hits[tier])
		}
		if mv != strconv.Itoa(miss[tier]) {
			t.Errorf("%s = %q, want %d", mk, mv, miss[tier])
		}
	}
}

// TestProp_InFlightGaugeNeverNegative drives a seeded random walk of Inc/Dec
// that never dips below zero in the model, sampling the exposed gauge after
// each step. The exposed value must equal the model and never be negative.
func TestProp_InFlightGaugeNeverNegative(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	r := New()

	model := 0
	const N = 3000
	for i := 0; i < N; i++ {
		if model == 0 || rng.Intn(2) == 0 {
			r.IncInFlight()
			model++
		} else {
			r.DecInFlight()
			model--
		}
		// Sample occasionally (scraping every step would be needlessly slow).
		if i%37 == 0 {
			out := scrape(r)
			v, ok := findSeries(t, out, "ccr_http_inflight_requests")
			if !ok {
				t.Fatalf("gauge missing at step %d", i)
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("gauge value %q not an int: %v", v, err)
			}
			if n < 0 {
				t.Fatalf("gauge went negative (%d) at step %d", n, i)
			}
			if n != model {
				t.Fatalf("gauge = %d, model = %d at step %d", n, model, i)
			}
		}
	}

	// Drain to zero and confirm.
	for model > 0 {
		r.DecInFlight()
		model--
	}
	if v, ok := findSeries(t, scrape(r), "ccr_http_inflight_requests"); !ok || v != "0" {
		t.Errorf("drained gauge = %q (ok=%v), want 0", v, ok)
	}
}

// TestConcurrent_SnapshotIsConsistent runs writers on every record path while a
// scraper concurrently renders full snapshots and validates each one as a
// well-formed exposition document. A torn write would surface as a grammar or
// histogram-invariant failure inside validateExposition. Under -race this also
// proves the mutex/atomic discipline is sound. Totals are checked exactly after
// the writers quiesce.
func TestConcurrent_SnapshotIsConsistent(t *testing.T) {
	r := New()
	const writers = 24
	const perW = 500

	var stop atomic.Bool
	var scrapeWG sync.WaitGroup
	scrapeWG.Add(1)
	var snapshots int
	go func() {
		defer scrapeWG.Done()
		for !stop.Load() {
			out := scrape(r)
			// Validate the live snapshot: every line well-formed, histogram
			// invariants intact — i.e. no torn line and no partial write.
			validateExposition(t, out)
			snapshots++
		}
	}()

	var wg sync.WaitGroup
	wg.Add(writers)
	for wkr := 0; wkr < writers; wkr++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perW; i++ {
				r.IncInFlight()
				r.RecordRequest("POST", "/v1/messages", 200, time.Duration(i%9)*time.Millisecond)
				r.RecordUpstream("prov", "model")
				r.RecordTokens("prov", "model", 3, 5)
				r.RecordCache("exact", (id+i)%2 == 0)
				r.DecInFlight()
			}
		}(wkr)
	}
	wg.Wait()
	stop.Store(true)
	scrapeWG.Wait()

	if snapshots == 0 {
		t.Fatal("scraper never captured a snapshot")
	}

	out := scrape(r)
	validateExposition(t, out)

	total := writers * perW
	exact := map[string]string{
		`ccr_http_requests_total{method="POST",path="/v1/messages",status="200"}`:    strconv.Itoa(total),
		`ccr_http_request_duration_seconds_count{method="POST",path="/v1/messages"}`: strconv.Itoa(total),
		`ccr_gen_ai_upstream_requests_total{provider="prov",model="model"}`:          strconv.Itoa(total),
		`ccr_gen_ai_input_tokens_total{provider="prov",model="model"}`:               strconv.Itoa(total * 3),
		`ccr_gen_ai_output_tokens_total{provider="prov",model="model"}`:              strconv.Itoa(total * 5),
	}
	for k, want := range exact {
		got, ok := findSeries(t, out, k)
		if !ok {
			t.Fatalf("series %q missing", k)
		}
		if got != want {
			t.Errorf("series %q = %s, want %s", k, got, want)
		}
	}
	if v, ok := findSeries(t, out, "ccr_http_inflight_requests"); !ok || v != "0" {
		t.Errorf("inflight gauge = %q (ok=%v), want 0 after balanced Inc/Dec", v, ok)
	}
}
