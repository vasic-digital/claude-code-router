// Package metrics is a self-contained, dependency-free metrics recorder for
// the Claude Code Router, implementing the Phase-2 "Prometheus metrics" layer
// described in docs/research/innovations/04-observability-and-metrics.md.
//
// It is deliberately standalone: it imports only the standard library and does
// NOT import internal/gateway, internal/router, internal/config, or
// internal/cache. The gateway and the management server will import THIS
// package (never the other way round), exactly as internal/cache is structured.
// That keeps the hot data-plane free of any metrics-vendor dependency and
// preserves the project's static-binary property — the Prometheus
// text-exposition format is simple enough to hand-render correctly (see
// exposition.go), so pulling in github.com/prometheus/client_golang would be
// pure cost here.
//
// # Metric catalogue
//
// The Recorder exposes the RED (Rate, Errors, Duration) HTTP triple plus the
// GenAI token/upstream/cache counters from the dossier's Data-definitions
// catalogue. Names follow the OpenTelemetry gen_ai.* / http.* semantic
// conventions where reasonable, under a stable ccr_ prefix:
//
//	ccr_http_requests_total{method,path,status}      counter   (Rate + Errors)
//	ccr_http_request_duration_seconds{method,path}   histogram (Duration)
//	ccr_http_inflight_requests                       gauge     (concurrency)
//	ccr_gen_ai_upstream_requests_total{provider,model}   counter
//	ccr_gen_ai_input_tokens_total{provider,model}        counter  gen_ai.usage.input_tokens
//	ccr_gen_ai_output_tokens_total{provider,model}       counter  gen_ai.usage.output_tokens
//	ccr_gen_ai_cache_lookups_total{tier,result}          counter  result=hit|miss
//
// All label sets are bounded and secret-free by construction: the caller is
// expected to pass a route *template* (not a raw URL with ids), a resolved
// provider *name* (never its API key), and a resolved model id — the same
// low-cardinality discipline the dossier's "Risks" section calls for.
//
// # Thread-safety and the hot path
//
// A single sync.Mutex guards the metric maps; every record call is O(1) under
// that lock (a map lookup + an integer add), and the in-flight gauge is a
// lock-free sync/atomic counter so the start/stop of a request never contends
// on the mutex. The Recorder is safe for concurrent use by many goroutines;
// see the -race tests.
//
// # Gateway / management seam (documented, NOT wired here)
//
// This package intentionally wires itself into nothing — like internal/cache
// and internal/gateway/logging_middleware.go, it is a ready-to-connect seam.
// Two one-liners, owned by files this package must not touch, complete it:
//
//	DATA PLANE (a future internal/gateway middleware, per request):
//	    rec.IncInFlight(); defer rec.DecInFlight()
//	    start := time.Now()
//	    c.Next()
//	    rec.RecordRequest(c.Request.Method, routeTemplate, c.Writer.Status(), time.Since(start))
//	    // token/upstream/cache calls fed from the anthropicUsage already parsed
//	    // in internal/gateway/messages.go and the internal/cache.Stats snapshot:
//	    rec.RecordUpstream(providerName, model)
//	    rec.RecordTokens(providerName, model, usage.InputTokens, usage.OutputTokens)
//	    rec.RecordCache("exact", hit)
//
//	CONTROL PLANE (cmd/ccr/management.go, alongside the existing /health, on the
//	loopback management server — off the hot path, deliberately un-compressed so
//	Prometheus scrapes stay plain text):
//	    mux.Handle("/metrics", rec.Handler())
package metrics

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// defaultBuckets are the upper bounds (seconds, inclusive) of the duration
// histogram. They match Prometheus' classic DefBuckets so downstream
// dashboards and recording rules that assume that layout work unchanged.
var defaultBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// counter is a labelled monotonic counter: one float value per label-set,
// keyed by the pre-rendered, escaped label string (e.g. `method="GET",path="/x"`).
type counter struct {
	name   string
	help   string
	values map[string]uint64
}

func newCounter(name, help string) *counter {
	return &counter{name: name, help: help, values: map[string]uint64{}}
}

func (c *counter) add(labelKey string, delta uint64) {
	c.values[labelKey] += delta
}

// histogramSeries is one label-set's histogram state. buckets[i] holds the
// count of observations whose value first fell at or below defaultBuckets[i]
// (non-cumulative); the exposition renderer accumulates them into the
// cumulative le buckets Prometheus expects.
type histogramSeries struct {
	buckets []uint64
	sum     float64
	count   uint64
}

// histogram is a labelled histogram over defaultBuckets.
type histogram struct {
	name   string
	help   string
	series map[string]*histogramSeries
}

func newHistogram(name, help string) *histogram {
	return &histogram{name: name, help: help, series: map[string]*histogramSeries{}}
}

func (h *histogram) observe(labelKey string, seconds float64) {
	s := h.series[labelKey]
	if s == nil {
		s = &histogramSeries{buckets: make([]uint64, len(defaultBuckets))}
		h.series[labelKey] = s
	}
	for i, b := range defaultBuckets {
		if seconds <= b {
			s.buckets[i]++
			break
		}
	}
	s.count++
	s.sum += seconds
}

// Recorder holds all metric families for one process. Construct it once with
// New and share the pointer; it is safe for concurrent use.
type Recorder struct {
	mu sync.Mutex

	httpRequests *counter // ccr_http_requests_total{method,path,status}
	httpDuration *histogram
	upstreamReqs *counter // ccr_gen_ai_upstream_requests_total{provider,model}
	inputTokens  *counter // ccr_gen_ai_input_tokens_total{provider,model}
	outputTokens *counter // ccr_gen_ai_output_tokens_total{provider,model}
	cacheLookups *counter // ccr_gen_ai_cache_lookups_total{tier,result}

	// inFlight is the ccr_http_inflight_requests gauge. It is a lock-free
	// atomic so the per-request Inc/Dec pair never contends on mu.
	inFlight atomic.Int64
}

// New returns a Recorder with every metric family initialised and zeroed.
func New() *Recorder {
	return &Recorder{
		httpRequests: newCounter("ccr_http_requests_total",
			"Total HTTP requests handled, by method, route template and status code."),
		httpDuration: newHistogram("ccr_http_request_duration_seconds",
			"HTTP request duration in seconds, by method and route template."),
		upstreamReqs: newCounter("ccr_gen_ai_upstream_requests_total",
			"Total upstream provider requests, by provider name and resolved model."),
		inputTokens: newCounter("ccr_gen_ai_input_tokens_total",
			"gen_ai.usage.input_tokens accumulated, by provider name and resolved model."),
		outputTokens: newCounter("ccr_gen_ai_output_tokens_total",
			"gen_ai.usage.output_tokens accumulated, by provider name and resolved model."),
		cacheLookups: newCounter("ccr_gen_ai_cache_lookups_total",
			"Response-cache lookups, by tier and result (hit|miss)."),
	}
}

// RecordRequest is the single per-request seam a gateway middleware calls once
// the handler chain has completed:
//
//	rec.RecordRequest(method, routeTemplate, status, time.Since(start))
//
// It bumps ccr_http_requests_total{method,path,status} and observes the
// ccr_http_request_duration_seconds{method,path} histogram. path must be a
// bounded route TEMPLATE (e.g. "/v1/messages"), never a raw URL carrying ids —
// that is what keeps label cardinality bounded and secret-free.
func (r *Recorder) RecordRequest(method, path string, status int, dur time.Duration) {
	reqKey := labelKey(
		label{"method", method},
		label{"path", path},
		label{"status", itoa(status)},
	)
	durKey := labelKey(
		label{"method", method},
		label{"path", path},
	)
	seconds := dur.Seconds()
	if seconds < 0 {
		seconds = 0
	}

	r.mu.Lock()
	r.httpRequests.add(reqKey, 1)
	r.httpDuration.observe(durKey, seconds)
	r.mu.Unlock()
}

// IncInFlight increments the ccr_http_inflight_requests gauge; a middleware
// calls it on entry and DecInFlight (typically deferred) on exit.
func (r *Recorder) IncInFlight() { r.inFlight.Add(1) }

// DecInFlight decrements the ccr_http_inflight_requests gauge.
func (r *Recorder) DecInFlight() { r.inFlight.Add(-1) }

// RecordUpstream counts one upstream request to provider for the resolved
// model (ccr_gen_ai_upstream_requests_total{provider,model}).
func (r *Recorder) RecordUpstream(provider, model string) {
	key := labelKey(label{"provider", provider}, label{"model", model})
	r.mu.Lock()
	r.upstreamReqs.add(key, 1)
	r.mu.Unlock()
}

// RecordTokens accumulates the input/output token usage of one generation,
// fed from the anthropicUsage the response encoders already parse. Negative
// counts are ignored (treated as zero) so a malformed usage block can never
// corrupt a monotonic counter.
func (r *Recorder) RecordTokens(provider, model string, input, output int) {
	key := labelKey(label{"provider", provider}, label{"model", model})
	r.mu.Lock()
	if input > 0 {
		r.inputTokens.add(key, uint64(input))
	}
	if output > 0 {
		r.outputTokens.add(key, uint64(output))
	}
	r.mu.Unlock()
}

// RecordCache counts one response-cache lookup at tier, hit==true for a hit and
// false for a miss (ccr_gen_ai_cache_lookups_total{tier,result}). It maps
// cleanly onto the internal/cache.Stats Hits/Misses counters.
func (r *Recorder) RecordCache(tier string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	key := labelKey(label{"tier", tier}, label{"result", result})
	r.mu.Lock()
	r.cacheLookups.add(key, 1)
	r.mu.Unlock()
}

// Handler returns an http.Handler that renders the current metric snapshot in
// Prometheus text-exposition format. Mount it on the CONTROL-plane management
// server (cmd/ccr/management.go), alongside /health:
//
//	mux.Handle("/metrics", rec.Handler())
//
// It is a plain GET handler with no compression, so Prometheus scrapes stay
// plain text on the loopback control plane, off the hot data path.
func (r *Recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteExposition(w)
	})
}
