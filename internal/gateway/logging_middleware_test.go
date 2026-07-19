package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/vasic-digital/claude-code-router/internal/logging"
)

// testLoggingEngine builds a minimal gin engine with LoggingMiddleware
// installed and a handler under /x that a test can shape via handle. Log
// output goes to buf so the test can inspect exactly what was written.
func testLoggingEngine(buf *bytes.Buffer, handle gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	logger := logging.NewWithOptions(buf, slog.LevelInfo, logging.FormatJSON)

	r := gin.New()
	r.Use(LoggingMiddleware(logger))
	r.Any("/x", handle)
	return r
}

// logLines splits buf's content into non-empty JSON-log lines.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	raw := strings.TrimRight(buf.String(), "\n")
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, l)
		}
		out = append(out, m)
	}
	return out
}

// One line per request, carrying method/path/status/duration/bytes/request_id.
func TestLoggingMiddlewareEmitsOneLineWithExpectedFields(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) {
		c.String(http.StatusCreated, "hello world")
	})

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly 1: %v", len(lines), lines)
	}
	line := lines[0]

	if line["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", line["method"])
	}
	if line["path"] != "/x" {
		t.Errorf("path = %v, want /x", line["path"])
	}
	if line["status"] != float64(http.StatusCreated) {
		t.Errorf("status = %v, want 201", line["status"])
	}
	if dur, ok := line["duration_ms"].(float64); !ok || dur < 0 {
		t.Errorf("duration_ms = %v, want a non-negative number", line["duration_ms"])
	}
	if bytesWritten, ok := line["bytes"].(float64); !ok || bytesWritten != float64(len("hello world")) {
		t.Errorf("bytes = %v, want %d", line["bytes"], len("hello world"))
	}
	reqID, ok := line["request_id"].(string)
	if !ok || reqID == "" {
		t.Errorf("request_id = %v, want a non-empty string", line["request_id"])
	}
	if rec.Header().Get(requestIDHeader) != reqID {
		t.Errorf("response header %s = %q, want it to match the logged request_id %q",
			requestIDHeader, rec.Header().Get(requestIDHeader), reqID)
	}
}

// An inbound X-Request-Id must be honoured verbatim, not overwritten by a
// freshly generated one.
func TestLoggingMiddlewareHonoursInboundRequestID(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestIDHeader, "caller-supplied-id-123")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get(requestIDHeader); got != "caller-supplied-id-123" {
		t.Errorf("response %s = %q, want the caller-supplied id echoed back", requestIDHeader, got)
	}

	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1", len(lines))
	}
	if lines[0]["request_id"] != "caller-supplied-id-123" {
		t.Errorf("logged request_id = %v, want the caller-supplied id", lines[0]["request_id"])
	}
}

// Header lookups must be case-insensitive, per HTTP convention (and per
// gin.Context.GetHeader's own canonicalisation).
func TestLoggingMiddlewareHonoursInboundRequestIDCaseInsensitively(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("x-request-id", "lowercase-header-id") // net/http canonicalises for us anyway
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	lines := logLines(t, &buf)
	if len(lines) != 1 || lines[0]["request_id"] != "lowercase-header-id" {
		t.Errorf("request_id = %v, want lowercase-header-id", lines[0]["request_id"])
	}
}

// No inbound X-Request-Id -> a fresh one is generated, non-empty, and
// distinct across separate requests (proving it is actually randomised,
// not a constant placeholder).
func TestLoggingMiddlewareGeneratesRequestIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) { c.Status(http.StatusOK) })

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/x", nil))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/x", nil))

	id1 := rec1.Header().Get(requestIDHeader)
	id2 := rec2.Header().Get(requestIDHeader)
	if id1 == "" || id2 == "" {
		t.Fatalf("generated request ids must be non-empty: %q, %q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("two separate requests generated the SAME request id %q — not actually randomised", id1)
	}

	lines := logLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}
	if lines[0]["request_id"] != id1 || lines[1]["request_id"] != id2 {
		t.Errorf("logged request ids %v/%v do not match response headers %v/%v",
			lines[0]["request_id"], lines[1]["request_id"], id1, id2)
	}
}

// The Authorization and X-Api-Key header VALUES must never appear anywhere
// in the log output, under any field.
func TestLoggingMiddlewareNeverLogsAuthHeaderValues(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) { c.Status(http.StatusOK) })

	const bearerSecret = "Bearer sk-super-secret-bearer-value-0123456789"
	const apiKeySecret = "super-secret-x-api-key-value-abcdefghijklmno"

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", bearerSecret)
	req.Header.Set("x-api-key", apiKeySecret)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	out := buf.String()
	if strings.Contains(out, bearerSecret) || strings.Contains(out, "sk-super-secret-bearer-value-0123456789") {
		t.Fatalf("Authorization header value leaked into log output: %s", out)
	}
	if strings.Contains(out, apiKeySecret) {
		t.Fatalf("x-api-key header value leaked into log output: %s", out)
	}
}

// Request and response BODIES must never appear in log output.
func TestLoggingMiddlewareNeverLogsBodies(t *testing.T) {
	var buf bytes.Buffer
	const requestMarker = "PROMPT_MARKER_f7a2c9"
	const responseMarker = "COMPLETION_MARKER_b81e04"

	r := testLoggingEngine(&buf, func(c *gin.Context) {
		body, _ := c.GetRawData()
		if !strings.Contains(string(body), requestMarker) {
			t.Errorf("handler did not receive the expected request marker; got %q", body)
		}
		c.String(http.StatusOK, responseMarker)
	})

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(requestMarker))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), responseMarker) {
		t.Fatalf("test setup broken: response body missing its own marker: %s", rec.Body.String())
	}

	out := buf.String()
	if strings.Contains(out, requestMarker) {
		t.Fatalf("request body content leaked into log output: %s", out)
	}
	if strings.Contains(out, responseMarker) {
		t.Fatalf("response body content leaked into log output: %s", out)
	}
}

// Exactly one line per request, even across several requests in sequence —
// guards against accidental double-logging (e.g. a middleware registered
// twice) or under-logging (a path where c.Next() short-circuits).
func TestLoggingMiddlewareExactlyOneLinePerRequestAcrossMultipleRequests(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) { c.Status(http.StatusNoContent) })

	const n = 5
	for i := 0; i < n; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	lines := logLines(t, &buf)
	if len(lines) != n {
		t.Fatalf("got %d log lines for %d requests, want exactly %d", len(lines), n, n)
	}
}

// A nil logger must fall back to a working default rather than panicking —
// this middleware must be safe to install before any caller has its own
// *slog.Logger wired up.
func TestLoggingMiddlewareNilLoggerFallsBackSafely(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(LoggingMiddleware(nil))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil logger must not break request handling)", rec.Code)
	}
	if rec.Header().Get(requestIDHeader) == "" {
		t.Errorf("request id header missing even with a nil logger")
	}
}

// A handler that errors/aborts without writing a body must still produce
// exactly one clean log line, with a sane (non-negative) byte count.
func TestLoggingMiddlewareHandlesZeroByteResponses(t *testing.T) {
	var buf bytes.Buffer
	r := testLoggingEngine(&buf, func(c *gin.Context) {
		c.AbortWithStatus(http.StatusNotFound)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1", len(lines))
	}
	if lines[0]["status"] != float64(http.StatusNotFound) {
		t.Errorf("status = %v, want 404", lines[0]["status"])
	}
	bytesWritten, ok := lines[0]["bytes"].(float64)
	if !ok || bytesWritten < 0 {
		t.Errorf("bytes = %v, want a non-negative number", lines[0]["bytes"])
	}
}

// newRequestID itself must produce distinct, non-empty values across calls —
// isolates the id-generation logic from the middleware plumbing above.
func TestNewRequestIDIsUniqueAndNonEmpty(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if id == "" {
			t.Fatal("newRequestID returned an empty string")
		}
		if seen[id] {
			t.Fatalf("newRequestID produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}
