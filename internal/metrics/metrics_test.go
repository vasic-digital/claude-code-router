package metrics

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// scrape renders the recorder to text-exposition form the way the Handler
// would, for assertions.
func scrape(r *Recorder) string {
	var b strings.Builder
	r.WriteExposition(&b)
	return b.String()
}

// findSeries returns the value of the exposition line whose full
// `name{labels}` (or bare `name`) prefix matches want, and whether it existed.
func findSeries(t *testing.T, out, want string) (string, bool) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		// A series line is "<name-and-labels> <value>".
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		if line[:sp] == want {
			return line[sp+1:], true
		}
	}
	return "", false
}

func TestRecordRequest_CountsAndLabels(t *testing.T) {
	r := New()
	for i := 0; i < 3; i++ {
		r.RecordRequest("GET", "/v1/messages", 200, 5*time.Millisecond)
	}
	r.RecordRequest("GET", "/v1/messages", 500, 5*time.Millisecond)
	r.RecordRequest("POST", "/v1/messages", 200, 5*time.Millisecond)

	out := scrape(r)

	cases := map[string]string{
		`ccr_http_requests_total{method="GET",path="/v1/messages",status="200"}`:  "3",
		`ccr_http_requests_total{method="GET",path="/v1/messages",status="500"}`:  "1",
		`ccr_http_requests_total{method="POST",path="/v1/messages",status="200"}`: "1",
	}
	for k, want := range cases {
		got, ok := findSeries(t, out, k)
		if !ok {
			t.Fatalf("series %q missing from:\n%s", k, out)
		}
		if got != want {
			t.Errorf("series %q = %s, want %s", k, got, want)
		}
	}
}

func TestRecordRequest_DurationHistogram(t *testing.T) {
	r := New()
	// Two fast (<=5ms → first bucket 0.005) and one slow (2s → 2.5 bucket).
	r.RecordRequest("GET", "/x", 200, 1*time.Millisecond)
	r.RecordRequest("GET", "/x", 200, 4*time.Millisecond)
	r.RecordRequest("GET", "/x", 200, 2*time.Second)

	out := scrape(r)

	// _count must be 3, +Inf bucket equals count, buckets are cumulative.
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_count{method="GET",path="/x"}`); !ok || v != "3" {
		t.Fatalf("_count = %q (ok=%v), want 3\n%s", v, ok, out)
	}
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/x",le="+Inf"}`); !ok || v != "3" {
		t.Errorf("+Inf bucket = %q (ok=%v), want 3", v, ok)
	}
	// le="0.005" should have captured the two sub-5ms observations.
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/x",le="0.005"}`); !ok || v != "2" {
		t.Errorf("le=0.005 bucket = %q (ok=%v), want 2", v, ok)
	}
	// le="1" is still cumulative 2 (the 2s obs not yet included).
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/x",le="1"}`); !ok || v != "2" {
		t.Errorf("le=1 bucket = %q (ok=%v), want 2", v, ok)
	}
	// le="2.5" includes all three.
	if v, ok := findSeries(t, out, `ccr_http_request_duration_seconds_bucket{method="GET",path="/x",le="2.5"}`); !ok || v != "3" {
		t.Errorf("le=2.5 bucket = %q (ok=%v), want 3", v, ok)
	}
}

func TestInFlightGauge(t *testing.T) {
	r := New()
	r.IncInFlight()
	r.IncInFlight()
	r.DecInFlight()
	out := scrape(r)
	if v, ok := findSeries(t, out, "ccr_http_inflight_requests"); !ok || v != "1" {
		t.Fatalf("inflight gauge = %q (ok=%v), want 1\n%s", v, ok, out)
	}
}

func TestTokenAndUpstreamCounters(t *testing.T) {
	r := New()
	r.RecordUpstream("deepseek", "deepseek-chat")
	r.RecordUpstream("deepseek", "deepseek-chat")
	r.RecordTokens("deepseek", "deepseek-chat", 1234, 567)
	r.RecordTokens("deepseek", "deepseek-chat", 6, 4)
	// Negative usage must be ignored, never corrupt the counter.
	r.RecordTokens("deepseek", "deepseek-chat", -100, -100)

	out := scrape(r)

	checks := map[string]string{
		`ccr_gen_ai_upstream_requests_total{provider="deepseek",model="deepseek-chat"}`: "2",
		`ccr_gen_ai_input_tokens_total{provider="deepseek",model="deepseek-chat"}`:      "1240",
		`ccr_gen_ai_output_tokens_total{provider="deepseek",model="deepseek-chat"}`:     "571",
	}
	for k, want := range checks {
		got, ok := findSeries(t, out, k)
		if !ok {
			t.Fatalf("series %q missing from:\n%s", k, out)
		}
		if got != want {
			t.Errorf("series %q = %s, want %s", k, got, want)
		}
	}
}

func TestCacheCounters(t *testing.T) {
	r := New()
	r.RecordCache("exact", true)
	r.RecordCache("exact", true)
	r.RecordCache("exact", false)

	out := scrape(r)
	if v, ok := findSeries(t, out, `ccr_gen_ai_cache_lookups_total{tier="exact",result="hit"}`); !ok || v != "2" {
		t.Errorf("cache hit = %q (ok=%v), want 2", v, ok)
	}
	if v, ok := findSeries(t, out, `ccr_gen_ai_cache_lookups_total{tier="exact",result="miss"}`); !ok || v != "1" {
		t.Errorf("cache miss = %q (ok=%v), want 1", v, ok)
	}
}

// TestLabelEscaping proves a label value containing a quote and a backslash is
// escaped correctly and round-trips, and that HELP/TYPE lines are present.
func TestLabelEscaping(t *testing.T) {
	r := New()
	// A hostile route template with a double-quote, a backslash and a newline.
	nasty := "/v1/\"quote\"\\back\nnewline"
	r.RecordRequest("GET", nasty, 200, time.Millisecond)

	out := scrape(r)

	wantLabel := `ccr_http_requests_total{method="GET",path="/v1/\"quote\"\\back\nnewline",status="200"}`
	if v, ok := findSeries(t, out, wantLabel); !ok || v != "1" {
		t.Fatalf("escaped series = %q (ok=%v), want 1\nfull output:\n%s", v, ok, out)
	}

	// HELP and TYPE lines must be present for the family.
	if !strings.Contains(out, "# HELP ccr_http_requests_total ") {
		t.Error("missing # HELP line for ccr_http_requests_total")
	}
	if !strings.Contains(out, "# TYPE ccr_http_requests_total counter") {
		t.Error("missing # TYPE line for ccr_http_requests_total")
	}
	// The raw (un-escaped) newline must NOT appear inside the value — escaping
	// must have turned it into a literal backslash-n.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ccr_http_requests_total{") && strings.Contains(line, "newline") {
			if !strings.Contains(line, `\n`) {
				t.Errorf("newline not escaped in line: %q", line)
			}
		}
	}
}

// TestExpositionIsParseable does a light structural parse of the whole output:
// every non-comment line must be "<name...> <value>" with a numeric value, and
// every family must carry HELP + TYPE.
func TestExpositionIsParseable(t *testing.T) {
	r := New()
	r.RecordRequest("GET", "/v1/messages", 200, 12*time.Millisecond)
	r.RecordUpstream("openai", "gpt-4o")
	r.RecordTokens("openai", "gpt-4o", 10, 20)
	r.RecordCache("exact", true)
	r.IncInFlight()

	out := scrape(r)

	typedFamilies := map[string]bool{}
	helpedFamilies := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			helpedFamilies[strings.Fields(line)[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			typedFamilies[strings.Fields(line)[2]] = true
		case strings.HasPrefix(line, "#"):
			// other comment — ignore
		default:
			sp := strings.LastIndexByte(line, ' ')
			if sp < 0 {
				t.Fatalf("malformed series line (no value): %q", line)
			}
			val := line[sp+1:]
			if _, err := strconv.ParseFloat(val, 64); err != nil {
				t.Fatalf("series value %q is not a float in line %q: %v", val, line, err)
			}
		}
	}

	for _, fam := range []string{
		"ccr_http_requests_total",
		"ccr_http_request_duration_seconds",
		"ccr_http_inflight_requests",
		"ccr_gen_ai_upstream_requests_total",
		"ccr_gen_ai_input_tokens_total",
		"ccr_gen_ai_output_tokens_total",
		"ccr_gen_ai_cache_lookups_total",
	} {
		if !typedFamilies[fam] {
			t.Errorf("family %q missing # TYPE", fam)
		}
		if !helpedFamilies[fam] {
			t.Errorf("family %q missing # HELP", fam)
		}
	}
}

func TestHandlerServesExposition(t *testing.T) {
	r := New()
	r.RecordRequest("GET", "/v1/messages", 200, time.Millisecond)

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain...", ct)
	}
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(buf.String(), `ccr_http_requests_total{method="GET",path="/v1/messages",status="200"} 1`) {
		t.Errorf("handler body missing expected series:\n%s", buf.String())
	}
}

// TestConcurrentRecording_RaceFreeAndExact fires many goroutines at every
// record path simultaneously; run under -race this proves thread-safety, and
// the totals must be exact.
func TestConcurrentRecording_RaceFreeAndExact(t *testing.T) {
	r := New()
	const goroutines = 50
	const perG = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				r.IncInFlight()
				r.RecordRequest("POST", "/v1/messages", 200, time.Millisecond)
				r.RecordUpstream("prov", "model")
				r.RecordTokens("prov", "model", 2, 3)
				r.RecordCache("exact", i%2 == 0)
				r.DecInFlight()
			}
		}()
	}
	// A concurrent scraper, to race the readers against the writers.
	var scWG sync.WaitGroup
	scWG.Add(1)
	go func() {
		defer scWG.Done()
		for i := 0; i < 100; i++ {
			_ = scrape(r)
		}
	}()
	wg.Wait()
	scWG.Wait()

	total := goroutines * perG
	out := scrape(r)

	exact := map[string]string{
		`ccr_http_requests_total{method="POST",path="/v1/messages",status="200"}`: strconv.Itoa(total),
		`ccr_gen_ai_upstream_requests_total{provider="prov",model="model"}`:       strconv.Itoa(total),
		`ccr_gen_ai_input_tokens_total{provider="prov",model="model"}`:            strconv.Itoa(total * 2),
		`ccr_gen_ai_output_tokens_total{provider="prov",model="model"}`:           strconv.Itoa(total * 3),
		`ccr_gen_ai_cache_lookups_total{tier="exact",result="hit"}`:               strconv.Itoa(total / 2),
		`ccr_gen_ai_cache_lookups_total{tier="exact",result="miss"}`:              strconv.Itoa(total / 2),
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

	// After every Inc has a matching Dec, the gauge must be exactly 0.
	if v, ok := findSeries(t, out, "ccr_http_inflight_requests"); !ok || v != "0" {
		t.Errorf("inflight gauge = %q (ok=%v), want 0", v, ok)
	}
}
