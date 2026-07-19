package security

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
)

// A newline-and-header-name payload that, if it ever reached the wire
// unescaped inside a header value, would smuggle a second header into the
// request. Used both as an API key and as a URL fragment below.
const crlfPayload = "sk-real\r\nX-Injected-Header: smuggled-value"

// captureConn is a rawServer-style raw TCP listener that records the exact
// bytes of the request it receives, so header-injection tests can prove
// something never appeared on the wire, not merely that a Go-level API
// didn't complain.
func captureConn(t *testing.T) (addr string, captured chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		r := bufio.NewReader(conn)
		var sb strings.Builder
		for {
			line, err := r.ReadString('\n')
			sb.WriteString(line)
			if err != nil || line == "\r\n" || line == "\n" {
				break
			}
		}
		// Minimal valid response so the client side completes cleanly either
		// way; irrelevant to what we're proving here.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"))
		ch <- sb.String()
	}()
	return ln.Addr().String(), ch
}

// ---------- CR/LF in the API key ----------

// Go's net/http transport validates header field values before writing them
// to the wire (httpguts.ValidHeaderFieldValue) and rejects anything
// containing control characters. This test proves that protection actually
// holds for THIS codebase's request-construction path (both proxy.Client and
// the gateway's own defaultUpstream), rather than merely asserting a fact
// about the standard library in the abstract.
func TestCRLFInAPIKeyIsRejectedNotSmuggled(t *testing.T) {
	addr, captured := captureConn(t)
	p := &config.Provider{
		Name: "p", APIBaseURL: "http://" + addr + "/x",
		APIKey: crlfPayload, Models: []string{"m"},
	}

	c := proxy.New(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Do(ctx, p, []byte(`{}`), false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected the CRLF-laced API key to be rejected before the request was sent")
	}
	if !strings.Contains(err.Error(), "invalid header field value") {
		t.Errorf("error = %v, want it to explain the header rejection", err)
	}
	if strings.Contains(err.Error(), "X-Injected-Header") {
		t.Fatalf("error text itself echoed the injected header name — investigate further: %v", err)
	}

	// Nothing should ever have reached the wire: the raw connection must
	// either see no bytes at all, or (if the transport connects before
	// validating) certainly never the injected header line.
	select {
	case raw := <-captured:
		if strings.Contains(raw, "X-Injected-Header") {
			t.Fatalf("the injected header reached the wire despite the client-side error:\n%s", raw)
		}
	case <-time.After(300 * time.Millisecond):
		// No connection was ever made — the strongest possible outcome: the
		// bad header value was caught before dialing.
	}
}

// ---------- CR/LF in the base URL ----------

// Unlike the API key, api_base_url is not itself a secret (it is ordinary,
// already-known configuration), so url.Parse's error legitimately echoing
// the rejected value back — the standard "parse %q: invalid control
// character in URL" shape — is normal, helpful diagnostic behaviour, not a
// leak. The security property that actually matters here, and the one this
// test asserts, is that the injected header never reaches the wire: the
// malformed URL must fail request construction rather than being sent as-is
// and smuggling a second header into the real request.
func TestCRLFInAPIBaseURLIsRejectedNotSmuggled(t *testing.T) {
	addr, captured := captureConn(t)
	p := &config.Provider{
		Name:       "p",
		APIBaseURL: "http://" + addr + "/x\r\nX-Injected-Header: smuggled-value",
		APIKey:     "sk-fine",
		Models:     []string{"m"},
	}

	c := proxy.New(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Do(ctx, p, []byte(`{}`), false)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected the CRLF-laced base URL to be rejected before the request was built")
	}
	if !strings.Contains(err.Error(), "invalid control character") {
		t.Errorf("error = %v, want it to explain the URL was rejected for a control character", err)
	}

	select {
	case raw := <-captured:
		if strings.Contains(raw, "X-Injected-Header") {
			t.Fatalf("the injected header reached the wire:\n%s", raw)
		}
	case <-time.After(300 * time.Millisecond):
		// No connection made at all — expected, since url construction fails
		// before any dial is attempted.
	}
}

// ---------- Same proof, through the full gateway's defaultUpstream ----------

func TestGatewayRejectsCRLFInAPIKeyWithoutSmuggling(t *testing.T) {
	addr, captured := captureConn(t)
	s := gwServer("http://"+addr+"/x", crlfPayload)

	runBounded(t, defaultBound, func() {
		rec := postMessages(s)
		if rec.Code == http.StatusOK {
			t.Fatalf("expected the CRLF-laced API key to produce an error, got 200")
		}
		if strings.Contains(rec.Body.String(), "X-Injected-Header") {
			t.Fatalf("gateway error body echoed the injected header: %s", rec.Body.String())
		}
	})

	select {
	case raw := <-captured:
		if strings.Contains(raw, "X-Injected-Header") {
			t.Fatalf("the injected header reached the wire via the gateway's upstream client:\n%s", raw)
		}
	case <-time.After(300 * time.Millisecond):
	}
}
