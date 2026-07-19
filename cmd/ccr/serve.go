package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/gateway"
)

// shutdownGrace bounds how long "serve" waits for in-flight requests to
// drain on SIGINT/SIGTERM before returning anyway. Long enough for a
// non-streaming call to finish, short enough that a supervisor's own kill
// timeout does not get triggered by us hanging.
const shutdownGrace = 10 * time.Second

// cmdServe runs the router service in the foreground: the management
// interface always, and the Anthropic-compatible gateway unless
// --no-gateway. It blocks until SIGINT/SIGTERM, then shuts both down
// gracefully. This is also what "ccr start"/"ccr ui" run as their detached
// child, and what "ccr web" aliases.
func cmdServe(args []string, stdout, stderr io.Writer) int {
	flags, rest, err := parseCommonFlags(args, false, true)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if len(rest) > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", rest[0])
		return 2
	}

	cfg, err := config.Load(config.Path())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var gw *gateway.Server
	if flags.Gateway {
		gw = gateway.New(cfg, gateway.Options{Port: defaultGatewayPort})
		if err := gw.Start(); err != nil {
			fmt.Fprintf(stderr, "start gateway: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "gateway listening on %s\n", gw.Addr())
	}

	mgmt, err := newManagementServer(flags.Host, flags.Port, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "start management interface: %v\n", err)
		if gw != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			_ = gw.Shutdown(ctx)
			cancel()
		}
		return 1
	}
	mgmt.Start()
	fmt.Fprintf(stdout, "management listening on http://%s\n", mgmt.Addr())

	if flags.Open {
		if err := openBrowser(fmt.Sprintf("http://%s", mgmt.Addr())); err != nil {
			// Best-effort only; a headless host has no browser to open, and
			// that is not a reason to refuse to serve.
			fmt.Fprintf(stderr, "note: could not open a browser: %v\n", err)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Fprintln(stdout, "shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if gw != nil {
		if err := gw.Shutdown(ctx); err != nil {
			fmt.Fprintf(stderr, "gateway shutdown: %v\n", err)
		}
	}
	if err := mgmt.Shutdown(ctx); err != nil {
		fmt.Fprintf(stderr, "management shutdown: %v\n", err)
	}
	return 0
}
