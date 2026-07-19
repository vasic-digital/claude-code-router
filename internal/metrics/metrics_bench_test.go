package metrics

// metrics_bench_test.go holds throughput benchmarks for the two hot paths a
// deployment cares about: the per-request record call on the data plane, and
// the exposition render on the control plane's /metrics scrape.

import (
	"io"
	"testing"
	"time"
)

// BenchmarkRecordRequest measures the single per-request seam under a fixed
// low-cardinality label set (the realistic steady state).
func BenchmarkRecordRequest(b *testing.B) {
	r := New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.RecordRequest("POST", "/v1/messages", 200, 7*time.Millisecond)
	}
}

// BenchmarkRecordRequestParallel measures the same seam under contention, which
// is the property the mutex/atomic split is designed for.
func BenchmarkRecordRequestParallel(b *testing.B) {
	r := New()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.IncInFlight()
			r.RecordRequest("POST", "/v1/messages", 200, time.Duration(i%9)*time.Millisecond)
			r.RecordTokens("prov", "model", 3, 5)
			r.DecInFlight()
			i++
		}
	})
}

// BenchmarkWriteExposition measures a full scrape render over a moderately
// populated recorder (several families, multiple label-sets, a histogram),
// which is what Prometheus pays on every scrape interval.
func BenchmarkWriteExposition(b *testing.B) {
	r := New()
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	paths := []string{"/v1/messages", "/v1/models", "/health", "/a/b"}
	for _, m := range methods {
		for _, p := range paths {
			for _, s := range []int{200, 400, 500} {
				r.RecordRequest(m, p, s, 12*time.Millisecond)
			}
		}
	}
	for _, prov := range []string{"openai", "deepseek", "anthropic"} {
		r.RecordUpstream(prov, "m")
		r.RecordTokens(prov, "m", 1000, 2000)
	}
	r.RecordCache("exact", true)
	r.RecordCache("semantic", false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.WriteExposition(io.Discard)
	}
}
