package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/vasic-digital/claude-code-router/internal/config"
)

// managementServer is the small control-plane HTTP server "ccr serve" (and
// therefore "start"/"ui") exposes on --host:--port (default 127.0.0.1:3458).
// It is intentionally separate from the gateway (internal/gateway.Server,
// port 3456): the gateway is the Anthropic-compatible API surface Claude
// Code talks to, while this is where a future web UI and control endpoints
// (start/stop/profile management) would live. Kept deliberately minimal here
// — building that UI is out of this task's scope — but real enough that
// --open has something functional to point a browser at.
type managementServer struct {
	httpSrv *http.Server
	ln      net.Listener
}

func newManagementServer(host string, port int, cfg *config.Config) (*managementServer, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"service":   "ccr-management",
			"providers": len(cfg.Providers),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Claude Code Router</title></head>`+
			`<body><h1>Claude Code Router</h1><p>Management interface is running. `+
			`The Anthropic-compatible gateway is served separately (see /health here for status).</p></body></html>`)
	})

	return &managementServer{
		httpSrv: &http.Server{Handler: mux},
		ln:      ln,
	}, nil
}

func (m *managementServer) Addr() string { return m.ln.Addr().String() }

// Start serves in the background; Serve's own error (including the expected
// one on Shutdown) is not surfaced here — the caller learns about shutdown
// through Shutdown's return, not this goroutine.
func (m *managementServer) Start() {
	go func() { _ = m.httpSrv.Serve(m.ln) }()
}

func (m *managementServer) Shutdown(ctx context.Context) error {
	return m.httpSrv.Shutdown(ctx)
}
