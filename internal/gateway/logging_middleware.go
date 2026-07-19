package gateway

// logging_middleware.go — per-request structured access logging.
//
// LoggingMiddleware is a gin.HandlerFunc FACTORY, following the same
// pattern as auth.go's RequireAPIKey: it is exported here but NOT installed
// anywhere. Wiring it into the route table (an s.eng.Use(...) call in
// gateway.go) is left to whichever agent owns that file — this file, like
// the rest of internal/gateway's seams, does not modify gateway.go.
//
// # What is logged
//
// Exactly one line per request, after the handler chain completes: method,
// path, status, duration, bytes written, and a request id (honouring an
// inbound X-Request-Id when present, generating one otherwise — see
// newRequestID). The request id is also echoed back as a response header,
// so a client and the gateway's own logs can be correlated.
//
// # What is deliberately NEVER logged
//
//   - Request or response BODIES. They carry prompts and completions — user
//     data — and this middleware never reads either body; it only inspects
//     request metadata (method, URL path) and response metadata (status,
//     size) that gin's ResponseWriter already tracks as a byte count, not
//     content.
//   - The Authorization or X-Api-Key header VALUES. This middleware does
//     not log ANY header value at all (not even harmless ones), so there is
//     no allow/deny list of header names to get wrong — the set of logged
//     fields is fixed and enumerated above, full stop.
//
// As defense in depth, a logger built via internal/logging.New additionally
// redacts secret-shaped attributes and message text before they are ever
// written (see that package). This middleware does not rely on that
// safety net — it simply never hands the logger anything sensitive — but
// passing a logging.New logger costs nothing and covers the case where a
// future edit to this file accidentally starts logging something it
// shouldn't.
import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/vasic-digital/claude-code-router/internal/logging"
)

// requestIDHeader is the header both consulted on the inbound request and
// set on the outbound response, so caller and gateway logs correlate.
const requestIDHeader = "X-Request-Id"

// LoggingMiddleware returns a gin.HandlerFunc that logs one structured line
// per request to logger, at Info level.
//
// A nil logger falls back to internal/logging.New(os.Stderr) — an
// env-configured (CCR_LOG_LEVEL / CCR_LOG_FORMAT) redacting logger — so this
// middleware is always safe to install even before a caller has its own
// *slog.Logger wired up. Passing an explicit logger (as tests do, pointed at
// a buffer) overrides that default entirely.
func LoggingMiddleware(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = logging.New(os.Stderr)
	}

	return func(c *gin.Context) {
		start := time.Now()

		reqID := strings.TrimSpace(c.GetHeader(requestIDHeader))
		if reqID == "" {
			reqID = newRequestID()
		}
		// Set before c.Next(): a handler further down the chain (or a
		// panic recovered by gin.Recovery ahead of this middleware in the
		// stack) may write the response before this middleware resumes, so
		// the header must already be in place, not appended afterward.
		c.Writer.Header().Set(requestIDHeader, reqID)

		c.Next()

		bytesWritten := c.Writer.Size()
		if bytesWritten < 0 {
			// gin reports -1 when nothing was ever written (e.g. the
			// connection was hijacked, or the client disconnected before
			// any bytes went out). Logging a raw -1 byte count would read
			// as a bug in THIS middleware rather than what it actually is.
			bytesWritten = 0
		}

		logger.Info("http_request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", bytesWritten,
			"request_id", reqID,
		)
	}
}

// newRequestID generates a 16-byte random id, formatted as a standard
// (RFC 4122 version 4) UUID string, e.g. "550e8400-e29b-41d4-a716-446655440000".
// crypto/rand keeps this collision-resistant across concurrent requests
// without pulling in a UUID library dependency.
//
// The hyphenated 8-4-4-4-12 shape is not just cosmetic: it keeps every
// contiguous run of hex digits at or below 12 characters, which matters
// because internal/logging's redaction layer treats any UN-hyphenated
// contiguous blob of 24+ base64/hex-ish characters as secret-shaped and
// scrubs it (see that package's redact.go — deliberately, since that same
// heuristic is what catches an unlabelled leaked API key). A flat 32-hex-
// character id with no separators — the more "obvious" encoding — would
// itself get redacted out of the very log line whose job is to report it.
// Formatting as a UUID sidesteps that collision entirely rather than
// special-casing this one field inside the logging package.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The OS CSPRNG failing is exceptionally rare, and if it happens
		// the process has bigger problems than log correlation — but a
		// request id must never itself be the reason a request fails, so
		// fall back to a timestamp-based id that is unique-enough for log
		// correlation rather than propagating the error.
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
