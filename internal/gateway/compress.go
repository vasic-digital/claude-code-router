package gateway

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
)

// encodingWriter wraps gin's ResponseWriter with a compressing body writer.
type encodingWriter struct {
	gin.ResponseWriter
	w io.WriteCloser
}

func (e *encodingWriter) Write(b []byte) (int, error) { return e.w.Write(b) }

// WriteString must also route through the compressor. gin's default
// implementation writes to the underlying connection directly, which would
// emit uncompressed bytes under a Content-Encoding header — producing a body
// the client cannot decode.
func (e *encodingWriter) WriteString(s string) (int, error) { return e.w.Write([]byte(s)) }

// Flush pushes buffered compressed bytes out. This is essential for SSE:
// without flushing the compressor (not just the socket), streamed tokens sit
// in the compression buffer and the client sees nothing until completion,
// which defeats streaming entirely.
func (e *encodingWriter) Flush() {
	if f, ok := e.w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	e.ResponseWriter.Flush()
}

// negotiate picks the best mutually supported encoding.
//
// Brotli is preferred over gzip: it compresses JSON and SSE markedly better.
// An explicit "identity" or an unparseable header yields no compression —
// guessing would risk sending a body the client cannot read.
func negotiate(acceptEncoding string) string {
	ae := strings.ToLower(acceptEncoding)
	if ae == "" {
		return ""
	}
	var hasBr, hasGzip bool
	for _, part := range strings.Split(ae, ",") {
		token := strings.TrimSpace(part)
		q := 1.0
		if i := strings.Index(token, ";"); i >= 0 {
			params := token[i+1:]
			token = strings.TrimSpace(token[:i])
			if j := strings.Index(params, "q="); j >= 0 {
				var parsed float64
				if _, err := fmt.Sscanf(strings.TrimSpace(params[j+2:]), "%g", &parsed); err == nil {
					q = parsed
				}
			}
		}
		// q=0 means "explicitly not acceptable".
		if q == 0 {
			continue
		}
		switch token {
		case "br":
			hasBr = true
		case "gzip":
			hasGzip = true
		}
	}
	if hasBr {
		return "br"
	}
	if hasGzip {
		return "gzip"
	}
	return ""
}

// compressionMiddleware compresses responses per Accept-Encoding.
func compressionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		enc := negotiate(c.GetHeader("Accept-Encoding"))
		if enc == "" {
			c.Next()
			return
		}

		var w io.WriteCloser
		switch enc {
		case "br":
			w = brotli.NewWriter(c.Writer)
		case "gzip":
			w = gzip.NewWriter(c.Writer)
		default:
			c.Next()
			return
		}

		c.Header("Content-Encoding", enc)
		// The body length changes under compression, so any upstream-derived
		// Content-Length is now wrong and must not be forwarded.
		c.Header("Vary", "Accept-Encoding")
		c.Writer.Header().Del("Content-Length")

		ew := &encodingWriter{ResponseWriter: c.Writer, w: w}
		c.Writer = ew
		defer func() {
			// Closing flushes the compressor's trailer. Without it the body is
			// truncated and clients report a corrupt stream.
			_ = w.Close()
		}()
		c.Next()
	}
}

// altSvcMiddleware advertises HTTP/3 availability so compatible clients can
// upgrade on a subsequent connection. Harmless to clients that ignore it.
func altSvcMiddleware(port int) gin.HandlerFunc {
	value := fmt.Sprintf(`h3=":%d"; ma=86400`, port)
	return func(c *gin.Context) {
		c.Header("Alt-Svc", value)
		c.Next()
	}
}

var _ http.Flusher = (*encodingWriter)(nil)
