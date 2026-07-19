package test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
)

// decompressBody undoes whatever Content-Encoding the gateway's compression
// middleware applied, so response-shape assertions run against the same
// bytes a real client would end up with after decoding.
func decompressBody(encoding string, body []byte) ([]byte, error) {
	switch encoding {
	case "":
		return body, nil
	case "br":
		return io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer r.Close()
		return io.ReadAll(r)
	default:
		return nil, fmt.Errorf("unknown Content-Encoding %q", encoding)
	}
}
