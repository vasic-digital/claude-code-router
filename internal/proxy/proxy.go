// Package proxy sends translated chat-completion requests to the upstream
// provider the router selected, and hands back the raw response for the
// gateway to relay (buffered or streamed) to Claude Code.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// Client sends requests to upstream providers.
//
// A single http.Client (and its underlying transport/connection pool) is
// meant to be reused across every call for the life of the gateway process:
// providers are hit repeatedly, and a fresh transport per request would
// throw away keep-alive connections for no benefit.
type Client struct {
	HTTP *http.Client
}

// New builds a Client whose transport gives up waiting for a response's
// headers after timeout.
//
// This is deliberately NOT http.Client.Timeout, which bounds the entire
// request including reading the body: for a streaming (SSE) upstream, the
// body is expected to keep arriving in chunks for as long as the model is
// generating, which can run far past any fixed per-request budget. Bounding
// only the header wait catches a genuinely unresponsive upstream while
// never cutting a legitimate, slow-but-alive stream short.
func New(timeout time.Duration) *Client {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.ResponseHeaderTimeout = timeout
	return &Client{HTTP: &http.Client{Transport: transport}}
}

// Do posts body to p's endpoint and returns the upstream's raw HTTP
// response.
//
// p.APIBaseURL is used VERBATIM as the request URL. config.Provider
// documents that field as already being the complete chat-completions
// endpoint (e.g. "https://api.deepseek.com/chat/completions"); appending
// any suffix here would double up the path for every configured provider
// and break them all identically, so this function must never do that.
//
// A non-2xx upstream status is returned as a normal *http.Response, not a
// Go error: the caller needs the real status code and body to relay a
// faithful error back to Claude Code, not a synthesized one. Only a
// transport-level failure (couldn't even complete the round trip) becomes
// an error.
//
// The returned error NEVER contains p.APIKey. Do not add wrapping here that
// echoes the request object, its headers, or the constructed *http.Request
// — both net/http's own url.Error and this function's own fmt.Errorf calls
// are restricted to the URL and provider name, which contain no secret.
func (c *Client) Do(ctx context.Context, p *config.Provider, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.APIBaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("proxy: build request for provider %q: %w", p.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net/http reports transport failures as a *url.Error whose Error()
		// string is "<method> \"<url>\": <cause>" — it never includes request
		// headers, so the Authorization header set above cannot leak here.
		return nil, fmt.Errorf("proxy: request to provider %q failed: %w", p.Name, err)
	}
	return resp, nil
}
