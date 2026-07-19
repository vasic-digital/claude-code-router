package gateway

// Proof that the metrics middleware is wired into the gateway: every request —
// including the unauthenticated liveness probe /health, which the middleware
// must never gate — increments ccr_http_requests_total, and the counter is
// visible through the SAME exposition the management /metrics endpoint serves
// (s.Metrics.Handler / WriteExposition). A nil Recorder leaves the path
// transparent.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vasic-digital/claude-code-router/internal/metrics"
)

// metricsExposition renders the recorder's current snapshot the way the
// management /metrics handler does, so assertions run against the real
// text-exposition output rather than internal counters.
func metricsExposition(t *testing.T, rec *metrics.Recorder) string {
	t.Helper()
	var buf bytes.Buffer
	rec.WriteExposition(&buf)
	return buf.String()
}

// A /health request is recorded (never gated) and shows up in the exposition
// with its route template and 200 status.
func TestMetricsMiddlewareRecordsHealth(t *testing.T) {
	s := testServerWithUpstream(t, "http://unused.invalid")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200 (the middleware must never gate liveness)", rec.Code)
	}

	exp := metricsExposition(t, s.Metrics)
	// The RED counter is present for /health at 200 via the route TEMPLATE.
	want := `ccr_http_requests_total{method="GET",path="/health",status="200"}`
	if !strings.Contains(exp, want) {
		t.Fatalf("exposition missing %q; got:\n%s", want, exp)
	}
	// The duration histogram is observed for the same template.
	if !strings.Contains(exp, `ccr_http_request_duration_seconds_count{method="GET",path="/health"}`) {
		t.Errorf("exposition missing duration histogram for /health; got:\n%s", exp)
	}
}

// A second /health request bumps the counter to 2 — the middleware records
// every request, not just the first.
func TestMetricsMiddlewareCountsEveryRequest(t *testing.T) {
	s := testServerWithUpstream(t, "http://unused.invalid")

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	exp := metricsExposition(t, s.Metrics)
	want := `ccr_http_requests_total{method="GET",path="/health",status="200"} 2`
	if !strings.Contains(exp, want) {
		t.Fatalf("exposition missing %q (expected two recorded requests); got:\n%s", want, exp)
	}
}

// An unmatched path collapses to the low-cardinality "/(unmatched)" bucket
// rather than leaking the raw URL as a label.
func TestMetricsMiddlewareUnmatchedPathBucket(t *testing.T) {
	s := testServerWithUpstream(t, "http://unused.invalid")

	req := httptest.NewRequest(http.MethodGet, "/no/such/route", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unmatched route status = %d, want 404", rec.Code)
	}
	exp := metricsExposition(t, s.Metrics)
	if !strings.Contains(exp, `path="/(unmatched)"`) {
		t.Errorf("exposition missing the /(unmatched) bucket; got:\n%s", exp)
	}
	if strings.Contains(exp, "/no/such/route") {
		t.Errorf("raw unmatched path leaked into a metric label; got:\n%s", exp)
	}
}

// A nil Recorder makes the middleware a transparent pass-through: the request
// still succeeds, nothing panics.
func TestMetricsMiddlewareNilRecorderTransparent(t *testing.T) {
	s := testServerWithUpstream(t, "http://unused.invalid")
	s.Metrics = nil

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/health status with nil Metrics = %d, want 200", rec.Code)
	}
}
